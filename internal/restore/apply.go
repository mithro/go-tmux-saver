package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// Report summarises what Apply actually did against a live tmux server.
// Created and Relocated count only creations that SUCCEEDED (RULING R28 —
// Apply measures real outcomes, it does not copy the Plan's structural
// counts); Skipped counts the plan's already-satisfied windows (its "note"
// actions, which never fail since they have no tmux side effect at all).
// Any action's failure is recorded in Notes.
type Report struct {
	Created, Relocated, Skipped int
	Notes                       []string
}

// Apply runs a Plan's Actions against t. WinPlaceholder ({{WIN}}) in a
// relocated window's actions is substituted with the index reported by that
// window's "new-window ... -P -F" reply, and stays in effect (unchanged)
// until the next relocation resolves it to a new value — see BuildPlan's doc
// comment for why every use of one relocation's placeholder is contiguous in
// the action list. Any other create (new-session, or a plain non-relocated
// new-window) clears a stale placeholder so it can never leak into a later
// block by accident.
//
// "contents" actions replay saved pane scrollback WITHOUT ever feeding it to
// the shell as input (RULING R26: load-buffer/paste-buffer types the bytes
// as keystrokes into a live interactive shell — every newline in the saved
// scrollback would execute the preceding line, and any "-e"-style escape
// sequence would be interpreted by readline). Instead, the bytes are written
// to <replayDir>/<paneKey>.txt (0600; replayDir is created 0700 as needed)
// and DISPLAYED via `send-keys -t <target> " cat '<path>'" Enter` — a single
// quoted key argument, so cat runs asynchronously in the pane exactly as if
// a user had typed it, with a leading space so history-ignore-space shells
// don't record it. The files are left in place; the caller (the CLI) owns
// cleaning up old replay directories.
//
// ctx is checked at the top of every loop iteration (FINDING 3): once it's
// done, Apply stops immediately and returns ctx.Err(), along with whatever
// Report it had accumulated so far.
//
// An error from an ordinary action is appended to Report.Notes and does not
// stop the rest of the plan — a later re-run of an up-to-date plan (built
// from the now-current live state) completes whatever didn't apply. The one
// exception is a failed session/window-creating command (new-session,
// new-window): since nothing downstream in that window's block can possibly
// succeed against a window that doesn't exist (and, for a relocation, its
// WinPlaceholder can't even be resolved), the remaining actions of that
// block are skipped — processing resumes at the next new-session/new-window
// action for a plain window failure. A failed new-session goes further: it
// aborts EVERY remaining action of that whole saved session (tracked by
// name, via Action.Session), not just its first window — every later
// new-window in that session targets a session that was never created, so
// there is nothing for any of them to succeed against either.
func Apply(ctx context.Context, t tmuxctl.Transport, p Plan, contents func(paneKey string) ([]byte, bool), replayDir string) (Report, error) {
	var report Report

	var winTarget string      // most recently resolved WinPlaceholder value, once a relocation has replied
	var abortedSession string // non-empty once a new-session has failed: skip the rest of that session
	windowAborted := false    // true while skipping the remainder of one failed window's block

	resolve := func(s string) string {
		if winTarget == "" {
			return s
		}
		return strings.ReplaceAll(s, WinPlaceholder, winTarget)
	}

	for _, a := range p.Actions {
		if err := ctx.Err(); err != nil {
			return report, err
		}

		if abortedSession != "" && a.Session == abortedSession {
			continue
		}

		switch a.Kind {
		case "note":
			report.Skipped++

		case "tmux":
			cmd := a.Args[0]
			sessionCreate := strings.HasPrefix(cmd, "new-session ")
			create := sessionCreate || strings.HasPrefix(cmd, "new-window ")
			if create {
				windowAborted = false
				if a.Note != "relocated" {
					winTarget = "" // never let a stale relocation target leak into a new block
				}
			} else if windowAborted {
				continue
			}

			resolved := resolve(cmd)
			lines, err := t.Run(ctx, resolved)
			if err != nil {
				report.Notes = append(report.Notes, fmt.Sprintf("%s: %v", resolved, err))
				if create {
					windowAborted = true
					if sessionCreate {
						abortedSession = a.Session
					}
				}
				continue
			}

			switch {
			case a.Note == "relocated":
				idx, ok := firstLine(lines)
				if !ok {
					report.Notes = append(report.Notes, fmt.Sprintf("%s: no window index in reply", resolved))
					windowAborted = true
					continue
				}
				winTarget = idx
				report.Relocated++
			case create:
				report.Created++
			}

		case "contents":
			if windowAborted {
				continue
			}
			target := resolve(a.Args[0])
			paneKey := a.Args[1]
			data, ok := contents(paneKey)
			if !ok {
				continue // nothing saved for this pane; not an error
			}
			path, err := writeReplayFile(replayDir, paneKey, data)
			if err != nil {
				report.Notes = append(report.Notes, fmt.Sprintf("contents %s: %v", target, err))
				continue
			}
			keys := " cat " + shellQuote([]string{path})
			// The target is data-derived (session/window names are arbitrary
			// user text), so it is quoted here rather than in BuildPlan: the
			// "contents" action carries a RAW target so WinPlaceholder
			// resolution above operates on plain text (C1/RULING R30).
			cmd := fmt.Sprintf("send-keys -t %s %s Enter", tmuxQuote(target), tmuxQuote(keys))
			if _, err := t.Run(ctx, cmd); err != nil {
				report.Notes = append(report.Notes, fmt.Sprintf("%s: %v", cmd, err))
			}
		}
	}

	return report, nil
}

// firstLine returns the first non-blank line of lines, verified to parse as
// an integer (tmux's "#{window_index}" reply).
func firstLine(lines []string) (string, bool) {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, err := strconv.Atoi(l); err != nil {
			return "", false
		}
		return l, true
	}
	return "", false
}

// writeReplayFile writes data to <replayDir>/<paneKey>.txt (0600), creating
// replayDir (0700) if needed, and returns the file's path.
func writeReplayFile(replayDir, paneKey string, data []byte) (string, error) {
	if err := os.MkdirAll(replayDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(replayDir, paneKey+".txt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
