// Package config loads and validates the fleet file (shed.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
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
	DefaultReviewCommand = "claude -p --model sonnet"
	DefaultCacheTTL      = 5 * time.Minute
	DefaultScopeEvery    = 30 * time.Minute
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
}

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
	// HandBack is the config's hand-back phrase, copied onto each signal
	// when the config is read.
	HandBack string `yaml:"-"`
}

// DefaultSignals returns the built-in signals, counted in hand-back comments.
func DefaultSignals() []Signal {
	return withHandBack(DefaultHandBack, []Signal{
		{Name: "decision", Ask: "Decisions needed:", Clear: []string{"none"}, AnsweredBy: "Owner ruling", SendsBack: true},
		{Name: "eyes", Ask: "Needs eyes:", Clear: []string{"nothing", "none"}, AnsweredBy: "Eyes checked"},
		{Name: "review", Ask: "**PR:**", FollowsPR: true},
	})
}

func withHandBack(phrase string, signals []Signal) []Signal {
	for i := range signals {
		signals[i].HandBack = phrase
	}
	return signals
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	repoRE = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)
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
	withHandBack(handBack, cfg.Signals)
	return &cfg, nil
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
				if src.Label == "" && src.Assignee == "" {
					fail("%s: issue source %s needs a label or an assignee", where, src.Repo)
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
		}
	}
	for name, value := range map[string]string{"cache_ttl": c.Supervisor.CacheTTL, "scope_every": c.Supervisor.ScopeEvery} {
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
	for label, model := range c.Supervisor.Models.Labels {
		if label == "" || model == "" {
			fail("supervisor: models.labels needs a label and a model in every entry")
		}
	}
	for _, s := range c.Signals {
		if s.Name == "" || s.Ask == "" {
			fail("a signal needs name and ask")
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
// machine's when it has none.
func (m Machine) SourcesFor(w Worker) []IssueSource {
	if len(w.Issues) > 0 {
		return w.Issues
	}
	return m.Issues
}

// AllSources returns every issue source on the machine, each once: the
// machine's and its workers'.
func (m Machine) AllSources() []IssueSource {
	var all []IssueSource
	seen := map[string]bool{}
	add := func(sources []IssueSource) {
		for _, src := range sources {
			key := src.Repo + "\x00" + src.Label + "\x00" + src.Assignee
			if !seen[key] {
				seen[key] = true
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
