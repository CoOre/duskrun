package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OpenAlert records that the (kind, subject) episode is open at level and
// returns the level it had before; 0 means it was closed. Callers alert when
// the level went up, so the read and the write share one transaction.
func (s *Store) OpenAlert(ctx context.Context, kind, subject string, level int, at time.Time) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("open alert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prev int
	err = tx.QueryRowContext(ctx,
		`SELECT level FROM alert_state WHERE kind = ? AND subject = ?`, kind, subject,
	).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("open alert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO alert_state (kind, subject, level, opened_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (kind, subject) DO UPDATE SET level = excluded.level`,
		kind, subject, level, at.Unix(),
	); err != nil {
		return 0, fmt.Errorf("open alert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("open alert: %w", err)
	}
	return prev, nil
}

// CloseAlert ends the (kind, subject) episode and reports whether one was open.
func (s *Store) CloseAlert(ctx context.Context, kind, subject string) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`DELETE FROM alert_state WHERE kind = ? AND subject = ?`, kind, subject)
	if err != nil {
		return false, fmt.Errorf("close alert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("close alert: %w", err)
	}
	return n > 0, nil
}

// AlertLevel returns the open level of the (kind, subject) episode, 0 if none.
func (s *Store) AlertLevel(ctx context.Context, kind, subject string) (int, error) {
	var level int
	err := s.read.QueryRowContext(ctx,
		`SELECT level FROM alert_state WHERE kind = ? AND subject = ?`, kind, subject,
	).Scan(&level)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("alert level: %w", err)
	}
	return level, nil
}

// RecentArtifactSizes returns the artifact sizes of the task's last n
// successful runs before beforeRunID, newest first. It is the history a new
// backup's size is judged against.
func (s *Store) RecentArtifactSizes(ctx context.Context, taskID, beforeRunID int64, n int) ([]int64, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT a.size
		   FROM artifact a
		   JOIN run r ON r.id = a.run_id
		  WHERE r.task_id = ? AND r.status = 'success' AND r.id < ?
		  ORDER BY r.id DESC
		  LIMIT ?`, taskID, beforeRunID, n)
	if err != nil {
		return nil, fmt.Errorf("recent artifact sizes: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var size int64
		if err := rows.Scan(&size); err != nil {
			return nil, err
		}
		out = append(out, size)
	}
	return out, rows.Err()
}

// RecentRunDurations returns how long the task's last n successful runs took,
// newest first.
func (s *Store) RecentRunDurations(ctx context.Context, taskID int64, n int) ([]time.Duration, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT finished_at - started_at
		   FROM run
		  WHERE task_id = ? AND status = 'success'
		    AND started_at IS NOT NULL AND finished_at IS NOT NULL
		  ORDER BY id DESC
		  LIMIT ?`, taskID, n)
	if err != nil {
		return nil, fmt.Errorf("recent run durations: %w", err)
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var sec int64
		if err := rows.Scan(&sec); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(sec)*time.Second)
	}
	return out, rows.Err()
}

// TouchKnownDevice records that the user signed in from fingerprint. It
// reports whether the fingerprint is new, and whether the user had any known
// device before — a user's very first login is new by definition and not
// worth an alert.
func (s *Store) TouchKnownDevice(ctx context.Context, userID int64, fingerprint, label string, at time.Time) (isNew, hadAny bool, err error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, false, fmt.Errorf("touch known device: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count, match int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(fingerprint = ?), 0) FROM known_device WHERE user_id = ?`,
		fingerprint, userID,
	).Scan(&count, &match); err != nil {
		return false, false, fmt.Errorf("touch known device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO known_device (user_id, fingerprint, label, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, fingerprint) DO UPDATE SET last_seen = excluded.last_seen, label = excluded.label`,
		userID, fingerprint, label, at.Unix(), at.Unix(),
	); err != nil {
		return false, false, fmt.Errorf("touch known device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("touch known device: %w", err)
	}
	return match == 0, count > 0, nil
}

// PruneKnownDevices forgets devices not seen since before: a laptop unused for
// half a year signing in again is worth hearing about.
func (s *Store) PruneKnownDevices(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM known_device WHERE last_seen < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune known devices: %w", err)
	}
	return res.RowsAffected()
}
