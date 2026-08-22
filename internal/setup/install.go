package setup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// timerUnits are the two systemd --user timers Install enables/starts and
// Update restarts.
var timerUnits = []string{"go-tmux-saver.timer", "go-tmux-saver-watch.timer"}

// WriteFiles atomically writes each of files under dir (ConfigHome-relative
// Rel paths), creating parent directories as needed (RULING R32: the
// go-tmux-saver/ directory — which holds the 0600 config.json — is created
// 0700; systemd/user/ and the tmux-server.service.d/ drop-in directory stay
// 0755). It performs no systemd/tmux side effects — Install uses it and
// then does those; the `setup generate` CLI subcommand uses it directly to
// materialize files into an arbitrary --dir without touching the user's
// real systemd state.
func WriteFiles(dir string, files []Managed) error {
	for _, f := range files {
		full := filepath.Join(dir, f.Rel)
		if err := ensureManagedDir(filepath.Dir(full), dirModeFor(f.Rel)); err != nil {
			return fmt.Errorf("setup: mkdir for %s: %w", f.Rel, err)
		}
		if err := writeManaged(full, f.Content, f.Mode); err != nil {
			return fmt.Errorf("setup: write %s: %w", f.Rel, err)
		}
	}
	return nil
}

// dirModeFor returns the mode WriteFiles creates rel's parent directory
// with (RULING R32): go-tmux-saver/ is 0700, everything else (systemd/user/
// and its tmux-server.service.d/ drop-in subdirectory) is 0755.
func dirModeFor(rel string) os.FileMode {
	if strings.HasPrefix(rel, "go-tmux-saver/") {
		return 0o700
	}
	return 0o755
}

// ensureManagedDir creates dir (and any missing parents) so dir itself ends
// up at exactly mode. Plain os.MkdirAll(dir, mode) can't guarantee this: it
// applies mode to every directory it creates along the way — wrong here,
// since dir's parents (e.g. ConfigHome, systemd/user/) must stay at the
// default 0755 regardless of dir's own mode — and mode itself is subject to
// umask. So parents are created at 0755 first (a no-op if they already
// exist), then dir is created (or left alone if it exists) and explicitly
// chmod'd to mode.
func ensureManagedDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chmod(dir, mode)
}

// writeManaged atomically writes content to path with the given mode: write
// to path+".part", chmod it explicitly (os.WriteFile's mode is subject to
// umask), then rename over path. It assumes path's parent directory already
// exists (see ensureManagedDir).
func writeManaged(path string, content []byte, mode os.FileMode) (err error) {
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

// readManagedFile reads the file that should hold f's content at
// env.ConfigHome/f.Rel, returning its bytes and mode. Validate and Update
// share this as their "what's actually on disk" read; they differ only in
// how they interpret the result (Validate reports missing/differs/mode
// Drifts and parses+Validates config.json instead of byte-comparing it;
// Update treats an up-to-date file as nothing-to-do and never even calls
// this for an existing config.json).
func readManagedFile(env Env, f Managed) (data []byte, mode os.FileMode, err error) {
	full := filepath.Join(env.ConfigHome, f.Rel)
	data, err = os.ReadFile(full)
	if err != nil {
		return nil, 0, err
	}
	if info, statErr := os.Stat(full); statErr == nil {
		mode = info.Mode().Perm()
	}
	return data, mode, nil
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
// With dryRun, Update computes and prints the same changed list — each
// preceded by a "would update: <rel>" line and followed by the same
// diffLines output Validate's "differs" Drift carries — but writes nothing
// and issues no systemctl calls.
func Update(env Env, files []Managed, dryRun bool) (changed []string, err error) {
	var toWrite []Managed
	diffs := make(map[string]string)
	for _, f := range files {
		full := filepath.Join(env.ConfigHome, f.Rel)

		if f.Rel == RelConfigJSON {
			if _, statErr := os.Stat(full); statErr == nil {
				continue // never overwrite an existing config.json
			}
		}

		data, mode, readErr := readManagedFile(env, f)
		upToDate := readErr == nil && bytes.Equal(data, f.Content) && mode == f.Mode.Perm()
		if upToDate {
			continue
		}
		changed = append(changed, f.Rel)
		toWrite = append(toWrite, f)
		diffs[f.Rel] = diffLines(string(f.Content), string(data))
	}

	if len(changed) == 0 {
		return nil, nil
	}

	if dryRun {
		for _, rel := range changed {
			fmt.Fprintf(env.Stdout, "would update: %s\n", rel)
			fmt.Fprint(env.Stdout, diffs[rel])
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
