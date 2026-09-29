// Package resume implements the built-in claude-resume placeholder: the
// command a restore types into each saved Claude pane. Rather than
// relaunching Claude blindly, it shows WHICH conversation the pane held and
// waits for confirmation — so restoring N panes doesn't stampede N Claude
// processes at once. A faithful port of the rcfiles ~/bin/claude-resume
// script (same UX, same semantics), so no external script is needed on
// rollout hosts.
package resume

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var uuidRe = regexp.MustCompile(`\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z`)

// Munge mirrors Claude Code's project-dir naming: '/' and '.' become '-'.
func Munge(path string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(path)
}

// FindTranscript locates <projectsDir>/<munged-cwd>/<sid>.jsonl by glob, so
// the (possibly worktree) launch directory doesn't have to be known.
func FindTranscript(projectsDir, sid string) string {
	hits, _ := filepath.Glob(filepath.Join(projectsDir, "*", sid+".jsonl"))
	if len(hits) == 0 {
		return ""
	}
	return hits[0]
}

// Meta is the short human context pulled from a session transcript.
type Meta struct {
	Summary, Title, FirstUser  string
	LaunchCwd, WorkCwd, Branch string
	LastTS                     string
}

// ReadMeta scans a transcript's JSONL for a label and the launch cwd.
// launch cwd = first cwd seen (its munge names the project folder — the
// directory to resume from); work cwd = last cwd (often a worktree).
func ReadMeta(path string) (Meta, bool) {
	var m Meta
	f, err := os.Open(path)
	if err != nil {
		return m, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var o struct {
			Type        string `json:"type"`
			Cwd         string `json:"cwd"`
			GitBranch   string `json:"gitBranch"`
			Timestamp   string `json:"timestamp"`
			Summary     string `json:"summary"`
			AiTitle     string `json:"aiTitle"`
			CustomTitle string `json:"customTitle"`
			Message     struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &o) != nil {
			continue
		}
		if o.Cwd != "" {
			if m.LaunchCwd == "" {
				m.LaunchCwd = o.Cwd
			}
			m.WorkCwd = o.Cwd
		}
		if o.GitBranch != "" {
			m.Branch = o.GitBranch
		}
		if o.Timestamp != "" && (o.Type == "user" || o.Type == "assistant") {
			m.LastTS = o.Timestamp
		}
		switch {
		case o.Type == "summary" && o.Summary != "":
			m.Summary = o.Summary
		case o.Type == "ai-title" && o.AiTitle != "":
			m.Title = o.AiTitle
		case o.Type == "custom-title" && o.CustomTitle != "":
			m.Title = o.CustomTitle
		case o.Type == "user" && m.FirstUser == "":
			m.FirstUser = firstUserText(o.Message.Content)
		}
	}
	return m, true
}

// firstUserText extracts the first plain-text chunk of a user message:
// either a bare string (ignored when it looks like markup) or the first
// {"type":"text"} part of a content list.
func firstUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.HasPrefix(strings.TrimSpace(s), "<") {
			return ""
		}
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" {
				return p.Text
			}
		}
	}
	return ""
}

// Label is the best one-line description: title > rolling summary > first
// user prompt, whitespace-collapsed and clipped to 200 runes.
func (m Meta) Label() string {
	text := m.Title
	if text == "" {
		text = m.Summary
	}
	if text == "" {
		text = m.FirstUser
	}
	if text == "" {
		text = "(no summary found)"
	}
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return text
}

func shortenHome(home, p string) string {
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// ChdirTarget returns the directory to resume from, or "". `claude
// --resume` is project-scoped: it resolves the id against the project
// matching the CURRENT directory's munged name, so the pane must cd back to
// the launch cwd (when it still exists and really is the transcript's
// project) instead of trusting wherever the restore recreated the pane.
func ChdirTarget(m Meta, transcript string) string {
	cwd := m.LaunchCwd
	if cwd == "" || transcript == "" {
		return ""
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return ""
	}
	if Munge(cwd) != filepath.Base(filepath.Dir(transcript)) {
		return ""
	}
	return cwd
}

// Render writes the placeholder banner. tty enables ANSI styling.
func Render(w io.Writer, home, sid string, meta *Meta, tty bool) {
	b, d, c, r := "", "", "", ""
	if tty {
		b, d, c, r = "\033[1m", "\033[2m", "\033[36m", "\033[0m"
	}
	if sid == "" {
		fmt.Fprintf(w, "\n%sResume Claude%s %s(no session id — picker)%s\n", b, r, d, r)
		fmt.Fprintf(w, "%s  Enter = open the resume picker · Ctrl-C = shell%s\n\n", d, r)
		return
	}
	fmt.Fprintf(w, "\n%sResume Claude session%s  %s%s%s%s…%s\n", b, r, c, sid[:8], r, d, r)
	if meta != nil {
		loc := shortenHome(home, meta.LaunchCwd)
		if meta.Branch != "" {
			if loc != "" {
				loc = fmt.Sprintf("%s  %s@ %s%s", loc, d, meta.Branch, r)
			} else {
				loc = meta.Branch
			}
		}
		if loc != "" {
			fmt.Fprintf(w, "  %s\n", loc)
		}
		if meta.WorkCwd != "" && meta.WorkCwd != meta.LaunchCwd {
			fmt.Fprintf(w, "  %s↳ worktree %s%s\n", d, shortenHome(home, meta.WorkCwd), r)
		}
		fmt.Fprintf(w, "  %s“%s%s%s”%s\n", d, r, meta.Label(), d, r)
		if meta.LastTS != "" {
			fmt.Fprintf(w, "  %slast active %s%s\n", d, meta.LastTS, r)
		}
	} else {
		fmt.Fprintf(w, "  %s(transcript not found — will still try to resume)%s\n", d, r)
	}
	fmt.Fprintf(w, "%s  Enter = resume · Ctrl-C = shell%s\n\n", d, r)
}

// Decision is what the placeholder resolved to: exec Argv (from Chdir when
// non-empty), or Skip back to the pane's shell.
type Decision struct {
	Argv  []string
	Chdir string
	Skip  bool
}

// Decide runs the whole placeholder flow against injected I/O: render the
// banner, wait for the confirm keypress (readLine returns an error on
// Ctrl-C/Ctrl-D → skip), announce the choice, and return what to exec.
// When stdin is not a tty (a send-keys restore — no human to wait on) it
// announces and resumes immediately rather than block. Either way a visible
// line records the choice, so a restored pane never silently execs claude.
func Decide(w io.Writer, home, projectsDir, sid string, stdoutTTY, stdinTTY bool, readLine func() (string, error)) Decision {
	sid = strings.TrimSpace(sid)
	var meta *Meta
	transcript := ""
	argv := []string{"claude"}
	if uuidRe.MatchString(sid) {
		transcript = FindTranscript(projectsDir, sid)
		if transcript != "" {
			if m, ok := ReadMeta(transcript); ok {
				meta = &m
			}
		}
		argv = []string{"claude", "--resume", sid}
	} else {
		sid = "" // junk that isn't a uuid → plain claude (resume picker)
	}

	Render(w, home, sid, meta, stdoutTTY)

	d, grn, r := "", "", ""
	if stdoutTTY {
		d, grn, r = "\033[2m", "\033[32m", "\033[0m"
	}
	if stdinTTY {
		if _, err := readLine(); err != nil {
			fmt.Fprintf(w, "%s↩ skipped — shell ready%s\n", d, r)
			return Decision{Skip: true}
		}
	}
	if sid != "" {
		fmt.Fprintf(w, "%s↳ resuming%s %s%s…%s\n", grn, r, d, sid[:8], r)
	} else {
		fmt.Fprintf(w, "%s↳ opening resume picker%s%s…%s\n", grn, r, d, r)
	}
	chdir := ""
	if meta != nil {
		chdir = ChdirTarget(*meta, transcript)
	}
	return Decision{Argv: argv, Chdir: chdir}
}

// TailLines returns the last n lines of data (n <= 0 returns data
// unchanged) — the placeholder prints only the tail of a pane's saved
// scrollback, enough to reproduce the visible console state without
// replaying megabytes of history.
func TailLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	seen := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			seen++
			if seen == n {
				return data[i+1:]
			}
		}
	}
	return data
}
