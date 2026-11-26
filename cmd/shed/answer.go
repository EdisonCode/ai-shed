package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// cmdAnswer posts the owner's answer on an issue under the phrase of the
// signal it answers, so that the answer counts: an answer in other words
// changes nothing.
func cmdAnswer(args []string) int {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	accept := fs.Bool("accept", false, "take the work as it stands (\"Owner ruling: accepted\" by default); the text, if any, follows")
	unblock := fs.Bool("unblock", false, "the cause of a blocked eye check is gone: hand the look to a worker again")
	signal := fs.String("signal", "", "the signal to answer, when more than one is open on the issue")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() < 2 || fs.NArg() > 3 {
		return fail(fmt.Errorf("usage: shed answer [-accept|-unblock] [-signal name] <machine> <issue> [file|-]"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	number, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(1), "#"))
	if err != nil {
		return fail(fmt.Errorf("issue %q is not a number", fs.Arg(1)))
	}
	text := ""
	if fs.NArg() == 3 {
		if text, err = readAnswer(fs.Arg(2)); err != nil {
			return fail(err)
		}
	}
	ctx := context.Background()
	issue, found, err := backlog.Find(ctx, backlog.GH{}, m.AllSources(), number)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("#%d is in no queue of %s", number, m.Name))
	}
	body, err := answerComment(issue, cfg.Signals, *signal, *accept, *unblock, text)
	if err != nil {
		return fail(fmt.Errorf("%s#%d: %w", issue.Repo, issue.Number, err))
	}
	if err := (backlog.GH{}).Comment(ctx, issue.Repo, issue.Number, body); err != nil {
		return fail(err)
	}
	first, _, _ := strings.Cut(body, "\n")
	fmt.Printf("posted on %s#%d: %s\n", issue.Repo, issue.Number, first)
	return exitOK
}

func readAnswer(name string) (string, error) {
	var data []byte
	var err error
	if name == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(name)
	}
	if err != nil {
		return "", fmt.Errorf("read the answer: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// answerComment is the comment that answers the issue's open signal with
// the text: the signal's phrase, then the text. It refuses when no open
// signal takes an answer, or when more than one does and none was chosen.
func answerComment(issue backlog.Issue, signals []config.Signal, pick string, accept, unblock bool, text string) (string, error) {
	var open, answerable []string
	byName := map[string]config.Signal{}
	for _, s := range signals {
		byName[s.Name] = s
	}
	for _, a := range backlog.Asks(issue, signals) {
		open = append(open, a.Name+" ("+a.Answer+")")
		if s, ok := byName[a.Name]; ok && s.AnsweredBy != "" && !s.FollowsPR && (pick == "" || pick == a.Name) {
			answerable = append(answerable, a.Name)
		}
	}
	switch {
	case len(answerable) == 0 && len(open) == 0:
		return "", fmt.Errorf("nothing on it waits on you")
	case len(answerable) == 0 && pick != "":
		return "", fmt.Errorf("no open %s signal takes an answer; open: %s", pick, strings.Join(open, "; "))
	case len(answerable) == 0:
		return "", fmt.Errorf("no open signal takes an answer in a comment; open: %s", strings.Join(open, "; "))
	case len(answerable) > 1:
		return "", fmt.Errorf("%d open signals take an answer (%s): choose one with -signal", len(answerable), strings.Join(answerable, ", "))
	}
	s := byName[answerable[0]]
	switch {
	case accept && unblock:
		return "", fmt.Errorf("-accept and -unblock answer different things: give one")
	case unblock:
		if state, _, _, _ := backlog.EyeCheck(issue, signals); s.Name != config.EyesSignal || s.UnblockedBy == "" || state != backlog.EyesBlocked {
			return "", fmt.Errorf("-unblock is for a blocked eye check, and the open %s signal is not one", s.Name)
		}
		return withText(s.UnblockedBy, text), nil
	case accept:
		if len(s.Accept) == 0 {
			return "", fmt.Errorf("the %s signal has no accept words: answer it with the text", s.Name)
		}
		body := s.AnsweredBy + ": " + s.Accept[0]
		if text != "" {
			body += "\n\n" + text
		}
		return body, nil
	case text == "":
		return "", fmt.Errorf("give the answer's text, in a file or - for standard input")
	}
	return withText(s.AnsweredBy, text), nil
}

func withText(phrase, text string) string {
	if text == "" {
		return phrase
	}
	return phrase + ": " + text
}
