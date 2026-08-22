package snapshot

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func mk(t *testing.T, dir, name string) { t.Helper(); os.MkdirAll(filepath.Join(dir, name), 0o700) }

func TestPruneKeepsRecentDailyAndLast(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// 4 snapshots today at 10-minute spacing, 3 older days (one each), 1 ancient
	names := []string{"snap-20260822T110000Z", "snap-20260822T111000Z", "snap-20260822T112000Z", "snap-20260822T113000Z",
		"snap-20260821T090000Z", "snap-20260820T090000Z", "snap-20260819T090000Z", "snap-20260601T090000Z"}
	for _, n := range names {
		mk(t, dir, n)
	}
	os.Symlink("snap-20260822T113000Z", filepath.Join(dir, "last"))
	mk(t, dir, "rejected/snap-20260822T100000Z")
	mk(t, dir, "rejected/snap-20260822T100100Z")
	removed, err := Prune(dir, 2, 30, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for _, keep := range []string{"snap-20260822T113000Z", "snap-20260822T112000Z", // newest 2
		"snap-20260821T090000Z", "snap-20260820T090000Z", "snap-20260819T090000Z", // daily within 30d
		"rejected/snap-20260822T100100Z"} {
		if !exists(keep) {
			t.Errorf("%s should be kept", keep)
		}
	}
	for _, gone := range []string{"snap-20260822T110000Z", "snap-20260822T111000Z", "snap-20260601T090000Z", "rejected/snap-20260822T100000Z"} {
		if exists(gone) {
			t.Errorf("%s should be removed", gone)
		}
	}
	if len(removed) != 4 {
		t.Errorf("removed = %v", removed)
	}
}

// TestPruneCandidatesMatchesPruneDryRun pins PruneCandidates as Prune's
// read-only half: called against an identical (untouched) tree with the same
// arguments, it must report exactly what Prune would remove, without
// removing anything itself.
func TestPruneCandidatesMatchesPruneDryRun(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	names := []string{"snap-20260822T110000Z", "snap-20260822T111000Z", "snap-20260822T112000Z", "snap-20260822T113000Z",
		"snap-20260821T090000Z", "snap-20260601T090000Z"}
	for _, n := range names {
		mk(t, dir, n)
	}
	os.Symlink("snap-20260822T113000Z", filepath.Join(dir, "last"))
	mk(t, dir, "rejected/snap-20260822T100000Z")
	mk(t, dir, "rejected/snap-20260822T100100Z")

	candidates := PruneCandidates(dir, 2, 30, 1, now)

	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for _, n := range names {
		if !exists(n) {
			t.Fatalf("PruneCandidates must not remove anything, but %s is gone", n)
		}
	}
	if !exists("rejected/snap-20260822T100000Z") || !exists("rejected/snap-20260822T100100Z") {
		t.Fatal("PruneCandidates must not remove anything, but a rejected/ entry is gone")
	}

	removed, err := Prune(dir, 2, 30, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(candidates)
	sort.Strings(removed)
	if !reflect.DeepEqual(candidates, removed) {
		t.Fatalf("PruneCandidates = %v, Prune removed = %v; want equal", candidates, removed)
	}
}
