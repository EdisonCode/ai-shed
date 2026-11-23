package supervisor

import (
	"fmt"
	"slices"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	// messageWindow is the time over which a worker's messages to one
	// session are counted.
	messageWindow = time.Hour
	// loopAfterTold is how many more messages to the same session, after the
	// worker was told to stop, make it stuck.
	loopAfterTold = 3
	// stopMessaging is typed into a worker that sent one session too many
	// messages within the hour.
	stopMessaging = "You sent %d messages to %s in the last hour. Stop messaging %s: two sessions that answer each other can go on without end, and each message costs usage. Put what is left to settle on the issue, and go on with your work."
)

// messageLoop tells a worker that messages one session past the hourly
// limit to stop, once per hour and session. A worker that goes on after it
// was told is stuck: the owner has to end the exchange. It reports whether
// it recorded a check-in.
func (s *Supervisor) messageLoop(w config.Worker, st *workerState, now time.Time) (bool, error) {
	limit := s.Settings.MessageLimitOrDefault()
	if limit == 0 {
		return false, nil
	}
	sent, err := runlog.Messages(s.StateDir, w.Name)
	if err != nil {
		return false, err
	}
	counts := runlog.CountSince(sent, now.Add(-messageWindow))
	targets := make([]string, 0, len(counts))
	for to := range counts {
		targets = append(targets, to)
	}
	slices.Sort(targets)
	for _, to := range targets {
		if counts[to] < limit {
			continue
		}
		told, ok := st.messagesTold[to]
		if !ok || now.Sub(told) >= messageWindow {
			c := runlog.Checkin{Time: now, Worker: w.Name, Kind: KindMessages, Verdict: runlog.VerdictNudge,
				Reason:  fmt.Sprintf("it sent %d messages to %s in the last hour, at or above the limit of %d", counts[to], to, limit),
				Message: fmt.Sprintf(stopMessaging, counts[to], to, to)}
			if err := s.Terminal.Send(w.Name, c.Message); err != nil {
				return false, err
			}
			c.Sent = true
			if st.messagesTold == nil {
				st.messagesTold = map[string]time.Time{}
			}
			st.messagesTold[to] = now
			delete(st.messagesStuck, to)
			s.record(st, c)
			return true, nil
		}
		after := runlog.CountSince(sent, told.Add(time.Nanosecond))[to]
		if after >= loopAfterTold && !st.messagesStuck[to] {
			if st.messagesStuck == nil {
				st.messagesStuck = map[string]bool{}
			}
			st.messagesStuck[to] = true
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindMessages, Verdict: runlog.VerdictStuck,
				Reason: fmt.Sprintf("it sent %s %d more messages after it was told to stop: end the exchange, for example by recycling one of the two sessions", to, after)})
			return true, nil
		}
	}
	return false, nil
}
