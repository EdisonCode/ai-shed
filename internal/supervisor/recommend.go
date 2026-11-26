package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// offerRecommendations gives the idle workers a one-off task for each ask
// of the machine's own issues that has waited on the owner past
// recommend_after with no recommendation: read the issue and the owner's
// guidance, and recommend the answer. Each ask gets one task, however
// often it is looked at; an issue in an earlier machine's queue is that
// machine's to offer.
func (s *Supervisor) offerRecommendations(own []backlog.Issue, now time.Time) {
	after := s.Settings.RecommendAfterOrZero()
	if after <= 0 || !s.anyIdle() {
		return
	}
	byName := map[string]config.Signal{}
	for _, sig := range s.Signals {
		byName[sig.Name] = sig
	}
	for _, i := range own {
		for _, a := range backlog.Asks(i, s.Signals) {
			sig := byName[a.Name]
			if sig.AnsweredBy == "" || sig.FollowsPR || sig.Name == config.EyesSignal || a.Since.IsZero() || now.Sub(a.Since) < after ||
				a.Recommended != "" || strings.Contains(strings.ToLower(a.Text), "recommend") {
				continue
			}
			key := fmt.Sprintf("recommend-%s-%d-%s-%d", slug(i.Repo), i.Number, slug(a.Name), a.Since.Unix())
			if s.taskOffered(key) {
				continue
			}
			id := fmt.Sprintf("%d-%s", now.Unix(), key)
			path := filepath.Join(s.StateDir, runlog.TasksDir, runlog.TaskAny, id+".md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				s.Logf("recommend %s#%d: %v", i.Repo, i.Number, err)
				return
			}
			if err := os.WriteFile(path, []byte(recommendBrief(i, a, sig, now, s.Settings.Guidance)), 0o644); err != nil {
				s.Logf("recommend %s#%d: %v", i.Repo, i.Number, err)
				return
			}
			s.Logf("%s#%d has waited on you for %s for a %s with no recommendation: an idle worker is asked to recommend one (task %s)", i.Repo, i.Number, now.Sub(a.Since).Round(time.Minute), a.Name, id)
		}
	}
}

// anyIdle reports whether a worker of the machine has nothing to go on.
func (s *Supervisor) anyIdle() bool {
	for _, st := range s.workers {
		if st != nil && !st.doneSince.IsZero() {
			return true
		}
	}
	return false
}

// taskOffered reports whether a task with the key was made already: it
// waits, or a worker was handed it.
func (s *Supervisor) taskOffered(key string) bool {
	matches, _ := filepath.Glob(filepath.Join(s.StateDir, runlog.TasksDir, "*", "*-"+key+".md"))
	return len(matches) > 0
}

var notSlug = regexp.MustCompile("[^a-z0-9]+")

// slug makes text fit a task id.
func slug(text string) string {
	return strings.Trim(notSlug.ReplaceAllString(strings.ToLower(text), "-"), "-")
}

// recommendBrief is the task that asks a worker to recommend the answer to
// an ask. The worker recommends; it never answers for the owner.
func recommendBrief(i backlog.Issue, a backlog.Ask, sig config.Signal, now time.Time, guidance string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Recommend an answer for %s#%d\n\n", i.Repo, i.Number)
	fmt.Fprintf(&b, "%s#%d (%s) has waited on the owner for %s for a %s, and nobody has recommended an answer. Prepare one for the owner to take or leave.\n\n", i.Repo, i.Number, i.Title, now.Sub(a.Since).Round(time.Minute), a.Name)
	fmt.Fprintf(&b, "1. Read the issue and its comments: gh issue view %d --repo %s --comments\n", i.Number, i.Repo)
	b.WriteString("2. Read the standing guidance: the Choices section of .shed/BRIEF.md, and the owner's rules below. Read the code the question is about where it helps.\n")
	fmt.Fprintf(&b, "3. Post one comment on the issue. Its first line is %s followed, on that same line, by the comment that would answer it: %s %s: <your answer>", backlog.RecommendPhrase, backlog.RecommendPhrase, sig.AnsweredBy)
	if len(sig.Accept) > 0 {
		fmt.Fprintf(&b, " (or %s %s: %s to take the worker's work as it stands)", backlog.RecommendPhrase, sig.AnsweredBy, sig.Accept[0])
	}
	b.WriteString(". On the lines after it, say why, and name the rule or the code you went by. Where the guidance does not settle it, say so and recommend the safest choice.\n")
	fmt.Fprintf(&b, "4. Never begin a line with %q: that answers for the owner, which is theirs alone. Change nothing else: no code, no labels, no other comment.\n", sig.AnsweredBy)
	b.WriteString("5. Write in your report file what you recommended and why, and stop.\n\n")
	b.WriteString("## The owner's rules\n\n")
	if g := strings.TrimSpace(guidance); g != "" {
		b.WriteString(g + "\n")
	} else {
		b.WriteString("(none given in the fleet file: go by the brief and the code)\n")
	}
	return b.String()
}
