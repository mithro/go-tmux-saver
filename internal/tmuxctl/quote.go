package tmuxctl

import "strings"

// Quote wraps s as ONE tmux double-quoted command-line argument.
//
// RULING R30: tmux's own double-quote syntax is NOT Go's %q, and NOT a
// POSIX shell's either — verified against this tmux (next-3.8) directly via
// control mode: inside "…", tmux EXPANDS "$NAME" (an argument written as
// "'echo' '$HOME'" is typed into the pane as "'echo' '/home/tim'" — tmux
// resolved $HOME itself before send-keys ever ran), and Go-style "\xNN"
// escapes are mangled (an argument written as "esc\x1bhere" is typed as
// "escx1bhere" — tmux doesn't understand \x and just drops the backslash).
// "\"", "\\", "\$", ";", "#{…}", and a raw literal tab all pass through
// tmux's double-quote parsing untouched.
//
// So only backslash, the closing quote, and the dollar sign need escaping
// for tmux itself. A literal newline or carriage return in the argument
// would otherwise end the command line before reaching tmux's own escape
// handling, so those are also escaped, as "\n"/"\r" text (not the control
// bytes) — tmux does understand those in a double-quoted string. Every
// other byte, including a raw ESC, passes through unescaped.
//
// EVERY data-derived argument on a tmux command line must go through Quote,
// including composed targets like "-t <session>:<index>.<pane>": a session
// or window name is arbitrary user text, and tmux treats an unquoted ';' as
// a command separator (a session named `evil;kill-window -t default:0`
// really did run kill-window against a live server before this was
// enforced) and an unquoted space as an argument separator.
func Quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\', '"', '$':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
