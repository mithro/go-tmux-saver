package setup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// timerUnits are the two systemd --user timers Install enables/starts and
// Update restarts.
var timerUnits = []string{"go-tmux-saver.timer", "go-tmux-saver-watch.timer"}

// WriteFiles atomically writes each of files under dir (ConfigHome-relative
// Rel paths), creating parent directories as needed. It performs no
// systemd/tmux side effects — Install uses it and then does those; the
// `setup generate` CLI subcommand uses it directly to materialize files
// into an arbitrary --dir without touching the user's real systemd state.
func WriteFiles(dir string, files []Managed) error {
	for _, f := range files {
		if err := writeManaged(filepath.Join(dir, f.Rel), f.Content, f.Mode); err != nil {
			return fmt.Errorf("setup: write %s: %w", f.Rel, err)
		}
	}
	return nil
}

// writeManaged atomically writes content to path with the given mode: write
// to path+".part", chmod it explicitly (os.WriteFile's mode is subject to
// umask), then rename over path.
func writeManaged(path string, content []byte, mode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	part := path + ".part"
	defer func() {
		if err != nil {
			os.Remove(part)
		}
	}()
	if err = os.WriteFile(part, content, mode); err != nil {
		return err
	}
	if err = os.Chmod(part, mode); err != nil {
		return err
	}
	return os.Rename(part, path)
}

// Install writes every file in files under env.ConfigHome, then makes
// systemd aware of them: `daemon-reload` (so it picks up the new/changed
// units and the tmux-server.service drop-in) followed by
// `enable --now` on both timers.
func Install(env Env, files []Managed) error {
	if err := WriteFiles(env.ConfigHome, files); err != nil {
		return err
	}
	if _, err := env.Systemctl("--user", "daemon-reload"); err != nil {
		return fmt.Errorf("setup: daemon-reload: %w", err)
	}
	args := append([]string{"--user", "enable", "--now"}, timerUnits...)
	if _, err := env.Systemctl(args...); err != nil {
		return fmt.Errorf("setup: enable timers: %w", err)
	}
	return nil
}

// Update rewrites the managed files that have drifted from files (missing,
// content-differs, or wrong mode), restarts the timers so any changed
// unit/timer content takes effect, and reports which Rel paths changed.
//
// config.json is special-cased: once it exists on disk it is never
// rewritten by Update (it is user-editable, generated fresh only by `setup
// generate`/`install` or when altogether missing) — this mirrors Validate,
// which likewise never content-diffs config.json.
//
// With dryRun, Update computes and reports the same changed list but writes
// nothing and issues no systemctl calls.
func Update(env Env, files []Managed, dryRun bool) (changed []string, err error) {
	var toWrite []Managed
	for _, f := range files {
		full := filepath.Join(env.ConfigHome, f.Rel)

		if f.Rel == RelConfigJSON {
			if _, statErr := os.Stat(full); statErr == nil {
				continue // never overwrite an existing config.json
			}
		}

		data, readErr := os.ReadFile(full)
		info, statErr := os.Stat(full)
		upToDate := readErr == nil && statErr == nil &&
			bytes.Equal(data, f.Content) && info.Mode().Perm() == f.Mode.Perm()
		if upToDate {
			continue
		}
		changed = append(changed, f.Rel)
		toWrite = append(toWrite, f)
	}

	if len(changed) == 0 {
		return nil, nil
	}

	if dryRun {
		for _, rel := range changed {
			fmt.Fprintf(env.Stdout, "would update: %s\n", rel)
		}
		return changed, nil
	}

	for _, rel := range changed {
		fmt.Fprintf(env.Stdout, "updating: %s\n", rel)
	}
	if err := Install(env, toWrite); err != nil {
		return changed, err
	}
	args := append([]string{"--user", "restart"}, timerUnits...)
	if _, err := env.Systemctl(args...); err != nil {
		return changed, fmt.Errorf("setup: restart timers: %w", err)
	}
	return changed, nil
}
