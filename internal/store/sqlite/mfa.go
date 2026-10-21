package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/CoOre/duskrun/internal/core"
)

// GetTOTP loads a user's TOTP enrolment, or ErrNotFound when there is none.
func (s *Store) GetTOTP(ctx context.Context, userID int64) (*core.TOTP, error) {
	var (
		t       core.TOTP
		created int64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT user_id, secret, key_id, last_step, created_at FROM user_totp WHERE user_id = ?`, userID,
	).Scan(&t.UserID, &t.Secret, &t.KeyID, &t.LastStep, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get totp for user %d: %w", userID, err)
	}
	t.CreatedAt = unixToTime(created)
	return &t, nil
}

// PutTOTP stores (or replaces) a user's TOTP secret. Replacing is how a user
// moves to a new phone: the old secret stops working the moment the new one is
// confirmed. lastStep is the step the confirming code was accepted at, so that
// same code cannot be replayed at the next login.
//
// recoveryHashes, when non-nil, replaces the user's recovery codes in the same
// transaction — the API passes them when this is the account's first factor.
func (s *Store) PutTOTP(ctx context.Context, t core.TOTP, recoveryHashes []string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("put totp for user %d: begin: %w", t.UserID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_totp (user_id, secret, key_id, last_step, created_at)
		 VALUES (?, ?, ?, ?, unixepoch())
		 ON CONFLICT (user_id) DO UPDATE SET
		   secret = excluded.secret, key_id = excluded.key_id,
		   last_step = excluded.last_step, created_at = excluded.created_at`,
		t.UserID, t.Secret, t.KeyID, t.LastStep,
	); err != nil {
		return fmt.Errorf("put totp for user %d: %w", t.UserID, err)
	}
	if recoveryHashes != nil {
		if err := replaceRecoveryCodes(ctx, tx, t.UserID, recoveryHashes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AdvanceTOTPStep records that a code for step was accepted. It reports false
// when an equal or newer step was already recorded — the code was used before,
// possibly by a concurrent request, and must be refused. The comparison lives
// in the UPDATE so two racing logins with the same code cannot both win.
func (s *Store) AdvanceTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE user_totp SET last_step = ? WHERE user_id = ? AND last_step < ?`, step, userID, step,
	)
	if err != nil {
		return false, fmt.Errorf("advance totp step for user %d: %w", userID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteTOTP removes the user's TOTP secret. Recovery codes go too when no
// passkey is left: they are a fallback for a second factor, and keeping them
// around without one would leave a credential nobody remembers holding.
func (s *Store) DeleteTOTP(ctx context.Context, userID int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete totp for user %d: begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete totp for user %d: %w", userID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := dropOrphanRecoveryCodes(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListWebAuthnCredentials returns a user's passkeys, oldest first.
func (s *Store) ListWebAuthnCredentials(ctx context.Context, userID int64) ([]core.WebAuthnCredential, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, user_id, credential_id, name, data, created_at, last_used_at
		   FROM webauthn_credential WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list webauthn credentials for user %d: %w", userID, err)
	}
	defer rows.Close()
	out := make([]core.WebAuthnCredential, 0, 4)
	for rows.Next() {
		var (
			c       core.WebAuthnCredential
			created int64
			used    sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.Name, &c.Data, &created, &used); err != nil {
			return nil, fmt.Errorf("scan webauthn credential: %w", err)
		}
		c.CreatedAt, c.LastUsedAt = unixToTime(created), nullTime(used)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddWebAuthnCredential stores a newly registered passkey and returns its id.
// recoveryHashes behaves as in PutTOTP.
func (s *Store) AddWebAuthnCredential(ctx context.Context, c core.WebAuthnCredential, recoveryHashes []string) (int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("add webauthn credential: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_credential (user_id, credential_id, name, data, created_at)
		 VALUES (?, ?, ?, ?, unixepoch())`,
		c.UserID, c.CredentialID, c.Name, c.Data,
	)
	if err != nil {
		return 0, fmt.Errorf("add webauthn credential: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if recoveryHashes != nil {
		if err := replaceRecoveryCodes(ctx, tx, c.UserID, recoveryHashes); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// TouchWebAuthnCredential stores the credential record as updated by a
// successful assertion (the sign counter moves) and stamps last_used_at.
func (s *Store) TouchWebAuthnCredential(ctx context.Context, id int64, data []byte, at time.Time) error {
	if _, err := s.write.ExecContext(ctx,
		`UPDATE webauthn_credential SET data = ?, last_used_at = ? WHERE id = ?`, data, at.Unix(), id,
	); err != nil {
		return fmt.Errorf("touch webauthn credential %d: %w", id, err)
	}
	return nil
}

// DeleteWebAuthnCredential removes one of userID's passkeys. The owner is part
// of the WHERE clause, so a caller cannot delete someone else's credential by
// guessing its id; that case reports ErrNotFound. Orphaned recovery codes are
// dropped as in DeleteTOTP.
func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, id int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete webauthn credential %d: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM webauthn_credential WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete webauthn credential %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := dropOrphanRecoveryCodes(ctx, tx, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceRecoveryCodes discards every recovery code of the user, used or not,
// and stores the given hashes as the new set.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace recovery codes for user %d: begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := replaceRecoveryCodes(ctx, tx, userID, hashes); err != nil {
		return err
	}
	return tx.Commit()
}

// CountRecoveryCodes reports how many unused recovery codes the user has.
func (s *Store) CountRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recovery_code WHERE user_id = ? AND used_at IS NULL`, userID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count recovery codes for user %d: %w", userID, err)
	}
	return n, nil
}

// UseRecoveryCode spends one code. It reports false when the hash matches no
// unused code of this user; like AdvanceTOTPStep, the check and the spend are
// one statement, so a code cannot be redeemed twice by racing requests.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, hash string, at time.Time) (bool, error) {
	res, err := s.write.ExecContext(ctx,
		`UPDATE recovery_code SET used_at = ?
		  WHERE id = (SELECT id FROM recovery_code
		               WHERE user_id = ? AND code_hash = ? AND used_at IS NULL LIMIT 1)`,
		at.Unix(), userID, hash,
	)
	if err != nil {
		return false, fmt.Errorf("use recovery code for user %d: %w", userID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ResetUserMFA removes every second factor and recovery code of the account and
// ends all of its sessions. It is the "lost my phone and my codes" path, used
// by an admin from the UI and by `duskrun user reset-2fa` from a shell; ending
// the sessions makes sure whoever might hold the lost device's session is out
// too.
func (s *Store) ResetUserMFA(ctx context.Context, userID int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reset mfa for user %d: begin: %w", userID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM app_user WHERE id = ?`, userID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("reset mfa for user %d: %w", userID, err)
	}
	for _, q := range []string{
		`DELETE FROM user_totp WHERE user_id = ?`,
		`DELETE FROM webauthn_credential WHERE user_id = ?`,
		`DELETE FROM recovery_code WHERE user_id = ?`,
		`DELETE FROM session WHERE user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return fmt.Errorf("reset mfa for user %d: %w", userID, err)
		}
	}
	return tx.Commit()
}

func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64, hashes []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_code WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("replace recovery codes for user %d: %w", userID, err)
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO recovery_code (user_id, code_hash) VALUES (?, ?)`, userID, h,
		); err != nil {
			return fmt.Errorf("replace recovery codes for user %d: %w", userID, err)
		}
	}
	return nil
}

// dropOrphanRecoveryCodes deletes the user's recovery codes when no second
// factor remains.
func dropOrphanRecoveryCodes(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM recovery_code WHERE user_id = ?
		   AND NOT EXISTS (SELECT 1 FROM user_totp WHERE user_id = ?)
		   AND NOT EXISTS (SELECT 1 FROM webauthn_credential WHERE user_id = ?)`,
		userID, userID, userID,
	); err != nil {
		return fmt.Errorf("drop orphan recovery codes for user %d: %w", userID, err)
	}
	return nil
}
