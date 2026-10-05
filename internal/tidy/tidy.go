// Package tidy finds the worktrees and branches on a worker machine whose
// work is merged, and removes them when asked. Workers start each issue on a
// new branch and their tools make worktrees for sub-tasks; nothing removes
// either when the pull request merges.
package tidy

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Repo is one clone on the machine with its worktrees and local branches.
type Repo struct {
	// Dir is the clone's git directory, which all its worktrees share.
	Dir string
	// Default is the remote's default branch, as in "origin/main".
	Default string
	// Name is owner/name on GitHub; empty when the remote is not GitHub's.
	Name      string
	Worktrees []Worktree
	Branches  []Branch
}

type Worktree struct {
	Path string
	Head string
	// Branch is empty for a detached worktree.
	Branch string
	// Dirty counts the files that are changed or untracked.
	Dirty int
	// Main is the clone itself; Worker is a worker's own directory.
	Main, Worker bool
}

type Branch struct {
	Name string
	Head string
}

// Item is one worktree or branch with what tidy decided about it.
type Item struct {
	Repo *Repo
	// Path is set for a worktree; Branch is its branch, or the branch itself.
	Path   string
	Branch string
	Head   string
	// Remove says the item goes. Why says what it is merged in, or why it
	// stays.
	Remove bool
	Why    string
}

// Merged finds the merged pull request whose head is the commit.
type Merged interface {
	// MergedPR returns the number of a merged pull request whose last commit
	// is this one; 0 when there is none.
	MergedPR(ctx context.Context, repo, commit string) (int, error)
}

// collect prints, for each clone behind the worker directories, its
// worktrees and branches. The directories arrive as arguments.
const collect = `seen="|"
for d in "$@"; do
  case "$d" in "~/"*) d="$HOME/${d#\~/}";; esac
  [ -d "$d" ] || continue
  echo "@@workerdir $(cd "$d" && pwd -P)"
  common=$(cd "$d" && cd "$(git rev-parse --git-common-dir 2>/dev/null)" 2>/dev/null && pwd -P) || continue
  case "$seen" in *"|$common|"*) continue;; esac
  seen="$seen$common|"
  git -C "$common" fetch --quiet origin 2>/dev/null
  def=$(git -C "$common" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || echo origin/main)
  echo "@@repo $def|$(git -C "$common" remote get-url origin 2>/dev/null)|$common"
  main=1
  git -C "$common" worktree list --porcelain | sed -n 's/^worktree //p' | while IFS= read -r p; do
    sha=$(git -C "$p" rev-parse HEAD 2>/dev/null)
    if [ -n "$sha" ]; then
      branch=$(git -C "$p" symbolic-ref --quiet --short HEAD 2>/dev/null || echo "")
      dirty=$(git -C "$p" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
      echo "@@worktree $sha|$dirty|$main|$branch|$(cd "$p" && pwd -P)"
    fi
    main=0
  done
  git -C "$common" for-each-ref --format='@@branch %(objectname)|%(refname:short)' refs/heads
done
echo "@@end"
`

// CollectCommand is the shell command that runs the collect script, which
// goes in on its standard input, for these workers' directories.
func CollectCommand(m config.Machine) string {
	args := []string{"sh", "-s", "--"}
	for _, w := range m.Workers {
		args = append(args, quote(w.Dir))
	}
	return strings.Join(args, " ")
}

// CollectScript is the script CollectCommand reads.
func CollectScript() string { return collect }

var remoteRE = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(\.git)?$`)

// Parse reads the collect script's output. Output without the end marker
// was cut short and is an error: a plan must not be made from half a list.
func Parse(out string) ([]*Repo, error) {
	var repos []*Repo
	workerDirs := map[string]bool{}
	complete := false
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(line, " ")
		switch tag {
		case "@@workerdir":
			workerDirs[rest] = true
		case "@@repo":
			f := strings.SplitN(rest, "|", 3)
			if len(f) != 3 {
				return nil, fmt.Errorf("tidy: unreadable line %q", line)
			}
			repo := &Repo{Default: f[0], Dir: f[2]}
			if m := remoteRE.FindStringSubmatch(f[1]); m != nil {
				repo.Name = m[1]
			}
			repos = append(repos, repo)
		case "@@worktree":
			f := strings.SplitN(rest, "|", 5)
			if len(f) != 5 || len(repos) == 0 {
				return nil, fmt.Errorf("tidy: unreadable line %q", line)
			}
			w := Worktree{Head: f[0], Main: f[2] == "1", Branch: f[3], Path: f[4]}
			if _, err := fmt.Sscanf(f[1], "%d", &w.Dirty); err != nil {
				return nil, fmt.Errorf("tidy: unreadable line %q", line)
			}
			repo := repos[len(repos)-1]
			repo.Worktrees = append(repo.Worktrees, w)
		case "@@branch":
			f := strings.SplitN(rest, "|", 2)
			if len(f) != 2 || len(repos) == 0 {
				return nil, fmt.Errorf("tidy: unreadable line %q", line)
			}
			repo := repos[len(repos)-1]
			repo.Branches = append(repo.Branches, Branch{Head: f[0], Name: f[1]})
		case "@@end":
			complete = true
		}
	}
	if !complete {
		return nil, fmt.Errorf("tidy: the machine's list is incomplete (%d bytes)", len(out))
	}
	// A worker's directory may be a worktree of a clone listed under another
	// worker, so the directories are matched after everything is read.
	for _, repo := range repos {
		for i := range repo.Worktrees {
			repo.Worktrees[i].Worker = workerDirs[repo.Worktrees[i].Path]
		}
	}
	return repos, nil
}

// Plan decides about every worktree and branch. A worktree goes when it has
// a branch, nothing uncommitted, and a head that is the last commit of a
// merged pull request. A branch goes when its head is such a commit and no
// worktree that stays has it checked out. The clone itself, a worker's
// directory and a detached worktree always stay.
//
// "Merged" is not read from the default branch's history. A squash merge
// leaves no trace there, and a branch that was only just made, with no
// commit of its own yet, would look merged.
func Plan(ctx context.Context, repos []*Repo, gh Merged) ([]Item, error) {
	var items []Item
	for _, repo := range repos {
		merged := map[string]string{}
		// mergedIn says what a commit is merged in, or "" when it is not.
		mergedIn := func(head string) (string, error) {
			if why, known := merged[head]; known {
				return why, nil
			}
			if repo.Name == "" {
				return "", nil
			}
			pr, err := gh.MergedPR(ctx, repo.Name, head)
			if err != nil {
				return "", err
			}
			if pr != 0 {
				merged[head] = fmt.Sprintf("merged in #%d", pr)
			} else {
				merged[head] = ""
			}
			return merged[head], nil
		}

		checkedOut := map[string]bool{}
		for _, w := range repo.Worktrees {
			item := Item{Repo: repo, Path: w.Path, Branch: w.Branch, Head: w.Head}
			switch {
			case w.Main:
				item.Why = "the clone itself"
			case w.Worker:
				item.Why = "a worker's directory"
			case w.Branch == "":
				item.Why = "detached: not on a branch"
			case w.Dirty > 0:
				item.Why = fmt.Sprintf("%d uncommitted or untracked file(s)", w.Dirty)
			default:
				why, err := mergedIn(w.Head)
				if err != nil {
					return nil, err
				}
				item.Remove, item.Why = why != "", why
				if why == "" {
					item.Why = "no merged pull request ends at its commit"
				}
			}
			if !item.Remove && w.Branch != "" {
				checkedOut[w.Branch] = true
			}
			items = append(items, item)
		}
		defaultName := strings.TrimPrefix(repo.Default, "origin/")
		for _, b := range repo.Branches {
			item := Item{Repo: repo, Branch: b.Name, Head: b.Head}
			switch {
			case b.Name == defaultName:
				item.Why = "the default branch"
			case checkedOut[b.Name]:
				item.Why = "checked out in a worktree that stays"
			default:
				why, err := mergedIn(b.Head)
				if err != nil {
					return nil, err
				}
				item.Remove, item.Why = why != "", why
				if why == "" {
					item.Why = "no merged pull request ends at its commit"
				}
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// ApplyScript builds the script that removes what the plan says goes. Every
// step checks again that the item is as it was listed: a worktree is removed
// only at the commit it had, and git itself refuses one with changes; a
// branch is deleted only at the commit it had, and never while checked out.
func ApplyScript(items []Item) string {
	var b strings.Builder
	dirs := map[string]bool{}
	for _, it := range items {
		if !it.Remove || it.Path == "" {
			continue
		}
		fmt.Fprintf(&b, "out=\"it changed since it was listed\"\n")
		fmt.Fprintf(&b, "if [ \"$(git -C %s rev-parse HEAD 2>/dev/null)\" = %s ] && out=$(git -C %s worktree remove %s 2>&1); then echo \"@@removed \"%s; else echo \"@@kept \"%s\"|$out\" | tr '\\n' ' '; echo; fi\n",
			quote(it.Path), quote(it.Head), quote(it.Repo.Dir), quote(it.Path), quote(it.Path), quote(it.Path))
	}
	for _, it := range items {
		if !it.Remove || it.Path != "" {
			continue
		}
		ref := quote("refs/heads/" + it.Branch)
		fmt.Fprintf(&b, "if git -C %s worktree list --porcelain | grep -qxF %s; then echo \"@@keptbranch \"%s\"|it is checked out in a worktree\"\n", quote(it.Repo.Dir), quote("branch refs/heads/"+it.Branch), quote(it.Branch))
		fmt.Fprintf(&b, "elif out=$(git -C %s update-ref -d %s %s 2>&1); then echo \"@@deleted \"%s; else echo \"@@keptbranch \"%s\"|$out\" | tr '\\n' ' '; echo; fi\n",
			quote(it.Repo.Dir), ref, quote(it.Head), quote(it.Branch), quote(it.Branch))
		dirs[it.Repo.Dir] = true
	}
	for _, it := range items {
		if it.Remove && !dirs[it.Repo.Dir+"\x00pruned"] {
			dirs[it.Repo.Dir+"\x00pruned"] = true
			fmt.Fprintf(&b, "git -C %s worktree prune\n", quote(it.Repo.Dir))
		}
	}
	b.WriteString("echo \"@@end\"\n")
	return b.String()
}

// Result is what the apply script did.
type Result struct {
	RemovedWorktrees []string
	DeletedBranches  []string
	// Kept lists what the plan said goes and the machine did not remove,
	// each with git's reason.
	Kept []string
}

// ParseApplied reads the apply script's output.
func ParseApplied(out string) (Result, error) {
	var res Result
	complete := false
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch tag {
		case "@@removed":
			res.RemovedWorktrees = append(res.RemovedWorktrees, rest)
		case "@@deleted":
			res.DeletedBranches = append(res.DeletedBranches, rest)
		case "@@kept", "@@keptbranch":
			what, why, _ := strings.Cut(rest, "|")
			res.Kept = append(res.Kept, what+": "+strings.TrimSpace(why))
		case "@@end":
			complete = true
		}
	}
	if !complete {
		return res, fmt.Errorf("tidy: the machine stopped before it finished; run it again to see what is left")
	}
	return res, nil
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
