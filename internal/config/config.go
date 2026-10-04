// Package config loads and validates the fleet file (shed.yaml).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// LocalHost is the host value for a machine that is reached without SSH.
const LocalHost = "local"

// DefaultTaskTimeout stops a hung task from blocking its schedule forever.
const DefaultTaskTimeout = time.Hour

type Config struct {
	Defaults Defaults  `yaml:"defaults"`
	Machines []Machine `yaml:"machines"`
	Signals  []Signal  `yaml:"signals"`
}

type Defaults struct {
	Checks []Check `yaml:"checks"`
}

type Machine struct {
	Name   string        `yaml:"name"`
	Host   string        `yaml:"host"`
	Init   string        `yaml:"init"`
	Checks []Check       `yaml:"checks"`
	Issues []IssueSource `yaml:"issues"`
	Tasks  []Task        `yaml:"tasks"`
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
}

func DefaultSignals() []Signal {
	return []Signal{
		{Name: "decision", Ask: "Decisions needed:", Clear: []string{"none"}, AnsweredBy: "Owner ruling"},
		{Name: "eyes", Ask: "Needs eyes:", Clear: []string{"nothing", "none"}},
		{Name: "review", Ask: "**PR:**"},
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

// TimeoutOrDefault returns the task's timeout. Validation has already
// rejected a value that does not parse.
func (t Task) TimeoutOrDefault() time.Duration {
	if d, err := time.ParseDuration(t.Timeout); err == nil {
		return d
	}
	return DefaultTaskTimeout
}
