// Package dockerproxy exposes a Docker Compose-internal TCP service as a local
// localhost endpoint by running a short-lived proxy container in the target
// Docker network.
package dockerproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Connectors.Register("docker-proxy", New)
}

const (
	defaultDockerBinary = "docker"
	defaultImage        = "alpine/socat:latest"
	proxyLabel          = "com.duskrun.proxy=true"
	listenAddress       = "127.0.0.1"
)

// Config is the docker-proxy connector config. Docker assigns the host port
// atomically via -p 127.0.0.1::<listen_port>/tcp, so duskrun never races by
// preselecting a free port itself.
type Config struct {
	Network    string `json:"network"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`

	// Optional knobs mostly useful for tests and controlled deployments.
	ListenPort    int    `json:"listen_port"`    // default: target_port
	Image         string `json:"image"`          // default: alpine/socat:latest
	DockerBinary  string `json:"docker_binary"`  // default: docker
	ContainerName string `json:"container_name"` // default: duskrun-proxy-<random>
}

func parseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, fmt.Errorf("docker-proxy: empty config")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("docker-proxy: bad config: %w", err)
	}
	if c.Network == "" {
		return c, fmt.Errorf("docker-proxy: network is required")
	}
	if c.TargetHost == "" {
		return c, fmt.Errorf("docker-proxy: target_host is required")
	}
	if !validPort(c.TargetPort) {
		return c, fmt.Errorf("docker-proxy: target_port must be 1..65535")
	}
	if c.ListenPort == 0 {
		c.ListenPort = c.TargetPort
	}
	if !validPort(c.ListenPort) {
		return c, fmt.Errorf("docker-proxy: listen_port must be 1..65535")
	}
	if c.Image == "" {
		c.Image = defaultImage
	}
	if c.DockerBinary == "" {
		c.DockerBinary = defaultDockerBinary
	}
	return c, nil
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// New builds a docker-proxy connector. It does not start Docker until Open.
func New(raw []byte) (plugin.Connector, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return &dockerProxy{cfg: c, runner: execRunner{}}, nil
}

type commandRunner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type dockerProxy struct {
	cfg       Config
	runner    commandRunner
	container string
	once      sync.Once
	closeErr  error
}

func (p *dockerProxy) Open(ctx context.Context) (plugin.Endpoint, error) {
	name, err := p.containerName()
	if err != nil {
		return plugin.Endpoint{}, err
	}

	args := runArgs(p.cfg, name)
	out, err := p.runner.CombinedOutput(ctx, p.cfg.DockerBinary, args...)
	if err != nil {
		return plugin.Endpoint{}, fmt.Errorf("docker-proxy: docker run: %w%s", err, commandOutputSuffix(out))
	}
	p.container = firstLine(out)
	if p.container == "" {
		p.container = name
	}

	host, port, err := p.inspectPublishedPort(ctx)
	if err != nil {
		_ = p.Close()
		return plugin.Endpoint{}, err
	}
	return plugin.Endpoint{Network: "tcp", Address: net.JoinHostPort(host, port)}, nil
}

func (p *dockerProxy) Close() error {
	if p.container == "" {
		return nil
	}
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := p.runner.CombinedOutput(ctx, p.cfg.DockerBinary, "stop", p.container)
		if err != nil {
			p.closeErr = fmt.Errorf("docker-proxy: docker stop %s: %w%s", p.container, err, commandOutputSuffix(out))
		}
	})
	return p.closeErr
}

func (p *dockerProxy) containerName() (string, error) {
	if p.cfg.ContainerName != "" {
		return p.cfg.ContainerName, nil
	}
	return randomContainerName("duskrun-proxy-")
}

func runArgs(c Config, name string) []string {
	listen := strconv.Itoa(c.ListenPort)
	target := net.JoinHostPort(c.TargetHost, strconv.Itoa(c.TargetPort))
	return []string{
		"run", "-d", "--rm",
		"--name", name,
		"--label", proxyLabel,
		"--network", c.Network,
		"-p", listenAddress + "::" + listen + "/tcp",
		c.Image,
		"tcp-listen:" + listen + ",fork,reuseaddr",
		"tcp-connect:" + target,
	}
}

func (p *dockerProxy) inspectPublishedPort(ctx context.Context) (string, string, error) {
	portKey := strconv.Itoa(p.cfg.ListenPort) + "/tcp"
	out, err := p.runner.CombinedOutput(ctx, p.cfg.DockerBinary, "inspect", "--format", "{{json .NetworkSettings.Ports}}", p.container)
	if err != nil {
		return "", "", fmt.Errorf("docker-proxy: docker inspect %s: %w%s", p.container, err, commandOutputSuffix(out))
	}
	return parsePublishedPort(out, portKey)
}

func parsePublishedPort(out []byte, portKey string) (string, string, error) {
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(out, &ports); err != nil {
		return "", "", fmt.Errorf("docker-proxy: parse docker inspect ports: %w", err)
	}
	bindings := ports[portKey]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return "", "", fmt.Errorf("docker-proxy: no published host port for %s", portKey)
	}
	host := bindings[0].HostIP
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = listenAddress
	}
	return host, bindings[0].HostPort, nil
}

func randomContainerName(prefix string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("docker-proxy: random container name: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func commandOutputSuffix(out []byte) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return ": " + s
	}
	return ""
}

func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

var _ plugin.Connector = (*dockerProxy)(nil)
