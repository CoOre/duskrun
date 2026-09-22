package gzip

import (
	"bytes"
	stdgzip "compress/gzip"
	"io"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestGzipRoundTrip compresses then decompresses through the codec and checks
// the payload survives intact.
func TestGzipRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("duskrun backup stream — highly compressible\n"), 1000)

	var comp bytes.Buffer
	w, err := Codec{}.NewWriter(&comp)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil { // flushes the final gzip block
		t.Fatalf("Close: %v", err)
	}
	if comp.Len() >= len(payload) {
		t.Fatalf("not compressed: %d >= %d", comp.Len(), len(payload))
	}

	// The artifact must be readable by stock gunzip, not only our NewReader.
	std, err := stdgzip.NewReader(bytes.NewReader(comp.Bytes()))
	if err != nil {
		t.Fatalf("stdlib gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(std)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("round-trip mismatch")
	}

	r, err := Codec{}.NewReader(bytes.NewReader(comp.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	got2, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll (codec): %v", err)
	}
	if !bytes.Equal(got2, payload) {
		t.Fatal("codec round-trip mismatch")
	}
}

// TestGzipRegistered confirms the init() wired gzip into the codec registry with
// the expected name and suffix.
func TestGzipRegistered(t *testing.T) {
	if !plugin.Codecs.Has("gzip") {
		t.Fatal("gzip not registered")
	}
	c, err := plugin.Codecs.Create("gzip", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Name() != "gzip" || c.Ext() != ".gz" {
		t.Fatalf("Name/Ext = %q/%q, want gzip/.gz", c.Name(), c.Ext())
	}
}
