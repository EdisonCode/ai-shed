package looks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// GH asks GitHub with the GitHub CLI, so shed needs no token of its own.
type GH struct{}

func (GH) ClosedSince(ctx context.Context, src config.IssueSource, since time.Time) ([]backlog.Issue, error) {
	return backlog.GH{}.Closed(ctx, src, since)
}

func (GH) MergedSince(ctx context.Context, repo string, since time.Time) (map[int]Pull, error) {
	out, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "merged", "--limit", "1000",
		"--search", "merged:>="+since.UTC().Format("2006-01-02"), "--json", "number,title,mergedAt,mergeCommit,baseRefName")
	if err != nil {
		return nil, err
	}
	return parseMerged(out, since)
}

func (GH) Pull(ctx context.Context, repo string, number int) (Pull, error) {
	out, err := gh(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "number,title,mergedAt,mergeCommit,baseRefName")
	if err != nil {
		return Pull{}, err
	}
	pulls, err := parseMerged(append(append([]byte("["), out...), ']'), time.Time{})
	if err != nil {
		return Pull{}, err
	}
	return pulls[number], nil
}

// parseMerged keeps the pull requests merged after since: the search is by
// day, the window is to the second.
func parseMerged(data []byte, since time.Time) (map[int]Pull, error) {
	var prs []struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		MergedAt    time.Time `json:"mergedAt"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: parse output: %w", err)
	}
	merged := map[int]Pull{}
	for _, pr := range prs {
		if pr.MergedAt.After(since) && pr.MergeCommit.OID != "" {
			merged[pr.Number] = Pull{Number: pr.Number, Title: pr.Title, MergedAt: pr.MergedAt, Commit: pr.MergeCommit.OID, Base: pr.BaseRefName}
		}
	}
	return merged, nil
}

func (GH) CommitTime(ctx context.Context, repo, commit string) (time.Time, error) {
	out, err := gh(ctx, "api", "repos/"+repo+"/commits/"+commit, "--jq", ".commit.committer.date")
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: commit %s has no readable date: %w", repo, commit, err)
	}
	return t, nil
}

func (GH) Contains(ctx context.Context, repo, head, commit string) (bool, error) {
	// The comparison says where head stands relative to commit. The first
	// page carries every changed file with its patch, which runs to
	// megabytes on a range far behind; the second page of one commit has no
	// files and the same status.
	out, err := gh(ctx, "api", "repos/"+repo+"/compare/"+commit+"..."+head+"?per_page=1&page=2", "--jq", ".status")
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(out))
	return status == "ahead" || status == "identical", nil
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		return nil, ghError(ctx, args, err, stderr.String(), time.Since(start))
	}
	return out, nil
}

// ghError says why a gh call failed. A call shed killed at its own deadline
// says so, so it is not read as a fault of gh or of its login.
func ghError(ctx context.Context, args []string, err error, stderr string, took time.Duration) error {
	call := "gh " + strings.Join(args[:min(len(args), 4)], " ")
	if ctx.Err() != nil {
		return fmt.Errorf("%s: timed out at shed's deadline after %s", call, took.Round(100*time.Millisecond))
	}
	return fmt.Errorf("%s: %w: %s", call, err, strings.TrimSpace(stderr))
}

// pullTimesBatch is how many pull requests one GraphQL call asks about.
const pullTimesBatch = 50

func (GH) PullTimes(ctx context.Context, repo string, numbers []int) (map[int]PullTimes, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("pull request times: %q is not owner/name", repo)
	}
	times := map[int]PullTimes{}
	for start := 0; start < len(numbers); start += pullTimesBatch {
		var fields strings.Builder
		for _, n := range numbers[start:min(start+pullTimesBatch, len(numbers))] {
			fmt.Fprintf(&fields, "p%d: pullRequest(number: %d) { %s }\n", n, n, pullTimesFields)
		}
		query := "query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {\n" + fields.String() + "} }"
		out, err := gh(ctx, "api", "graphql", "-F", "owner="+owner, "-F", "name="+name, "-f", "query="+query)
		if err != nil {
			return nil, fmt.Errorf("%s: pull request times: %w", repo, err)
		}
		batch, err := parsePullTimes(out)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
		for n, t := range batch {
			times[n] = t
		}
	}
	return times, nil
}

const pullTimesFields = `createdAt mergedAt
first: commits(first: 1) { nodes { commit { authoredDate } } }
head: commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
  __typename ... on CheckRun { startedAt completedAt } ... on StatusContext { createdAt } } } } } } }
timelineItems(itemTypes: [READY_FOR_REVIEW_EVENT], last: 1) { nodes { ... on ReadyForReviewEvent { createdAt } } }`

// parsePullTimes reads the answer to a PullTimes query, whose pull requests
// are named p<number>.
func parsePullTimes(data []byte) (map[int]PullTimes, error) {
	type commitNodes struct {
		Nodes []struct {
			Commit struct {
				AuthoredDate      time.Time `json:"authoredDate"`
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []struct {
							StartedAt   *time.Time `json:"startedAt"`
							CompletedAt *time.Time `json:"completedAt"`
							CreatedAt   *time.Time `json:"createdAt"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	}
	var answer struct {
		Data struct {
			Repository map[string]*struct {
				CreatedAt     time.Time   `json:"createdAt"`
				MergedAt      time.Time   `json:"mergedAt"`
				First         commitNodes `json:"first"`
				Head          commitNodes `json:"head"`
				TimelineItems struct {
					Nodes []struct {
						CreatedAt time.Time `json:"createdAt"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return nil, fmt.Errorf("pull request times: parse output: %w", err)
	}
	times := map[int]PullTimes{}
	for alias, pr := range answer.Data.Repository {
		var n int
		if pr == nil || !strings.HasPrefix(alias, "p") {
			continue
		}
		if _, err := fmt.Sscanf(alias[1:], "%d", &n); err != nil {
			continue
		}
		t := PullTimes{Ready: pr.CreatedAt, Merged: pr.MergedAt}
		if len(pr.TimelineItems.Nodes) > 0 {
			t.Ready = pr.TimelineItems.Nodes[0].CreatedAt
		}
		if len(pr.First.Nodes) > 0 {
			t.FirstCommit = pr.First.Nodes[0].Commit.AuthoredDate
		}
		if len(pr.Head.Nodes) > 0 && pr.Head.Nodes[0].Commit.StatusCheckRollup != nil {
			for _, c := range pr.Head.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes {
				// A status has only the time it was set: its start and its end.
				started, done := c.StartedAt, c.CompletedAt
				if c.CreatedAt != nil {
					started, done = c.CreatedAt, c.CreatedAt
				}
				if started != nil && (t.ChecksStarted.IsZero() || started.Before(t.ChecksStarted)) {
					t.ChecksStarted = *started
				}
				if done != nil && done.After(t.ChecksDone) {
					t.ChecksDone = *done
				}
			}
		}
		times[n] = t
	}
	return times, nil
}
