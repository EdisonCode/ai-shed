package main

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestTheMachineSideOfLoginTakesEveryFlagTheOwnerSideSends(t *testing.T) {
	for _, code := range []string{"", "AbC-123_x#state"} {
		args := remoteLoginArgs(code)
		if args[0] != "login" {
			t.Fatalf("args = %q, want the login command", args)
		}
		fs, gotCode, here, _ := loginFlags()
		if err := fs.Parse(args[1:]); err != nil {
			t.Fatalf("the machine side rejects %q: %v", args, err)
		}
		if !*here || *gotCode != code || fs.NArg() != 0 {
			t.Fatalf("parsed %q as here = %v, code = %q, args = %q", args, *here, *gotCode, fs.Args())
		}
	}
}

func TestLoginStartsTheToolInAWorkersFolderItAlreadyTrusts(t *testing.T) {
	m := config.Machine{Workers: []config.Worker{{Name: "app", Dir: "~/src/app", Command: "claude --permission-mode auto"}, {Name: "docs", Dir: "/srv/docs"}}}
	command, dir := loginStart(m, "/home/owner")
	if command != "claude --permission-mode auto" || dir != "/home/owner/src/app" {
		t.Fatalf("start = %q in %q, want the first worker's command in its folder", command, dir)
	}
	if command, dir := loginStart(config.Machine{}, "/home/owner"); command != config.DefaultWorkerCommand || dir != "/home/owner" {
		t.Fatalf("start with no worker = %q in %q", command, dir)
	}
}
