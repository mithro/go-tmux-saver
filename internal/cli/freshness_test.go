package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// freshCfg pins the thresholds the freshness tests reason about: interval
// 10m, warn at 2× (20m), stale at 3× (30m), indicator on.
func freshCfg() config.Config {
	c := config.Default()
	c.IntervalMinutes = 10
	c.WarnStaleFactor = 2
	c.WatchStaleFactor = 3
	c.StatusIndicator = true
	return c
}

// setLastGood writes the fresh marker and backdates it to age before now.
func setLastGood(t *testing.T, dir string, age time.Duration, now time.Time) {
	t.Helper()
	if err := snapshot.TouchFresh(dir); err != nil {
		t.Fatal(err)
	}
	ts := now.Add(-age)
	if err := os.Chtimes(filepath.Join(dir, "fresh"), ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessTmuxTiers(t *testing.T) {
	now := time.Now()
	cfg := freshCfg()
	cases := []struct {
		name       string
		age        time.Duration
		wantText   string // empty => the whole token must be empty
		wantColour string
	}{
		{"fresh", 5 * time.Minute, "", ""},
		{"just under warn", 19 * time.Minute, "", ""},
		{"warn", 25 * time.Minute, "save 25m", "fg=yellow"},
		{"stale", 45 * time.Minute, "save 45m", "fg=red"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setLastGood(t, dir, tc.age, now)
			got := freshnessTmux(dir, cfg, now)
			if tc.wantText == "" {
				if got != "" {
					t.Fatalf("want empty token for age %v, got %q", tc.age, got)
				}
				return
			}
			if !strings.Contains(got, tc.wantText) || !strings.Contains(got, tc.wantColour) {
				t.Fatalf("token %q missing %q / %q", got, tc.wantText, tc.wantColour)
			}
			if !strings.HasSuffix(got, "#[default]") {
				t.Errorf("token %q must reset style with #[default]", got)
			}
		})
	}
}

func TestFreshnessTmuxNoSaveAndDisabled(t *testing.T) {
	now := time.Now()
	cfg := freshCfg()

	// No marker at all: a red "no save" warning.
	if got := freshnessTmux(t.TempDir(), cfg, now); !strings.Contains(got, "no save") || !strings.Contains(got, "fg=red") {
		t.Fatalf("no-save token = %q, want a red 'no save'", got)
	}

	// Indicator disabled: always empty, even when very stale.
	cfg.StatusIndicator = false
	dir := t.TempDir()
	setLastGood(t, dir, 3*time.Hour, now)
	if got := freshnessTmux(dir, cfg, now); got != "" {
		t.Fatalf("disabled indicator must emit nothing, got %q", got)
	}
}

func TestFreshnessPlain(t *testing.T) {
	now := time.Now()
	cfg := freshCfg()

	dir := t.TempDir()
	setLastGood(t, dir, 45*time.Minute, now)
	if got := freshnessPlain(dir, cfg, now); !strings.HasPrefix(got, "stale ") {
		t.Fatalf("plain(stale) = %q, want a 'stale ...' line", got)
	}
	fresh := t.TempDir()
	setLastGood(t, fresh, 3*time.Minute, now)
	if got := freshnessPlain(fresh, cfg, now); !strings.HasPrefix(got, "fresh ") {
		t.Fatalf("plain(fresh) = %q, want a 'fresh ...' line", got)
	}
	if got := freshnessPlain(t.TempDir(), cfg, now); got != "no save" {
		t.Fatalf("plain(no marker) = %q, want 'no save'", got)
	}
}

func TestCompactAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{90 * time.Minute, "1h"},
		{50 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := compactAge(tc.d); got != tc.want {
			t.Errorf("compactAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
