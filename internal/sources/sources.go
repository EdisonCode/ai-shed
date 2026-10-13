// Package sources audits what a worker's tool reads before its first item:
// the instruction files, the memory index and the brief. Every turn of a
// long session reads them again, so a rule written twice costs twice and
// two copies drift apart. The audit counts the tokens of each source, finds
// paragraphs that say the same thing in two sources, and finds references
// that went stale. It changes nothing.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Source is one file the tool loads at session start.
type Source struct {
	Path string `json:"path"`
	// Kind is global, parent, repository, import, memory or brief.
	Kind   string `json:"kind"`
	Tokens int    `json:"tokens"`
	Text   string `json:"-"`
}

// Location is a line of a source.
type Location struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Duplicate is a paragraph that two sources both carry, nearly word for
// word.
type Duplicate struct {
	A, B       Location
	Similarity float64 `json:"similarity"`
	Excerpt    string  `json:"excerpt"`
}

// Stale is a reference that no longer holds.
type Stale struct {
	Location
	Ref string `json:"ref"`
	Why string `json:"why"`
}

// Contradiction is two statements that a model read as saying opposite
// things. It is for the owner to settle; shed resolves nothing.
type Contradiction struct {
	A   string `json:"a"`
	B   string `json:"b"`
	Why string `json:"why"`
}

// Audit is what a worker loads and what is wrong with it.
type Audit struct {
	Dir            string          `json:"dir"`
	Sources        []Source        `json:"sources"`
	Total          int             `json:"total"`
	Duplicates     []Duplicate     `json:"duplicates,omitempty"`
	Stale          []Stale         `json:"stale,omitempty"`
	Contradictions []Contradiction `json:"contradictions,omitempty"`
	Problems       []string        `json:"problems,omitempty"`
}

// instructionFiles are the files Claude Code reads in each directory from
// the working directory up to the root.
var instructionFiles = []string{"CLAUDE.md", "CLAUDE.local.md", filepath.Join(".claude", "CLAUDE.md"), "AGENTS.md"}

// maxImportDepth bounds how deep @imports are followed, as the tool does.
const maxImportDepth = 5

var (
	importRE = regexp.MustCompile(`(?m)^@(\S+)\s*$`)
	slugRE   = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// MemorySlug is the name of a directory's folder under ~/.claude/projects.
func MemorySlug(dir string) string {
	return slugRE.ReplaceAllString(dir, "-")
}

// Collect lists the sources a session started in dir loads, in the order the
// tool reads them: the global instructions, those of each parent directory
// from the root down, the repository's, each followed by what it imports,
// the memory index, and the worker's brief.
func Collect(dir, home string) []Source {
	var found []Source
	seen := map[string]bool{}
	var add func(path, kind string, depth int)
	add = func(path, kind string, depth int) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		seen[path] = true
		text := string(data)
		found = append(found, Source{Path: path, Kind: kind, Tokens: tokens(text), Text: text})
		if depth >= maxImportDepth {
			return
		}
		for _, m := range importRE.FindAllStringSubmatch(text, -1) {
			target := m[1]
			switch {
			case strings.HasPrefix(target, "~/"):
				target = filepath.Join(home, target[2:])
			case !filepath.IsAbs(target):
				target = filepath.Join(filepath.Dir(path), target)
			}
			add(target, "import", depth+1)
		}
	}
	add(filepath.Join(home, ".claude", "CLAUDE.md"), "global", 0)
	var parents []string
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		parents = append([]string{d}, parents...)
		if d == filepath.Dir(d) {
			break
		}
	}
	for _, d := range parents {
		for _, name := range instructionFiles {
			add(filepath.Join(d, name), "parent", 0)
		}
	}
	for _, name := range instructionFiles {
		add(filepath.Join(dir, name), "repository", 0)
	}
	add(filepath.Join(home, ".claude", "projects", MemorySlug(dir), "memory", "MEMORY.md"), "memory", 0)
	add(filepath.Join(dir, ".shed", "BRIEF.md"), "brief", 0)
	return found
}

// tokens estimates a text's tokens at four characters each.
func tokens(text string) int {
	return (len(text) + 3) / 4
}

// IssueOpen reports whether an issue of the worker's repository is open.
type IssueOpen func(ctx context.Context, number int) (bool, error)

// Run audits the sources a session in dir loads. With issueOpen, a
// reference to an issue that the text calls open is checked.
func Run(ctx context.Context, dir, home string, issueOpen IssueOpen) Audit {
	a := Audit{Dir: dir, Sources: Collect(dir, home)}
	for _, s := range a.Sources {
		a.Total += s.Tokens
	}
	a.Duplicates = duplicates(a.Sources)
	a.Stale = stale(ctx, dir, a.Sources, issueOpen, &a.Problems)
	return a
}

// paragraph is a block of a source between empty lines.
type paragraph struct {
	at       Location
	text     string
	shingles map[string]bool
}

// A paragraph is compared from minWords words, and a single statement (a
// list item, a line) from minStatement: a heading says too little to be a
// copy. Two units are alike when their word triples overlap by
// minSimilarity, or when the shorter is minContained inside the longer: the
// same rule, rewritten in a few more words.
const (
	minWords      = 12
	minStatement  = 6
	minSimilarity = 0.5
	minContained  = 0.7
)

// listMarkRE is the mark of a list item or a heading.
var listMarkRE = regexp.MustCompile(`^\s*([-*+>#]+|\d+[.)])\s+`)

// units splits a source into what is compared: its paragraphs, and each of
// its lines that is a statement of its own.
func units(s Source) []paragraph {
	out := paragraphs(s)
	for n, line := range strings.Split(s.Text, "\n") {
		text := strings.TrimSpace(listMarkRE.ReplaceAllString(line, ""))
		words := normalWords(text)
		if len(words) >= minStatement && len(words) < minWords*2 {
			out = append(out, paragraph{at: Location{Path: s.Path, Line: n + 1}, text: text, shingles: shingles(words)})
		}
	}
	return out
}

func paragraphs(s Source) []paragraph {
	var out []paragraph
	lines := strings.Split(s.Text, "\n")
	start := 0
	flush := func(end int) {
		text := strings.TrimSpace(strings.Join(lines[start:end], " "))
		words := normalWords(text)
		if len(words) >= minWords {
			out = append(out, paragraph{at: Location{Path: s.Path, Line: start + 1}, text: text, shingles: shingles(words)})
		}
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush(i)
			start = i + 1
		}
	}
	flush(len(lines))
	return out
}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

func normalWords(text string) []string {
	return wordRE.FindAllString(strings.ToLower(text), -1)
}

func shingles(words []string) map[string]bool {
	set := map[string]bool{}
	for i := 0; i+3 <= len(words); i++ {
		set[strings.Join(words[i:i+3], " ")] = true
	}
	return set
}

// alike says how much two units say the same thing: the overlap of their
// word triples, or how much of the shorter the longer contains when that
// is high enough on its own.
func alike(a, b map[string]bool) float64 {
	shared := 0
	for k := range a {
		if b[k] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 || shared == 0 {
		return 0
	}
	jaccard := float64(shared) / float64(union)
	contained := float64(shared) / float64(min(len(a), len(b)))
	switch {
	case jaccard >= minSimilarity:
		return jaccard
	case contained >= minContained:
		return contained
	}
	return 0
}

// duplicates finds what two sources both say, once per pair of lines.
func duplicates(sources []Source) []Duplicate {
	var all [][]paragraph
	for _, s := range sources {
		all = append(all, units(s))
	}
	var out []Duplicate
	seen := map[[2]Location]bool{}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			for _, p := range all[i] {
				for _, q := range all[j] {
					key := [2]Location{p.at, q.at}
					if seen[key] {
						continue
					}
					if sim := alike(p.shingles, q.shingles); sim > 0 {
						seen[key] = true
						out = append(out, Duplicate{A: p.at, B: q.at, Similarity: sim, Excerpt: excerpt(p.text)})
					}
				}
			}
		}
	}
	return out
}

func excerpt(text string) string {
	if len(text) <= 80 {
		return text
	}
	return text[:79] + "…"
}

var (
	pathRE  = regexp.MustCompile("`([^`\\s]+)`")
	issueRE = regexp.MustCompile(`(?:^|[\s(])#(\d+)\b`)
	openRE  = regexp.MustCompile(`(?i)\bopen\b`)
)

// projectKinds are the sources about this repository; the global and parent
// instructions span other repositories, and their paths are theirs.
var projectKinds = map[string]bool{"repository": true, "import": true, "memory": true}

// stale finds the paths a source names that the checkout does not have, and
// the issues it calls open that are closed.
func stale(ctx context.Context, dir string, sources []Source, issueOpen IssueOpen, problems *[]string) []Stale {
	var out []Stale
	asked := map[int]bool{}
	for _, s := range sources {
		if !projectKinds[s.Kind] {
			continue
		}
		for n, line := range strings.Split(s.Text, "\n") {
			at := Location{Path: s.Path, Line: n + 1}
			for _, m := range pathRE.FindAllStringSubmatch(line, -1) {
				if ref := m[1]; looksLikePath(ref) {
					if _, err := os.Stat(filepath.Join(dir, ref)); err != nil {
						out = append(out, Stale{Location: at, Ref: ref, Why: "no such path in the checkout"})
					}
				}
			}
			if issueOpen == nil || !openRE.MatchString(line) {
				continue
			}
			for _, m := range issueRE.FindAllStringSubmatch(line, -1) {
				number, _ := strconv.Atoi(m[1])
				if asked[number] {
					continue
				}
				asked[number] = true
				open, err := issueOpen(ctx, number)
				switch {
				case err != nil:
					*problems = append(*problems, fmt.Sprintf("#%d: %v", number, err))
				case !open:
					out = append(out, Stale{Location: at, Ref: "#" + m[1], Why: "closed, and the text says it is open"})
				}
			}
		}
	}
	return out
}

// looksLikePath reports whether a backticked word names a file or directory
// of the repository: it has a slash and is not a URL, an absolute or home
// path, a placeholder, a pattern or a command.
func looksLikePath(ref string) bool {
	if !strings.Contains(ref, "/") || strings.Contains(ref, "://") {
		return false
	}
	if strings.ContainsAny(ref[:1], "/~$.-") || strings.ContainsAny(ref, "<>{}*?|=:@") {
		return false
	}
	return true
}

// ContradictionPrompt asks a model for the statements of the sources that
// contradict each other.
func ContradictionPrompt(sources []Source) string {
	var b strings.Builder
	b.WriteString(`Below are the instruction files an AI coding agent reads before it starts work. Find the pairs of statements that contradict each other: one asks for what the other forbids, or they give two different answers to the same question. Ignore statements that only overlap or repeat each other.

Answer with a JSON array and nothing else: [{"a": "the first statement, quoted exactly", "b": "the second statement, quoted exactly", "why": "one sentence"}]. Answer [] when there are none.
`)
	for _, s := range sources {
		fmt.Fprintf(&b, "\n<source path=%q>\n%s\n</source>\n", s.Path, s.Text)
	}
	return b.String()
}

// parseContradictions reads the model's answer: the JSON array in it.
func parseContradictions(answer string) ([]Contradiction, error) {
	start, end := strings.Index(answer, "["), strings.LastIndex(answer, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("the answer has no JSON array: %q", excerpt(answer))
	}
	var out []Contradiction
	if err := json.Unmarshal([]byte(answer[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("the answer is not a list of contradictions: %w", err)
	}
	return out, nil
}

// Contradictions asks a model, through ask, which statements of the
// sources contradict each other.
func Contradictions(ctx context.Context, sources []Source, ask func(ctx context.Context, prompt string) (string, error)) ([]Contradiction, error) {
	answer, err := ask(ctx, ContradictionPrompt(sources))
	if err != nil {
		return nil, err
	}
	return parseContradictions(answer)
}
