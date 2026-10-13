// Package risk tiers a pull request for verification by what it changes,
// with rules that need no model: the parts of the code it touches, its size,
// and whether its tests changed with its code. A low-risk change gets a
// cheap check; a high-risk one gets a strong model told to break it.
package risk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Diff is what a pull request changes.
type Diff struct {
	Files []string
	// Lines is the lines added and deleted.
	Lines int
}

// Tier rates a pull request high or low, with the reasons that made it high.
func Tier(v config.Verify, d Diff) (string, []string) {
	var reasons []string
	for _, a := range v.Areas {
		if touches(d.Files, a.Paths) {
			reasons = append(reasons, "touches "+a.Name)
		}
	}
	code, tests := false, false
	for _, f := range d.Files {
		switch {
		case isTest(f):
			tests = true
		case !isProse(f):
			code = true
		}
	}
	// A long change to the documentation alone is not risky.
	if code && d.Lines > v.HighLinesOrDefault() {
		reasons = append(reasons, fmt.Sprintf("%d lines changed", d.Lines))
	}
	if code && !tests {
		reasons = append(reasons, "code changed and no test did")
	}
	if len(reasons) > 0 {
		return config.TierHigh, reasons
	}
	return config.TierLow, nil
}

// touches reports whether a file is one of the paths or inside one, at a
// path segment.
func touches(files, paths []string) bool {
	for _, f := range files {
		for _, p := range paths {
			p = strings.TrimSuffix(p, "/")
			if f == p || strings.HasPrefix(f, p+"/") {
				return true
			}
		}
	}
	return false
}

// isTest reports whether a file is a test, by the names test files have in
// most languages.
func isTest(f string) bool {
	base := path.Base(f)
	return strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") || strings.HasPrefix(f, "test/") || strings.HasPrefix(f, "tests/") ||
		strings.Contains(f, "/test/") || strings.Contains(f, "/tests/") || strings.Contains(f, "/__tests__/") || strings.Contains(f, "/spec/")
}

// isProse reports whether a file is documentation, which needs no test.
func isProse(f string) bool {
	switch strings.ToLower(path.Ext(f)) {
	case ".md", ".txt", ".rst", ".adoc":
		return true
	}
	return strings.HasPrefix(f, "docs/")
}

// GH reads a pull request's diff with the GitHub CLI.
type GH struct{}

func (GH) Diff(ctx context.Context, repo string, pr int) (Diff, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "files,additions,deletions")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Diff{}, fmt.Errorf("gh pr view %s#%d: %w: %s", repo, pr, err, strings.TrimSpace(stderr.String()))
	}
	return parseDiff(out)
}

func parseDiff(data []byte) (Diff, error) {
	var pr struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
		Files     []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &pr); err != nil {
		return Diff{}, fmt.Errorf("gh pr view: parse output: %w", err)
	}
	d := Diff{Lines: pr.Additions + pr.Deletions}
	for _, f := range pr.Files {
		d.Files = append(d.Files, f.Path)
	}
	return d, nil
}
