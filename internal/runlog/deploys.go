package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeploysFile records the staging deploys the agent started.
const DeploysFile = "deploys.jsonl"

// What became of a staging deploy.
const (
	DeployStarted   = "started"    // the deploy command ran; staging is not yet serving a new commit
	DeployServed    = "served"     // staging serves a new commit
	DeployFailed    = "failed"     // the deploy command failed
	DeployNotServed = "not served" // staging still served the old commit when the wait ran out
)

// Deploy is one line of deploys.jsonl. A deploy that runs writes a started
// line and later a served or not-served line.
type Deploy struct {
	Repo  string    `json:"repo"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	// Before is the commit staging served when the deploy started, After
	// the one it served when it ended.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Waiting is how many looks waited for the deploy.
	Waiting  int    `json:"waiting,omitempty"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
	Result   string `json:"result"`
}

func AppendDeploy(dir string, d Deploy) error {
	return appendJSON(dir, DeploysFile, d)
}

func ParseDeploy(line string) (Deploy, error) {
	var d Deploy
	if err := json.Unmarshal([]byte(line), &d); err != nil {
		return Deploy{}, fmt.Errorf("parse deploy record: %w", err)
	}
	return d, nil
}

// ReadDeploys returns the deploy records in file order.
func ReadDeploys(dir string) ([]Deploy, error) {
	data, err := os.ReadFile(filepath.Join(dir, DeploysFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read deploys: %w", err)
	}
	var out []Deploy
	for _, line := range strings.Split(string(data), "\n") {
		if d, err := ParseDeploy(line); err == nil {
			out = append(out, d)
		}
	}
	return out, nil
}
