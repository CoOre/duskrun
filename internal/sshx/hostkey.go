// Package sshx holds the SSH pieces shared by everything that dials SSH: the
// ssh-tunnel and docker-ssh-proxy connectors and the sftp storage. It implements
// host-key verification with TOFU (trust-on-first-use) or strict known_hosts
// enforcement — the SSH equivalent of pinning, so a compromised network can't
// silently swap the server's key.
//
// It lives outside internal/connector on purpose: a storage plugin must not have
// to import a connector package to check a host key, and one verification is the
// only way both paths stay honest.
package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Host-key modes.
const (
	ModeTOFU   = "tofu"   // accept an unknown host on first sight and remember it
	ModeStrict = "strict" // only accept hosts already in known_hosts
)

// hostKeyMu serialises appends to a known_hosts file across concurrent dials.
var hostKeyMu sync.Mutex

// HostKeyCallback returns an ssh.HostKeyCallback backed by a known_hosts file.
//
//   - A matching known key → accepted.
//   - An unknown host (KeyError.Want empty): tofu → append the key and accept;
//     strict → reject.
//   - A key MISMATCH (KeyError.Want non-empty) → always rejected (possible MITM),
//     in both modes.
//
// path is the known_hosts file (created, with parents, if missing). mode
// defaults to tofu when empty.
func HostKeyCallback(path, mode string) (ssh.HostKeyCallback, error) {
	if mode == "" {
		mode = ModeTOFU
	}
	if mode != ModeTOFU && mode != ModeStrict {
		return nil, fmt.Errorf("sshx: unknown host-key mode %q", mode)
	}
	if err := ensureFile(path); err != nil {
		return nil, err
	}
	// Sanity-check the file is loadable up front; the callback reloads it on each
	// verification so a key appended by a prior TOFU accept is seen immediately.
	if _, err := knownhosts.New(path); err != nil {
		return nil, fmt.Errorf("sshx: load known_hosts: %w", err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		base, err := knownhosts.New(path)
		if err != nil {
			return fmt.Errorf("sshx: reload known_hosts: %w", err)
		}
		err = base(hostname, remote, key)
		if err == nil {
			return nil // known and matches
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}
		if len(keyErr.Want) > 0 {
			// A different key is already pinned for this host → refuse.
			return fmt.Errorf("sshx: host key mismatch for %s (possible MITM): %w", hostname, err)
		}
		// Unknown host.
		if mode == ModeStrict {
			return fmt.Errorf("sshx: unknown host %s and mode=strict: %w", hostname, err)
		}
		return appendKnownHost(path, hostname, key)
	}, nil
}

// appendKnownHost writes a known_hosts line for hostname→key (TOFU accept).
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	hostKeyMu.Lock()
	defer hostKeyMu.Unlock()

	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sshx: open known_hosts: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("sshx: append known_hosts: %w", err)
	}
	return nil
}

// ensureFile creates path (and its parent dir) as an empty file if missing.
func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("sshx: mkdir known_hosts dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sshx: create known_hosts: %w", err)
	}
	return f.Close()
}
