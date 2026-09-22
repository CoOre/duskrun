package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// LastSuccessByTask returns, per task, when its most recent successful run
// finished. Tasks that never succeeded are absent from the map — the caller
// distinguishes "never worked" from "worked a while ago".
//
// finished_at can be NULL on an old row, so it falls back to created_at rather
// than dropping the run and reporting the task as never-successful.
func (s *Store) LastSuccessByTask(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT task_id, MAX(COALESCE(finished_at, created_at))
		   FROM run
		  WHERE status = 'success'
		  GROUP BY task_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("last success by task: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]time.Time)
	for rows.Next() {
		var (
			taskID int64
			at     int64
		)
		if err := rows.Scan(&taskID, &at); err != nil {
			return nil, fmt.Errorf("scan last success: %w", err)
		}
		out[taskID] = unixToTime(at)
	}
	return out, rows.Err()
}

// ListWatchdogAlerts returns the tasks currently in a reported stale episode.
func (s *Store) ListWatchdogAlerts(ctx context.Context) (map[int64]core.WatchdogRecord, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT task_id, alerted_at, last_success FROM watchdog_alert`)
	if err != nil {
		return nil, fmt.Errorf("list watchdog alerts: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]core.WatchdogRecord)
	for rows.Next() {
		var (
			rec         core.WatchdogRecord
			alertedAt   int64
			lastSuccess sql.NullInt64
		)
		if err := rows.Scan(&rec.TaskID, &alertedAt, &lastSuccess); err != nil {
			return nil, fmt.Errorf("scan watchdog alert: %w", err)
		}
		rec.AlertedAt = unixToTime(alertedAt)
		rec.LastSuccess = nullTime(lastSuccess)
		out[rec.TaskID] = rec
	}
	return out, rows.Err()
}

// MarkWatchdogAlerted records that a task's stale episode has been reported.
// Upsert, so a re-report of the same task overwrites rather than failing.
func (s *Store) MarkWatchdogAlerted(ctx context.Context, rec core.WatchdogRecord) error {
	var lastSuccess any
	if rec.LastSuccess != nil {
		lastSuccess = rec.LastSuccess.Unix()
	}
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO watchdog_alert (task_id, alerted_at, last_success)
		 VALUES (?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET alerted_at = excluded.alerted_at,
		                                    last_success = excluded.last_success`,
		rec.TaskID, rec.AlertedAt.Unix(), lastSuccess,
	)
	if err != nil {
		return fmt.Errorf("mark watchdog alert for task %d: %w", rec.TaskID, err)
	}
	return nil
}

// ClearWatchdogAlert closes a task's stale episode.
func (s *Store) ClearWatchdogAlert(ctx context.Context, taskID int64) error {
	if _, err := s.write.ExecContext(ctx,
		`DELETE FROM watchdog_alert WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("clear watchdog alert for task %d: %w", taskID, err)
	}
	return nil
}
