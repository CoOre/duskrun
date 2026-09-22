package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

const sessionSelect = `SELECT id, user_id, token_hash, user_agent, ip, created_at, last_seen_at, expires_at FROM session`

func scanSession(row rowScanner) (*core.Session, error) {
	var (
		s                          core.Session
		created, lastSeen, expires int64
	)
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.UserAgent, &s.IP, &created, &lastSeen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = unixToTime(created), unixToTime(lastSeen), unixToTime(expires)
	return &s, nil
}

// CreateSession stores a session row and returns its id. TokenHash must already
// be hashed by the caller — this layer never sees the token.
func (s *Store) CreateSession(ctx context.Context, sess core.Session) (int64, error) {
	res, err := s.write.ExecContext(ctx,
		`INSERT INTO session (user_id, token_hash, user_agent, ip, created_at, last_seen_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.UserID, sess.TokenHash, sess.UserAgent, sess.IP,
		sess.CreatedAt.Unix(), sess.LastSeenAt.Unix(), sess.ExpiresAt.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("create session: %w", err)
	}
	return res.LastInsertId()
}

// SessionByToken resolves a hashed token to its session and owner in one read.
// Expiry and the owner's disabled flag are returned as data, not enforced here:
// the caller decides what to do (the API rejects, the sweeper deletes).
func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (*core.Session, *core.User, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT s.id, s.user_id, s.token_hash, s.user_agent, s.ip, s.created_at, s.last_seen_at, s.expires_at,
		        u.id, u.email, u.name, u.role, u.password_hash, u.disabled, u.created_at, u.last_login_at
		   FROM session s JOIN app_user u ON u.id = s.user_id
		  WHERE s.token_hash = ?`, tokenHash)

	var (
		sess                       core.Session
		u                          core.User
		created, lastSeen, expires int64
		role                       string
		disabled                   int
		userCreated                int64
		lastLogin                  sql.NullInt64
	)
	err := row.Scan(
		&sess.ID, &sess.UserID, &sess.TokenHash, &sess.UserAgent, &sess.IP, &created, &lastSeen, &expires,
		&u.ID, &u.Email, &u.Name, &role, &u.PasswordHash, &disabled, &userCreated, &lastLogin,
	)
	if err == sql.ErrNoRows {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("session by token: %w", err)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = unixToTime(created), unixToTime(lastSeen), unixToTime(expires)
	u.Role = core.Role(role)
	u.Disabled = disabled != 0
	u.CreatedAt = unixToTime(userCreated)
	u.LastLoginAt = nullTime(lastLogin)
	return &sess, &u, nil
}

// GetSession loads one session by id.
func (s *Store) GetSession(ctx context.Context, id int64) (*core.Session, error) {
	return scanSession(s.read.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, id))
}

// ListSessions returns sessions, newest first. userID > 0 narrows to one owner;
// zero returns every session (the admin view).
func (s *Store) ListSessions(ctx context.Context, userID int64) ([]core.Session, error) {
	q, args := sessionSelect+` ORDER BY last_seen_at DESC`, []any(nil)
	if userID > 0 {
		q, args = sessionSelect+` WHERE user_id = ? ORDER BY last_seen_at DESC`, []any{userID}
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := make([]core.Session, 0, 8)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// DeleteSession ends one session.
func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM session WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchSession advances last_seen_at, which is what slides the expiry window.
//
// It is deliberately rate-limited by the caller (see api.principal): every
// dashboard poll would otherwise queue a write against the single-writer pool
// for no benefit — the value is only ever read at minute granularity.
func (s *Store) TouchSession(ctx context.Context, id int64, seen, expires time.Time) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE session SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		seen.Unix(), expires.Unix(), id,
	); err != nil {
		return fmt.Errorf("touch session %d: %w", id, err)
	}
	return nil
}

// DeleteExpiredSessions drops sessions past expires_at and reports how many
// went. Called on startup next to ReapStale, and lazily when one is presented.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM session WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
