package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sqlitedrv "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

// ErrInUse is returned by the Delete* methods when another entity still depends
// on the row: a task referencing a connection/storage (enforced by an ON DELETE
// RESTRICT foreign key), a task with an in-flight run, or a secret referenced by
// a connection/storage. The API layer maps it to 409 Conflict.
var ErrInUse = errors.New("sqlite: in use")

// isForeignKeyViolation reports whether err is a SQLite constraint failure from
// a referenced parent being deleted. An ON DELETE RESTRICT foreign key surfaces
// under modernc as SQLITE_CONSTRAINT_TRIGGER (the RESTRICT action fires like a
// trigger), while a plain FK check reports SQLITE_CONSTRAINT_FOREIGNKEY — accept
// either so both spellings map to ErrInUse.
func isForeignKeyViolation(err error) bool {
	var se *sqlitedrv.Error
	if errors.As(err, &se) {
		c := se.Code()
		return c == sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY || c == sqlitelib.SQLITE_CONSTRAINT_TRIGGER
	}
	return false
}

// DeleteConnection removes a connection by id. A connection referenced by any
// task is protected by an ON DELETE RESTRICT foreign key, so the delete fails
// with ErrInUse rather than orphaning tasks.
func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	return s.deleteGuarded(ctx, "connection", id)
}

// DeleteStorage removes a storage by id. Tasks and artifacts reference storages
// via ON DELETE RESTRICT foreign keys, so a storage still in use yields ErrInUse.
func (s *Store) DeleteStorage(ctx context.Context, id int64) error {
	return s.deleteGuarded(ctx, "storage", id)
}

// deleteGuarded deletes one row from table by id, translating a foreign-key
// RESTRICT failure into ErrInUse and a no-op delete into ErrNotFound.
func (s *Store) deleteGuarded(ctx context.Context, table string, id int64) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("delete %s %d: %w", table, id, ErrInUse)
		}
		return fmt.Errorf("delete %s %d: %w", table, id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete %s %d: rows affected: %w", table, id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTask removes a task by id. Its run history and artifact catalog rows are
// removed by ON DELETE CASCADE; the physical backup objects in storage are left
// for the Retention Manager (deleting them here would need every artifact's
// storage handle). A task with a queued or running run is refused with ErrInUse
// so a dump in flight is never yanked out from under the worker.
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	var active int
	if err := s.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run WHERE task_id = ? AND status IN ('queued', 'running')`, id,
	).Scan(&active); err != nil {
		return fmt.Errorf("delete task %d: check active run: %w", id, err)
	}
	if active > 0 {
		return fmt.Errorf("delete task %d: %w", id, ErrInUse)
	}
	res, err := s.write.ExecContext(ctx, `DELETE FROM task WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete task %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete task %d: rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSecret removes a secret by id. Secret references are plain text (there is
// no foreign key), so callers should consult SecretRefs first to refuse deleting
// a secret still in use; this method itself only enforces existence.
func (s *Store) DeleteSecret(ctx context.Context, id int64) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM secret WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete secret %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete secret %d: rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SecretRefs returns human labels for every connection, storage or notifier
// channel that still references the named secret. A secret is referenced by a
// connection's or storage's secret_ref, and by any `<key>_ref` anywhere in a
// plugin config — the same convention core.SecretResolver resolves, so what
// counts as "in use" here matches exactly what would break if the secret went
// away. Each ref may be written "secret://name" or as the bare name. An empty
// result means the secret is safe to delete.
func (s *Store) SecretRefs(ctx context.Context, name string) ([]string, error) {
	conns, err := s.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	storages, err := s.ListStorages(ctx)
	if err != nil {
		return nil, err
	}
	channels, err := s.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, c := range conns {
		if refName(c.SecretRef) == name || hasRef(c.ConnectorConfig, name) {
			refs = append(refs, fmt.Sprintf("соединение %q", c.Name))
		}
	}
	for _, st := range storages {
		if refName(st.SecretRef) == name || hasRef(st.Config, name) {
			refs = append(refs, fmt.Sprintf("хранилище %q", st.Name))
		}
	}
	for _, ch := range channels {
		if hasRef(ch.Config, name) {
			refs = append(refs, fmt.Sprintf("канал уведомлений %q", ch.Name))
		}
	}
	return refs, nil
}

// refName normalises a "secret://name" (or bare "name") reference to its name.
func refName(ref string) string {
	return strings.TrimSpace(strings.TrimPrefix(ref, "secret://"))
}

// hasRef reports whether a config blob references the named secret under any
// `<key>_ref`, at any depth.
func hasRef(raw json.RawMessage, name string) bool {
	for _, ref := range configRefs(raw) {
		if ref == name {
			return true
		}
	}
	return false
}

// configRefs collects every "<key>_ref" value in a config blob, walking nested
// objects and arrays. A plugin config is opaque JSON — token_ref for telegram,
// access_key_ref for s3, private_key_ref for sftp, a grouped fetch block for a
// staged dumper — so references cannot be read under a fixed key.
func configRefs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	var out []string
	collectRefs(v, &out)
	return out
}

func collectRefs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			if strings.HasSuffix(key, "_ref") {
				if s, _ := val.(string); s != "" {
					*out = append(*out, refName(s))
					continue
				}
			}
			collectRefs(val, out)
		}
	case []any:
		for _, item := range t {
			collectRefs(item, out)
		}
	}
}
