package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// OffTrainPhrase starts shed's comment on an issue whose pull request it
// took off a merge train: its checks failed on the head shed brought up to
// date, or the update did not apply.
const OffTrainPhrase = "Off the merge train by shed:"

// Boarded returns the pull request of the issue's last hand-back when shed
// queued it for merge after that hand-back and has not taken it off a merge
// train since, with when it was queued; 0 otherwise.
func Boarded(issue Issue, signals []config.Signal) (int, time.Time) {
	pr, on, at, _ := train(issue, signals)
	if !on {
		return 0, time.Time{}
	}
	return pr, at
}

func boarded(issue Issue, signals []config.Signal) bool {
	pr, _ := Boarded(issue, signals)
	return pr != 0
}

// TakenOff reports whether shed took the pull request of the issue's last
// hand-back off a merge train. It boards again only after a new hand-back
// is verified.
func TakenOff(issue Issue, signals []config.Signal) bool {
	_, _, _, off := train(issue, signals)
	return off
}

// train reads, after the issue's last hand-back, whether its pull request is
// on a merge train and since when, and whether it was taken off one.
func train(issue Issue, signals []config.Signal) (pr int, on bool, at time.Time, off bool) {
	index, pr := lastHandBack(issue, signals)
	if index < 0 {
		return 0, false, time.Time{}, false
	}
	want := "#" + strconv.Itoa(pr) + ","
	for _, c := range issue.Comments[index+1:] {
		body := strings.TrimSpace(c.Body)
		switch {
		case strings.HasPrefix(body, QueuedPhrase) && strings.Contains(body, want):
			on, at = true, c.CreatedAt
		case strings.HasPrefix(body, OffTrainPhrase) && strings.Contains(body, want):
			on, off = false, true
		}
	}
	return pr, on, at, off
}

// MergeReady reports whether shed may queue the issue's pull request for
// merge, with its verification: a verifier passed it at low risk, its checks
// are green, it is not a draft, not on its way to the merge already, not
// taken off a merge train since its hand-back, and nothing else on the issue
// waits on the owner. A look on staging waits for nothing when lookAfter is
// set: a worker does it after the merge.
func MergeReady(issue Issue, signals []config.Signal, now time.Time, lookAfter bool) (VerificationState, bool) {
	v := Verification(issue, signals)
	pr := issue.OpenPRs[v.PR]
	if v.State != VerifyPassed || v.Tier != config.TierLow || pr.Draft || pr.Pending || pr.Problem != "" || pr.AutoMerge {
		return v, false
	}
	if on, _ := Boarded(issue, signals); on != 0 || TakenOff(issue, signals) {
		return v, false
	}
	for _, a := range Effects(issue, signals, now) {
		if a.Effect != EffectMerge && !(lookAfter && a.Name == config.EyesSignal) {
			return v, false
		}
	}
	return v, true
}

// Head is a pull request as it stands at its head commit: what a merge train
// needs to know before it merges.
type Head struct {
	// Open is set while the pull request is open: not merged, not closed.
	Open bool
	SHA  string
	// Base is the branch the pull request merges into.
	Base string
	// Behind counts the commits of the base branch the head does not have.
	Behind int
	// Checks counts the checks reported on the head, skipped ones aside.
	// None reported yet is not green: the checks of a new head take a
	// moment to start.
	Checks int
	// Skipped counts the checks reported as skipped. A skipped check tested
	// nothing: a head whose every check was skipped is not green.
	Skipped int
	Pending bool
	// Problem says why the pull request cannot merge at this head, as PR's.
	Problem string
}

// Green reports whether every check of the head passed, and some did.
func (h Head) Green() bool {
	return h.Checks > 0 && !h.Pending && h.Problem == ""
}

// AllSkipped reports whether checks were reported on the head and every one
// was skipped: its CI did not run on it.
func (h Head) AllSkipped() bool {
	return h.Checks == 0 && h.Skipped > 0
}

// ReadHead reads the pull request at its head commit, and how far that head
// is behind its base branch.
func (GH) ReadHead(ctx context.Context, repo string, pr int) (Head, error) {
	out, err := gh(ctx, "pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "state,headRefOid,baseRefName,mergeable,statusCheckRollup")
	if err != nil {
		return Head{}, err
	}
	head, base, err := parseHead(out)
	if err != nil {
		return Head{}, fmt.Errorf("gh pr view %s#%d: %w", repo, pr, err)
	}
	if !head.Open {
		return head, nil
	}
	out, err = gh(ctx, "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head.SHA), "--jq", ".behind_by")
	if err != nil {
		return Head{}, err
	}
	if head.Behind, err = strconv.Atoi(strings.TrimSpace(string(out))); err != nil {
		return Head{}, fmt.Errorf("compare %s %s...%s: behind_by %q: %w", repo, base, head.SHA, out, err)
	}
	return head, nil
}

func parseHead(data []byte) (Head, string, error) {
	var pr struct {
		State     string  `json:"state"`
		SHA       string  `json:"headRefOid"`
		Base      string  `json:"baseRefName"`
		Mergeable string  `json:"mergeable"`
		Checks    []check `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(data, &pr); err != nil {
		return Head{}, "", fmt.Errorf("parse output: %w", err)
	}
	if pr.SHA == "" || pr.Base == "" {
		return Head{}, "", fmt.Errorf("no head commit or base branch in %s", data)
	}
	skipped := 0
	for _, c := range pr.Checks {
		if c.Conclusion == "SKIPPED" {
			skipped++
		}
	}
	return Head{Open: pr.State == "OPEN", SHA: pr.SHA, Base: pr.Base, Checks: len(pr.Checks) - skipped, Skipped: skipped, Pending: pending(pr.Checks), Problem: prProblem(pr.Mergeable, pr.Checks)}, pr.Base, nil
}

// UpdateBranch brings the pull request up to date with its base branch: a
// merge of the base into it, or a rebase onto it.
func (GH) UpdateBranch(ctx context.Context, repo string, pr int, rebase bool) error {
	args := []string{"pr", "update-branch", strconv.Itoa(pr), "--repo", repo}
	if rebase {
		args = append(args, "--rebase")
	}
	_, err := gh(ctx, args...)
	return err
}

// MergeHead merges the pull request only if its head is still the commit
// whose checks were read.
func (GH) MergeHead(ctx context.Context, repo string, pr int, method, sha string) error {
	_, err := gh(ctx, "pr", "merge", strconv.Itoa(pr), "--repo", repo, "--"+method, "--match-head-commit", sha)
	return err
}

// Comment writes a comment on the issue.
func (GH) Comment(ctx context.Context, repo string, issue int, body string) error {
	_, err := gh(ctx, "issue", "comment", strconv.Itoa(issue), "--repo", repo, "--body", body)
	return err
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(3, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
