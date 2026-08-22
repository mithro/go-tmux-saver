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
// nothing already absent from the pane/window lines. Unlike pane/window/
// state lines, a malformed grouped_session line never produces a warning —
// its fields are never read regardless of shape.
//
// pane_title and window_name are free text and CAN contain embedded tab
// characters (tmux allows arbitrary bytes there); every other field in a
// pane/window line is tmux-resurrect-controlled and tab-free. So pane and
// window lines are parsed right-to-left: the trailing fixed-shape fields
// are peeled off from the end first (splitTail, mirroring internal/collect's
// own helper of the same name — copied here rather than imported across
// packages for one tiny function), leaving a head whose last field (title
// or name) is then taken as everything remaining via a bounded
// strings.SplitN, so it safely absorbs any embedded tabs. The one field this
// does NOT protect is full_command (the very last field of a pane line): a
// tab embedded there still misparses the line, but the resulting field-count
// mismatch is caught by the same validation used for any other malformed
// line and turned into a warning (see below) rather than silently
// corrupting a later field.
//
// Lines that don't parse as one of the four known shapes — unrecognized
// line-type tag, wrong field count, or an index field that isn't a valid
// integer — are skipped rather than treated as a hard error (real saves
// have been observed to contain stray malformed lines, e.g. from a pane
// title that embedded a raw newline rather than a tab, splitting one
// logical record across two text lines), but each skip is recorded as a
// warning ("line N: <reason> (<up to 60 chars of the line>)") returned
// alongside the snapshot, so a best-effort import is never silent about
// what it dropped.
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
// A tar entry whose name doesn't parse, or that parses but names a
// session/window/pane not present in the save file's own pane/window
// lines, also produces a warning rather than being silently dropped or
// (worse) silently added under a synthetic key nothing else references.
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

// splitTail peels the last nTail tab-delimited fields off the right end of
// line, returning everything before them as head. Mirrors
// internal/collect.splitTail (copied rather than imported across packages
// for one tiny helper): as long as the nTail trailing fields themselves
// never contain a tab, this correctly isolates them regardless of how many
// tabs are embedded earlier in the line (e.g. in a pane title or window
// name) — the split works from the right, so embedded tabs earlier in the
// line just end up inside head, to be resolved by a bounded SplitN there.
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

// warningExcerptLimit bounds how much of a skipped line is echoed back in
// its warning, so one absurdly long line can't blow out the warning list.
const warningExcerptLimit = 60

func appendWarning(warnings *[]string, lineNo int, line, reason string) {
	excerpt := line
	if len(excerpt) > warningExcerptLimit {
		excerpt = excerpt[:warningExcerptLimit]
	}
	*warnings = append(*warnings, fmt.Sprintf("line %d: %s (%s)", lineNo, reason, excerpt))
}

// FromResurrect parses a tmux-resurrect save file at savePath (and,
// optionally, its sibling pane_contents.tar.gz at contentsTar — pass "" to
// skip contents entirely) into a go-tmux-saver snapshot.Snapshot plus a map
// of pane scrollback keyed by snapshot.PaneKey, ready for
// snapshot.Store.Stage. claudeResumePath is the configured claude-resume
// helper path (config.Config.ClaudeResumePath, tilde expanded or not —
// FromResurrect expands it itself); see resumeRegex.
//
// The returned warnings list one entry per skipped/malformed save-file line
// and per pane_contents.tar.gz entry that couldn't be matched to a pane
// (see the package doc) — this is a best-effort import: a malformed line or
// stray tar entry never fails the whole conversion, but every one is
// reported so nothing is silently dropped.
func FromResurrect(savePath, contentsTar, claudeResumePath string) (*snapshot.Snapshot, map[string][]byte, []string, error) {
	data, err := os.ReadFile(savePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("import-resurrect: read %s: %w", savePath, err)
	}
	resume := resumeRegex(claudeResumePath)

	var warnings []string
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

	for i, line := range strings.Split(string(data), "\n") {
		lineNo := i + 1
		if line == "" {
			continue
		}
		tag, _, _ := strings.Cut(line, "\t")

		switch tag {
		case "grouped_session":
			continue // no pane/window lines were ever emitted for these (see package doc); never warned

		case "state":
			fields := strings.Split(line, "\t")
			if len(fields) != 3 {
				appendWarning(&warnings, lineNo, line, fmt.Sprintf("state line has %d fields, want 3", len(fields)))
				continue
			}
			clientSession = fields[1]

		case "pane":
			// Trailing 4 fields (dir, pane_active, pane_command,
			// full_command) are tab-free; peeling them off the right first
			// lets pane_title (the last field of head) absorb any embedded
			// tabs via the bounded SplitN below.
			head, tail, ok := splitTail(line, 4)
			if !ok {
				appendWarning(&warnings, lineNo, line, "pane line has too few fields")
				continue
			}
			pieces := strings.SplitN(head, "\t", 7) // tag, session, window, window_active, window_flags, pane_index, pane_title
			if len(pieces) != 7 {
				appendWarning(&warnings, lineNo, line, "pane line has too few fields")
				continue
			}
			session := pieces[1]
			winIdx, err := strconv.Atoi(pieces[2])
			if err != nil {
				appendWarning(&warnings, lineNo, line, fmt.Sprintf("bad window index %q", pieces[2]))
				continue
			}
			paneIdx, err := strconv.Atoi(pieces[5])
			if err != nil {
				appendWarning(&warnings, lineNo, line, fmt.Sprintf("bad pane index %q", pieces[5]))
				continue
			}
			title := pieces[6]
			dir := strings.TrimPrefix(tail[0], ":")
			active := tail[1] == "1"
			fullCommand := strings.TrimPrefix(tail[3], ":")

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
			// Trailing 4 fields (window_active, window_flags,
			// window_layout, automatic_rename) are tab-free; peeling them
			// off the right first lets window_name (the last field of
			// head) absorb any embedded tabs via the bounded SplitN below.
			head, tail, ok := splitTail(line, 4)
			if !ok {
				appendWarning(&warnings, lineNo, line, "window line has too few fields")
				continue
			}
			pieces := strings.SplitN(head, "\t", 4) // tag, session, window, window_name
			if len(pieces) != 4 {
				appendWarning(&warnings, lineNo, line, "window line has too few fields")
				continue
			}
			session := pieces[1]
			winIdx, err := strconv.Atoi(pieces[2])
			if err != nil {
				appendWarning(&warnings, lineNo, line, fmt.Sprintf("bad window index %q", pieces[2]))
				continue
			}
			name := strings.TrimPrefix(pieces[3], ":")
			active := tail[0] == "1"
			flags := strings.TrimPrefix(tail[1], ":")
			layout := tail[2]
			autoRename := tail[3] == "on"

			noteSession(session)
			w := windowFor(session, winIdx)
			w.Name = name
			w.Active = active
			w.Flags = flags
			w.Layout = layout
			w.AutomaticRename = autoRename

		default:
			appendWarning(&warnings, lineNo, line, "unrecognized line type")
		}
	}

	sort.Strings(sessionOrder)
	sessions := make([]snapshot.Session, 0, len(sessionOrder))
	validKeys := map[string]bool{}
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
			for _, p := range w.Panes {
				validKeys[snapshot.PaneKey(name, w.Index, p.Index)] = true
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
		var contentWarnings []string
		contents, contentWarnings, err = readPaneContents(contentsTar, validKeys)
		if err != nil {
			return nil, nil, nil, err
		}
		warnings = append(warnings, contentWarnings...)
	}

	return snap, contents, warnings, nil
}

// readPaneContents reads a tmux-resurrect pane_contents.tar.gz and returns
// its per-pane scrollback keyed by snapshot.PaneKey (see the package doc
// for the tar entry naming this parses), plus a warning for every entry
// whose name doesn't parse or that names a pane not in valid.
func readPaneContents(path string, valid map[string]bool) (map[string][]byte, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("import-resurrect: open %s: %w", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("import-resurrect: %s: %w", path, err)
	}
	defer gz.Close()

	out := map[string][]byte{}
	var warnings []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("import-resurrect: %s: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		session, window, pane, ok := parsePaneEntryName(filepath.Base(hdr.Name))
		if !ok {
			warnings = append(warnings, fmt.Sprintf("tar entry %q: cannot parse pane path", hdr.Name))
			continue
		}
		key := snapshot.PaneKey(session, window, pane)
		if !valid[key] {
			warnings = append(warnings, fmt.Sprintf("tar entry %q: no matching pane (session=%q window=%d pane=%d)", hdr.Name, session, window, pane))
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, fmt.Errorf("import-resurrect: %s: %s: %w", path, hdr.Name, err)
		}
		out[key] = data
	}
	return out, warnings, nil
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
