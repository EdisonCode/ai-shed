// Package deploy puts the shed binary and the fleet file on a machine.
package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
)

// Paths on the machine, under the user's home.
const (
	RemoteBinary  = ".local/bin/shed"
	RemoteConfig  = ".config/shed/shed.yaml"
	RemoteMachine = ".config/shed/machine"
)

var platforms = map[string]string{
	"Linux x86_64":  "linux-amd64",
	"Linux aarch64": "linux-arm64",
	"Darwin arm64":  "darwin-arm64",
	"Darwin x86_64": "darwin-amd64",
}

// Platform maps the output of `uname -sm` to the suffix of a dist binary.
func Platform(uname string) (string, error) {
	p, ok := platforms[strings.TrimSpace(uname)]
	if !ok {
		return "", fmt.Errorf("unsupported platform %q", strings.TrimSpace(uname))
	}
	return p, nil
}

// Machine copies the matching binary from distDir, the fleet file and the
// machine's name to the machine. Each file is written beside its target and
// moved into place, so a running agent never sees half a file.
func Machine(ctx context.Context, r probe.Runner, m config.Machine, distDir, configPath string) error {
	uname, err := r.Run(ctx, m, "uname -sm", nil)
	if err != nil {
		return err
	}
	platform, err := Platform(string(uname))
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(filepath.Join(distDir, "shed-"+platform))
	if err != nil {
		return fmt.Errorf("read binary (run `make dist` first): %w", err)
	}
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	files := []struct {
		path string
		data []byte
		mode string
	}{
		{RemoteBinary, binary, "755"},
		{RemoteConfig, cfg, "644"},
		{RemoteMachine, []byte(m.Name + "\n"), "644"},
	}
	for _, f := range files {
		cmd := fmt.Sprintf(`f="$HOME/%s" && mkdir -p "$(dirname "$f")" && cat > "$f.new" && chmod %s "$f.new" && mv "$f.new" "$f"`, f.path, f.mode)
		if _, err := r.Run(ctx, m, cmd, bytes.NewReader(f.data)); err != nil {
			return fmt.Errorf("write ~/%s: %w", f.path, err)
		}
	}
	return nil
}
