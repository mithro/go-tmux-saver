package snapshot

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func snapTime(name string) (time.Time, bool) {
	t, err := time.Parse(dirTimeFormat, strings.TrimPrefix(name, "snap-"))
	return t, err == nil
}

func listSnaps(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "snap-*"))
	var out []string
	for _, p := range m {
		if strings.HasSuffix(p, ".tmp") {
			continue
		}
		if _, ok := snapTime(filepath.Base(p)); ok {
			out = append(out, filepath.Base(p))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out))) // newest first (timestamps sort lexically)
	return out
}

// Prune removes snapshot dirs outside the retention policy. Never removes the
// `last` target. Returns the names removed.
func Prune(dir string, keep, dailyDays, rejectedKeep int, now time.Time) ([]string, error) {
	lastTarget, _ := os.Readlink(filepath.Join(dir, "last"))
	keepSet := map[string]bool{lastTarget: true}
	snaps := listSnaps(dir)
	for i, n := range snaps {
		if i < keep {
			keepSet[n] = true
		}
	}
	seenDay := map[string]bool{}
	cutoff := now.AddDate(0, 0, -dailyDays)
	for _, n := range snaps { // newest first → first per day wins
		t, _ := snapTime(n)
		day := t.Format("2006-01-02")
		if t.After(cutoff) && !seenDay[day] {
			seenDay[day] = true
			keepSet[n] = true
		}
	}
	var removed []string
	for _, n := range snaps {
		if !keepSet[n] {
			if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
				return removed, err
			}
			removed = append(removed, n)
		}
	}
	rej := listSnaps(filepath.Join(dir, "rejected"))
	for i, n := range rej {
		if i >= rejectedKeep {
			if err := os.RemoveAll(filepath.Join(dir, "rejected", n)); err != nil {
				return removed, err
			}
			removed = append(removed, "rejected/"+n)
		}
	}
	return removed, nil
}
