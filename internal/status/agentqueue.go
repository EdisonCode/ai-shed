package status

import (
	"fmt"
	"time"

	"github.com/edisoncode/ai-shed/internal/runlog"
)

// agentQueueFresh is how old a worker's queue, as its agent last wrote it,
// may be and still speak for the agent. A resting worker's queue is read
// every few minutes; an older one is from an agent that stopped.
const agentQueueFresh = time.Hour

// MarkAgentHolds takes the agent's word over the watcher's for the issues
// the watcher finds queued: the agent knows the machine's own state (its
// pace, its log of what each worker has in hand) and the watcher does not.
// An issue that every worker's queue holds is held, with the agent's
// reason. When workers are idle beside such issues, it says why, with what
// became of their reserve.
func MarkAgentHolds(report *MachineReport) {
	if report.Probe == nil || len(report.Probe.Queues) == 0 {
		return
	}
	var views []runlog.QueueView
	for _, w := range report.Workers {
		v, ok := report.Probe.Queues[w.Name]
		if !w.Ask && ok && report.Probe.Now.Sub(v.Time) < agentQueueFresh {
			views = append(views, v)
		}
	}
	if len(views) == 0 {
		return
	}
	held, first := 0, ""
	for n := range report.Issues {
		st := &report.Issues[n]
		if st.State != Queued {
			continue
		}
		hold, seen := agentHold(views, st.Repo, st.Number)
		if !seen || hold == "" {
			continue
		}
		st.State, st.Held = Held, "agent: "+hold
		held++
		if first == "" {
			first = fmt.Sprintf("#%d %s", st.Number, hold)
		}
	}
	idle, builders := idleBuilders(*report)
	if idle == 0 {
		return
	}
	reserve := ""
	for _, v := range views {
		if v.Reserve != "" && workerIdle(report.Workers, v.Worker) {
			reserve = fmt.Sprintf("; the reserve of %s was %s", v.Worker, v.Reserve)
			break
		}
	}
	if held == 0 && reserve == "" {
		return
	}
	why := fmt.Sprintf("the agent holds %d queued issue(s), first %s", held, first)
	if held == 0 {
		why = "the agent holds no queued issue"
	}
	report.Attention = append(report.Attention, fmt.Sprintf("%d of %d workers have no work: %s%s. `shed queue %s` lists each worker's queue as the agent sees it", idle, builders, why, reserve, report.Name))
}

// agentHold returns why the agent holds the issue, and whether any worker's
// queue has it. It is held only when every queue that has it holds it: one
// worker that can take it is enough.
func agentHold(views []runlog.QueueView, repo string, number int) (hold string, seen bool) {
	for _, v := range views {
		for _, e := range v.Items {
			if e.Repo != repo || e.Number != number {
				continue
			}
			if e.Hold == "" {
				return "", true
			}
			if !seen {
				hold, seen = e.Hold, true
			}
		}
	}
	return hold, seen
}
