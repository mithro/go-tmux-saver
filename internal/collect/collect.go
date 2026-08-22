// Package collect builds a snapshot from a live tmux server.
package collect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
	"github.com/mithro/go-tmux-saver/internal/trace"
)

const (
	SessCmd   = "list-sessions -F \"#{session_name}\t#{session_grouped}\t#{session_attached}\""
	WinCmd    = "list-windows -a -F \"#{session_name}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_flags}\t#{window_layout}\t#{automatic-rename}\""
	PaneCmd   = "list-panes -a -F \"#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_id}\t#{pane_active}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}\t#{history_size}\""
	ServerCmd = "display-message -p \"#{start_time}\t#{version}\t#{client_session}\""
)

// Collector builds a snapshot.Snapshot and per-pane contents by issuing
// list-sessions/list-windows/list-panes/capture-pane over a single
// tmuxctl.Transport (a live control-mode connection or a Fake in tests).
type Collector struct {
	T         tmuxctl.Transport
	Procs     *procs.Table
	Reg       procs.ClaudeRegistry
	Allowlist []string
	Host      string
	Now       func() time.Time
}

// splitTail peels exactly nTail trailing tab-separated fields off the right
// end of line, working right-to-left via strings.LastIndexByte, and returns
// the remaining left-hand "head" plus the tail fields in original
// left-to-right order. ok is false if line has fewer than nTail tabs (i.e.
// fewer than nTail+1 total fields), in which case head/tail are unusable.
//
// This lets a free-text field (pane_title, window_name, ...) that sits to
// the left of a run of fixed-format trailing fields absorb any tab
// characters embedded in it: peel the known-safe tail off the right first,
// then split what's left with strings.SplitN so the leftmost free-text
// field soaks up whatever tabs remain.
func splitTail(line string, nTail int) (head string, tail []string, ok bool) {
	tail = make([]string, nTail)
	rest := line
	for i := nTail - 1; i >= 0; i-- {
		idx := strings.LastIndexByte(rest, '\t')
		if idx < 0 {
			return "", nil, false
		}
		tail[i] = rest[idx+1:]
		rest = rest[:idx]
	}
	return rest, tail, true
}

func (c *Collector) Collect(ctx context.Context) (*snapshot.Snapshot, map[string][]byte, error) {
	start := time.Now()
	now := c.Now
	if now == nil {
		now = time.Now
	}
	snap := &snapshot.Snapshot{Schema: snapshot.SchemaVersion, Host: c.Host, TakenAt: now().UTC()}

	stop := trace.Time("collect.server")
	lines, err := c.T.Run(ctx, ServerCmd)
	stop()
	if err != nil {
		return nil, nil, fmt.Errorf("server info: %w", err)
	}
	if len(lines) == 1 {
		f := strings.Split(lines[0], "\t")
		if len(f) == 3 {
			snap.ServerStart, _ = strconv.ParseInt(f[0], 10, 64)
			snap.TmuxVersion = f[1]
			snap.Client.Session = f[2]
		}
	}

	stop = trace.Time("collect.list-sessions")
	sessLines, err := c.T.Run(ctx, SessCmd)
	stop()
	if err != nil {
		return nil, nil, fmt.Errorf("list-sessions: %w", err)
	}
	sessions := map[string]*snapshot.Session{}
	var order []string
	for _, l := range sessLines {
		// session_name \t session_grouped \t session_attached
		name, tail, ok := splitTail(l, 2)
		if !ok {
			return nil, nil, fmt.Errorf("malformed list-sessions line: %q", l)
		}
		grouped := tail[0]
		if grouped == "1" {
			continue // grouped clone
		}
		sessions[name] = &snapshot.Session{Name: name}
		order = append(order, name)
	}
	sort.Strings(order)

	stop = trace.Time("collect.list-windows")
	winLines, err := c.T.Run(ctx, WinCmd)
	stop()
	if err != nil {
		return nil, nil, fmt.Errorf("list-windows: %w", err)
	}
	// Pass 1: append every window to its session, recording where it landed.
	winPos := map[string]int{} // "session\tindex" -> position in se.Windows
	for _, l := range winLines {
		// head = session_name \t window_index \t window_name (window_name
		// absorbs any embedded tabs); tail = window_active, window_flags,
		// window_layout, automatic-rename.
		head, tail, ok := splitTail(l, 4)
		if !ok {
			return nil, nil, fmt.Errorf("malformed list-windows line: %q", l)
		}
		f := strings.SplitN(head, "\t", 3)
		if len(f) != 3 {
			return nil, nil, fmt.Errorf("malformed list-windows line: %q", l)
		}
		session, winIdxStr, name := f[0], f[1], f[2]
		active, flags, layout, autoRename := tail[0], tail[1], tail[2], tail[3]

		se, ok := sessions[session]
		if !ok {
			continue // grouped clone or unknown session
		}
		idx, err := strconv.Atoi(winIdxStr)
		if err != nil {
			return nil, nil, fmt.Errorf("malformed list-windows line (window_index): %q", l)
		}
		w := snapshot.Window{Index: idx, Name: name, Active: active == "1", Flags: flags, Layout: layout, AutomaticRename: autoRename == "on"}
		if w.Active {
			se.ActiveWindow = idx
		}
		se.Windows = append(se.Windows, w)
		winPos[session+"\t"+winIdxStr] = len(se.Windows) - 1
	}

	stop = trace.Time("collect.list-panes")
	paneLines, err := c.T.Run(ctx, PaneCmd)
	stop()
	if err != nil {
		return nil, nil, fmt.Errorf("list-panes: %w", err)
	}
	type capture struct {
		key, id string
		hist    int
	}
	var caps []capture
	var resolveTotal time.Duration
	// Pass 2: no further appends happen to any se.Windows slice, so taking
	// &se.Windows[pos] here is safe and stable for the rest of Collect.
	for _, l := range paneLines {
		// head = session_name, window_index, pane_index, pane_id,
		// pane_active, pane_pid, pane_current_path, pane_title (pane_title
		// absorbs any embedded tabs, since it is last of the 8); tail =
		// history_size. Note: pane_current_path comes before pane_title, so
		// a tab embedded in the path (not the title) would misparse — this
		// is accepted per the task brief, since paths practically never
		// contain tabs and title is the field most likely to carry
		// arbitrary user text (e.g. unicode status glyphs, command lines).
		head, tail, ok := splitTail(l, 1)
		if !ok {
			return nil, nil, fmt.Errorf("malformed list-panes line: %q", l)
		}
		f := strings.SplitN(head, "\t", 8)
		if len(f) != 8 {
			return nil, nil, fmt.Errorf("malformed list-panes line: %q", l)
		}
		session, winIdxStr, paneIdxStr, paneID, paneActive, panePIDStr, cwd, title := f[0], f[1], f[2], f[3], f[4], f[5], f[6], f[7]
		histStr := tail[0]

		se, ok := sessions[session]
		if !ok {
			continue // grouped clone or unknown session
		}
		pos, ok := winPos[session+"\t"+winIdxStr]
		if !ok {
			continue
		}
		w := &se.Windows[pos]
		idx, err := strconv.Atoi(paneIdxStr)
		if err != nil {
			return nil, nil, fmt.Errorf("malformed list-panes line (pane_index): %q", l)
		}
		pid, err := strconv.Atoi(panePIDStr)
		if err != nil {
			return nil, nil, fmt.Errorf("malformed list-panes line (pane_pid): %q", l)
		}
		hist, err := strconv.Atoi(histStr)
		if err != nil {
			return nil, nil, fmt.Errorf("malformed list-panes line (history_size): %q", l)
		}
		rs := time.Now()
		restore := procs.Resolve(c.Procs, c.Reg, pid, c.Allowlist)
		resolveTotal += time.Since(rs)
		p := snapshot.Pane{
			Index:        idx,
			ID:           paneID,
			Active:       paneActive == "1",
			Cwd:          cwd,
			Title:        title,
			HistoryLines: hist,
			Restore:      restore,
		}
		w.Panes = append(w.Panes, p)
		caps = append(caps, capture{snapshot.PaneKey(session, w.Index, idx), paneID, hist})
	}

	trace.Logf("%-22s %v (%d panes)", "collect.resolve", resolveTotal, len(caps))

	contents := map[string][]byte{}
	var capTotal, capMax time.Duration
	capBytes, capLines := 0, 0
	for _, cp := range caps {
		cs := time.Now()
		lines, err := c.T.Run(ctx, fmt.Sprintf("capture-pane -epJ -S -%d -t %s", cp.hist, cp.id))
		if err != nil {
			return nil, nil, fmt.Errorf("capture %s: %w", cp.id, err)
		}
		body := []byte(strings.Join(lines, "\n") + "\n")
		contents[cp.key] = body
		if trace.Enabled {
			d := time.Since(cs)
			capTotal += d
			if d > capMax {
				capMax = d
			}
			capBytes += len(body)
			capLines += len(lines)
		}
	}
	trace.Logf("%-22s %v (n=%d max=%v lines=%d bytes=%d)", "collect.capture-pane", capTotal, len(caps), capMax, capLines, capBytes)

	for _, name := range order {
		se := sessions[name]
		sort.Slice(se.Windows, func(i, j int) bool { return se.Windows[i].Index < se.Windows[j].Index })
		for i := range se.Windows {
			ps := se.Windows[i].Panes
			sort.Slice(ps, func(a, b int) bool { return ps[a].Index < ps[b].Index })
		}
		snap.Sessions = append(snap.Sessions, *se)
	}

	p, w := snap.CountPanes()
	snap.Stats = snapshot.Stats{Panes: p, Windows: w, Sessions: len(snap.Sessions), DurationMS: time.Since(start).Milliseconds()}
	return snap, contents, nil
}
