package setup

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateKeyBindingsAgainstRealTmux closes the loop that fixtures
// cannot: it renders the managed tmux.conf, sources it into a throwaway tmux
// server, and runs the real binding check over that server's actual
// `list-keys` output. Without this, a change to either the template or the
// validator could keep every fixture-based test green while the two disagree
// about what tmux actually prints.
//
// It also asserts the stale case that RULING R43 is about: a server still
// holding the pre-R42 foreground `M-s` must be reported as drift.
//
// The server is started with -f /dev/null (no user ~/.tmux.conf, see
// tmuxctl.StartTestServer) and killed on cleanup. Skips without tmux.
func TestValidateKeyBindingsAgainstRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "go-tmux-saver")
	p := testParams()
	p.Binary = binary
	files, err := Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var conf []byte
	for _, f := range files {
		if f.Rel == RelTmuxConf {
			conf = f.Content
		}
	}
	if len(conf) == 0 {
		t.Fatalf("Render produced no %s", RelTmuxConf)
	}
	confPath := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(confPath, conf, 0o644); err != nil {
		t.Fatal(err)
	}

	sock := fmt.Sprintf("gts-kb-%d-%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "_"))
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	tmux("-f", "/dev/null", "new-session", "-d", "-s", "default", "tail -f /dev/null")
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	tmux("source-file", confPath)

	env := Env{TmuxBindings: func() (string, error) { return tmux("list-keys"), nil }}

	// Sanity: the live table really does carry the rendered forms, so a pass
	// below cannot come from the validator being lax about missing input.
	live, _ := env.TmuxBindings()
	if !strings.Contains(live, "run-shell -b") {
		t.Fatalf("live list-keys has no backgrounded binding:\n%s", live)
	}
	if !bytes.Contains(conf, []byte("run-shell -b \""+binary+" save\"")) {
		t.Fatalf("rendered tmux.conf lost the background M-s form:\n%s", conf)
	}

	if drift, got := validateKeyBindings(env); got {
		t.Fatalf("validateKeyBindings() reported drift for a freshly sourced tmux.conf: %+v\nlist-keys:\n%s", drift, live)
	}

	// Now regress the server to the pre-R42 foreground binding, exactly as an
	// installation that has not re-sourced the managed file would look.
	tmux("bind-key", "M-s", "run-shell", binary+" save")
	drift, got := validateKeyBindings(env)
	if !got {
		t.Fatal("validateKeyBindings() accepted a foreground M-s; a stale binding must be drift")
	}
	if drift.Kind != "keybinding-missing" || drift.Path != RelTmuxConf {
		t.Fatalf("drift = %+v, want {Path: %s, Kind: keybinding-missing}", drift, RelTmuxConf)
	}
	if !strings.Contains(drift.Diff, "not to the background form (run-shell -b)") {
		t.Errorf("drift.Diff = %q, want it to name the background form", drift.Diff)
	}
}
