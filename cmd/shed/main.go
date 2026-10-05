// Command shed watches and manages the worker machines of a homelab.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/edisoncode/ai-shed/internal/config"
)

// version is set at build time; see the Makefile.
var version = "dev"

const usage = `shed watches the worker machines in your shed.

Usage:
  shed status [-json] [-watch 1m]   report every machine; exit 1 when something needs you
  shed digest [-since 24h] [-json]  what merged, how the eye checks went, what waits on you
  shed validate                     check the fleet file
  shed deploy [-install-agent] [machine...]
                                    send this version and the fleet file to machines
  shed task [-worker name] <machine> <brief-file|->
                                    push a one-off task with its own brief, ahead of the queue
  shed task -report <id> <machine>  print the report a worker wrote for a task
  shed task -cancel <id> <machine>  cancel a task that no worker was handed yet
  shed bump [-worker name] <machine> <issue>
                                    put an open issue first in its queue, or in that worker's
  shed bump -undo <machine> <issue> put it back in its usual place
  shed pause [-for 30m] <machine>   hand out nothing and nudge nobody for a while
  shed resume <machine> [worker]    end a pause early, and a hold at a usage limit that is over
  shed recycle [-now] <machine> <worker>
                                    give a worker a fresh session after its current issue
  shed tidy [-apply] <machine>      list the worktrees and branches whose pull request is merged;
                                    with -apply, remove them
  shed template issue               print an issue template for issues a worker can take
  shed update                       replace this shed with the latest release
  shed preflight [-machine name]    on a machine: check that each worker's command comes up ready
  shed agent [-machine name]        on a machine: supervise its workers and run its tasks
  shed mark working|idle|waiting    in a worker's tool hook: tell shed the worker's state
  shed version                      print the version

Every command accepts -config <path>. Without it, shed reads $SHED_CONFIG,
then ./shed.yaml, then ~/.config/shed/shed.yaml.
`

// Exit codes: 0 all good, 1 something needs the owner, 2 usage or config error.
const (
	exitOK        = 0
	exitAttention = 1
	exitError     = 2
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitError)
	}
	var code int
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "status":
		code = cmdStatus(args)
	case "agent":
		code = cmdAgent(args)
	case "deploy":
		code = cmdDeploy(args)
	case "validate":
		code = cmdValidate(args)
	case "preflight":
		code = cmdPreflight(args)
	case "recycle":
		code = cmdRecycle(args)
	case "update":
		code = cmdUpdate(args)
	case "mark":
		code = cmdMark(args)
	case "tidy":
		code = cmdTidy(args)
	case "task":
		code = cmdTask(args)
	case "pause":
		code = cmdPause(args)
	case "resume":
		code = cmdResume(args)
	case "bump":
		code = cmdBump(args)
	case "digest":
		code = cmdDigest(args)
	case "template":
		code = cmdTemplate(args)
	case "version":
		fmt.Println("shed", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "shed: unknown command %q\n\n%s", cmd, usage)
		code = exitError
	}
	os.Exit(code)
}

// loadConfig parses the flags of a subcommand and loads the fleet file.
func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, string, error) {
	path := fs.String("config", "", "path to the fleet file")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	resolved, err := config.Resolve(*path)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(resolved)
	return cfg, resolved, err
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "shed: %v\n", err)
	return exitError
}

func cmdValidate(args []string) int {
	cfg, path, err := loadConfig(flag.NewFlagSet("validate", flag.ContinueOnError), args)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("%s: ok, %d machine(s)\n", path, len(cfg.Machines))
	return exitOK
}
