package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// LoginWindow is the throwaway window `shed login` signs the machine's tool
// in from. It is not a worker's name, so no supervisor looks at it.
const LoginWindow = "login-account"

const loginRows = 80

// LoginTerminal is a Terminal that can also capture a window with wrapped
// lines joined, so a long sign-in URL reads as one line.
type LoginTerminal interface {
	Terminal
	CaptureJoined(window string, lines int) (string, error)
}

var (
	urlRE         = regexp.MustCompile(`https://\S+`)
	loginMethodRE = regexp.MustCompile(`(?i)login method`)
	loginOKRE     = regexp.MustCompile(`(?i)login successful|logged in as|successfully logged in`)
	loginFailRE   = regexp.MustCompile(`(?i)invalid|error|failed|expired|denied`)
	loginCodeRE   = regexp.MustCompile(`^[A-Za-z0-9#._~-]+$`)
)

// StartLogin starts the tool in the login window, types its login command,
// takes the first choice of a login method menu (the subscription account)
// and returns the sign-in URL the tool prints. The window stays open: the
// code from the browser is typed into it by FinishLogin.
func StartLogin(term LoginTerminal, command, dir string, now func() time.Time, sleep func(time.Duration)) (string, error) {
	if stale, err := term.Observe(LoginWindow); err != nil {
		return "", err
	} else if stale.Exists {
		if err := term.Close(LoginWindow); err != nil {
			return "", err
		}
	}
	if err := term.Open(LoginWindow, dir); err != nil {
		return "", err
	}
	screen := ""
	for _, text := range []string{command, "/login"} {
		var err error
		if screen, err = typeAndRead(term, text, now, sleep); err != nil {
			term.Close(LoginWindow)
			return "", err
		}
	}
	if loginMethodRE.MatchString(screen) {
		var err error
		if screen, err = typeAndRead(term, "", now, sleep); err != nil {
			term.Close(LoginWindow)
			return "", err
		}
	}
	url := LoginURL(screen)
	if url == "" {
		term.Close(LoginWindow)
		return "", fmt.Errorf("the tool printed no sign-in URL. Its screen:\n%s", indent(screen))
	}
	return url, nil
}

// FinishLogin types the code from the browser into the login window and
// reads whether the tool took it. On success the window is closed. On a
// failure it stays open, so another code can be typed.
func FinishLogin(term LoginTerminal, code string, now func() time.Time, sleep func(time.Duration)) (string, error) {
	if err := ValidLoginCode(code); err != nil {
		return "", err
	}
	obs, err := term.Observe(LoginWindow)
	if err != nil {
		return "", err
	}
	if !obs.Exists {
		return "", errors.New("no login is waiting for a code on this machine: run `shed login <machine>` first")
	}
	screen, err := typeAndRead(term, code, now, sleep)
	if err != nil {
		return "", err
	}
	switch {
	case loginOKRE.MatchString(screen):
		return screen, term.Close(LoginWindow)
	case loginFailRE.MatchString(screen):
		return screen, fmt.Errorf("the tool did not take the code. Its screen:\n%s", indent(screen))
	default:
		return screen, fmt.Errorf("could not tell whether the login worked. Its screen:\n%s", indent(screen))
	}
}

func typeAndRead(term LoginTerminal, text string, now func() time.Time, sleep func(time.Duration)) (string, error) {
	if err := term.Send(LoginWindow, text); err != nil {
		return "", err
	}
	if _, err := waitToSettle(term, LoginWindow, now, sleep); err != nil {
		return "", err
	}
	return term.CaptureJoined(LoginWindow, loginRows)
}

// LoginURL returns the last https URL on the screen; "" for none.
func LoginURL(screen string) string {
	found := urlRE.FindAllString(screen, -1)
	if len(found) == 0 {
		return ""
	}
	return found[len(found)-1]
}

// ValidLoginCode checks that a code is the kind a sign-in page gives, before
// it goes through a shell and into a terminal.
func ValidLoginCode(code string) error {
	if !loginCodeRE.MatchString(code) {
		return fmt.Errorf("%q does not look like a sign-in code: letters, digits and #._~- only", code)
	}
	return nil
}

// Account names the account Claude Code on this machine is logged in as,
// from its own state file; "" when it names none.
func Account(home string) (string, error) {
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the logged-in account: %w", err)
	}
	var state struct {
		OAuthAccount struct {
			Email        string `json:"emailAddress"`
			Organization string `json:"organizationName"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("read the logged-in account: %w", err)
	}
	account := state.OAuthAccount.Email
	if org := strings.TrimSpace(state.OAuthAccount.Organization); account != "" && org != "" {
		account += " (" + org + ")"
	}
	return account, nil
}
