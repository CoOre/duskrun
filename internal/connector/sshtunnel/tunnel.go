package sshtunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/sshx"
)

func init() {
	plugin.Connectors.Register("ssh-tunnel", New)
}

// Config is the ssh-tunnel connector config. The SSH private key is resolved
// upstream from a secret (private_key_ref); inline private_key is accepted for
// tests/simple setups. The tunnel forwards a local port to remote_host:remote_port
// as reached from the SSH server (a "direct" decorator over SSH).
type Config struct {
	SSHHost string `json:"ssh_host"`
	SSHPort int    `json:"ssh_port"` // default 22
	SSHUser string `json:"ssh_user"`

	PrivateKey    string `json:"private_key"`     // PEM (resolved)
	PrivateKeyRef string `json:"private_key_ref"` // secret ref (resolved upstream)

	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`

	KnownHostsPath string `json:"known_hosts_path"` // default data/known_hosts
	HostKeyMode    string `json:"host_key_mode"`    // tofu (default) | strict

	KeepaliveSec int `json:"keepalive_sec"` // default 15
	DialTimeout  int `json:"dial_timeout_sec"`
}

const (
	defaultSSHPort      = 22
	defaultKnownHosts   = "data/known_hosts"
	defaultKeepaliveSec = 15
	defaultDialTimeout  = 15
)

func parseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, fmt.Errorf("ssh-tunnel: empty config")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("ssh-tunnel: bad config: %w", err)
	}
	if c.SSHHost == "" {
		return c, fmt.Errorf("ssh-tunnel: ssh_host is required")
	}
	if c.SSHUser == "" {
		return c, fmt.Errorf("ssh-tunnel: ssh_user is required")
	}
	if c.RemoteHost == "" || c.RemotePort == 0 {
		return c, fmt.Errorf("ssh-tunnel: remote_host and remote_port are required")
	}
	if c.SSHPort == 0 {
		c.SSHPort = defaultSSHPort
	}
	if c.KnownHostsPath == "" {
		c.KnownHostsPath = defaultKnownHosts
	}
	if c.HostKeyMode == "" {
		c.HostKeyMode = sshx.ModeTOFU
	}
	if c.KeepaliveSec == 0 {
		c.KeepaliveSec = defaultKeepaliveSec
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = defaultDialTimeout
	}
	return c, nil
}

// New builds an ssh-tunnel connector. It does not dial until Open.
func New(raw []byte) (plugin.Connector, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &tunnel{cfg: c}, nil
}

// tunnel is a Connector that stands up an SSH port-forward on Open and tears it
// down on Close, handing the Dumper a local 127.0.0.1:<port> endpoint.
type tunnel struct {
	cfg      Config
	client   *ssh.Client
	listener net.Listener
	stop     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}

// Open dials the SSH server, starts a local forwarding listener, and returns the
// local endpoint. The DB dumper then connects to the local side as if direct.
func (t *tunnel) Open(ctx context.Context) (plugin.Endpoint, error) {
	if t.cfg.PrivateKey == "" {
		return plugin.Endpoint{}, fmt.Errorf("ssh-tunnel: no private key resolved (set private_key or private_key_ref)")
	}
	signer, err := ssh.ParsePrivateKey([]byte(t.cfg.PrivateKey))
	if err != nil {
		return plugin.Endpoint{}, fmt.Errorf("ssh-tunnel: parse private key: %w", err)
	}
	hostKey, err := sshx.HostKeyCallback(t.cfg.KnownHostsPath, t.cfg.HostKeyMode)
	if err != nil {
		return plugin.Endpoint{}, err
	}

	clientCfg := &ssh.ClientConfig{
		User:            t.cfg.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKey,
		Timeout:         time.Duration(t.cfg.DialTimeout) * time.Second,
	}
	sshAddr := net.JoinHostPort(t.cfg.SSHHost, strconv.Itoa(t.cfg.SSHPort))
	client, err := ssh.Dial("tcp", sshAddr, clientCfg)
	if err != nil {
		return plugin.Endpoint{}, fmt.Errorf("ssh-tunnel: dial %s: %w", sshAddr, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = client.Close()
		return plugin.Endpoint{}, fmt.Errorf("ssh-tunnel: local listener: %w", err)
	}

	t.client = client
	t.listener = ln
	t.stop = make(chan struct{})

	remoteAddr := net.JoinHostPort(t.cfg.RemoteHost, strconv.Itoa(t.cfg.RemotePort))
	t.wg.Add(2)
	go t.serve(remoteAddr)
	go t.keepalive()

	return plugin.Endpoint{Network: "tcp", Address: ln.Addr().String()}, nil
}

// serve accepts local connections and forwards each over the SSH client.
func (t *tunnel) serve(remoteAddr string) {
	defer t.wg.Done()
	for {
		local, err := t.listener.Accept()
		if err != nil {
			return // listener closed on Close()
		}
		t.wg.Add(1)
		go t.forward(local, remoteAddr)
	}
}

func (t *tunnel) forward(local net.Conn, remoteAddr string) {
	defer t.wg.Done()
	defer local.Close()
	remote, err := t.client.Dial("tcp", remoteAddr)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-t.stop:
	}
}

// keepalive sends periodic SSH keepalive requests so the forward survives idle
// gaps and dead-connection failures surface promptly.
func (t *tunnel) keepalive() {
	defer t.wg.Done()
	ticker := time.NewTicker(time.Duration(t.cfg.KeepaliveSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			if _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// Close tears down the tunnel: stop accepting, close the SSH client, wait for
// in-flight forwards to drain.
func (t *tunnel) Close() error {
	if t.stop == nil {
		return nil // never opened
	}
	t.once.Do(func() { close(t.stop) })
	if t.listener != nil {
		_ = t.listener.Close()
	}
	if t.client != nil {
		_ = t.client.Close()
	}
	t.wg.Wait()
	return nil
}
