package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/duskrun/duskrun/internal/plugin"
)

// --- test SSH/SFTP server -----------------------------------------------------
//
// The plugin is mostly protocol behaviour (atomic rename, mkdir -p, missing-key
// delete), so it is tested against a real SFTP server serving a temp directory
// rather than against a mock that would agree with whatever the code does.

type testServer struct {
	addr    string
	root    string
	hostKey ssh.PublicKey
	stop    func()
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	root := t.TempDir()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil // the plugin's auth is not under test
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	srv := &testServer{addr: ln.Addr().String(), root: root, hostKey: hostSigner.PublicKey()}
	srv.stop = func() { _ = ln.Close(); <-done }
	t.Cleanup(srv.stop)
	return srv
}

func serveConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) >= 4 &&
					string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
			}
		}()
		go func() {
			server, err := sftp.NewServer(ch)
			if err != nil {
				_ = ch.Close()
				return
			}
			_ = server.Serve()
			_ = server.Close()
		}()
	}
}

// clientKeyPEM returns a fresh unencrypted ed25519 key in OpenSSH PEM form.
func clientKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

// newStorage builds the plugin against the test server, with a known_hosts file
// in a temp dir so TOFU has somewhere to record the host key.
func newStorage(t *testing.T, srv *testServer, base string, over ...func(map[string]any)) *Storage {
	t.Helper()
	host, port, err := net.SplitHostPort(srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"host":             host,
		"port":             atoi(t, port),
		"user":             "backup",
		"private_key":      clientKeyPEM(t),
		"path":             base,
		"known_hosts_path": filepath.Join(t.TempDir(), "known_hosts"),
	}
	for _, f := range over {
		f(cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := New(raw)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = st.(*Storage).Close() })
	return st.(*Storage)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- tests --------------------------------------------------------------------

func TestSftpRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	ctx := context.Background()

	payload := []byte("duskrun sftp round trip")
	// The key carries a task/db prefix that does not exist yet — Write must
	// create the intermediate directories or every first backup would fail.
	meta, err := st.Write(ctx, "orders/all/20260813_0200_all.dump.zst", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if meta.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(payload))
	}

	rc, err := st.Read(ctx, "orders/all/20260813_0200_all.dump.zst")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

// TestSftpPartialWriteLeavesNothing is the reason writes are staged: a dump that
// dies mid-stream must not leave a short file under the artifact's key, which
// retention would count as a complete backup.
func TestSftpPartialWriteLeavesNothing(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	ctx := context.Background()

	key := "orders/all/partial.dump"
	_, err := st.Write(ctx, key, io.MultiReader(
		bytes.NewReader([]byte("half a dump")),
		errReader{errors.New("pg_dump failed: connection reset")},
	))
	if err == nil {
		t.Fatal("Write = nil error, want the reader failure surfaced")
	}

	if _, err := os.Stat(filepath.Join(srv.root, filepath.FromSlash(key))); !os.IsNotExist(err) {
		t.Fatalf("the failed upload left a visible artifact: %v", err)
	}
	// Nor a temp file: a leftover would accumulate on every failed run.
	entries, err := os.ReadDir(filepath.Join(srv.root, "orders", "all"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Fatalf("leftover after a failed upload: %s", e.Name())
	}
}

func TestSftpOverwritesExistingKey(t *testing.T) {
	// Two runs of the same task within the same minute produce the same key; a
	// rename that refuses an existing target would fail the second one.
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	ctx := context.Background()

	if _, err := st.Write(ctx, "t/db/x.dump", bytes.NewReader([]byte("first"))); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := st.Write(ctx, "t/db/x.dump", bytes.NewReader([]byte("second"))); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	rc, err := st.Read(ctx, "t/db/x.dump")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "second" {
		t.Fatalf("content = %q, want the second write", got)
	}
}

func TestSftpListPrefixAndTemps(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	ctx := context.Background()

	for _, k := range []string{"orders/all/a.dump", "orders/all/b.dump", "other/all/c.dump"} {
		if _, err := st.Write(ctx, k, bytes.NewReader([]byte(k))); err != nil {
			t.Fatalf("Write %s: %v", k, err)
		}
	}
	// A temp file from an upload in flight must not be reported as an artifact:
	// retention would treat it as an orphan and delete a live upload.
	if err := os.WriteFile(filepath.Join(srv.root, "orders", "all", "z.dump.dr-tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	objs, err := st.List(ctx, "orders/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	want := []string{"orders/all/a.dump", "orders/all/b.dump"}
	if strings.Join(sorted(keys), ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}

	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") = %d objects, want 3", len(all))
	}
}

// TestSftpDeleteMissingIsNotAnError: a second retention pass over an artifact
// already pruned must not fail the sweep.
func TestSftpDeleteMissingIsNotAnError(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	ctx := context.Background()

	if _, err := st.Write(ctx, "t/db/gone.dump", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "t/db/gone.dump"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete(ctx, "t/db/gone.dump"); err != nil {
		t.Fatalf("second Delete = %v, want nil for a missing key", err)
	}
	if err := st.Delete(ctx, "never/existed.dump"); err != nil {
		t.Fatalf("Delete of an unknown key = %v, want nil", err)
	}
}

func TestSftpReadMissingFails(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)
	if _, err := st.Read(context.Background(), "nope.dump"); err == nil {
		t.Fatal("Read of a missing key = nil error")
	}
}

func TestSftpCancelledContextStopsUpload(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := st.Write(ctx, "t/db/x.dump", bytes.NewReader(bytes.Repeat([]byte("x"), 1<<16)))
	if err == nil {
		t.Fatal("Write with a cancelled context = nil error")
	}
}

func TestSftpConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
	}{
		{"empty", ``},
		{"no host", `{"user":"u","path":"/b","private_key":"x"}`},
		{"no user", `{"host":"h","path":"/b","private_key":"x"}`},
		{"no path", `{"host":"h","user":"u","private_key":"x"}`},
		{"bad port", `{"host":"h","user":"u","path":"/b","port":70000,"private_key":"x"}`},
		{"bad host key mode", `{"host":"h","user":"u","path":"/b","host_key_mode":"none","private_key":"x"}`},
		{"no key", `{"host":"h","user":"u","path":"/b"}`},
		{"unparsable key", `{"host":"h","user":"u","path":"/b","private_key":"not a key"}`},
	}
	for _, c := range cases {
		if _, err := New([]byte(c.cfg)); err == nil {
			t.Fatalf("%s: New = nil error, want a rejection at write time", c.name)
		}
	}
}

func TestSftpConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte(`{"host":"h","user":"u","path":"/backups/"}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if c.Port != 22 {
		t.Errorf("port = %d, want 22", c.Port)
	}
	if c.HostKeyMode != "tofu" {
		t.Errorf("host_key_mode = %q, want tofu", c.HostKeyMode)
	}
	if c.KnownHostsPath != defaultKnownHosts {
		t.Errorf("known_hosts_path = %q, want %q", c.KnownHostsPath, defaultKnownHosts)
	}
	if c.Path != "/backups" {
		t.Errorf("path = %q, want it cleaned", c.Path)
	}
}

// TestSftpStrictModeRejectsUnknownHost proves the storage goes through the same
// host-key check as the ssh-tunnel connector: with an empty known_hosts and
// mode=strict, the connection must be refused rather than trusted.
func TestSftpStrictModeRejectsUnknownHost(t *testing.T) {
	srv := newTestServer(t)
	st := newStorage(t, srv, srv.root, func(m map[string]any) { m["host_key_mode"] = "strict" })

	_, err := st.Write(context.Background(), "t/db/x.dump", bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("Write = nil error, want the unknown host refused in strict mode")
	}
	if !strings.Contains(err.Error(), "unknown host") {
		t.Fatalf("err = %v, want an unknown-host rejection", err)
	}
}

// TestSftpTofuPinsHostKey: the first connection records the host key, so a later
// swap is detectable. The mismatch half of the rule is covered where it lives,
// in internal/sshx — the point here is that the storage uses that file at all.
func TestSftpTofuPinsHostKey(t *testing.T) {
	srv := newTestServer(t)
	known := filepath.Join(t.TempDir(), "known_hosts")
	st := newStorage(t, srv, srv.root, func(m map[string]any) { m["known_hosts_path"] = known })

	if _, err := st.Write(context.Background(), "t/db/x.dump", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	body, err := os.ReadFile(known)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		t.Fatal("known_hosts is empty after a TOFU accept")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

var _ plugin.Storage = (*Storage)(nil)
