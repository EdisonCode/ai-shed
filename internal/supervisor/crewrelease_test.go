package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// crewQueue gives the fixture a crew worker with a crew-mate on another
// machine, #12 in its hands, and #13, a slice of goal:export, waiting.
func crewQueue(t *testing.T) (*fixture, *fakeReleaser) {
	f := started(t, config.Supervisor{CacheTTL: "1h", GoalLabels: "goal:", CrewReleaseAfter: "20m"}, assign(12), onTrack)
	f.Account = &config.Account{Name: "max-1", Machines: []string{"box"}, Overflow: "fleet:ready"}
	f.Machine.Workers[0].Crew = true
	f.Machine.Workers[0].PeerList = []config.Peer{{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true}}
	releaser := &fakeReleaser{}
	f.Releaser = releaser
	f.lister.issues = []backlog.Issue{labeled(12, "machine:box"), labeled(13, "machine:box", "goal:export")}
	f.tick(2 * time.Minute) // #12 is handed over
	return f, releaser
}

func TestASliceWaitingBehindBusyBuildersGoesToTheCrewElsewhere(t *testing.T) {
	f, releaser := crewQueue(t)
	// Every builder has been busy since #12 was handed over.
	f.tick(time.Minute)
	f.tick(18 * time.Minute)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v before crew_release_after", releaser.released)
	}
	f.tick(2 * time.Minute)
	if len(releaser.released) != 1 {
		t.Fatalf("released = %+v, want the slice after 20m", releaser.released)
	}
	r := releaser.released[0]
	if r.number != 13 || r.add != "fleet:ready" || !strings.Contains(r.comment, backlog.CrewReleasePhrase) || !strings.Contains(r.comment, "goal:export") {
		t.Fatalf("release = %+v", r)
	}
}

func TestAnIssueOfNoGoalStaysWithItsMachine(t *testing.T) {
	f, releaser := crewQueue(t)
	f.lister.issues = []backlog.Issue{labeled(12, "machine:box"), labeled(13, "machine:box")}
	f.tick(time.Minute)
	f.tick(time.Hour)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v, want nothing: no goal", releaser.released)
	}
}

func TestWithNoCrewMateElsewhereNothingIsReleased(t *testing.T) {
	f, releaser := crewQueue(t)
	f.Machine.Workers[0].PeerList = []config.Peer{{Machine: "box", Worker: "api", Session: "box-api"}}
	f.tick(time.Minute)
	f.tick(time.Hour)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v, want nothing: the crew is all on this machine", releaser.released)
	}
}

func TestASliceReleasedForTheCrewIsLeftForACrewWorker(t *testing.T) {
	released := labeled(20, "fleet:ready", "goal:export")
	released.Comments = []backlog.Comment{{Body: "Released: mini, every worker busy for 20m while this slice of goal:export waits: released " + backlog.CrewReleasePhrase + "."}}
	if !backlog.ReleasedForCrew(released) {
		t.Fatal("the release is not read as for the crew")
	}
	released.Comments = append(released.Comments, backlog.Comment{Body: "## Hand-back\n**PR:** #41 (ready)"})
	if backlog.ReleasedForCrew(released) {
		t.Fatal("a hand-back after the release still marks it for the crew")
	}
}
