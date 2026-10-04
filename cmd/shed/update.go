package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edisoncode/ai-shed/internal/release"
)

// cmdUpdate replaces this shed with the latest release. Worker machines are
// not touched: `shed deploy` brings them to the same version.
func cmdUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	ctx := context.Background()
	src := release.GH{}
	latest, err := src.Latest(ctx)
	if err != nil {
		return fail(err)
	}
	if latest == version {
		fmt.Printf("shed %s is the latest release\n", version)
		return exitOK
	}
	cache, err := release.CacheDir()
	if err != nil {
		return fail(err)
	}
	binary, err := release.Binary(ctx, src, cache, latest, release.LocalPlatform())
	if err != nil {
		return fail(err)
	}
	self, err := os.Executable()
	if err != nil {
		return fail(fmt.Errorf("find this program: %w", err))
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return fail(fmt.Errorf("find this program: %w", err))
	}
	if err := release.Replace(self, binary); err != nil {
		return fail(err)
	}
	fmt.Printf("shed %s -> %s (%s)\nRun `shed deploy` to bring the machines to the same version.\n", version, latest, self)
	return exitOK
}
