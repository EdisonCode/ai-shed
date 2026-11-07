package supervisor

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const handOver = "Your context was cleared. Read .shed/BRIEF.md first, then go on. One-off task from the owner, ahead of your queue: read .shed/tasks/t1.md in full and carry it out."

// inputBox draws a tool's screen with the given lines in its input box.
func inputBox(above string, box ...string) string {
	rule := strings.Repeat("─", 60)
	return above + "\n" + rule + "\n" + strings.Join(box, "\n") + "\n" + rule + "\n  ? for shortcuts   Update installed · /restart to apply\n"
}

func TestAMessageWrappedInTheInputBoxIsSeenUnsent(t *testing.T) {
	screen := inputBox("", "> Your context was cleared. Read .shed/BRIEF.md first, then go", "  on. One-off task from the owner, ahead of your queue: read")
	if !unsentInput(screen, handOver) {
		t.Fatalf("the hand-over in the box was not seen:\n%s", screen)
	}
}

func TestAMessageInARoundedInputBoxIsSeenUnsent(t *testing.T) {
	screen := "╭" + strings.Repeat("─", 40) + "╮\n│ > Your context was cleared. Read .shed/BRIEF.md first │\n╰" + strings.Repeat("─", 40) + "╯\n"
	if !unsentInput(screen, handOver) {
		t.Fatalf("the hand-over in the box was not seen:\n%s", screen)
	}
}

func TestAMessageEchoedAboveAnEmptyBoxWasSent(t *testing.T) {
	screen := inputBox("> "+handOver+"\n\n✻ Reading .shed/BRIEF.md…", "> ")
	if unsentInput(screen, handOver) {
		t.Fatalf("a sent hand-over was seen unsent:\n%s", screen)
	}
}

func TestAPlaceholderInTheBoxIsNotTheMessage(t *testing.T) {
	screen := inputBox("", "> "+faintOpen+"Your context was cleared. Read .shed/BRIEF.md first"+faintClose)
	if unsentInput(screen, handOver) {
		t.Fatalf("the tool's placeholder was read as typed text:\n%s", screen)
	}
}

func TestAScreenWithNoInputBoxTellsNothing(t *testing.T) {
	if unsentInput("> "+handOver+"\n", handOver) {
		t.Fatal("a screen with no box was read as an unsent message")
	}
}

func TestATaskHandOverLeftUnsentIsSubmittedWithoutAskingTheReviewer(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done)
	f.tick(2 * time.Minute) // found done; it rests
	f.pushTask(t, runlog.TaskAny, "t1", "Audit the export.")
	f.tick(time.Minute)
	if !slices.Equal(f.term.sent, []string{taskMessage}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	reviews := len(f.reviewer.prompts)
	f.term.screen = inputBox("", "> "+taskMessage)
	f.term.running(f.now)
	f.tick(2 * time.Minute)

	if f.term.submitted != 1 || len(f.reviewer.prompts) != reviews {
		t.Fatalf("submitted %d, reviews %d (were %d); want one Enter and no review", f.term.submitted, len(f.reviewer.prompts), reviews)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictUnsent || !strings.Contains(c.Reason, "Enter was pressed once") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestAClearLeftTypedIsEmptiedAndTypedAgainBeforeTheHandOver(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12))
	f.term.sticks = map[string]int{"/clear": 1}
	f.tick(2 * time.Minute)

	if f.term.interrupts != 1 || len(f.term.sent) != 4 || f.term.sent[0] != "/clear" || f.term.sent[1] != "/clear" || f.term.sent[2] != "/model sonnet" {
		t.Fatalf("interrupts %d, sent = %q; want the input emptied once and /clear typed again", f.term.interrupts, f.term.sent)
	}
	if c := f.lastCheckin(t); c.Issue != 12 || !c.Sent {
		t.Fatalf("check-in = %+v, want #12 handed over", c)
	}
}

func TestAModelCommandLeftTypedIsEmptiedAndTypedAgain(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12))
	f.term.sticks = map[string]int{"/model sonnet": 1}
	f.tick(2 * time.Minute)

	if f.term.interrupts != 1 || !slices.Equal(f.term.sent[:3], []string{"/clear", "/model sonnet", "/model sonnet"}) {
		t.Fatalf("interrupts %d, sent = %q", f.term.interrupts, f.term.sent)
	}
}

func TestAClearThatStaysTypedEndsTheSessionAndTypesNoHandOver(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12))
	f.term.sticks = map[string]int{"/clear": 2}
	f.tick(2 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"/clear", "/clear"}) {
		t.Fatalf("sent = %q; nothing may be typed on top of a command that did not run", f.term.sent)
	}
	if !slices.Equal(f.term.closed, []string{"app"}) {
		t.Fatalf("closed = %q, want the session ended", f.term.closed)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictRecycled || !strings.Contains(c.Reason, `"/clear" stayed typed`) {
		t.Fatalf("check-in = %+v, want recycled with the command named", c)
	}
}

func TestAMessageTypedBehindACommandIsNotSubmitted(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done)
	f.tick(2 * time.Minute) // found done; it rests
	f.pushTask(t, runlog.TaskAny, "t1", "Audit the export.")
	f.tick(time.Minute)
	reviews := len(f.reviewer.prompts)
	f.term.screen = inputBox("", "> /clear "+taskMessage)
	f.term.running(f.now)
	f.tick(2 * time.Minute)

	if f.term.submitted != 0 || len(f.reviewer.prompts) != reviews {
		t.Fatalf("submitted %d, reviews %d (were %d); Enter would send the command and the message as one", f.term.submitted, len(f.reviewer.prompts), reviews)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictRecycled || !strings.Contains(c.Reason, "behind other text") {
		t.Fatalf("check-in = %+v, want recycled", c)
	}
}

func TestAMessageBehindOtherTextIsSeenStacked(t *testing.T) {
	screen := inputBox("", "> /clear "+handOver)
	if !stackedInput(screen, handOver) || unsentInput(screen, handOver) {
		t.Fatalf("stacked %v, unsent %v; want stacked only:\n%s", stackedInput(screen, handOver), unsentInput(screen, handOver), screen)
	}
}
