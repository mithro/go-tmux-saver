package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/mail"
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
// misconfigured seed_session must NOT be classified as ErrNoServer — it is a
// genuine error, so --auto still exits 1 (not "skipped"). It also pins the
// round-2 fix: Dial itself must notice tmux's %error reply to the initial
// attach (rather than handing back a *Client wrapping an already-exited
// tmux), so the logged error's Detail carries the session name straight from
// Dial — proving the failure was caught there, not later inside Collect as a
// generic "control connection closed".
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
	if !strings.Contains(ev[0].Detail, "nonexistent-session") {
		t.Fatalf("event detail = %q, want it to contain %q (proving the error came from Dial, not Collect)", ev[0].Detail, "nonexistent-session")
	}
}

// TestSaveCLIAutoSuccessSendsRecoveryMailWhenMarkerPresent covers the
// save-success recovery hook: a pre-created rate-limit marker for
// "go-tmux-saver.service" (as `alert` would have left behind after an
// earlier failure) must be cleared by a successful `save --auto`
// (kept/unchanged), sending exactly one recovery mail through the same
// injectable sender the alert command uses. A second successful save with no
// marker left must send nothing more.
func TestSaveCLIAutoSuccessSendsRecoveryMailWhenMarkerPresent(t *testing.T) {
	s := fakeSendmail(t)
	sock := tmuxctl.StartTestServer(t)
	cfgPath := writeConfig(t, `{"mail_to": "ops@example.com"}`)
	dataDir := t.TempDir()

	rl := mail.RateLimiter{Dir: dataDir}
	if !rl.ShouldSend("go-tmux-saver.service", time.Now()) {
		t.Fatal("setup: ShouldSend = false, want true (fresh marker)")
	}

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--auto", "--no-display", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.HasPrefix(out.String(), "kept") {
		t.Fatalf("stdout = %q, want prefix %q", out.String(), "kept")
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls = %d, want 1", s.count())
	}
	if !strings.Contains(s.last(), "go-tmux-saver.service recovered") || !strings.Contains(s.last(), "To: ops@example.com") {
		t.Fatalf("message = %q, want a recovered subject for the unit and To: ops@example.com", s.last())
	}
	if !strings.Contains(s.last(), "save succeeded:") {
		t.Fatalf("message = %q, want a body starting with %q", s.last(), "save succeeded:")
	}

	// The marker is gone now, so a second successful save must not send
	// another recovery mail.
	out.Reset()
	errb.Reset()
	code = Run([]string{"save", "--auto", "--no-display", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 0 {
		t.Fatalf("second save exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls after second save = %d, want still 1 (no marker to clear)", s.count())
	}
}

// TestSaveCLIAutoSuccessClearsWatchMarkerToo covers C3/RULING R46: the
// watch unit's alert marker was never cleared by anything, so after the
// first staleness mail the watchdog went permanently silent. A successful
// `save --auto` now clears BOTH unit markers and sends one recovery mail
// per marker that existed — and, the marker being gone, the NEXT failure
// streak for the watch unit mails again instead of being rate-limited
// forever.
func TestSaveCLIAutoSuccessClearsWatchMarkerToo(t *testing.T) {
	s := fakeSendmail(t)
	sock := tmuxctl.StartTestServer(t)
	cfgPath := writeConfig(t, `{"mail_to": "ops@example.com"}`)
	dataDir := t.TempDir()

	rl := mail.RateLimiter{Dir: dataDir}
	for _, unit := range []string{alertUnit, watchAlertUnit} {
		if !rl.ShouldSend(unit, time.Now()) {
			t.Fatalf("setup: ShouldSend(%s) = false, want true (fresh marker)", unit)
		}
	}

	var out, errb bytes.Buffer
	code := Run([]string{"save", "--auto", "--no-display", "--config", cfgPath, "--data-dir", dataDir, "--socket", sock}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 2 {
		t.Fatalf("sendmail calls = %d, want 2 (one recovery per cleared marker); bodies=%q", s.count(), s.all())
	}
	joined := strings.Join(s.all(), "\n")
	for _, unit := range []string{alertUnit, watchAlertUnit} {
		if !strings.Contains(joined, unit+" recovered") {
			t.Errorf("no recovery mail for %s in:\n%s", unit, joined)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "alert-"+unit)); !os.IsNotExist(err) {
			t.Errorf("marker for %s still present (stat err = %v)", unit, err)
		}
	}

	// Marker gone ⇒ the next watch failure is not rate-limited any more.
	if !rl.ShouldSend(watchAlertUnit, time.Now()) {
		t.Fatalf("ShouldSend(%s) after a successful save = false; the watchdog would stay silent forever", watchAlertUnit)
	}
}

// TestRunSaveSkipsWhenAnotherSaveHoldsTheLock covers I6/RULING R47: two
// saves against one data dir can collide (the 10-minute timer firing while
// a manual M-s save is still running on a 41-pane server). The loser must
// not run at all — concurrent Stage/Promote/prune against the same store,
// and EnsureDir's snap-*.tmp sweep, would otherwise fight over the same
// paths. It skips cleanly: a `skipped` event with detail "save in
// progress", no error, exit 0 for the caller.
func TestRunSaveSkipsWhenAnotherSaveHoldsTheLock(t *testing.T) {
	d := deps(t, saveFake())

	// Stand in for the other save: flock is per open file description, so
	// a second open in this same process is genuinely contended.
	release, ok, err := tryLockDataDir(d.Store.Dir)
	if err != nil || !ok {
		t.Fatalf("tryLockDataDir = %v %v, want the lock", ok, err)
	}
	defer release()

	o, err := RunSave(context.Background(), d)
	if err != nil {
		t.Fatalf("RunSave: %v, want a clean skip", err)
	}
	if o.Kind != "skipped" {
		t.Fatalf("Kind = %q, want %q", o.Kind, "skipped")
	}
	if len(d.T.(*tmuxctl.Fake).Calls) != 0 {
		t.Errorf("the losing save must not touch tmux at all, got calls %v", d.T.(*tmuxctl.Fake).Calls)
	}
	ev, err := snapshot.TailEvents(d.Store.Dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "skipped" || ev[0].Detail != "save in progress" {
		t.Fatalf("events %+v, want one skipped/save-in-progress event", ev)
	}

	// Once the other save finishes, the next one runs normally.
	release()
	o, err = RunSave(context.Background(), d)
	if err != nil || o.Kind != "kept" {
		t.Fatalf("after release: %+v %v, want a kept save", o, err)
	}
}

// TestRunSaveDoesNotDeleteALiveSaveStagingDir covers the other half of
// I6/RULING R47: the stale-snap-*.tmp sweep used to run in EnsureDir, on
// every subcommand's bring-up and OUTSIDE any lock, so it could delete the
// staging directory of a save that was still writing into it. The sweep now
// happens inside RunSave with the lock held, and EnsureDir leaves tmp dirs
// alone.
func TestRunSaveDoesNotDeleteALiveSaveStagingDir(t *testing.T) {
	d := deps(t, saveFake())
	live := filepath.Join(d.Store.Dir, "snap-20260822T120000Z.tmp")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}

	// The other save holds the lock, so this one skips without sweeping.
	release, ok, err := tryLockDataDir(d.Store.Dir)
	if err != nil || !ok {
		t.Fatalf("tryLockDataDir = %v %v", ok, err)
	}
	if o, err := RunSave(context.Background(), d); err != nil || o.Kind != "skipped" {
		t.Fatalf("RunSave = %+v %v, want skipped", o, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a skipping save must not delete the lock holder's staging dir: %v", err)
	}
	// EnsureDir (every subcommand's bring-up) must not delete it either.
	if err := d.Store.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("EnsureDir deleted a live save's staging dir: %v", err)
	}

	// With the lock free, the save owns the store and sweeps it.
	release()
	if o, err := RunSave(context.Background(), d); err != nil || o.Kind != "kept" {
		t.Fatalf("RunSave = %+v %v, want kept", o, err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("a save holding the lock should sweep stale staging dirs (stat err = %v)", err)
	}
}

// TestDisplayCmdUsesTmuxQuoting pins the minor R30 consistency fix: the
// in-tmux summary went out as Go's %q, which is not tmux's own
// double-quote syntax — tmux expands "$NAME" inside "…" and mangles
// Go-style \xNN escapes. A pane title or session name in the summary could
// therefore be expanded by tmux before display.
func TestDisplayCmdUsesTmuxQuoting(t *testing.T) {
	got := displayCmd(`saved $HOME "x"`)
	want := `display-message "go-tmux-saver: saved \$HOME \"x\""`
	if got != want {
		t.Fatalf("displayCmd = %q, want %q", got, want)
	}
}

// TestRunSaveUnreadableLastIsHardError pins the minor: Store.Last() failing
// for any reason OTHER than "no snapshot yet" (a corrupt layout.json, an
// unreadable store) used to be swallowed — the degenerate-snapshot guard
// silently compares against nothing and every save is accepted, exactly
// when the store is in a state that most warrants suspicion. It is now a
// hard error with an `error` event.
func TestRunSaveUnreadableLastIsHardError(t *testing.T) {
	d := deps(t, saveFake())
	snapDir := filepath.Join(d.Store.Dir, "snap-20260822T120000Z")
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "layout.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("snap-20260822T120000Z", filepath.Join(d.Store.Dir, "last")); err != nil {
		t.Fatal(err)
	}

	o, err := RunSave(context.Background(), d)
	if err == nil {
		t.Fatalf("RunSave = %+v, nil error; want a hard error rather than a silently unguarded save", o)
	}
	if o.Kind != "error" {
		t.Errorf("Kind = %q, want %q", o.Kind, "error")
	}
	if _, err := os.Lstat(filepath.Join(d.Store.Dir, "last")); err != nil {
		t.Errorf("the corrupt snapshot must be left alone for a human to look at: %v", err)
	}
	ev, err := snapshot.TailEvents(d.Store.Dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "error" {
		t.Fatalf("events %+v, want one error event", ev)
	}
}

// TestRunSaveDanglingLastIsHardError covers issue #3's save half: a `last`
// symlink pointing at a deleted snapshot must fail the save loudly, not
// read as "first save" (which would silently disable the unchanged-dedup
// and the degenerate guard).
func TestRunSaveDanglingLastIsHardError(t *testing.T) {
	d := deps(t, saveFake())
	if err := os.Symlink("snap-20260822T120000Z", filepath.Join(d.Store.Dir, "last")); err != nil {
		t.Fatal(err)
	}
	o, err := RunSave(context.Background(), d)
	if err == nil || !errors.Is(err, snapshot.ErrDanglingLast) {
		t.Fatalf("RunSave = %+v err=%v; want ErrDanglingLast", o, err)
	}
	if o.Kind != "error" {
		t.Errorf("Kind = %q, want %q", o.Kind, "error")
	}
}
