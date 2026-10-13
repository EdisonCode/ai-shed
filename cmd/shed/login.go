package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// loginReport is what `shed login -here -json` prints on a machine.
type loginReport struct {
	Version string `json:"version"`
	// URL is set by the first step: where the owner signs in.
	URL string `json:"url,omitempty"`
	// The rest is set by the second step, once the code was taken.
	Account     string   `json:"account,omitempty"`
	Resumed     []string `json:"resumed,omitempty"`
	Reconnected []string `json:"reconnected,omitempty"`
	ByHand      []string `json:"by_hand,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// remoteControlLost matches a worker screen that says its Remote Control
// connection ended, as a change of account does.
var (
	remoteControlRE = regexp.MustCompile(`(?i)remote control`)
	disconnectedRE  = regexp.MustCompile(`(?i)organization changed|disconnected`)
)

// cmdLogin moves a machine's tool to another account in two steps, both
// from the owner's machine. `shed login <machine>` prints the sign-in URL.
// The owner opens it, signs in, and gives the code the page shows to
// `shed login -code <code> <machine>`.
func cmdLogin(args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	code := fs.String("code", "", "the code the sign-in page gave")
	here := fs.Bool("here", false, "run the step on this machine (shed runs this over ssh)")
	machine := fs.String("machine", "", "with -here: this machine's name in the fleet file")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if *code != "" {
		if err := supervisor.ValidLoginCode(*code); err != nil {
			return fail(err)
		}
	}
	if *here {
		return loginHere(cfg, path, *machine, *code)
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: shed login [-code <code>] <machine>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	command := `"$HOME/` + deploy.RemoteBinary + `" login -here -json`
	if *code != "" {
		// The code was checked: it has no character a shell reads.
		command += " -code '" + *code + "'"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, runErr := probe.ShellRunner{}.Run(ctx, m, m.Command(command), nil)
	var report loginReport
	if err := json.Unmarshal(out, &report); err != nil {
		if runErr != nil {
			return fail(fmt.Errorf("login on %s: %w", m.Name, runErr))
		}
		return fail(fmt.Errorf("login on %s: read the machine's report: %w", m.Name, err))
	}
	if report.Version != version {
		return fail(fmt.Errorf("login on %s: the machine runs shed %s, this is %s: deploy it first", m.Name, report.Version, version))
	}
	if report.Error != "" {
		fmt.Fprintf(os.Stderr, "%s: %s\n", m.Name, report.Error)
		return exitAttention
	}
	if *code == "" {
		fmt.Printf("Open this URL, sign in with the account %s should use, and copy the code the page shows:\n\n%s\n\nThen run:\n\n  shed login -code <code> %s\n", m.Name, report.URL, m.Name)
		return exitOK
	}
	account := report.Account
	if account == "" {
		account = "an account its tool does not name"
	}
	fmt.Printf("%s is logged in as %s.\n", m.Name, account)
	if len(report.Resumed) > 0 {
		fmt.Printf("Usage-limit holds ended for: %v\n", report.Resumed)
	}
	if len(report.Reconnected) > 0 {
		fmt.Printf("Remote Control was asked to reconnect in: %v\n", report.Reconnected)
	}
	if len(report.ByHand) > 0 {
		fmt.Printf("Remote Control dropped and the worker was busy; type /remote-control in it when it stops: %v\n", report.ByHand)
		return exitAttention
	}
	return exitOK
}

// loginHere runs one step of the login on this machine and prints the
// report as JSON. A step that fails says so in the report, so the owner's
// machine can show why.
func loginHere(cfg *config.Config, path, machine, code string) int {
	report := loginReport{Version: version}
	if err := loginStep(cfg, path, machine, code, &report); err != nil {
		report.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return fail(err)
	}
	if report.Error != "" {
		return exitAttention
	}
	return exitOK
}

func loginStep(cfg *config.Config, path, machine, code string, report *loginReport) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	name, err := thisMachine(machine, home)
	if err != nil {
		return err
	}
	m, ok := cfg.Machine(name)
	if !ok {
		return fmt.Errorf("machine %q is not in %s", name, path)
	}
	term := supervisor.Tmux{}
	if code == "" {
		command := config.DefaultWorkerCommand
		if len(m.Workers) > 0 {
			command = m.Workers[0].CommandOrDefault()
		}
		report.URL, err = supervisor.StartLogin(term, command, home, time.Now, time.Sleep)
		return err
	}
	if _, err := supervisor.FinishLogin(term, code, time.Now, time.Sleep); err != nil {
		return err
	}
	if report.Account, err = supervisor.Account(home); err != nil {
		return err
	}
	// The new account has its own limit: no worker waits on the old one.
	state := filepath.Join(home, runlog.StateDir)
	resume := filepath.Join(state, runlog.ResumeDir)
	if err := os.MkdirAll(resume, 0o755); err != nil {
		return fmt.Errorf("end the usage-limit holds: %w", err)
	}
	var errs []error
	for _, w := range m.Workers {
		if err := os.WriteFile(filepath.Join(resume, w.Name), nil, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("end the usage-limit hold of %s: %w", w.Name, err))
			continue
		}
		report.Resumed = append(report.Resumed, w.Name)
		screen, err := term.Capture(w.Name, 40)
		if err != nil || !remoteControlRE.MatchString(screen) || !disconnectedRE.MatchString(screen) {
			continue
		}
		// A command typed into a turn in progress would be read as a
		// message, so only a worker at rest is asked to reconnect.
		if mark, _ := runlog.ReadMark(state, w.Name); mark.State != runlog.MarkIdle {
			report.ByHand = append(report.ByHand, w.Name)
			continue
		}
		if err := term.Send(w.Name, "/remote-control"); err != nil {
			errs = append(errs, fmt.Errorf("reconnect Remote Control in %s: %w", w.Name, err))
			continue
		}
		report.Reconnected = append(report.Reconnected, w.Name)
	}
	return errors.Join(errs...)
}
