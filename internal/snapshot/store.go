package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const dirTimeFormat = "20060102T150405Z"

type Store struct {
	Dir   string
	Codec Codec
}

type Staged struct {
	store  *Store
	tmpDir string
	name   string // snap-<ts>
}

// EnsureDir creates the store's directory layout. It deliberately does NOT
// sweep leftover snap-*.tmp staging directories: every subcommand calls
// EnsureDir on start-up, with no lock held, so a sweep here could delete
// the staging directory of a save that is still writing into it (RULING
// R47). Sweeping is CleanStaleTmp's job, called by the save that holds the
// data dir's exclusive lock.
func (s *Store) EnsureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(s.Dir, "rejected"), 0o700)
}

// CleanStaleTmp removes leftover snap-*.tmp staging directories (from a
// save killed mid-Stage). The CALLER must hold the data dir's exclusive
// save lock: that is what makes "leftover" a safe inference — a live save
// holds the lock for its whole staging window, so nothing that survives to
// here can still be in use.
func (s *Store) CleanStaleTmp() error {
	stale, err := filepath.Glob(filepath.Join(s.Dir, "snap-*.tmp"))
	if err != nil {
		return err
	}
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			return err
		}
	}
	return nil
}

// Stage writes snap + contents into snap-<ts>.tmp/, hardlinking any pane whose
// content hash equals the same PaneKey's hash in the current last snapshot.
func (s *Store) Stage(snap *Snapshot, contents map[string][]byte) (*Staged, error) {
	if s.Codec == nil {
		return nil, errors.New("store: nil codec")
	}
	snap.ContentsCodec = s.Codec.Name()
	name := "snap-" + snap.TakenAt.UTC().Format(dirTimeFormat)
	tmp := filepath.Join(s.Dir, name+".tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "panes"), 0o700); err != nil {
		return nil, err
	}
	prev, prevDir, err := s.Last()
	prevHash := map[string]Pane{}
	if err == nil {
		for _, se := range prev.Sessions {
			for _, w := range se.Windows {
				for _, p := range w.Panes {
					prevHash[PaneKey(se.Name, w.Index, p.Index)] = p
				}
			}
		}
	}
	for si := range snap.Sessions {
		se := &snap.Sessions[si]
		for wi := range se.Windows {
			w := &se.Windows[wi]
			for pi := range w.Panes {
				p := &w.Panes[pi]
				key := PaneKey(se.Name, w.Index, p.Index)
				data, ok := contents[key]
				if !ok {
					continue
				}
				sum := sha256.Sum256(data)
				p.ContentSHA256 = hex.EncodeToString(sum[:])
				p.ContentFile = key + ".txt" + s.Codec.Ext()
				dst := filepath.Join(tmp, "panes", p.ContentFile)
				if pp, ok := prevHash[key]; ok && pp.ContentSHA256 == p.ContentSHA256 && pp.ContentFile == p.ContentFile {
					if os.Link(filepath.Join(prevDir, "panes", pp.ContentFile), dst) == nil {
						continue
					}
				}
				if err := s.writeCompressed(dst, data); err != nil {
					os.RemoveAll(tmp)
					return nil, err
				}
			}
		}
	}
	if err := writeJSONAtomic(filepath.Join(tmp, "layout.json"), snap); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return &Staged{store: s, tmpDir: tmp, name: name}, nil
}

func (s *Store) writeCompressed(path string, data []byte) (err error) {
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(part)
		}
	}()
	w, err := s.Codec.NewWriter(f)
	if err != nil {
		f.Close()
		return err
	}
	if _, err = w.Write(data); err != nil {
		w.Close()
		f.Close()
		return err
	}
	if err = w.Close(); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(part, path)
}

func writeJSONAtomic(path string, v any) (err error) {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(part)
		}
	}()
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(part, path)
}

// fsyncDir fsyncs directory dir, making renames/creates inside it durable
// on disk. File CONTENTS are fsynced at write time (writeCompressed /
// writeJSONAtomic); without the directory fsync a crash+reboot could lose
// the directory ENTRIES those writes created (issue #7).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func (st *Staged) Promote() (string, error) {
	// Issue #7: make the staged tree's directory entries durable before it
	// is renamed into place, and the renames themselves durable after —
	// `last` must never survive a crash pointing at a snapshot whose
	// entries were lost.
	if err := fsyncDir(filepath.Join(st.tmpDir, "panes")); err != nil {
		return "", err
	}
	if err := fsyncDir(st.tmpDir); err != nil {
		return "", err
	}
	final := filepath.Join(st.store.Dir, st.name)
	if err := os.Rename(st.tmpDir, final); err != nil {
		return "", err
	}
	link := filepath.Join(st.store.Dir, "last")
	tmpLink := link + ".tmp"
	os.Remove(tmpLink)
	if err := os.Symlink(st.name, tmpLink); err != nil {
		return "", err
	}
	if err := os.Rename(tmpLink, link); err != nil {
		return "", err
	}
	// One fsync of the store dir covers both renames (snapshot dir + last).
	return final, fsyncDir(st.store.Dir)
}

func (st *Staged) Reject() (string, error) {
	dst := filepath.Join(st.store.Dir, "rejected", st.name)
	os.RemoveAll(dst)
	if err := os.Rename(st.tmpDir, dst); err != nil {
		return dst, err
	}
	return dst, fsyncDir(filepath.Dir(dst))
}

func (st *Staged) Discard() error { return os.RemoveAll(st.tmpDir) }

// ErrDanglingLast means the `last` symlink exists but its target snapshot
// is gone or half-deleted. That is store corruption, not the normal "no
// snapshot yet" first-run state — so it deliberately does NOT match
// os.ErrNotExist, which callers (save's guard, restore --on-start) treat as
// benign absence and would otherwise silently skip on.
var ErrDanglingLast = errors.New("dangling last symlink")

// Last returns the snapshot `last` points at. os.ErrNotExist if there is no
// `last` symlink at all; ErrDanglingLast if the symlink exists but its
// target does not load.
func (s *Store) Last() (*Snapshot, string, error) {
	target, err := os.Readlink(filepath.Join(s.Dir, "last"))
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(s.Dir, target)
	snap, err := s.Load(dir)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil, dir, fmt.Errorf("last -> %s: %w", target, ErrDanglingLast)
	}
	return snap, dir, err
}

// Newest walks the store's snap-* directories newest-first (their names are
// UTC timestamps, so lexical order is time order) and returns the first one
// that loads cleanly — the recovery fallback when `last` is dangling.
// Staging (.tmp) and rejected directories are never candidates.
// os.ErrNotExist if no intact snapshot exists.
func (s *Store) Newest() (*Snapshot, string, error) {
	dirs, err := filepath.Glob(filepath.Join(s.Dir, "snap-*"))
	if err != nil {
		return nil, "", err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if strings.HasSuffix(dir, ".tmp") {
			continue
		}
		if snap, err := s.Load(dir); err == nil {
			return snap, dir, nil
		}
	}
	return nil, "", fmt.Errorf("no intact snapshot in %s: %w", s.Dir, os.ErrNotExist)
}

func (s *Store) Load(dir string) (*Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, "layout.json"))
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if snap.Schema != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema %d", dir, snap.Schema)
	}
	return &snap, nil
}

// ReadContent decodes a pane's scrollback using the codec named in the
// snapshot. Reading many panes from the same dir one at a time via this
// method re-parses dir's layout.json on every call just to learn the codec
// name — for that case, ContentReader parses it once up front instead.
func (s *Store) ReadContent(dir string, p Pane) ([]byte, error) {
	if p.ContentFile == "" {
		return nil, os.ErrNotExist
	}
	snap, err := s.Load(dir)
	if err != nil {
		return nil, err
	}
	codec, ok := LookupCodec(snap.ContentsCodec)
	if !ok {
		return nil, fmt.Errorf("unknown codec %q", snap.ContentsCodec)
	}
	return readPaneContent(dir, codec, p)
}

// ContentReader loads dir's snapshot once to resolve its contents codec, and
// returns a function that decodes any of that snapshot's panes' scrollback —
// for reading many panes from one dir (e.g. restoring a whole snapshot),
// this avoids ReadContent's per-call re-parse of layout.json.
func (s *Store) ContentReader(dir string) (func(p Pane) ([]byte, error), error) {
	snap, err := s.Load(dir)
	if err != nil {
		return nil, err
	}
	codec, ok := LookupCodec(snap.ContentsCodec)
	if !ok {
		return nil, fmt.Errorf("unknown codec %q", snap.ContentsCodec)
	}
	return func(p Pane) ([]byte, error) { return readPaneContent(dir, codec, p) }, nil
}

// readPaneContent decodes one pane's content file (in dir/panes/) with codec.
func readPaneContent(dir string, codec Codec, p Pane) ([]byte, error) {
	if p.ContentFile == "" {
		return nil, os.ErrNotExist
	}
	paneFile := filepath.Join(dir, "panes", p.ContentFile)
	f, err := os.Open(paneFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := codec.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", paneFile, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", paneFile, err)
	}
	return data, nil
}
