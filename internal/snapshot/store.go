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

func (s *Store) EnsureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, "rejected"), 0o700); err != nil {
		return err
	}
	stale, _ := filepath.Glob(filepath.Join(s.Dir, "snap-*.tmp"))
	for _, d := range stale {
		os.RemoveAll(d)
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

func (st *Staged) Promote() (string, error) {
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
	return final, os.Rename(tmpLink, link)
}

func (st *Staged) Reject() (string, error) {
	dst := filepath.Join(st.store.Dir, "rejected", st.name)
	os.RemoveAll(dst)
	return dst, os.Rename(st.tmpDir, dst)
}

func (st *Staged) Discard() error { return os.RemoveAll(st.tmpDir) }

// Last returns the snapshot `last` points at. os.ErrNotExist if none.
func (s *Store) Last() (*Snapshot, string, error) {
	target, err := os.Readlink(filepath.Join(s.Dir, "last"))
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(s.Dir, target)
	snap, err := s.Load(dir)
	return snap, dir, err
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
