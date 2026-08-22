package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

const (
	resurrectFixture = "../importer/testdata/tmux_resurrect_sample.txt"
	resurrectTar     = "../importer/testdata/pane_contents.tar.gz"
)

// TestImportResurrectCLIPromotesByDefault covers `import-resurrect
// <savefile> --contents <tar>` against the shared fixture (see
// internal/importer/resurrect_test.go for the format it exercises): by
// default the converted snapshot is staged AND promoted, so it becomes
// `last` — the first restore after switching from tmux-resurrect has data.
func TestImportResurrectCLIPromotesByDefault(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", resurrectFixture, "--contents", resurrectTar, "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "imported 2 sessions, 3 windows, 4 panes (3 with contents)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}

	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: dataDir, Codec: gz}
	snap, dir, err := st.Last()
	if err != nil {
		t.Fatalf("Store.Last() after promote: %v", err)
	}
	panes, windows := snap.CountPanes()
	if len(snap.Sessions) != 2 || windows != 3 || panes != 4 {
		t.Fatalf("promoted snapshot sessions=%d windows=%d panes=%d, want 2/3/4", len(snap.Sessions), windows, panes)
	}
	if snap.TmuxVersion != "imported-resurrect" {
		t.Fatalf("tmux version = %q", snap.TmuxVersion)
	}
	// Contents were staged too: at least one pane's content file exists on
	// disk under the promoted snapshot dir.
	entries, err := os.ReadDir(dir + "/panes")
	if err != nil {
		t.Fatalf("read panes dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("panes dir has %d files, want 3", len(entries))
	}
}

// TestImportResurrectCLINoPromoteDiscards covers `--promote=false`: the
// summary is still printed, but the staged snapshot is discarded rather
// than becoming `last`.
func TestImportResurrectCLINoPromoteDiscards(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", resurrectFixture, "--contents", resurrectTar, "--promote=false", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "imported 2 sessions, 3 windows, 4 panes (3 with contents)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}

	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: dataDir, Codec: gz}
	if _, _, err := st.Last(); !os.IsNotExist(err) {
		t.Fatalf("Store.Last() after --promote=false: err = %v, want ErrNotExist", err)
	}
}

// TestImportResurrectCLINoContentsFlag covers omitting --contents entirely:
// import must still succeed, with a "(0 with contents)" summary.
func TestImportResurrectCLINoContentsFlag(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", resurrectFixture, "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "imported 2 sessions, 3 windows, 4 panes (0 with contents)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}
}

// TestImportResurrectCLISavefileAfterFlags covers RULING R36: savefile
// LAST, flags first (`import-resurrect --contents TAR --data-dir DIR
// <savefile>`) — this ordering used to misparse (the tar path could be
// mistaken for the savefile); it must now work identically to savefile-first.
func TestImportResurrectCLISavefileAfterFlags(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", "--contents", resurrectTar, "--config", cfgPath, "--data-dir", dataDir, resurrectFixture}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "imported 2 sessions, 3 windows, 4 panes (3 with contents)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}

	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: dataDir, Codec: gz}
	snap, _, err := st.Last()
	if err != nil {
		t.Fatalf("Store.Last() after promote: %v", err)
	}
	panes, windows := snap.CountPanes()
	if len(snap.Sessions) != 2 || windows != 3 || panes != 4 {
		t.Fatalf("promoted snapshot sessions=%d windows=%d panes=%d, want 2/3/4", len(snap.Sessions), windows, panes)
	}
}

// TestImportResurrectCLIMissingSavefile covers no positional savefile at
// all (only flags): exit 2, usage printed to stderr, nothing staged.
func TestImportResurrectCLIMissingSavefile(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", "--contents", resurrectTar, "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "usage: import-resurrect") {
		t.Fatalf("stderr = %q, want usage message", errb.String())
	}

	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: dataDir, Codec: gz}
	if _, _, err := st.Last(); !os.IsNotExist(err) {
		t.Fatalf("Store.Last() after missing-savefile error: err = %v, want ErrNotExist", err)
	}
}

// malformedResurrectFixture writes a one-warning save file (one well-formed
// pane/window/state record plus one wholly unrecognized line) to a temp
// file and returns its path — shared by the RULING R37 tests below.
func malformedResurrectFixture(t *testing.T) string {
	t.Helper()
	content := strings.Join([]string{
		"pane\tops\t0\t1\t:*\t0\tt\t:/x\t1\tssh\t:ssh h",
		"this is not a resurrect line at all",
		"window\tops\t0\t:w\t1\t:*\tlay,1\toff",
		"state\tops\tops",
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "malformed.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestImportResurrectCLIWarningsSuffixAndStderr covers RULING R37's
// best-effort default: a savefile with one skipped line still exits 0, the
// skipped line is reported on stderr, and the stdout summary gains a
// " (1 lines skipped)" suffix.
func TestImportResurrectCLIWarningsSuffixAndStderr(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")
	fixture := malformedResurrectFixture(t)

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", fixture, "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "imported 1 sessions, 1 windows, 1 panes (0 with contents) (1 lines skipped)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}
	if !strings.Contains(errb.String(), "warning:") || !strings.Contains(errb.String(), "line 2:") {
		t.Fatalf("stderr = %q, want a warning mentioning line 2", errb.String())
	}
}

// TestImportResurrectCLIStrictExitsNonzero covers RULING R37's --strict:
// the same skipped-line savefile now exits 1 (the summary and warnings are
// still printed, and the snapshot is still staged/promoted — --strict only
// changes the exit code).
func TestImportResurrectCLIStrictExitsNonzero(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")
	fixture := malformedResurrectFixture(t)

	var out, errb bytes.Buffer
	code := Run([]string{"import-resurrect", fixture, "--strict", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "(1 lines skipped)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("stdout = %q, want it to contain %q", out.String(), want)
	}
	if !strings.Contains(errb.String(), "warning:") {
		t.Fatalf("stderr = %q, want a warning", errb.String())
	}

	// --strict doesn't stop staging/promotion — the import still happened.
	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: dataDir, Codec: gz}
	if _, _, err := st.Last(); err != nil {
		t.Fatalf("Store.Last() after --strict exit: %v, want the snapshot still promoted", err)
	}
}

// TestImportResurrectPromoteLogsEventAndTouchesFresh pins the minor:
// rollout step 2 is "import your tmux-resurrect save", and `status`
// immediately afterwards reported STALE with no events at all, because the
// import promoted a snapshot without recording anything. A promoting import
// now logs a `kept` event with detail "import-resurrect" and touches the
// freshness marker; a --promote=false run (which discards its staging dir)
// must do neither.
func TestImportResurrectPromoteLogsEventAndTouchesFresh(t *testing.T) {
	newStore := func() *snapshot.Store {
		t.Helper()
		gz, _ := snapshot.LookupCodec("gzip")
		st := &snapshot.Store{Dir: t.TempDir(), Codec: gz}
		if err := st.EnsureDir(); err != nil {
			t.Fatal(err)
		}
		return st
	}

	noPromote := newStore()
	var out, errb bytes.Buffer
	if code := RunImportResurrect(&out, &errb, noPromote, resurrectFixture, "", "/bin/claude-resume", false, false); code != 0 {
		t.Fatalf("--promote=false exit %d: %s", code, errb.String())
	}
	if ev, _ := snapshot.TailEvents(noPromote.Dir, 10); len(ev) != 0 {
		t.Errorf("a non-promoting import must log nothing, got %+v", ev)
	}
	if _, ok, _ := snapshot.LastGood(noPromote.Dir); ok {
		t.Error("a non-promoting import must not touch the freshness marker")
	}

	promoted := newStore()
	out.Reset()
	errb.Reset()
	if code := RunImportResurrect(&out, &errb, promoted, resurrectFixture, "", "/bin/claude-resume", true, false); code != 0 {
		t.Fatalf("--promote exit %d: %s", code, errb.String())
	}
	ev, err := snapshot.TailEvents(promoted.Dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "kept" || ev[0].Detail != "import-resurrect" {
		t.Fatalf("events %+v, want one kept/import-resurrect event", ev)
	}
	if ev[0].Panes != 4 || ev[0].Sessions != 2 {
		t.Errorf("event counts %+v, want the imported snapshot's 4 panes / 2 sessions", ev[0])
	}
	if _, ok, _ := snapshot.LastGood(promoted.Dir); !ok {
		t.Error("a promoting import must touch the freshness marker so status isn't instantly STALE")
	}
}
