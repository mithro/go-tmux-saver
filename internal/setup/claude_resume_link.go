package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// knownClaudeResumeSHA256 lists the content checksums of PREVIOUS
// claude-resume implementations that setup may replace with the symlink to
// this binary. Anything else sitting at the claude-resume path — an unknown
// script, a foreign binary, a symlink to some other tool — is the user's
// and is never touched.
var knownClaudeResumeSHA256 = map[string]bool{
	// rcfiles bin/claude-resume: the stdlib-python placeholder script this
	// binary's `claude-resume` subcommand is a port of (as deployed
	// 2026-06 → 2026-08).
	"7bfc1cbdd1e6c0df044a9838ea50c4d692201d0bf3920243eead1fe59055ce06": true,
}

// linkAction is what EnsureClaudeResumeLink decided about the claude-resume
// path.
type linkAction int

const (
	linkOK      linkAction = iota // already the wanted symlink
	linkCreate                    // nothing there → create the symlink
	linkReplace                   // a replaceable predecessor → re-point it
	linkForeign                   // unknown item → leave it alone
)

// classifyClaudeResume inspects path and decides whether setup owns it.
// Replaceable predecessors are exactly: a broken symlink; a symlink whose
// target is a go-tmux-saver binary (an old install location); and a
// script/file — plain or reached through the symlink — whose content
// checksum is in knownClaudeResumeSHA256. binary is the wanted target.
func classifyClaudeResume(path, binary string) (linkAction, string) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return linkCreate, "absent"
		}
		return linkForeign, "unreadable: " + err.Error()
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		dest, err := os.Readlink(path)
		if err != nil {
			return linkForeign, "unreadable symlink: " + err.Error()
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		if dest == binary {
			return linkOK, ""
		}
		if _, err := os.Stat(path); err != nil {
			return linkReplace, "broken symlink -> " + dest
		}
		if filepath.Base(dest) == "go-tmux-saver" {
			return linkReplace, "symlink to old go-tmux-saver binary " + dest
		}
		if sum, ok := fileSHA256(path); ok && knownClaudeResumeSHA256[sum] {
			return linkReplace, "symlink to known claude-resume script " + dest
		}
		return linkForeign, "symlink to a different tool: " + dest
	}

	if fi.Mode().IsRegular() {
		if sum, ok := fileSHA256(path); ok && knownClaudeResumeSHA256[sum] {
			return linkReplace, "known claude-resume script"
		}
		return linkForeign, "unknown file (not a known claude-resume script)"
	}
	return linkForeign, "not a file or symlink"
}

// fileSHA256 hashes path's content (following symlinks).
func fileSHA256(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}

// EnsureClaudeResumeLink makes env.ClaudeResumeLink a symlink to env.Binary
// when the path is absent or holds a replaceable predecessor (see
// classifyClaudeResume), and leaves anything unknown strictly alone.
// Returns whether it changed the filesystem and a human note describing the
// outcome. A dryRun classifies and reports without touching anything. No-op
// (with a note) when Env lacks the link path or binary.
func EnsureClaudeResumeLink(env Env, dryRun bool) (changed bool, note string, err error) {
	if env.ClaudeResumeLink == "" || env.Binary == "" {
		return false, "claude-resume: not managed (no link path/binary configured)", nil
	}
	action, reason := classifyClaudeResume(env.ClaudeResumeLink, env.Binary)
	short := env.ClaudeResumeLink
	switch action {
	case linkOK:
		return false, fmt.Sprintf("claude-resume: %s ok", short), nil
	case linkForeign:
		return false, fmt.Sprintf("claude-resume: %s left unchanged (%s)", short, reason), nil
	}
	verb := map[linkAction]string{linkCreate: "created", linkReplace: "replaced"}[action]
	if dryRun {
		return true, fmt.Sprintf("claude-resume: would have %s %s -> %s (%s)", verb, short, env.Binary, reason), nil
	}
	if err := os.MkdirAll(filepath.Dir(env.ClaudeResumeLink), 0o755); err != nil {
		return false, "", fmt.Errorf("claude-resume link: %w", err)
	}
	// Symlink-then-rename so the path never transits a missing state.
	tmp := env.ClaudeResumeLink + ".gts-tmp"
	os.Remove(tmp)
	if err := os.Symlink(env.Binary, tmp); err != nil {
		return false, "", fmt.Errorf("claude-resume link: %w", err)
	}
	if err := os.Rename(tmp, env.ClaudeResumeLink); err != nil {
		os.Remove(tmp)
		return false, "", fmt.Errorf("claude-resume link: %w", err)
	}
	return true, fmt.Sprintf("claude-resume: %s %s -> %s (%s)", verb, short, env.Binary, reason), nil
}

// ClaudeResumeDrift reports the link as a Drift when EnsureClaudeResumeLink
// would change it (absent / broken / old binary / known script). A foreign
// item is deliberately NOT drift: setup will never touch it, so validate
// must not fail forever over it.
func ClaudeResumeDrift(env Env) (Drift, bool) {
	if env.ClaudeResumeLink == "" || env.Binary == "" {
		return Drift{}, false
	}
	action, reason := classifyClaudeResume(env.ClaudeResumeLink, env.Binary)
	if action == linkCreate || action == linkReplace {
		return Drift{Path: env.ClaudeResumeLink, Kind: "claude-resume-link", Diff: reason}, true
	}
	return Drift{}, false
}
