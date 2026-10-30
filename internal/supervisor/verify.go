package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/risk"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// DiffReader reads what a pull request changes.
type DiffReader interface {
	Diff(ctx context.Context, repo string, pr int) (risk.Diff, error)
}

// diffKept is how long a pull request's diff is reused for its tier. A
// rework is a new hand-back, which is verified at the tier of its own diff.
const diffKept = 10 * time.Minute

type keptDiff struct {
	diff risk.Diff
	at   time.Time
}

// verifies reports whether the repository verifies its pull requests.
func (s *Supervisor) verifies(repo string) bool {
	for _, r := range s.Repos {
		if r.Name == repo {
			return r.Verify != nil
		}
	}
	return false
}

// mergesHigh reports whether the repository lets shed queue the merge of a
// high-risk pass too.
func (s *Supervisor) mergesHigh(repo string) bool {
	r, ok := s.repoConfig(repo)
	return ok && r.Verify != nil && r.Verify.MergeHigh
}

// withVerifies adds the pull requests that wait for a verifier to the queue
// of a worker that verifies, each at its risk tier. Their checks have
// passed: no model looks at work a deterministic check already failed.
// Where another verifier shares the queue, a worker is not handed what it
// built: a second reader is the point. Alone, it verifies its own work in a
// fresh context.
func (s *Supervisor) withVerifies(ctx context.Context, w config.Worker, items []QueueItem, open []backlog.Issue, checkins []runlog.Checkin) []QueueItem {
	if !w.Verifies || s.Diffs == nil {
		return items
	}
	// The builder still holds the issue after its hand-back; only another
	// verifier on it takes it.
	taken := map[int]bool{}
	for _, other := range s.Machine.Workers {
		if issue, _ := runlog.InHand(checkins, other.Name); other.Name != w.Name && issue != 0 && runlog.BuildingInHand(checkins, other.Name) == 0 {
			taken[issue] = true
		}
	}
	for _, i := range open {
		if !i.Verify || taken[i.Number] {
			continue
		}
		v := backlog.Verification(i, s.Signals)
		if v.State != backlog.VerifyDue {
			continue
		}
		choice := slices.ContainsFunc(s.Machine.Workers, func(other config.Worker) bool {
			return other.Name != w.Name && other.Verifies && slices.ContainsFunc(s.Machine.SourcesFor(other), func(src config.IssueSource) bool { return src.Repo == i.Repo })
		})
		if choice && runlog.Builder(checkins, i.Number) == w.Name {
			continue
		}
		tier, reasons, err := s.tierOf(ctx, i.Repo, v.PR)
		if err != nil {
			s.Logf("worker %s: tier of %s#%d: %v", w.Name, i.Repo, v.PR, err)
			continue
		}
		words := tier
		if v.Since != "" {
			reasons = append(reasons, "re-verify: a verifier passed it at "+v.Since+" and it had new commits since")
		}
		if len(reasons) > 0 {
			words += " (" + strings.Join(reasons, "; ") + ")"
		}
		item := QueueItem{Repo: i.Repo, Number: i.Number, Title: i.Title, Verify: words, VerifyPR: v.PR, Tier: tier,
			Model: s.Settings.Models.For(i.LabelNames())}
		if m := s.Settings.Models.Verify[tier]; m != "" {
			item.Model, item.Apply = m, config.ModelSwitch
		}
		items = slices.DeleteFunc(items, func(q QueueItem) bool { return q.Repo == i.Repo && q.Number == i.Number })
		items = append(items, item)
	}
	return items
}

// tierOf rates a pull request's risk from its diff.
func (s *Supervisor) tierOf(ctx context.Context, repo string, pr int) (string, []string, error) {
	key := fmt.Sprintf("%s#%d", repo, pr)
	kept, ok := s.diffs[key]
	if !ok || s.Now().Sub(kept.at) > diffKept {
		d, err := s.Diffs.Diff(ctx, repo, pr)
		if err != nil {
			return "", nil, err
		}
		if s.diffs == nil {
			s.diffs = map[string]keptDiff{}
		}
		kept = keptDiff{diff: d, at: s.Now()}
		s.diffs[key] = kept
	}
	var rules config.Verify
	for _, r := range s.Repos {
		if r.Name == repo && r.Verify != nil {
			rules = *r.Verify
		}
	}
	tier, reasons := risk.Tier(rules, kept.diff)
	return tier, reasons, nil
}

// verifyBrief tells a worker that verifies how to do it.
func (s *Supervisor) verifyBrief(b *strings.Builder, w config.Worker) {
	if !w.Verifies {
		return
	}
	fmt.Fprintf(b, `
## Verification

You check pull requests that other workers built, before the owner reviews
them. The supervisor names the pull request, its issue and its risk tier,
and starts you with an empty context: judge the work on its own evidence.

- Read the issue: its goal and its acceptance. Read the hand-back. Then
  read the diff (`+"`gh pr diff <number>`"+`) and check the branch out in
  your own directory (`+"`gh pr checkout <number>`"+`), never in another
  worker's.
- Low tier: go through it once. Does it do what the issue asks? Do its
  tests test that? Does it keep the standing orders? Run the tests it
  touches.
- High tier: try to break it. Wrong and empty input, the failure paths,
  concurrent use, what the change does to the code it did not touch. Run
  the full suite.
- Re-verify (a verifier passed an earlier head, then new commits came):
  read what changed since that head (`+"`git diff <head>..HEAD`"+`). When
  it touches only tests or docs, judge that change alone; otherwise verify
  the whole pull request at its tier.
- Change no code. Commit nothing, push nothing, approve or merge nothing.
`)
	if eyes := s.signal(config.EyesSignal); eyes.Ask != "" {
		fmt.Fprintf(b, `- When the hand-back asks for an eye check (after `+"`%s`"+`), judge that
  ask too: after the merge, someone looks on staging from it alone. It must say
  where to look on staging, what to do there (with the state the check
  needs), and what a pass looks like, matching the issue's acceptance. Each
  of these it lacks is a finding, and the verification fails.
`, eyes.Ask)
	}
	b.WriteString(`- When it does what the issue asks, post a comment on the issue that starts
  with ` + "`Verified (<tier>):`" + ` and gives your evidence: the commands you ran
  with their results, and each acceptance item with how you checked it.
  Where the repository lets shed merge what you pass, your pass queues the
  merge. When the merge cannot be undone (a dropped column or table, rows
  rewritten for good, data sent to another system), start with
  ` + "`Verified (<tier>, owner merges):`" + ` instead and say why: the owner
  merges it.
- When it does not, post a comment that starts with
  ` + "`Verification failed (<tier>):`" + `, then one finding per line: what is
  wrong and how to see it. Do not fix it: the pull request goes back to a
  worker that builds.
- Then say so here and stop.
`)
}
