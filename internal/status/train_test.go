package status

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
)

func withTrain() *config.Config {
	cfg := withRepo(false)
	cfg.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{MergeLow: true, MergeTrain: config.TrainOne}}}
	return cfg
}

func TestTheReleaseStateShowsTheMergeTrain(t *testing.T) {
	inHand := issue(12, "## Hand-back\n**PR:** #41 (ready)", "Verified (low): ran the tests", backlog.QueuedPhrase+" pull request #41, verified at low risk.")
	next := issue(13, "## Hand-back\n**PR:** #44 (ready)", "Verified (low): tried an empty file")
	high := issue(14, "## Hand-back\n**PR:** #45 (ready)", "Verified (high): ran the migration")
	prs := map[int]backlog.PR{41: {Pending: true}, 44: {}, 45: {}}
	for _, i := range []*backlog.Issue{&inHand, &next, &high} {
		i.OpenPRs = prs
	}
	c := Collector{Lister: fakeLister{issues: []backlog.Issue{inHand, next, high}}, Looks: &fakeLooks{}}
	repos := c.CollectRepos(context.Background(), withTrain())
	var out bytes.Buffer
	Render(&out, nil, repos)
	for _, want := range []string{"org/app\n", "merge train: PR #41 (#12) in hand ", ", waiting for its checks; next PR #44 (#13)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "#45") {
		t.Fatalf("a high-risk pull request is shown on the train:\n%s", out.String())
	}
}

func TestAnEmptyMergeTrainSaysSo(t *testing.T) {
	c := Collector{Lister: fakeLister{}, Looks: &fakeLooks{}}
	var out bytes.Buffer
	Render(&out, nil, c.CollectRepos(context.Background(), withTrain()))
	if !strings.Contains(out.String(), "merge train: empty") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestARepositoryWithoutATrainShowsNone(t *testing.T) {
	var out bytes.Buffer
	Render(&out, nil, []RepoReport{{Report: releaseState(), Lookers: 1, Now: now}})
	if strings.Contains(out.String(), "merge train") {
		t.Fatalf("output:\n%s", out.String())
	}
}

type fakeBatches struct {
	batches []backlog.Batch
	heads   map[int]backlog.Head
}

func (f fakeBatches) Batches(context.Context, string) ([]backlog.Batch, error) { return f.batches, nil }

func (f fakeBatches) ReadHead(_ context.Context, _ string, pr int) (backlog.Head, error) {
	return f.heads[pr], nil
}

func TestTheReleaseStateShowsABatchWithItsPullRequestsChecksAndAge(t *testing.T) {
	cfg := withTrain()
	cfg.Repos[0].Verify.MergeTrain = config.TrainBatch
	boarded := func(n, pr int) backlog.Issue {
		i := issue(n, fmt.Sprintf("## Hand-back\n**PR:** #%d (ready)", pr), "Verified (low): ran the tests", fmt.Sprintf("%s pull request #%d, verified at low risk.", backlog.QueuedPhrase, pr))
		i.OpenPRs = map[int]backlog.PR{pr: {}}
		return i
	}
	batches := fakeBatches{
		batches: []backlog.Batch{{PR: 101, Created: time.Now().Add(-25 * time.Minute), Cars: []backlog.Car{{Issue: 12, PR: 41}, {Issue: 13, PR: 42}}}},
		heads:   map[int]backlog.Head{101: {Open: true, Checks: 2, Pending: true}},
	}
	c := Collector{Lister: fakeLister{issues: []backlog.Issue{boarded(12, 41), boarded(13, 42), boarded(14, 43)}}, Looks: &fakeLooks{}, Batches: batches}
	var out bytes.Buffer
	Render(&out, nil, c.CollectRepos(context.Background(), cfg))
	want := "merge train: batch PR #101 of PR #41 (#12), PR #42 (#13) 25m, checks running; PR #43 (#14) in hand "
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

func TestTheReleaseStateSaysABatchWhoseEveryCheckWasSkippedWasNotTested(t *testing.T) {
	cfg := withTrain()
	cfg.Repos[0].Verify.MergeTrain = config.TrainBatch
	batches := fakeBatches{
		batches: []backlog.Batch{{PR: 101, Created: time.Now().Add(-25 * time.Minute), Cars: []backlog.Car{{Issue: 12, PR: 41}}}},
		heads:   map[int]backlog.Head{101: {Open: true, Skipped: 5}},
	}
	c := Collector{Lister: fakeLister{}, Looks: &fakeLooks{}, Batches: batches}
	var out bytes.Buffer
	Render(&out, nil, c.CollectRepos(context.Background(), cfg))
	want := "batch PR #101 of PR #41 (#12) 25m, every check was skipped: its CI did not run on it"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

func TestABatchTheBaseKeepsMovingUnderWaitsOnTheOwner(t *testing.T) {
	cfg := withTrain()
	cfg.Repos[0].Verify.MergeTrain = config.TrainBatch
	batch := backlog.Batch{PR: 103, Created: time.Now().Add(-5 * time.Minute), Cars: []backlog.Car{{Issue: 12, PR: 41}}, Rebuilt: backlog.Rebuilt{Times: 1, Green: 1, Last: "d00d123"}}
	batches := fakeBatches{batches: []backlog.Batch{batch}, heads: map[int]backlog.Head{103: {Open: true, Checks: 2, Pending: true}}}
	c := Collector{Lister: fakeLister{}, Looks: &fakeLooks{}, Batches: batches}
	repos := c.CollectRepos(context.Background(), cfg)
	if got := repos[0].Attention(); len(got) != 0 {
		t.Fatalf("attention = %q after one rebuild, want none yet", got)
	}
	var out bytes.Buffer
	Render(&out, nil, repos)
	if want := "checks running, built again 1 time(s) for the base moving outside the train"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
	batches.batches[0].Rebuilt.Times = 2
	c.Batches = batches
	got := c.CollectRepos(context.Background(), cfg)[0].Attention()
	if len(got) != 1 || !strings.Contains(got[0], "batch PR #103 waits on you (base moved outside the train): built again 2 time(s), 1 of them after its checks passed") || !strings.Contains(got[0], "last d00d123") {
		t.Fatalf("attention = %q, want the batch the base keeps moving under named with its cause", got)
	}
}

func TestTheDigestNamesABatchTheBaseKeepsMovingUnder(t *testing.T) {
	train := &Train{Batches: []TrainBatch{{PR: 103, Cars: []TrainCar{{Issue: 12, PR: 41}}, Rebuilt: backlog.Rebuilt{Times: 3, Green: 2, Last: "d00d123"}}}}
	d := Digest{Since: now.Add(-24 * time.Hour), Now: now, Report: Report{Repos: []RepoReport{{Report: looks.Report{Repo: "org/app"}, Train: train}}}}
	var out bytes.Buffer
	RenderDigest(&out, d)
	if want := "! merge train org/app: batch PR #103 waits on you (base moved outside the train): built again 3 time(s), 2 of them after its checks passed"; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}
