package gates

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.FixedZone("PDT", -7*60*60))

type status struct{ repo, commit, state, description string }

type fakeGitHub struct {
	pulls    map[string][]Pull
	statuses []status
	comments map[int]string
}

func (f *fakeGitHub) OpenPulls(_ context.Context, repo string) ([]Pull, error) {
	return f.pulls[repo], nil
}

func (f *fakeGitHub) SetStatus(_ context.Context, repo, commit, state, description string) error {
	f.statuses = append(f.statuses, status{repo, commit, state, description})
	return nil
}

func (f *fakeGitHub) PostGates(_ context.Context, repo string, pr int, body string) error {
	if f.comments == nil {
		f.comments = map[int]string{}
	}
	f.comments[pr] = body
	return nil
}

// fakeTree holds the commits of each worker's checkout.
type fakeTree struct {
	commits     map[string][]string
	checkedOut  []string
	checkoutErr error
}

func (f *fakeTree) Has(_ context.Context, dir, commit string) bool {
	return slices.Contains(f.commits[dir], commit)
}

func (f *fakeTree) Checkout(_ context.Context, dir, worktree, commit string) error {
	f.checkedOut = append(f.checkedOut, worktree+"@"+commit)
	return f.checkoutErr
}

type run struct{ dir, script string }

type fixture struct {
	*Runner
	gh   *fakeGitHub
	tree *fakeTree
	runs []run
	exit int
	out  string
	now  time.Time
	busy string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{gh: &fakeGitHub{pulls: map[string][]Pull{}}, tree: &fakeTree{commits: map[string][]string{}}, now: t0}
	f.Runner = &Runner{
		Machine: config.Machine{
			Name:    "box",
			Init:    "source ~/.profile",
			Issues:  []config.IssueSource{{Repo: "org/app", Label: "machine:box"}},
			Workers: []config.Worker{{Name: "app", Dir: "/w/app"}, {Name: "docs", Dir: "/w/docs"}},
		},
		Repos:    []config.Repo{{Name: "org/app", Gates: config.Gates{Command: "make lint test", Timeout: "10m"}}},
		GitHub:   f.gh,
		Tree:     f.tree,
		StateDir: t.TempDir(),
		Now:      func() time.Time { return f.now },
		Busy:     func(context.Context) string { return f.busy },
		Logf:     t.Logf,
	}
	f.Run = func(_ context.Context, dir, script string, timeout time.Duration) (string, int) {
		f.runs = append(f.runs, run{dir, script})
		f.now = f.now.Add(2 * time.Minute)
		return f.out, f.exit
	}
	return f
}

func (f *fixture) gates(t *testing.T) []runlog.Gate {
	t.Helper()
	g, err := runlog.ReadGates(f.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestThePullRequestsOfThisMachineAreGatedOnTheirHeadCommit(t *testing.T) {
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}, {Number: 42, Head: "bbb222"}}
	f.tree.commits["/w/docs"] = []string{"aaa111"} // 42 was pushed from another machine
	f.Tick(context.Background())

	if len(f.runs) != 1 || f.runs[0].script != "source ~/.profile\nmake lint test" || !strings.HasSuffix(f.runs[0].dir, "gates/docs") {
		t.Fatalf("runs = %+v, want the gates of org/app once, in the docs worker's gate tree, after the machine's init", f.runs)
	}
	want := []status{
		{"org/app", "aaa111", "pending", "running on box"},
		{"org/app", "aaa111", "success", "passed in 2m on box"},
	}
	if !slices.Equal(f.gh.statuses, want) {
		t.Fatalf("statuses = %+v, want %+v", f.gh.statuses, want)
	}
	if g := f.gates(t); len(g) != 1 || g[0].PR != 41 || g[0].Worker != "docs" || !g[0].Passed() {
		t.Fatalf("records = %+v", g)
	}
}

func TestACommitIsGatedOnce(t *testing.T) {
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111"}
	f.Tick(context.Background())
	f.Tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("ran %d times, want once", len(f.runs))
	}

	// A new push is a new commit.
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "ccc333"}}
	f.tree.commits["/w/app"] = append(f.tree.commits["/w/app"], "ccc333")
	f.Tick(context.Background())
	if len(f.runs) != 2 {
		t.Fatalf("ran %d times after a new push, want twice", len(f.runs))
	}
}

func TestAFailureIsPostedWithItsOutputAndAPassReplacesIt(t *testing.T) {
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111", "ccc333"}
	f.exit, f.out = 2, "ok  pkg/a\n--- FAIL: TestExport\n    export_test.go:12: got 3, want 4\nFAIL pkg/b\n"
	f.Tick(context.Background())

	last := f.gh.statuses[len(f.gh.statuses)-1]
	if last.state != "failure" || last.description != "failed in 2m on box: FAIL pkg/b" {
		t.Fatalf("status = %+v", last)
	}
	body := f.gh.comments[41]
	for _, want := range []string{Marker, "failed on aaa111", "export_test.go:12: got 3, want 4", "gates/aaa111.log"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}

	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "ccc333"}}
	f.exit, f.out = 0, "all good\n"
	f.Tick(context.Background())
	if body := f.gh.comments[41]; !strings.Contains(body, "passed on ccc333") || strings.Contains(body, "got 3") {
		t.Fatalf("comment after a pass:\n%s", body)
	}
}

func TestAPassWithNoEarlierFailurePostsNoComment(t *testing.T) {
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111"}
	f.Tick(context.Background())
	if len(f.gh.comments) != 0 {
		t.Fatalf("comments = %v, want none for a first pass", f.gh.comments)
	}
}

func TestDraftsAndReposWithoutGatesAreLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.Machine.Issues = append(f.Machine.Issues, config.IssueSource{Repo: "org/site", Label: "machine:box"})
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111", Draft: true}}
	f.gh.pulls["org/site"] = []Pull{{Number: 7, Head: "ddd444"}}
	f.tree.commits["/w/app"] = []string{"aaa111", "ddd444"}
	f.Tick(context.Background())
	if len(f.runs) != 0 {
		t.Fatalf("runs = %+v, want none", f.runs)
	}
}

func TestABusyMachineHoldsTheGatesUntilMaxWait(t *testing.T) {
	f := newFixture(t)
	f.Machine.Capacity = config.Capacity{MaxLoad: 4, MaxWait: "20m"}
	f.busy = "load 9.1 is over 4"
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111"}
	f.Tick(context.Background())
	f.now = f.now.Add(10 * time.Minute)
	f.Tick(context.Background())
	if len(f.runs) != 0 {
		t.Fatalf("ran on a busy machine before max wait")
	}
	f.now = f.now.Add(11 * time.Minute)
	f.Tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("ran %d times after max wait, want once", len(f.runs))
	}
}

func TestACheckoutThatFailsPostsNothingAndIsTriedAgainLater(t *testing.T) {
	// A worker cannot fix a broken gate tree: a status would send it back
	// to its pull request for nothing.
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111"}
	f.tree.checkoutErr = errFake("worktree is locked")
	f.Tick(context.Background())
	if len(f.gh.statuses) != 0 || len(f.runs) != 0 || len(f.gates(t)) != 0 {
		t.Fatalf("statuses = %+v, runs = %+v after a failed checkout", f.gh.statuses, f.runs)
	}
	f.tree.checkoutErr = nil
	f.now = f.now.Add(5 * time.Minute)
	f.Tick(context.Background())
	if len(f.runs) != 0 {
		t.Fatalf("tried again after 5m, want 10m")
	}
	f.now = f.now.Add(5 * time.Minute)
	f.Tick(context.Background())
	if len(f.runs) != 1 {
		t.Fatalf("ran %d times after 10m, want once", len(f.runs))
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestParsePullsReadsTheHeadOfEachPullRequest(t *testing.T) {
	pulls, err := parsePulls([]byte(`[{"number":41,"headRefOid":"aaa111","isDraft":false},{"number":42,"headRefOid":"bbb222","isDraft":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []Pull{{41, "aaa111", false}, {42, "bbb222", true}}; !slices.Equal(pulls, want) {
		t.Fatalf("pulls = %+v, want %+v", pulls, want)
	}
}

func TestARunCutShortByTheAgentStoppingPostsNoResultAndRunsAgain(t *testing.T) {
	f := newFixture(t)
	f.gh.pulls["org/app"] = []Pull{{Number: 41, Head: "aaa111"}}
	f.tree.commits["/w/app"] = []string{"aaa111"}
	ctx, cancel := context.WithCancel(context.Background())
	f.Run = func(context.Context, string, string, time.Duration) (string, int) {
		cancel()
		return "shed: stopped because the agent shut down", -1
	}
	f.Tick(ctx)
	if len(f.gates(t)) != 0 || len(f.gh.statuses) != 1 || f.gh.statuses[0].state != "pending" {
		t.Fatalf("records = %+v, statuses = %+v; want only the pending status", f.gates(t), f.gh.statuses)
	}
}
