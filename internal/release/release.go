// Package release fetches published shed binaries, so a machine needs no Go
// toolchain to install, update or deploy shed.
package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Repo is where releases are published.
const Repo = "EdisonCode/ai-shed"

const checksumsFile = "checksums.txt"

var tagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// IsRelease reports whether a version is a published release, not a build
// from a working tree.
func IsRelease(version string) bool {
	return tagRE.MatchString(version)
}

// LocalPlatform is this machine's platform, as release binaries name it.
func LocalPlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Source is where release files come from.
type Source interface {
	// Latest returns the tag of the newest release.
	Latest(ctx context.Context) (string, error)
	// Download puts the named files of a release into dir.
	Download(ctx context.Context, tag, dir string, files ...string) error
}

// GH reads releases with the GitHub CLI.
type GH struct{}

func (GH) Latest(ctx context.Context) (string, error) {
	out, err := gh(ctx, "release", "view", "--repo", Repo, "--json", "tagName", "--jq", ".tagName")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (GH) Download(ctx context.Context, tag, dir string, files ...string) error {
	args := []string{"release", "download", tag, "--repo", Repo, "--dir", dir, "--clobber"}
	for _, f := range files {
		args = append(args, "--pattern", f)
	}
	_, err := gh(ctx, args...)
	return err
}

func gh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh %s %s: %w: %s", args[0], args[1], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// CacheDir is where downloaded release binaries are kept.
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find cache directory: %w", err)
	}
	return filepath.Join(dir, "shed"), nil
}

// Binary returns the path of a release's binary for a platform, downloading
// it into cacheDir if it is not there. The file always matches the release's
// published checksum.
func Binary(ctx context.Context, src Source, cacheDir, tag, platform string) (string, error) {
	dir := filepath.Join(cacheDir, tag)
	name := "shed-" + platform
	path, sums := filepath.Join(dir, name), filepath.Join(dir, checksumsFile)
	if verify(path, sums) == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	if err := src.Download(ctx, tag, dir, name, checksumsFile); err != nil {
		return "", err
	}
	if err := verify(path, sums); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("release %s: %w", tag, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		return "", fmt.Errorf("make binary executable: %w", err)
	}
	return path, nil
}

// verify checks a file against its line in a checksums file.
func verify(path, sumsPath string) error {
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == filepath.Base(path) {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("%s is not in the checksums file", filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("read binary: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s does not match its published checksum", filepath.Base(path))
	}
	return nil
}

// Replace puts the new binary where the old one is. The new file is written
// beside the old and moved into place, so a running shed keeps working.
func Replace(target, newBinary string) error {
	data, err := os.ReadFile(newBinary)
	if err != nil {
		return fmt.Errorf("read new binary: %w", err)
	}
	tmp := target + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("write %s (is the directory yours to write?): %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", target, err)
	}
	return nil
}
