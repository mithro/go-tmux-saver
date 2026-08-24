package tmuxctl

import "testing"

// TestQuote covers RULING R30's escaping rules directly: backslash,
// double-quote, and dollar sign are escaped (dollar because tmux's own
// double-quote parsing expands "$NAME" — verified against this tmux via
// control mode); a literal newline is escaped as "\n" text; and a raw ESC
// byte (0x1B) passes through completely untouched (Go's %q would have
// mangled it, per the same verification — tmux doesn't understand \xNN).
func TestQuote(t *testing.T) {
	in := "a\\b\"c$d\ne" + "\x1b" + "f"
	got := Quote(in)
	want := `"a\\b\"c\$d\ne` + "\x1b" + `f"`
	if got != want {
		t.Fatalf("Quote(%q) = %q, want %q", in, got, want)
	}
}

// TestQuoteCommandSeparators pins the C1 property the restore planner
// depends on: a value containing tmux's command separator (';') or an
// argument separator (' ') comes back as ONE quoted argument, with the
// separators inert inside the quotes.
func TestQuoteCommandSeparators(t *testing.T) {
	if got, want := Quote("evil;kill-window -t default:0"), `"evil;kill-window -t default:0"`; got != want {
		t.Fatalf("Quote = %q, want %q", got, want)
	}
}

// TestUnvisName covers the decode side of tmux's name storage (issue #8's
// hostile-names work): tmux vis(3)-encodes session/window names at
// creation (backslash doubled, control chars → \t/\n/\ooo...), so restore
// must decode a stored name before handing it back to -s/-n — tmux's own
// re-encoding then reproduces the stored spelling exactly.
func TestUnvisName(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain name`, `plain name`},
		{`a\\b`, `a\b`},              // stored doubled backslash
		{`a\\\\b`, `a\\b`},           // two stored backslashes
		{`tab\there`, "tab\there"},   // \t → TAB
		{`nl\nhere`, "nl\nhere"},     // \n → LF
		{`oct\011here`, "oct\there"}, // \011 → TAB (octal form)
		{`bel\007`, "bel\a"},         // full 3-digit octal
		{`mixed\\and\ttab`, "mixed\\and\ttab"},
		{`unknown\qkeep`, `unknown\qkeep`}, // unknown escape preserved
		{`q"uo\\te.s;s`, `q"uo\te.s;s`},    // the live-test fixture shape
	}
	for _, c := range cases {
		if got := UnvisName(c.in); got != c.want {
			t.Errorf("UnvisName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
