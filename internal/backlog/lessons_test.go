package backlog

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestLessonsAreReadFromHandBacksUntilTheOwnerSettlesThem(t *testing.T) {
	signals := config.DefaultSignals()
	i := Issue{Repo: "org/app", Number: 12, Comments: []Comment{
		{Body: "## Hand-back\n**PR:** #41\n**Lessons:**\n- The integration suite needs `make db-reset` first.\n- gh pr checkout fails in a worktree; use git fetch.\n\n**Decisions needed:** none"},
	}}
	got := Lessons(i, signals)
	want := []string{"The integration suite needs `make db-reset` first.", "gh pr checkout fails in a worktree; use git fetch."}
	if len(got) != 2 || got[0].Text != want[0] || got[1].Text != want[1] {
		t.Fatalf("lessons = %+v, want %q", got, want)
	}
	i.Comments = append(i.Comments, Comment{Body: "Lessons promoted: both, into the testing memory."})
	if got := Lessons(i, signals); len(got) != 0 {
		t.Fatalf("lessons after the owner settled them = %+v", got)
	}
	i.Comments = append(i.Comments, Comment{Body: "## Hand-back\n**PR:** #41\n**Lessons:** none"})
	if got := Lessons(i, signals); len(got) != 0 {
		t.Fatalf("lessons = %+v for none", got)
	}
}

func TestALessonOutsideAHandBackIsNotOne(t *testing.T) {
	i := Issue{Comments: []Comment{{Body: "Lessons: we should talk about this"}}}
	if got := Lessons(i, config.DefaultSignals()); len(got) != 0 {
		t.Fatalf("lessons = %+v", got)
	}
	if LessonKey("  The suite needs make db-reset first. ") != LessonKey("the suite needs  make db-reset first") {
		t.Fatal("two spellings of one lesson have different keys")
	}
}
