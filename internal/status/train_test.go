package status

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

func withTrain() *config.Config {
	cfg := withRepo(false)
	cfg.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{MergeLow: true, MergeTrain: true}}}
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
