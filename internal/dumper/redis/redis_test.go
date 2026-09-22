package redis

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

func TestRedisArgs(t *testing.T) {
	args, err := buildArgs(
		Options{},
		plugin.Endpoint{Network: "tcp", Address: "cache.example.com:6379"},
		"backup",
	)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{"-h", "cache.example.com", "-p", "6379", "--user", "backup", "--rdb", "-"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n  %v\nwant\n  %v", args, want)
	}
}

func TestRedisArgsUnixAndTLS(t *testing.T) {
	args, err := buildArgs(
		Options{TLS: true},
		plugin.Endpoint{Network: "unix", Address: "/var/run/redis/redis.sock"},
		"",
	)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{"-s", "/var/run/redis/redis.sock", "--tls", "--rdb", "-"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n  %v\nwant\n  %v", args, want)
	}
}

func TestRedisArgsRejectUnknownNetwork(t *testing.T) {
	if _, err := buildArgs(Options{}, plugin.Endpoint{Network: "quic", Address: "x"}, ""); err == nil {
		t.Fatal("buildArgs = nil error for an unknown network")
	}
}

func TestRedisRestoreHint(t *testing.T) {
	got := (Dumper{}).RestoreHint(nil)
	// The hint must not read like a client-side one-liner: an RDB is loaded by
	// the server at startup, and pretending otherwise invites a lost restore.
	if !strings.Contains(got, "dump.rdb") || !strings.Contains(got, "stop redis") {
		t.Fatalf("hint = %q, want the server-side restore procedure", got)
	}
}

// stubBinary writes an executable shell script and points the dumper at it.
func stubBinary(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "redis-cli-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := redisCLIBinary
	redisCLIBinary = path
	t.Cleanup(func() { redisCLIBinary = orig })
}

// TestRedisPasswordGoesToEnvNotArgv is the `ps aux` guard: -a would put the
// password in the process list (redis-cli warns about exactly this), so it must
// travel in REDISCLI_AUTH instead.
func TestRedisPasswordGoesToEnvNotArgv(t *testing.T) {
	stubBinary(t, `echo "argv: $@"; echo "env: $REDISCLI_AUTH"`)

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:6379"},
		plugin.Credentials{Password: "s3cret-pw"},
		nil,
	)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	argv, env, _ := strings.Cut(string(out), "env:")
	if strings.Contains(argv, "s3cret-pw") {
		t.Fatalf("argv %q contains the password", argv)
	}
	if !strings.Contains(env, "s3cret-pw") {
		t.Fatalf("REDISCLI_AUTH = %q, want the password", strings.TrimSpace(env))
	}
}

// TestRedisNonZeroExitIsReadError is the contract from plugin.Dumper.Dump. It
// matters more here than elsewhere: redis-cli reports progress on stderr and a
// refused connection still produces a clean, empty stdout.
func TestRedisNonZeroExitIsReadError(t *testing.T) {
	stubBinary(t, "echo 'Could not connect to Redis at 127.0.0.1:6379: Connection refused' >&2\nexit 1")

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:6379"},
		plugin.Credentials{},
		nil,
	)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	defer rc.Close()

	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("read to EOF returned nil error, want the non-zero exit surfaced")
	} else if !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err = %v, want the tool's stderr included", err)
	}
}

// TestRedisStderrChatterStaysOutOfTheArtifact: redis-cli narrates the transfer on
// stderr; if that leaked into stdout the RDB would be corrupt.
func TestRedisStderrChatterStaysOutOfTheArtifact(t *testing.T) {
	stubBinary(t, "echo 'SYNC sent to master' >&2\nprintf 'REDIS0011'\n")

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:6379"},
		plugin.Credentials{},
		nil,
	)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	defer rc.Close()

	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != "REDIS0011" {
		t.Fatalf("artifact = %q, want only the RDB bytes", out)
	}
}

func TestRedisDumpMissingBinary(t *testing.T) {
	orig := redisCLIBinary
	redisCLIBinary = "redis-cli-does-not-exist-duskrun"
	defer func() { redisCLIBinary = orig }()

	_, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:6379"},
		plugin.Credentials{},
		nil,
	)
	if err == nil {
		t.Fatal("Dump succeeded with a missing binary, want error")
	}
	if !strings.Contains(err.Error(), "is redis-cli installed?") {
		t.Fatalf("error %q missing the install hint", err)
	}
}

func TestRedisStagedUnsupported(t *testing.T) {
	_, _, err := Dumper{}.DumpStaged(context.Background(), plugin.Endpoint{}, plugin.Credentials{}, nil)
	if !errors.Is(err, plugin.ErrModeUnsupported) {
		t.Fatalf("DumpStaged err = %v, want ErrModeUnsupported", err)
	}
}
