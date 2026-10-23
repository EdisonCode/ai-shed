package status

import (
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// agentQueues gives the probe the queue each named worker's agent kept.
func agentQueues(res *probe.Result, views ...runlog.QueueView) {
	res.Queues = map[string]runlog.QueueView{}
	for _, v := range views {
		res.Queues[v.Worker] = v
	}
}

func TestStatusShowsAnIssueTheAgentHoldsWithTheAgentsReason(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}, {Name: "docs"}}}
	res := healthy()
	res.Checkins = []runlog.Checkin{{Time: now.Add(-time.Hour), Worker: "app", Kind: "idle", Verdict: runlog.VerdictDone, Reason: "nothing workable"}}
	paced := runlog.QueueEntry{Repo: "org/app", Number: 7, Hold: "paced: the account is ahead of its weekly pace"}
	agentQueues(res,
		runlog.QueueView{Time: res.Now, Worker: "app", Items: []runlog.QueueEntry{paced}, Reserve: "not listed: the worker took 4 issues from it in the last 1h"},
		runlog.QueueView{Time: res.Now, Worker: "docs", Items: []runlog.QueueEntry{paced}})
	r := Assess(m, signals, now, res, []backlog.Issue{issue(7)})
	MarkAgentHolds(&r)
	if got := r.Issues[0]; got.State != Held || got.Held != "agent: "+paced.Hold {
		t.Fatalf("#7 = %s %q, want held with the agent's reason", got.State, got.Held)
	}
	wantAttention(t, r, "1 of 2 workers have no work: the agent holds 1 queued issue(s), first #7 paced: the account is ahead of its weekly pace; the reserve of app was not listed: the worker took 4 issues from it in the last 1h. `shed queue box` lists each worker's queue as the agent sees it")
}

func TestAnIssueOneWorkerCanTakeStaysQueued(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}, {Name: "docs"}}}
	res := healthy()
	agentQueues(res,
		runlog.QueueView{Time: res.Now, Worker: "app", Items: []runlog.QueueEntry{{Repo: "org/app", Number: 7, Hold: "held: overlaps #9"}}},
		runlog.QueueView{Time: res.Now, Worker: "docs", Items: []runlog.QueueEntry{{Repo: "org/app", Number: 7}}})
	r := Assess(m, signals, now, res, []backlog.Issue{issue(7)})
	MarkAgentHolds(&r)
	if got := r.Issues[0]; got.State != Queued {
		t.Fatalf("#7 = %s %q, want queued: docs can take it", got.State, got.Held)
	}
}

func TestAQueueTheAgentWroteLongAgoDoesNotSpeakForIt(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}}}
	res := healthy()
	agentQueues(res, runlog.QueueView{Time: res.Now.Add(-2 * time.Hour), Worker: "app", Items: []runlog.QueueEntry{{Repo: "org/app", Number: 7, Hold: "held: overlaps #9"}}})
	r := Assess(m, signals, now, res, []backlog.Issue{issue(7)})
	MarkAgentHolds(&r)
	if got := r.Issues[0]; got.State != Queued {
		t.Fatalf("#7 = %s %q, want queued: the agent's queue is two hours old", got.State, got.Held)
	}
}
