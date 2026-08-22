package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/config"
)

func testParams() Params {
	return Params{
		Version:         "v9.9.9",
		Binary:          "/usr/bin/go-tmux-saver",
		Socket:          "main",
		SeedSession:     "default",
		SeedWindow:      "h",
		IntervalMinutes: 10,
		MailTo:          "tim",
	}
}

func renderTestFiles(t *testing.T) []Managed {
	t.Helper()
	files, err := Render(testParams())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return files
}

// fakeSystemctl records every call it's given and answers just enough to
// look like a freshly-Installed, healthy system: both timers enabled and
// active, `systemd-analyze --user verify` clean, and tmux-server.service's
// live ExecStartPost showing the drop-in's restore line. Its override
// fields let individual Validate-drift tests knock exactly one of those
// canned answers off healthy, without hand-rolling a whole new fake.
type fakeSystemctl struct {
	calls [][]string

	// isActiveFail, when non-empty, makes is-active for that unit name
	// answer "inactive" instead of "active" (is-enabled is untouched, so
	// this alone drives one unit-inactive Drift).
	isActiveFail string
	// verifyErr/verifyOut, when verifyErr is non-nil, make the
	// `systemd-analyze --user verify` call fail with verifyErr and
	// verifyOut as its combined output.
	verifyErr error
	verifyOut string
	// showOut, when non-empty, overrides the default ExecStartPost `show`
	// output (e.g. to one missing "restore --on-start").
	showOut string
}

func (f *fakeSystemctl) run(args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case f.isActiveFail != "" && len(args) >= 3 && args[1] == "is-active" && args[2] == f.isActiveFail:
		return "inactive\n", nil
	case len(args) >= 2 && args[1] == "is-enabled":
		return "enabled\n", nil
	case len(args) >= 2 && args[1] == "is-active":
		return "active\n", nil
	case len(args) >= 2 && args[1] == "verify":
		if f.verifyErr != nil {
			return f.verifyOut, f.verifyErr
		}
		return "", nil
	case len(args) >= 3 && args[1] == "show":
		if f.showOut != "" {
			return f.showOut, nil
		}
		return "ExecStartPost={ path=/usr/bin/go-tmux-saver ; argv[]=/usr/bin/go-tmux-saver restore --on-start ; ignore_errors=no }\n", nil
	default:
		return "", nil
	}
}

// fakeTmuxBindingsOK mimics `tmux list-keys` output for a correctly
// installed setup: M-s bound with run-shell -b (background), M-r foreground.
func fakeTmuxBindingsOK() (string, error) {
	return "" +
		"bind-key    -T prefix     M-s  run-shell -b \"go-tmux-saver save\"\n" +
		"bind-key    -T prefix     M-r  run-shell \"go-tmux-saver restore --merge\"\n", nil
}

func testEnv(home string, fake *fakeSystemctl) Env {
	return Env{
		ConfigHome:   home,
		Systemctl:    fake.run,
		TmuxBindings: fakeTmuxBindingsOK,
		Stdout:       io.Discard,
	}
}

// (a) Render golden: every file starts with the managed header (except
// config.json, which stays pure JSON), the service file's ExecStart line is
// exactly right, and Render's output is the full, deterministically
// ordered set of managed files.
func TestRenderGolden(t *testing.T) {
	p := testParams()
	files, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	wantRels := []string{
		RelService, RelTimer, RelWatchService, RelWatchTimer,
		RelAlertService, RelTmuxDropin, RelTmuxConf, RelConfigJSON,
	}
	if len(files) != len(wantRels) {
		t.Fatalf("Render returned %d files, want %d", len(files), len(wantRels))
	}
	for i, want := range wantRels {
		if files[i].Rel != want {
			t.Errorf("files[%d].Rel = %q, want %q (Render must be deterministic)", i, files[i].Rel, want)
		}
	}

	wantHeader := fmt.Sprintf(Header, p.Version)
	for _, f := range files {
		if f.Rel == RelConfigJSON {
			if bytes.Contains(f.Content, []byte(wantHeader)) {
				t.Errorf("config.json must not carry the managed header (it must stay valid JSON): %s", f.Content)
			}
			if f.Mode.Perm() != 0o600 {
				t.Errorf("config.json mode = %o, want 0600", f.Mode.Perm())
			}
			continue
		}
		if !bytes.HasPrefix(f.Content, []byte(wantHeader)) {
			t.Errorf("%s: content does not start with the managed header:\n%s", f.Rel, f.Content)
		}
		if f.Mode.Perm() != 0o644 {
			t.Errorf("%s: mode = %o, want 0644", f.Rel, f.Mode.Perm())
		}
	}

	var service Managed
	for _, f := range files {
		if f.Rel == RelService {
			service = f
		}
	}
	if !bytes.Contains(service.Content, []byte("ExecStart=/usr/bin/go-tmux-saver save --auto --no-display")) {
		t.Errorf("%s content = %s, want the exact ExecStart line", RelService, service.Content)
	}

	var timer Managed
	for _, f := range files {
		if f.Rel == RelTimer {
			timer = f
		}
	}
	if !bytes.Contains(timer.Content, []byte("OnUnitActiveSec=10min")) {
		t.Errorf("%s content = %s, want OnUnitActiveSec=10min", RelTimer, timer.Content)
	}

	// The key bindings are pinned exactly: M-s must be backgrounded with
	// `run-shell -b` (a save takes seconds, dominated by tmux's own
	// capture-pane cost, and must never block the server), M-r must stay
	// foreground so the user sees the result of a mutating restore.
	var tmuxConf Managed
	for _, f := range files {
		if f.Rel == RelTmuxConf {
			tmuxConf = f
		}
	}
	for _, want := range []string{
		"bind-key M-s run-shell -b \"/usr/bin/go-tmux-saver save\"\n",
		"bind-key M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n",
	} {
		if !bytes.Contains(tmuxConf.Content, []byte(want)) {
			t.Errorf("%s content = %s, want it to contain %q", RelTmuxConf, tmuxConf.Content, want)
		}
	}
	if bytes.Contains(tmuxConf.Content, []byte("bind-key M-r run-shell -b")) {
		t.Errorf("%s: M-r must stay in the foreground:\n%s", RelTmuxConf, tmuxConf.Content)
	}
}

// (b) Install writes every file at the right ConfigHome-relative path with
// the right mode, and the fake Systemctl saw daemon-reload before
// enable --now on both timers.
func TestInstallWritesFilesAndEnablesTimers(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)

	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, f := range files {
		full := filepath.Join(home, f.Rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("stat %s: %v", f.Rel, err)
		}
		if info.Mode().Perm() != f.Mode.Perm() {
			t.Errorf("%s: mode = %04o, want %04o", f.Rel, info.Mode().Perm(), f.Mode.Perm())
		}
		got, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read %s: %v", f.Rel, err)
		}
		if !bytes.Equal(got, f.Content) {
			t.Errorf("%s: content on disk does not match Render's output", f.Rel)
		}
	}

	idxReload, idxEnable := -1, -1
	for i, c := range fake.calls {
		if len(c) >= 2 && c[0] == "--user" && c[1] == "daemon-reload" {
			idxReload = i
		}
		if len(c) >= 2 && c[0] == "--user" && c[1] == "enable" {
			idxEnable = i
		}
	}
	if idxReload == -1 {
		t.Fatalf("Install never called daemon-reload; calls = %v", fake.calls)
	}
	if idxEnable == -1 {
		t.Fatalf("Install never called enable; calls = %v", fake.calls)
	}
	if idxReload >= idxEnable {
		t.Fatalf("Install must daemon-reload before enable --now; calls = %v", fake.calls)
	}
	enableCall := strings.Join(fake.calls[idxEnable], " ")
	for _, want := range []string{"--now", "go-tmux-saver.timer", "go-tmux-saver-watch.timer"} {
		if !strings.Contains(enableCall, want) {
			t.Errorf("enable call = %q, want it to contain %q", enableCall, want)
		}
	}

	// RULING R32: go-tmux-saver/ (holds the 0600 config.json) is 0700;
	// systemd/user/ and its tmux-server.service.d/ drop-in dir stay 0755.
	dirModes := map[string]os.FileMode{
		"go-tmux-saver":                      0o700,
		"systemd/user":                       0o755,
		"systemd/user/tmux-server.service.d": 0o755,
	}
	for rel, want := range dirModes {
		info, err := os.Stat(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("stat dir %s: %v", rel, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", rel)
		}
		if info.Mode().Perm() != want {
			t.Errorf("dir %s: mode = %04o, want %04o", rel, info.Mode().Perm(), want)
		}
	}
}

// (c) Validate reports no drift right after Install, exactly one `differs`
// drift (with a non-empty Diff) once a file is corrupted, and a `missing`
// drift once the drop-in is deleted.
func TestValidateDetectsDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)

	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if drifts := Validate(env, files); len(drifts) != 0 {
		t.Fatalf("Validate right after Install = %+v, want none", drifts)
	}

	corrupted := filepath.Join(home, RelTimer)
	if err := os.WriteFile(corrupted, []byte("garbage, not a rendered unit"), 0o644); err != nil {
		t.Fatalf("corrupt %s: %v", RelTimer, err)
	}
	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate after corrupting %s = %+v, want exactly 1 drift", RelTimer, drifts)
	}
	if drifts[0].Kind != "differs" || drifts[0].Path != RelTimer {
		t.Fatalf("Validate drift = %+v, want {Path: %s, Kind: differs}", drifts[0], RelTimer)
	}
	if drifts[0].Diff == "" {
		t.Fatal("differs drift has an empty Diff")
	}

	// Restore to a clean install, then delete the tmux-server drop-in.
	if err := WriteFiles(home, files); err != nil {
		t.Fatalf("restore files: %v", err)
	}
	dropin := filepath.Join(home, RelTmuxDropin)
	if err := os.Remove(dropin); err != nil {
		t.Fatalf("remove %s: %v", RelTmuxDropin, err)
	}
	drifts = Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate after deleting %s = %+v, want exactly 1 drift", RelTmuxDropin, drifts)
	}
	if drifts[0].Kind != "missing" || drifts[0].Path != RelTmuxDropin {
		t.Fatalf("Validate drift = %+v, want {Path: %s, Kind: missing}", drifts[0], RelTmuxDropin)
	}
}

// (d) Update --dry-run reports the differing file without writing it or
// touching systemd; a real Update rewrites it and restarts both timers.
func TestUpdateDryRunThenApplies(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)

	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	driftContent := "garbage, not a rendered unit"
	driftPath := filepath.Join(home, RelTimer)
	if err := os.WriteFile(driftPath, []byte(driftContent), 0o644); err != nil {
		t.Fatalf("drift %s: %v", RelTimer, err)
	}
	fake.calls = nil // isolate the calls Update itself makes

	var dryOut bytes.Buffer
	dryEnv := env
	dryEnv.Stdout = &dryOut
	changed, err := Update(dryEnv, files, true)
	if err != nil {
		t.Fatalf("Update --dry-run: %v", err)
	}
	if len(changed) != 1 || changed[0] != RelTimer {
		t.Fatalf("Update --dry-run changed = %v, want [%s]", changed, RelTimer)
	}
	got, err := os.ReadFile(driftPath)
	if err != nil {
		t.Fatalf("read %s: %v", RelTimer, err)
	}
	if string(got) != driftContent {
		t.Fatalf("Update --dry-run must not write; %s = %q", RelTimer, got)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("Update --dry-run must not call Systemctl; calls = %v", fake.calls)
	}

	// FINDING 1: dry-run must print the same line diff Validate's "differs"
	// Drift carries, not just the "would update: <rel>" header.
	dryText := dryOut.String()
	if !strings.Contains(dryText, "would update: "+RelTimer) {
		t.Fatalf("dry-run output = %q, want a %q header line", dryText, "would update: "+RelTimer)
	}
	if !strings.Contains(dryText, "-") || !strings.Contains(dryText, "+") {
		t.Fatalf("dry-run output = %q, want a -/+ line diff", dryText)
	}
	if !strings.Contains(dryText, driftContent) {
		t.Fatalf("dry-run output = %q, want it to show the corrupted content %q", dryText, driftContent)
	}

	changed, err = Update(env, files, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(changed) != 1 || changed[0] != RelTimer {
		t.Fatalf("Update changed = %v, want [%s]", changed, RelTimer)
	}
	got, err = os.ReadFile(driftPath)
	if err != nil {
		t.Fatalf("read %s: %v", RelTimer, err)
	}
	var want Managed
	for _, f := range files {
		if f.Rel == RelTimer {
			want = f
		}
	}
	if !bytes.Equal(got, want.Content) {
		t.Fatalf("Update did not rewrite %s to the rendered content", RelTimer)
	}

	sawRestart := false
	for _, c := range fake.calls {
		if len(c) >= 2 && c[0] == "--user" && c[1] == "restart" {
			joined := strings.Join(c, " ")
			if strings.Contains(joined, "go-tmux-saver.timer") && strings.Contains(joined, "go-tmux-saver-watch.timer") {
				sawRestart = true
			}
		}
	}
	if !sawRestart {
		t.Fatalf("Update must restart both timers; calls = %v", fake.calls)
	}
}

// FINDING 2 — mode: chmod'ing a managed unit away from its rendered mode
// must surface as a single `mode` Drift on that file.
func TestValidateModeDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	target := filepath.Join(home, RelTimer)
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod %s: %v", RelTimer, err)
	}

	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate after chmod = %+v, want exactly 1 drift", drifts)
	}
	if drifts[0].Kind != "mode" || drifts[0].Path != RelTimer {
		t.Fatalf("Validate drift = %+v, want {Path: %s, Kind: mode}", drifts[0], RelTimer)
	}
	if drifts[0].Diff == "" {
		t.Fatal("mode drift has an empty Diff")
	}
}

// FINDING 2 — unit-inactive: a timer that answers is-active=inactive (while
// still is-enabled) must surface as a single `unit-inactive` Drift on that
// timer's unit name.
func TestValidateUnitInactiveDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{isActiveFail: "go-tmux-saver.timer"}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate = %+v, want exactly 1 drift", drifts)
	}
	if drifts[0].Kind != "unit-inactive" || drifts[0].Path != "go-tmux-saver.timer" {
		t.Fatalf("Validate drift = %+v, want {Path: go-tmux-saver.timer, Kind: unit-inactive}", drifts[0])
	}
}

// FINDING 2 — unit-invalid: `systemd-analyze --user verify` failing must
// surface as a single `unit-invalid` Drift with a non-empty Diff.
func TestValidateUnitInvalidDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{verifyErr: errors.New("exit status 1"), verifyOut: "Failed to parse ExecStart= setting\n"}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate = %+v, want exactly 1 drift", drifts)
	}
	if drifts[0].Kind != "unit-invalid" {
		t.Fatalf("Validate drift = %+v, want Kind unit-invalid", drifts[0])
	}
	if drifts[0].Diff == "" {
		t.Fatal("unit-invalid drift has an empty Diff")
	}
}

// FINDING 2 — dropin-missing: a live tmux-server.service ExecStartPost that
// doesn't contain "restore --on-start" must surface as a single
// `dropin-missing` Drift, even though the drop-in file itself is present
// and byte-correct on disk (this is a live-state check, not a file check —
// see validateDropinEffective's doc comment).
func TestValidateDropinMissingDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{
		showOut: "ExecStartPost={ path=/usr/bin/tmux ; argv[]=/usr/bin/tmux -L main set-window-option -t default:h remain-on-exit on ; ignore_errors=no }\n",
	}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate = %+v, want exactly 1 drift", drifts)
	}
	if drifts[0].Kind != "dropin-missing" || drifts[0].Path != RelTmuxDropin {
		t.Fatalf("Validate drift = %+v, want {Path: %s, Kind: dropin-missing}", drifts[0], RelTmuxDropin)
	}
}

// FINDING 2 — keybinding-missing: TmuxBindings() output missing the M-r
// restore binding must surface as a single `keybinding-missing` Drift.
func TestValidateKeybindingMissingDrift(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)
	env.TmuxBindings = func() (string, error) {
		return "bind-key    -T prefix     M-s  run-shell -b \"go-tmux-saver save\"\n", nil // no M-r line
	}
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	drifts := Validate(env, files)
	if len(drifts) != 1 {
		t.Fatalf("Validate = %+v, want exactly 1 drift", drifts)
	}
	if drifts[0].Kind != "keybinding-missing" || drifts[0].Path != RelTmuxConf {
		t.Fatalf("Validate drift = %+v, want {Path: %s, Kind: keybinding-missing}", drifts[0], RelTmuxConf)
	}
}

// Key-binding validation must accept the bindings exactly as tmux
// `list-keys` prints them once installed — M-s backgrounded with
// `run-shell -b`, M-r foreground, both with the absolute binary path — and
// must reject every way they can genuinely be wrong, including a server
// still holding the pre-R42 foreground M-s (RULING R43).
func TestValidateKeyBindingsAcceptsListKeysShapes(t *testing.T) {
	const installed = "" +
		"bind-key -T prefix M-s run-shell -b \"/usr/bin/go-tmux-saver save\"\n" +
		"bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n"

	tests := []struct {
		name      string
		out       string
		err       error
		wantDrift bool
		wantDiff  string // substring the Diff must carry when drifting
	}{
		{name: "installed (M-s backgrounded, M-r foreground)", out: installed},
		{
			name: "extra unrelated bindings, padding and other key tables",
			out: "bind-key -T prefix c new-window\n" +
				"bind-key    -T copy-mode  M-r   send-keys -X middle-line\n" +
				"bind-key    -T prefix     M-s   run-shell -b \"go-tmux-saver save\"\n" +
				"bind-key    -T prefix     M-r   run-shell \"go-tmux-saver restore --merge\"   \n",
		},
		{
			// The whole point of R42: a server still running the old
			// foreground binding is stale, and the validator must say so
			// rather than pass or report a bare "missing".
			name: "legacy foreground M-s is drift",
			out: "bind-key -T prefix M-s run-shell \"/usr/bin/go-tmux-saver save\"\n" +
				"bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n",
			wantDrift: true,
			wantDiff:  "not to the background form (run-shell -b)",
		},
		{
			// Anchoring: a longer command must not satisfy a shorter one.
			name: "M-r bound to a longer look-alike command",
			out: "bind-key -T prefix M-s run-shell -b \"/usr/bin/go-tmux-saver save\"\n" +
				"bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge-foo\"\n",
			wantDrift: true,
			wantDiff:  `M-r is not bound to run-shell "<binary> restore --merge"`,
		},
		{
			name: "M-s bound to a longer look-alike command",
			out: "bind-key -T prefix M-s run-shell -b \"/usr/bin/go-tmux-saver save-buffer\"\n" +
				"bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n",
			wantDrift: true,
			wantDiff:  `M-s is not bound to run-shell -b "<binary> save"`,
		},
		{
			name:      "M-r missing",
			out:       "bind-key -T prefix M-s run-shell -b \"/usr/bin/go-tmux-saver save\"\n",
			wantDrift: true,
			wantDiff:  "M-r is not bound",
		},
		{
			name:      "M-s missing",
			out:       "bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n",
			wantDrift: true,
			wantDiff:  "M-s is not bound",
		},
		{name: "nothing bound", out: "bind-key -T prefix c new-window\n", wantDrift: true, wantDiff: "M-s is not bound"},
		{
			// The key and the command must be on the SAME line: M-s bound to
			// something else, with the save hung off another key, is drift.
			name: "key and command on different lines",
			out: "bind-key -T prefix M-s display-message \"nope\"\n" +
				"bind-key -T prefix M-x run-shell -b \"/usr/bin/go-tmux-saver save\"\n" +
				"bind-key -T prefix M-r run-shell \"/usr/bin/go-tmux-saver restore --merge\"\n",
			wantDrift: true,
			wantDiff:  "M-s is not bound",
		},
		{name: "tmux unavailable", out: installed, err: errors.New("no server running"), wantDrift: true, wantDiff: "no server running"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{TmuxBindings: func() (string, error) { return tc.out, tc.err }}
			drift, got := validateKeyBindings(env)
			if got != tc.wantDrift {
				t.Fatalf("validateKeyBindings() drift = %v (%+v), want %v", got, drift, tc.wantDrift)
			}
			if !got {
				return
			}
			if drift.Kind != "keybinding-missing" || drift.Path != RelTmuxConf {
				t.Fatalf("drift = %+v, want {Path: %s, Kind: keybinding-missing}", drift, RelTmuxConf)
			}
			if !strings.Contains(drift.Diff, tc.wantDiff) {
				t.Errorf("drift.Diff = %q, want it to contain %q", drift.Diff, tc.wantDiff)
			}
		})
	}
}

// FINDING 3 — Update must never rewrite an existing config.json, even when
// it differs from the rendered default: it must not appear in `changed`,
// its bytes on disk must be untouched, and (since it's still valid JSON
// that parses+Validate()s) Validate must report no drift for it either.
func TestUpdateNeverOverwritesExistingConfigJSON(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}

	customCfg := config.Default()
	customCfg.Socket = "custom"
	custom, err := json.MarshalIndent(customCfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal custom config: %v", err)
	}
	custom = append(custom, '\n')
	cfgPath := filepath.Join(home, RelConfigJSON)
	if err := os.WriteFile(cfgPath, custom, 0o600); err != nil {
		t.Fatalf("write custom %s: %v", RelConfigJSON, err)
	}

	changed, err := Update(env, files, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, rel := range changed {
		if rel == RelConfigJSON {
			t.Fatalf("Update listed %s as changed: %v", RelConfigJSON, changed)
		}
	}

	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", RelConfigJSON, err)
	}
	if !bytes.Equal(got, custom) {
		t.Fatalf("Update rewrote %s:\ngot  %s\nwant %s", RelConfigJSON, got, custom)
	}

	if drifts := Validate(env, files); len(drifts) != 0 {
		t.Fatalf("Validate with a custom-but-valid %s = %+v, want none", RelConfigJSON, drifts)
	}
}

// TestRenderDropinIgnoresExecStartPostFailure covers C2/RULING R45: the
// tmux-server.service drop-in must prefix its ExecStartPost with '-' so a
// non-zero restore can never make systemd fail tmux-server.service itself.
func TestRenderDropinIgnoresExecStartPostFailure(t *testing.T) {
	files := renderTestFiles(t)
	var dropin Managed
	for _, f := range files {
		if f.Rel == RelTmuxDropin {
			dropin = f
		}
	}
	want := "ExecStartPost=-/usr/bin/go-tmux-saver restore --on-start\n"
	if !bytes.Contains(dropin.Content, []byte(want)) {
		t.Fatalf("%s content = %s, want it to contain %q", RelTmuxDropin, dropin.Content, want)
	}
}

// TestValidateDropinToleratesIgnoreErrorsPrefix covers C2's validator half:
// with the '-' prefix in place, `systemctl show -p ExecStartPost` reports
// ignore_errors=yes, and validate must still recognise the drop-in as
// effective rather than reporting dropin-missing drift.
func TestValidateDropinToleratesIgnoreErrorsPrefix(t *testing.T) {
	home := t.TempDir()
	files := renderTestFiles(t)
	fake := &fakeSystemctl{
		showOut: "ExecStartPost={ path=/usr/bin/go-tmux-saver ; argv[]=/usr/bin/go-tmux-saver restore --on-start ; flags=ignore-failure ; ignore_errors=yes }\n",
	}
	env := testEnv(home, fake)
	if err := Install(env, files); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for _, d := range Validate(env, files) {
		if d.Kind == "dropin-missing" {
			t.Fatalf("dropin-missing drift with the '-' (ignore_errors=yes) form: %+v", d)
		}
	}
}
