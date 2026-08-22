package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

func saveFake() *tmuxctl.Fake {
	return &tmuxctl.Fake{Replies: map[string][]string{
		collect.SessCmd:                 {"default\t0\t1"},
		collect.WinCmd:                  {"default\t0\tw\t1\t*\tL\ton"},
		collect.PaneCmd:                 {"default\t0\t0\t%0\t1\t100\t/home/tim\tt\t1", "default\t0\t1\t%1\t0\t300\t/home/tim\tt\t1"},
		collect.ServerCmd:               {"1\tnext-3.8\tdefault"},
		"capture-pane -epJ -S -1 -t %0": {"a"}, "capture-pane -epJ -S -1 -t %1": {"b"},
	}, Default: []string{}}
}

func deps(t *testing.T, f *tmuxctl.Fake) SaveDeps {
	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: t.TempDir(), Codec: gz}
	st.EnsureDir()
	tb, _ := procs.Scan("../procs/testdata/proc")
	cfg := config.Default()
	cfg.Guard.MinPanes = 2
	return SaveDeps{T: f, Store: st, Procs: tb, Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"}, Cfg: cfg, Host: "h", Display: func(string) {}}
}

func TestSaveOutcomes(t *testing.T) {
	d := deps(t, saveFake())
	ctx := context.Background()
	o, err := RunSave(ctx, d)
	if err != nil || o.Kind != "kept" || o.Panes != 2 {
		t.Fatalf("first save %+v %v", o, err)
	}
	o, _ = RunSave(ctx, d)
	if o.Kind != "unchanged" {
		t.Fatalf("second identical save should be unchanged, got %+v", o)
	}
	// degenerate: server now shows 0 panes
	d.T = &tmuxctl.Fake{Replies: map[string][]string{collect.SessCmd: {"default\t0\t1"}, collect.WinCmd: {}, collect.PaneCmd: {},
		collect.ServerCmd: {"1\tnext-3.8\tdefault"}}, Default: []string{}}
	o, _ = RunSave(ctx, d)
	if o.Kind != "rejected-degenerate" || o.LastPanes != 2 {
		t.Fatalf("degenerate %+v", o)
	}
	ev, _ := snapshot.TailEvents(d.Store.Dir, 10)
	if len(ev) != 3 || ev[0].Outcome != "kept" || ev[1].Outcome != "unchanged" || ev[2].Outcome != "rejected-degenerate" || ev[2].Detail != "0 vs 2" {
		t.Fatalf("events %+v", ev)
	}
	if _, ok, _ := snapshot.LastGood(d.Store.Dir); !ok {
		t.Fatal("fresh marker expected")
	}
	if len(d.T.(*tmuxctl.Fake).Calls) == 0 {
		t.Fatal("expected calls")
	}
	_ = time.Second
}

// writeConfig writes a JSON config overlay (typically "{}" or a small
// override) to a fresh temp file and returns its path, for driving the save
// subcommand through Run() rather than RunSave() directly.
func writeConfig(t *testing.T, json string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSaveCLIAutoNoServerSkipped covers `save --auto` against a socket with
// no tmux server listening: exit 0, "skipped" on stdout, one "skipped" event.
func TestSaveCLIAutoNoServerSkipped(t *testing.T) {
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()
	sock := fmt.Sprintf("gts-nonexistent-%d", os.Getpid())

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--auto", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "skipped")
	}
	ev, err := snapshot.TailEvents(dataDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "skipped" || ev[0].Detail != "no server" {
		t.Fatalf("events %+v", ev)
	}
}

// TestSaveCLIManualNoServerErrors covers a manual (non---auto) save against
// a socket with no tmux server: exit 1, not a skip.
func TestSaveCLIManualNoServerErrors(t *testing.T) {
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()
	sock := fmt.Sprintf("gts-nonexistent-%d", os.Getpid())

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}

// TestSaveCLILiveServerKept drives the save subcommand end-to-end against a
// real tmux server (tmuxctl.StartTestServer), proving --data-dir wins over
// config.DataDir(): the "last" symlink must land in the flag-supplied dir.
func TestSaveCLILiveServerKept(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--no-display", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.HasPrefix(out.String(), "kept") {
		t.Fatalf("stdout = %q, want prefix %q", out.String(), "kept")
	}
	ev, err := snapshot.TailEvents(dataDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ev {
		if e.Outcome == "kept" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events %+v, want a kept event", ev)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "last")); err != nil {
		t.Fatalf("expected %s/last to exist (proves --data-dir wins over config.DataDir()): %v", dataDir, err)
	}
}

// TestSaveCLIAutoBadSeedSessionErrors pins Finding 1: a live server with a
// misconfigured seed_session must NOT be classified as ErrNoServer just
// because Dial's attach failure surfaces "control connection closed" — it is
// a genuine error, so --auto still exits 1 (not "skipped").
func TestSaveCLIAutoBadSeedSessionErrors(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	cfgPath := writeConfig(t, `{"seed_session": "nonexistent-session"}`)
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--auto", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (not skipped); stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "skipped") {
		t.Fatalf("stdout = %q, should not report skipped", out.String())
	}
	ev, err := snapshot.TailEvents(dataDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "error" {
		t.Fatalf("events %+v, want one error event", ev)
	}
}
