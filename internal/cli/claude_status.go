package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mithro/go-tmux-saver/internal/resume"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// rcGreen is how Claude Code renders a connected Remote Control indicator.
const rcGreen = "\x1b[38;5;114m/rc\x1b[39m"

// rcState reads Remote Control's state from the bottom of a pane captured
// with escapes (capture-pane -e). The session file's bridgeSessionId
// survives a disconnect, so the coloured /rc indicator is the only live
// signal: green = connected, "/rc failed" = failed, any other /rc =
// connecting, none = off.
func rcState(screen []string) string {
	end := len(screen)
	for end > 0 && strings.TrimSpace(screen[end-1]) == "" {
		end--
	}
	tail := strings.Join(screen[max(0, end-6):end], "\n")
	switch {
	case strings.Contains(tail, rcGreen):
		return "connected"
	case strings.Contains(tail, "/rc failed"):
		return "failed"
	case strings.Contains(tail, "/rc"):
		return "connecting"
	}
	return "off"
}

var modelRe = regexp.MustCompile(`^claude-([a-z]+)-(\d+(?:-\d{1,2})?)(?:-\d{8})?$`)

// modelLabel turns a model id into family + version: claude-opus-5-5 →
// "Opus 5.5", claude-haiku-4-5-20251001 → "Haiku 4.5". Unknown shapes are
// returned as-is, "" as "?".
func modelLabel(model string) string {
	if model == "" {
		return "?"
	}
	m := modelRe.FindStringSubmatch(model)
	if m == nil {
		return model
	}
	return strings.ToUpper(m[1][:1]) + m[1][1:] + " " + strings.ReplaceAll(m[2], "-", ".")
}

// ago renders a duration as a short age: 5s, 2m, 2h01m, 2d07h.
func ago(d time.Duration) string {
	s := max(0, int(d/time.Second))
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%02dh", s/86400, s%86400/3600)
}

var versionNumRe = regexp.MustCompile(`\d+`)

// versionLess compares dotted versions numerically (2.1.9 < 2.1.28).
func versionLess(a, b string) bool {
	x, y := versionNumRe.FindAllString(a, -1), versionNumRe.FindAllString(b, -1)
	for i := 0; i < len(x) && i < len(y); i++ {
		p, _ := strconv.Atoi(x[i])
		q, _ := strconv.Atoi(y[i])
		if p != q {
			return p < q
		}
	}
	return len(x) < len(y)
}

// latestVersion is the newest Claude Code install in dir
// (~/.local/share/claude/versions — one entry per installed version), or ""
// when it can't be read. That is the newest *installed* version, not the
// newest released one.
func latestVersion(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	latest := ""
	for _, e := range ents {
		if latest == "" || versionLess(latest, e.Name()) {
			latest = e.Name()
		}
	}
	return latest
}

var shSafeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shQuote single-quotes s for a shell, unless it needs no quoting.
func shQuote(s string) string {
	if shSafeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// statusRow is one Claude pane in the --status table. Queued and Idle are
// negative when unknown (no transcript).
type statusRow struct {
	Window, Name, Status, RC, Version, Model string
	Queued                                   int
	Idle                                     time.Duration
	Resume                                   string
}

// collectStatus builds a row per Claude pane in the selected windows. It
// only reads: the process table, session files, transcripts, and each
// pane's visible screen.
func collectStatus(ctx context.Context, d SuspendDeps, sessArg, winArg string, all bool) ([]statusRow, error) {
	tb, err := d.Scan()
	if err != nil {
		return nil, err
	}
	wins, err := selectWindows(ctx, d.T, sessArg, winArg, all)
	if err != nil {
		return nil, err
	}
	var rows []statusRow
	for _, w := range wins {
		panes, err := windowPanes(ctx, d.T, w.Sess, w.Index)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", w.Sess, w.Index, err)
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
			proc, _ := tb.Get(claudePID)
			entry, _ := d.Reg.Entry(proc)
			row := statusRow{
				Window: fmt.Sprintf("%s:%d", w.Sess, w.Index), Name: entry.Name,
				Status: entry.Status, Version: entry.Version, Queued: -1, Idle: -1,
			}
			if screen, err := d.T.Run(ctx, fmt.Sprintf("capture-pane -e -p -t %s", tmuxctl.Quote(p[0]))); err == nil {
				row.RC = rcState(screen)
			} else {
				row.RC = "?"
			}
			if tr := resume.FindTranscript(d.ProjectsDir, sid); tr != "" {
				if a, ok := resume.ReadActivity(tr); ok {
					row.Model = a.Model
					row.Queued = a.Queued(entry.StartedAt)
					if ts, err := time.Parse(time.RFC3339Nano, a.LastTS); err == nil {
						row.Idle = d.Now().Sub(ts)
					}
				}
			}
			// `claude --resume` is project-scoped, so the command runs from
			// the process's launch directory.
			row.Resume = "claude --resume " + sid
			if entry.Cwd != "" {
				row.Resume = "cd " + shQuote(entry.Cwd) + " && " + row.Resume
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// clip shortens s to n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// RunClaudeStatus prints a compact table of the selected Claude panes and a
// summary line. It never sends a key to any pane.
func RunClaudeStatus(ctx context.Context, d SuspendDeps, sessArg, winArg string, all, showResume bool) error {
	rows, err := collectStatus(ctx, d, sessArg, winArg, all)
	if err != nil {
		return err
	}
	latest := latestVersion(d.VersionsDir)
	orDash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	table := [][]string{{"window", "name", "status", "rc", "version", "model", "queued", "idle"}}
	if showResume {
		table[0] = append(table[0], "resume")
	}
	old, rcDown, pending := 0, 0, 0
	for _, r := range rows {
		version := orDash(r.Version)
		if latest != "" && r.Version != "" && r.Version != latest {
			version += " OLD"
			old++
		}
		queued, idle := "?", "?"
		if r.Queued >= 0 {
			queued = "-"
			if r.Queued > 0 {
				queued = strconv.Itoa(r.Queued)
				pending++
			}
		}
		if r.Idle >= 0 {
			idle = ago(r.Idle)
		}
		if r.RC != "connected" {
			rcDown++
		}
		cells := []string{r.Window, orDash(clip(r.Name, 24)), orDash(r.Status), r.RC, version, modelLabel(r.Model), queued, idle}
		if showResume {
			cells = append(cells, r.Resume)
		}
		table = append(table, cells)
	}
	widths := make([]int, len(table[0]))
	for _, cells := range table {
		for i, c := range cells {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	for _, cells := range table {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(c + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)))
		}
		fmt.Fprintln(d.Out, strings.TrimRight(b.String(), " "))
	}
	fmt.Fprintf(d.Out, "\n%d sessions; latest installed claude %s (%d older); remote control not connected: %d; with queued messages: %d\n",
		len(rows), orDash(latest), old, rcDown, pending)
	return nil
}
