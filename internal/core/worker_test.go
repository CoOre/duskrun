package core_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

var errFakeBoom = errors.New("fake dumper: connection refused")

func init() {
	plugin.Dumpers.Register("fakefail", func(_ []byte) (plugin.Dumper, error) {
		return fakeDumper{data: []byte("partial"), fail: errFakeBoom}, nil
	})
	plugin.Dumpers.Register("fakeslow", func(_ []byte) (plugin.Dumper, error) {
		return fakeDumper{block: 5 * time.Second}, nil
	})
}

// seedFakeTaskFull seeds a fake-engine task with explicit retries and timeout.
func seedFakeTaskFull(t *testing.T, path, dir, engine string, retries, timeoutSec int) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cr, err := db.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, connector_config, created_at)
		 VALUES ('fc', ?, 'direct', '{"host":"127.0.0.1","port":5432}', unixepoch())`, engine,
	)
	if err != nil {
		t.Fatal(err)
	}
	connID, _ := cr.LastInsertId()
	sr, err := db.ExecContext(ctx,
		`INSERT INTO storage (name, type, config, created_at)
		 VALUES ('fs', 'localfs', ?, unixepoch())`, `{"root":"`+dir+`"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	storID, _ := sr.LastInsertId()
	tr, err := db.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, codec_chain, dumper_opts, cron, enabled, retries, timeout_sec, created_at)
		 VALUES ('ft', ?, ?, '["zstd"]', '{"database":"testdb"}', '0 2 * * *', 1, ?, ?, unixepoch())`,
		connID, storID, retries, timeoutSec,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tr.LastInsertId()
	return id
}

func newTestPool(t *testing.T, st *sqlite.Store) *core.Pool {
	t.Helper()
	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	return core.NewPool(st, exec, core.PoolConfig{
		Workers: 1,
		Idle:    time.Millisecond,
		Backoff: func(int) time.Duration { return 0 }, // no real sleeps in tests
	}, nil)
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestWorkerExecutesTask: a queued fake-engine run is claimed, executed, and
// recorded success with an artifact.
func TestWorkerExecutesTask(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakeexec", 0, 1800)
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pool := newTestPool(t, st)
	did, err := pool.ProcessNext(context.Background(), "w0")
	if err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", did, err)
	}

	db := rawDB(t, path)
	var status string
	var artifacts int
	if err := db.QueryRow(`SELECT status FROM run WHERE task_id = ?`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(core.StatusSuccess) {
		t.Fatalf("run status = %q, want success", status)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifact`).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 1 {
		t.Fatalf("artifacts = %d, want 1", artifacts)
	}
}

// TestWorkerRetriesOnFailure: a failing dumper fails the run and, with retries=1,
// enqueues a fresh attempt=2 run.
func TestWorkerRetriesOnFailure(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakefail", 1, 1800)
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pool := newTestPool(t, st)
	did, err := pool.ProcessNext(context.Background(), "w0")
	if err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", did, err)
	}

	db := rawDB(t, path)
	// First run failed.
	var failed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run WHERE task_id = ? AND status = 'failed' AND attempt = 1`, taskID).
		Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("failed attempt-1 runs = %d, want 1", failed)
	}
	// A retry run with attempt=2 is queued.
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run WHERE task_id = ? AND status = 'queued' AND attempt = 2`, taskID).
		Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued attempt-2 runs = %d, want 1", queued)
	}
}

// TestWorkerRespectsTimeout: a dumper that blocks 5s under a 1s per-task timeout
// fails the run with a deadline error near the deadline instead of hanging.
// (timeout_sec is whole seconds, so 1s is the smallest expressible per-task
// timeout; the mechanism is identical to a 50ms one.)
func TestWorkerRespectsTimeout(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakeslow", 0, 1) // 1s per-task timeout
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pool := newTestPool(t, st)
	start := time.Now()
	did, err := pool.ProcessNext(context.Background(), "w0")
	if err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", did, err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("ProcessNext took %s, want it to abort near the 1s timeout", elapsed)
	}

	db := rawDB(t, path)
	var status, errMsg string
	if err := db.QueryRow(`SELECT status, error FROM run WHERE task_id = ?`, taskID).Scan(&status, &errMsg); err != nil {
		t.Fatal(err)
	}
	if status != string(core.StatusFailed) {
		t.Fatalf("run status = %q, want failed", status)
	}
	if errMsg == "" {
		t.Fatal("expected a non-empty error explaining the cancellation")
	}
}
