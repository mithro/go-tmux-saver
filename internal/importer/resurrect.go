// Package importer converts a legacy tmux-resurrect save
// (tmux_resurrect_*.txt plus its sibling pane_contents.tar.gz) into a
// go-tmux-saver snapshot.Snapshot, so the first restore after switching from
// tmux-resurrect to go-tmux-saver has data to restore from. This is a
// one-time migration path, not an ongoing integration.
//
// # Observed tmux-resurrect save-file format
//
// Derived 2026-08-22 by reading tmux-resurrect's own save.sh/helpers.sh
// (~/.tmux/plugins/tmux-resurrect/scripts/) alongside a live save under
// ~/.local/share/tmux/resurrect/ on ten64. The file is tab-separated, one
// record per line, four line types:
//
//	pane\t<session>\t<window>\t<window_active 0|1>\t<window_flags>\t
//	    <pane_index>\t<pane_title>\t<dir>\t<pane_active 0|1>\t
//	    <pane_command>\t<full_command>
//	window\t<session>\t<window>\t<window_name>\t<window_active 0|1>\t
//	    <window_flags>\t<window_layout>\t<automatic_rename>
//	state\t<client_session>\t<client_last_session>
//	grouped_session\t<session>\t<original_session>\t<alt_window>\t<active_window>
//
// window_flags, dir, and full_command carry a literal leading ':' even when
// the value itself is empty — tmux-resurrect's convention (see pane_format/
// window_format in save.sh) for distinguishing "empty string" from "field
// absent" over a tab-separated line built by naive shell string
// concatenation. This importer strips exactly one leading ':' from those
// fields (window_name is also ':'-prefixed the same way). window_layout and
// pane_title are NOT ':'-prefixed.
//
// automatic_rename is "on", "off", or ":" when the window option was never
// set at the window level (show-window-options -v returned nothing, so
// save.sh substitutes ":" as a placeholder — see dump_windows in save.sh).
// Mirroring internal/collect's own on-live-tmux convention (which only ever
// treats a literal "on" as true), this importer maps "on" -> true and both
// "off" and ":" (unset) -> false; unset windows lose their inherited
// session/global automatic-rename state on import, a known simplification.
//
// grouped_session lines describe a session that mirrors another session's
// windows (tmux's session-group attach). tmux-resurrect's own dump_panes/
// dump_windows (is_session_grouped) never emit pane/window lines for such a
// session, so this importer's skipping of grouped_session lines loses
// nothing already absent from the pane/window lines.
//
// Lines that don't parse as one of the four known shapes (wrong tag, wrong
// field count) are skipped rather than treated as a hard error: real saves
// have been observed to contain stray malformed lines (e.g. a pane title
// that itself embedded a raw newline, splitting one logical record across
// two text lines) that a strict parser would otherwise choke the entire
// import on.
//
// # Pane contents tarball
//
// pane_contents.tar.gz holds one entry per pane with any captured
// scrollback, named (see pane_contents_file/pane_contents_create_archive in
// helpers.sh): "./pane_contents/pane-<session>:<window>.<pane>" — no file
// extension. tmux session names cannot contain ':', so splitting the
// basename on the LAST ':' (session vs. "<window>.<pane>") and then on the
// following '.' (window index vs. pane index, both plain decimal) is
// unambiguous. Panes with no on-screen/history content when the pane was
// saved (see pane_has_any_content in save.sh) have no tarball entry at all.
package importer

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// uuidPattern matches a lowercase-hex RFC 4122 UUID.
const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// resumeRegex builds the pattern used to recognize a "resume this Claude
// session" full_command: the literal "claude-resume" or "--resume" (as
// internal/procs.Resolve's own resumeRe does for live collection), plus —
// since this importer has no process tree to fall back on, only the
// full_command text — the basename of the configured claudeResumePath, in
// case the resume helper has been installed under a different name.
func resumeRegex(claudeResumePath string) *regexp.Regexp {
	names := []string{"claude-resume", "--resume"}
	if claudeResumePath != "" {
		if base := filepath.Base(expandTilde(claudeResumePath)); base != "" && base != "." && base != string(filepath.Separator) {
			seen := false
			for _, n := range names {
				if n == base {
					seen = true
					break
				}
			}
			if !seen {
				names = append(names, regexp.QuoteMeta(base))
			}
		}
	}
	pattern := `(?:^|[\s/])(?:` + strings.Join(names, "|") + `)\s+(` + uuidPattern + `)`
	return regexp.MustCompile(pattern)
}

// expandTilde expands a leading "~" or "~/" in p to the user's home
// directory (mirrors internal/cli's expandHome; duplicated here rather than
// exported cross-package for a single helper).
func expandTilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	return p
}

// classifyRestore maps a resurrect full_command (already stripped of its
// leading ':') to a snapshot.Restore per the task's restore-kind mapping:
// empty -> shell; a claude-resume/--resume invocation -> claude (with the
// recovered uuid); anything else -> argv, best-effort whitespace-split.
// full_command was captured by tmux-resurrect as a single shell string
// (its default "pane_current_command" strategy just re-quotes argv[0], but
// alternate strategies can hand back an arbitrary shell command line with
// quoting/redirection/pipes) — splitting it on whitespace is a deliberate
// best-effort limitation, not a shell parse: any quoting in the original
// command will end up wrong in the resulting Argv.
func classifyRestore(fullCommand string, resume *regexp.Regexp) snapshot.Restore {
	fullCommand = strings.TrimSpace(fullCommand)
	if fullCommand == "" {
		return snapshot.Restore{Kind: "shell"}
	}
	if m := resume.FindStringSubmatch(fullCommand); m != nil {
		return snapshot.Restore{Kind: "claude", ClaudeSession: m[1]}
	}
	return snapshot.Restore{Kind: "argv", Argv: strings.Fields(fullCommand)}
}

type winKey struct {
	session string
	index   int
}

// FromResurrect parses a tmux-resurrect save file at savePath (and,
// optionally, its sibling pane_contents.tar.gz at contentsTar — pass "" to
// skip contents entirely) into a go-tmux-saver snapshot.Snapshot plus a
// map of pane scrollback keyed by snapshot.PaneKey, ready for
// snapshot.Store.Stage. claudeResumePath is the configured
// claude-resume helper path (config.Config.ClaudeResumePath, tilde
// expanded or not — FromResurrect expands it itself); see resumeRegex.
func FromResurrect(savePath, contentsTar, claudeResumePath string) (*snapshot.Snapshot, map[string][]byte, error) {
	data, err := os.ReadFile(savePath)
	if err != nil {
		return nil, nil, fmt.Errorf("import-resurrect: read %s: %w", savePath, err)
	}
	resume := resumeRegex(claudeResumePath)

	var sessionOrder []string
	sessionSeen := map[string]bool{}
	windowOrder := map[string][]int{}
	windows := map[winKey]*snapshot.Window{}
	var clientSession string

	noteSession := func(name string) {
		if !sessionSeen[name] {
			sessionSeen[name] = true
			sessionOrder = append(sessionOrder, name)
		}
	}
	windowFor := func(session string, index int) *snapshot.Window {
		k := winKey{session, index}
		w, ok := windows[k]
		if !ok {
			w = &snapshot.Window{Index: index}
			windows[k] = w
			windowOrder[session] = append(windowOrder[session], index)
		}
		return w
	}

	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "grouped_session":
			continue // no pane/window lines were ever emitted for these (see package doc)

		case "state":
			if len(fields) != 3 {
				continue
			}
			clientSession = fields[1]

		case "pane":
			if len(fields) != 11 {
				continue
			}
			session := fields[1]
			winIdx, err := strconv.Atoi(fields[2])
			if err != nil {
				continue
			}
			paneIdx, err := strconv.Atoi(fields[5])
			if err != nil {
				continue
			}
			title := fields[6]
			dir := strings.TrimPrefix(fields[7], ":")
			active := fields[8] == "1"
			fullCommand := strings.TrimPrefix(fields[10], ":")

			noteSession(session)
			w := windowFor(session, winIdx)
			w.Panes = append(w.Panes, snapshot.Pane{
				Index:   paneIdx,
				Cwd:     dir,
				Title:   title,
				Active:  active,
				Restore: classifyRestore(fullCommand, resume),
			})

		case "window":
			if len(fields) != 8 {
				continue
			}
			session := fields[1]
			winIdx, err := strconv.Atoi(fields[2])
			if err != nil {
				continue
			}
			name := strings.TrimPrefix(fields[3], ":")
			active := fields[4] == "1"
			flags := strings.TrimPrefix(fields[5], ":")
			layout := fields[6]
			autoRename := fields[7] == "on"

			noteSession(session)
			w := windowFor(session, winIdx)
			w.Name = name
			w.Active = active
			w.Flags = flags
			w.Layout = layout
			w.AutomaticRename = autoRename

		default:
			continue
		}
	}

	sort.Strings(sessionOrder)
	sessions := make([]snapshot.Session, 0, len(sessionOrder))
	for _, name := range sessionOrder {
		se := snapshot.Session{Name: name}
		idxs := append([]int(nil), windowOrder[name]...)
		sort.Ints(idxs)
		for _, idx := range idxs {
			w := windows[winKey{name, idx}]
			sort.Slice(w.Panes, func(i, j int) bool { return w.Panes[i].Index < w.Panes[j].Index })
			if w.Active {
				se.ActiveWindow = w.Index
			}
			se.Windows = append(se.Windows, *w)
		}
		sessions = append(sessions, se)
	}

	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	snap := &snapshot.Snapshot{
		Schema:      snapshot.SchemaVersion,
		Host:        host,
		TmuxVersion: "imported-resurrect",
		TakenAt:     time.Now(),
		Sessions:    sessions,
		Client:      snapshot.ClientState{Session: clientSession},
	}
	panes, windowCount := snap.CountPanes()
	snap.Stats = snapshot.Stats{Panes: panes, Windows: windowCount, Sessions: len(sessions)}

	contents := map[string][]byte{}
	if contentsTar != "" {
		contents, err = readPaneContents(contentsTar)
		if err != nil {
			return nil, nil, err
		}
	}

	return snap, contents, nil
}

// readPaneContents reads a tmux-resurrect pane_contents.tar.gz and returns
// its per-pane scrollback keyed by snapshot.PaneKey (see the package doc
// for the tar entry naming this parses).
func readPaneContents(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("import-resurrect: open %s: %w", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("import-resurrect: %s: %w", path, err)
	}
	defer gz.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("import-resurrect: %s: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		session, window, pane, ok := parsePaneEntryName(filepath.Base(hdr.Name))
		if !ok {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("import-resurrect: %s: %s: %w", path, hdr.Name, err)
		}
		out[snapshot.PaneKey(session, window, pane)] = data
	}
	return out, nil
}

// parsePaneEntryName parses a pane_contents tar entry's basename
// ("pane-<session>:<window>.<pane>") into its session/window/pane parts.
func parsePaneEntryName(base string) (session string, window, pane int, ok bool) {
	rest, ok := strings.CutPrefix(base, "pane-")
	if !ok {
		return "", 0, 0, false
	}
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return "", 0, 0, false
	}
	session = rest[:colon]
	winPane := rest[colon+1:]
	dot := strings.LastIndex(winPane, ".")
	if dot < 0 {
		return "", 0, 0, false
	}
	w, err := strconv.Atoi(winPane[:dot])
	if err != nil {
		return "", 0, 0, false
	}
	p, err := strconv.Atoi(winPane[dot+1:])
	if err != nil {
		return "", 0, 0, false
	}
	return session, w, p, true
}
