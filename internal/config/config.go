// Package config loads and validates the fleet file (shed.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/edisoncode/ai-shed/internal/pricing"
)

// LocalHost is the host value for a machine that is reached without SSH.
const LocalHost = "local"

// DefaultTaskTimeout stops a hung task from blocking its schedule forever.
const DefaultTaskTimeout = time.Hour

type Config struct {
	Defaults   Defaults   `yaml:"defaults"`
	Machines   []Machine  `yaml:"machines"`
	Signals    []Signal   `yaml:"signals"`
	Supervisor Supervisor `yaml:"supervisor"`
	// HandBack is the phrase that marks a comment as a worker's hand-back.
	// A signal's ask counts only in such a comment, so the same words in a
	// plan, a brief or a discussion do not hold an issue back. Omitted, it is
	// DefaultHandBack; set to "" to count an ask in any comment.
	HandBack *string `yaml:"handback"`
	// Repos says, per repository, how to see what is deployed. With it a
	// worker can do the eye checks of merged work on staging.
	Repos []Repo `yaml:"repos"`
	// Pricing sets the rate of a model, in US dollars per million tokens,
	// in place of the published one, or adds a model shed has no price for.
	// The rates weigh the tokens in cost comments and the digest.
	Pricing map[string]pricing.Rate `yaml:"pricing"`
	// Accounts names the machines that share one Claude account, and so one
	// weekly limit, with a budget to pace the week to.
	Accounts []Account `yaml:"accounts"`
}

// Account is a Claude account that some machines share.
type Account struct {
	Name     string   `yaml:"name"`
	Machines []string `yaml:"machines"`
	// WeeklyBudget is what the account may spend in a week, in US dollars at
	// the published API rates: the same weights as the cost comments.
	WeeklyBudget float64 `yaml:"weekly_budget"`
	// Resets is when the weekly limit starts again, on the machines' clock,
	// as "Wed 18:00".
	Resets string `yaml:"resets"`
	// HoldAllBut is how many workers of a machine may still take new work
	// when the account is far ahead of its pace. Nil is 1.
	HoldAllBut *int `yaml:"hold_all_but"`
	// Overflow is a label. A worker of the account that reaches its weekly
	// limit, or is stuck, with work that lives outside its session, moves
	// its issue onto it, and a machine that lists the label as a backfill
	// source takes it up.
	Overflow string `yaml:"overflow"`
}

const resetLayout = "Mon 15:04"

// Paced reports whether the account is paced through its week: it has a
// weekly budget.
func (a Account) Paced() bool {
	return a.WeeklyBudget > 0
}

// Share is a machine's part of the weekly budget: each machine's agent sees
// only its own spend, so each is paced to an equal share.
func (a Account) Share() float64 {
	if len(a.Machines) == 0 {
		return 0
	}
	return a.WeeklyBudget / float64(len(a.Machines))
}

// HoldAllButOrDefault is how many workers keep taking work far ahead of pace.
func (a Account) HoldAllButOrDefault() int {
	if a.HoldAllBut == nil {
		return 1
	}
	return *a.HoldAllBut
}

// WeekStart is the last reset at or before now, in now's location.
func (a Account) WeekStart(now time.Time) time.Time {
	at, err := time.Parse(resetLayout, a.Resets)
	if err != nil {
		return now.Add(-7 * 24 * time.Hour)
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	for start.Weekday() != weekdayOf(a.Resets) || start.After(now) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

func weekdayOf(resets string) time.Weekday {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.HasPrefix(resets, d.String()[:3]) {
			return d
		}
	}
	return time.Sunday
}

// AccountOf returns the account the machine is in.
func (c *Config) AccountOf(machine string) (Account, bool) {
	for _, a := range c.Accounts {
		if slices.Contains(a.Machines, machine) {
			return a, true
		}
	}
	return Account{}, false
}

// Prices is the published price table with the fleet file's rates in it.
func (c *Config) Prices() pricing.Table {
	return pricing.Default().With(c.Pricing)
}

// Repo is what shed knows about a repository's deploys.
type Repo struct {
	Name       string      `yaml:"repo"`
	Staging    Environment `yaml:"staging"`
	Production Environment `yaml:"production"`
	// LookBack is how far back merged work is looked for when production's
	// commit is older than that, or is not known.
	LookBack string `yaml:"look_back"`
	// Gates is what the agent runs on the head commit of each pull request
	// built on its machine.
	Gates Gates `yaml:"gates"`
	// Verify, when set, has a worker with verifies: true check each handed-
	// back pull request before the owner is asked to review it, as deep as
	// the pull request's risk tier says.
	Verify *Verify `yaml:"verify"`
	// MaxOpenPRs holds new issues of the repository while it has this many
	// open pull requests or more: started work is finished before more is
	// started. Rework, verification and the issue a worker has in hand go
	// on. 0, the default, holds nothing.
	MaxOpenPRs int `yaml:"max_open_prs"`
	// MaxOpenPRsCount is what counts toward MaxOpenPRs: every open pull
	// request (all, the default), or only the ones that are not drafts
	// (ready). A draft runs no CI. With ready, new issues are not held at
	// the limit: a worker opens its pull request as a draft and parks it,
	// and the agent marks the oldest parked draft ready when there is room.
	MaxOpenPRsCount string `yaml:"max_open_prs_count"`
}

// What counts toward max_open_prs.
const (
	PRCountAll   = "all"
	PRCountReady = "ready"
)

// ParksAtPRLimit reports whether the repository's limit counts ready pull
// requests only, and parks new work as drafts at the limit.
func (r Repo) ParksAtPRLimit() bool {
	return r.MaxOpenPRs > 0 && r.MaxOpenPRsCount == PRCountReady
}

// Verify says how a repository's pull requests are tiered for verification.
type Verify struct {
	// Areas are the parts of the code a change to which is high risk: money,
	// sign-in, data migrations, a bounded context with strict invariants.
	Areas []Area `yaml:"areas"`
	// HighLines is how many changed lines make a pull request high risk.
	HighLines int `yaml:"high_lines"`
	// MergeLow moves the owner's approval of a low-risk pull request from
	// the merge to the promotion: one that a verifier passed at low risk,
	// with green checks and nothing else waiting on the owner, is queued
	// for merge by the machine that built it. Off unless set.
	MergeLow bool `yaml:"merge_low"`
	// MergeHigh does the same for a pull request a verifier passed at high
	// risk, unless the verifier said the merge is the owner's. It needs
	// MergeLow. Off unless set.
	MergeHigh bool `yaml:"merge_high"`
	// MergeMethod is how such a pull request merges where the repository
	// has no merge queue: merge, squash or rebase.
	MergeMethod string `yaml:"merge_method"`
	// MergeTrain is for a repository that cannot have a merge queue. With
	// true, shed merges what it queues one pull request at a time, each
	// brought up to date with the base branch and merged only when its
	// checks pass on that head. With batch, it tests up to BatchSize pull
	// requests together on one draft pull request and merges them all when
	// its checks pass. Off unless set; it needs merge_low.
	MergeTrain TrainMode `yaml:"merge_train"`
	// BatchSize is how many pull requests a batch train tests together.
	BatchSize int `yaml:"batch_size"`
	// MergeTrainAlone are paths a change to which a batch train tests in a
	// batch of its own, such as a schema, the migrations or a lockfile. A
	// path names a file or a directory, as in areas.
	MergeTrainAlone []string `yaml:"merge_train_alone"`
}

// TrainMode is how a merge train merges: one pull request at a time, or in
// batches. "" runs no train.
type TrainMode string

const (
	TrainOne   TrainMode = "one"
	TrainBatch TrainMode = "batch"
)

// DefaultBatchSize is how many pull requests a batch train tests together.
const DefaultBatchSize = 5

// UnmarshalYAML reads merge_train: true, false or batch.
func (t *TrainMode) UnmarshalYAML(node *yaml.Node) error {
	var on bool
	if err := node.Decode(&on); err == nil {
		*t = ""
		if on {
			*t = TrainOne
		}
		return nil
	}
	var word string
	if err := node.Decode(&word); err != nil || word != string(TrainBatch) {
		return fmt.Errorf("line %d: merge_train must be true, false or batch, not %q", node.Line, node.Value)
	}
	*t = TrainBatch
	return nil
}

// BatchSizeOrDefault is how many pull requests a batch train tests together.
func (v Verify) BatchSizeOrDefault() int {
	if v.BatchSize <= 0 {
		return DefaultBatchSize
	}
	return v.BatchSize
}

// MergeMethodOrDefault is the merge method of a queued merge.
func (v Verify) MergeMethodOrDefault() string {
	if v.MergeMethod == "" {
		return "merge"
	}
	return v.MergeMethod
}

// Area is a named set of paths whose changes are high risk.
type Area struct {
	Name  string   `yaml:"name"`
	Paths []string `yaml:"paths"`
}

// DefaultHighLines is the size of a pull request that is high risk by size
// alone.
const DefaultHighLines = 400

func (v Verify) HighLinesOrDefault() int {
	if v.HighLines <= 0 {
		return DefaultHighLines
	}
	return v.HighLines
}

// Risk tiers of a pull request.
const (
	TierLow  = "low"
	TierHigh = "high"
)

// Gates is a repository's fast checks: format, lint, unit tests, build. The
// agent of the machine that built a pull request runs them on its head
// commit and posts the result as the shed/gates commit status.
type Gates struct {
	// Command runs in a checkout of the commit, after the machine's init
	// line. Exit 0 passes.
	Command string `yaml:"command"`
	Timeout string `yaml:"timeout"`
}

// DefaultGatesTimeout is how long a gate run may take.
const DefaultGatesTimeout = 30 * time.Minute

func (g Gates) TimeoutOrDefault() time.Duration {
	return durationOr(g.Timeout, DefaultGatesTimeout)
}

// Environment is one place a repository is deployed to.
type Environment struct {
	// Commit is a command that prints the commit the environment serves.
	Commit string `yaml:"commit"`
	// URL is where a worker opens the environment in a browser.
	URL string `yaml:"url"`
	// Orders say what a worker may and may not do there during an eye check.
	Orders string `yaml:"orders"`
	// Viewport is the browser size an eye check is done at when the
	// hand-back names none, as "<width>x<height>" in CSS pixels. shed cannot
	// resize a browser: the worker is told the size and says what it had.
	Viewport string `yaml:"viewport"`
	// Deploy is a command that deploys the default branch to staging. With
	// it, the agent of DeployFrom deploys when looks pile up, as DeployWhen
	// says, and never while a look is in progress there.
	Deploy     string     `yaml:"deploy"`
	DeployFrom string     `yaml:"deploy_from"`
	DeployWhen DeployWhen `yaml:"deploy_when"`
}

// DeployWhen says when looks have waited long enough for a staging deploy:
// when this many wait, or when the oldest has waited this long.
type DeployWhen struct {
	WaitingLooks int    `yaml:"waiting_looks"`
	OrAfter      string `yaml:"or_after"`
}

const (
	DefaultWaitingLooks = 5
	DefaultDeployAfter  = 4 * time.Hour
)

func (d DeployWhen) WaitingLooksOrDefault() int {
	if d.WaitingLooks <= 0 {
		return DefaultWaitingLooks
	}
	return d.WaitingLooks
}

func (d DeployWhen) OrAfterOrDefault() time.Duration {
	return durationOr(d.OrAfter, DefaultDeployAfter)
}

// DefaultLookBack is how far back merged work is looked for.
const DefaultLookBack = 14 * 24 * time.Hour

// Deployed reports whether shed knows how to read what staging serves: the
// release state and the eye checks need it. A repository may be listed for
// its gates alone.
func (r Repo) Deployed() bool {
	return r.Staging.Commit != ""
}

// Trains reports whether shed runs a merge train for the repository.
func (r Repo) Trains() bool {
	return r.Verify != nil && r.Verify.MergeLow && r.Verify.MergeTrain != ""
}

// Batches reports whether shed runs a batch train for the repository.
func (r Repo) Batches() bool {
	return r.Trains() && r.Verify.MergeTrain == TrainBatch
}

func (r Repo) LookBackOrDefault() time.Duration {
	return durationOr(r.LookBack, DefaultLookBack)
}

// Repo returns what shed knows about the repository's deploys.
func (c *Config) Repo(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}

// DefaultHandBack is the phrase that marks a hand-back comment.
const DefaultHandBack = "Hand-back"

// Supervisor sets how the agent checks in on a machine's workers.
type Supervisor struct {
	// Command reads a prompt on stdin and prints the reviewer's answer.
	Command string `yaml:"command"`
	// CacheTTL is how long a worker's prompt cache stays warm after its last
	// request. shed cannot see the cache; this is what the owner's plan gives.
	CacheTTL string `yaml:"cache_ttl"`
	// ScopeEvery is how often a busy worker is checked against its brief.
	ScopeEvery string `yaml:"scope_every"`
	// WhenCold says what a nudge does to a worker whose cache has expired.
	WhenCold string `yaml:"when_cold"`
	// ClearCommand, typed into a worker, empties its context.
	ClearCommand string `yaml:"clear_command"`
	// ModelCommand, typed into a worker, switches its model. {model} is
	// replaced with the model's name.
	ModelCommand string `yaml:"model_command"`
	Models       Models `yaml:"models"`
	// NoWorkAfter is how long a worker may have no work before the owner is
	// told. Empty tells nobody.
	NoWorkAfter string `yaml:"no_work_after"`
	// LowQueue tells the owner when a queue has fewer issues than this that
	// a worker can act on. 0 tells nobody.
	LowQueue int `yaml:"low_queue"`
	// Stack keeps a worker on an issue whose pull request waits for review
	// when its hand-back lists what is still to do: the rest is built on a
	// branch from that pull request's. Without it the worker takes another
	// issue while the review waits.
	Stack bool `yaml:"stack"`
	// RecycleContext clears a worker's context at its next idle hand-off
	// once its session's last call carried more than this many tokens of
	// context. Every turn re-reads that context, so it is most of the spend
	// of a long issue. 0 never clears for size.
	RecycleContext int `yaml:"recycle_context"`
	// OwnWaitMax bounds how long an idle worker that was found waiting on
	// work of its own (a background command, a timer) is left alone. It is
	// then told to check that work, and the second time in one issue it is
	// stuck. "0" turns the bound off.
	OwnWaitMax string `yaml:"own_wait_max"`
	// OverlapLabels is a label prefix, such as "area:". Two issues with the
	// same label of that prefix touch the same code: one is held while a
	// worker has the other in hand, as for issues whose Paths overlap.
	OverlapLabels string `yaml:"overlap_labels"`
	// ContextBudget is how many tokens a worker's tool may load before its
	// first item; above it, shed status says so. 0 for no budget.
	ContextBudget int `yaml:"context_budget"`
}

// NoWorkAfterOrZero is how long a worker may have no work before the owner
// is told; 0 for never.
func (s Supervisor) NoWorkAfterOrZero() time.Duration {
	return durationOr(s.NoWorkAfter, 0)
}

// Models picks the model for an issue from its labels, so the cost of the
// model matches the difficulty of the work.
type Models struct {
	// Default is the model for an issue with no model label.
	Default string `yaml:"default"`
	// Labels maps an issue label to a model.
	Labels map[string]string `yaml:"labels"`
	// Apply says what the agent does with an issue's model.
	Apply string `yaml:"apply"`
	// ApplyLabels sets Apply for the issues with a label, as in
	// {model:sonnet: switch}: a plain implementation issue runs its whole
	// session on its model, while the rest are told theirs.
	ApplyLabels map[string]string `yaml:"apply_labels"`
	// Session is the model a session runs on for an issue it is only told
	// the model of. It is needed when some issues switch and some are told:
	// after a switched issue, the session goes back to it.
	Session string `yaml:"session"`
	// EyeChecks is the model an eye check runs on. The session switches to
	// it for the look and back for the next issue, so a look does not run
	// on the model the hardest work needs.
	EyeChecks string `yaml:"eye_checks"`
	// Verify maps a risk tier to the model a verification at that tier runs
	// on, as in {low: haiku, high: opus}. The session switches for it and
	// back for the next issue.
	Verify map[string]string `yaml:"verify"`
}

// Values of Models.Apply.
const (
	// ModelSwitch switches the worker's own session to the model.
	ModelSwitch = "switch"
	// ModelTell leaves the session alone and tells the worker the model, for
	// a worker that orchestrates sub-agents and picks their models itself.
	ModelTell = "tell"
)

// TellOnly reports whether the worker is told the model and not switched.
func (m Models) TellOnly() bool {
	return m.Apply == ModelTell
}

// ApplyFor is what is done with the model of an issue with these labels:
// the first of ApplyLabels it has, else Apply.
func (m Models) ApplyFor(labels []string) string {
	for _, label := range labels {
		if a, ok := m.ApplyLabels[label]; ok {
			return a
		}
	}
	return m.Apply
}

// For returns the model for an issue with these labels, or "" when no model
// is configured.
func (m Models) For(labels []string) string {
	for _, label := range labels {
		if model, ok := m.Labels[label]; ok {
			return model
		}
	}
	return m.Default
}

// Values of Supervisor.WhenCold.
const (
	ColdClear  = "clear"  // start from an empty context and re-read the brief
	ColdResume = "resume" // keep the context and pay to read it again
)

const (
	DefaultReviewCommand = "claude -p --model sonnet --output-format json"
	DefaultCacheTTL      = 5 * time.Minute
	DefaultScopeEvery    = 30 * time.Minute
	DefaultOwnWaitMax    = 45 * time.Minute
	DefaultWorkerCommand = "claude"
	DefaultClearCommand  = "/clear"
	DefaultModelCommand  = "/model {model}"
	modelPlaceholder     = "{model}"
)

type Defaults struct {
	Checks []Check `yaml:"checks"`
	// StandingOrders are the owner's rules for every worker on every machine:
	// how to build, what never to do. They are added to each worker's brief.
	StandingOrders string `yaml:"standing_orders"`
	// DeployHook is a command that `shed deploy` runs on the watcher after
	// each machine it deploys to. It carries what shed does not know about:
	// the files a worker's tool needs, such as agent definitions and skills.
	DeployHook string `yaml:"deploy_hook"`
	// Notify is a command the agent runs on its machine when something
	// starts to need the owner. The message is in SHED_MESSAGE.
	Notify string `yaml:"notify"`
}

type Machine struct {
	Name    string        `yaml:"name"`
	Host    string        `yaml:"host"`
	Init    string        `yaml:"init"`
	Checks  []Check       `yaml:"checks"`
	Issues  []IssueSource `yaml:"issues"`
	Tasks   []Task        `yaml:"tasks"`
	Workers []Worker      `yaml:"workers"`
	// Capacity says when the machine is too busy to be given new work.
	Capacity Capacity `yaml:"capacity"`
}

// Capacity is the machine's test for "busy". While it is busy no worker is
// handed a new issue; work in progress goes on. With neither test set the
// machine always has room.
type Capacity struct {
	// MaxLoad is the 1-minute load average per core above which the machine
	// is busy. 0 turns the test off.
	MaxLoad float64 `yaml:"max_load"`
	// BusyWhen is a command that exits 0 while the machine is busy with
	// something that is not shed's, such as a CI job.
	BusyWhen string `yaml:"busy_when"`
	// MaxWait is the longest a hand-over is held. After it the issue is
	// handed over anyway, so a machine that is never quiet still works.
	MaxWait string `yaml:"max_wait"`
	// Nice starts each worker's command at this lower CPU priority (1 to
	// 19), so what else runs on the machine, such as CI runners, gets the
	// cores first. Everything the worker starts inherits it; the agent
	// itself keeps its own. 0 leaves the priority alone.
	Nice int `yaml:"nice"`
	// ReleaseAfter is how long the machine may stay busy before it gives
	// the work in its queue that nobody has started to the fleet: one issue
	// each ReleaseAfter, onto its account's overflow label, which a machine
	// with room takes up. Empty releases nothing.
	ReleaseAfter string `yaml:"release_after"`
}

// ReleaseAfterOrZero is ReleaseAfter, or 0 when the machine releases
// nothing.
func (c Capacity) ReleaseAfterOrZero() time.Duration {
	return durationOr(c.ReleaseAfter, 0)
}

// MaxNice is the lowest CPU priority a worker can be given.
const MaxNice = 19

// NicePrefix is what goes in front of a worker's command to start it at the
// machine's priority: "nice -n 10 ", or "" for none.
func (c Capacity) NicePrefix() string {
	if c.Nice == 0 {
		return ""
	}
	return fmt.Sprintf("nice -n %d ", c.Nice)
}

// DefaultMaxWait is how long a hand-over is held on a busy machine.
const DefaultMaxWait = 20 * time.Minute

// Set reports whether the machine has a test for busy.
func (c Capacity) Set() bool {
	return c.MaxLoad > 0 || c.BusyWhen != ""
}

func (c Capacity) MaxWaitOrDefault() time.Duration {
	return durationOr(c.MaxWait, DefaultMaxWait)
}

// Worker is a long-running agent session that the machine's agent keeps
// alive and supervises. It runs in tmux window shed:<name>.
type Worker struct {
	Name string `yaml:"name"`
	// Dir is the working directory; a leading ~/ is the user's home.
	Dir string `yaml:"dir"`
	// Command starts the session. The first prompt is appended as one argument.
	Command string `yaml:"command"`
	// Brief is the worker's scope: what to work on and what to leave alone.
	Brief string `yaml:"brief"`
	// Issues is this worker's own queue. Without it the worker takes from
	// the machine's queue.
	Issues []IssueSource `yaml:"issues"`
	// FreshPerIssue clears the worker's context each time it is handed a new
	// issue, so one issue's conversation does not follow it into the next.
	// Leave it off for themed work where the issues build on each other.
	FreshPerIssue bool `yaml:"fresh_per_issue"`
	// Verifies says the worker checks pull requests that others built before
	// the owner reviews them. Its queue's repositories need verify under
	// repos.
	Verifies bool `yaml:"verifies"`
	// EyeChecks says the worker has a browser and may do the eye checks of
	// merged work on staging. Its queue's repositories need a `repos` entry.
	EyeChecks bool `yaml:"eye_checks"`
	// Ask makes the worker a session that answers the owner's questions. Its
	// session is kept running and it is handed no issue and no task: nothing
	// is typed into the owner's conversation with it.
	Ask bool `yaml:"ask"`
	// Env is set in the worker's session: per-worker values such as the
	// test user it signs in to staging with, so two workers on one machine
	// do not share one account.
	Env map[string]string `yaml:"env"`
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WorkerPlaceholder in a repository's staging orders is replaced with the
// name of the worker that reads them.
const WorkerPlaceholder = "{worker}"

// Check is a command that must exit 0 for the machine to be ready for work.
type Check struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

// IssueSource selects the open issues that are a machine's backlog.
type IssueSource struct {
	Repo     string `yaml:"repo"`
	Label    string `yaml:"label"`
	Assignee string `yaml:"assignee"`
	// Priority lists labels, highest first. An issue with one of them is
	// worked before an issue with a later one or with none; within one
	// priority the oldest issue goes first.
	Priority []string `yaml:"priority"`
	// Query is a GitHub issue search, such as `label:"priority: P1"
	// no:assignee`. It selects issues nobody put in a queue by hand.
	Query string `yaml:"query"`
	// Backfill makes the source a reserve: an issue is taken from it only
	// when the other sources of the list have nothing a worker can act on.
	// An issue that is taken gets the label of the list's first labelled
	// source of the same repository, and is in that queue from then on.
	Backfill bool `yaml:"backfill"`
}

// IssueSourceKey says which issues a source selects. Two sources with the
// same key list the same issues.
type IssueSourceKey struct{ Repo, Label, Assignee, Query string }

func (s IssueSource) Key() IssueSourceKey {
	return IssueSourceKey{s.Repo, s.Label, s.Assignee, s.Query}
}

// claimLabel is the label a backfill issue of this repository gets when a
// worker with these sources takes it; "" when no source gives one.
func claimLabel(sources []IssueSource, repo string) string {
	for _, src := range sources {
		if !src.Backfill && src.Repo == repo && src.Label != "" {
			return src.Label
		}
	}
	return ""
}

// Rank is the place of an issue with these labels in the source's priority
// order: 0 is first. An issue with no priority label ranks after all of them.
func (s IssueSource) Rank(labels []string) int {
	for rank, priority := range s.Priority {
		for _, label := range labels {
			if label == priority {
				return rank
			}
		}
	}
	return len(s.Priority)
}

// Task is a command the machine's agent runs on a cron schedule.
type Task struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Run      string `yaml:"run"`
	Timeout  string `yaml:"timeout"`
}

// Signal is a phrase convention in issue comments that means the issue waits
// on the owner.
type Signal struct {
	Name       string   `yaml:"name"`
	Ask        string   `yaml:"ask"`
	Clear      []string `yaml:"clear"`
	AnsweredBy string   `yaml:"answered_by"`
	// FollowsPR is for a signal whose ask names a pull request ("#41"). The
	// signal then closes when that pull request is merged or closed, so an
	// issue that takes several pull requests comes back into the queue.
	FollowsPR bool `yaml:"follows_pr"`
	// SendsBack is for a signal that the owner answers with words a worker
	// must act on. When the answer comes after the worker handed back and its
	// pull request is still open, the issue goes back to a worker.
	SendsBack bool `yaml:"sends_back"`
	// Accept lists the words that, right after the answering phrase, take
	// the work as it stands ("Owner ruling: accepted"). Such an answer closes
	// the signal and sends nothing back.
	Accept []string `yaml:"accept"`
	// FailedBy is the phrase a worker writes when it did the check the
	// signal asks for and the work did not pass. It closes the signal: the
	// look was done. What to do about it is a decision the worker asks for.
	FailedBy string `yaml:"failed_by"`
	// BlockedBy is the phrase a worker writes when it could not do the check
	// the signal asks for: it could not sign in, or the place was down. The
	// check is then not handed to a worker again until the owner writes
	// UnblockedBy, or does the check. Both are for the eyes signal, where an
	// empty one gets its default.
	BlockedBy   string `yaml:"blocked_by"`
	UnblockedBy string `yaml:"unblocked_by"`
	// Grace is how long the owner has to answer before the issue is parked
	// and its worker takes the next one. Until then the worker stays on the
	// issue and does the parts the question does not touch. It is for the
	// decision signal, where an empty one gets DefaultGrace.
	Grace string `yaml:"grace"`
	// Risky says which choices a worker must not make by itself. Every other
	// choice is a safe default: the worker takes the conservative option and
	// records it after Decided. Both are for the decision signal, where an
	// empty one gets its default.
	Risky   string `yaml:"risky"`
	Decided string `yaml:"decided"`
	// Remaining is the phrase after which a hand-back lists what is still
	// to do on the issue. It is for the follows_pr signal, where an empty
	// one gets DefaultRemaining. A hand-back whose pull request merged and
	// that lists nothing is the issue's last: it is not handed out again.
	Remaining string `yaml:"remaining"`
	// HandBack is the config's hand-back phrase, copied onto each signal
	// when the config is read.
	HandBack string `yaml:"-"`
}

// ReadySignal is the name of the signal a worker opens when an issue it took
// from a backfill source does not say enough to be worked.
const ReadySignal = "ready"

func readySignal() Signal {
	return Signal{Name: ReadySignal, Ask: "Not ready:", Clear: []string{"none"}, AnsweredBy: "Ready now"}
}

// DecisionSignal is the name of the signal that asks the owner to decide.
const DecisionSignal = "decision"

// The defaults of the decision signal.
const (
	DefaultGrace   = 15 * time.Minute
	DefaultDecided = "Decided without you:"
	DefaultRisky   = "anything that changes what is merged or deployed, how money or sign-in behaves, a data migration, or a choice about the product that is the owner's or a customer's to make"
)

// GraceOrDefault is how long the owner has to answer; 0 for a signal that
// parks nothing.
func (s Signal) GraceOrDefault() time.Duration {
	if s.Name != DecisionSignal {
		return durationOr(s.Grace, 0)
	}
	return durationOr(s.Grace, DefaultGrace)
}

// DefaultRemaining is the phrase after which a hand-back lists what is
// still to do.
const DefaultRemaining = "Still to do:"

// withDecisionDefaults fills in what a decision signal, and the signal that
// follows a pull request, do not say.
func withDecisionDefaults(signals []Signal) []Signal {
	for i, s := range signals {
		if s.FollowsPR && s.Remaining == "" {
			signals[i].Remaining = DefaultRemaining
		}
		if s.Name != DecisionSignal {
			continue
		}
		if s.Risky == "" {
			signals[i].Risky = DefaultRisky
		}
		if s.Decided == "" {
			signals[i].Decided = DefaultDecided
		}
	}
	return signals
}

// EyesSignal is the name of the signal that asks for an eye check. A worker
// with eye_checks answers it on staging.
const EyesSignal = "eyes"

// The phrases for an eye check that could not be done, and for the owner's
// word that it can be tried again.
const (
	DefaultBlockedBy   = "Eyes blocked"
	DefaultUnblockedBy = "Eyes unblocked"
)

// DefaultSignals returns the built-in signals, counted in hand-back comments.
func DefaultSignals() []Signal {
	return withDecisionDefaults(withHandBack(DefaultHandBack, []Signal{
		{Name: DecisionSignal, Ask: "Decisions needed:", Clear: []string{"none"}, AnsweredBy: "Owner ruling", SendsBack: true, Accept: []string{"accepted"}},
		{Name: EyesSignal, Ask: "Needs eyes:", Clear: []string{"nothing", "none"}, AnsweredBy: "Eyes checked", FailedBy: "Eyes failed", BlockedBy: DefaultBlockedBy, UnblockedBy: DefaultUnblockedBy},
		{Name: "review", Ask: "**PR:**", Clear: []string{"none"}, FollowsPR: true},
		readySignal(),
	}))
}

func withHandBack(phrase string, signals []Signal) []Signal {
	for i := range signals {
		signals[i].HandBack = phrase
	}
	return signals
}

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	viewportRE = regexp.MustCompile(`^[1-9][0-9]*x[1-9][0-9]*$`)
	repoRE     = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)
)

// Resolve picks the config path: the flag, then $SHED_CONFIG, then ./shed.yaml,
// then ~/.config/shed/shed.yaml.
func Resolve(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if p := os.Getenv("SHED_CONFIG"); p != "" {
		return p, nil
	}
	if _, err := os.Stat("shed.yaml"); err == nil {
		return "shed.yaml", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "shed", "shed.yaml"), nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(cfg.Signals) == 0 {
		cfg.Signals = DefaultSignals()
	}
	handBack := DefaultHandBack
	if cfg.HandBack != nil {
		handBack = *cfg.HandBack
	}
	// A fleet file with signals of its own and a backfill source needs the
	// signal that keeps an unready issue from being handed out again.
	if cfg.hasBackfill() && !slices.ContainsFunc(cfg.Signals, func(s Signal) bool { return s.Name == ReadySignal }) {
		cfg.Signals = append(cfg.Signals, readySignal())
	}
	withDecisionDefaults(withHandBack(handBack, cfg.Signals))
	// A fleet file written before a look could be recorded as blocked names
	// no phrases for it.
	for i, s := range cfg.Signals {
		if s.Name != EyesSignal {
			continue
		}
		if s.BlockedBy == "" {
			cfg.Signals[i].BlockedBy = DefaultBlockedBy
		}
		if s.UnblockedBy == "" {
			cfg.Signals[i].UnblockedBy = DefaultUnblockedBy
		}
	}
	if err := cfg.validateEyeChecks(); err != nil {
		return nil, err
	}
	if err := cfg.validateVerify(); err != nil {
		return nil, err
	}
	if err := cfg.validateOverflow(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateVerify checks that verification can happen where it is asked for:
// a repository that verifies needs a worker that verifies and takes its
// issues, or its reviews would wait for a verifier forever.
// validateOverflow checks that a released issue can be taken: some machine
// lists the overflow label as a backfill source, and no machine has it as
// its own queue.
func (c *Config) validateOverflow() error {
	var errs []error
	for _, a := range c.Accounts {
		if a.Overflow == "" {
			continue
		}
		taken := false
		for _, m := range c.Machines {
			for _, src := range append(append([]IssueSource{}, m.Issues...), workerSources(m)...) {
				switch {
				case src.Label == a.Overflow && src.Backfill:
					taken = true
				case src.Label == a.Overflow:
					errs = append(errs, fmt.Errorf("accounts: %s: overflow %q is the queue of machine %q; list it with backfill: true", a.Name, a.Overflow, m.Name))
				}
			}
		}
		if !taken {
			errs = append(errs, fmt.Errorf("accounts: %s: no machine lists overflow %q as a backfill source, so nothing would take a released issue", a.Name, a.Overflow))
		}
	}
	return errors.Join(errs...)
}

func workerSources(m Machine) []IssueSource {
	var all []IssueSource
	for _, w := range m.Workers {
		all = append(all, w.Issues...)
	}
	return all
}

func (c *Config) validateVerify() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for _, r := range c.Repos {
		if r.Verify == nil {
			continue
		}
		covered := false
		for _, m := range c.Machines {
			for _, w := range m.Workers {
				if w.Verifies && slices.ContainsFunc(m.SourcesFor(w), func(s IssueSource) bool { return s.Repo == r.Name }) {
					covered = true
				}
			}
		}
		if !covered {
			fail("repos: %s: verify needs a worker with verifies: true whose queue takes %s", r.Name, r.Name)
		}
		if m := r.Verify.MergeMethod; m != "" && m != "merge" && m != "squash" && m != "rebase" {
			fail("repos: %s: verify.merge_method %q must be merge, squash or rebase", r.Name, m)
		}
		if r.Verify.MergeHigh && !r.Verify.MergeLow {
			fail("repos: %s: verify.merge_high needs merge_low: a high-risk pass is queued only where a low-risk one is", r.Name)
		}
		if r.Verify.MergeTrain != "" && !r.Verify.MergeLow {
			fail("repos: %s: verify.merge_train merges what merge_low queues: turn on merge_low too", r.Name)
		}
		if r.Verify.MergeTrain != TrainBatch && (r.Verify.BatchSize != 0 || len(r.Verify.MergeTrainAlone) > 0) {
			fail("repos: %s: verify.batch_size and verify.merge_train_alone are for merge_train: batch", r.Name)
		}
		if r.Verify.BatchSize < 0 {
			fail("repos: %s: verify.batch_size must be 1 or more", r.Name)
		}
		for _, a := range r.Verify.Areas {
			if a.Name == "" || len(a.Paths) == 0 {
				fail("repos: %s: verify.areas needs a name and paths in every entry", r.Name)
			}
		}
	}
	for _, m := range c.Machines {
		for _, w := range m.Workers {
			if !w.Verifies {
				continue
			}
			if w.Ask {
				fail("machine %q: worker %q answers questions and verifies nothing: remove verifies, or remove ask", m.Name, w.Name)
			}
			if !slices.ContainsFunc(m.SourcesFor(w), func(s IssueSource) bool { r, ok := c.Repo(s.Repo); return ok && r.Verify != nil }) {
				fail("machine %q: worker %q has verifies, but no repository of its queue has verify under repos", m.Name, w.Name)
			}
		}
	}
	return errors.Join(errs...)
}

// validateEyeChecks runs after the default signals are in place: a worker
// that does eye checks needs to know where staging is and what to write.
func (c *Config) validateEyeChecks() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for _, m := range c.Machines {
		for _, w := range m.Workers {
			if !w.EyeChecks {
				continue
			}
			for _, src := range m.SourcesFor(w) {
				if _, ok := c.Repo(src.Repo); !ok {
					fail("machine %q: worker %q has eye_checks, but %s has no entry under repos", m.Name, w.Name, src.Repo)
				}
			}
			eyes := false
			for _, s := range c.Signals {
				if s.Name == EyesSignal {
					eyes = s.AnsweredBy != "" && s.FailedBy != ""
				}
			}
			if !eyes {
				fail("machine %q: worker %q has eye_checks, but no signal named %q has answered_by and failed_by", m.Name, w.Name, EyesSignal)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(c.Machines) == 0 {
		fail("no machines defined")
	}
	for _, ch := range c.Defaults.Checks {
		if ch.Name == "" || ch.Run == "" {
			fail("defaults: a check needs name and run")
		}
	}
	seen := map[string]bool{}
	for i, m := range c.Machines {
		where := fmt.Sprintf("machine %q", m.Name)
		if !nameRE.MatchString(m.Name) {
			fail("machine #%d: name %q must match %s", i+1, m.Name, nameRE)
		}
		if seen[m.Name] {
			fail("%s: duplicate name", where)
		}
		seen[m.Name] = true
		if m.Host == "" {
			fail("%s: host is required (an ssh target, or %q)", where, LocalHost)
		}
		for _, ch := range m.Checks {
			if ch.Name == "" || ch.Run == "" {
				fail("%s: a check needs name and run", where)
			}
		}
		checkSources := func(where string, sources []IssueSource) {
			for _, src := range sources {
				if !repoRE.MatchString(src.Repo) {
					fail("%s: issue repo %q must be owner/name", where, src.Repo)
				}
				if src.Label == "" && src.Assignee == "" && src.Query == "" {
					fail("%s: issue source %s needs a label, an assignee or a query", where, src.Repo)
				}
				if src.Backfill && claimLabel(sources, src.Repo) == "" {
					fail("%s: backfill source %s needs a source with a label for the same repo beside it: an issue that is taken gets that label", where, src.Repo)
				}
			}
		}
		checkSources(where, m.Issues)
		tasks := map[string]bool{}
		for _, t := range m.Tasks {
			if !nameRE.MatchString(t.Name) {
				fail("%s: task name %q must match %s", where, t.Name, nameRE)
			}
			if tasks[t.Name] {
				fail("%s: duplicate task %q", where, t.Name)
			}
			tasks[t.Name] = true
			if t.Run == "" {
				fail("%s: task %q has no run command", where, t.Name)
			}
			if _, err := ParseSchedule(t.Schedule); err != nil {
				fail("%s: task %q schedule %q: %v", where, t.Name, t.Schedule, err)
			}
			if t.Timeout != "" {
				if d, err := time.ParseDuration(t.Timeout); err != nil || d <= 0 {
					fail("%s: task %q timeout %q is not a positive duration such as 30m", where, t.Name, t.Timeout)
				}
			}
		}
		if m.Capacity.MaxLoad < 0 {
			fail("%s: capacity.max_load must not be negative", where)
		}
		if r := m.Capacity.ReleaseAfter; r != "" {
			if d, err := time.ParseDuration(r); err != nil || d <= 0 {
				fail("%s: capacity.release_after %q is not a positive duration such as 30m", where, r)
			}
			if !m.Capacity.Set() {
				fail("%s: capacity.release_after needs max_load or busy_when to say when the machine is busy", where)
			}
			if a, ok := c.AccountOf(m.Name); !ok || a.Overflow == "" {
				fail("%s: capacity.release_after needs an account with overflow, the label a machine with room takes released work from", where)
			}
		}
		if n := m.Capacity.Nice; n < 0 || n > MaxNice {
			fail("%s: capacity.nice %d must be from 0 to %d", where, n, MaxNice)
		}
		if w := m.Capacity.MaxWait; w != "" {
			if d, err := time.ParseDuration(w); err != nil || d <= 0 {
				fail("%s: capacity.max_wait %q is not a positive duration such as 20m", where, w)
			}
		}
		workers := map[string]bool{}
		for _, w := range m.Workers {
			if !nameRE.MatchString(w.Name) {
				fail("%s: worker name %q must match %s", where, w.Name, nameRE)
			}
			if workers[w.Name] {
				fail("%s: duplicate worker %q", where, w.Name)
			}
			workers[w.Name] = true
			if w.Dir == "" {
				fail("%s: worker %q has no dir", where, w.Name)
			}
			if strings.TrimSpace(w.Brief) == "" {
				fail("%s: worker %q has no brief", where, w.Name)
			}
			checkSources(fmt.Sprintf("%s: worker %q", where, w.Name), w.Issues)
			for name := range w.Env {
				if !envNameRE.MatchString(name) {
					fail("%s: worker %q: env %q is not a variable name", where, w.Name, name)
				}
			}
			if w.Ask {
				if len(w.Issues) > 0 {
					fail("%s: worker %q answers questions and has no queue: remove issues, or remove ask", where, w.Name)
				}
				if w.EyeChecks {
					fail("%s: worker %q answers questions and does no eye checks: remove eye_checks, or remove ask", where, w.Name)
				}
				if w.FreshPerIssue {
					fail("%s: worker %q answers questions and is handed no issue: remove fresh_per_issue, or remove ask", where, w.Name)
				}
			}
		}
	}
	if c.Supervisor.LowQueue < 0 {
		fail("supervisor: low_queue must not be negative")
	}
	for name, value := range map[string]string{"cache_ttl": c.Supervisor.CacheTTL, "scope_every": c.Supervisor.ScopeEvery, "no_work_after": c.Supervisor.NoWorkAfter, "own_wait_max": c.Supervisor.OwnWaitMax} {
		if value == "" {
			continue
		}
		if d, err := time.ParseDuration(value); err != nil || d <= 0 {
			fail("supervisor: %s %q is not a positive duration such as 1h", name, value)
		}
	}
	if w := c.Supervisor.WhenCold; w != "" && w != ColdClear && w != ColdResume {
		fail("supervisor: when_cold %q must be %s or %s", w, ColdClear, ColdResume)
	}
	if mc := c.Supervisor.ModelCommand; mc != "" && !strings.Contains(mc, modelPlaceholder) {
		fail("supervisor: model_command %q must contain %s", mc, modelPlaceholder)
	}
	if a := c.Supervisor.Models.Apply; a != "" && a != ModelSwitch && a != ModelTell {
		fail("supervisor: models.apply %q must be %s or %s", a, ModelSwitch, ModelTell)
	}
	inAccount := map[string]string{}
	for _, a := range c.Accounts {
		if a.Name == "" {
			fail("accounts: every account needs a name")
		}
		paced := a.WeeklyBudget != 0 || a.Resets != ""
		if !paced && a.Overflow == "" {
			fail("accounts: %s: give it weekly_budget and resets to pace it, or overflow to share its work", a.Name)
		}
		if paced && a.WeeklyBudget <= 0 {
			fail("accounts: %s: weekly_budget must be a positive amount in dollars at API rates", a.Name)
		}
		if _, err := time.Parse(resetLayout, a.Resets); paced && err != nil {
			fail("accounts: %s: resets %q must be a day and a time, such as \"Wed 18:00\"", a.Name, a.Resets)
		}
		if a.HoldAllBut != nil && *a.HoldAllBut < 0 {
			fail("accounts: %s: hold_all_but must not be negative", a.Name)
		}
		for _, m := range a.Machines {
			if _, ok := c.Machine(m); !ok {
				fail("accounts: %s: machine %q is not in the fleet file", a.Name, m)
			}
			if other, ok := inAccount[m]; ok {
				fail("accounts: machine %s is in both %s and %s", m, other, a.Name)
			}
			inAccount[m] = a.Name
		}
	}
	mixed := false
	for label, a := range c.Supervisor.Models.ApplyLabels {
		if a != ModelSwitch && a != ModelTell {
			fail("supervisor: models.apply_labels %s: %q must be %s or %s", label, a, ModelSwitch, ModelTell)
		}
		if (a == ModelTell) != c.Supervisor.Models.TellOnly() {
			mixed = true
		}
	}
	for tier := range c.Supervisor.Models.Verify {
		if tier != TierLow && tier != TierHigh {
			fail("supervisor: models.verify %q must be %s or %s", tier, TierLow, TierHigh)
		}
	}
	if m := c.Supervisor.Models; len(m.Verify) > 0 && m.EyeChecks == "" {
		switch {
		case (m.TellOnly() || mixed) && m.Session == "":
			fail("supervisor: models.verify needs models.session: the model a session goes back to after a verification")
		case !m.TellOnly() && !mixed && m.Default == "":
			fail("supervisor: models.verify needs models.default: the model the next issue goes back to after a verification")
		}
	}
	if m := c.Supervisor.Models; m.EyeChecks != "" {
		switch {
		case (m.TellOnly() || mixed) && m.Session == "":
			fail("supervisor: models.eye_checks needs models.session: the model a session goes back to after a look")
		case !m.TellOnly() && !mixed && m.Default == "":
			fail("supervisor: models.eye_checks needs models.default: the model the next issue goes back to after a look")
		}
	}
	if mixed && c.Supervisor.Models.Session == "" {
		fail("supervisor: models.apply_labels mixes switch and tell, so models.session must name the model a told session runs on")
	}
	for label, model := range c.Supervisor.Models.Labels {
		if label == "" || model == "" {
			fail("supervisor: models.labels needs a label and a model in every entry")
		}
	}
	for _, s := range c.Signals {
		if s.Name == "" || s.Ask == "" {
			fail("a signal needs name and ask")
		}
		if s.Grace != "" {
			if d, err := time.ParseDuration(s.Grace); err != nil || d <= 0 {
				fail("signal %q: grace %q is not a positive duration such as 15m", s.Name, s.Grace)
			}
		}
	}
	repos := map[string]bool{}
	for _, r := range c.Repos {
		where := fmt.Sprintf("repos: %s", r.Name)
		if !repoRE.MatchString(r.Name) {
			fail("repos: repo %q must be owner/name", r.Name)
		}
		if repos[r.Name] {
			fail("%s: duplicate entry", where)
		}
		repos[r.Name] = true
		deploys := r.Staging != (Environment{}) || r.Production != (Environment{}) || r.LookBack != ""
		switch {
		case deploys && r.Staging.Commit == "":
			fail("%s: staging.commit is required: a command that prints the commit staging serves", where)
		case !deploys && r.Gates.Command == "" && r.Verify == nil && r.MaxOpenPRs == 0:
			fail("%s: give it staging, gates, verify or max_open_prs, or leave it out", where)
		}
		if r.MaxOpenPRs < 0 {
			fail("%s: max_open_prs %d must not be negative", where, r.MaxOpenPRs)
		}
		switch c := r.MaxOpenPRsCount; {
		case c != "" && c != PRCountAll && c != PRCountReady:
			fail("%s: max_open_prs_count %q must be all or ready", where, c)
		case c != "" && r.MaxOpenPRs == 0:
			fail("%s: max_open_prs_count needs max_open_prs", where)
		}
		if r.Gates.Timeout != "" {
			if d, err := time.ParseDuration(r.Gates.Timeout); err != nil || d <= 0 {
				fail("%s: gates.timeout %q is not a positive duration such as 20m", where, r.Gates.Timeout)
			}
		}
		if r.Gates.Timeout != "" && r.Gates.Command == "" {
			fail("%s: gates.timeout needs gates.command", where)
		}
		if r.Staging.Viewport != "" && !viewportRE.MatchString(r.Staging.Viewport) {
			fail("%s: staging.viewport %q is not a size such as 1440x900", where, r.Staging.Viewport)
		}
		if r.Production.URL != "" || r.Production.Orders != "" || r.Production.Viewport != "" || r.Production.Deploy != "" || r.Production.DeployFrom != "" || r.Production.DeployWhen != (DeployWhen{}) {
			fail("%s: production takes only commit; no worker goes there", where)
		}
		switch st := r.Staging; {
		case st.Deploy != "" && st.DeployFrom == "":
			fail("%s: staging.deploy needs staging.deploy_from: the machine whose agent deploys", where)
		case st.Deploy == "" && (st.DeployFrom != "" || st.DeployWhen != (DeployWhen{})):
			fail("%s: staging.deploy_from and staging.deploy_when need staging.deploy", where)
		case st.DeployFrom != "":
			if _, ok := c.Machine(st.DeployFrom); !ok {
				fail("%s: staging.deploy_from %q is not a machine in the fleet file", where, st.DeployFrom)
			}
		}
		if after := r.Staging.DeployWhen.OrAfter; after != "" {
			if d, err := time.ParseDuration(after); err != nil || d <= 0 {
				fail("%s: staging.deploy_when.or_after %q is not a positive duration such as 4h", where, after)
			}
		}
		if r.LookBack != "" {
			if d, err := time.ParseDuration(r.LookBack); err != nil || d <= 0 {
				fail("%s: look_back %q is not a positive duration such as 336h", where, r.LookBack)
			}
		}
	}
	return errors.Join(errs...)
}

// ParseSchedule accepts five-field cron expressions and descriptors such as
// "@daily" or "@every 6h".
func ParseSchedule(s string) (cron.Schedule, error) {
	return cron.ParseStandard(s)
}

// Machine returns the machine with the given name.
func (c *Config) Machine(name string) (Machine, bool) {
	for _, m := range c.Machines {
		if m.Name == name {
			return m, true
		}
	}
	return Machine{}, false
}

// SourcesFor returns the worker's queue: its own issue sources, or the
// machine's when it has none. A backfill source is not among them.
func (m Machine) SourcesFor(w Worker) []IssueSource {
	return m.sources(w, false)
}

// BackfillFor returns the worker's backfill sources: where it takes an issue
// from when its queue has nothing it can act on.
func (m Machine) BackfillFor(w Worker) []IssueSource {
	return m.sources(w, true)
}

// Builders returns the workers that are handed work: all but the ones that
// answer the owner's questions.
func (m Machine) Builders() []Worker {
	var builders []Worker
	for _, w := range m.Workers {
		if !w.Ask {
			builders = append(builders, w)
		}
	}
	return builders
}

// TakesWork returns nil when the machine has a worker of this name that is
// handed work, and otherwise says why an issue or a task cannot go to it.
func (m Machine) TakesWork(name string) error {
	for _, w := range m.Workers {
		switch {
		case w.Name != name:
		case w.Ask:
			return fmt.Errorf("worker %q of machine %s answers questions and is handed no work", name, m.Name)
		default:
			return nil
		}
	}
	return fmt.Errorf("machine %s has no worker %q", m.Name, name)
}

func (m Machine) sources(w Worker, backfill bool) []IssueSource {
	// A worker that answers questions has no queue, not even the machine's.
	if w.Ask {
		return nil
	}
	all := m.Issues
	if len(w.Issues) > 0 {
		all = w.Issues
	}
	var sources []IssueSource
	for _, src := range all {
		if src.Backfill == backfill {
			sources = append(sources, src)
		}
	}
	return sources
}

// ReserveSources returns every backfill source on the machine, each once:
// the machine's and its workers'.
func (m Machine) ReserveSources() []IssueSource {
	var all []IssueSource
	seen := map[IssueSourceKey]bool{}
	for _, w := range m.Builders() {
		for _, src := range m.BackfillFor(w) {
			if !seen[src.Key()] {
				seen[src.Key()] = true
				all = append(all, src)
			}
		}
	}
	return all
}

// ClaimLabel is the label a backfill issue of the repository gets when this
// worker takes it.
func (m Machine) ClaimLabel(w Worker, repo string) string {
	return claimLabel(m.SourcesFor(w), repo)
}

// AllSources returns every issue source on the machine, each once: the
// machine's and its workers'. A backfill source is not among them: its
// issues are in nobody's queue until a worker takes one.
func (m Machine) AllSources() []IssueSource {
	var all []IssueSource
	seen := map[IssueSourceKey]bool{}
	add := func(sources []IssueSource) {
		for _, src := range sources {
			if !src.Backfill && !seen[src.Key()] {
				seen[src.Key()] = true
				all = append(all, src)
			}
		}
	}
	add(m.Issues)
	for _, w := range m.Workers {
		add(w.Issues)
	}
	return all
}

// QueueLabels returns the labels that put an issue in a queue of the fleet.
// An issue with one of them is taken, whatever a backfill source lists.
// EyeCheckRepos lists the repositories whose eye checks a worker does on
// staging, after the merge: each is deployed to staging, and a worker with
// eye_checks on some machine of the fleet takes its issues.
func (c *Config) EyeCheckRepos() []string {
	var repos []string
	for _, m := range c.Machines {
		for _, w := range m.Workers {
			if !w.EyeChecks {
				continue
			}
			for _, src := range m.SourcesFor(w) {
				if r, ok := c.Repo(src.Repo); ok && r.Deployed() && !slices.Contains(repos, r.Name) {
					repos = append(repos, r.Name)
				}
			}
		}
	}
	return repos
}

func (c *Config) QueueLabels() []string {
	var labels []string
	for _, m := range c.Machines {
		for _, src := range m.AllSources() {
			if src.Label != "" && !slices.Contains(labels, src.Label) {
				labels = append(labels, src.Label)
			}
		}
	}
	return labels
}

// hasBackfill reports whether any list of the fleet has a backfill source.
func (c *Config) hasBackfill() bool {
	for _, m := range c.Machines {
		lists := [][]IssueSource{m.Issues}
		for _, w := range m.Workers {
			lists = append(lists, w.Issues)
		}
		for _, list := range lists {
			if slices.ContainsFunc(list, func(src IssueSource) bool { return src.Backfill }) {
				return true
			}
		}
	}
	return false
}

// ChecksFor returns the default checks followed by the machine's own.
func (c *Config) ChecksFor(m Machine) []Check {
	return append(append([]Check{}, c.Defaults.Checks...), m.Checks...)
}

// Command returns the shell script for one check or task on this machine: the
// machine's init line, then the command.
func (m Machine) Command(run string) string {
	if m.Init == "" {
		return run
	}
	return m.Init + "\n" + run
}

// CommandOrDefault returns the command that starts the worker's session.
func (w Worker) CommandOrDefault() string {
	if w.Command == "" {
		return DefaultWorkerCommand
	}
	return w.Command
}

func (s Supervisor) CommandOrDefault() string {
	if s.Command == "" {
		return DefaultReviewCommand
	}
	return s.Command
}

func (s Supervisor) CacheTTLOrDefault() time.Duration {
	return durationOr(s.CacheTTL, DefaultCacheTTL)
}

func (s Supervisor) ScopeEveryOrDefault() time.Duration {
	return durationOr(s.ScopeEvery, DefaultScopeEvery)
}

// OwnWaitMaxOrDefault is how long an idle worker may wait on work of its
// own before it is told to check it; 0 for no bound.
func (s Supervisor) OwnWaitMaxOrDefault() time.Duration {
	if s.OwnWaitMax == "0" {
		return 0
	}
	return durationOr(s.OwnWaitMax, DefaultOwnWaitMax)
}

// ClearWhenCold reports whether a nudge to a cold worker clears its context.
func (s Supervisor) ClearWhenCold() bool {
	return s.WhenCold != ColdResume
}

func (s Supervisor) ClearCommandOrDefault() string {
	if s.ClearCommand == "" {
		return DefaultClearCommand
	}
	return s.ClearCommand
}

// ModelCommandFor returns the line that switches a worker to the model.
func (s Supervisor) ModelCommandFor(model string) string {
	command := s.ModelCommand
	if command == "" {
		command = DefaultModelCommand
	}
	return strings.ReplaceAll(command, modelPlaceholder, model)
}

// durationOr parses a duration that validation has already accepted.
func durationOr(value string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(value); err == nil {
		return d
	}
	return fallback
}

// TimeoutOrDefault returns the task's timeout. Validation has already
// rejected a value that does not parse.
func (t Task) TimeoutOrDefault() time.Duration {
	return durationOr(t.Timeout, DefaultTaskTimeout)
}
