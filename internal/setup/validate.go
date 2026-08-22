package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/config"
)

// Drift describes one way an installed file or the live systemd/tmux state
// disagrees with what Render(p) says it should be.
type Drift struct {
	Path string // a Rel path for file-level drift, a unit name for unit-level drift
	Kind string // missing | differs | mode | unit-inactive | unit-invalid | dropin-missing | keybinding-missing
	Diff string
}

// Validate checks files (as previously Install/Update-ed under
// env.ConfigHome) against the live system: every managed file exists, is
// byte-identical to files (except config.json, which is only parsed and
// Validate()'d, never content-diffed — it's user-editable), and has the
// right mode; both timers are systemd-enabled and active; the units all
// pass `systemd-analyze --user verify`; the tmux-server.service drop-in is
// actually taking effect (its ExecStartPost shows up live); and the tmux
// M-s/M-r key bindings are bound. It returns one Drift per problem found,
// in a deterministic order (files in files' order, then timers, then
// verify, then the drop-in, then key bindings).
func Validate(env Env, files []Managed) []Drift {
	var drifts []Drift

	for _, f := range files {
		if d, ok := validateFile(env, f); ok {
			drifts = append(drifts, d...)
		}
	}

	for _, unit := range timerUnits {
		if d, ok := validateTimerState(env, unit); ok {
			drifts = append(drifts, d)
		}
	}

	if d, ok := validateUnitsVerify(env, files); ok {
		drifts = append(drifts, d)
	}

	if d, ok := validateDropinEffective(env); ok {
		drifts = append(drifts, d)
	}

	if d, ok := validateKeyBindings(env); ok {
		drifts = append(drifts, d)
	}

	return drifts
}

// validateFile checks one managed file's existence, content (except
// config.json, which is parsed+Validate()'d instead), and mode. It can
// return more than one Drift (e.g. wrong content AND wrong mode).
func validateFile(env Env, f Managed) ([]Drift, bool) {
	data, mode, err := readManagedFile(env, f)
	if err != nil {
		return []Drift{{Path: f.Rel, Kind: "missing", Diff: err.Error()}}, true
	}

	var drifts []Drift
	if f.Rel == RelConfigJSON {
		var cfg config.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			drifts = append(drifts, Drift{Path: f.Rel, Kind: "differs", Diff: "parse config.json: " + err.Error()})
		} else if err := cfg.Validate(); err != nil {
			drifts = append(drifts, Drift{Path: f.Rel, Kind: "differs", Diff: "validate config.json: " + err.Error()})
		}
	} else if !bytes.Equal(data, f.Content) {
		drifts = append(drifts, Drift{Path: f.Rel, Kind: "differs", Diff: diffLines(string(f.Content), string(data))})
	}

	if mode != f.Mode.Perm() {
		drifts = append(drifts, Drift{
			Path: f.Rel, Kind: "mode",
			Diff: fmt.Sprintf("mode is %04o, want %04o", mode, f.Mode.Perm()),
		})
	}

	return drifts, len(drifts) > 0
}

func validateTimerState(env Env, unit string) (Drift, bool) {
	var problems []string
	if out, err := env.Systemctl("--user", "is-enabled", unit); err != nil || strings.TrimSpace(out) != "enabled" {
		problems = append(problems, fmt.Sprintf("is-enabled=%q err=%v", strings.TrimSpace(out), err))
	}
	if out, err := env.Systemctl("--user", "is-active", unit); err != nil || strings.TrimSpace(out) != "active" {
		problems = append(problems, fmt.Sprintf("is-active=%q err=%v", strings.TrimSpace(out), err))
	}
	if len(problems) == 0 {
		return Drift{}, false
	}
	return Drift{Path: unit, Kind: "unit-inactive", Diff: strings.Join(problems, "; ")}, true
}

// validateUnitsVerify runs `systemd-analyze --user verify` (via
// env.Systemctl, see Env's doc comment) against every rendered unit file
// (services and timers; the tmux-server.service.d drop-in is a fragment,
// not a standalone unit, so it's excluded).
func validateUnitsVerify(env Env, files []Managed) (Drift, bool) {
	var unitPaths []string
	for _, f := range files {
		if strings.HasPrefix(f.Rel, "systemd/user/") && !strings.Contains(f.Rel, ".service.d/") {
			unitPaths = append(unitPaths, filepath.Join(env.ConfigHome, f.Rel))
		}
	}
	if len(unitPaths) == 0 {
		return Drift{}, false
	}
	args := append([]string{"--user", "verify"}, unitPaths...)
	out, err := env.Systemctl(args...)
	if err == nil {
		return Drift{}, false
	}
	return Drift{Path: "systemd/user", Kind: "unit-invalid", Diff: strings.TrimSpace(out + " " + err.Error())}, true
}

// validateDropinEffective checks that systemd's live view of
// tmux-server.service actually shows the drop-in's ExecStartPost, i.e. that
// the drop-in file on disk is the one systemd is using — not just that the
// file exists (that's validateFile's "missing"/"differs" job).
func validateDropinEffective(env Env) (Drift, bool) {
	out, err := env.Systemctl("--user", "show", "tmux-server.service", "-p", "ExecStartPost")
	if err == nil && strings.Contains(out, "restore --on-start") {
		return Drift{}, false
	}
	diff := strings.TrimSpace(out)
	if err != nil {
		diff = strings.TrimSpace(diff + " " + err.Error())
	}
	return Drift{Path: RelTmuxDropin, Kind: "dropin-missing", Diff: diff}, true
}

// validateKeyBindings checks the live tmux key table (as `list-keys` prints
// it, e.g. `bind-key -T prefix M-s run-shell -b "…/go-tmux-saver save"`)
// still binds M-s to a save and M-r to a merge-restore.
//
// The match is per line and on the key plus the command, deliberately not on
// the whole run-shell form: M-s runs the save with `run-shell -b` (background,
// so a multi-second save never blocks the tmux server) while M-r stays
// foreground, and neither the -b nor the binary's path should make a
// correctly-bound key look like drift. Requiring both halves on one line
// stops an M-s bound to something else plus a save bound to another key from
// passing as a pair.
func validateKeyBindings(env Env) (Drift, bool) {
	out, err := env.TmuxBindings()
	ok := err == nil &&
		bindsKeyTo(out, "M-s", "go-tmux-saver save") &&
		bindsKeyTo(out, "M-r", "go-tmux-saver restore --merge")
	if ok {
		return Drift{}, false
	}
	diff := strings.TrimSpace(out)
	if err != nil {
		diff = strings.TrimSpace(diff + " " + err.Error())
	}
	return Drift{Path: RelTmuxConf, Kind: "keybinding-missing", Diff: diff}, true
}

// bindsKeyTo reports whether any single line of tmux `list-keys` output
// mentions both key and cmd. key is matched as a whitespace-delimited field
// so "M-s" does not match a longer key name that merely contains it.
func bindsKeyTo(listKeys, key, cmd string) bool {
	for _, line := range strings.Split(listKeys, "\n") {
		if !strings.Contains(line, cmd) {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == key {
				return true
			}
		}
	}
	return false
}

// diffLines is a simple line-based unified-ish diff: for every line index
// where want and got disagree it emits a "-want"/"+got" pair. It's meant to
// give a human enough context to see what changed, not to be a minimal
// diff.
func diffLines(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	n := len(wantLines)
	if len(gotLines) > n {
		n = len(gotLines)
	}
	var buf strings.Builder
	for i := 0; i < n; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			fmt.Fprintf(&buf, "-%s\n+%s\n", w, g)
		}
	}
	return buf.String()
}
