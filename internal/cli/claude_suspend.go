package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/restore"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// SuspendDeps bundles what RunSuspend needs so tests can drive it with a
// tmuxctl.Fake, fixture /proc tables and no real sleeping.
type SuspendDeps struct {
	T   tmuxctl.Transport
	Reg procs.ClaudeRegistry
	// Scan re-reads the process table — called once up front and again on
	// every exit-confirmation poll.
	Scan      func() (*procs.Table, error)
	Allowlist []string
	// Exe is the binary whose claude-resume placeholder gets typed into
	// the pane after Claude exits.
	Exe string
	// SavedDir receives one pane-capture file per suspended pane, handed
	// to the placeholder via --saved-output (issue #15).
	SavedDir    string
	Out         io.Writer
	Sleep       func(time.Duration)
	ExitTimeout time.Duration
}

// shellArgQuote renders argv as a single-quoted, space-joined string safe
// for a pane's shell to re-parse (same rules as restore's shellQuote).
func shellArgQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

// claudeInPane resolves the pane's Claude session and the claude PROCESS
// pid inside its subtree, or ok=false when the pane isn't running Claude.
func claudeInPane(tb *procs.Table, reg procs.ClaudeRegistry, allowlist []string, panePID int) (sid string, claudePID int, ok bool) {
	r := procs.Resolve(tb, reg, panePID, allowlist)
	if r.Kind != "claude" || r.ClaudeSession == "" {
		return "", 0, false
	}
	for _, pid := range tb.Subtree(panePID) {
		if p, ok := tb.Get(pid); ok && p.Comm == "claude" {
			return r.ClaudeSession, pid, true
		}
	}
	// Resolved via a placeholder cmdline (claude-resume/--resume) rather
	// than a live claude process — nothing to suspend.
	return "", 0, false
}

// suspendPane parks one pane's running Claude behind the placeholder:
// capture scrollback → type /exit → confirm the claude process is gone →
// type the placeholder with the capture as --saved-output.
func suspendPane(ctx context.Context, d SuspendDeps, target, paneID string, sid string, claudePID int) error {
	capture, err := d.T.Run(ctx, fmt.Sprintf("capture-pane -epJ -S - -t %s", tmuxctl.Quote(paneID)))
	if err != nil {
		return fmt.Errorf("capture-pane: %w", err)
	}
	if err := os.MkdirAll(d.SavedDir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(d.SavedDir, strings.TrimPrefix(paneID, "%")+"-"+sid[:8]+".txt")
	if err := os.WriteFile(file, []byte(strings.Join(capture, "\n")+"\n"), 0o600); err != nil {
		return err
	}

	// /exit typed as text first, Enter separately after a beat — Claude's
	// slash-command palette needs the text settled before the confirm.
	if _, err := d.T.Run(ctx, fmt.Sprintf("send-keys -t %s %s", tmuxctl.Quote(paneID), tmuxctl.Quote("/exit"))); err != nil {
		return fmt.Errorf("send /exit: %w", err)
	}
	d.Sleep(500 * time.Millisecond)
	if _, err := d.T.Run(ctx, fmt.Sprintf("send-keys -t %s Enter", tmuxctl.Quote(paneID))); err != nil {
		return fmt.Errorf("send Enter: %w", err)
	}

	// Confirm Claude actually exited — bounded poll on the process table.
	deadline := time.Now().Add(d.ExitTimeout)
	for {
		tb, err := d.Scan()
		if err != nil {
			return err
		}
		if p, ok := tb.Get(claudePID); !ok || p.Comm != "claude" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("claude (pid %d) still running after %s — not suspended", claudePID, d.ExitTimeout)
		}
		d.Sleep(250 * time.Millisecond)
	}
	// Give the shell prompt a beat to come back before typing into it.
	d.Sleep(300 * time.Millisecond)

	// Leading space: keep the placeholder invocation out of shell history,
	// like the restore replay does.
	cmd := " " + shellArgQuote([]string{d.Exe, "claude-resume", sid, "--saved-output", file})
	if _, err := d.T.Run(ctx, fmt.Sprintf("send-keys -t %s %s Enter", tmuxctl.Quote(paneID), tmuxctl.Quote(cmd))); err != nil {
		return fmt.Errorf("type placeholder: %w", err)
	}
	fmt.Fprintf(d.Out, "suspended %s (%s…)\n", target, sid[:8])
	return nil
}

// windowPanes lists (paneID, panePID) for one window target.
func windowPanes(ctx context.Context, t tmuxctl.Transport, sess string, winIdx int) ([][2]string, error) {
	lines, err := t.Run(ctx, fmt.Sprintf("list-panes -t %s -F \"#{pane_id}\t#{pane_pid}\"", tmuxctl.Quote(fmt.Sprintf("=%s:%d", sess, winIdx))))
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, l := range lines {
		if id, pid, ok := strings.Cut(l, "\t"); ok {
			out = append(out, [2]string{id, pid})
		}
	}
	return out, nil
}

// resolveWindow turns a window argument (index or name) into indexes within
// sess. A name may match several windows; all matches are returned.
func resolveWindow(ctx context.Context, t tmuxctl.Transport, sess, winArg string) ([]int, error) {
	if n, err := strconv.Atoi(winArg); err == nil {
		return []int{n}, nil
	}
	lines, err := t.Run(ctx, fmt.Sprintf("list-windows -t %s -F \"#{window_index}\t#{window_name}\"", tmuxctl.Quote("="+sess)))
	if err != nil {
		return nil, err
	}
	var idxs []int
	for _, l := range lines {
		idxStr, name, ok := strings.Cut(l, "\t")
		if !ok || name != winArg {
			continue
		}
		if n, err := strconv.Atoi(idxStr); err == nil {
			idxs = append(idxs, n)
		}
	}
	if len(idxs) == 0 {
		return nil, fmt.Errorf("no window named %q in session %q", winArg, sess)
	}
	return idxs, nil
}

// suspendWindow suspends every Claude pane in one window; returns how many
// panes were suspended and how many failed.
func suspendWindow(ctx context.Context, d SuspendDeps, tb *procs.Table, sess string, winIdx int) (done, failed int) {
	panes, err := windowPanes(ctx, d.T, sess, winIdx)
	if err != nil {
		fmt.Fprintf(d.Out, "error: %s:%d: %v\n", sess, winIdx, err)
		return 0, 1
	}
	for _, p := range panes {
		panePID, err := strconv.Atoi(p[1])
		if err != nil {
			continue
		}
		sid, claudePID, ok := claudeInPane(tb, d.Reg, d.Allowlist, panePID)
		if !ok {
			continue
		}
		target := fmt.Sprintf("%s:%d %s", sess, winIdx, p[0])
		if err := suspendPane(ctx, d, target, p[0], sid, claudePID); err != nil {
			fmt.Fprintf(d.Out, "error: %s: %v\n", target, err)
			failed++
			continue
		}
		done++
	}
	return done, failed
}

// RunSuspend executes claude-suspend: one window (by [session +] index or
// name) or, with all=true, every window of every session group.
func RunSuspend(ctx context.Context, d SuspendDeps, sessArg, winArg string, all bool) (done, failed int, err error) {
	tb, err := d.Scan()
	if err != nil {
		return 0, 0, err
	}
	if all {
		live, err := restore.QueryLive(ctx, d.T)
		if err != nil {
			return 0, 0, err
		}
		for sess, wins := range live.Sessions {
			for _, w := range wins {
				dn, fl := suspendWindow(ctx, d, tb, sess, w.Index)
				done, failed = done+dn, failed+fl
			}
		}
		return done, failed, nil
	}
	idxs, err := resolveWindow(ctx, d.T, sessArg, winArg)
	if err != nil {
		return 0, 0, err
	}
	for _, idx := range idxs {
		dn, fl := suspendWindow(ctx, d, tb, sessArg, idx)
		done, failed = done+dn, failed+fl
	}
	return done, failed, nil
}

// currentSession names the tmux session hosting this process's pane
// ($TMUX_PANE), for the session-less argument form.
func currentSession(ctx context.Context, t tmuxctl.Transport) (string, error) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", fmt.Errorf("not inside tmux ($TMUX_PANE unset) — name the session: claude-suspend <session> <window>")
	}
	lines, err := t.Run(ctx, fmt.Sprintf("display-message -p -t %s \"#{session_name}\"", tmuxctl.Quote(pane)))
	if err != nil || len(lines) == 0 {
		return "", fmt.Errorf("resolve current session: %v", err)
	}
	return lines[0], nil
}

func init() {
	register(command{"claude-suspend", "park running Claude session(s) behind the claude-resume placeholder (/exit, confirm, re-type)", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("claude-suspend", flag.ContinueOnError)
		all := fs.Bool("all", false, "suspend every Claude session in every window of every session group")
		exitTimeout := fs.Duration("exit-timeout", 30*time.Second, "how long to wait for Claude to exit after /exit")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		pos := fs.Args()
		if !*all && len(pos) == 0 || len(pos) > 2 {
			fmt.Fprintln(stderr, "usage: claude-suspend [<session>] <window> | claude-suspend --all")
			return 2
		}

		cfg, store, msg, code := commonSetup(*cfgPath, *socket, *dataDir)
		if code != 0 {
			fmt.Fprintln(stderr, msg)
			return code
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		tr, err := openTransport(ctx, cfg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer tr.Close()

		sessArg, winArg := "", ""
		switch len(pos) {
		case 1:
			winArg = pos[0]
			if !*all {
				if sessArg, err = currentSession(ctx, tr); err != nil {
					fmt.Fprintln(stderr, "claude-suspend:", err)
					return 2
				}
			}
		case 2:
			sessArg, winArg = pos[0], pos[1]
		}

		exe, err := resolveBinary()
		if err != nil {
			fmt.Fprintln(stderr, "claude-suspend:", err)
			return 1
		}
		home, _ := os.UserHomeDir()
		d := SuspendDeps{
			T: tr, Reg: procs.ClaudeRegistry{Dir: filepath.Join(home, ".claude", "sessions")},
			Scan:      func() (*procs.Table, error) { return procs.Scan("/proc") },
			Allowlist: cfg.Allowlist, Exe: exe,
			SavedDir: filepath.Join(store.Dir, "suspend"),
			Out:      stdout, Sleep: time.Sleep, ExitTimeout: *exitTimeout,
		}
		done, failed, err := RunSuspend(ctx, d, sessArg, winArg, *all)
		if err != nil {
			fmt.Fprintln(stderr, "claude-suspend:", err)
			return 1
		}
		fmt.Fprintf(stdout, "suspended %d, failed %d\n", done, failed)
		if failed > 0 {
			return 1
		}
		return 0
	}})
}
