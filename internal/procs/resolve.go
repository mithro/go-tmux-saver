package procs

import (
	"regexp"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

var DefaultAllowlist = []string{"ssh", "mosh-client", "claude", "claude-resume", "vi", "vim", "nvim", "emacs", "man", "less", "more", "tail", "top", "htop"}

var resumeRe = regexp.MustCompile(`(?:^|[\s/])(?:claude-resume|--resume)\s+([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

var shells = map[string]bool{"bash": true, "zsh": true, "sh": true, "fish": true, "dash": true}

// Resolve decides how a pane should be restored from its process subtree.
func Resolve(t *Table, reg ClaudeRegistry, panePID int, allowlist []string) snapshot.Restore {
	if _, ok := t.Get(panePID); !ok {
		return snapshot.Restore{Kind: "shell"}
	}
	pids := t.Subtree(panePID)
	for _, pid := range pids {
		if p, _ := t.Get(pid); p.Comm == "claude" {
			if sid, ok := reg.SessionFor(p); ok {
				return snapshot.Restore{Kind: "claude", ClaudeSession: sid}
			}
		}
	}
	for _, pid := range pids {
		p, _ := t.Get(pid)
		if m := resumeRe.FindStringSubmatch(strings.Join(p.Cmdline, " ")); m != nil {
			return snapshot.Restore{Kind: "claude", ClaudeSession: m[1]}
		}
	}
	allowed := map[string]bool{}
	for _, a := range allowlist {
		allowed[a] = true
	}
	for _, pid := range pids {
		p, _ := t.Get(pid)
		if shells[p.Comm] {
			continue
		}
		if allowed[p.Comm] && len(p.Cmdline) > 0 {
			return snapshot.Restore{Kind: "argv", Argv: append([]string(nil), p.Cmdline...)}
		}
	}
	return snapshot.Restore{Kind: "shell"}
}
