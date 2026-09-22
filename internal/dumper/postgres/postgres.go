// Package postgres implements the postgres Dumper via streaming pg_dump.
//
// The process plumbing (stdout streaming, concurrent stderr drain, exit→read
// error) lives in internal/dumper/execstream; this package only builds the
// pg_dump argv and wires credentials via PGPASSWORD.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/duskrun/duskrun/internal/dumper/execstream"
	"github.com/duskrun/duskrun/internal/plugin"
)

func init() {
	plugin.Dumpers.Register("postgres", func(_ []byte) (plugin.Dumper, error) {
		return Dumper{}, nil
	})
}

var (
	pgDumpBinary    = "pg_dump"
	pgDumpallBinary = "pg_dumpall"
	psqlBinary      = "psql"
)

// Options are the postgres-specific dump options (task.dumper_opts JSON).
type Options struct {
	Database      string   `json:"database"`
	Username      string   `json:"username"`
	Format        string   `json:"format"`         // "custom" (default) | "plain" | "directory"
	Jobs          int      `json:"jobs"`           // -j (directory/custom parallel)
	ExcludeTable  []string `json:"exclude_table"`  // --exclude-table entries
	AllDatabases  bool     `json:"all_databases"`  // dump the whole cluster via pg_dumpall
	ExcludeSystem bool     `json:"exclude_system"` // with AllDatabases: drop postgres/template1
}

func parseOptions(opt plugin.DumpOptions) (Options, error) {
	var o Options
	b, err := json.Marshal(opt)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("postgres: bad options: %w", err)
	}
	// In all-databases mode pg_dumpall dumps the whole cluster, so no single
	// database name is needed (or used).
	if !o.AllDatabases && o.Database == "" {
		return o, fmt.Errorf("postgres: database is required")
	}
	if o.Format == "" {
		o.Format = "custom"
	}
	return o, nil
}

// Dumper is the postgres driver.
type Dumper struct{}

func (Dumper) Mode() plugin.DumpMode { return plugin.ModeStream }

func (Dumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}

// RestoreHint returns the human restore command for the produced dump.
func (Dumper) RestoreHint(opt plugin.DumpOptions) string {
	o, _ := parseOptions(opt)
	if o.AllDatabases {
		// pg_dumpall output is plain SQL that recreates every database and the
		// cluster globals; restore it as a superuser against a running cluster.
		return "psql -f <file>"
	}
	if o.Format == "plain" {
		return "psql -d " + o.Database + " -f <file>"
	}
	return "pg_restore -d " + o.Database + " <file>"
}

// buildDumpallArgs assembles the pg_dumpall argv for a whole-cluster dump. Pure,
// for unit testing. pg_dumpall always emits plain SQL and has no --format/-j.
func buildDumpallArgs(o Options, ep plugin.Endpoint, fallbackUser string) ([]string, error) {
	args := []string{"--no-password"}
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-p", port)
	case "unix":
		args = append(args, "-h", ep.Address)
	default:
		return nil, fmt.Errorf("postgres: unsupported endpoint network %q", ep.Network)
	}
	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "-U", user)
	}
	if o.ExcludeSystem {
		// template0 is never dumped by pg_dumpall (it disallows connections);
		// drop the remaining maintenance databases so only user data is captured.
		args = append(args, "--exclude-database=template1", "--exclude-database=postgres")
	}
	return args, nil
}

// buildArgs assembles the pg_dump argv from options and the endpoint. fallbackUser
// is the connection's credential username, used when options omit one. It is a
// pure function so the argv can be unit-tested without spawning pg_dump.
func buildArgs(o Options, ep plugin.Endpoint, fallbackUser string) ([]string, error) {
	args := []string{"--format=" + formatFlag(o.Format), "--no-password"}
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-p", port)
	case "unix":
		args = append(args, "-h", ep.Address) // pg_dump accepts a socket dir as host
	default:
		return nil, fmt.Errorf("postgres: unsupported endpoint network %q", ep.Network)
	}
	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "-U", user)
	}
	if o.Jobs > 0 && o.Format != "plain" {
		args = append(args, "-j", strconv.Itoa(o.Jobs))
	}
	for _, t := range o.ExcludeTable {
		if t = strings.TrimSpace(t); t != "" {
			args = append(args, "--exclude-table", t)
		}
	}
	args = append(args, o.Database)
	return args, nil
}

// Dump starts pg_dump (or pg_dumpall for a whole-cluster dump) and streams its
// stdout. Password is passed via PGPASSWORD.
func (Dumper) Dump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, opt plugin.DumpOptions) (io.ReadCloser, error) {
	o, err := parseOptions(opt)
	if err != nil {
		return nil, err
	}

	var (
		args []string
		tool = "pg_dump"
	)
	if o.AllDatabases {
		tool = "pg_dumpall"
		args, err = buildDumpallArgs(o, ep, cr.Username)
	} else {
		args, err = buildArgs(o, ep, cr.Username)
	}
	if err != nil {
		return nil, err
	}

	binary := pgDumpBinary
	if o.AllDatabases {
		binary = pgDumpallBinary
	}
	if major, err := detectServerMajor(ctx, ep, cr, o); err == nil && major > 0 {
		if o.AllDatabases {
			binary = selectPgDumpallBinary(major, binaryExists)
		} else {
			binary = selectPgDumpBinary(major, binaryExists)
		}
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cr.Password)

	rc, err := execstream.Stream(cmd, tool)
	if err != nil {
		return nil, fmt.Errorf("postgres: start %s: %w (is %s installed?)", tool, err, tool)
	}
	return rc, nil
}

func detectServerMajor(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) (int, error) {
	args, err := buildPsqlVersionArgs(o, ep, cr.Username)
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, psqlBinary, args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cr.Password)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return n / 10000, nil
}

func buildPsqlVersionArgs(o Options, ep plugin.Endpoint, fallbackUser string) ([]string, error) {
	args := []string{"--no-password", "-Atqc", "SHOW server_version_num"}
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-p", port)
	case "unix":
		args = append(args, "-h", ep.Address)
	default:
		return nil, fmt.Errorf("postgres: unsupported endpoint network %q", ep.Network)
	}
	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "-U", user)
	}
	// In all-databases mode there is no target database; probe the always-present
	// maintenance database instead.
	db := o.Database
	if db == "" {
		db = "postgres"
	}
	args = append(args, "-d", db)
	return args, nil
}

type existsFunc func(string) bool

func selectPgDumpBinary(serverMajor int, exists existsFunc) string {
	return selectVersionedPgBinary("pg_dump", pgDumpBinary, serverMajor, exists)
}

func selectPgDumpallBinary(serverMajor int, exists existsFunc) string {
	return selectVersionedPgBinary("pg_dumpall", pgDumpallBinary, serverMajor, exists)
}

// selectVersionedPgBinary picks the client binary matching the server major, or
// the newest available one (a newer client can dump older servers), falling back
// to the bare tool name on PATH.
func selectVersionedPgBinary(tool, fallback string, serverMajor int, exists existsFunc) string {
	if serverMajor > 0 {
		for _, name := range versionedPgNames(tool, serverMajor) {
			if exists(name) {
				return name
			}
		}
	}
	for major := 18; major >= serverMajor && major >= 9; major-- {
		for _, name := range versionedPgNames(tool, major) {
			if exists(name) {
				return name
			}
		}
	}
	return fallback
}

func versionedPgNames(tool string, major int) []string {
	return []string{
		filepath.Join("/usr/lib/postgresql", strconv.Itoa(major), "bin", tool),
		fmt.Sprintf("%s-%d", tool, major),
	}
}

func binaryExists(name string) bool {
	if strings.ContainsRune(name, os.PathSeparator) {
		info, err := os.Stat(name)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func formatFlag(f string) string {
	switch f {
	case "plain":
		return "plain"
	case "directory":
		return "directory"
	default:
		return "custom"
	}
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("postgres: bad tcp address %q", addr)
	}
	return addr[:i], addr[i+1:], nil
}
