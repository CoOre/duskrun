package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// ErrLastAdmin rejects a change that would leave the instance with no enabled
// administrator. Recovering from that state needs DUSKRUN_API_TOKEN, which the
// deployment is allowed not to set — so the store refuses instead.
var ErrLastAdmin = errors.New("sqlite: last enabled admin")

// ErrSelfTarget rejects an admin disabling, deleting or demoting themselves.
var ErrSelfTarget = errors.New("sqlite: cannot target own account")

const userSelect = `SELECT id, email, name, role, password_hash, disabled, created_at, last_login_at FROM app_user`

func scanUser(row rowScanner) (*core.User, error) {
	var (
		u        core.User
		role     string
		disabled int
		created  int64
		login    sql.NullInt64
	)
	err := row.Scan(&u.ID, &u.Email, &u.Name, &role, &u.PasswordHash, &disabled, &created, &login)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.Role = core.Role(role)
	u.Disabled = disabled != 0
	u.CreatedAt = unixToTime(created)
	u.LastLoginAt = nullTime(login)
	return &u, nil
}

// ListUsers returns every account, oldest first.
func (s *Store) ListUsers(ctx context.Context) ([]core.User, error) {
	rows, err := s.read.QueryContext(ctx, userSelect+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := make([]core.User, 0, 8)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// GetUser loads one account by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*core.User, error) {
	return scanUser(s.read.QueryRowContext(ctx, userSelect+` WHERE id = ?`, id))
}

// GetUserByEmail loads one account by login. Email matching is case-insensitive:
// addresses are stored lowercased by CreateUser, and callers normalise too.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*core.User, error) {
	return scanUser(s.read.QueryRowContext(ctx, userSelect+` WHERE email = ?`, normalizeEmail(email)))
}

// CountUsers reports how many accounts exist. Zero means the instance is still
// on the bootstrap path, where DUSKRUN_API_TOKEN is the only way in.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_user`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts an account and returns its id.
func (s *Store) CreateUser(ctx context.Context, u core.User) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO app_user (email, name, role, password_hash, disabled, created_at)
		 VALUES (?, ?, ?, ?, ?, unixepoch())`,
		normalizeEmail(u.Email), u.Name, string(u.Role), u.PasswordHash, boolToInt(u.Disabled),
	)
	if err != nil {
		return 0, fmt.Errorf("create user %q: %w", u.Email, err)
	}
	return res.LastInsertId()
}

// UpdateUser replaces an account's editable fields (name, role, disabled).
//
// The write happens inside a transaction that first re-counts enabled admins,
// so two concurrent demotions cannot both observe "there is another admin" and
// between them leave none. Disabling also drops the account's sessions: §5.2
// requires access to end immediately, and leaving rows behind would let an open
// client keep working until its next revalidation.
func (s *Store) UpdateUser(ctx context.Context, u core.User) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update user %d: begin: %w", u.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id = ?`, u.ID))
	if err != nil {
		return err
	}

	losingAdmin := cur.Role == core.RoleAdmin && !cur.Disabled &&
		(u.Role != core.RoleAdmin || u.Disabled)
	if losingAdmin {
		if err := ensureNotLastAdmin(ctx, tx, u.ID); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE app_user SET name = ?, role = ?, disabled = ? WHERE id = ?`,
		u.Name, string(u.Role), boolToInt(u.Disabled), u.ID,
	); err != nil {
		return fmt.Errorf("update user %d: %w", u.ID, err)
	}

	if u.Disabled && !cur.Disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session WHERE user_id = ?`, u.ID); err != nil {
			return fmt.Errorf("update user %d: drop sessions: %w", u.ID, err)
		}
	}
	return tx.Commit()
}

// DeleteUser removes an account and, by cascade, its sessions.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete user %d: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id = ?`, id))
	if err != nil {
		return err
	}
	if cur.Role == core.RoleAdmin && !cur.Disabled {
		if err := ensureNotLastAdmin(ctx, tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_user WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete user %d: %w", id, err)
	}
	return tx.Commit()
}

// ensureNotLastAdmin fails when except is the only enabled administrator left.
func ensureNotLastAdmin(ctx context.Context, tx *sql.Tx, except int64) error {
	var others int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM app_user WHERE role = 'admin' AND disabled = 0 AND id <> ?`, except,
	).Scan(&others); err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if others == 0 {
		return ErrLastAdmin
	}
	return nil
}

// SetUserPassword stores a new hash and ends the account's other sessions.
//
// keepSession is the session id to spare — the one the caller is using, so a
// password change from the UI does not log the user out of the tab they are
// typing in. Zero spares nothing, which is the CLI path (`duskrun user passwd`
// has no session of its own, and an operator resetting a password from a shell
// almost always wants every existing client kicked).
func (s *Store) SetUserPassword(ctx context.Context, userID int64, hash string, keepSession int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set password for user %d: begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE app_user SET password_hash = ? WHERE id = ?`, hash, userID)
	if err != nil {
		return fmt.Errorf("set password for user %d: %w", userID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session WHERE user_id = ? AND id <> ?`, userID, keepSession,
	); err != nil {
		return fmt.Errorf("set password for user %d: drop sessions: %w", userID, err)
	}
	return tx.Commit()
}

// TouchUserLogin stamps last_login_at.
func (s *Store) TouchUserLogin(ctx context.Context, userID int64, at time.Time) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE app_user SET last_login_at = ? WHERE id = ?`, at.Unix(), userID,
	); err != nil {
		return fmt.Errorf("touch login for user %d: %w", userID, err)
	}
	return nil
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
