package dockerproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/sshx"
)

func init() {
	plugin.Connectors.Register("docker-ssh-proxy", NewSSH)
}

const (
	defaultSSHPort      = 22
	defaultKnownHosts   = "data/known_hosts"
	defaultKeepaliveSec = 15
	defaultDialTimeout  = 15
)

// SSHConfig is the docker-ssh-proxy connector config. It connects to an SSH
// host, starts a temporary Docker proxy container there, and exposes it through
// a local SSH forward.
type SSHConfig struct {
	SSHHost string `json:"ssh_host"`
	SSHPort int    `json:"ssh_port"` // default 22
	SSHUser string `json:"ssh_user"`

	PrivateKey    string `json:"private_key"`     // PEM (resolved)
	PrivateKeyRef string `json:"private_key_ref"` // secret ref (resolved upstream)

	KnownHostsPath string `json:"known_hosts_path"` // default data/known_hosts
	HostKeyMode    string `json:"host_key_mode"`    // tofu (default) | strict
	KeepaliveSec   int    `json:"keepalive_sec"`    // default 15
	DialTimeout    int    `json:"dial_timeout_sec"` // default 15

	Network    string `json:"network"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`

	ListenPort    int    `json:"listen_port"`    // default: target_port
	Image         string `json:"image"`          // default: alpine/socat:latest
	DockerBinary  string `json:"docker_binary"`  // default: docker on the SSH host
	ContainerName string `json:"container_name"` // default: duskrun-ssh-proxy-<random>
}

func parseSSHConfig(raw []byte) (SSHConfig, error) {
	var c SSHConfig
	if len(raw) == 0 {
		return c, fmt.Errorf("docker-ssh-proxy: empty config")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("docker-ssh-proxy: bad config: %w", err)
	}
	if c.SSHHost == "" {
		return c, fmt.Errorf("docker-ssh-proxy: ssh_host is required")
	}
	if c.SSHUser == "" {
		return c, fmt.Errorf("docker-ssh-proxy: ssh_user is required")
	}
	if c.Network == "" {
		return c, fmt.Errorf("docker-ssh-proxy: network is required")
	}
	if c.TargetHost == "" {
		return c, fmt.Errorf("docker-ssh-proxy: target_host is required")
	}
	if !validPort(c.TargetPort) {
		return c, fmt.Errorf("docker-ssh-proxy: target_port must be 1..65535")
	}
	if c.ListenPort == 0 {
		c.ListenPort = c.TargetPort
	}
	if !validPort(c.ListenPort) {
		return c, fmt.Errorf("docker-ssh-proxy: listen_port must be 1..65535")
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
	if c.Image == "" {
		c.Image = defaultImage
	}
	if c.DockerBinary == "" {
		c.DockerBinary = defaultDockerBinary
	}
	return c, nil
}

// NewSSH builds a docker-ssh-proxy connector. It does not dial SSH or Docker
// until Open.
func NewSSH(raw []byte) (plugin.Connector, error) {
	c, err := parseSSHConfig(raw)
	if err != nil {
		return nil, err
	}
	return &dockerSSHProxy{cfg: c}, nil
}

type dockerSSHProxy struct {
	cfg       SSHConfig
	client    *ssh.Client
	listener  net.Listener
	stop      chan struct{}
	container string
	wg        sync.WaitGroup
	once      sync.Once
	closeErr  error
}

func (p *dockerSSHProxy) Open(ctx context.Context) (plugin.Endpoint, error) {
	if err := p.connect(); err != nil {
		return plugin.Endpoint{}, err
	}

	name, err := p.containerName()
	if err != nil {
		_ = p.Close()
		return plugin.Endpoint{}, err
	}
	out, err := p.remoteDocker(ctx, runArgs(Config{
		Network:       p.cfg.Network,
		TargetHost:    p.cfg.TargetHost,
		TargetPort:    p.cfg.TargetPort,
		ListenPort:    p.cfg.ListenPort,
		Image:         p.cfg.Image,
		ContainerName: name,
	}, name)...)
	if err != nil {
		_ = p.Close()
		return plugin.Endpoint{}, fmt.Errorf("docker-ssh-proxy: remote docker run: %w%s", err, commandOutputSuffix(out))
	}
	p.container = firstLine(out)
	if p.container == "" {
		p.container = name
	}

	remoteHost, remotePort, err := p.inspectRemotePublishedPort(ctx)
	if err != nil {
		_ = p.Close()
		return plugin.Endpoint{}, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = p.Close()
		return plugin.Endpoint{}, fmt.Errorf("docker-ssh-proxy: local listener: %w", err)
	}
	p.listener = ln
	p.stop = make(chan struct{})

	remoteAddr := net.JoinHostPort(remoteHost, remotePort)
	p.wg.Add(2)
	go p.serve(remoteAddr)
	go p.keepalive()

	return plugin.Endpoint{Network: "tcp", Address: ln.Addr().String()}, nil
}

func (p *dockerSSHProxy) connect() error {
	if p.cfg.PrivateKey == "" {
		return fmt.Errorf("docker-ssh-proxy: no private key resolved (set private_key or private_key_ref)")
	}
	signer, err := ssh.ParsePrivateKey([]byte(p.cfg.PrivateKey))
	if err != nil {
		return fmt.Errorf("docker-ssh-proxy: parse private key: %w", err)
	}
	hostKey, err := sshx.HostKeyCallback(p.cfg.KnownHostsPath, p.cfg.HostKeyMode)
	if err != nil {
		return err
	}
	clientCfg := &ssh.ClientConfig{
		User:            p.cfg.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKey,
		Timeout:         time.Duration(p.cfg.DialTimeout) * time.Second,
	}
	sshAddr := net.JoinHostPort(p.cfg.SSHHost, strconv.Itoa(p.cfg.SSHPort))
	client, err := ssh.Dial("tcp", sshAddr, clientCfg)
	if err != nil {
		return fmt.Errorf("docker-ssh-proxy: dial %s: %w", sshAddr, err)
	}
	p.client = client
	return nil
}

func (p *dockerSSHProxy) Close() error {
	p.once.Do(func() {
		if p.stop != nil {
			close(p.stop)
		}
		if p.listener != nil {
			_ = p.listener.Close()
		}
		if p.client != nil && p.container != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, err := p.remoteDocker(ctx, "stop", p.container)
			cancel()
			if err != nil {
				p.closeErr = fmt.Errorf("docker-ssh-proxy: remote docker stop %s: %w%s", p.container, err, commandOutputSuffix(out))
			}
		}
		if p.client != nil {
			_ = p.client.Close()
		}
		p.wg.Wait()
	})
	return p.closeErr
}

func (p *dockerSSHProxy) containerName() (string, error) {
	if p.cfg.ContainerName != "" {
		return p.cfg.ContainerName, nil
	}
	return randomContainerName("duskrun-ssh-proxy-")
}

func (p *dockerSSHProxy) inspectRemotePublishedPort(ctx context.Context) (string, string, error) {
	portKey := strconv.Itoa(p.cfg.ListenPort) + "/tcp"
	out, err := p.remoteDocker(ctx, "inspect", "--format", "{{json .NetworkSettings.Ports}}", p.container)
	if err != nil {
		return "", "", fmt.Errorf("docker-ssh-proxy: remote docker inspect %s: %w%s", p.container, err, commandOutputSuffix(out))
	}
	return parsePublishedPort(out, portKey)
}

func (p *dockerSSHProxy) remoteDocker(ctx context.Context, args ...string) ([]byte, error) {
	if p.client == nil {
		return nil, fmt.Errorf("ssh client is not connected")
	}
	session, err := p.client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	cmd := shellJoin(append([]string{p.cfg.DockerBinary}, args...))
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := session.CombinedOutput(cmd)
		done <- result{out: out, err: err}
	}()
	select {
	case res := <-done:
		return res.out, res.err
	case <-ctx.Done():
		_ = session.Close()
		return nil, ctx.Err()
	}
}

func (p *dockerSSHProxy) serve(remoteAddr string) {
	defer p.wg.Done()
	for {
		local, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go p.forward(local, remoteAddr)
	}
}

func (p *dockerSSHProxy) forward(local net.Conn, remoteAddr string) {
	defer p.wg.Done()
	defer local.Close()
	remote, err := p.client.Dial("tcp", remoteAddr)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-p.stop:
	}
}

func (p *dockerSSHProxy) keepalive() {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Duration(p.cfg.KeepaliveSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			if _, _, err := p.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool { return !shellSafeRune(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var _ plugin.Connector = (*dockerSSHProxy)(nil)

// shellSafeRune reports whether r can appear unquoted in a POSIX shell word.
func shellSafeRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
		r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=' || r == ','
}
