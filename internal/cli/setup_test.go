package cli

import (
	"bytes"
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
