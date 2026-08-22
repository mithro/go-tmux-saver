package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/procs"
)

func TestDefaultsAndLoad(t *testing.T) {
	d := Default()
	if d.Socket != "main" || d.IntervalMinutes != 10 || d.Guard.MinPanes != 5 || d.Guard.Divisor != 3 ||
		d.Contents.Codec != "gzip" || !d.Contents.Enabled || d.Retention.Keep != 50 || d.SeedSession != "default" || d.SeedWindow != "h" {
		t.Fatalf("defaults %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"interval_minutes": 5, "guard": {"divisor": 4}, "contents": {"codec": "nope"}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.IntervalMinutes != 5 || c.Guard.Divisor != 4 || c.Guard.MinPanes != 5 || c.Socket != "main" {
		t.Fatalf("overlay %+v", c)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown codec must fail validation")
	}
	if c, err := Load(filepath.Join(t.TempDir(), "missing.json")); err != nil || c.Socket != "main" {
		t.Fatalf("missing file should yield defaults: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", "/x")
	if DataDir() != "/x/go-tmux-saver" {
		t.Fatal(DataDir())
	}
}

func TestPathAndDataDirXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xc")
	if got, want := Path(), "/xc/go-tmux-saver/config.json"; got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got, want := Path(), "/home/u/.config/go-tmux-saver/config.json"; got != want {
		t.Fatalf("Path() with empty XDG_CONFIG_HOME = %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "")
	if got, want := DataDir(), "/home/u/.local/share/go-tmux-saver"; got != want {
		t.Fatalf("DataDir() with empty XDG_DATA_HOME = %q, want %q", got, want)
	}
}

func TestLoadEmptyAllowlistOverlay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"allowlist": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Allowlist) != 0 {
		t.Fatalf("Allowlist = %v, want empty", c.Allowlist)
	}
	err = c.Validate()
	if err == nil {
		t.Fatal("empty allowlist must fail validation")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error %q does not mention allowlist", err)
	}
}

func TestValidateBranches(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantKey string
	}{
		{"interval_minutes", func(c *Config) { c.IntervalMinutes = 0 }, "interval_minutes"},
		{"guard.divisor", func(c *Config) { c.Guard.Divisor = 1 }, "guard.divisor"},
		{"guard.min_panes", func(c *Config) { c.Guard.MinPanes = 0 }, "guard.min_panes"},
		{"retention.keep", func(c *Config) { c.Retention.Keep = 0 }, "retention.keep"},
		{"contents.codec", func(c *Config) { c.Contents.Codec = "nope" }, "contents.codec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf("error %q does not mention %q", err, tc.wantKey)
			}
		})
	}
}

func TestDefaultAllowlistNotAliased(t *testing.T) {
	orig := procs.DefaultAllowlist[0]
	d := Default()
	d.Allowlist[0] = "mutated-should-not-leak"
	if procs.DefaultAllowlist[0] != orig {
		t.Fatalf("procs.DefaultAllowlist[0] mutated: got %q, want %q", procs.DefaultAllowlist[0], orig)
	}
}
