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
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
	Comments  []Comment `json:"comments"`
	Labels    []Label   `json:"labels"`
	// OpenPRs holds the repo's open pull requests by number. It is nil when
	// the lister did not look them up.
	OpenPRs map[int]PR `json:"-"`
	// MergedPRs holds the numbers of the repo's recently merged pull
	// requests. It is nil when the lister did not look them up.
	MergedPRs map[int]bool `json:"-"`
	// Verify is set by the reader when the issue's repository verifies a
	// handed-back pull request before the owner reviews it.
	Verify bool `json:"-"`
	// Deps are the issues this one waits for, with their state: the ones
	// its text names and the ones GitHub's blocked-by relation names. Nil
	// when the lister did not look them up.
	Deps []Dep `json:"-"`
}

// PR is the state of an open pull request.
type PR struct {
	// Problem says why the pull request cannot merge as it stands, for
	// example "conflicts with the base branch". Empty when nothing is wrong.
	Problem string
	// Draft is set while the pull request is a draft.
	Draft bool
	// Pending is set while a check of its head still runs.
	Pending bool
	// AutoMerge is set when the pull request merges by itself once its
	// checks pass, or waits in a merge queue.
	AutoMerge bool
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
	merged, err := mergedPRs(ctx, src.Repo)
	if err != nil {
		return nil, err
	}
	for i := range issues {
		issues[i].OpenPRs, issues[i].MergedPRs = openPRs, merged
	}
	if err := resolveDeps(ctx, src.Repo, issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// mergedLimit is how many of the latest merged pull requests are asked for.
// A hand-back that names an older one is not taken as finished, and its
// issue is handed out as before.
const mergedLimit = 200

func mergedPRs(ctx context.Context, repo string) (map[int]bool, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repo, "--state", "merged", "--limit", strconv.Itoa(mergedLimit), "--json", "number")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list %s --state merged: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list %s --state merged: parse output: %w", repo, err)
	}
	merged := make(map[int]bool, len(prs))
	for _, pr := range prs {
		merged[pr.Number] = true
	}
	return merged, nil
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
		"--json", "number,title,body,url,updatedAt,comments,labels"}, filter...)
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
// Claim puts a queue label on an issue, and takes off the label of the pool
// it came from, if any.
func (GH) Claim(ctx context.Context, repo string, number int, add, remove string) error {
	args := []string{"issue", "edit", strconv.Itoa(number), "--repo", repo, "--add-label", add}
	if remove != "" {
		args = append(args, "--remove-label", remove)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue edit %s#%d: %w: %s", repo, number, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func openPRs(ctx context.Context, repo string) (map[int]PR, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "1000",
		"--json", "number,mergeable,isDraft,statusCheckRollup,autoMergeRequest")
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
	// Status is a check run's progress: QUEUED, IN_PROGRESS, COMPLETED.
	Status string `json:"status"`
}

func parsePRs(data []byte) (map[int]PR, error) {
	var prs []struct {
		Number    int       `json:"number"`
		Mergeable string    `json:"mergeable"`
		Draft     bool      `json:"isDraft"`
		Checks    []check   `json:"statusCheckRollup"`
		AutoMerge *struct{} `json:"autoMergeRequest"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: parse output: %w", err)
	}
	open := make(map[int]PR, len(prs))
	for _, pr := range prs {
		open[pr.Number] = PR{Problem: prProblem(pr.Mergeable, pr.Checks), Draft: pr.Draft, Pending: pending(pr.Checks), AutoMerge: pr.AutoMerge != nil}
	}
	return open, nil
}

// pending reports whether a check still runs: a check run that has not
// completed, or a commit status that is pending.
func pending(checks []check) bool {
	for _, c := range checks {
		if (c.Name != "" && c.Status != "" && c.Status != "COMPLETED") || c.State == "PENDING" || c.State == "EXPECTED" {
			return true
		}
	}
	return false
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
	// WaitsFor is set when the pull request is a draft on purpose: the
	// hand-back says what it waits for, as in "#41 (draft: waits for #40)".
	// Such a pull request is not ready for the owner to review.
	WaitsFor string `json:"waits_for,omitempty"`
	// Effect is what answering the ask does: returns, in grace, read or
	// merge. Empty when it was not worked out.
	Effect string `json:"effect,omitempty"`
}

// What names the ask and the pull request it waits on, as in "review #41".
func (a Ask) What() string {
	switch {
	case a.WaitsFor != "":
		return fmt.Sprintf("draft #%d (%s)", a.PR, a.WaitsFor)
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
	verifying := unverified(issue, signals)
	if _, pr := lastHandBack(issue, signals); issue.OpenPRs[pr].AutoMerge {
		// On its way to the merge: nothing about it waits on the owner.
		verifying = true
	}
	var asks []Ask
	for _, s := range signals {
		isOpen, pr, since, line := signalState(issue, s)
		if !isOpen || (rework && !s.SendsBack) || (verifying && s.FollowsPR) {
			// A review waits until a verifier has passed the work.
			continue
		}
		ask := Ask{Name: s.Name, Since: since}
		if s.FollowsPR {
			ask.PR, ask.NoPR = pr, pr == 0
			if issue.OpenPRs[pr].Draft {
				ask.WaitsFor = draftNote(line)
			}
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
	if v := Verification(issue, signals); v.State == VerifyFailed {
		reasons = append(reasons, fmt.Sprintf("the verifier sent pull request #%d back: %s; fix what it found and hand back again", pr, v.Note))
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
	return entriesAfter(body, s.Decided, s.Clear)
}

// entriesAfter reads the entries after a phrase: the rest of its line, then
// the lines of the list that follows, up to an empty line or the next of the
// hand-back's own headings. A first entry that is a clear word is none.
func entriesAfter(body, phrase string, clear []string) []string {
	at := strings.Index(strings.ToLower(body), strings.ToLower(phrase))
	if phrase == "" || at < 0 {
		return nil
	}
	var entries []string
	for n, line := range strings.Split(body[at+len(phrase):], "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "*_"))
		switch {
		case line == "" && n == 0:
			continue
		case line == "" || strings.HasPrefix(line, "#") || headingRE.MatchString(line):
			return entries
		case n == 0 && isClear(strings.ToLower(line), clear):
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
	index, pr, _ = handBack(issue, signals)
	return index, pr
}

// Remaining returns what the issue's last hand-back says is still to do,
// one per entry; nil when it says nothing, or none.
func Remaining(issue Issue, signals []config.Signal) []string {
	for _, s := range signals {
		if !s.FollowsPR || s.Remaining == "" {
			continue
		}
		for i := len(issue.Comments) - 1; i >= 0; i-- {
			body := issue.Comments[i].Body
			if s.HandBack != "" && !strings.Contains(strings.ToLower(body), strings.ToLower(s.HandBack)) {
				continue
			}
			return entriesAfter(body, s.Remaining, []string{"none", "nothing"})
		}
	}
	return nil
}

// Finished returns the pull request that finished the issue, or 0. An
// issue is finished when the pull request its last hand-back follows has
// merged, the hand-back lists nothing still to do, and nobody has written
// on the issue since but in another hand-back. Such an issue may stay open,
// as when the merge was to a branch that does not close issues; handed out
// again, a worker finds nothing to do.
func Finished(issue Issue, signals []config.Signal) int {
	if issue.OpenPRs == nil || issue.MergedPRs == nil {
		return 0
	}
	index, pr := lastHandBack(issue, signals)
	if index < 0 || pr == 0 || !issue.MergedPRs[pr] || len(Remaining(issue, signals)) > 0 {
		return 0
	}
	if _, open := issue.OpenPRs[pr]; open {
		return 0
	}
	for _, c := range issue.Comments[index+1:] {
		if !isHandBack(c.Body, signals) {
			return 0
		}
	}
	return pr
}

// isHandBack reports whether a comment is a worker's hand-back. With no
// hand-back phrase, no comment can be told apart as one.
func isHandBack(body string, signals []config.Signal) bool {
	for _, s := range signals {
		if s.HandBack != "" && strings.Contains(strings.ToLower(body), strings.ToLower(s.HandBack)) {
			return true
		}
	}
	return false
}

// HandedPR returns the pull request the issue's latest hand-back named, or
// 0 when the hand-back names none.
func HandedPR(issue Issue, signals []config.Signal) int {
	_, pr := lastHandBack(issue, signals)
	return pr
}

// handBack is lastHandBack that also returns the first pull request the
// hand-back named, which may have merged while a later one is still open.
func handBack(issue Issue, signals []config.Signal) (index, pr, first int) {
	index = -1
	for _, s := range signals {
		if !s.FollowsPR {
			continue
		}
		for i, c := range issue.Comments {
			if value, found := asked(strings.ToLower(c.Body), s); found && i >= index {
				if n := openPR(issue, value); n != 0 {
					index, pr, first = i, n, prNumbers(value)[0]
				}
			}
		}
	}
	return index, pr, first
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
// request its last ask named (0 for none), when that ask was made, and the
// line that named the pull request, as the worker wrote it. An ask may name
// several pull requests: the one that counts is the first that is still open.
func signalState(issue Issue, s config.Signal) (open bool, pr int, since time.Time, line string) {
	for _, c := range issue.Comments {
		body := strings.ToLower(c.Body)
		// The answer is tested first: a comment that both cites an earlier
		// answer and asks again leaves the signal open.
		if s.AnsweredBy != "" && strings.Contains(body, strings.ToLower(s.AnsweredBy)) {
			open = false
		}
		if value, found := asked(body, s); found {
			open, pr, since = !isClear(value, s.Clear), openPR(issue, value), c.CreatedAt
			line, _ = noteAfter(c.Body, s.Ask)
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
			return false, pr, since, line
		}
	}
	return open, pr, since, line
}

var draftRE = regexp.MustCompile(`(?i)\(draft\b[\s:,;-]*([^)]*)\)`)

// draftNote returns what a hand-back says its draft pull request waits for:
// the words after "draft" in the brackets that follow the number. "" when it
// says nothing, which leaves the draft a review like any other.
func draftNote(line string) string {
	if m := draftRE.FindStringSubmatch(line); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

var prRE = regexp.MustCompile(`#(\d+)`)

// prNumbers returns the pull requests named on the first line of the text.
func prNumbers(value string) []int {
	line, _, _ := strings.Cut(value, "\n")
	var numbers []int
	for _, m := range prRE.FindAllStringSubmatch(line, -1) {
		if n, _ := strconv.Atoi(m[1]); n != 0 {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

// openPR returns the pull request that the first line of the text hands
// back: the first it names that is still open, or the first it names when
// none is, or 0. A hand-back may name the part that merged and the part
// that is still to be reviewed on one line.
func openPR(issue Issue, value string) int {
	numbers := prNumbers(value)
	for _, n := range numbers {
		if _, open := issue.OpenPRs[n]; open {
			return n
		}
	}
	if len(numbers) == 0 {
		return 0
	}
	return numbers[0]
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
	// The look is of work that merged: the first pull request the hand-back
	// named, whatever else it has open.
	_, _, pr = handBack(issue, signals)
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

// Release moves an issue off a machine's queue onto the fleet's overflow
// label, and says so in a comment.
func (GH) Release(ctx context.Context, repo string, number int, remove []string, add, comment string) error {
	args := []string{"issue", "edit", strconv.Itoa(number), "--repo", repo, "--add-label", add}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	for _, step := range [][]string{args, {"issue", "comment", strconv.Itoa(number), "--repo", repo, "--body", comment}} {
		cmd := exec.CommandContext(ctx, "gh", step...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gh %s %s#%d: %w: %s", step[1], repo, number, err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}

// ReleasedPhrase starts the comment of a worker's machine that released an
// issue to the fleet.
const ReleasedPhrase = "Released:"

// ReleasedBy names who released the issue, as in "mini/app", when its last
// release was not followed by a hand-back; "" otherwise.
func ReleasedBy(issue Issue) string {
	by := ""
	for _, c := range issue.Comments {
		body := strings.TrimSpace(c.Body)
		switch {
		case strings.HasPrefix(body, ReleasedPhrase):
			who := strings.TrimSpace(strings.TrimPrefix(body, ReleasedPhrase))
			who, _, _ = strings.Cut(who, ",")
			by = strings.TrimSpace(who)
		case strings.Contains(strings.ToLower(body), strings.ToLower(config.DefaultHandBack)):
			by = ""
		}
	}
	return by
}

// QueueMerge puts the pull request on its way to the merge: into the
// repository's merge queue, or to merge by itself once its required checks
// pass. Then it says so on the issue.
func (GH) QueueMerge(ctx context.Context, repo string, issue, pr int, method, comment string) error {
	for _, step := range [][]string{
		{"pr", "merge", strconv.Itoa(pr), "--repo", repo, "--auto", "--" + method},
		{"issue", "comment", strconv.Itoa(issue), "--repo", repo, "--body", comment},
	} {
		cmd := exec.CommandContext(ctx, "gh", step...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gh %s %s %s#%s: %w: %s", step[0], step[1], repo, step[2], err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
