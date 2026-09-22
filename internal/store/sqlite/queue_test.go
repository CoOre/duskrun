package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/duskrun/duskrun/internal/core"
)

// openTemp opens a Store on a throwaway file DB (not :memory:, since the two
// pools must see the same database).
func openTemp(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedTask inserts the minimal connection+storage+task graph and returns task id.
func seedTask(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, created_at) VALUES ('c','postgres','direct', unixepoch())`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO storage (name, type, created_at) VALUES ('s','localfs', unixepoch())`,
	); err != nil {
		t.Fatal(err)
	}
	res, err := st.write.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, cron, created_at)
		 VALUES ('t', 1, 1, '0 2 * * *', unixepoch())`,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestClaimExactlyOnce is the load-bearing invariant: many workers racing to
// claim one queued run must yield exactly one winner; everyone else gets ErrNoRun.
func TestClaimExactlyOnce(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	taskID := seedTask(t, st)

	runID, err := st.Enqueue(ctx, taskID)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	const workers = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		noRun int
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := st.ClaimNext(ctx, "w")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrNoRun):
				noRun++
			case err != nil:
				t.Errorf("ClaimNext: %v", err)
			case r.ID == runID && r.Status == core.StatusRunning:
				wins++
			default:
				t.Errorf("unexpected claim: %+v", r)
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if noRun != workers-1 {
		t.Fatalf("ErrNoRun count = %d, want %d", noRun, workers-1)
	}
}

// TestActiveRunUniqueness proves the partial unique index forbids a second
// active run for the same task while one is queued/running.
func TestActiveRunUniqueness(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	taskID := seedTask(t, st)

	if _, err := st.Enqueue(ctx, taskID); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if _, err := st.Enqueue(ctx, taskID); err == nil {
		t.Fatal("second Enqueue succeeded, want unique-constraint failure")
	}
}

// TestReapStale frees a run stuck in running (simulating a crashed worker) and
// lets the task be enqueued again.
func TestReapStale(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	taskID := seedTask(t, st)

	if _, err := st.Enqueue(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNext(ctx, "w"); err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	// Task now has an active (running) run — enqueue must be blocked.
	if _, err := st.Enqueue(ctx, taskID); err == nil {
		t.Fatal("Enqueue during running succeeded, want blocked")
	}

	reaped, err := st.ReapStale(ctx)
	if err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	if len(reaped) != 1 {
		t.Fatalf("reaped = %d, want 1", len(reaped))
	}
	// After reaping, the task is free to schedule again.
	if _, err := st.Enqueue(ctx, taskID); err != nil {
		t.Fatalf("Enqueue after reap: %v", err)
	}
}
