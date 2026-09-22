package age

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	fage "filippo.io/age"
)

func roundTrip(t *testing.T, c Codec, payload []byte) []byte {
	t.Helper()
	var enc bytes.Buffer
	w, err := c.NewWriter(&enc)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil { // finalises the age STREAM
		t.Fatalf("Close: %v", err)
	}
	if bytes.Equal(enc.Bytes(), payload) {
		t.Fatal("ciphertext equals plaintext — not encrypted")
	}

	r, err := c.NewReader(bytes.NewReader(enc.Bytes()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return got
}

// TestAgeX25519RoundTrip encrypts to a generated recipient and decrypts with the
// matching identity.
func TestAgeX25519RoundTrip(t *testing.T) {
	id, err := fage.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	cfg, _ := json.Marshal(Config{
		Mode:       "x25519",
		Recipients: []string{id.Recipient().String()},
		Identity:   id.String(),
	})
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := bytes.Repeat([]byte("secret backup bytes\n"), 1000)
	got := roundTrip(t, c.(Codec), payload)
	if !bytes.Equal(got, payload) {
		t.Fatal("decrypted payload differs from original")
	}
}

// TestAgePassphraseRoundTrip encrypts and decrypts with a shared passphrase.
func TestAgePassphraseRoundTrip(t *testing.T) {
	cfg, _ := json.Marshal(Config{Mode: "passphrase", Passphrase: "correct horse battery staple"})
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := []byte("a smaller secret")
	got := roundTrip(t, c.(Codec), payload)
	if !bytes.Equal(got, payload) {
		t.Fatal("decrypted payload differs from original")
	}
}

func TestAgeExtAndName(t *testing.T) {
	c := Codec{}
	if c.Name() != "age" || c.Ext() != ".age" {
		t.Fatalf("Name/Ext = %q/%q, want age/.age", c.Name(), c.Ext())
	}
}

func TestAgeMissingRecipient(t *testing.T) {
	c, _ := New([]byte(`{"mode":"x25519"}`))
	if _, err := c.NewWriter(io.Discard); err == nil {
		t.Fatal("NewWriter with no recipients succeeded, want error")
	}
}
