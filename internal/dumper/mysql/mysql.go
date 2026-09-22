// Package mysql implements the mysql Dumper via streaming mysqldump.
//
// Process plumbing (stdout streaming, stderr drain, exit→read error) is shared
// with postgres through internal/dumper/execstream; this package only builds the
// mysqldump argv and passes the password via MYSQL_PWD.
package mysql

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/duskrun/duskrun/internal/dumper/execstream"
	"github.com/duskrun/duskrun/internal/plugin"
)

// Tool names are overridable in tests to force start/probe errors.
var (
	mysqldumpBinary = "mysqldump"
	mysqlBinary     = "mysql"
)

func init() {
	plugin.Dumpers.Register("mysql", func(_ []byte) (plugin.Dumper, error) {
		return Dumper{}, nil
	})
}

// Options are the mysql-specific dump options (task.dumper_opts JSON). The three
// consistency/completeness flags default to ON when omitted (nil pointer).
type Options struct {
	Database          string `json:"database"`
	Username          string `json:"username"`
	DumpBinary        string `json:"dump_binary"`
	SingleTransaction *bool  `json:"single_transaction"` // default true
	Routines          *bool  `json:"routines"`           // default true
	Triggers          *bool  `json:"triggers"`           // default true
	AllDatabases      bool   `json:"all_databases"`      // dump every database on the server
	ExcludeSystem     bool   `json:"exclude_system"`     // with AllDatabases: skip system schemas

	// resolvedDatabases is the explicit list to dump when AllDatabases &&
	// ExcludeSystem; it is populated at Dump time (never from JSON) because
	// mysqldump has no native "all databases except system" flag.
	resolvedDatabases []string
}

// systemSchemas are the server-managed databases dropped by ExcludeSystem.
var systemSchemas = map[string]bool{
	"information_schema": true,
	"performance_schema": true,
	"mysql":              true,
	"sys":                true,
}

func parseOptions(opt plugin.DumpOptions) (Options, error) {
	var o Options
	b, err := json.Marshal(opt)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("mysql: bad options: %w", err)
	}
	// In all-databases mode mysqldump spans the whole server, so no single
	// database name is required.
	if !o.AllDatabases && o.Database == "" {
		return o, fmt.Errorf("mysql: database is required")
	}
	return o, nil
}

// Dumper is the mysql driver.
type Dumper struct{}

func (Dumper) Mode() plugin.DumpMode { return plugin.ModeStream }

func (Dumper) DumpStaged(context.Context, plugin.Endpoint, plugin.Credentials, plugin.DumpOptions) (plugin.RemotePath, plugin.Fetcher, error) {
	return plugin.RemotePath{}, nil, plugin.ErrModeUnsupported
}

// RestoreHint returns the human restore command for the produced dump.
func (Dumper) RestoreHint(opt plugin.DumpOptions) string {
	o, _ := parseOptions(opt)
	if o.AllDatabases {
		// --all-databases / --databases dumps carry their own USE statements.
		return "mysql < <file>"
	}
	return "mysql " + o.Database + " < <file>"
}

// enabled reports a *bool that defaults to true when nil.
func enabled(b *bool) bool { return b == nil || *b }

type dumpPlan struct {
	Binary string
	Legacy bool
}

func planDump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) dumpPlan {
	if o.DumpBinary != "" {
		return dumpPlan{Binary: o.DumpBinary}
	}
	needsLegacy := false
	if has, err := detectGenerationExpressionColumn(ctx, ep, cr, o); err == nil {
		needsLegacy = !has
	}
	if needsLegacy {
		if binary := selectLegacyMySQLDumpBinary(binaryExists); binary != "" {
			return dumpPlan{Binary: binary}
		}
		return dumpPlan{Legacy: true}
	}
	return dumpPlan{Binary: selectDefaultMySQLDumpBinary(binaryExists)}
}

func selectLegacyMySQLDumpBinary(exists existsFunc) string {
	for _, name := range legacyMySQLDumpNames() {
		if exists(name) {
			return name
		}
	}
	return ""
}

func selectDefaultMySQLDumpBinary(exists existsFunc) string {
	for _, name := range defaultMySQLDumpNames() {
		if exists(name) {
			return name
		}
	}
	return mysqldumpBinary
}

func legacyMySQLDumpNames() []string {
	return []string{
		"mysqldump56",
		"mysqldump-5.6",
		"mysqldump5.6",
		"/usr/local/bin/mysqldump56",
		"/usr/bin/mysqldump56",
	}
}

func defaultMySQLDumpNames() []string {
	if mysqldumpBinary == "mariadb-dump" {
		return []string{"mariadb-dump", "mysqldump"}
	}
	return []string{mysqldumpBinary, "mariadb-dump"}
}

// buildArgs assembles the mysqldump argv. Pure, for unit testing.
func buildArgs(o Options, ep plugin.Endpoint, fallbackUser string) ([]string, error) {
	var args []string
	if enabled(o.SingleTransaction) {
		args = append(args, "--single-transaction")
	}
	if enabled(o.Routines) {
		args = append(args, "--routines")
	}
	if enabled(o.Triggers) {
		args = append(args, "--triggers")
	}
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-P", port)
	case "unix":
		args = append(args, "--socket="+ep.Address)
	default:
		return nil, fmt.Errorf("mysql: unsupported endpoint network %q", ep.Network)
	}
	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	switch {
	case o.AllDatabases && len(o.resolvedDatabases) > 0:
		// ExcludeSystem: an explicit database list resolved at Dump time.
		args = append(args, "--databases")
		args = append(args, o.resolvedDatabases...)
	case o.AllDatabases:
		args = append(args, "--all-databases")
	default:
		args = append(args, o.Database)
	}
	return args, nil
}

// Dump starts mysqldump and streams its stdout. Password via MYSQL_PWD.
func (Dumper) Dump(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, opt plugin.DumpOptions) (io.ReadCloser, error) {
	o, err := parseOptions(opt)
	if err != nil {
		return nil, err
	}

	// mysqldump has no "all databases except system" flag, so resolve an explicit
	// list up front when the user asked to skip system schemas.
	if o.AllDatabases && o.ExcludeSystem {
		dbs, err := listUserDatabases(ctx, ep, cr, o)
		if err != nil {
			return nil, fmt.Errorf("mysql: list databases: %w", err)
		}
		if len(dbs) == 0 {
			return nil, fmt.Errorf("mysql: no user databases to back up")
		}
		o.resolvedDatabases = dbs
	}

	args, err := buildArgs(o, ep, cr.Username)
	if err != nil {
		return nil, err
	}

	plan := planDump(ctx, ep, cr, o)
	if plan.Legacy {
		// Ancient MySQL (≤5.6) with no compatible mysqldump on PATH: the modern
		// client's own information_schema queries (generation_expression) fail, so
		// fall back to the manual per-table dumper. For a whole-server dump iterate
		// it across every user database.
		if o.AllDatabases {
			return legacyDumpAllViaMySQL(ctx, ep, cr, o)
		}
		return legacyDumpViaMySQL(ctx, ep, cr, o)
	}
	return startDumpCommand(ctx, plan.Binary, args, cr.Password)
}

// listUserDatabases returns the server's databases minus the system schemas,
// sorted, for an ExcludeSystem all-databases dump.
func listUserDatabases(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) ([]string, error) {
	out, err := mysqlOutput(ctx, ep, cr, o, "SHOW DATABASES", false)
	if err != nil {
		return nil, err
	}
	var dbs []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name == "" || systemSchemas[strings.ToLower(name)] {
			continue
		}
		dbs = append(dbs, name)
	}
	return dbs, sc.Err()
}

func startDumpCommand(ctx context.Context, binary string, args []string, password string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+password)
	rc, err := execstream.Stream(cmd, binary)
	if err != nil {
		return nil, fmt.Errorf("mysql: start %s: %w (is the dump client installed?)", binary, err)
	}
	return mysqlReadCloser{ReadCloser: rc}, nil
}

type mysqlReadCloser struct {
	io.ReadCloser
}

func (r mysqlReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		err = withCompatibilityHint(err)
	}
	return n, err
}

func withCompatibilityHint(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "generation_expression") &&
		strings.Contains(msg, "Unknown column") {
		return fmt.Errorf("%w (mysql compatibility hint: the source server is likely MySQL 5.6 or older, and the automatic selector did not find a compatible legacy mysqldump on PATH; install a MySQL 5.6-compatible client as mysqldump56/mysqldump-5.6, or upgrade the source server to MySQL 5.7+/MariaDB; --column-statistics does not fix this error)", err)
	}
	return err
}

type tableInfo struct {
	Name string
	Type string
}

type columnInfo struct {
	Name string
	Type string
}

func legacyDumpViaMySQL(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) (io.ReadCloser, error) {
	tables, err := listTables(ctx, ep, cr, o)
	if err != nil {
		return nil, fmt.Errorf("mysql legacy dump: list tables: %w", err)
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeLegacyDump(ctx, pw, ep, cr, o, tables))
	}()
	return pr, nil
}

// legacyDumpAllViaMySQL streams a whole-server dump for ancient MySQL by running
// the manual per-table dumper against every user database in turn. System schemas
// are always skipped (they cannot be dumped this way and must not be restored).
func legacyDumpAllViaMySQL(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) (io.ReadCloser, error) {
	dbs, err := listUserDatabases(ctx, ep, cr, o)
	if err != nil {
		return nil, fmt.Errorf("mysql legacy all: list databases: %w", err)
	}
	if len(dbs) == 0 {
		return nil, fmt.Errorf("mysql legacy all: no user databases to back up")
	}
	pr, pw := io.Pipe()
	go func() {
		var werr error
		for _, db := range dbs {
			od := o
			od.Database = db
			tables, e := listTables(ctx, ep, cr, od)
			if e != nil {
				werr = fmt.Errorf("mysql legacy all: list tables %s: %w", db, e)
				break
			}
			if e := writeLegacyDump(ctx, pw, ep, cr, od, tables); e != nil {
				werr = fmt.Errorf("mysql legacy all: dump %s: %w", db, e)
				break
			}
		}
		pw.CloseWithError(werr)
	}()
	return pr, nil
}

func writeLegacyDump(ctx context.Context, w io.Writer, ep plugin.Endpoint, cr plugin.Credentials, o Options, tables []tableInfo) error {
	if _, err := fmt.Fprintf(w, "-- Duskrun legacy MySQL dump\nCREATE DATABASE IF NOT EXISTS %s;\nUSE %s;\nSET FOREIGN_KEY_CHECKS=0;\n\n", quoteIdent(o.Database), quoteIdent(o.Database)); err != nil {
		return err
	}
	for _, t := range tables {
		if t.Type != "BASE TABLE" {
			continue
		}
		create, err := showCreate(ctx, ep, cr, o, t)
		if err != nil {
			return fmt.Errorf("show create %s: %w", t.Name, err)
		}
		if _, err := fmt.Fprintf(w, "DROP TABLE IF EXISTS %s;\n%s;\n\n", quoteIdent(t.Name), create); err != nil {
			return err
		}
		cols, err := listColumns(ctx, ep, cr, o, t.Name)
		if err != nil {
			return fmt.Errorf("columns %s: %w", t.Name, err)
		}
		if len(cols) > 0 {
			if err := streamInsertRows(ctx, w, ep, cr, o, t.Name, cols); err != nil {
				return fmt.Errorf("data %s: %w", t.Name, err)
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
	}
	for _, t := range tables {
		if t.Type != "VIEW" {
			continue
		}
		create, err := showCreate(ctx, ep, cr, o, t)
		if err != nil {
			return fmt.Errorf("show create view %s: %w", t.Name, err)
		}
		if _, err := fmt.Fprintf(w, "DROP VIEW IF EXISTS %s;\n%s;\n\n", quoteIdent(t.Name), create); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "SET FOREIGN_KEY_CHECKS=1;\n")
	return err
}

func listTables(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) ([]tableInfo, error) {
	const sql = `SELECT table_name, table_type FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type IN ('BASE TABLE','VIEW') ORDER BY table_type='VIEW', table_name`
	out, err := mysqlOutput(ctx, ep, cr, o, sql, false)
	if err != nil {
		return nil, err
	}
	var tables []tableInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) >= 2 {
			tables = append(tables, tableInfo{Name: parts[0], Type: parts[1]})
		}
	}
	return tables, sc.Err()
}

func listColumns(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options, table string) ([]columnInfo, error) {
	sql := fmt.Sprintf(`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=%s ORDER BY ordinal_position`, quoteString(table))
	out, err := mysqlOutput(ctx, ep, cr, o, sql, false)
	if err != nil {
		return nil, err
	}
	var cols []columnInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) >= 2 {
			cols = append(cols, columnInfo{Name: parts[0], Type: parts[1]})
		}
	}
	return cols, sc.Err()
}

func showCreate(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options, t tableInfo) (string, error) {
	kind := "TABLE"
	if t.Type == "VIEW" {
		kind = "VIEW"
	}
	out, err := mysqlOutput(ctx, ep, cr, o, "SHOW CREATE "+kind+" "+quoteIdent(t.Name), false)
	if err != nil {
		return "", err
	}
	line := strings.TrimRight(string(out), "\n")
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unexpected SHOW CREATE output")
	}
	return unescapeBatchField(parts[1]), nil
}

func streamInsertRows(ctx context.Context, w io.Writer, ep plugin.Endpoint, cr plugin.Credentials, o Options, table string, cols []columnInfo) error {
	sql := buildInsertSelect(table, cols)
	args, err := buildMySQLQueryArgs(o, ep, cr.Username, sql, true)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, mysqlBinary, args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cr.Password)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func buildInsertSelect(table string, cols []columnInfo) string {
	var pieces []string
	pieces = append(pieces, quoteString("INSERT INTO "+quoteIdent(table)+" ("))
	for i, c := range cols {
		if i > 0 {
			pieces = append(pieces, quoteString(","))
		}
		pieces = append(pieces, quoteString(quoteIdent(c.Name)))
	}
	pieces = append(pieces, quoteString(") VALUES ("))
	for i, c := range cols {
		if i > 0 {
			pieces = append(pieces, quoteString(","))
		}
		pieces = append(pieces, valueSQL(c))
	}
	pieces = append(pieces, quoteString(");"))
	return "SELECT CONCAT(" + strings.Join(pieces, ",") + ") FROM " + quoteIdent(table)
}

func valueSQL(c columnInfo) string {
	id := quoteIdent(c.Name)
	switch strings.ToLower(c.Type) {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob", "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection":
		return "IF(" + id + " IS NULL,'NULL',CONCAT('0x',HEX(" + id + ")))"
	default:
		return "IF(" + id + " IS NULL,'NULL',QUOTE(" + id + "))"
	}
}

func mysqlOutput(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options, sql string, raw bool) ([]byte, error) {
	args, err := buildMySQLQueryArgs(o, ep, cr.Username, sql, raw)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, mysqlBinary, args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cr.Password)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func detectGenerationExpressionColumn(ctx context.Context, ep plugin.Endpoint, cr plugin.Credentials, o Options) (bool, error) {
	const sql = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='information_schema' AND table_name='COLUMNS' AND column_name='GENERATION_EXPRESSION'`
	args, err := buildMySQLQueryArgs(o, ep, cr.Username, sql, true)
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, mysqlBinary, args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cr.Password)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func buildMySQLQueryArgs(o Options, ep plugin.Endpoint, fallbackUser, sql string, raw bool) ([]string, error) {
	args := []string{"--batch", "--skip-column-names"}
	if raw {
		args = append(args, "--raw")
	}
	args = append(args, "--execute", sql)
	switch ep.Network {
	case "tcp":
		host, port, err := splitHostPort(ep.Address)
		if err != nil {
			return nil, err
		}
		args = append(args, "-h", host, "-P", port)
	case "unix":
		args = append(args, "--socket="+ep.Address)
	default:
		return nil, fmt.Errorf("mysql: unsupported endpoint network %q", ep.Network)
	}
	user := o.Username
	if user == "" {
		user = fallbackUser
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	if o.Database != "" {
		args = append(args, o.Database)
	}
	return args, nil
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`) + "'"
}

func unescapeBatchField(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

type existsFunc func(string) bool

func binaryExists(name string) bool {
	if strings.ContainsRune(name, os.PathSeparator) {
		info, err := os.Stat(name)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("mysql: bad tcp address %q", addr)
	}
	host, port = addr[:i], addr[i+1:]
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("mysql: bad tcp port in %q", addr)
	}
	return host, port, nil
}
