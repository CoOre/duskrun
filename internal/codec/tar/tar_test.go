package tar

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestTarRoundTrip archives a payload through the codec and reads it back both
// with stock archive/tar and with the codec's own NewReader.
func TestTarRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("duskrun dump body\n"), 500)

	c, err := New([]byte(`{"name":"orders.dump"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var arc bytes.Buffer
	w, err := c.NewWriter(&arc)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// Write in chunks to exercise the buffering path.
	for off := 0; off < len(payload); off += 97 {
		end := off + 97
		if end > len(payload) {
			end = len(payload)
		}
		if _, err := w.Write(payload[off:end]); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Stock tar reader: header name + size + body must match.
	tr := tar.NewReader(bytes.NewReader(arc.Bytes()))
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if h.Name != "orders.dump" {
		t.Fatalf("member name = %q, want orders.dump", h.Name)
	}
	if h.Size != int64(len(payload)) {
		t.Fatalf("member size = %d, want %d", h.Size, len(payload))
	}
	body, _ := io.ReadAll(tr)
	if !bytes.Equal(body, payload) {
		t.Fatal("stock tar body mismatch")
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Fatalf("expected single member, got %v", err)
	}

	// Codec NewReader: unwraps back to the raw payload.
	r, err := c.NewReader(bytes.NewReader(arc.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("codec round-trip mismatch")
	}
}

// TestTarDefaultMemberName confirms an empty config archives under "dump".
func TestTarDefaultMemberName(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var arc bytes.Buffer
	w, _ := c.NewWriter(&arc)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h, err := tar.NewReader(bytes.NewReader(arc.Bytes())).Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if h.Name != "dump" {
		t.Fatalf("default member = %q, want dump", h.Name)
	}
}

// TestTarRegistered confirms init() wired tar into the codec registry.
func TestTarRegistered(t *testing.T) {
	if !plugin.Codecs.Has("tar") {
		t.Fatal("tar not registered")
	}
	c, err := plugin.Codecs.Create("tar", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Name() != "tar" || c.Ext() != ".tar" {
		t.Fatalf("Name/Ext = %q/%q, want tar/.tar", c.Name(), c.Ext())
	}
}
