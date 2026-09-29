package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/restore"
	"github.com/mithro/go-tmux-saver/internal/resume"
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
	// IdleFor, when > 0, restricts suspension to sessions whose last
	// user/assistant turn is at least this old (issue #46). ProjectsDir is
	// where their transcripts live (~/.claude/projects); Now is the clock.
	IdleFor     time.Duration
	ProjectsDir string
	Now         func() time.Time
	// DryRun reports what would be suspended without touching any pane.
	DryRun bool
}

// lastTurn is when session sid last had a user or assistant entry in its
// transcript. The transcript's mtime is useless for this: /remote-control
// appends bridge/system entries to every open session. ok=false when there
// is no transcript or no turn yet (a never-messaged session).
func lastTurn(projectsDir, sid string) (time.Time, bool) {
	path := resume.FindTranscript(projectsDir, sid)
	if path == "" {
		return time.Time{}, false
	}
	m, ok := resume.ReadMeta(path)
	if !ok || m.LastTS == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, m.LastTS)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// idleGate decides whether a Claude pane passes the --idle-for filter. It
// returns a short note for the report line: the idle age when it passes,
// the reason when it doesn't.
func idleGate(d SuspendDeps, sid string) (pass bool, note string) {
	if d.IdleFor <= 0 {
		return true, ""
	}
	ts, ok := lastTurn(d.ProjectsDir, sid)
	if !ok {
		return false, "no transcript or no turn yet"
	}
	age := d.Now().Sub(ts).Round(time.Minute)
	if age < d.IdleFor {
		return false, fmt.Sprintf("active %s ago", age)
	}
	return true, fmt.Sprintf("idle %s", age)
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

// isInputRule reports whether a screen line is one of the horizontal rules
// that frame Claude's input box (the top one may carry a session title).
func isInputRule(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "─") && strings.HasSuffix(t, "─")
}

// promptBlocker inspects the visible screen of a Claude pane and returns ""
// only when /exit can be typed safely: Claude idle at an EMPTY input box.
// Otherwise it names the reason. Typing into anything else destroys work:
// a draft would be submitted with /exit appended (issue #42), and in an
// open dialog Enter picks the highlighted option.
func promptBlocker(screen []string) string {
	last := -1
	for i, l := range screen {
		if strings.Contains(l, "esc to interrupt") {
			return "Claude is mid-turn"
		}
		if strings.HasPrefix(strings.TrimLeft(l, " "), "❯") {
			last = i
		}
	}
	if last < 0 {
		return "no Claude input box on screen"
	}
	// The input box is exactly: rule, one ❯ line, rule. A multi-line draft
	// or a dialog (whose ❯ marks the highlighted option) breaks the frame.
	if last == 0 || last == len(screen)-1 || !isInputRule(screen[last-1]) || !isInputRule(screen[last+1]) {
		return "input box not empty or a dialog is open"
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(screen[last], " "), "❯")) != "" {
		return "input box not empty"
	}
	return ""
}

// checkPrompt captures the pane's visible screen and refuses to proceed
// unless promptBlocker finds it safe.
func checkPrompt(ctx context.Context, t tmuxctl.Transport, paneID string) error {
	screen, err := t.Run(ctx, fmt.Sprintf("capture-pane -p -t %s", tmuxctl.Quote(paneID)))
	if err != nil {
		return fmt.Errorf("capture-pane: %w", err)
	}
	if b := promptBlocker(screen); b != "" {
		return fmt.Errorf("%s — not suspended", b)
	}
	return nil
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
		// Idle filter first: an active session is often mid-turn, and that
		// is a skip, not a prompt-guard failure.
		pass, note := idleGate(d, sid)
		if !pass {
			fmt.Fprintf(d.Out, "skip %s (%s…): %s\n", target, sid[:8], note)
			continue
		}
		// The prompt guard only reads the screen, so a dry run reports the
		// panes a real run would refuse.
		if err := checkPrompt(ctx, d.T, p[0]); err != nil {
			fmt.Fprintf(d.Out, "error: %s: %v\n", target, err)
			failed++
			continue
		}
		if d.DryRun {
			fmt.Fprintf(d.Out, "would suspend %s (%s…) %s\n", target, sid[:8], note)
			done++
			continue
		}
		if err := suspendPane(ctx, d, target, p[0], sid, claudePID); err != nil {
			fmt.Fprintf(d.Out, "error: %s: %v\n", target, err)
			failed++
			continue
		}
		done++
	}
	return done, failed
}

// winRef is one tmux window: session name + window index.
type winRef struct {
	Sess  string
	Index int
}

// selectWindows turns the command's arguments into windows: one window (by
// [session +] index or name — a name may match several) or, with all=true,
// every window of every session group, in session-name then index order.
func selectWindows(ctx context.Context, t tmuxctl.Transport, sessArg, winArg string, all bool) ([]winRef, error) {
	var out []winRef
	if all {
		live, err := restore.QueryLive(ctx, t)
		if err != nil {
			return nil, err
		}
		for sess, wins := range live.Sessions {
			for _, w := range wins {
				out = append(out, winRef{sess, w.Index})
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Sess != out[j].Sess {
				return out[i].Sess < out[j].Sess
			}
			return out[i].Index < out[j].Index
		})
		return out, nil
	}
	idxs, err := resolveWindow(ctx, t, sessArg, winArg)
	if err != nil {
		return nil, err
	}
	for _, idx := range idxs {
		out = append(out, winRef{sessArg, idx})
	}
	return out, nil
}

// RunSuspend executes claude-suspend: one window (by [session +] index or
// name) or, with all=true, every window of every session group.
func RunSuspend(ctx context.Context, d SuspendDeps, sessArg, winArg string, all bool) (done, failed int, err error) {
	tb, err := d.Scan()
	if err != nil {
		return 0, 0, err
	}
	wins, err := selectWindows(ctx, d.T, sessArg, winArg, all)
	if err != nil {
		return 0, 0, err
	}
	for _, w := range wins {
		dn, fl := suspendWindow(ctx, d, tb, w.Sess, w.Index)
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
		idleFor := fs.Duration("idle-for", 0, "only suspend sessions whose last user/assistant turn is at least this old (e.g. 48h)")
		dryRun := fs.Bool("dry-run", false, "list the Claude panes that would be suspended; touch nothing")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		pos := fs.Args()
		if !*all && len(pos) == 0 || len(pos) > 2 {
			fmt.Fprintln(stderr, "usage: claude-suspend [--idle-for D] [--dry-run] ([<session>] <window> | --all)")
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
			IdleFor: *idleFor, ProjectsDir: filepath.Join(home, ".claude", "projects"),
			Now: time.Now, DryRun: *dryRun,
		}
		done, failed, err := RunSuspend(ctx, d, sessArg, winArg, *all)
		if err != nil {
			fmt.Fprintln(stderr, "claude-suspend:", err)
			return 1
		}
		verb := "suspended"
		if *dryRun {
			verb = "would suspend"
		}
		fmt.Fprintf(stdout, "%s %d, failed %d\n", verb, done, failed)
		if failed > 0 {
			return 1
		}
		return 0
	}})
}
