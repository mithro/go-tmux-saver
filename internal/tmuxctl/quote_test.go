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
