package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/mail"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// TestStatusCLICheckFresh covers the full status/--check-fresh/--json
// contract: two events are appended, a fresh marker is created and then
// backdated 2h (past the default 30-minute interval*watch_stale_factor
// limit) via os.Chtimes, and `status --check-fresh` must report STALE and
// exit 1. Re-touching the marker to now must flip it back to exit 0, and
// `--json` in that fresh state must parse with "stale": false.
func TestStatusCLICheckFresh(t *testing.T) {
	dataDir := t.TempDir()
	snapshot.AppendEvent(dataDir, snapshot.Event{Time: time.Now(), Outcome: "kept", Panes: 2})
	snapshot.AppendEvent(dataDir, snapshot.Event{Time: time.Now(), Outcome: "kept", Panes: 3})

	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	backdated := time.Now().Add(-2 * time.Hour)
	freshMarker := filepath.Join(dataDir, "fresh")
	if err := os.Chtimes(freshMarker, backdated, backdated); err != nil {
		t.Fatal(err)
	}

	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "STALE") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "STALE")
	}

	// Re-touch to now: no longer stale, exit 0.
	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	code = Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "STALE") {
		t.Fatalf("stdout = %q, should not contain %q now that the marker is fresh", out.String(), "STALE")
	}

	// --json parses and "stale" matches the (now fresh) state.
	out.Reset()
	errb.Reset()
	code = Run([]string{"status", "--json", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("json exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	var js struct {
		LastGood   string `json:"last_good"`
		AgeSeconds int64  `json:"age_seconds"`
		Stale      bool   `json:"stale"`
		Events     []struct {
			Outcome string `json:"outcome"`
		} `json:"events"`
		Snapshots int `json:"snapshots"`
	}
	if err := json.Unmarshal(out.Bytes(), &js); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if js.Stale {
		t.Fatalf("json stale = true, want false: %+v", js)
	}
	if js.LastGood == "" {
		t.Fatalf("json last_good empty, want set")
	}
	if len(js.Events) != 2 || js.Events[0].Outcome != "kept" || js.Events[1].Outcome != "kept" {
		t.Fatalf("json events = %+v, want 2 kept events", js.Events)
	}
}

// TestStatusCLINoMarkerStale covers the "LastGood absent" half of the STALE
// condition: with no fresh marker at all, --check-fresh must exit 1 and
// report STALE, and --json must report stale:true with an empty last_good.
func TestStatusCLINoMarkerStale(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "STALE") {
		t.Fatalf("stdout = %q, want to contain %q", out.String(), "STALE")
	}

	out.Reset()
	code = Run([]string{"status", "--json", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("json (no --check-fresh) exit %d, want 0", code)
	}
	var js struct {
		LastGood string `json:"last_good"`
		Stale    bool   `json:"stale"`
	}
	if err := json.Unmarshal(out.Bytes(), &js); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if !js.Stale {
		t.Fatalf("json stale = false, want true (no marker): %+v", js)
	}
	if js.LastGood != "" {
		t.Fatalf("json last_good = %q, want empty (no marker)", js.LastGood)
	}
}

// TestStatusCLISnapshotCount covers "data dir + snapshot count": creates two
// real snap-* dirs plus one stale snap-*.tmp dir (which must be excluded)
// and asserts the JSON "snapshots" field counts only the two real ones.
func TestStatusCLISnapshotCount(t *testing.T) {
	dataDir := t.TempDir()
	for _, n := range []string{"snap-20260822T110000Z", "snap-20260822T120000Z", "snap-20260822T130000Z.tmp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"status", "--json", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	var js struct {
		Snapshots int `json:"snapshots"`
	}
	if err := json.Unmarshal(out.Bytes(), &js); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out.String(), err)
	}
	if js.Snapshots != 2 {
		t.Fatalf("snapshots = %d, want 2 (excluding the .tmp dir)", js.Snapshots)
	}
}

// TestRunStatusDirect exercises RunStatus directly (rather than via Run) so
// the seam mandated by the brief is itself covered, and pins the STALE exit
// code driven purely by the `now` parameter rather than wall-clock time.
func TestRunStatusDirect(t *testing.T) {
	dataDir := t.TempDir()
	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	lastGood, _, _ := snapshot.LastGood(dataDir)

	cfg := config.Default()

	var out bytes.Buffer
	// now far enough past lastGood to exceed interval*watch_stale_factor.
	staleNow := lastGood.Add(time.Duration(cfg.IntervalMinutes*cfg.WatchStaleFactor+1) * time.Minute)
	code := RunStatus(&out, dataDir, cfg, false, true, 10, staleNow)
	if code != 1 {
		t.Fatalf("RunStatus at staleNow = %d, want 1; out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "STALE") {
		t.Fatalf("out = %q, want STALE", out.String())
	}

	out.Reset()
	code = RunStatus(&out, dataDir, cfg, false, true, 10, lastGood)
	if code != 0 {
		t.Fatalf("RunStatus at lastGood itself = %d, want 0; out=%q", code, out.String())
	}
}

// TestStatusCheckFreshClearsWatchMarker covers C3/RULING R46's second half:
// a NON-stale `status --check-fresh` (the watch unit's own success path)
// clears the watch unit's alert marker and mails one recovery, through the
// same injectable sender the alert subcommand uses. A stale run must clear
// nothing.
func TestStatusCheckFreshClearsWatchMarker(t *testing.T) {
	s := fakeSendmail(t)
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, `{"mail_to": "ops@example.com"}`)
	marker := filepath.Join(dataDir, "alert-"+watchAlertUnit)

	rl := mail.RateLimiter{Dir: dataDir}
	if !rl.ShouldSend(watchAlertUnit, time.Now()) {
		t.Fatal("setup: ShouldSend = false, want true (fresh marker)")
	}

	// Stale (no fresh marker at all): exit 1, and the alert marker survives.
	var out, errb bytes.Buffer
	if code := Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb); code != 1 {
		t.Fatalf("stale exit %d, want 1", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a STALE check must not clear the marker: %v", err)
	}
	if s.count() != 0 {
		t.Fatalf("sendmail calls on the stale path = %d, want 0", s.count())
	}

	// Fresh: exit 0, marker cleared, exactly one recovery mail.
	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("fresh exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fresh --check-fresh must clear the watch marker (stat err = %v)", err)
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls = %d, want 1", s.count())
	}
	if !strings.Contains(s.last(), watchAlertUnit+" recovered") || !strings.Contains(s.last(), "To: ops@example.com") {
		t.Fatalf("message = %q, want a recovered subject for %s to ops@example.com", s.last(), watchAlertUnit)
	}

	// A second fresh run has no marker to clear, so it must stay silent.
	out.Reset()
	if code := Run([]string{"status", "--check-fresh", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb); code != 0 {
		t.Fatalf("second fresh exit %d, want 0", code)
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls after a second fresh run = %d, want still 1", s.count())
	}
}
