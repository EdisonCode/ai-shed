package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// loginTerm answers each thing typed into the login window with the next
// screen.
type loginTerm struct {
	pfTerminal
	screens []string
}

func (l *loginTerm) CaptureJoined(string, int) (string, error) {
	n := min(len(l.sent), len(l.screens)) - 1
	if n < 0 {
		return "", nil
	}
	return l.screens[n], nil
}

func newLoginTerm(screens ...string) *loginTerm {
	return &loginTerm{pfTerminal: *newPF("2.1.296"), screens: screens}
}

func (l *loginTerm) clock() (func() time.Time, func(time.Duration)) {
	return func() time.Time { return l.now }, func(d time.Duration) { l.now = l.now.Add(d) }
}

const signInURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-0000-4000-8000-00000000abcd&response_type=code&redirect_uri=https%3A%2F%2Fexample.com%2Fcallback&code_challenge=c4allenge&code_challenge_method=S256&state=xyz"

func TestASignInURLTheToolBrokeOverLinesIsReadWhole(t *testing.T) {
	broken := "Browser didn't open? Use the url below to sign in:\n\n" + signInURL[:80] + "\n" + signInURL[80:170] + "\n" + signInURL[170:] + "\n\nPaste code here if prompted >"
	if got := LoginURL(broken); got != signInURL {
		t.Fatalf("url = %q, want %q", got, signInURL)
	}
}

func TestACutSignInURLIsNotGivenOut(t *testing.T) {
	term := newLoginTerm("> ", "Use the url below to sign in:\n\n"+signInURL[:80]+"\n\nPaste code here if prompted >")
	now, sleep := term.clock()
	if url, err := StartLogin(term, "claude", t.TempDir(), now, sleep); err == nil || !strings.Contains(err.Error(), "client_id is missing or cut") {
		t.Fatalf("url = %q, err = %v", url, err)
	}
}

func TestStartLoginTypesLoginPicksTheSubscriptionAndReturnsTheURL(t *testing.T) {
	term := newLoginTerm(
		"> ",
		"Select login method:\n❯ 1. Claude account with subscription\n  2. Anthropic Console account",
		"Browser didn't open? Use the url below to sign in:\n\n"+signInURL+"\n\nPaste code here if prompted >",
	)
	now, sleep := term.clock()
	url, err := StartLogin(term, "claude --permission-mode auto", t.TempDir(), now, sleep)
	if err != nil {
		t.Fatal(err)
	}
	if url != signInURL {
		t.Fatalf("url = %q, want %q", url, signInURL)
	}
	if !slices.Equal(term.sent, []string{"claude --permission-mode auto", "/login", ""}) {
		t.Fatalf("typed %q", term.sent)
	}
	if len(term.closed) != 0 || term.opened[0] != LoginWindow {
		t.Fatalf("opened %q, closed %q: the window must stay open for the code", term.opened, term.closed)
	}
}

func TestStartLoginWithNoURLClosesTheWindowAndShowsTheScreen(t *testing.T) {
	term := newLoginTerm("> ", "Error: could not reach the server")
	now, sleep := term.clock()
	_, err := StartLogin(term, "claude", t.TempDir(), now, sleep)
	if err == nil || !strings.Contains(err.Error(), "could not reach the server") {
		t.Fatalf("err = %v, want the screen", err)
	}
	if !slices.Equal(term.closed, []string{LoginWindow}) {
		t.Fatalf("closed %q", term.closed)
	}
}

func TestStartLoginStopsAtAFolderTrustQuestionWithoutAnsweringIt(t *testing.T) {
	term := newLoginTerm("Accessing workspace: /home/owner\nQuick safety check: Is this a project you created or one you trust?\n❯ No, exit\n  Yes, I trust this folder")
	now, sleep := term.clock()
	_, err := StartLogin(term, "claude", "/home/owner", now, sleep)
	if err == nil || !strings.Contains(err.Error(), "asks whether to trust /home/owner") {
		t.Fatalf("err = %v, want the trust question named", err)
	}
	if !slices.Equal(term.sent, []string{"claude"}) || !slices.Equal(term.closed, []string{LoginWindow}) {
		t.Fatalf("typed %q, closed %q: /login must not be typed into the question", term.sent, term.closed)
	}
}

func TestFinishLoginPastesTheCodeAndClosesTheWindowOnSuccess(t *testing.T) {
	term := newLoginTerm("Login successful. Press Enter to continue")
	term.windows[LoginWindow] = Observation{Exists: true, Command: "2.1.296", LastActivity: term.now}
	now, sleep := term.clock()
	if _, err := FinishLogin(term, "AbC-123_x#state", now, sleep); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(term.sent, []string{"AbC-123_x#state"}) || !slices.Equal(term.closed, []string{LoginWindow}) {
		t.Fatalf("typed %q, closed %q", term.sent, term.closed)
	}
}

func TestFinishLoginThatFailsKeepsTheWindowForAnotherCode(t *testing.T) {
	term := newLoginTerm("OAuth error: Invalid code. Paste code here if prompted >")
	term.windows[LoginWindow] = Observation{Exists: true, Command: "2.1.296", LastActivity: term.now}
	now, sleep := term.clock()
	_, err := FinishLogin(term, "wrong", now, sleep)
	if err == nil || !strings.Contains(err.Error(), "Invalid code") || len(term.closed) != 0 {
		t.Fatalf("err = %v, closed %q", err, term.closed)
	}
}

func TestFinishLoginWithNoLoginStartedSaysWhatToRunFirst(t *testing.T) {
	term := newLoginTerm()
	now, sleep := term.clock()
	if _, err := FinishLogin(term, "abc", now, sleep); err == nil || !strings.Contains(err.Error(), "shed login <machine>") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginCodeIsCheckedBeforeItIsTyped(t *testing.T) {
	for code, ok := range map[string]bool{"AbC-123_x#state.v2~": true, "": false, "abc; rm -rf ~": false, "a b": false, "$(id)": false} {
		if err := ValidLoginCode(code); (err == nil) != ok {
			t.Errorf("ValidLoginCode(%q) = %v, want ok %v", code, err, ok)
		}
	}
}

func TestAccountIsReadFromTheToolsOwnFile(t *testing.T) {
	home := t.TempDir()
	body := `{"numStartups": 3, "oauthAccount": {"emailAddress": "owner@example.com", "organizationName": "Example Org"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Account(home); err != nil || got != "owner@example.com (Example Org)" {
		t.Fatalf("account = %q, %v", got, err)
	}
	if got, err := Account(t.TempDir()); err != nil || got != "" {
		t.Fatalf("with no file: %q, %v; want none", got, err)
	}
}
