package config

import (
	"os"
	"path/filepath"
	"testing"
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
