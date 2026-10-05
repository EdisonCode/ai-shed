// Package looks finds the eye checks that merged work still owes: what is
// merged, is not yet in production, and nobody has looked at on staging.
package looks

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// Look states.
const (
	Awaiting = "awaiting" // merged, but staging does not serve it yet
	Due      = "due"      // staging serves it and nobody has looked
	Failed   = "failed"   // someone looked on staging and it did not pass
)

// commitTimeout bounds the owner's commit command, which usually asks a
// server over the network.
const commitTimeout = 20 * time.Second

// Look is the eye check of one issue's merged pull request.
type Look struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	PR     int    `json:"pr"`
	State  string `json:"state"`
	// Since is when the look was asked for, or when it failed.
	Since time.Time `json:"since"`
}

// Report is what one repository has merged since production and what of it
// still owes a look.
type Report struct {
	Repo string `json:"repo"`
	// Staging and Production are the commits the environments serve.
	// Production is empty when the fleet file has no command for it.
	Staging    string `json:"staging"`
	Production string `json:"production,omitempty"`
	// Since is where the window starts: production's commit, or the look-back
	// limit when that is later.
	Since time.Time `json:"since"`
	// Merged counts the pull requests merged in the window.
	Merged int    `json:"merged"`
	Looks  []Look `json:"looks"`
}

// GitHub is what the reader asks about a repository.
type GitHub interface {
	// ClosedSince returns the source's issues that were closed after since.
	ClosedSince(ctx context.Context, src config.IssueSource, since time.Time) ([]backlog.Issue, error)
	// MergedSince returns the merge commit of each pull request merged after
	// since, by pull request number.
	MergedSince(ctx context.Context, repo string, since time.Time) (map[int]string, error)
	CommitTime(ctx context.Context, repo, commit string) (time.Time, error)
	// Contains reports whether head has commit in its history.
	Contains(ctx context.Context, repo, head, commit string) (bool, error)
}

// Reader reads a repository's looks.
type Reader struct {
	GitHub GitHub
	// Run runs an owner's commit command and returns what it printed.
	Run func(ctx context.Context, command string) (string, error)
	Now func() time.Time

	// contained remembers whether a head has a commit: that never changes.
	contained map[string]bool
}

// Read reports on one repository. open holds the open issues of sources;
// issues of other repositories in it are ignored.
func (r *Reader) Read(ctx context.Context, repo config.Repo, sources []config.IssueSource, open []backlog.Issue, signals []config.Signal) (Report, error) {
	report := Report{Repo: repo.Name, Since: r.Now().Add(-repo.LookBackOrDefault())}
	var err error
	if report.Staging, err = r.commit(ctx, repo.Name, "staging", repo.Staging.Commit); err != nil {
		return report, err
	}
	if repo.Production.Commit != "" {
		if report.Production, err = r.commit(ctx, repo.Name, "production", repo.Production.Commit); err != nil {
			return report, err
		}
		deployed, err := r.GitHub.CommitTime(ctx, repo.Name, report.Production)
		if err != nil {
			return report, err
		}
		if deployed.After(report.Since) {
			report.Since = deployed
		}
	}
	merged, err := r.GitHub.MergedSince(ctx, repo.Name, report.Since)
	if err != nil {
		return report, err
	}
	report.Merged = len(merged)

	var issues []backlog.Issue
	for _, i := range open {
		if i.Repo == repo.Name {
			issues = append(issues, i)
		}
	}
	for _, src := range sources {
		if src.Repo != repo.Name {
			continue
		}
		closed, err := r.GitHub.ClosedSince(ctx, src, report.Since)
		if err != nil {
			return report, err
		}
		issues = append(issues, closed...)
	}
	seen := map[int]bool{}
	for _, issue := range issues {
		state, pr, since := backlog.EyeCheck(issue, signals)
		commit, isMerged := merged[pr]
		if state == "" || !isMerged || seen[issue.Number] {
			continue
		}
		seen[issue.Number] = true
		look := Look{Repo: repo.Name, Number: issue.Number, Title: issue.Title, URL: issue.URL, PR: pr, State: Failed, Since: since}
		if state == backlog.EyesAsked {
			onStaging, err := r.contains(ctx, repo.Name, report.Staging, commit)
			if err != nil {
				return report, err
			}
			look.State = Awaiting
			if onStaging {
				look.State = Due
			}
		}
		report.Looks = append(report.Looks, look)
	}
	sort.SliceStable(report.Looks, func(a, b int) bool { return report.Looks[a].Since.Before(report.Looks[b].Since) })
	return report, nil
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func (r *Reader) commit(ctx context.Context, repo, environment, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commitTimeout)
	defer cancel()
	out, err := r.Run(ctx, command)
	if err != nil {
		return "", fmt.Errorf("%s: read %s's commit: %w", repo, environment, err)
	}
	commit := strings.ToLower(strings.TrimSpace(out))
	if !commitRE.MatchString(commit) {
		return "", fmt.Errorf("%s: %s's commit command printed %q, not a commit hash", repo, environment, clip(commit, 80))
	}
	return commit, nil
}

func (r *Reader) contains(ctx context.Context, repo, head, commit string) (bool, error) {
	key := repo + " " + head + " " + commit
	if has, known := r.contained[key]; known {
		return has, nil
	}
	has, err := r.GitHub.Contains(ctx, repo, head, commit)
	if err != nil {
		return false, err
	}
	if r.contained == nil {
		r.contained = map[string]bool{}
	}
	r.contained[key] = has
	return has, nil
}

// Shell runs a commit command with sh.
func Shell(ctx context.Context, command string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
