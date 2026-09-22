package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sqlitedrv "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"

	"github.com/duskrun/duskrun/internal/core"
)

// ErrNoRun is returned by ClaimNext when the queue has nothing to hand out. It
// aliases core.ErrNoQueuedRun so the worker pool (in package core) can match it
// with errors.Is without importing this package.
var ErrNoRun = core.ErrNoQueuedRun

// ErrActiveRun is returned by Enqueue/Requeue when the partial unique index
// ux_run_active already has an active (queued/running) run for the task. It
// wraps core.ErrAlreadyActive so the dispatcher (in package core, which cannot
// import this package) can classify it with errors.Is without an import cycle.
var ErrActiveRun = core.ErrAlreadyActive

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure —
// the way the ux_run_active partial index surfaces a duplicate active run.
func isUniqueViolation(err error) bool {
	var se *sqlitedrv.Error
	if errors.As(err, &se) {
		c := se.Code()
		return c == sqlitelib.SQLITE_CONSTRAINT_UNIQUE || c == sqlitelib.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// Enqueue inserts a queued run for a task and returns its id. The partial unique
// index ux_run_active enforces "at most one active run per task": if a run for
// this task is already queued or running, the insert fails the constraint and
// this returns an error the scheduler treats as "already scheduled" (skip).
func (s *Store) Enqueue(ctx context.Context, taskID int64) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO run (task_id, status, attempt, created_at)
		 VALUES (?, 'queued', 1, unixepoch())`,
		taskID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("enqueue task %d: %w", taskID, ErrActiveRun)
		}
		return 0, fmt.Errorf("enqueue task %d: %w", taskID, err)
	}
	return res.LastInsertId()
}

// ClaimNext atomically claims the oldest queued run for worker and flips it to
// running. It uses a single UPDATE ... RETURNING with a subselect: the whole
// statement is atomic under the write lock (write pool is MaxOpenConns=1 +
// immediate tx), so two workers can never claim the same run — the loser simply
// updates zero rows and gets ErrNoRun. Returns ErrNoRun when the queue is empty.
func (s *Store) ClaimNext(ctx context.Context, worker string) (*core.Run, error) {
	row := s.write.QueryRowContext(ctx,
		`UPDATE run
		    SET status = 'running',
		        worker = ?,
		        started_at = unixepoch()
		  WHERE id = (
		        SELECT id FROM run
		         WHERE status = 'queued'
		         ORDER BY id
		         LIMIT 1
		  )
		RETURNING id, task_id, attempt, started_at, created_at`,
		worker,
	)
	var (
		r       core.Run
		started sql.NullInt64
		created int64
	)
	if err := row.Scan(&r.ID, &r.TaskID, &r.Attempt, &started, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoRun
		}
		return nil, fmt.Errorf("claim next: %w", err)
	}
	r.Status = core.StatusRunning
	r.Worker = worker
	r.CreatedAt = unixToTime(created)
	if t := nullTime(started); t != nil {
		r.StartedAt = t
	}
	return &r, nil
}

// Finish records a terminal status (success/failed/skipped) with its log/error.
func (s *Store) Finish(ctx context.Context, runID int64, status core.RunStatus, log, errMsg string) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE run
		    SET status = ?, finished_at = unixepoch(), log = ?, error = ?
		  WHERE id = ?`,
		string(status), log, errMsg, runID,
	)
	if err != nil {
		return fmt.Errorf("finish run %d: %w", runID, err)
	}
	return nil
}

// Requeue schedules a retry: it clears the active run's terminal fields and, if
// attempts remain, inserts a fresh queued run with attempt+1. Because the old
// run is already finished (failed), inserting the new queued row does not
// violate ux_run_active. Returns the new run id, or 0 if retries are exhausted.
func (s *Store) Requeue(ctx context.Context, taskID int64, nextAttempt, maxAttempts int) (int64, error) {
	if nextAttempt > maxAttempts {
		return 0, nil
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO run (task_id, status, attempt, created_at)
		 VALUES (?, 'queued', ?, unixepoch())`,
		taskID, nextAttempt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("requeue task %d: %w", taskID, ErrActiveRun)
		}
		return 0, fmt.Errorf("requeue task %d: %w", taskID, err)
	}
	return res.LastInsertId()
}

// ReapStale is called on startup: runs left in 'running' by a crashed process
// have no live worker. It marks them failed so ux_run_active is freed and the
// misfire policy can decide whether to re-run. Returns the reaped run ids.
func (s *Store) ReapStale(ctx context.Context) ([]int64, error) {
	rows, err := s.write.QueryContext(ctx,
		`UPDATE run
		    SET status = 'failed',
		        finished_at = unixepoch(),
		        error = 'reaped: worker gone (process restart)'
		  WHERE status = 'running'
		RETURNING id`,
	)
	if err != nil {
		return nil, fmt.Errorf("reap stale: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
