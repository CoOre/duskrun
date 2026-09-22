package dockerproxy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	c, err := parseConfig([]byte(`{
		"network":"app_default",
		"target_host":"postgres",
		"target_port":5432
	}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.ListenPort != 5432 {
		t.Errorf("listen_port = %d, want target_port default", c.ListenPort)
	}
	if c.Image != defaultImage {
		t.Errorf("image = %q, want %q", c.Image, defaultImage)
	}
	if c.DockerBinary != defaultDockerBinary {
		t.Errorf("docker_binary = %q, want %q", c.DockerBinary, defaultDockerBinary)
	}

	for _, bad := range []string{
		`{}`,
		`{"network":"n"}`,
		`{"network":"n","target_host":"db"}`,
		`{"network":"n","target_host":"db","target_port":0}`,
		`{"network":"n","target_host":"db","target_port":65536}`,
		`{"network":"n","target_host":"db","target_port":5432,"listen_port":70000}`,
	} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("parseConfig(%s) = nil error, want error", bad)
		}
	}
}

func TestOpenStartsProxyWithDockerAssignedHostPort(t *testing.T) {
	r := &fakeRunner{
		responses: []fakeResponse{
			{out: []byte("container-123\n")},
			{out: []byte(`{"15432/tcp":[{"HostIp":"127.0.0.1","HostPort":"49161"}]}`)},
			{out: []byte("container-123\n")},
		},
	}
	p := &dockerProxy{
		cfg: Config{
			Network:       "app_default",
			TargetHost:    "postgres",
			TargetPort:    5432,
			ListenPort:    15432,
			Image:         "alpine/socat:latest",
			DockerBinary:  "docker",
			ContainerName: "duskrun-proxy-test",
		},
		runner: r,
	}

	ep, err := p.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ep.Network != "tcp" || ep.Address != "127.0.0.1:49161" {
		t.Fatalf("endpoint = %+v, want tcp 127.0.0.1:49161", ep)
	}

	wantRun := fakeCall{
		name: "docker",
		args: []string{
			"run", "-d", "--rm",
			"--name", "duskrun-proxy-test",
			"--label", proxyLabel,
			"--network", "app_default",
			"-p", "127.0.0.1::15432/tcp",
			"alpine/socat:latest",
			"tcp-listen:15432,fork,reuseaddr",
			"tcp-connect:postgres:5432",
		},
	}
	if !reflect.DeepEqual(r.calls[0], wantRun) {
		t.Fatalf("docker run call = %#v, want %#v", r.calls[0], wantRun)
	}

	wantInspect := fakeCall{
		name: "docker",
		args: []string{"inspect", "--format", "{{json .NetworkSettings.Ports}}", "container-123"},
	}
	if !reflect.DeepEqual(r.calls[1], wantInspect) {
		t.Fatalf("docker inspect call = %#v, want %#v", r.calls[1], wantInspect)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wantStop := fakeCall{name: "docker", args: []string{"stop", "container-123"}}
	if !reflect.DeepEqual(r.calls[2], wantStop) {
		t.Fatalf("docker stop call = %#v, want %#v", r.calls[2], wantStop)
	}
}

func TestOpenCleansUpContainerWhenInspectFails(t *testing.T) {
	r := &fakeRunner{
		responses: []fakeResponse{
			{out: []byte("container-123\n")},
			{err: errors.New("inspect failed"), out: []byte("no such container")},
			{out: []byte("container-123\n")},
		},
	}
	p := &dockerProxy{
		cfg: Config{
			Network:       "app_default",
			TargetHost:    "postgres",
			TargetPort:    5432,
			ListenPort:    5432,
			Image:         defaultImage,
			DockerBinary:  defaultDockerBinary,
			ContainerName: "duskrun-proxy-test",
		},
		runner: r,
	}

	if _, err := p.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded, want inspect error")
	}
	if len(r.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(r.calls))
	}
	wantStop := fakeCall{name: "docker", args: []string{"stop", "container-123"}}
	if !reflect.DeepEqual(r.calls[2], wantStop) {
		t.Fatalf("cleanup call = %#v, want %#v", r.calls[2], wantStop)
	}
}

func TestInspectPublishedPortNormalizesWildcardHostIP(t *testing.T) {
	r := &fakeRunner{
		responses: []fakeResponse{
			{out: []byte(`{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"49161"}]}`)},
		},
	}
	p := &dockerProxy{
		cfg: Config{
			ListenPort:   5432,
			DockerBinary: defaultDockerBinary,
		},
		runner:    r,
		container: "container-123",
	}

	host, port, err := p.inspectPublishedPort(context.Background())
	if err != nil {
		t.Fatalf("inspectPublishedPort: %v", err)
	}
	if host != "127.0.0.1" || port != "49161" {
		t.Fatalf("host/port = %s/%s, want 127.0.0.1/49161", host, port)
	}
}

func TestParseSSHConfigDefaultsAndValidation(t *testing.T) {
	c, err := parseSSHConfig([]byte(`{
		"ssh_host":"docker-host.example.com",
		"ssh_user":"deploy",
		"private_key":"unused",
		"network":"app_default",
		"target_host":"postgres",
		"target_port":5432
	}`))
	if err != nil {
		t.Fatalf("parseSSHConfig: %v", err)
	}
	if c.SSHPort != 22 || c.ListenPort != 5432 || c.Image != defaultImage || c.DockerBinary != defaultDockerBinary {
		t.Fatalf("defaults = %+v, want ssh_port/listen_port/image/docker_binary defaults", c)
	}

	for _, bad := range []string{
		`{}`,
		`{"ssh_host":"h"}`,
		`{"ssh_host":"h","ssh_user":"u"}`,
		`{"ssh_host":"h","ssh_user":"u","network":"n"}`,
		`{"ssh_host":"h","ssh_user":"u","network":"n","target_host":"db"}`,
		`{"ssh_host":"h","ssh_user":"u","network":"n","target_host":"db","target_port":0}`,
	} {
		if _, err := parseSSHConfig([]byte(bad)); err == nil {
			t.Errorf("parseSSHConfig(%s) = nil error, want error", bad)
		}
	}
}

func TestShellJoinQuotesRemoteDockerArgs(t *testing.T) {
	got := shellJoin([]string{"docker", "run", "--name", "duskrun's proxy", "--network", "app default"})
	want := "docker run --name 'duskrun'\\''s proxy' --network 'app default'"
	if got != want {
		t.Fatalf("shellJoin = %q, want %q", got, want)
	}
}

type fakeCall struct {
	name string
	args []string
}

type fakeResponse struct {
	out []byte
	err error
}

type fakeRunner struct {
	calls     []fakeCall
	responses []fakeResponse
}

func (r *fakeRunner) CombinedOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string(nil), args...)})
	if len(r.responses) == 0 {
		return nil, errors.New("unexpected command")
	}
	resp := r.responses[0]
	r.responses = r.responses[1:]
	return resp.out, resp.err
}

var _ plugin.Connector = (*dockerProxy)(nil)
