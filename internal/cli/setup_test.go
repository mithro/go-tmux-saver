package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	gtssetup "github.com/mithro/go-tmux-saver/internal/setup"
)

// withFakeExecCommand swaps execCommand for fn for the duration of the
// calling test, restoring the real implementation via t.Cleanup. Every
// test in this file that must not touch the real system uses this — none
// of them may let a real systemctl/systemd-analyze/tmux process run.
func withFakeExecCommand(t *testing.T, fn func(timeout time.Duration, name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := execCommand
	execCommand = fn
	t.Cleanup(func() { execCommand = orig })
}

// (a) resolveBinary returns an absolute, fully symlink-resolved path (it's
// exercised against the running `go test` binary itself, which is a real
// file on disk).
func TestResolveBinary(t *testing.T) {
	bin, err := resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	if !filepath.IsAbs(bin) {
		t.Fatalf("resolveBinary() = %q, want an absolute path", bin)
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", bin, err)
	}
	if resolved != bin {
		t.Fatalf("resolveBinary() = %q, not fully symlink-resolved (EvalSymlinks gives %q)", bin, resolved)
	}
}

// (b) realSystemctl's dispatch: a "verify" call goes to `systemd-analyze`,
// everything else goes to `systemctl` — both with the same args — and every
// call (RULING R33) observes the 15s setup-path timeout.
func TestRealSystemctlDispatchesVerifyToSystemdAnalyze(t *testing.T) {
	type call struct {
		timeout time.Duration
		name    string
		args    []string
	}
	var calls []call
	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{timeout, name, append([]string(nil), args...)})
		return []byte("ok\n"), nil
	})

	if _, err := realSystemctl("--user", "verify", "/a/b.service", "/a/c.timer"); err != nil {
		t.Fatalf("realSystemctl verify: %v", err)
	}
	if _, err := realSystemctl("--user", "is-active", "go-tmux-saver.timer"); err != nil {
		t.Fatalf("realSystemctl is-active: %v", err)
	}
	if _, err := realSystemctl("--user", "daemon-reload"); err != nil {
		t.Fatalf("realSystemctl daemon-reload: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("execCommand calls = %+v, want 3", calls)
	}

	if calls[0].name != "systemd-analyze" {
		t.Errorf("verify dispatched to %q, want systemd-analyze", calls[0].name)
	}
	if got, want := strings.Join(calls[0].args, " "), "--user verify /a/b.service /a/c.timer"; got != want {
		t.Errorf("verify args = %q, want %q", got, want)
	}

	if calls[1].name != "systemctl" {
		t.Errorf("is-active dispatched to %q, want systemctl", calls[1].name)
	}
	if got, want := strings.Join(calls[1].args, " "), "--user is-active go-tmux-saver.timer"; got != want {
		t.Errorf("is-active args = %q, want %q", got, want)
	}

	if calls[2].name != "systemctl" {
		t.Errorf("daemon-reload dispatched to %q, want systemctl", calls[2].name)
	}

	for i, c := range calls {
		if c.timeout != setupExecTimeout {
			t.Errorf("calls[%d] (%s) timeout = %v, want the setup-path timeout %v", i, c.name, c.timeout, setupExecTimeout)
		}
	}
}

// realTmuxBindings must dispatch to `tmux -L <socket> list-keys` via the
// same execCommand seam (covered alongside realSystemctl's dispatch test
// since both back Env's injectables), observing the 15s setup-path timeout
// (RULING R33).
func TestRealTmuxBindingsDispatch(t *testing.T) {
	type call struct {
		timeout time.Duration
		name    string
		args    []string
	}
	var calls []call
	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{timeout, name, append([]string(nil), args...)})
		return []byte("bind-key -T prefix M-s run-shell -b \"go-tmux-saver save\"\n"), nil
	})

	bindings := realTmuxBindings(mustLoadDefaultConfig(t, "main"))
	out, err := bindings()
	if err != nil {
		t.Fatalf("realTmuxBindings(): %v", err)
	}
	if !strings.Contains(out, "M-s") {
		t.Fatalf("realTmuxBindings() = %q, want it to pass through execCommand's output", out)
	}
	if len(calls) != 1 || calls[0].name != "tmux" {
		t.Fatalf("execCommand calls = %+v, want one call to tmux", calls)
	}
	if got, want := strings.Join(calls[0].args, " "), "-L main list-keys"; got != want {
		t.Errorf("tmux args = %q, want %q", got, want)
	}
	if calls[0].timeout != setupExecTimeout {
		t.Errorf("tmux call timeout = %v, want the setup-path timeout %v", calls[0].timeout, setupExecTimeout)
	}
}

// mustLoadDefaultConfig is a small helper for tests that only need a
// config.Config with a specific Socket (nothing else about it matters).
func mustLoadDefaultConfig(t *testing.T, socket string) config.Config {
	t.Helper()
	cfgPath := writeConfig(t, `{"socket":"`+socket+`"}`)
	cfg, err := loadSetupCfg(cfgPath)
	if err != nil {
		t.Fatalf("loadSetupCfg: %v", err)
	}
	return cfg
}

// (c) timerState (wired to realTimerState by setup.go's init) reports
// "unknown" when the injected exec fails, and the trimmed exec output
// otherwise — and (RULING R33) it must request the short 3s
// timerStateExecTimeout, not the 15s setup-path bound, so `status` can't
// block long on a hung systemd.
func TestTimerStateViaExecCommand(t *testing.T) {
	var gotTimeout time.Duration
	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		gotTimeout = timeout
		return nil, errors.New("boom")
	})
	if got := timerState(); got != "unknown" {
		t.Fatalf("timerState() on exec error = %q, want %q", got, "unknown")
	}
	if gotTimeout != timerStateExecTimeout {
		t.Fatalf("timerState() used timeout %v, want the 3s timerStateExecTimeout %v", gotTimeout, timerStateExecTimeout)
	}

	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		gotTimeout = timeout
		return []byte("active\n"), nil
	})
	if got := timerState(); got != "active" {
		t.Fatalf("timerState() = %q, want %q", got, "active")
	}
	if gotTimeout != timerStateExecTimeout {
		t.Fatalf("timerState() used timeout %v, want the 3s timerStateExecTimeout %v", gotTimeout, timerStateExecTimeout)
	}
	if gotTimeout == setupExecTimeout {
		t.Fatalf("timerState() must not use the setup-path timeout %v", setupExecTimeout)
	}
}

// (d) `setup generate --dir <tmp>` writes every managed file under tmp with
// the right modes and makes zero execCommand calls — it must not touch the
// real systemd/tmux state even when run against a real ConfigHome.
func TestSetupGenerateCLIWritesFilesWithNoExecCalls(t *testing.T) {
	execCalls := 0
	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		execCalls++
		return nil, fmt.Errorf("execCommand must not be called by `setup generate`: %s %v", name, args)
	})

	dir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"setup", "generate", "--dir", dir, "--config", cfgPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if execCalls != 0 {
		t.Fatalf("setup generate made %d execCommand calls, want 0", execCalls)
	}

	rels := []struct {
		rel  string
		mode os.FileMode
	}{
		{gtssetup.RelService, 0o644},
		{gtssetup.RelTimer, 0o644},
		{gtssetup.RelWatchService, 0o644},
		{gtssetup.RelWatchTimer, 0o644},
		{gtssetup.RelAlertService, 0o644},
		{gtssetup.RelTmuxDropin, 0o644},
		{gtssetup.RelTmuxConf, 0o644},
		{gtssetup.RelConfigJSON, 0o600},
	}
	for _, r := range rels {
		full := filepath.Join(dir, r.rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Fatalf("stat %s: %v", r.rel, err)
		}
		if info.Mode().Perm() != r.mode {
			t.Errorf("%s: mode = %04o, want %04o", r.rel, info.Mode().Perm(), r.mode)
		}
	}
}

// TestSetupGenerateNoDirWritesToStdout covers I4/RULING R50: `setup
// generate` with no --dir used to default to the REAL ConfigHome and write
// there — a "show me what you'd install" command that silently installed.
// With no --dir it now renders to stdout with "=== <rel> ===" separators
// and touches no files at all.
func TestSetupGenerateNoDirWritesToStdout(t *testing.T) {
	withFakeExecCommand(t, func(timeout time.Duration, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("execCommand must not be called by `setup generate`: %s %v", name, args)
	})
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"setup", "generate", "--config", cfgPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}

	for _, rel := range []string{
		gtssetup.RelService, gtssetup.RelTimer, gtssetup.RelWatchService, gtssetup.RelWatchTimer,
		gtssetup.RelAlertService, gtssetup.RelTmuxDropin, gtssetup.RelTmuxConf, gtssetup.RelConfigJSON,
	} {
		if !strings.Contains(out.String(), "=== "+rel+" ===") {
			t.Errorf("stdout missing the %q separator:\n%s", rel, out.String())
		}
	}
	if !strings.Contains(out.String(), "ExecStartPost=-") {
		t.Errorf("stdout should carry the rendered file CONTENT, not just names:\n%s", out.String())
	}

	entries, err := os.ReadDir(configHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("setup generate with no --dir wrote into ConfigHome: %v", entries)
	}
}

// healthyFakeExec answers every systemctl/systemd-analyze/tmux call the
// setup validate path makes as a freshly-installed, healthy system would.
func healthyFakeExec(timeout time.Duration, name string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case name == "tmux":
		return []byte("bind-key -T prefix M-s run-shell -b \"go-tmux-saver save\"\n" +
			"bind-key -T prefix M-r run-shell \"go-tmux-saver restore --merge\"\n"), nil
	case strings.Contains(joined, "is-enabled"):
		return []byte("enabled\n"), nil
	case strings.Contains(joined, "is-active"):
		return []byte("active\n"), nil
	case strings.Contains(joined, "show"):
		return []byte("ExecStartPost={ path=/x ; argv[]=/x restore --on-start ; ignore_errors=yes }\n"), nil
	default: // verify, daemon-reload, enable
		return nil, nil
	}
}

// TestSetupValidateJSON covers I9/RULING R50: `setup validate --json` emits
// {"ok":bool,"drifts":[{"path":…,"kind":…,"diff":…}]} so the drift check is
// machine-readable (an Ansible/monitoring consumer shouldn't have to scrape
// the human text). Exit codes are unchanged: 0 clean, 1 with drift.
func TestSetupValidateJSON(t *testing.T) {
	type driftJSON struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Diff string `json:"diff"`
	}
	type report struct {
		OK     bool        `json:"ok"`
		Drifts []driftJSON `json:"drifts"`
	}

	// (1) Nothing installed at all: ok=false, one "missing" drift per file.
	// HOME is hermetic too: validate now checks ~/bin/claude-resume, which
	// must never observe (or, in install/update tests, rewrite) the real
	// user's link.
	withFakeExecCommand(t, healthyFakeExec)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"setup", "validate", "--json", "--config", cfgPath}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (drift present); stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	var rep report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if rep.OK {
		t.Errorf("ok = true with drift present: %+v", rep)
	}
	if len(rep.Drifts) == 0 {
		t.Fatalf("drifts empty, want one per missing managed file: %s", out.String())
	}
	found := false
	for _, d := range rep.Drifts {
		if d.Path == gtssetup.RelService && d.Kind == "missing" {
			found = true
		}
		if d.Path == "" || d.Kind == "" {
			t.Errorf("drift with an empty path/kind: %+v", d)
		}
	}
	if !found {
		t.Errorf("no missing drift for %s: %+v", gtssetup.RelService, rep.Drifts)
	}

	// (2) Everything installed and healthy: ok=true, empty drifts, exit 0.
	// "Healthy" now includes ~/bin/claude-resume pointing at this (the
	// test) binary — created here the same way setup install would.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExe, filepath.Join(home, "bin", "claude-resume")); err != nil {
		t.Fatal(err)
	}
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"setup", "generate", "--dir", configHome, "--config", cfgPath}, &out, &errb); code != 0 {
		t.Fatalf("setup generate exit %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	code = Run([]string{"setup", "validate", "--json", "--config", cfgPath}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (no drift); stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	rep = report{}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if !rep.OK || len(rep.Drifts) != 0 {
		t.Fatalf("report = %+v, want ok:true with no drifts", rep)
	}
}
