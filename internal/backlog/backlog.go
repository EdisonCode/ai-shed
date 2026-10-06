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

// Find returns the open issue with this number from the first of the sources
// that has it.
func Find(ctx context.Context, lister Lister, sources []config.IssueSource, number int) (Issue, bool, error) {
	for _, src := range sources {
		issues, err := lister.List(ctx, src)
		if err != nil {
			return Issue{}, false, err
		}
		for _, i := range issues {
			if i.Number == number {
				return i, true, nil
			}
		}
	}
	return Issue{}, false, nil
}

// GH lists issues with the GitHub CLI, so shed needs no token of its own.
type GH struct{}

func (GH) List(ctx context.Context, src config.IssueSource) ([]Issue, error) {
	issues, err := issueList(ctx, src, "--state", "open")
	if err != nil {
		return nil, err
	}
	openPRs, err := openPRs(ctx, src.Repo)
	if err != nil {
		return nil, err
	}
	for i := range issues {
		issues[i].OpenPRs = openPRs
	}
	return issues, nil
}

// Closed returns the source's issues that were closed after since. Their
// open pull requests are not looked up.
func (GH) Closed(ctx context.Context, src config.IssueSource, since time.Time) ([]Issue, error) {
	// The search is by day; the caller's window is narrowed by what it does
	// with each issue's pull request.
	return issueList(ctx, src, "--state", "closed", "--search", "closed:>="+since.UTC().Format("2006-01-02"))
}

func issueList(ctx context.Context, src config.IssueSource, filter ...string) ([]Issue, error) {
	args := append([]string{"issue", "list", "--repo", src.Repo, "--limit", "200",
		"--json", "number,title,url,updatedAt,comments,labels"}, filter...)
	if src.Label != "" {
		args = append(args, "--label", src.Label)
	}
	if src.Assignee != "" {
		args = append(args, "--assignee", src.Assignee)
	}
	if src.Query != "" {
		args = withSearch(args, src.Query)
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
	for i := range issues {
		issues[i].Repo = src.Repo
	}
	return issues, nil
}

// withSearch adds a search to the arguments. gh takes one --search: a second
// would replace the first, so the terms are joined.
func withSearch(args []string, query string) []string {
	for i, a := range args {
		if a == "--search" && i+1 < len(args) {
			args[i+1] += " " + query
			return args
		}
	}
	return append(args, "--search", query)
}

// Claim puts the label on an issue, which puts it in the queue the label
// selects. shed changes nothing else on GitHub.
func (GH) Claim(ctx context.Context, repo string, number int, label string) error {
	cmd := exec.CommandContext(ctx, "gh", "issue", "edit", strconv.Itoa(number), "--repo", repo, "--add-label", label)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue edit %s#%d: %w: %s", repo, number, err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
	// NoPR is set when a follows_pr signal is open and its ask named no pull
	// request. No merge can close such an ask.
	NoPR bool `json:"no_pr,omitempty"`
}

// What names the ask and the pull request it waits on, as in "review #41".
func (a Ask) What() string {
	switch {
	case a.PR != 0:
		return fmt.Sprintf("%s #%d", a.Name, a.PR)
	case a.NoPR:
		return a.Name + " (no pull request named)"
	}
	return a.Name
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
			ask.PR, ask.NoPR = pr, pr == 0
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
			reasons = append(reasons, fmt.Sprintf("the owner answered in the issue (%s) after pull request #%d was handed back; read the answer and apply it; if it asks for no change, hand back again and say so", s.AnsweredBy, pr))
			break
		}
	}
	if open.Problem != "" {
		reasons = append(reasons, fmt.Sprintf("pull request #%d %s", pr, open.Problem))
	}
	return strings.Join(reasons, "; ")
}

// Ruled reports whether the owner answered a question of the issue's last
// hand-back and no worker has handed back since: the answer waits for a
// worker. Such an issue goes ahead of new work.
func Ruled(issue Issue, signals []config.Signal) bool {
	for _, s := range signals {
		if !s.SendsBack || s.AnsweredBy == "" {
			continue
		}
		question, last := -1, -1
		for i, c := range issue.Comments {
			body := strings.ToLower(c.Body)
			if s.HandBack == "" || strings.Contains(body, strings.ToLower(s.HandBack)) {
				last = i
			}
			if value, found := asked(body, s); found && !isClear(value, s.Clear) {
				question = i
			}
		}
		if answer := lastComment(issue, s.AnsweredBy); question >= 0 && answer > question && answer > last {
			return true
		}
	}
	return false
}

// Decided returns the choices a worker made by itself and recorded in the
// issue's last hand-back, one per entry; nil for none.
func Decided(issue Issue, signals []config.Signal) []string {
	for _, s := range signals {
		if s.Decided == "" {
			continue
		}
		for i := len(issue.Comments) - 1; i >= 0; i-- {
			body := issue.Comments[i].Body
			if s.HandBack != "" && !strings.Contains(strings.ToLower(body), strings.ToLower(s.HandBack)) {
				continue
			}
			return decidedIn(body, s)
		}
	}
	return nil
}

// decidedIn reads the entries after the signal's Decided phrase: the rest of
// its line, then the lines of the list that follows, up to an empty line or
// the next of the hand-back's own headings ("**PR:**").
func decidedIn(body string, s config.Signal) []string {
	at := strings.Index(strings.ToLower(body), strings.ToLower(s.Decided))
	if at < 0 {
		return nil
	}
	var entries []string
	for n, line := range strings.Split(body[at+len(s.Decided):], "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "*_"))
		switch {
		case line == "" && n == 0:
			continue
		case line == "" || strings.HasPrefix(line, "#") || headingRE.MatchString(line):
			return entries
		case n == 0 && isClear(strings.ToLower(line), s.Clear):
			return nil
		}
		entries = append(entries, strings.TrimSpace(entryRE.ReplaceAllString(line, "")))
	}
	return entries
}

var (
	// headingRE matches a line that opens another part of a hand-back, such
	// as "PR:** #41" once the leading emphasis is cut.
	headingRE = regexp.MustCompile(`^[A-Z][A-Za-z ]{1,30}:\*\*`)
	entryRE   = regexp.MustCompile(`^([-*]|\d+[.)])\s+`)
)

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
		// A check that was done and failed is not asked for any more. It is
		// tested last: the comment that reports it may repeat the ask.
		if failedIn(body, s) {
			open = false
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

func failedIn(body string, s config.Signal) bool {
	return s.FailedBy != "" && strings.Contains(body, strings.ToLower(s.FailedBy))
}

// What an issue says about the eye check of its last handed-back pull request.
const (
	EyesAsked   = "asked"   // a worker asked for a look and nobody has looked
	EyesFailed  = "failed"  // someone looked and the work did not pass
	EyesPassed  = "passed"  // someone looked and said so
	EyesBlocked = "blocked" // a worker tried to look and could not
)

// EyeCheck reports whether the issue's handed-back work still owes a look
// (EyesAsked), was looked at and failed (EyesFailed) or passed (EyesPassed),
// could not be looked at (EyesBlocked), or never asked for one (""). pr is
// the pull request of the last hand-back, since is when the state began, and
// note is what stopped a blocked look.
func EyeCheck(issue Issue, signals []config.Signal) (state string, pr int, since time.Time, note string) {
	_, pr = lastHandBack(issue, signals)
	if pr == 0 {
		return "", 0, time.Time{}, ""
	}
	for _, s := range signals {
		if s.Name != config.EyesSignal {
			continue
		}
		for _, c := range issue.Comments {
			body := strings.ToLower(c.Body)
			// An answer with no ask before it answers nothing.
			if s.AnsweredBy != "" && state != "" && strings.Contains(body, strings.ToLower(s.AnsweredBy)) {
				state, since = EyesPassed, c.CreatedAt
			}
			if value, found := asked(body, s); found {
				state, since = "", c.CreatedAt
				if !isClear(value, s.Clear) {
					state = EyesAsked
				}
			}
			if failedIn(body, s) {
				state, since = EyesFailed, c.CreatedAt
			}
			// A look that was done, or never asked for, cannot be blocked.
			if why, found := noteAfter(c.Body, s.BlockedBy); found && (state == EyesAsked || state == EyesBlocked) {
				state, since, note = EyesBlocked, c.CreatedAt, why
			}
			// The owner cleared what was in the way: the look is owed again.
			if state == EyesBlocked && s.UnblockedBy != "" && strings.Contains(body, strings.ToLower(s.UnblockedBy)) {
				state, since = EyesAsked, c.CreatedAt
			}
		}
	}
	if state != EyesBlocked {
		note = ""
	}
	return state, pr, since, note
}

// noteAfter returns the first line that follows the phrase in a comment, in
// the comment's own case.
func noteAfter(body, phrase string) (string, bool) {
	at := strings.Index(strings.ToLower(body), strings.ToLower(phrase))
	if phrase == "" || at < 0 {
		return "", false
	}
	rest := strings.TrimLeft(body[min(at+len(phrase), len(body)):], ": \t\r\n*_`\"'")
	line, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(line), true
}
