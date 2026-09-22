package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genHostKey returns a fresh ssh.PublicKey for tests.
func genHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return sshPub
}

func addr() net.Addr { return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22} }

// TestHostKeyTOFUAcceptsAndPersists: a new host is accepted on first sight, the
// key is written, and a second verification with the same key succeeds.
func TestHostKeyTOFUAcceptsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	key := genHostKey(t)

	cb, err := HostKeyCallback(path, ModeTOFU)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("127.0.0.1:22", addr(), key); err != nil {
		t.Fatalf("first verify (TOFU accept) failed: %v", err)
	}

	// The key must be persisted.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("known_hosts not written after TOFU accept")
	}

	// A fresh callback (reloading the file) must now recognise the host.
	cb2, err := HostKeyCallback(path, ModeStrict)
	if err != nil {
		t.Fatalf("HostKeyCallback strict: %v", err)
	}
	if err := cb2("127.0.0.1:22", addr(), key); err != nil {
		t.Fatalf("second verify (known host) failed: %v", err)
	}
}

// TestHostKeyRejectsMismatch: once a host is pinned, a different key is rejected
// (possible MITM) even in TOFU mode.
func TestHostKeyRejectsMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	keyA := genHostKey(t)
	keyB := genHostKey(t)

	cb, err := HostKeyCallback(path, ModeTOFU)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("db.internal:22", addr(), keyA); err != nil {
		t.Fatalf("pin keyA failed: %v", err)
	}
	// Same host, different key → must be refused.
	if err := cb("db.internal:22", addr(), keyB); err == nil {
		t.Fatal("mismatched host key was accepted, want rejection")
	}
}

// TestHostKeyStrictRejectsUnknown: strict mode refuses a host not already pinned.
func TestHostKeyStrictRejectsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	cb, err := HostKeyCallback(path, ModeStrict)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("new.host:22", addr(), genHostKey(t)); err == nil {
		t.Fatal("strict mode accepted an unknown host, want rejection")
	}
}
