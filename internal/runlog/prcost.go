package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PRCostDir holds, per worker, the last cost shed posted on each pull
// request, for the usage report to total.
const PRCostDir = "cost"

// PRCost is what the work on one issue cost, as posted on its pull request.
type PRCost struct {
	Repo    string    `json:"repo"`
	PR      int       `json:"pr"`
	Issue   int       `json:"issue"`
	Worker  string    `json:"worker"`
	Updated time.Time `json:"updated"`
	// Models holds the tokens by model, as the comment names it.
	Models map[string]ModelTokens `json:"models"`
	// Dollars is the cost at API rates of the models that have one, and
	// Unpriced names the models that have none.
	Dollars  float64  `json:"dollars"`
	Unpriced []string `json:"unpriced,omitempty"`
}

// ModelTokens are one model's tokens and their cost at API rates.
type ModelTokens struct {
	Input        int     `json:"input"`
	CacheWrite   int     `json:"cache_write"`
	CacheWrite1h int     `json:"cache_write_1h,omitempty"`
	CacheRead    int     `json:"cache_read"`
	Output       int     `json:"output"`
	Dollars      float64 `json:"dollars"`
	Priced       bool    `json:"priced"`
}

// Add counts a call into the model's tokens.
func (t *ModelTokens) Add(u Usage) {
	t.Input += u.Input
	t.CacheWrite += u.CacheWrite
	t.CacheWrite1h += u.CacheWrite1h
	t.CacheRead += u.CacheRead
	t.Output += u.Output
}

// Billed is every token the model was billed for, of any kind.
func (t ModelTokens) Billed() int {
	return t.Input + t.CacheWrite + t.CacheRead + t.Output
}

func prCostPath(dir, worker string) string {
	return filepath.Join(dir, PRCostDir, worker+".prs.json")
}

// RecordPRCost keeps the cost of a pull request, replacing what was kept
// for it before.
func RecordPRCost(dir string, c PRCost) error {
	path := prCostPath(dir, c.Worker)
	costs, err := readPRCostFile(path)
	if err != nil {
		return err
	}
	if costs == nil {
		costs = map[string]PRCost{}
	}
	costs[fmt.Sprintf("%s#%d", c.Repo, c.PR)] = c
	data, err := json.MarshalIndent(costs, "", "  ")
	if err != nil {
		return fmt.Errorf("write pull request cost: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write pull request cost: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write pull request cost: %w", err)
	}
	return nil
}

// ReadPRCosts returns every worker's pull request costs, by repository and
// number.
func ReadPRCosts(dir string) ([]PRCost, error) {
	paths, err := filepath.Glob(filepath.Join(dir, PRCostDir, "*.prs.json"))
	if err != nil {
		return nil, fmt.Errorf("read pull request costs: %w", err)
	}
	var out []PRCost
	for _, path := range paths {
		costs, err := readPRCostFile(path)
		if err != nil {
			return nil, err
		}
		for _, c := range costs {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].PR < out[j].PR
	})
	return out, nil
}

func readPRCostFile(path string) (map[string]PRCost, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pull request costs: %w", err)
	}
	var costs map[string]PRCost
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	if err := json.Unmarshal(data, &costs); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return costs, nil
}
