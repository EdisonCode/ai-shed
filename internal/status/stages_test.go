package status

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/looks"
)

func TestStagesSplitAPullRequestIntoBuildChecksAndWaits(t *testing.T) {
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	got := stagesOf(looks.PullTimes{FirstCommit: at(0), Ready: at(40), ChecksStarted: at(41), ChecksDone: at(52), Merged: at(352)})
	want := Stages{Build: 40 * time.Minute, Checks: 11 * time.Minute, Waits: 300 * time.Minute}
	if got != want {
		t.Fatalf("stages = %+v, want %+v", got, want)
	}
}

func TestAPullRequestWithNoChecksWaitsFromReady(t *testing.T) {
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	got := stagesOf(looks.PullTimes{FirstCommit: at(0), Ready: at(10), Merged: at(70)})
	if got.Checks != 0 || got.Waits != time.Hour {
		t.Fatalf("stages = %+v, want no checks and a wait of 1h", got)
	}
}

func TestChecksThatFinishBeforeReadyWaitFromReady(t *testing.T) {
	// A draft whose checks went green before it was marked ready waited on
	// nobody until it was.
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	got := stagesOf(looks.PullTimes{FirstCommit: at(0), ChecksStarted: at(5), ChecksDone: at(15), Ready: at(60), Merged: at(90)})
	if got.Waits != 30*time.Minute {
		t.Fatalf("waits = %s, want 30m", got.Waits)
	}
}

func TestASpanThatRunsBackwardsIsLeftOut(t *testing.T) {
	// A commit dated after the pull request opened (a rebase that rewrote
	// the author date) says nothing of how long the build took.
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	got := stagesOf(looks.PullTimes{FirstCommit: at(20), Ready: at(10), Merged: at(30)})
	if got.Build != 0 || got.Waits != 20*time.Minute {
		t.Fatalf("stages = %+v, want no build and a wait of 20m", got)
	}
}

type fakeTimes struct {
	times map[int]looks.PullTimes
	err   error
}

func (f fakeTimes) PullTimes(_ context.Context, _ string, numbers []int) (map[int]looks.PullTimes, error) {
	return f.times, f.err
}

func TestDigestSaysWhereThePullRequestsTimeWent(t *testing.T) {
	at := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	merged := &fakeMerged{pulls: map[int]looks.Pull{
		50: {Number: 50, Title: "Retry the export", MergedAt: at(-5)},
		51: {Number: 51, Title: "Paginate the audit log", MergedAt: at(-1)},
		52: {Number: 52, Title: "Show the total", MergedAt: at(-2)},
	}}
	times := fakeTimes{times: map[int]looks.PullTimes{
		50: {FirstCommit: at(-9), Ready: at(-8), ChecksStarted: at(-8), ChecksDone: at(-7.5), Merged: at(-5)},
		51: {FirstCommit: at(-4), Ready: at(-3.5), ChecksStarted: at(-3.5), ChecksDone: at(-3.25), Merged: at(-1)},
		52: {FirstCommit: at(-6), Ready: at(-3), Merged: at(-2)},
	}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Times: times}
	d := c.Digest(context.Background(), withRepo(true), merged, now.Add(-24*time.Hour), now)
	if len(d.Stages) != 3 {
		t.Fatalf("stages = %+v, want one per merged pull request", d.Stages)
	}
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Where the time went: 3 merged pull request(s), median and worst",
		"build   1h   3h   org/app#52  first commit to ready for review",
		"checks  22m  30m  org/app#50  the merged head's checks, first started to last finished",
		"waits   2h   2h   org/app#50  checks finished or ready, whichever is later, to merged",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDigestSaysWhenThePullRequestsTimesCouldNotBeRead(t *testing.T) {
	merged := &fakeMerged{pulls: map[int]looks.Pull{50: {Number: 50, Title: "Retry the export", MergedAt: now.Add(-time.Hour)}}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Times: fakeTimes{err: errors.New("gh api graphql: rate limited")}}
	d := c.Digest(context.Background(), withRepo(true), merged, now.Add(-24*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	if !strings.Contains(out.String(), "! gh api graphql: rate limited") {
		t.Fatalf("digest does not say the times could not be read:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Where the time went") {
		t.Fatalf("digest shows stages it could not read:\n%s", out.String())
	}
}
