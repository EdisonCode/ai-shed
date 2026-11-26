package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/edisoncode/ai-shed/skills"
)

// cmdSkill prints the shed skill this shed ships, installs it where Claude
// Code reads it, or says whether the installed one has fallen behind.
func cmdSkill(args []string) int {
	fs := flag.NewFlagSet("skill", flag.ContinueOnError)
	install := fs.Bool("install", false, "write the skill to "+skills.ShedPath("~"))
	check := fs.Bool("check", false, "exit 1 when the installed skill is not the one this shed ships")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if !*install && !*check {
		os.Stdout.Write(skills.Shed)
		return exitOK
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	if *install {
		path, err := skills.InstallShed(home)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("installed the shed skill of shed %s at %s\n", version, path)
		return exitOK
	}
	if warnStaleSkill(home) {
		return exitAttention
	}
	return exitOK
}

// warnStaleSkill says when the installed shed skill is not the one this
// shed ships, and reports whether it said so.
func warnStaleSkill(home string) bool {
	stale, err := skills.ShedStale(home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shed: %v\n", err)
		return true
	}
	if stale {
		fmt.Printf("The shed skill at %s is not the one shed %s ships: an assistant reading it misses what changed. Run `shed skill -install` to replace it.\n", skills.ShedPath(home), version)
	}
	return stale
}
