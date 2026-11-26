package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func recommender(t *testing.T, idle bool) *Supervisor {
	t.Helper()
	st := &workerState{}
	if idle {
		st.doneSince = t0
	}
	return &Supervisor{Settings: config.Supervisor{RecommendAfter: "2h", Guidance: "Keep old columns."}, Signals: config.DefaultSignals(),
		StateDir: t.TempDir(), Logf: t.Logf, workers: map[string]*workerState{"app": st}}
}

func decisionIssue(asked time.Time, question string) backlog.Issue {
	return backlog.Issue{Repo: "org/app", Number: 12, Title: "Paginate the audit log", Comments: []backlog.Comment{
		{Body: "## Hand-back\n**PR:** none\n**Decisions needed:** " + question, CreatedAt: asked}}}
}

func offered(t *testing.T, s *Supervisor) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(s.StateDir, runlog.TasksDir, runlog.TaskAny, "*.md"))
	return matches
}

func TestAnAskWithNoRecommendationPastItsTimeBecomesOneTaskForAnIdleWorker(t *testing.T) {
	s := recommender(t, true)
	own := []backlog.Issue{decisionIssue(t0.Add(-3*time.Hour), "keep the old column?")}

	s.offerRecommendations(own, t0)
	s.offerRecommendations(own, t0.Add(time.Minute))

	tasks := offered(t, s)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %q, want one", tasks)
	}
	brief, _ := os.ReadFile(tasks[0])
	for _, want := range []string{"org/app#12", "Shed recommends: Owner ruling: <your answer>", "Never begin a line with \"Owner ruling\"", "Keep old columns."} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	if id := strings.TrimSuffix(filepath.Base(tasks[0]), ".md"); !runlog.ValidTaskID(id) {
		t.Fatalf("task id %q is not valid", id)
	}
}

func TestNoRecommendationIsAskedForTooSoonWithNoIdleWorkerOrWhenOneIsGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		idle     bool
		asked    time.Duration
		question string
	}{
		"too soon":           {true, time.Hour, "keep the old column?"},
		"no idle worker":     {false, 3 * time.Hour, "keep the old column?"},
		"worker recommended": {true, 3 * time.Hour, "keep the old column? Recommend: keep it"},
	} {
		s := recommender(t, tc.idle)
		s.offerRecommendations([]backlog.Issue{decisionIssue(t0.Add(-tc.asked), tc.question)}, t0)
		if tasks := offered(t, s); len(tasks) != 0 {
			t.Errorf("%s: tasks = %q, want none", name, tasks)
		}
	}
}

func TestWithoutRecommendAfterNoTaskIsMade(t *testing.T) {
	s := recommender(t, true)
	s.Settings.RecommendAfter = ""
	s.offerRecommendations([]backlog.Issue{decisionIssue(t0.Add(-30*time.Hour), "keep it?")}, t0)
	if tasks := offered(t, s); len(tasks) != 0 {
		t.Fatalf("tasks = %q, want none", tasks)
	}
}
