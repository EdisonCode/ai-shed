package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/runlog"
)

func TestQueueSaysWhyEachItemIsHeldAndWhatTheReserveDid(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	queues := map[string]runlog.QueueView{"app": {Time: now.Add(-3 * time.Minute), Worker: "app", Reserve: "not listed: #3 can be acted on", Items: []runlog.QueueEntry{
		{Repo: "org/app", Number: 3, Title: "Fix login", Kind: "rework"},
		{Repo: "org/app", Number: 7, Title: "Add export", Hold: "paced: the account is ahead of its weekly pace"},
	}}}
	var out bytes.Buffer
	renderQueue(&out, "box", "app", queues, now)
	renderQueue(&out, "box", "docs", queues, now)
	for _, want := range []string{
		"box worker app: 1 of 2 item(s) workable, read 3m ago",
		"org/app#7  ",
		"paced: the account is ahead of its weekly pace",
		"reserve: not listed: #3 can be acted on",
		"box worker docs: no queue yet",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}
