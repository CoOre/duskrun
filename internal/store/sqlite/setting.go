package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/duskrun/duskrun/internal/core"
)

// ListSettings returns every stored setting, keyed order.
func (s *Store) ListSettings(ctx context.Context) ([]core.Setting, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT key, value, updated_at FROM setting ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()
	out := make([]core.Setting, 0, 8)
	for rows.Next() {
		var (
			st      core.Setting
			updated int64
		)
		if err := rows.Scan(&st.Key, &st.Value, &updated); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		st.UpdatedAt = unixToTime(updated)
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetSetting reads one key. A missing key returns def, not an error: callers
// want "the value or the default", and every setting has a sensible default.
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM setting WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return def, nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return v, nil
}

// PutSettings upserts keys in one transaction, so a multi-field save from the
// settings page cannot land half-applied.
func (s *Store) PutSettings(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO setting (key, value, updated_at) VALUES (?, ?, unixepoch())
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v,
		); err != nil {
			return fmt.Errorf("put setting %q: %w", k, err)
		}
	}
	return tx.Commit()
}
