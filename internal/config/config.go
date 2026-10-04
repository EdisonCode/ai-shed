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
}

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
}

type Machine struct {
	Name    string        `yaml:"name"`
	Host    string        `yaml:"host"`
	Init    string        `yaml:"init"`
	Checks  []Check       `yaml:"checks"`
	Issues  []IssueSource `yaml:"issues"`
	Tasks   []Task        `yaml:"tasks"`
	Workers []Worker      `yaml:"workers"`
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
}

func DefaultSignals() []Signal {
	return []Signal{
		{Name: "decision", Ask: "Decisions needed:", Clear: []string{"none"}, AnsweredBy: "Owner ruling"},
		{Name: "eyes", Ask: "Needs eyes:", Clear: []string{"nothing", "none"}, AnsweredBy: "Eyes checked"},
		{Name: "review", Ask: "**PR:**", FollowsPR: true},
	}
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
		for _, src := range m.Issues {
			if !repoRE.MatchString(src.Repo) {
				fail("%s: issue repo %q must be owner/name", where, src.Repo)
			}
			if src.Label == "" && src.Assignee == "" {
				fail("%s: issue source %s needs a label or an assignee", where, src.Repo)
			}
		}
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
