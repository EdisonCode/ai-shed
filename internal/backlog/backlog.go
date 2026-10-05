// Package backlog reads a machine's open GitHub issues and finds the ones
// that wait on the owner.
package backlog

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

type Issue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
	Comments  []Comment `json:"comments"`
	Labels    []Label   `json:"labels"`
	// OpenPRs holds the repo's open pull requests by number. It is nil when
	// the lister did not look them up.
	OpenPRs map[int]PR `json:"-"`
}

// PR is the state of an open pull request.
type PR struct {
	// Problem says why the pull request cannot merge as it stands, for
	// example "conflicts with the base branch". Empty when nothing is wrong.
	Problem string
}

type Label struct {
	Name string `json:"name"`
}

// LabelNames returns the issue's labels as plain names.
func (i Issue) LabelNames() []string {
	names := make([]string, len(i.Labels))
	for n, l := range i.Labels {
		names[n] = l.Name
	}
	return names
}

type Comment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// Lister returns the open issues of one source, comments oldest first.
type Lister interface {
	List(ctx context.Context, src config.IssueSource) ([]Issue, error)
}

// GH lists issues with the GitHub CLI, so shed needs no token of its own.
type GH struct{}

func (GH) List(ctx context.Context, src config.IssueSource) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", src.Repo, "--state", "open", "--limit", "200",
		"--json", "number,title,url,updatedAt,comments,labels"}
	if src.Label != "" {
		args = append(args, "--label", src.Label)
	}
	if src.Assignee != "" {
		args = append(args, "--assignee", src.Assignee)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh issue list %s: %w: %s", src.Repo, err, strings.TrimSpace(stderr.String()))
	}
	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("gh issue list %s: parse output: %w", src.Repo, err)
	}
	openPRs, err := openPRs(ctx, src.Repo)
	if err != nil {
		return nil, err
	}
	for i := range issues {
		issues[i].Repo, issues[i].OpenPRs = src.Repo, openPRs
	}
	return issues, nil
}

func openPRs(ctx context.Context, repo string) (map[int]PR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "1000",
		"--json", "number,mergeable,statusCheckRollup")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	return parsePRs(out)
}

// check is one entry of a pull request's checks: a check run (Name and
// Conclusion) or a commit status (Context and State).
type check struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func parsePRs(data []byte) (map[int]PR, error) {
	var prs []struct {
		Number    int     `json:"number"`
		Mergeable string  `json:"mergeable"`
		Checks    []check `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: parse output: %w", err)
	}
	open := make(map[int]PR, len(prs))
	for _, pr := range prs {
		open[pr.Number] = PR{Problem: prProblem(pr.Mergeable, pr.Checks)}
	}
	return open, nil
}

var failed = map[string]bool{"FAILURE": true, "TIMED_OUT": true, "STARTUP_FAILURE": true, "ERROR": true}

// prProblem says what stops a pull request from merging: a conflict first,
// since a rebase reruns the checks anyway.
func prProblem(mergeable string, checks []check) string {
	if mergeable == "CONFLICTING" {
		return "conflicts with the base branch"
	}
	for _, c := range checks {
		if failed[c.Conclusion] || failed[c.State] {
			return fmt.Sprintf("has a failed check (%s)", cmp.Or(c.Name, c.Context))
		}
	}
	return ""
}

// Waiting returns the names of the signals that are open on the issue: the
// reasons it waits on the owner. While the issue needs a worker again (see
// Rework), what the worker asked the owner to look at or review is not among
// them: the work will change, and the worker asks again when it hands back.
// A question it asked the owner still is.
func Waiting(issue Issue, signals []config.Signal) []string {
	var open []string
	for _, a := range Asks(issue, signals) {
		open = append(open, a.Name)
	}
	return open
}

// Ask is one thing an issue asks of the owner: an open signal.
type Ask struct {
	// Name is the signal's name.
	Name string `json:"name"`
	// Since is when the worker last asked; zero when the comment has no time.
	Since time.Time `json:"since"`
	// PR is the pull request a follows_pr signal waits on; 0 for none.
	PR int `json:"pr,omitempty"`
}

// Asks returns what the issue asks of the owner, in the order of the signals.
// Waiting says which asks are left out while the issue needs a worker again.
func Asks(issue Issue, signals []config.Signal) []Ask {
	rework := Rework(issue, signals) != ""
	var asks []Ask
	for _, s := range signals {
		isOpen, pr, since := signalState(issue, s)
		if !isOpen || (rework && !s.SendsBack) {
			continue
		}
		ask := Ask{Name: s.Name, Since: since}
		if s.FollowsPR {
			ask.PR = pr
		}
		asks = append(asks, ask)
	}
	return asks
}

// Rework says why an issue that was handed back needs a worker again while
// its pull request is still open: the owner answered after the hand-back with
// something other than an acceptance, or the pull request cannot merge as it
// stands. Empty when it does not.
func Rework(issue Issue, signals []config.Signal) string {
	handBack, pr := lastHandBack(issue, signals)
	open, isOpen := issue.OpenPRs[pr]
	if handBack < 0 || !isOpen {
		return ""
	}
	var reasons []string
	for _, s := range signals {
		if !s.SendsBack || s.AnsweredBy == "" {
			continue
		}
		// The last answer rules: an acceptance after a change was asked for
		// takes the change back.
		if last := lastComment(issue, s.AnsweredBy); last > handBack && !accepts(issue.Comments[last].Body, s) {
			reasons = append(reasons, fmt.Sprintf("the owner answered in the issue (%s) after pull request #%d was handed back; read the answer and apply it", s.AnsweredBy, pr))
			break
		}
	}
	if open.Problem != "" {
		reasons = append(reasons, fmt.Sprintf("pull request #%d %s", pr, open.Problem))
	}
	return strings.Join(reasons, "; ")
}

// accepts reports whether an answer takes the work as it stands: the text
// after the answering phrase starts with one of the signal's accept words.
func accepts(body string, s config.Signal) bool {
	value, _ := afterPhrase(strings.ToLower(body), strings.ToLower(s.AnsweredBy))
	return isClear(strings.TrimLeft(value, ": \t\r\n*_`\"'"), s.Accept)
}

// lastHandBack returns the position of the last comment in which a worker
// handed back a pull request, and that pull request's number; -1 for none.
func lastHandBack(issue Issue, signals []config.Signal) (index, pr int) {
	index = -1
	for _, s := range signals {
		if !s.FollowsPR {
			continue
		}
		for i, c := range issue.Comments {
			if value, found := asked(strings.ToLower(c.Body), s); found && i >= index {
				if n := prNumber(value); n != 0 {
					index, pr = i, n
				}
			}
		}
	}
	return index, pr
}

// lastComment returns the position of the last comment that has the phrase;
// -1 for none.
func lastComment(issue Issue, phrase string) int {
	last := -1
	for i, c := range issue.Comments {
		if strings.Contains(strings.ToLower(c.Body), strings.ToLower(phrase)) {
			last = i
		}
	}
	return last
}

// signalState reports whether the signal is open on the issue, the pull
// request its last ask named (0 for none), and when that ask was made.
func signalState(issue Issue, s config.Signal) (open bool, pr int, since time.Time) {
	for _, c := range issue.Comments {
		body := strings.ToLower(c.Body)
		// The answer is tested first: a comment that both cites an earlier
		// answer and asks again leaves the signal open.
		if s.AnsweredBy != "" && strings.Contains(body, strings.ToLower(s.AnsweredBy)) {
			open = false
		}
		if value, found := asked(body, s); found {
			open, pr, since = !isClear(value, s.Clear), prNumber(value), c.CreatedAt
		}
	}
	// A signal that follows a pull request is over once that pull request is
	// merged or closed.
	if open && s.FollowsPR && pr != 0 && issue.OpenPRs != nil {
		if _, stillOpen := issue.OpenPRs[pr]; !stillOpen {
			return false, pr, since
		}
	}
	return open, pr, since
}

var prRE = regexp.MustCompile(`#(\d+)`)

// prNumber returns the pull request named on the first line of the text, or 0.
func prNumber(value string) int {
	line, _, _ := strings.Cut(value, "\n")
	m := prRE.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// asked returns what follows the signal's ask in a comment. The ask counts
// only in a hand-back comment: the same words in a plan, a brief or a
// discussion ask the owner nothing. body is lower case.
func asked(body string, s config.Signal) (string, bool) {
	if s.HandBack != "" && !strings.Contains(body, strings.ToLower(s.HandBack)) {
		return "", false
	}
	return afterPhrase(body, strings.ToLower(s.Ask))
}

// afterPhrase returns the text that follows the phrase, without leading
// whitespace and markdown emphasis.
func afterPhrase(body, phrase string) (string, bool) {
	_, rest, found := strings.Cut(body, phrase)
	if !found {
		return "", false
	}
	return strings.TrimLeft(rest, " \t\r\n*_`\"'"), true
}

func isClear(value string, clear []string) bool {
	for _, word := range clear {
		if strings.HasPrefix(value, strings.ToLower(word)) {
			return true
		}
	}
	return false
}
