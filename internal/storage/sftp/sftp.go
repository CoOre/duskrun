// Package sftp implements Storage over SFTP: artifacts land on a remote host
// reachable by SSH, which is what most operators already have when they have no
// object store.
//
// Two properties are load-bearing and both mirror localfs:
//
//   - Writes are atomic — stream to a temp name in the destination directory,
//     then rename. An interrupted upload otherwise leaves a short file under the
//     final key, and retention would count it as a complete artifact.
//   - Delete of a missing key is not an error, so a repeated retention pass over
//     an already-pruned artifact does not fail the sweep.
//
// Host-key verification is the shared internal/sshx one (TOFU or strict against
// known_hosts) — the same check the ssh-tunnel connector performs, not a second
// implementation with its own idea of trust.
package sftp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/sshx"
)

func init() {
	plugin.Storages.Register("sftp", New)
}

// Config is the sftp storage config (the storage.config JSON blob). The private
// key is resolved upstream from private_key_ref by core.SecretResolver; an inline
// private_key is accepted for tests and simple setups.
type Config struct {
	Host string `json:"host"`
	Port int    `json:"port"` // default 22
	User string `json:"user"`

	PrivateKey    string `json:"private_key"`     // PEM (resolved)
	PrivateKeyRef string `json:"private_key_ref"` // secret ref (resolved upstream)
	Passphrase    string `json:"passphrase"`      // for an encrypted key (resolved)

	Path string `json:"path"` // base directory on the remote host

	KnownHostsPath string `json:"known_hosts_path"` // default data/known_hosts
	HostKeyMode    string `json:"host_key_mode"`    // tofu (default) | strict

	DialTimeoutSec int `json:"dial_timeout_sec"` // default 15
}

const (
	defaultPort        = 22
	defaultKnownHosts  = "data/known_hosts"
	defaultDialTimeout = 15
	// dirPerm/filePerm are applied to created directories and the temp file. The
	// artifact may be an unencrypted database dump, so nothing is group-readable.
	dirPerm  = os.FileMode(0o750)
	filePerm = os.FileMode(0o600)
)

func parseConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, fmt.Errorf("sftp: empty config")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("sftp: bad config: %w", err)
	}
	if c.Host == "" {
		return c, fmt.Errorf("sftp: host is required")
	}
	if c.User == "" {
		return c, fmt.Errorf("sftp: user is required")
	}
	if c.Path == "" {
		return c, fmt.Errorf("sftp: path is required")
	}
	if c.Port == 0 {
		c.Port = defaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, fmt.Errorf("sftp: port %d out of range", c.Port)
	}
	if c.KnownHostsPath == "" {
		c.KnownHostsPath = defaultKnownHosts
	}
	if c.HostKeyMode == "" {
		c.HostKeyMode = sshx.ModeTOFU
	}
	if c.HostKeyMode != sshx.ModeTOFU && c.HostKeyMode != sshx.ModeStrict {
		return c, fmt.Errorf("sftp: unknown host_key_mode %q", c.HostKeyMode)
	}
	if c.DialTimeoutSec == 0 {
		c.DialTimeoutSec = defaultDialTimeout
	}
	c.Path = path.Clean(c.Path)
	return c, nil
}

// New builds an sftp Storage. Everything that can be judged without the network
// is judged here, so a config that can never work is refused when it is written
// rather than at 02:00 on the first scheduled run. No connection is opened: the
// session is established lazily and shared by the calls that need it.
func New(raw []byte) (plugin.Storage, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	if c.PrivateKey == "" {
		return nil, fmt.Errorf("sftp: no private key resolved (set private_key or private_key_ref)")
	}
	signer, err := parseKey(c.PrivateKey, c.Passphrase)
	if err != nil {
		return nil, err
	}
	return &Storage{cfg: c, signer: signer}, nil
}

func parseKey(pem, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(pem), []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("sftp: parse private key: %w", err)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, fmt.Errorf("sftp: parse private key: %w", err)
	}
	return signer, nil
}

// Storage is an SFTP-backed artifact store rooted at cfg.Path.
type Storage struct {
	cfg    Config
	signer ssh.Signer

	// mu guards the lazily-established session. A Storage is used by one run at a
	// time, but retention walks several artifacts through the same instance.
	mu     sync.Mutex
	ssh    *ssh.Client
	client *sftp.Client
}

// connect returns the shared SFTP session, dialling on first use. A session that
// died between calls (idle timeout on the server) is discarded and redialled.
func (s *Storage) connect(ctx context.Context) (*sftp.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		if _, err := s.client.Getwd(); err == nil {
			return s.client, nil
		}
		s.closeLocked()
	}

	hostKey, err := sshx.HostKeyCallback(s.cfg.KnownHostsPath, s.cfg.HostKeyMode)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(s.cfg.DialTimeoutSec) * time.Second
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	// Dial through a context-aware dialer so a hung TCP connect is cut by the
	// run's timeout instead of holding a worker for the full SSH timeout.
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftp: dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            s.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sftp: ssh handshake %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Time{}) // the transfer itself must not inherit it
	client := ssh.NewClient(sshConn, chans, reqs)

	sc, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("sftp: open sftp session: %w", err)
	}
	s.ssh, s.client = client, sc
	return sc, nil
}

func (s *Storage) closeLocked() {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
	if s.ssh != nil {
		_ = s.ssh.Close()
		s.ssh = nil
	}
}

// Close tears down the shared session. Storage has no Close in the plugin
// contract, so this exists for tests and for callers that hold an instance.
func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}

// remote maps an artifact key onto an absolute remote path. Keys always use "/",
// which is also SFTP's separator, so no OS-specific conversion belongs here.
func (s *Storage) remote(key string) string {
	return path.Join(s.cfg.Path, path.Clean("/"+key))
}

// Write streams r to path/key atomically: a temp name in the destination
// directory, then a rename. The intermediate directories are created because
// artifact keys carry a task/db prefix that will not exist on first write.
func (s *Storage) Write(ctx context.Context, key string, r io.Reader) (plugin.ObjectMeta, error) {
	client, err := s.connect(ctx)
	if err != nil {
		return plugin.ObjectMeta{}, err
	}
	dst := s.remote(key)
	if err := mkdirAll(client, path.Dir(dst)); err != nil {
		return plugin.ObjectMeta{}, err
	}

	tmp := dst + ".dr-tmp"
	f, err := client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("sftp: create %s: %w", tmp, err)
	}
	// Until the rename below succeeds, the temp file is garbage: remove it on
	// every failure path so a broken upload leaves nothing behind at all.
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = client.Remove(tmp)
		}
	}()

	if err := client.Chmod(tmp, filePerm); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("sftp: chmod %s: %w", tmp, err)
	}
	n, err := f.ReadFrom(cancelReader{ctx: ctx, r: r})
	if err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("sftp: upload %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("sftp: close %s: %w", tmp, err)
	}
	// PosixRename overwrites an existing target; plain SSH_FXP_RENAME fails on
	// one, which would break a re-run producing the same key within a minute.
	if err := client.PosixRename(tmp, dst); err != nil {
		return plugin.ObjectMeta{}, fmt.Errorf("sftp: rename %s: %w", key, err)
	}
	committed = true
	return plugin.ObjectMeta{Key: key, Size: n}, nil
}

// Read opens path/key for download or verify. The returned ReadCloser owns the
// remote file handle; the shared session stays open for later calls.
func (s *Storage) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	client, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	f, err := client.Open(s.remote(key))
	if err != nil {
		return nil, fmt.Errorf("sftp: open %s: %w", key, err)
	}
	return f, nil
}

// List returns objects under path whose key starts with prefix, recursively.
// Keys are relative to the configured path, matching what Write was given.
func (s *Storage) List(ctx context.Context, prefix string) ([]plugin.Object, error) {
	client, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	var out []plugin.Object
	walker := client.Walk(s.cfg.Path)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			// A directory that vanished mid-walk (a concurrent prune) must not
			// fail the whole listing: retention would then report an error for a
			// state that is already what it wanted.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("sftp: list: %w", err)
		}
		info := walker.Stat()
		if info == nil || info.IsDir() {
			continue
		}
		key, ok := s.keyOf(walker.Path())
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		if strings.HasSuffix(key, ".dr-tmp") {
			continue // an upload in flight is not an artifact
		}
		out = append(out, plugin.Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
	}
	return out, nil
}

// keyOf turns an absolute remote path back into an artifact key.
func (s *Storage) keyOf(p string) (string, bool) {
	rel := strings.TrimPrefix(p, s.cfg.Path)
	if rel == p && s.cfg.Path != "/" {
		return "", false
	}
	return strings.TrimPrefix(rel, "/"), true
}

// Delete removes path/key. A missing key is not an error — see the package doc.
func (s *Storage) Delete(ctx context.Context, key string) error {
	client, err := s.connect(ctx)
	if err != nil {
		return err
	}
	if err := client.Remove(s.remote(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sftp: delete %s: %w", key, err)
	}
	return nil
}

// mkdirAll creates dir and its parents. sftp.Client.MkdirAll exists but reports
// an error when a component exists as a symlink to a directory, which is a
// perfectly ordinary layout on a backup host.
func mkdirAll(client *sftp.Client, dir string) error {
	if dir == "" || dir == "/" || dir == "." {
		return nil
	}
	if fi, err := client.Stat(dir); err == nil {
		if fi.IsDir() {
			return nil
		}
		return fmt.Errorf("sftp: %s exists and is not a directory", dir)
	}
	if err := mkdirAll(client, path.Dir(dir)); err != nil {
		return err
	}
	if err := client.Mkdir(dir); err != nil {
		// Lost a race with a concurrent writer — fine as long as it is a dir now.
		if fi, statErr := client.Stat(dir); statErr == nil && fi.IsDir() {
			return nil
		}
		return fmt.Errorf("sftp: mkdir %s: %w", dir, err)
	}
	if err := client.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("sftp: chmod %s: %w", dir, err)
	}
	return nil
}

// cancelReader stops a transfer when the run's context is cancelled. Without it
// an upload to an unresponsive host would ignore the task timeout: the SFTP
// client has no context-aware copy.
type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (c cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
