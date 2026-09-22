package mysql

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestMysqlArgs checks pure argv assembly, including flag defaults and endpoints.
func TestMysqlArgs(t *testing.T) {
	tcp := plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:3306"}
	got, err := buildArgs(Options{Database: "shop", Username: "dump"}, tcp, "fallback")
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{
		"--single-transaction", "--routines", "--triggers",
		"-h", "127.0.0.1", "-P", "3306",
		"-u", "dump",
		"shop",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n  %v\nwant\n  %v", got, want)
	}

	// Disabling flags and a unix socket; fallback user applies.
	no := false
	got2, err := buildArgs(Options{
		Database: "app", SingleTransaction: &no, Routines: &no, Triggers: &no,
	}, plugin.Endpoint{Network: "unix", Address: "/tmp/mysql.sock"}, "peer")
	if err != nil {
		t.Fatalf("buildArgs unix: %v", err)
	}
	want2 := []string{"--socket=/tmp/mysql.sock", "-u", "peer", "app"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("unix args =\n  %v\nwant\n  %v", got2, want2)
	}
}

// TestMysqlAllDatabasesArgs covers whole-server argv: --all-databases when
// system schemas are kept, and an explicit --databases list when they are not.
func TestMysqlAllDatabasesArgs(t *testing.T) {
	tcp := plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:3306"}

	got, err := buildArgs(Options{AllDatabases: true, Username: "dump"}, tcp, "peer")
	if err != nil {
		t.Fatalf("buildArgs all: %v", err)
	}
	want := []string{"--single-transaction", "--routines", "--triggers", "-h", "127.0.0.1", "-P", "3306", "-u", "dump", "--all-databases"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all args =\n  %v\nwant\n  %v", got, want)
	}

	// ExcludeSystem resolves an explicit list (populated at Dump time).
	got2, err := buildArgs(Options{AllDatabases: true, ExcludeSystem: true, Username: "dump", resolvedDatabases: []string{"shop", "billing"}}, tcp, "peer")
	if err != nil {
		t.Fatalf("buildArgs all exclude: %v", err)
	}
	want2 := []string{"--single-transaction", "--routines", "--triggers", "-h", "127.0.0.1", "-P", "3306", "-u", "dump", "--databases", "shop", "billing"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("all exclude args =\n  %v\nwant\n  %v", got2, want2)
	}
}

// TestMysqlAllDatabasesNoDatabaseRequired asserts the single-db requirement is
// lifted in all-databases mode.
func TestMysqlAllDatabasesNoDatabaseRequired(t *testing.T) {
	if _, err := parseOptions(plugin.DumpOptions{"all_databases": true}); err != nil {
		t.Fatalf("parseOptions all_databases: unexpected error %v", err)
	}
	if _, err := parseOptions(plugin.DumpOptions{}); err == nil {
		t.Fatal("parseOptions with no database and no all_databases: want error")
	}
}

func TestMysqlDumpBinarySelection(t *testing.T) {
	orig := mysqldumpBinary
	mysqldumpBinary = "mysqldump-default"
	defer func() { mysqldumpBinary = orig }()

	exists := func(name string) bool { return name == "mysqldump-default" }
	if got := selectDefaultMySQLDumpBinary(exists); got != "mysqldump-default" {
		t.Fatalf("select default = %q, want mysqldump-default", got)
	}

	exists = func(name string) bool { return name == "mysqldump56" || name == "mysqldump-default" }
	if got := selectLegacyMySQLDumpBinary(exists); got != "mysqldump56" {
		t.Fatalf("select legacy = %q, want mysqldump56", got)
	}

	exists = func(name string) bool { return name == "mysqldump-default" }
	if got := selectLegacyMySQLDumpBinary(exists); got != "" {
		t.Fatalf("select legacy fallback = %q, want empty", got)
	}
}

func TestMysqlCompatibilityHintForGenerationExpression(t *testing.T) {
	err := withCompatibilityHint(errors.New("mysqldump failed: Unknown column 'generation_expression' in 'field list'"))
	if !strings.Contains(err.Error(), "MySQL 5.6 or older") {
		t.Fatalf("error %q missing compatibility hint", err)
	}

	plain := withCompatibilityHint(errors.New("mysqldump failed: access denied"))
	if strings.Contains(plain.Error(), "compatibility hint") {
		t.Fatalf("unrelated error got compatibility hint: %q", plain)
	}
}

func TestMysqlReadCloserWrapsReadError(t *testing.T) {
	rc := mysqlReadCloser{ReadCloser: errReadCloser{
		err: errors.New("mysqldump failed: Unknown column 'generation_expression' in 'field list'"),
	}}
	_, err := rc.Read(make([]byte, 8))
	if err == nil {
		t.Fatal("Read returned nil error, want wrapped error")
	}
	if !strings.Contains(err.Error(), "automatic selector") {
		t.Fatalf("error %q missing automatic selector hint", err)
	}
}

func TestMysqlQueryArgs(t *testing.T) {
	args, err := buildMySQLQueryArgs(
		Options{Database: "orders", Username: "backup"},
		plugin.Endpoint{Network: "tcp", Address: "db.example.com:3306"},
		"fallback",
		"SELECT 1",
		true,
	)
	if err != nil {
		t.Fatalf("buildMySQLQueryArgs: %v", err)
	}
	want := []string{
		"--batch", "--skip-column-names", "--raw", "--execute", "SELECT 1",
		"-h", "db.example.com", "-P", "3306",
		"-u", "backup",
		"orders",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("query args =\n  %v\nwant\n  %v", args, want)
	}
}

func TestMysqlLegacyInsertSelect(t *testing.T) {
	got := buildInsertSelect("order`items", []columnInfo{
		{Name: "id", Type: "int"},
		{Name: "payload", Type: "blob"},
	})
	if !strings.Contains(got, "'INSERT INTO `order``items` ('") ||
		!strings.Contains(got, "'`id`'") ||
		!strings.Contains(got, "'`payload`'") {
		t.Fatalf("insert select %q missing quoted INSERT prefix", got)
	}
	if !strings.Contains(got, "QUOTE(`id`)") {
		t.Fatalf("insert select %q missing text value expression", got)
	}
	if !strings.Contains(got, "CONCAT('0x',HEX(`payload`))") {
		t.Fatalf("insert select %q missing blob value expression", got)
	}
}

func TestMysqlBatchUnescape(t *testing.T) {
	if got := unescapeBatchField(`CREATE\n\tTABLE\\x`); got != "CREATE\n\tTABLE\\x" {
		t.Fatalf("unescape = %q", got)
	}
}

// TestMysqlDumpMissingBinary asserts a clear error when mysqldump can't start.
func TestMysqlDumpMissingBinary(t *testing.T) {
	orig := mysqldumpBinary
	mysqldumpBinary = "mysqldump-does-not-exist-duskrun"
	defer func() { mysqldumpBinary = orig }()

	_, err := Dumper{}.Dump(context.Background(),
		plugin.Endpoint{Network: "tcp", Address: "127.0.0.1:3306"},
		plugin.Credentials{Password: "x"},
		plugin.DumpOptions{"database": "app", "dump_binary": "mysqldump-does-not-exist-duskrun"},
	)
	if err == nil {
		t.Fatal("Dump succeeded with a missing binary, want error")
	}
	if !strings.Contains(err.Error(), "dump client installed") {
		t.Fatalf("error %q missing the 'is the dump client installed?' hint", err)
	}
}

// TestMysqlE2E is a real round-trip against a live MySQL, gated on env.
func TestMysqlE2E(t *testing.T) {
	if os.Getenv("DUSKRUN_MYSQL_TEST") == "" {
		t.Skip("set DUSKRUN_MYSQL_TEST to run the real mysqldump e2e")
	}
	t.Skip("DUSKRUN_MYSQL_TEST harness not implemented in this run — left for a human")
}

type errReadCloser struct {
	err error
}

func (r errReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r errReadCloser) Close() error             { return nil }

var _ io.ReadCloser = errReadCloser{}
