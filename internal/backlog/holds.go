package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Dep is an issue that must close before another one is worked.
type Dep struct {
	Repo   string
	Number int
	// Open is set while the issue is open, as the lister read it.
	Open bool
}

func (d Dep) key() string { return d.Repo + "#" + strconv.Itoa(d.Number) }

// depRE finds "Depends on #12", "blocked by: org/api#7, #9" and the like.
var (
	depRE = regexp.MustCompile(`(?i)\b(?:depends on|blocked by)\s*:?\s*((?:[\w.-]+/[\w.-]+)?#\d+(?:\s*(?:,|and|&)\s*(?:[\w.-]+/[\w.-]+)?#\d+)*)`)
	refRE = regexp.MustCompile(`(?:([\w.-]+/[\w.-]+))?#(\d+)`)
)

// DependencyRefs returns the issues the issue's body and comments say it
// depends on, in the order they are named. Their state is not known.
func DependencyRefs(i Issue) []Dep {
	texts := []string{i.Body}
	for _, c := range i.Comments {
		texts = append(texts, c.Body)
	}
	var deps []Dep
	for _, text := range texts {
		for _, m := range depRE.FindAllStringSubmatch(text, -1) {
			for _, ref := range refRE.FindAllStringSubmatch(m[1], -1) {
				n, _ := strconv.Atoi(ref[2])
				d := Dep{Repo: i.Repo, Number: n}
				if ref[1] != "" {
					d.Repo = ref[1]
				}
				if !slices.Contains(deps, d) && !(d.Repo == i.Repo && d.Number == i.Number) {
					deps = append(deps, d)
				}
			}
		}
	}
	return deps
}

// OpenDeps returns the issue's dependencies that are still open.
func OpenDeps(i Issue) []Dep {
	var open []Dep
	for _, d := range i.Deps {
		if d.Open {
			open = append(open, d)
		}
	}
	return open
}

// DepWords names dependencies: #12 in the issue's own repository, and
// org/api#7 in another.
func DepWords(deps []Dep, repo string) string {
	words := make([]string, len(deps))
	for n, d := range deps {
		words[n] = fmt.Sprintf("#%d", d.Number)
		if d.Repo != repo {
			words[n] = d.key()
		}
	}
	return strings.Join(words, ", ")
}

var pathsRE = regexp.MustCompile(`(?im)^\s*(?:\*\*)?paths:(?:\*\*)?\s*(.+)$`)

// Paths returns the files and directories the issue says it touches: the
// last "Paths:" line of its body and comments, as a worker writes it with
// its plan.
func Paths(i Issue) []string {
	texts := []string{i.Body}
	for _, c := range i.Comments {
		texts = append(texts, c.Body)
	}
	var line string
	for _, text := range texts {
		if all := pathsRE.FindAllStringSubmatch(text, -1); len(all) > 0 {
			line = all[len(all)-1][1]
		}
	}
	var paths []string
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		p := strings.TrimPrefix(strings.Trim(f, "`*"), "./")
		if p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// Overlap says why two issues would touch the same code, or "" when they
// would not: a shared label that starts with labelPrefix (an area of the
// code, a module, a bounded context), or a path of one inside a path of the
// other.
func Overlap(a, b Issue, labelPrefix string) string {
	if labelPrefix != "" {
		for _, l := range a.LabelNames() {
			if strings.HasPrefix(l, labelPrefix) && slices.Contains(b.LabelNames(), l) {
				return "both are " + l
			}
		}
	}
	for _, p := range Paths(a) {
		for _, q := range Paths(b) {
			if within(p, q) {
				return "both touch " + shorter(p, q)
			}
		}
	}
	return ""
}

// within reports whether one path is the other or inside it, at a path
// segment: internal/billing holds internal/billing/a.go, not
// internal/billingx.
func within(p, q string) bool {
	p, q = strings.TrimSuffix(p, "/"), strings.TrimSuffix(q, "/")
	return p == q || strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/")
}

func shorter(p, q string) string {
	if len(strings.TrimSuffix(q, "/")) < len(strings.TrimSuffix(p, "/")) {
		return q
	}
	return p
}

// resolveDeps fills in each issue's dependencies with one GraphQL call: the
// blocked-by relation of every listed issue, and the state of every issue
// their text names.
func resolveDeps(ctx context.Context, repo string, issues []Issue) error {
	if len(issues) == 0 {
		return nil
	}
	repos := []string{repo}
	refs := map[string][]int{}
	for _, i := range issues {
		for _, d := range DependencyRefs(i) {
			if !slices.Contains(repos, d.Repo) {
				repos = append(repos, d.Repo)
			}
			if !slices.Contains(refs[d.Repo], d.Number) {
				refs[d.Repo] = append(refs[d.Repo], d.Number)
			}
		}
	}
	var q strings.Builder
	q.WriteString("query {\n")
	for n, r := range repos {
		owner, name, _ := strings.Cut(r, "/")
		fmt.Fprintf(&q, "r%d: repository(owner: %q, name: %q) {\n", n, owner, name)
		if n == 0 {
			for _, i := range issues {
				fmt.Fprintf(&q, "i%d: issue(number: %d) { blockedBy(first: 20) { nodes { number state repository { nameWithOwner } } } }\n", i.Number, i.Number)
			}
		}
		for _, number := range refs[r] {
			fmt.Fprintf(&q, "d%d: issue(number: %d) { state }\n", number, number)
		}
		q.WriteString("}\n")
	}
	q.WriteString("}")
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+q.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("gh api graphql %s dependencies: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	native, state, err := parseDeps(out, repos)
	if err != nil {
		return fmt.Errorf("%s dependencies: %w", repo, err)
	}
	for n := range issues {
		deps := slices.Clone(native[issues[n].Number])
		for _, d := range DependencyRefs(issues[n]) {
			d.Open = state[d.key()]
			if !slices.ContainsFunc(deps, func(x Dep) bool { return x.key() == d.key() }) {
				deps = append(deps, d)
			}
		}
		issues[n].Deps = deps
		if issues[n].Deps == nil {
			issues[n].Deps = []Dep{}
		}
	}
	return nil
}

// parseDeps reads the answer to resolveDeps' query: the blocked-by issues of
// each listed issue of the first repository, by number, and whether each
// named issue is open, by repo#number. An issue GitHub could not return is
// left out, and counts as closed.
func parseDeps(data []byte, repos []string) (map[int][]Dep, map[string]bool, error) {
	var answer struct {
		Data map[string]map[string]*struct {
			State     string `json:"state"`
			BlockedBy struct {
				Nodes []struct {
					Number     int    `json:"number"`
					State      string `json:"state"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"nodes"`
			} `json:"blockedBy"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return nil, nil, fmt.Errorf("parse output: %w", err)
	}
	native, state := map[int][]Dep{}, map[string]bool{}
	for alias, fields := range answer.Data {
		idx, err := strconv.Atoi(strings.TrimPrefix(alias, "r"))
		if err != nil || idx < 0 || idx >= len(repos) {
			continue
		}
		for field, issue := range fields {
			if issue == nil || len(field) < 2 {
				continue
			}
			number, err := strconv.Atoi(field[1:])
			if err != nil {
				continue
			}
			switch field[0] {
			case 'i':
				for _, b := range issue.BlockedBy.Nodes {
					native[number] = append(native[number], Dep{Repo: b.Repository.NameWithOwner, Number: b.Number, Open: b.State == "OPEN"})
				}
			case 'd':
				state[fmt.Sprintf("%s#%d", repos[idx], number)] = issue.State == "OPEN"
			}
		}
	}
	return native, state, nil
}
