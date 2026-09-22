package secret

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := NewBox([]byte("correct horse battery staple"), "env:v1")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("s3cr3t-db-password")
	sealed, err := box.Seal("db/pg-primary", "db-password", plain)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.KeyID != "env:v1" {
		t.Fatalf("KeyID = %q, want env:v1", sealed.KeyID)
	}
	if bytes.Contains(sealed.Ciphertext, plain) {
		t.Fatal("plaintext leaked into ciphertext blob")
	}
	got, err := box.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, want %q", got, plain)
	}
}

func TestOpenWithWrongMasterKeyFails(t *testing.T) {
	a, _ := NewBox([]byte("master-A"), "env:v1")
	b, _ := NewBox([]byte("master-B"), "env:v1")
	sealed, err := a.Seal("n", "t", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("Open with wrong master key succeeded, want auth failure")
	}
}

func TestUniqueDEKPerSecret(t *testing.T) {
	box, _ := NewBox([]byte("m"), "env:v1")
	s1, _ := box.Seal("a", "t", []byte("same"))
	s2, _ := box.Seal("b", "t", []byte("same"))
	if bytes.Equal(s1.Ciphertext, s2.Ciphertext) {
		t.Fatal("identical plaintext produced identical blobs — DEK/nonce not random")
	}
}
