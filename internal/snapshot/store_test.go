package snapshot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func twoPaneSnap(ts time.Time) *Snapshot {
	return &Snapshot{Schema: 1, TakenAt: ts, Sessions: []Session{{Name: "s", Windows: []Window{{Index: 0, Name: "w",
		Panes: []Pane{{Index: 0, ID: "%1", Restore: Restore{Kind: "shell"}}, {Index: 1, ID: "%2", Restore: Restore{Kind: "shell"}}}}}}}}
}

func nlink(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Nlink)
}

func TestStagePromoteHardlinkAndLast(t *testing.T) {
	gz, _ := LookupCodec("gzip")
	st := &Store{Dir: t.TempDir(), Codec: gz}
	if err := st.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Last(); !os.IsNotExist(err) {
		t.Fatalf("empty store Last err = %v", err)
	}
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	c1 := map[string][]byte{"s_0_0": []byte("AAA"), "s_0_1": []byte("BBB")}
	stg, err := st.Stage(twoPaneSnap(t1), c1)
	if err != nil {
		t.Fatal(err)
	}
	dir1, err := stg.Promote()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir1) != "snap-20260822T100000Z" {
		t.Fatalf("dir name %s", dir1)
	}
	if _, err := os.Stat(filepath.Join(dir1, "layout.json")); err != nil {
		t.Fatal(err)
	}
	last, lastDir, err := st.Last()
	if err != nil || lastDir != dir1 || last.Sessions[0].Windows[0].Panes[0].ContentSHA256 == "" {
		t.Fatalf("Last = %+v %s %v", last, lastDir, err)
	}
	got, err := st.ReadContent(dir1, last.Sessions[0].Windows[0].Panes[1])
	if err != nil || string(got) != "BBB" {
		t.Fatalf("ReadContent = %q %v", got, err)
	}

	// second snapshot: pane 0 unchanged (→ hardlink), pane 1 changed (→ new file)
	t2 := t1.Add(10 * time.Minute)
	c2 := map[string][]byte{"s_0_0": []byte("AAA"), "s_0_1": []byte("CCC")}
	stg2, _ := st.Stage(twoPaneSnap(t2), c2)
	dir2, _ := stg2.Promote()
	f0 := filepath.Join(dir2, "panes", "s_0_0.txt.gz")
	if nlink(t, f0) != 2 {
		t.Fatalf("unchanged pane should be hardlinked (nlink=2), got %d", nlink(t, f0))
	}
	if nlink(t, filepath.Join(dir2, "panes", "s_0_1.txt.gz")) != 1 {
		t.Fatal("changed pane must be a fresh file")
	}
	// removing the old snapshot keeps the shared file alive
	os.RemoveAll(dir1)
	if got, _ := st.ReadContent(dir2, last.Sessions[0].Windows[0].Panes[0]); string(got) != "AAA" {
		t.Fatalf("content lost after pruning old dir: %q", got)
	}
	if _, lastDir, _ = st.Last(); lastDir != dir2 {
		t.Fatal("last not updated")
	}
	if fi, _ := os.Stat(st.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
}

func TestRejectAndDiscardLeaveLastAlone(t *testing.T) {
	gz, _ := LookupCodec("gzip")
	st := &Store{Dir: t.TempDir(), Codec: gz}
	st.EnsureDir()
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	stg, _ := st.Stage(twoPaneSnap(t1), map[string][]byte{"s_0_0": []byte("A"), "s_0_1": []byte("B")})
	dir1, _ := stg.Promote()
	stg2, _ := st.Stage(twoPaneSnap(t1.Add(time.Minute)), map[string][]byte{})
	rdir, err := stg2.Reject()
	if err != nil || filepath.Dir(rdir) != filepath.Join(st.Dir, "rejected") {
		t.Fatalf("reject = %s %v", rdir, err)
	}
	stg3, _ := st.Stage(twoPaneSnap(t1.Add(2*time.Minute)), map[string][]byte{})
	if err := stg3.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, lastDir, _ := st.Last(); lastDir != dir1 {
		t.Fatal("reject/discard must not move last")
	}
	if m, _ := filepath.Glob(filepath.Join(st.Dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("tmp dirs left: %v", m)
	}
}
