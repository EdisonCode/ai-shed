package supervisor

import (
	"strings"
	"testing"
)

// The bottom of a real Claude Code screen, as tmux captures it with styles.
// The worker is idle. The text after the prompt mark is a suggestion the
// tool drew faint; nothing was typed.
const idleWithSuggestion = "  Ready for the supervisor to name the issue.\n\n" +
	"\x1b[38;5;246m✻\x1b[39m \x1b[38;5;246mCogitated for 21s · done 1:29 PM\x1b[39m\n\n" +
	"\x1b[38;5;244m────────\n" +
	"\x1b[39m❯ \x1b[2mYour issue is #128: apply the owner ruling\x1b[0m\n" +
	"\x1b[38;5;244m────────\n" +
	"\x1b[39m  \x1b[38;5;220m⏵⏵ auto mode on\x1b[38;5;246m (shift+tab to cycle) · PR\x1b[39m \x1b[4m\x1b[38;5;246m\x1b]8;id=9s6rs2;https://github.com/org/app/pull/131\x1b\\#131\x1b[0m\x1b[38;5;246m\x1b]8;;\x1b\\ · ← for agents\x1b[39m\n"

func TestPlainMarksTheGreyedSuggestionAndNothingElse(t *testing.T) {
	want := "  Ready for the supervisor to name the issue.\n\n" +
		"✻ Cogitated for 21s · done 1:29 PM\n\n" +
		"────────\n" +
		"❯ [greyed out: Your issue is #128: apply the owner ruling]\n" +
		"────────\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle) · PR #131 · ← for agents\n"
	if got := plain(idleWithSuggestion); got != want {
		t.Fatalf("plain =\n%s\nwant\n%s", got, want)
	}
}

func TestPlain(t *testing.T) {
	cases := []struct{ name, styled, want string }{
		{"no styles", "❯ hello\n", "❯ hello\n"},
		{"faint ended by 22", "a \x1b[2mhint\x1b[22m b", "a [greyed out: hint] b"},
		{"faint ended by a bare reset", "\x1b[2mhint\x1b[mdone", "[greyed out: hint]done"},
		{"faint combined with a colour", "\x1b[2;38;5;246mhint\x1b[0m", "[greyed out: hint]"},
		{"colour number 2 is not faint", "\x1b[38;5;2mgreen\x1b[39m", "green"},
		{"true colour with a 2 in it is not faint", "\x1b[38;2;2;2;2mdark\x1b[39m", "dark"},
		{"a faint span ends with its line", "\x1b[2mone\ntwo\x1b[0m", "[greyed out: one]\n[greyed out: two]"},
		{"faint spaces are left alone", "a\x1b[2m   \x1b[0mb", "a   b"},
		{"the span's own spacing stays outside the marker", "\x1b[2m  hint \x1b[0m", "  [greyed out: hint] "},
		{"cursor and erase sequences are removed", "\x1b[2Kline\x1b[1;1H", "line"},
		{"a hyperlink keeps its text", "\x1b]8;;https://x.test\x07link\x1b]8;;\x07", "link"},
		{"a cut-off sequence does not break it", "text\x1b[38;5", "text"},
	}
	for _, tc := range cases {
		if got := plain(tc.styled); got != tc.want {
			t.Errorf("%s: plain(%q) = %q, want %q", tc.name, tc.styled, got, tc.want)
		}
	}
}

func TestPlainLeavesNoEscapeBehind(t *testing.T) {
	if got := plain(idleWithSuggestion); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("an escape character is left in %q", got)
	}
}
