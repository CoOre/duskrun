package core_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// TestArtifactKeyWithoutDatabase covers the engines that have no database to
// name: redis always dumps the whole instance, and mongodump does when no
// database is selected. An empty key segment would produce "task//20260813_0200_"
// — a path with an empty directory and a nameless file.
func TestArtifactKeyWithoutDatabase(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cr, err := db.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, connector_config, created_at)
		 VALUES ('kc', 'fakeexec', 'direct', '{"host":"127.0.0.1","port":6379}', unixepoch())`)
	if err != nil {
		t.Fatal(err)
	}
	connID, _ := cr.LastInsertId()
	sr, err := db.ExecContext(ctx,
		`INSERT INTO storage (name, type, config, created_at) VALUES ('fs', 'localfs', ?, unixepoch())`,
		`{"root":"`+dir+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	storID, _ := sr.LastInsertId()
	tr, err := db.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, codec_chain, dumper_opts, cron, enabled, timeout_sec, created_at)
		 VALUES ('cache', ?, ?, '[]', '{}', '0 2 * * *', 1, 1800, unixepoch())`, connID, storID)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := tr.LastInsertId()
	db.Close()

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	runID, err := st.StartManualRun(ctx, taskID)
	if err != nil {
		t.Fatalf("StartManualRun: %v", err)
	}
	out, err := core.NewExecutor(st, nil, time.Now).Run(ctx, task, runID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(out.Key, "//") {
		t.Fatalf("key = %q, want no empty path segment", out.Key)
	}
	if !strings.HasPrefix(out.Key, "cache/all/") {
		t.Fatalf("key = %q, want the whole-instance dump labelled 'all'", out.Key)
	}
}
