package mongodb

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

func TestMongoArgs(t *testing.T) {
	args, err := buildArgs(
		Options{Database: "app", Collection: "orders", Query: `{"paid":true}`, ReadPreference: "secondaryPreferred"},
		plugin.Endpoint{Network: "tcp", Address: "db.example.com:27017"},
		"backup", "/tmp/cfg.yaml",
	)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{
		"--archive",
		"--host", "db.example.com", "--port", "27017",
		"--username", "backup", "--authenticationDatabase", "admin",
		"--config", "/tmp/cfg.yaml",
		"--db", "app",
		"--collection", "orders",
		"--query", `{"paid":true}`,
		"--readPreference", "secondaryPreferred",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n  %v\nwant\n  %v", args, want)
	}
}

// TestMongoArgsNoGzip guards the double-compression trap: gzip here would make
// the codec chain's .zst suffix describe an archive that is already compressed.
func TestMongoArgsNoGzip(t *testing.T) {
	args, err := buildArgs(Options{}, plugin.Endpoint{Network: "tcp", Address: "h:27017"}, "", "")
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "gzip") {
			t.Fatalf("args %v carry a gzip flag", args)
		}
	}
	if args[0] != "--archive" {
		t.Fatalf("args %v do not start with --archive (stdout mode)", args)
	}
}

func TestMongoArgsUnixAndExclusions(t *testing.T) {
	args, err := buildArgs(
		Options{Database: "app", ExcludeCollection: []string{"sessions", " ", "cache"}, AuthDatabase: "app"},
		plugin.Endpoint{Network: "unix", Address: "/tmp/mongodb-27017.sock"},
		"backup", "",
	)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--host /tmp/mongodb-27017.sock") {
		t.Fatalf("args %v missing socket host", args)
	}
	if !strings.Contains(joined, "--authenticationDatabase app") {
		t.Fatalf("args %v missing explicit auth database", args)
	}
	if strings.Count(joined, "--excludeCollection") != 2 {
		t.Fatalf("args %v want exactly two exclusions (blank entry dropped)", args)
	}
}

func TestMongoOptionValidation(t *testing.T) {
	cases := []struct {
		name string
		opt  plugin.DumpOptions
	}{
		{"collection without database", plugin.DumpOptions{"collection": "orders"}},
		{"query without collection", plugin.DumpOptions{"database": "app", "query": "{}"}},
		{"exclusions without database", plugin.DumpOptions{"exclude_collection": []string{"x"}}},
	}
	for _, c := range cases {
		if _, err := parseOptions(c.opt); err == nil {
			t.Fatalf("%s: parseOptions = nil error, want a rejection", c.name)
		}
	}
	// The whole-server dump is legitimate: no database means every database.
	if _, err := parseOptions(plugin.DumpOptions{}); err != nil {
		t.Fatalf("empty options rejected: %v", err)
	}
}

func TestMongoRestoreHint(t *testing.T) {
	if got := (Dumper{}).RestoreHint(plugin.DumpOptions{"database": "app"}); !strings.Contains(got, "--nsInclude='app.*'") {
		t.Fatalf("hint = %q, want the namespace scoped to the dumped database", got)
	}
	if got := (Dumper{}).RestoreHint(plugin.DumpOptions{}); got != "mongorestore --archive=<file>" {
		t.Fatalf("hint = %q", got)
	}
}

func TestMongoPasswordConfigFile(t *testing.T) {
	path, cleanup, err := writePasswordConfig(`p"a:s s#word\`)
	if err != nil {
		t.Fatalf("writePasswordConfig: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %v, want 0600 — the file holds a live credential", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "password: \"p\\\"a:s s#word\\\\\"\n"; got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config still present after cleanup: %v", err)
	}
}

func TestMongoNoPasswordWritesNoFile(t *testing.T) {
	path, cleanup, err := writePasswordConfig("")
	if err != nil {
		t.Fatalf("writePasswordConfig: %v", err)
	}
	defer cleanup()
	if path != "" {
		t.Fatalf("config path = %q, want none for a passwordless connection", path)
	}
}

// stubBinary writes an executable shell script and points the dumper at it.
func stubBinary(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mongodump-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := mongodumpBinary
	mongodumpBinary = path
	t.Cleanup(func() { mongodumpBinary = orig })
}

// TestMongoPasswordNeverInArgv is the `ps aux` guard: the credential travels in
// the config file, and the argv the process is started with must not carry it.
func TestMongoPasswordNeverInArgv(t *testing.T) {
	stubBinary(t, `echo "$@"`)

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:27017"},
		plugin.Credentials{Username: "backup", Password: "s3cret-pw"},
		plugin.DumpOptions{"database": "app"},
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
	if strings.Contains(string(out), "s3cret-pw") {
		t.Fatalf("argv %q contains the password", out)
	}
	if !strings.Contains(string(out), "--config") {
		t.Fatalf("argv %q missing --config (password would have nowhere to come from)", out)
	}
}

// TestMongoRemovesPasswordFileOnClose: the credential must not outlive the dump.
func TestMongoRemovesPasswordFileOnClose(t *testing.T) {
	// The stub echoes the config path it was given, so the test can watch it.
	stubBinary(t, `while [ $# -gt 0 ]; do if [ "$1" = "--config" ]; then echo "$2"; fi; shift; done`)

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:27017"},
		plugin.Credentials{Username: "backup", Password: "pw"},
		plugin.DumpOptions{"database": "app"},
	)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		t.Fatal("stub reported no config path")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config missing while the dump was running: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("config survived the dump: %v", err)
	}
}

// TestMongoNonZeroExitIsReadError is the contract from plugin.Dumper.Dump: a
// failing tool must surface as a read error. A silent EOF here would store a
// truncated archive as a successful backup.
func TestMongoNonZeroExitIsReadError(t *testing.T) {
	stubBinary(t, "printf 'partial archive'\necho 'mongodump: connection refused' >&2\nexit 3")

	rc, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:27017"},
		plugin.Credentials{},
		plugin.DumpOptions{"database": "app"},
	)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	defer rc.Close()

	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("read to EOF returned nil error, want the non-zero exit surfaced")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the tool's stderr included", err)
	}
}

func TestMongoDumpMissingBinary(t *testing.T) {
	orig := mongodumpBinary
	mongodumpBinary = "mongodump-does-not-exist-duskrun"
	defer func() { mongodumpBinary = orig }()

	_, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:27017"},
		plugin.Credentials{},
		plugin.DumpOptions{"database": "app"},
	)
	if err == nil {
		t.Fatal("Dump succeeded with a missing binary, want error")
	}
	if !strings.Contains(err.Error(), "is mongodump installed?") {
		t.Fatalf("error %q missing the install hint", err)
	}
}

func TestMongoStagedUnsupported(t *testing.T) {
	_, _, err := Dumper{}.DumpStaged(context.Background(), plugin.Endpoint{}, plugin.Credentials{}, nil)
	if !errors.Is(err, plugin.ErrModeUnsupported) {
		t.Fatalf("DumpStaged err = %v, want ErrModeUnsupported", err)
	}
}
