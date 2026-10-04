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
  shed validate                     check the fleet file
  shed deploy [machine...]          copy the binary and the fleet file to machines
  shed agent [-machine name]        on a machine: supervise its workers and run its tasks
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
