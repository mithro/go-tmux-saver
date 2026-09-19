package tmuxtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolateRelocatesTmuxTmpdir checks that, with TMUX_TMPDIR unset, Isolate
// points it at a fresh private directory (never /tmp/tmux-$UID next to a real
// server), unsets TMUX for the run, passes the run's exit code through, and
// removes the directory afterwards.
func TestIsolateRelocatesTmuxTmpdir(t *testing.T) {
	restore := stashEnv(t, "TMUX_TMPDIR", "TMUX")
	defer restore()
	os.Unsetenv("TMUX_TMPDIR")
	os.Setenv("TMUX", "/tmp/tmux-1001/main,1234,0")

	var seenTmpdir string
	var tmuxStillSet bool
	code := Isolate(func() int {
		seenTmpdir = os.Getenv("TMUX_TMPDIR")
		_, tmuxStillSet = os.LookupEnv("TMUX")
		return 7
	})

	if code != 7 {
		t.Errorf("Isolate returned %d, want the run's code 7", code)
	}
	if tmuxStillSet {
		t.Error("TMUX was still set during the run; Isolate must unset it")
	}
	if seenTmpdir == "" {
		t.Fatal("TMUX_TMPDIR was empty during the run; Isolate must set it")
	}
	// The tmux socket dir would be seenTmpdir/tmux-$UID/* — it must not be
	// the default /tmp location that holds the production server.
	if seenTmpdir == "/tmp" || strings.HasPrefix(filepath.Clean(seenTmpdir), "/tmp/tmux-") {
		t.Errorf("TMUX_TMPDIR = %q, want a private dir well away from /tmp/tmux-*", seenTmpdir)
	}
	if _, err := os.Stat(seenTmpdir); !os.IsNotExist(err) {
		t.Errorf("private TMUX_TMPDIR %q still exists after Isolate (stat err=%v); it must be removed", seenTmpdir, err)
	}
}

// TestIsolateRespectsPresetTmpdir checks that an already-set TMUX_TMPDIR (the
// sandboxed runner's own scratch dir) is left untouched and not removed.
func TestIsolateRespectsPresetTmpdir(t *testing.T) {
	restore := stashEnv(t, "TMUX_TMPDIR", "TMUX")
	defer restore()
	preset := t.TempDir()
	os.Setenv("TMUX_TMPDIR", preset)

	var seen string
	code := Isolate(func() int {
		seen = os.Getenv("TMUX_TMPDIR")
		return 0
	})

	if code != 0 {
		t.Errorf("Isolate returned %d, want 0", code)
	}
	if seen != preset {
		t.Errorf("TMUX_TMPDIR during run = %q, want the preset %q (Isolate must not override it)", seen, preset)
	}
	if _, err := os.Stat(preset); err != nil {
		t.Errorf("preset TMUX_TMPDIR %q was removed/altered: %v; Isolate must leave an outer sandbox's dir alone", preset, err)
	}
}

// stashEnv snapshots the named env vars and returns a restore func.
func stashEnv(t *testing.T, keys ...string) func() {
	t.Helper()
	saved := make(map[string]*string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			v := v
			saved[k] = &v
		} else {
			saved[k] = nil
		}
	}
	return func() {
		for k, v := range saved {
			if v == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *v)
			}
		}
	}
}
