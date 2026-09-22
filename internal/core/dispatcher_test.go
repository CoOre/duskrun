package core_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// clock is a settable time source for deterministic dispatcher tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// openStore opens a temp-file Store and a raw seeding handle on the same DB.
func openStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disp.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// seedEnabledTask inserts a connection+storage+task (enabled) via a raw handle
// on the same DB file and returns the task id. Uses the real schema applied by
// sqlite.Open, so this exercises real SQLite end to end.
func seedEnabledTask(t *testing.T, path, cron string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO storage (name, type, created_at) VALUES ('s','localfs', unixepoch())`,
	); err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, cron, enabled, created_at)
		 VALUES ('t', 1, 1, ?, 1, unixepoch())`, cron,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func countRuns(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM run`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDispatcherEnqueuesDueTask: a task whose 02:00 activation falls inside the
// tick window (lastTick 01:00 → now 02:01) is enqueued exactly once.
func TestDispatcherEnqueuesDueTask(t *testing.T) {
	st, path := openStore(t)
	seedEnabledTask(t, path, "0 2 * * *")

	clk := &clock{t: time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)}
	d := core.NewDispatcher(st, clk.now, nil)

	clk.t = time.Date(2026, 7, 22, 2, 1, 0, 0, time.UTC)
	n, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("enqueued = %d, want 1", n)
	}
	if got := countRuns(t, path); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}

	// A second tick in the same minute window must not double-enqueue: the run
	// is still queued (active), so the enqueue is refused and skipped.
	clk.t = time.Date(2026, 7, 22, 2, 1, 30, 0, time.UTC)
	n2, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second tick enqueued = %d, want 0 (not due again)", n2)
	}
}

// TestDispatcherSkipsWhenActive: with a run already active, a due tick refuses to
// create a second one (misfire=skip) — the active-run invariant holds.
func TestDispatcherSkipsWhenActive(t *testing.T) {
	st, path := openStore(t)
	taskID := seedEnabledTask(t, path, "0 2 * * *")

	// Pre-existing active run.
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("seed Enqueue: %v", err)
	}

	clk := &clock{t: time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)}
	d := core.NewDispatcher(st, clk.now, nil)

	clk.t = time.Date(2026, 7, 22, 2, 1, 0, 0, time.UTC)
	n, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Fatalf("enqueued = %d, want 0 (task already active)", n)
	}
	if got := countRuns(t, path); got != 1 {
		t.Fatalf("runs = %d, want 1 (no second active run)", got)
	}
}
