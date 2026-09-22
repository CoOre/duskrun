package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("sqlite: not found")

// GetTaskByName loads a task by its unique name.
func (s *Store) GetTaskByName(ctx context.Context, name string) (*core.Task, error) {
	return scanTask(s.read.QueryRowContext(ctx, taskSelect+` WHERE name = ?`, name))
}

// GetTask loads a task by id (used by the dispatcher/worker to resolve a run's
// task without a second name lookup).
func (s *Store) GetTask(ctx context.Context, id int64) (*core.Task, error) {
	return scanTask(s.read.QueryRowContext(ctx, taskSelect+` WHERE id = ?`, id))
}

// ListEnabledTasks returns every enabled task, ordered by id — the scheduler's
// working set. Disabled tasks are skipped so toggling `enabled` off pauses a
// task without deleting it.
func (s *Store) ListEnabledTasks(ctx context.Context) ([]core.Task, error) {
	rows, err := s.read.QueryContext(ctx, taskSelect+` WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list enabled tasks: %w", err)
	}
	defer rows.Close()
	var out []core.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

const taskSelect = `SELECT id, name, connection_id, dumper_opts, codec_chain, storage_id,
	cron, retention, notifiers, enabled, misfire, retries, timeout_sec, watchdog_sec, enabled_at, created_at FROM task`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scan routine
// serves single-row lookups and list iteration alike.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*core.Task, error) {
	var (
		t                                         core.Task
		dumperOpts, codecChain, retention, notifs string
		enabled                                   int
		misfire                                   string
		timeoutSec, watchdogSec                   int64
		enabledAt, created                        int64
	)
	err := row.Scan(&t.ID, &t.Name, &t.ConnectionID, &dumperOpts, &codecChain, &t.StorageID,
		&t.Cron, &retention, &notifs, &enabled, &misfire, &t.Retries, &timeoutSec, &watchdogSec,
		&enabledAt, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.DumperOpts = json.RawMessage(dumperOpts)
	if err := json.Unmarshal([]byte(codecChain), &t.CodecChain); err != nil {
		return nil, fmt.Errorf("task %q: codec_chain: %w", t.Name, err)
	}
	if err := json.Unmarshal([]byte(retention), &t.Retention); err != nil {
		return nil, fmt.Errorf("task %q: retention: %w", t.Name, err)
	}
	if err := json.Unmarshal([]byte(notifs), &t.Notifiers); err != nil {
		return nil, fmt.Errorf("task %q: notifiers: %w", t.Name, err)
	}
	t.Enabled = enabled != 0
	t.Misfire = core.MisfirePolicy(misfire)
	t.Timeout = time.Duration(timeoutSec) * time.Second
	t.Watchdog = time.Duration(watchdogSec) * time.Second
	t.EnabledAt = unixToTime(enabledAt)
	t.CreatedAt = unixToTime(created)
	return &t, nil
}

// ListTasks returns every task (enabled or not), newest id last — the API's
// task listing.
func (s *Store) ListTasks(ctx context.Context) ([]core.Task, error) {
	rows, err := s.read.QueryContext(ctx, taskSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []core.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

const runSelect = `SELECT id, task_id, status, worker, attempt, started_at, finished_at, error, log, created_at FROM run`

func scanRun(row rowScanner) (*core.Run, error) {
	var (
		r                 core.Run
		worker, status    string
		errMsg, logs      string
		started, finished sql.NullInt64
		created           int64
	)
	err := row.Scan(&r.ID, &r.TaskID, &status, &worker, &r.Attempt, &started, &finished, &errMsg, &logs, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan run: %w", err)
	}
	r.Status = core.RunStatus(status)
	r.Worker = worker
	r.Error = errMsg
	r.Log = logs
	r.StartedAt = nullTime(started)
	r.FinishedAt = nullTime(finished)
	r.CreatedAt = unixToTime(created)
	return &r, nil
}

// ListRuns returns runs filtered by task id (0 = any) and status ("" = any),
// newest first.
func (s *Store) ListRuns(ctx context.Context, taskID int64, status string) ([]core.Run, error) {
	q := runSelect + ` WHERE 1=1`
	var args []any
	if taskID != 0 {
		q += ` AND task_id = ?`
		args = append(args, taskID)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC`

	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []core.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListRunsPage returns one keyset-paginated run page, newest first. Cursor
// semantics follow the descending id order:
//   - beforeID loads older rows (id < beforeID)
//   - afterID loads newer rows (id > afterID)
//   - anchorID starts at a remembered row or the closest older row (id <= anchorID)
func (s *Store) ListRunsPage(ctx context.Context, taskID int64, status string, limit int, beforeID, afterID, anchorID int64) ([]core.Run, error) {
	q := runSelect + ` WHERE 1=1`
	var args []any
	if taskID != 0 {
		q += ` AND task_id = ?`
		args = append(args, taskID)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if beforeID != 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	if afterID != 0 {
		q += ` AND id > ?`
		args = append(args, afterID)
	}
	if anchorID != 0 {
		q += ` AND id <= ?`
		args = append(args, anchorID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs page: %w", err)
	}
	defer rows.Close()
	var out []core.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetRun loads one run (including its log) by id.
func (s *Store) GetRun(ctx context.Context, id int64) (*core.Run, error) {
	return scanRun(s.read.QueryRowContext(ctx, runSelect+` WHERE id = ?`, id))
}

// CreateConnection inserts a connection and returns its id.
func (s *Store) CreateConnection(ctx context.Context, c core.Connection) (int64, error) {
	cfg := string(c.ConnectorConfig)
	if cfg == "" {
		cfg = "{}"
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO connection (name, engine, connector_type, connector_config, username, secret_ref, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, unixepoch())`,
		c.Name, c.Engine, c.ConnectorType, cfg, c.Username, c.SecretRef,
	)
	if err != nil {
		return 0, fmt.Errorf("create connection %q: %w", c.Name, err)
	}
	return res.LastInsertId()
}

// UpdateConnection replaces an existing connection's editable fields.
func (s *Store) UpdateConnection(ctx context.Context, c core.Connection) error {
	cfg := string(c.ConnectorConfig)
	if cfg == "" {
		cfg = "{}"
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE connection
		    SET name = ?, engine = ?, connector_type = ?, connector_config = ?, username = ?, secret_ref = ?
		  WHERE id = ?`,
		c.Name, c.Engine, c.ConnectorType, cfg, c.Username, c.SecretRef, c.ID,
	)
	if err != nil {
		return fmt.Errorf("update connection %d: %w", c.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update connection %d: rows affected: %w", c.ID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateStorage inserts a storage and returns its id.
func (s *Store) CreateStorage(ctx context.Context, st core.Storage) (int64, error) {
	cfg := string(st.Config)
	if cfg == "" {
		cfg = "{}"
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO storage (name, type, config, secret_ref, created_at)
		 VALUES (?, ?, ?, ?, unixepoch())`,
		st.Name, st.Type, cfg, st.SecretRef,
	)
	if err != nil {
		return 0, fmt.Errorf("create storage %q: %w", st.Name, err)
	}
	return res.LastInsertId()
}

// UpdateStorage replaces an existing storage's editable fields.
func (s *Store) UpdateStorage(ctx context.Context, st core.Storage) error {
	cfg := string(st.Config)
	if cfg == "" {
		cfg = "{}"
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE storage
		    SET name = ?, type = ?, config = ?, secret_ref = ?
		  WHERE id = ?`,
		st.Name, st.Type, cfg, st.SecretRef, st.ID,
	)
	if err != nil {
		return fmt.Errorf("update storage %d: %w", st.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update storage %d: rows affected: %w", st.ID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateTask inserts a task and returns its id. JSON columns default sanely when
// the caller leaves them zero.
func (s *Store) CreateTask(ctx context.Context, t core.Task) (int64, error) {
	dumperOpts := string(t.DumperOpts)
	if dumperOpts == "" {
		dumperOpts = "{}"
	}
	codecChain, err := json.Marshal(t.CodecChain)
	if err != nil {
		return 0, fmt.Errorf("marshal codec_chain: %w", err)
	}
	retention, err := json.Marshal(t.Retention)
	if err != nil {
		return 0, fmt.Errorf("marshal retention: %w", err)
	}
	notifiers, err := json.Marshal(t.Notifiers)
	if err != nil {
		return 0, fmt.Errorf("marshal notifiers: %w", err)
	}
	misfire := t.Misfire
	if misfire == "" {
		misfire = core.MisfireRunOnceNow
	}
	timeoutSec := int64(t.Timeout / time.Second)
	if timeoutSec == 0 {
		timeoutSec = 1800
	}
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO task (name, connection_id, storage_id, dumper_opts, codec_chain, cron,
			retention, notifiers, enabled, misfire, retries, timeout_sec, watchdog_sec,
			enabled_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch(), unixepoch())`,
		t.Name, t.ConnectionID, t.StorageID, dumperOpts, string(codecChain), t.Cron,
		string(retention), string(notifiers), enabled, string(misfire), t.Retries, timeoutSec,
		int64(t.Watchdog/time.Second),
	)
	if err != nil {
		return 0, fmt.Errorf("create task %q: %w", t.Name, err)
	}
	return res.LastInsertId()
}

// UpdateTask replaces an existing task's editable fields.
func (s *Store) UpdateTask(ctx context.Context, t core.Task) error {
	dumperOpts := string(t.DumperOpts)
	if dumperOpts == "" {
		dumperOpts = "{}"
	}
	codecChain, err := json.Marshal(t.CodecChain)
	if err != nil {
		return fmt.Errorf("marshal codec_chain: %w", err)
	}
	retention, err := json.Marshal(t.Retention)
	if err != nil {
		return fmt.Errorf("marshal retention: %w", err)
	}
	notifiers, err := json.Marshal(t.Notifiers)
	if err != nil {
		return fmt.Errorf("marshal notifiers: %w", err)
	}
	misfire := t.Misfire
	if misfire == "" {
		misfire = core.MisfireRunOnceNow
	}
	timeoutSec := int64(t.Timeout / time.Second)
	if timeoutSec == 0 {
		timeoutSec = 1800
	}
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	res, err := s.write.ExecContext(ctx,
		`UPDATE task
		    SET name = ?, connection_id = ?, storage_id = ?, dumper_opts = ?, codec_chain = ?,
		        cron = ?, retention = ?, notifiers = ?, enabled = ?, misfire = ?,
		        retries = ?, timeout_sec = ?, watchdog_sec = ?,
		        -- Stamped only on an off-to-on transition. The enabled column read
		        -- here is the row's PRE-update value (SQL evaluates every SET
		        -- expression against the original row), so editing an already
		        -- enabled task keeps its original window.
		        enabled_at = CASE WHEN enabled = 0 AND ? = 1 THEN unixepoch() ELSE enabled_at END
		  WHERE id = ?`,
		t.Name, t.ConnectionID, t.StorageID, dumperOpts, string(codecChain), t.Cron,
		string(retention), string(notifiers), enabled, string(misfire), t.Retries, timeoutSec,
		int64(t.Watchdog/time.Second), enabled, t.ID,
	)
	if err != nil {
		return fmt.Errorf("update task %d: %w", t.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update task %d: rows affected: %w", t.ID, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetConnection loads a connection by id.
func (s *Store) GetConnection(ctx context.Context, id int64) (*core.Connection, error) {
	var (
		c       core.Connection
		cfg     string
		created int64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, engine, connector_type, connector_config, username, secret_ref, created_at
		   FROM connection WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Engine, &c.ConnectorType, &cfg, &c.Username, &c.SecretRef, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan connection: %w", err)
	}
	c.ConnectorConfig = json.RawMessage(cfg)
	c.CreatedAt = unixToTime(created)
	return &c, nil
}

// GetStorage loads a storage by id.
func (s *Store) GetStorage(ctx context.Context, id int64) (*core.Storage, error) {
	var (
		st      core.Storage
		cfg     string
		created int64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, type, config, secret_ref, created_at FROM storage WHERE id = ?`, id,
	).Scan(&st.ID, &st.Name, &st.Type, &cfg, &st.SecretRef, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan storage: %w", err)
	}
	st.Config = json.RawMessage(cfg)
	st.CreatedAt = unixToTime(created)
	return &st, nil
}

// ListConnections returns all connections ordered by id — the UI's picker set.
func (s *Store) ListConnections(ctx context.Context) ([]core.Connection, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, engine, connector_type, connector_config, username, secret_ref, created_at
		   FROM connection ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()
	var out []core.Connection
	for rows.Next() {
		var (
			c       core.Connection
			cfg     string
			created int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Engine, &c.ConnectorType, &cfg, &c.Username, &c.SecretRef, &created); err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		c.ConnectorConfig = json.RawMessage(cfg)
		c.CreatedAt = unixToTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListStorages returns all storages ordered by id — the UI's picker set.
func (s *Store) ListStorages(ctx context.Context) ([]core.Storage, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, type, config, secret_ref, created_at FROM storage ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list storages: %w", err)
	}
	defer rows.Close()
	var out []core.Storage
	for rows.Next() {
		var (
			st      core.Storage
			cfg     string
			created int64
		)
		if err := rows.Scan(&st.ID, &st.Name, &st.Type, &cfg, &st.SecretRef, &created); err != nil {
			return nil, fmt.Errorf("scan storage: %w", err)
		}
		st.Config = json.RawMessage(cfg)
		st.CreatedAt = unixToTime(created)
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetSecretByRef loads a secret by a "secret://name" reference (or bare name).
func (s *Store) GetSecretByRef(ctx context.Context, ref string) (*core.Secret, error) {
	name := strings.TrimPrefix(ref, "secret://")
	var (
		sec      core.Secret
		created  int64
		lastUsed sql.NullInt64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, type, ciphertext, key_id, created_at, last_used_at
		   FROM secret WHERE name = ?`, name,
	).Scan(&sec.ID, &sec.Name, &sec.Type, &sec.Ciphertext, &sec.KeyID, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan secret: %w", err)
	}
	sec.CreatedAt = unixToTime(created)
	sec.LastUsedAt = nullTime(lastUsed)
	return &sec, nil
}

// GetSecretByID loads a secret's metadata by id (name, type, key, timestamps).
// Ciphertext is not selected — the API never exposes it; this exists so the
// delete path can resolve an id to its name for the reference check.
func (s *Store) GetSecretByID(ctx context.Context, id int64) (*core.Secret, error) {
	var (
		sec      core.Secret
		created  int64
		lastUsed sql.NullInt64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, type, key_id, created_at, last_used_at
		   FROM secret WHERE id = ?`, id,
	).Scan(&sec.ID, &sec.Name, &sec.Type, &sec.KeyID, &created, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get secret %d: %w", id, err)
	}
	sec.CreatedAt = unixToTime(created)
	sec.LastUsedAt = nullTime(lastUsed)
	return &sec, nil
}

// ListSecrets returns secret metadata (name, type, key, timestamps) ordered by
// name. Ciphertext is deliberately not selected — the API never exposes it.
func (s *Store) ListSecrets(ctx context.Context) ([]core.Secret, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, type, key_id, created_at, last_used_at
		   FROM secret ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	var out []core.Secret
	for rows.Next() {
		var (
			sec      core.Secret
			created  int64
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&sec.ID, &sec.Name, &sec.Type, &sec.KeyID, &created, &lastUsed); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		sec.CreatedAt = unixToTime(created)
		sec.LastUsedAt = nullTime(lastUsed)
		out = append(out, sec)
	}
	return out, rows.Err()
}

// PutSecret upserts a sealed secret by name.
func (s *Store) PutSecret(ctx context.Context, sec core.Secret) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO secret (name, type, ciphertext, key_id, created_at)
		 VALUES (?, ?, ?, ?, unixepoch())
		 ON CONFLICT(name) DO UPDATE SET
		   type = excluded.type, ciphertext = excluded.ciphertext, key_id = excluded.key_id`,
		sec.Name, sec.Type, sec.Ciphertext, sec.KeyID,
	)
	if err != nil {
		return 0, fmt.Errorf("put secret %q: %w", sec.Name, err)
	}
	return res.LastInsertId()
}

// StartManualRun records a run started outside the scheduler (TZ §7 "Run now").
// It inserts directly as running; ux_run_active still forbids a second active
// run for the task.
func (s *Store) StartManualRun(ctx context.Context, taskID int64) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO run (task_id, status, attempt, started_at, created_at)
		 VALUES (?, 'running', 1, unixepoch(), unixepoch())`,
		taskID,
	)
	if err != nil {
		return 0, fmt.Errorf("start manual run: %w", err)
	}
	return res.LastInsertId()
}

// ListArtifactsByTask returns all artifacts produced by a task's runs, newest
// first — the working set for the Retention Manager.
func (s *Store) ListArtifactsByTask(ctx context.Context, taskID int64) ([]core.Artifact, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT a.id, a.run_id, a.storage_id, a.key, a.size, a.checksum, a.created_at, a.expires_hint
		   FROM artifact a
		   JOIN run r ON r.id = a.run_id
		  WHERE r.task_id = ?
		  ORDER BY a.created_at DESC, a.id DESC`, taskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list artifacts by task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []core.Artifact
	for rows.Next() {
		var (
			a       core.Artifact
			created int64
			expires sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &a.RunID, &a.StorageID, &a.Key, &a.Size, &a.Checksum, &created, &expires); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		a.CreatedAt = unixToTime(created)
		a.ExpiresHint = nullTime(expires)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LastSuccessfulArtifactSize returns the artifact size of the most recent
// successful run for the task, or 0 if there is none yet. It is used as the
// estimated total for a new run's live progress bar — a compressed-vs-compressed
// comparison (same codec chain), so it is a meaningful ballpark rather than an
// exact figure.
func (s *Store) LastSuccessfulArtifactSize(ctx context.Context, taskID int64) (int64, error) {
	var size int64
	err := s.read.QueryRowContext(ctx,
		`SELECT a.size
		   FROM artifact a
		   JOIN run r ON r.id = a.run_id
		  WHERE r.task_id = ? AND r.status = 'success'
		  ORDER BY r.id DESC
		  LIMIT 1`, taskID,
	).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("last artifact size for task %d: %w", taskID, err)
	}
	return size, nil
}

// DeleteArtifact removes an artifact row from the catalog (after the object is
// removed from storage by the Retention Manager).
func (s *Store) DeleteArtifact(ctx context.Context, id int64) error {
	if _, err := s.write.ExecContext(ctx, `DELETE FROM artifact WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete artifact %d: %w", id, err)
	}
	return nil
}

// GetArtifact loads one artifact by id (for download).
func (s *Store) GetArtifact(ctx context.Context, id int64) (*core.Artifact, error) {
	var (
		a       core.Artifact
		created int64
		expires sql.NullInt64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, run_id, storage_id, key, size, checksum, created_at, expires_hint
		   FROM artifact WHERE id = ?`, id,
	).Scan(&a.ID, &a.RunID, &a.StorageID, &a.Key, &a.Size, &a.Checksum, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	a.CreatedAt = unixToTime(created)
	a.ExpiresHint = nullTime(expires)
	return &a, nil
}

// GetArtifactByRun loads the newest artifact produced by a run.
func (s *Store) GetArtifactByRun(ctx context.Context, runID int64) (*core.Artifact, error) {
	var (
		a       core.Artifact
		created int64
		expires sql.NullInt64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT id, run_id, storage_id, key, size, checksum, created_at, expires_hint
		   FROM artifact
		  WHERE run_id = ?
		  ORDER BY id DESC
		  LIMIT 1`, runID,
	).Scan(&a.ID, &a.RunID, &a.StorageID, &a.Key, &a.Size, &a.Checksum, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan artifact by run: %w", err)
	}
	a.CreatedAt = unixToTime(created)
	a.ExpiresHint = nullTime(expires)
	return &a, nil
}

// InsertArtifact records a produced artifact in the catalog.
func (s *Store) InsertArtifact(ctx context.Context, a core.Artifact) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO artifact (run_id, storage_id, key, size, checksum, created_at)
		 VALUES (?, ?, ?, ?, ?, unixepoch())`,
		a.RunID, a.StorageID, a.Key, a.Size, a.Checksum,
	)
	if err != nil {
		return 0, fmt.Errorf("insert artifact: %w", err)
	}
	return res.LastInsertId()
}
