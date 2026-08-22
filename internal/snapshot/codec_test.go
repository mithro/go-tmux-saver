package snapshot

import (
	"bytes"
	"io"
	"testing"
)

func TestGzipCodecRoundTrip(t *testing.T) {
	c, ok := LookupCodec("gzip")
	if !ok || c.Ext() != ".gz" {
		t.Fatalf("gzip codec missing or wrong ext: %v %v", c, ok)
	}
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "hello scrollback")
	w.Close()
	r, err := c.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "hello scrollback" {
		t.Fatalf("got %q", got)
	}
	if _, ok := LookupCodec("zstd"); ok {
		t.Fatal("zstd must not be registered yet")
	}
}
