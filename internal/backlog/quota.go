package backlog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Filed is an issue a worker opened: it carries the FoundWhilePhrase marker.
type Filed struct {
	Number    int
	CreatedAt time.Time
}

// FiledSince lists the repository's issues opened since a time that carry
// the marker of an issue a worker opened, open or closed.
func (GH) FiledSince(ctx context.Context, repo string, since time.Time) ([]Filed, error) {
	out, err := gh(ctx, "issue", "list", "--repo", repo, "--state", "all", "--limit", "500",
		"--search", fmt.Sprintf(`"%s" in:body created:>=%s`, FoundWhilePhrase, since.UTC().Format("2006-01-02")),
		"--json", "number,body,createdAt")
	if err != nil {
		return nil, err
	}
	return parseFiled(out, repo)
}

func parseFiled(data []byte, repo string) ([]Filed, error) {
	var issues []struct {
		Number    int       `json:"number"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(data, &issues); err != nil {
		return nil, fmt.Errorf("gh issue list %s filed by workers: parse output: %w", repo, err)
	}
	var filed []Filed
	for _, i := range issues {
		// The search matches the words anywhere; the marker names an issue.
		if FoundWhileBuilding(i.Body) {
			filed = append(filed, Filed{Number: i.Number, CreatedAt: i.CreatedAt})
		}
	}
	return filed, nil
}

// OverQuota returns the issues workers opened past the quota of their day,
// counted in order of opening, with days in loc. A quota of 0 or less holds
// nothing.
func OverQuota(filed []Filed, quota int, loc *time.Location) map[int]bool {
	over := map[int]bool{}
	if quota <= 0 {
		return over
	}
	sorted := slices.Clone(filed)
	slices.SortStableFunc(sorted, func(a, b Filed) int { return a.CreatedAt.Compare(b.CreatedAt) })
	perDay := map[string]int{}
	for _, f := range sorted {
		day := f.CreatedAt.In(loc).Format("2006-01-02")
		perDay[day]++
		if perDay[day] > quota {
			over[f.Number] = true
		}
	}
	return over
}

// Admitted reports whether the owner admitted the issue: a comment begins
// with the answering phrase of the ready signal, "Ready now" by default.
func Admitted(issue Issue, signals []config.Signal) bool {
	for _, s := range signals {
		if s.Name == config.ReadySignal && s.AnsweredBy != "" && lastComment(issue, s.AnsweredBy) >= 0 {
			return true
		}
	}
	return false
}
