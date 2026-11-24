package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// applyHolds holds the items that would make work for another: an issue
// that depends on one still open, an issue of a repository at its limit of
// open pull requests (or, where the limit counts ready ones, marks it to be
// parked as a draft), and an issue that touches the same code as one
// another worker of the machine has in hand. An issue that touches the same
// code as one in flight on another machine is not held: that machine builds
// in its own checkout. The worker is told about it instead. Two branches on the same
// code go stale on each other's merge, and cost a rebase and a full run of
// the checks. With stacking, an issue whose one open dependency has a ready
// pull request is built on that pull request's branch instead.
//
// Started work is never held: the worker's own issue in hand, rework and
// eye checks. Nor is a verification, which brings an open pull request
// closer to its merge, nor an issue the owner bumped.
func (s *Supervisor) applyHolds(w config.Worker, items []QueueItem, open, elsewhere []backlog.Issue, checkins []runlog.Checkin) {
	byNumber, listed := map[int]backlog.Issue{}, map[string]backlog.Issue{}
	for _, i := range open {
		byNumber[i.Number] = i
		listed[fmt.Sprintf("%s#%d", i.Repo, i.Number)] = i
	}
	inHand := runlog.IssueInHand(checkins, w.Name)
	others := map[int]string{}
	for _, other := range s.Machine.Workers {
		if n := runlog.BuildingInHand(checkins, other.Name); other.Name != w.Name && n != 0 {
			others[n] = other.Name
		}
	}
	inFlight := make([]int, 0, len(others))
	for n := range others {
		inFlight = append(inFlight, n)
	}
	slices.Sort(inFlight)
	for n := range items {
		item := &items[n]
		issue, ok := byNumber[item.Number]
		if ok && item.Rework != "" && item.Number != inHand {
			for _, number := range inFlight {
				if other, ok := byNumber[number]; ok {
					if why := backlog.Overlap(issue, other, s.Settings.OverlapLabels); why != "" {
						item.overlaps = fmt.Sprintf("overlaps #%d, which %s has in hand: %s", number, others[number], why)
						break
					}
				}
			}
		}
		if !ok || item.Look != "" || item.Rework != "" || item.Stack != "" || item.Number == inHand {
			continue
		}
		if hold := s.quotaHold(issue); hold != "" && !item.Bumped {
			item.Blocked = hold
			continue
		}
		if hold, _ := backlog.FollowHold(issue, listed, s.Signals, s.Covers); hold != "" {
			if len(backlog.MissingDeps(issue)) == 0 && s.stackOnDep(item, backlog.OpenDeps(issue), byNumber) {
				continue
			}
			item.Blocked = hold
			continue
		}
		limit, parks := s.prLimit(issue.Repo)
		if hold := backlog.PRLimitHold(issue, limit, parks); hold != "" && item.Verify == "" && !item.Bumped && !item.resumes() {
			item.Blocked = hold
			continue
		}
		if park := backlog.PRLimitPark(issue, limit, parks); park != "" && item.Verify == "" && !item.resumes() {
			item.Park = park
		}
		for _, number := range inFlight {
			other, ok := byNumber[number]
			if !ok {
				continue
			}
			if why := backlog.Overlap(issue, other, s.Settings.OverlapLabels); why != "" {
				item.Blocked = fmt.Sprintf("overlaps #%d, which %s has in hand: %s", number, others[number], why)
				break
			}
		}
		for _, other := range elsewhere {
			if item.Blocked != "" || item.Elsewhere != "" {
				break
			}
			if other.Repo != issue.Repo || other.Number == issue.Number {
				continue
			}
			if why := backlog.Overlap(issue, other, s.Settings.OverlapLabels); why != "" {
				item.Elsewhere = s.elsewhereNote(w, other, why)
			}
		}
	}
}

// prLimit is the repository's limit of open pull requests, 0 for none, and
// whether it counts ready ones only and parks new work at the limit.
func (s *Supervisor) prLimit(repo string) (limit int, parks bool) {
	for _, r := range s.Repos {
		if r.Name == repo {
			return r.MaxOpenPRs, r.ParksAtPRLimit()
		}
	}
	return 0, false
}

// stackOnDep lets an item go ahead on top of its one open dependency, when
// the fleet stacks and that dependency has a ready pull request that waits
// for review.
func (s *Supervisor) stackOnDep(item *QueueItem, deps []backlog.Dep, byNumber map[int]backlog.Issue) bool {
	if !s.Settings.Stack || len(deps) != 1 || deps[0].Repo != item.Repo {
		return false
	}
	dep, ok := byNumber[deps[0].Number]
	if !ok {
		return false
	}
	pr := backlog.HandedPR(dep, s.Signals)
	state, open := dep.OpenPRs[pr]
	if pr == 0 || !open || state.Draft || state.Problem != "" {
		return false
	}
	item.StackOn = fmt.Sprintf("#%d's pull request #%d waits for review", dep.Number, pr)
	item.StackDep, item.StackPR = dep.Number, pr
	return true
}

// inFlightElsewhere lists the issues other machines of the fleet are
// building, as far as the issues say: they carry another machine's queue
// label, and their plan is written and not handed back. Only a fleet that
// shares work across machines (an account with overflow) asks: one search
// per repository. A worker with peers on other machines asks too: its
// roster names what they build.
func (s *Supervisor) inFlightElsewhere(ctx context.Context, w config.Worker) []backlog.Issue {
	sharesWork := s.Account != nil && s.Account.Overflow != ""
	remotePeers := slices.ContainsFunc(w.PeerList, func(p config.Peer) bool { return p.Remote })
	if !sharesWork && !remotePeers {
		return nil
	}
	own := map[string]bool{}
	for _, src := range s.Machine.AllSources() {
		own[src.Label] = true
	}
	var labels []string
	for _, l := range s.QueueLabels {
		if !own[l] && (s.Account == nil || l != s.Account.Overflow) {
			labels = append(labels, fmt.Sprintf("%q", l))
		}
	}
	if len(labels) == 0 {
		return nil
	}
	var repos []string
	for _, src := range s.Machine.SourcesFor(w) {
		if !slices.Contains(repos, src.Repo) {
			repos = append(repos, src.Repo)
		}
	}
	var out []backlog.Issue
	for _, repo := range repos {
		issues, err := s.Lister.List(ctx, config.IssueSource{Repo: repo, Query: "label:" + strings.Join(labels, ",")})
		if err != nil {
			s.Logf("worker %s: work in flight elsewhere: %v", w.Name, err)
			continue
		}
		for _, i := range issues {
			if backlog.InFlight(i, s.Signals) {
				out = append(out, i)
			}
		}
	}
	return out
}

// elsewhereNote says which issue in flight on another machine touches the
// same code as an item, and which of the worker's peers there it may tell
// what it changes.
func (s *Supervisor) elsewhereNote(w config.Worker, other backlog.Issue, why string) string {
	machine := ""
	if s.QueuesOf != nil {
		if machines := s.QueuesOf(other.Repo, other.LabelNames()); len(machines) > 0 {
			machine = machines[0]
		}
	}
	where := "on another machine"
	if machine != "" {
		where = "on " + machine
	}
	note := fmt.Sprintf("overlaps #%d, in flight %s: %s", other.Number, where, why)
	if signer := backlog.PlanSession(other); signer != "" && slices.ContainsFunc(w.PeerList, func(p config.Peer) bool { return p.Session == signer }) {
		return note + fmt.Sprintf("; #%d is built by `%s`: tell it what you change that #%d builds on; rebase on the default branch after #%d's pull request merges", other.Number, signer, other.Number, other.Number)
	}
	var peers []string
	for _, p := range w.PeerList {
		if p.Remote && (machine == "" || p.Machine == machine) {
			peers = append(peers, "`"+p.Session+"`")
		}
	}
	if len(peers) > 0 {
		note += fmt.Sprintf("; tell your peers there (%s) what you change that #%d builds on", strings.Join(peers, ", "), other.Number)
	}
	return note + fmt.Sprintf("; rebase on the default branch after #%d's pull request merges", other.Number)
}

// FiledLister lists the issues workers opened in a repository since a time.
type FiledLister interface {
	FiledSince(ctx context.Context, repo string, since time.Time) ([]backlog.Filed, error)
}

// filedRecheck is how long the issues workers opened are kept before they
// are read again, and filedWindow how far back they are read: an issue
// held over its day's quota stays held until the owner admits it, so its
// day must stay in view.
const (
	filedRecheck = 10 * time.Minute
	filedWindow  = 7 * 24 * time.Hour
)

// filedState is what was last read of one repository's issues opened by
// workers: which are past their day's quota.
type filedState struct {
	at   time.Time
	over map[int]bool
}

// refreshFiled reads again, at most every filedRecheck, which issues workers
// opened past their day's quota in each repository with issue_quota. A read
// that fails keeps what was read before and is logged.
func (s *Supervisor) refreshFiled(ctx context.Context) {
	if s.Filed == nil {
		return
	}
	now := s.Now()
	for _, repo := range s.Repos {
		if repo.IssueQuota <= 0 || now.Sub(s.filed[repo.Name].at) < filedRecheck {
			continue
		}
		if s.filed == nil {
			s.filed = map[string]filedState{}
		}
		filed, err := s.Filed.FiledSince(ctx, repo.Name, now.Add(-filedWindow))
		if err != nil {
			s.Logf("read the issues workers opened in %s: %v", repo.Name, err)
			continue
		}
		s.filed[repo.Name] = filedState{at: now, over: backlog.OverQuota(filed, repo.IssueQuota, now.Location())}
	}
}

// quotaHold says why an issue a worker opened is held: it came past its
// day's quota, and the owner has not admitted it. "" when it is not held.
func (s *Supervisor) quotaHold(issue backlog.Issue) string {
	if !s.filed[issue.Repo].over[issue.Number] || backlog.Admitted(issue, s.Signals) {
		return ""
	}
	quota := 0
	if repo, ok := s.repoConfig(issue.Repo); ok {
		quota = repo.IssueQuota
	}
	return fmt.Sprintf("over the quota of %d issue(s) workers open a day: it waits for triage; write `Ready now` on it to admit it", quota)
}
