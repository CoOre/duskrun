// Package redis implements the redis Dumper via streaming `redis-cli --rdb -`.
//
// Process plumbing (stdout streaming, stderr drain, exit→read error) is shared
// with the other dumpers through internal/dumper/execstream; this package builds
// the redis-cli argv and passes the password via REDISCLI_AUTH.
//
// `--rdb -` writes the RDB snapshot to stdout and reports its progress on stderr,
// which is what makes redis a streaming dumper rather than a staged one. Verified
// against redis-cli 7.4: a connection failure exits non-zero, so execstream turns
// it into a read error and the run fails instead of storing an empty artifact.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/duskrun/duskrun/internal/dumper/execstream"
	"github.com/duskrun/duskrun/internal/plugin"
)

// redisCLIBinary is overridable in tests to force start failures and to point at
// a stub that reproduces a non-zero exit.
var redisCLIBinary = "redis-cli"

func init() {
	plugin.Dumpers.Register("redis", func(_ []byte) (plugin.Dumper, error) {
		return Dumper{}, nil
	})
}

// Options are the redis-specific dump options (task.dumper_opts JSON).
//
// There is deliberately no database-number option: --rdb always transfers the
// whole keyspace, so a `db` field would suggest a filter the dump does not apply.
type Options struct {
	Username string `json:"username"` // redis 6+ ACL user; empty = default user
	TLS      bool   `json:"tls"`      // connect with --tls
}

func parseOptions(opt plugin.DumpOptions) (Options, error) {
	var o Options
	b, err := json.Marshal(opt)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("redis: bad options: %w", err)
	}
	return o, nil
}

// Dumper is the redis driver.
type Dumper struct{}

func (Dumper) Mode() plugin.DumpMode { return plugin.ModeStream }

func (Dumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}

// RestoreHint returns the human restore command for the produced RDB file.
//
// Restoring an RDB is a server operation, not a client one: the file is put in
// place and the server loads it at startup. The hint says so, because a hint that
// looked like a one-liner would be worse than none.
func (Dumper) RestoreHint(plugin.DumpOptions) string {
	return "systemctl stop redis && cp <file> /var/lib/redis/dump.rdb && chown redis: /var/lib/redis/dump.rdb && systemctl start redis"
}

// buildArgs assembles the redis-cli argv. Pure, so the argv can be asserted
// without spawning redis-cli. The password is never part of it: -a is visible in
// `ps` and redis-cli itself warns about it — REDISCLI_AUTH carries it instead.
func buildArgs(o Options, ep plugin.Endpoint, fallbackUser string) ([]string, error) {
	var args []string
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-p", port)
	case "unix":
		args = append(args, "-s", ep.Address)
	default:
		return nil, fmt.Errorf("redis: unsupported endpoint network %q", ep.Network)
	}

	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "--user", user)
	}
	if o.TLS {
		args = append(args, "--tls")
	}
	// "-" is the filename that means stdout.
	args = append(args, "--rdb", "-")
	return args, nil
}

// Dump starts redis-cli and streams the RDB snapshot from its stdout.
func (Dumper) Dump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, opt plugin.DumpOptions) (io.ReadCloser, error) {
	o, err := parseOptions(opt)
	if err != nil {
		return nil, err
	}
	args, err := buildArgs(o, ep, cr.Username)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, redisCLIBinary, args...)
	cmd.Env = os.Environ()
	if cr.Password != "" {
		cmd.Env = append(cmd.Env, "REDISCLI_AUTH="+cr.Password)
	}

	rc, err := execstream.Stream(cmd, "redis-cli")
	if err != nil {
		return nil, fmt.Errorf("redis: start redis-cli: %w (is redis-cli installed?)", err)
	}
	return rc, nil
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("redis: bad tcp address %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}
