package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/edisoncode/ai-shed/contrib"
)

// templates are the files shed prints for a repository, by name, each with
// where it goes.
var templates = map[string]struct {
	content []byte
	path    string
}{
	"issue": {contrib.WorkerIssue, ".github/ISSUE_TEMPLATE/worker-task.md"},
}

// cmdTemplate prints a template on standard output, so it can be redirected
// into the repository or handed to an agent that sets the repository up.
// Where the file goes is said on standard error, which a redirect leaves on
// the screen.
func cmdTemplate(args []string) int {
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(args) != 1 {
		return fail(fmt.Errorf("usage: shed template <%s>", strings.Join(names, "|")))
	}
	tmpl, ok := templates[args[0]]
	if !ok {
		return fail(fmt.Errorf("no template %q; there is: %s", args[0], strings.Join(names, ", ")))
	}
	os.Stdout.Write(tmpl.content)
	fmt.Fprintf(os.Stderr, "shed: this goes in %s of the repository the workers take issues from\n", tmpl.path)
	return exitOK
}
