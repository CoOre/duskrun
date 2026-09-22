package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/duskrun/duskrun/internal/core"
)

const notifierSelect = `SELECT id, name, type, config, events, enabled, created_at FROM notifier`

func scanNotifier(row rowScanner) (*core.NotifierChannel, error) {
	var (
		c              core.NotifierChannel
		config, events string
		enabled        int
		created        int64
	)
	err := row.Scan(&c.ID, &c.Name, &c.Type, &config, &events, &enabled, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan notifier: %w", err)
	}
	c.Config = json.RawMessage(config)
	if err := json.Unmarshal([]byte(events), &c.Events); err != nil {
		return nil, fmt.Errorf("notifier %q: events: %w", c.Name, err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = unixToTime(created)
	return &c, nil
}

// ListNotifiers returns every configured channel, oldest first.
func (s *Store) ListNotifiers(ctx context.Context) ([]core.NotifierChannel, error) {
	rows, err := s.read.QueryContext(ctx, notifierSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list notifiers: %w", err)
	}
	defer rows.Close()
	var out []core.NotifierChannel
	for rows.Next() {
		c, err := scanNotifier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// GetNotifier loads one channel by id.
func (s *Store) GetNotifier(ctx context.Context, id int64) (*core.NotifierChannel, error) {
	return scanNotifier(s.read.QueryRowContext(ctx, notifierSelect+` WHERE id = ?`, id))
}

// CreateNotifier inserts a channel and returns its id.
func (s *Store) CreateNotifier(ctx context.Context, c core.NotifierChannel) (int64, error) {
	config, events, err := notifierJSON(c)
	if err != nil {
		return 0, err
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO notifier (name, type, config, events, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, unixepoch())`,
		c.Name, c.Type, config, events, boolToInt(c.Enabled),
	)
	if err != nil {
		return 0, fmt.Errorf("create notifier %q: %w", c.Name, err)
	}
	return res.LastInsertId()
}

// UpdateNotifier replaces a channel's editable fields.
//
// A rename cascades into task.notifiers, which references channels by name.
// Without it the rename would succeed while every task pointing at the old name
// silently stopped notifying — visible only as a warn line in the daemon log.
// Both statements share one transaction so a half-applied rename cannot strand
// those references.
func (s *Store) UpdateNotifier(ctx context.Context, c core.NotifierChannel) error {
	config, events, err := notifierJSON(c)
	if err != nil {
		return err
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update notifier %d: begin: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var oldName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM notifier WHERE id = ?`, c.ID).Scan(&oldName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("update notifier %d: %w", c.ID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE notifier SET name = ?, type = ?, config = ?, events = ?, enabled = ? WHERE id = ?`,
		c.Name, c.Type, config, events, boolToInt(c.Enabled), c.ID,
	); err != nil {
		return fmt.Errorf("update notifier %d: %w", c.ID, err)
	}

	if oldName != c.Name {
		// Rewrite the JSON array element-wise so only an exact name matches —
		// a string replace would also hit channels whose names overlap.
		if _, err := tx.ExecContext(ctx,
			`UPDATE task
			    SET notifiers = (SELECT json_group_array(CASE WHEN value = ? THEN ? ELSE value END)
			                       FROM json_each(task.notifiers))
			  WHERE EXISTS (SELECT 1 FROM json_each(task.notifiers) WHERE value = ?)`,
			oldName, c.Name, oldName,
		); err != nil {
			return fmt.Errorf("update notifier %d: cascade rename: %w", c.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update notifier %d: commit: %w", c.ID, err)
	}
	return nil
}

// DeleteNotifier removes a channel. It refuses while a task still references it
// by name: a silently missing channel is a task that stops notifying without
// anyone noticing.
func (s *Store) DeleteNotifier(ctx context.Context, id int64) error {
	ch, err := s.GetNotifier(ctx, id)
	if err != nil {
		return err
	}
	users, err := s.NotifierUsers(ctx, ch.Name)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: used by %v", ErrInUse, users)
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM notifier WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete notifier %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete notifier %d: rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// NotifierUsers lists the tasks whose notifiers array contains name. The array
// is JSON, so the match goes through json_each rather than a LIKE that would
// also hit substrings of other channel names.
func (s *Store) NotifierUsers(ctx context.Context, name string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT t.name FROM task t, json_each(t.notifiers) j
		  WHERE j.value = ? ORDER BY t.name`, name)
	if err != nil {
		return nil, fmt.Errorf("notifier users for %q: %w", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var taskName string
		if err := rows.Scan(&taskName); err != nil {
			return nil, fmt.Errorf("scan notifier user: %w", err)
		}
		out = append(out, taskName)
	}
	return out, rows.Err()
}

// InsertNotification appends a delivery attempt to the log.
func (s *Store) InsertNotification(ctx context.Context, n core.Notification) (int64, error) {
	var runID any
	if n.RunID != nil {
		runID = *n.RunID
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO notification (kind, task, run_id, channel, status, error, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.Kind, n.Task, runID, n.Channel, string(n.Status), n.Error, n.CreatedAt.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert notification: %w", err)
	}
	return res.LastInsertId()
}

// ListNotifications returns the most recent delivery attempts, newest first.
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]core.Notification, error) {
	if limit <= 0 {
		limit = defaultNotificationLimit
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, kind, task, run_id, channel, status, error, created_at
		   FROM notification ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	var out []core.Notification
	for rows.Next() {
		var (
			n       core.Notification
			runID   sql.NullInt64
			status  string
			created int64
		)
		if err := rows.Scan(&n.ID, &n.Kind, &n.Task, &runID, &n.Channel, &status, &n.Error, &created); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		if runID.Valid {
			v := runID.Int64
			n.RunID = &v
		}
		n.Status = core.NotificationStatus(status)
		n.CreatedAt = unixToTime(created)
		out = append(out, n)
	}
	return out, rows.Err()
}

// PruneNotifications trims the delivery log to its newest keep entries and
// returns how many rows went. Nothing else deletes from it: one row per event
// per channel, successes included, is ~200k rows a year for a single
// five-minute task, and the database file is the operator's disk.
func (s *Store) PruneNotifications(ctx context.Context, keep int) (int64, error) {
	if keep <= 0 {
		keep = defaultNotificationKeep
	}
	res, err := s.write.ExecContext(ctx,
		`DELETE FROM notification
		  WHERE id NOT IN (SELECT id FROM notification ORDER BY created_at DESC, id DESC LIMIT ?)`,
		keep)
	if err != nil {
		return 0, fmt.Errorf("prune notifications: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune notifications: rows affected: %w", err)
	}
	return n, nil
}

// defaultNotificationLimit bounds an unqualified delivery-log listing.
const defaultNotificationLimit = 50

// defaultNotificationKeep bounds the log when a caller passes no size.
const defaultNotificationKeep = 5000

func notifierJSON(c core.NotifierChannel) (config, events string, err error) {
	config = string(c.Config)
	if config == "" {
		config = "{}"
	}
	kinds := c.Events
	if kinds == nil {
		kinds = []string{}
	}
	raw, err := json.Marshal(kinds)
	if err != nil {
		return "", "", fmt.Errorf("marshal notifier events: %w", err)
	}
	return config, string(raw), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
