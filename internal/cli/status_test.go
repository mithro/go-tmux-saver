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

// TestRunStatusStaleTextExact pins the exact STALE line format (issue #8:
// previously untested) — the watch alert and humans both read it.
func TestRunStatusStaleTextExact(t *testing.T) {
	dataDir := t.TempDir()
	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	backdated := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dataDir, "fresh"), backdated, backdated); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default() // interval 10m × factor 3 = 30m limit
	var buf bytes.Buffer
	rc := RunStatus(&buf, dataDir, cfg, false, true, 5, now)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	if !strings.Contains(buf.String(), "STALE: last good save 2h0m0s ago (limit 30m0s)\n") {
		t.Fatalf("output = %q, want the exact STALE line", buf.String())
	}
}

// TestRunStatusJSONCheckFreshStale covers the --json --check-fresh stale
// combo (issue #8): exit 1 with machine-parseable output — stale:true in
// the JSON, and no human STALE line mixed into the stream.
func TestRunStatusJSONCheckFreshStale(t *testing.T) {
	dataDir := t.TempDir() // no fresh marker at all → stale
	var buf bytes.Buffer
	rc := RunStatus(&buf, dataDir, config.Default(), true, true, 5, time.Now())
	if rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	var rep struct {
		Stale bool `json:"stale"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("output not clean JSON: %v\n%s", err, buf.String())
	}
	if !rep.Stale {
		t.Fatal("stale = false, want true")
	}
	if strings.Contains(buf.String(), "STALE:") {
		t.Fatalf("human STALE line leaked into JSON output: %s", buf.String())
	}
}

// TestRunStatusAgeEqualsLimitIsFresh pins the boundary (issue #8): an age
// exactly equal to the limit is still fresh — staleness requires age >
// limit, so a save landing exactly on the boundary doesn't flap the alert.
func TestRunStatusAgeEqualsLimitIsFresh(t *testing.T) {
	dataDir := t.TempDir()
	if err := snapshot.TouchFresh(dataDir); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	limit := time.Duration(cfg.IntervalMinutes*cfg.WatchStaleFactor) * time.Minute
	now := time.Now()
	exact := now.Add(-limit)
	if err := os.Chtimes(filepath.Join(dataDir, "fresh"), exact, exact); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if rc := RunStatus(&buf, dataDir, cfg, false, true, 5, now); rc != 0 {
		t.Fatalf("rc = %d, want 0 (age == limit is fresh)\n%s", rc, buf.String())
	}
}

// TestRunStatusSurfacesReadErrors covers issue #8's swallowed-error item:
// an unreadable events.log and an unlistable data dir must produce visible
// warnings (and a JSON warnings field), not silently report "no events" /
// "0 snapshots".
func TestRunStatusSurfacesReadErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission-based test is meaningless as root")
	}
	dataDir := t.TempDir()
	// events.log as a DIRECTORY → open succeeds, read fails (EISDIR).
	if err := os.MkdirAll(filepath.Join(dataDir, "events.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Write-and-traverse-only data dir → ReadDir (snapshot count) fails,
	// while path traversal to fresh/events.log still works.
	if err := os.Chmod(dataDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dataDir, 0o700) })

	var buf bytes.Buffer
	RunStatus(&buf, dataDir, config.Default(), false, false, 5, time.Now())
	out := buf.String()
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "events.log") {
		t.Errorf("text output lacks an events.log warning:\n%s", out)
	}
	if !strings.Contains(out, "snapshot") && !strings.Contains(out, "data dir") {
		t.Errorf("text output lacks a data-dir/snapshot warning:\n%s", out)
	}

	buf.Reset()
	RunStatus(&buf, dataDir, config.Default(), true, false, 5, time.Now())
	var rep struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
	if len(rep.Warnings) == 0 {
		t.Fatalf("JSON warnings empty, want the read errors surfaced:\n%s", buf.String())
	}
}
