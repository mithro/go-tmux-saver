package cli

import (
	"bytes"
	"os"
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
