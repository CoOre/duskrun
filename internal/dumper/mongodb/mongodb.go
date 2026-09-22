// Package mongodb implements the mongodb Dumper via streaming mongodump.
//
// Process plumbing (stdout streaming, stderr drain, exit→read error) is shared
// with postgres/mysql through internal/dumper/execstream; this package builds the
// mongodump argv and keeps the password out of it.
//
// mongodump has no password environment variable (unlike PGPASSWORD/MYSQL_PWD),
// and a password on the command line is readable by every process on the host, so
// the credential is written to a private temp config file that mongodump reads
// with --config and that is removed when the stream is closed.
package mongodb

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

// mongodumpBinary is overridable in tests to force start failures and to point
// at a stub that reproduces a non-zero exit.
var mongodumpBinary = "mongodump"

func init() {
	plugin.Dumpers.Register("mongodb", func(_ []byte) (plugin.Dumper, error) {
		return Dumper{}, nil
	})
}

// Options are the mongodb-specific dump options (task.dumper_opts JSON).
type Options struct {
	Database          string   `json:"database"`           // empty = every database
	Collection        string   `json:"collection"`         // requires Database
	ExcludeCollection []string `json:"exclude_collection"` // requires Database
	Query             string   `json:"query"`              // JSON filter, requires Collection
	ReadPreference    string   `json:"read_preference"`    // e.g. "secondaryPreferred"
	Username          string   `json:"username"`
	AuthDatabase      string   `json:"auth_database"` // --authenticationDatabase
}

func parseOptions(opt plugin.DumpOptions) (Options, error) {
	var o Options
	b, err := json.Marshal(opt)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("mongodb: bad options: %w", err)
	}
	if o.Collection != "" && o.Database == "" {
		return o, fmt.Errorf("mongodb: collection requires database")
	}
	if o.Query != "" && o.Collection == "" {
		return o, fmt.Errorf("mongodb: query requires collection")
	}
	if len(o.ExcludeCollection) > 0 && o.Database == "" {
		return o, fmt.Errorf("mongodb: exclude_collection requires database")
	}
	return o, nil
}

// Dumper is the mongodb driver.
type Dumper struct{}

func (Dumper) Mode() plugin.DumpMode { return plugin.ModeStream }

func (Dumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}

// RestoreHint returns the human restore command for the produced archive.
func (Dumper) RestoreHint(opt plugin.DumpOptions) string {
	o, _ := parseOptions(opt)
	if o.Database != "" {
		// The archive carries its own namespaces; --nsInclude keeps a restore of a
		// single-database archive from touching anything else on the target.
		return "mongorestore --archive=<file> --nsInclude='" + o.Database + ".*'"
	}
	return "mongorestore --archive=<file>"
}

// buildArgs assembles the mongodump argv. Pure, so the argv can be asserted
// without spawning mongodump. The password is never part of it — see the package
// comment; configPath is the private file carrying it (empty when there is none).
func buildArgs(o Options, ep plugin.Endpoint, fallbackUser, configPath string) ([]string, error) {
	// --archive with no value writes the archive to stdout, which is exactly what
	// the pipeline consumes. --gzip is deliberately absent: compression is the
	// codec chain's job, and doing it twice would make the artifact name lie.
	args := []string{"--archive"}

	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "--host", host, "--port", port)
	case "unix":
		// mongodump accepts a socket path where a host is expected.
		args = append(args, "--host", ep.Address)
	default:
		return nil, fmt.Errorf("mongodb: unsupported endpoint network %q", ep.Network)
	}

	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "--username", user)
		authDB := o.AuthDatabase
		if authDB == "" {
			authDB = "admin"
		}
		args = append(args, "--authenticationDatabase", authDB)
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	if o.Database != "" {
		args = append(args, "--db", o.Database)
	}
	if o.Collection != "" {
		args = append(args, "--collection", o.Collection)
	}
	for _, c := range o.ExcludeCollection {
		if c = strings.TrimSpace(c); c != "" {
			args = append(args, "--excludeCollection", c)
		}
	}
	if q := strings.TrimSpace(o.Query); q != "" {
		args = append(args, "--query", q)
	}
	if rp := strings.TrimSpace(o.ReadPreference); rp != "" {
		args = append(args, "--readPreference", rp)
	}
	return args, nil
}

// Dump starts mongodump and streams the archive from its stdout.
func (Dumper) Dump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, opt plugin.DumpOptions) (io.ReadCloser, error) {
	o, err := parseOptions(opt)
	if err != nil {
		return nil, err
	}

	configPath, cleanup, err := writePasswordConfig(cr.Password)
	if err != nil {
		return nil, err
	}

	args, err := buildArgs(o, ep, cr.Username, configPath)
	if err != nil {
		cleanup()
		return nil, err
	}

	cmd := exec.CommandContext(ctx, mongodumpBinary, args...)
	rc, err := execstream.Stream(cmd, "mongodump")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("mongodb: start mongodump: %w (is mongodump installed?)", err)
	}
	return cleanupReader{ReadCloser: rc, cleanup: cleanup}, nil
}

// writePasswordConfig writes the password into a 0600 file mongodump reads with
// --config. It returns an empty path and a no-op cleanup when there is no
// password, so trust/no-auth deployments spawn no file at all.
func writePasswordConfig(password string) (string, func(), error) {
	if password == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "duskrun-mongodump-*.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("mongodb: password config: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	// os.CreateTemp already creates with 0600; make it explicit rather than
	// depending on it, since the file holds a live credential.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("mongodb: password config: %w", err)
	}
	if _, err := f.WriteString("password: " + yamlQuote(password) + "\n"); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("mongodb: password config: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("mongodb: password config: %w", err)
	}
	return path, cleanup, nil
}

// yamlQuote renders s as a YAML double-quoted scalar. A password may legitimately
// contain #, :, quotes or a leading space, any of which changes the meaning of a
// bare scalar.
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// cleanupReader removes the password file once the pipeline closes the stream.
// The pipeline always closes the dump reader (RunPipeline defers it), including
// on failure, so the file's lifetime is the dump's.
type cleanupReader struct {
	io.ReadCloser
	cleanup func()
}

func (r cleanupReader) Close() error {
	err := r.ReadCloser.Close()
	r.cleanup()
	return err
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("mongodb: bad tcp address %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}
