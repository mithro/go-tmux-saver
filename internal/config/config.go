// Package config loads and validates go-tmux-saver's configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// Config holds all tunables for go-tmux-saver.
type Config struct {
	Socket           string   `json:"socket"`             // "main"
	SeedSession      string   `json:"seed_session"`       // "default"
	SeedWindow       string   `json:"seed_window"`        // "h"
	IntervalMinutes  int      `json:"interval_minutes"`   // 10
	WatchStaleFactor int      `json:"watch_stale_factor"` // 3
	Allowlist        []string `json:"allowlist"`
	Guard            struct {
		MinPanes int `json:"min_panes"`
		Divisor  int `json:"divisor"`
	} `json:"guard"`
	Contents struct {
		Enabled bool   `json:"enabled"`
		Codec   string `json:"codec"`
	} `json:"contents"`
	Retention struct {
		Keep      int `json:"keep"`
		DailyDays int `json:"daily_days"`
		Rejected  int `json:"rejected"`
	} `json:"retention"`
	MailTo           string `json:"mail_to"`            // $USER
	ClaudeResumePath string `json:"claude_resume_path"` // "" = built-in `go-tmux-saver claude-resume`; set to use an external script
	DataDir          string `json:"-"`                  // derived: $XDG_DATA_HOME/go-tmux-saver
}

// Default returns the built-in default configuration.
func Default() Config {
	var c Config
	c.Socket = "main"
	c.SeedSession = "default"
	c.SeedWindow = "h"
	c.IntervalMinutes = 10
	c.WatchStaleFactor = 3
	c.Allowlist = append([]string(nil), procs.DefaultAllowlist...)
	c.Guard.MinPanes = 5
	c.Guard.Divisor = 3
	c.Contents.Enabled = true
	c.Contents.Codec = "gzip"
	c.Retention.Keep = 50
	c.Retention.DailyDays = 30
	c.Retention.Rejected = 20
	c.MailTo = os.Getenv("USER")
	c.ClaudeResumePath = "" // built-in claude-resume subcommand
	c.DataDir = DataDir()
	return c
}

// Path returns the default config file location: $XDG_CONFIG_HOME/go-tmux-saver/config.json
// (falling back to ~/.config when XDG_CONFIG_HOME is unset).
func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(base, "go-tmux-saver", "config.json")
}

// DataDir returns the default data directory: $XDG_DATA_HOME/go-tmux-saver
// (falling back to ~/.local/share when XDG_DATA_HOME is unset).
func DataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "share")
	}
	return filepath.Join(base, "go-tmux-saver")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return os.Getenv("HOME")
}

// Load reads the JSON config file at path and overlays it onto Default().
// A missing file is not an error — Default() is returned as-is.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("config: parse %s: %w", path, err)
	}
	c.DataDir = DataDir()
	return c, nil
}

// Validate checks that the configuration's values are sane, returning a
// descriptive error naming the offending key on failure.
func (c Config) Validate() error {
	if c.IntervalMinutes < 1 {
		return fmt.Errorf("config: interval_minutes must be >= 1, got %d", c.IntervalMinutes)
	}
	if c.Guard.Divisor < 2 {
		return fmt.Errorf("config: guard.divisor must be >= 2, got %d", c.Guard.Divisor)
	}
	if c.Guard.MinPanes < 1 {
		return fmt.Errorf("config: guard.min_panes must be >= 1, got %d", c.Guard.MinPanes)
	}
	if _, ok := snapshot.LookupCodec(c.Contents.Codec); !ok {
		return fmt.Errorf("config: contents.codec %q is not registered", c.Contents.Codec)
	}
	if len(c.Allowlist) == 0 {
		return fmt.Errorf("config: allowlist must be non-empty")
	}
	if c.Retention.Keep < 1 {
		return fmt.Errorf("config: retention.keep must be >= 1, got %d", c.Retention.Keep)
	}
	return nil
}
