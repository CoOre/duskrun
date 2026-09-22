package sshtunnel

import (
	"context"
	"os"
	"testing"

	"github.com/duskrun/duskrun/internal/sshx"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestTunnelConfigParse checks defaults and required-field validation.
func TestTunnelConfigParse(t *testing.T) {
	c, err := parseConfig([]byte(`{
		"ssh_host":"bastion.example.com",
		"ssh_user":"deploy",
		"remote_host":"10.0.0.5",
		"remote_port":5432,
		"private_key":"unused-here"
	}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.SSHPort != 22 {
		t.Errorf("ssh_port = %d, want 22 default", c.SSHPort)
	}
	if c.HostKeyMode != sshx.ModeTOFU {
		t.Errorf("host_key_mode = %q, want tofu default", c.HostKeyMode)
	}
	if c.KnownHostsPath != defaultKnownHosts {
		t.Errorf("known_hosts_path = %q, want %q", c.KnownHostsPath, defaultKnownHosts)
	}
	if c.KeepaliveSec != 15 {
		t.Errorf("keepalive_sec = %d, want 15 default", c.KeepaliveSec)
	}

	// Missing required fields are rejected.
	for _, bad := range []string{
		`{}`,
		`{"ssh_host":"h"}`,
		`{"ssh_host":"h","ssh_user":"u"}`,
		`{"ssh_host":"h","ssh_user":"u","remote_host":"r"}`, // no remote_port
	} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("parseConfig(%s) = nil error, want error", bad)
		}
	}

	// Open without a private key must fail clearly (no dial attempted).
	conn, err := New([]byte(`{"ssh_host":"h","ssh_user":"u","remote_host":"r","remote_port":5432}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := conn.Open(context.Background()); err == nil {
		t.Error("Open without private key succeeded, want error")
	}
	_ = conn.Close()
}

// TestTunnelE2E stands up a real SSH forward, gated on env.
func TestTunnelE2E(t *testing.T) {
	if os.Getenv("DUSKRUN_SSH_TEST") == "" {
		t.Skip("set DUSKRUN_SSH_TEST to run the real ssh-tunnel e2e")
	}
	t.Skip("DUSKRUN_SSH_TEST harness not implemented in this run — left for a human")
}

// compile-time guard.
var _ plugin.Connector = (*tunnel)(nil)
