package backlog

import (
	"testing"
	"time"
)

func TestTheIssuesWorkersOpenPastTheDaysQuotaAreOverIt(t *testing.T) {
	day := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	filed := []Filed{
		{Number: 14, CreatedAt: day.Add(3 * time.Hour)},
		{Number: 12, CreatedAt: day.Add(time.Hour)},
		{Number: 13, CreatedAt: day.Add(2 * time.Hour)},
		{Number: 20, CreatedAt: day.Add(24 * time.Hour)},
	}
	over := OverQuota(filed, 2, time.UTC)
	if len(over) != 1 || !over[14] {
		t.Fatalf("over = %v, want #14 only: the third of its day; #20 opened the next day", over)
	}
	if got := OverQuota(filed, 0, time.UTC); len(got) != 0 {
		t.Fatalf("no quota: over = %v", got)
	}
}

func TestOnlyIssuesWithTheMarkerCountAsFiledByAWorker(t *testing.T) {
	data := `[{"number":12,"body":"Found while building #9\n\nThe export times out.","createdAt":"2026-11-24T10:00:00Z"},
{"number":13,"body":"We found while building the page that it is slow.","createdAt":"2026-11-24T11:00:00Z"}]`
	filed, err := parseFiled([]byte(data), "org/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 1 || filed[0].Number != 12 {
		t.Fatalf("filed = %+v, want #12 only", filed)
	}
}
