package postgres

import (
	"reflect"
	"testing"

	"github.com/duskrun/duskrun/internal/plugin"
)

// TestPostgresArgs checks the pure argv assembly for tcp and unix endpoints.
func TestPostgresArgs(t *testing.T) {
	tcp := plugin.Endpoint{Network: "tcp", Address: "db.example.com:5432"}
	got, err := buildArgs(Options{
		Database: "orders", Username: "backup", Format: "custom",
		Jobs: 4, ExcludeTable: []string{"audit_log", " "},
	}, tcp, "ignored-fallback")
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{
		"--format=custom", "--no-password",
		"-h", "db.example.com", "-p", "5432",
		"-U", "backup",
		"-j", "4",
		"--exclude-table", "audit_log",
		"orders",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n  %v\nwant\n  %v", got, want)
	}

	// Fallback username is used when options omit one; plain format drops -j.
	unix := plugin.Endpoint{Network: "unix", Address: "/var/run/postgresql"}
	got2, err := buildArgs(Options{Database: "app", Format: "plain", Jobs: 2}, unix, "peer")
	if err != nil {
		t.Fatalf("buildArgs unix: %v", err)
	}
	want2 := []string{"--format=plain", "--no-password", "-h", "/var/run/postgresql", "-U", "peer", "app"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("unix args =\n  %v\nwant\n  %v", got2, want2)
	}
}

// TestPostgresDumpallArgs covers the pg_dumpall argv for whole-cluster dumps.
func TestPostgresDumpallArgs(t *testing.T) {
	tcp := plugin.Endpoint{Network: "tcp", Address: "db.example.com:5432"}
	got, err := buildDumpallArgs(Options{AllDatabases: true, Username: "backup"}, tcp, "peer")
	if err != nil {
		t.Fatalf("buildDumpallArgs: %v", err)
	}
	want := []string{"--no-password", "-h", "db.example.com", "-p", "5432", "-U", "backup"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dumpall args =\n  %v\nwant\n  %v", got, want)
	}

	// ExcludeSystem drops the maintenance databases; fallback user applies.
	got2, err := buildDumpallArgs(Options{AllDatabases: true, ExcludeSystem: true}, tcp, "peer")
	if err != nil {
		t.Fatalf("buildDumpallArgs exclude: %v", err)
	}
	want2 := []string{"--no-password", "-h", "db.example.com", "-p", "5432", "-U", "peer", "--exclude-database=template1", "--exclude-database=postgres"}
	if !reflect.DeepEqual(got2, want2) {
		t.Fatalf("dumpall exclude args =\n  %v\nwant\n  %v", got2, want2)
	}
}

// TestPostgresAllDatabasesNoDatabaseRequired asserts the single-db requirement is
// lifted in all-databases mode.
func TestPostgresAllDatabasesNoDatabaseRequired(t *testing.T) {
	if _, err := parseOptions(plugin.DumpOptions{"all_databases": true}); err != nil {
		t.Fatalf("parseOptions all_databases: unexpected error %v", err)
	}
	if _, err := parseOptions(plugin.DumpOptions{}); err == nil {
		t.Fatal("parseOptions with no database and no all_databases: want error")
	}
}

func TestPostgresVersionSelection(t *testing.T) {
	exists := func(name string) bool {
		return name == "/usr/lib/postgresql/16/bin/pg_dump" ||
			name == "/usr/lib/postgresql/18/bin/pg_dump"
	}
	if got := selectPgDumpBinary(16, exists); got != "/usr/lib/postgresql/16/bin/pg_dump" {
		t.Fatalf("select exact = %q, want pg_dump 16", got)
	}

	exists = func(name string) bool {
		return name == "/usr/lib/postgresql/18/bin/pg_dump"
	}
	if got := selectPgDumpBinary(16, exists); got != "/usr/lib/postgresql/18/bin/pg_dump" {
		t.Fatalf("select newer = %q, want pg_dump 18", got)
	}

	exists = func(string) bool { return false }
	if got := selectPgDumpBinary(16, exists); got != "pg_dump" {
		t.Fatalf("select fallback = %q, want pg_dump", got)
	}
}

func TestPostgresPsqlVersionArgs(t *testing.T) {
	args, err := buildPsqlVersionArgs(
		Options{Database: "orders", Username: "backup"},
		plugin.Endpoint{Network: "tcp", Address: "db.example.com:5432"},
		"fallback",
	)
	if err != nil {
		t.Fatalf("buildPsqlVersionArgs: %v", err)
	}
	want := []string{
		"--no-password", "-Atqc", "SHOW server_version_num",
		"-h", "db.example.com", "-p", "5432",
		"-U", "backup",
		"-d", "orders",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("psql args =\n  %v\nwant\n  %v", args, want)
	}
}
