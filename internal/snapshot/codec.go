package snapshot

import (
	"compress/gzip"
	"io"
	"sync"
)

// Codec compresses per-pane scrollback files. Pluggable so zstd etc. can be
// added later without a format break (the snapshot records the codec name).
type Codec interface {
	Name() string
	Ext() string
	NewWriter(w io.Writer) (io.WriteCloser, error)
	NewReader(r io.Reader) (io.ReadCloser, error)
}

var (
	codecMu sync.RWMutex
	codecs  = map[string]Codec{}
)

func RegisterCodec(c Codec) {
	codecMu.Lock()
	codecs[c.Name()] = c
	codecMu.Unlock()
}

func LookupCodec(name string) (Codec, bool) {
	codecMu.RLock()
	defer codecMu.RUnlock()
	c, ok := codecs[name]
	return c, ok
}

type gzipCodec struct{}

func (gzipCodec) Name() string { return "gzip" }
func (gzipCodec) Ext() string  { return ".gz" }
func (gzipCodec) NewWriter(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriterLevel(w, gzip.BestSpeed)
}
func (gzipCodec) NewReader(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}

func init() {
	RegisterCodec(gzipCodec{})
}
