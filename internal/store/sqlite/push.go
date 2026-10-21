package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/CoOre/duskrun/internal/core"
)

// VAPIDKeys returns the instance's push signing keys, or ErrNotFound before
// the first serve generated them.
func (s *Store) VAPIDKeys(ctx context.Context) (*core.VAPIDKeys, error) {
	var (
		k       core.VAPIDKeys
		created int64
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT public_key, private_key, key_id, created_at FROM push_vapid WHERE id = 1`,
	).Scan(&k.PublicKey, &k.PrivateKey, &k.KeyID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get vapid keys: %w", err)
	}
	k.CreatedAt = unixToTime(created)
	return &k, nil
}

// InitVAPIDKeys stores k unless a key pair already exists, and returns the
// pair that is stored afterwards. Two processes starting on one database at
// once both end up with the winner's keys instead of each believing in its own:
// subscriptions are bound to the public key, so a silent switch would orphan
// every browser that subscribed in between.
func (s *Store) InitVAPIDKeys(ctx context.Context, k core.VAPIDKeys) (*core.VAPIDKeys, error) {
	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO push_vapid (id, public_key, private_key, key_id, created_at)
		 VALUES (1, ?, ?, ?, unixepoch())
		 ON CONFLICT (id) DO NOTHING`,
		k.PublicKey, k.PrivateKey, k.KeyID,
	); err != nil {
		return nil, fmt.Errorf("init vapid keys: %w", err)
	}
	return s.VAPIDKeys(ctx)
}

const pushSubscriptionColumns = `p.id, p.user_id, p.endpoint, p.p256dh, p.auth, p.user_agent,
	p.created_at, p.last_success_at, p.last_error, p.fail_count`

func scanPushSubscription(row rowScanner, extra ...any) (core.PushSubscription, error) {
	var (
		sub     core.PushSubscription
		created int64
		success sql.NullInt64
	)
	dest := append([]any{&sub.ID, &sub.UserID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.UserAgent,
		&created, &success, &sub.LastError, &sub.FailCount}, extra...)
	if err := row.Scan(dest...); err != nil {
		return sub, fmt.Errorf("scan push subscription: %w", err)
	}
	sub.CreatedAt = unixToTime(created)
	sub.LastSuccessAt = nullTime(success)
	return sub, nil
}

// UpsertPushSubscription stores a browser's subscription for sub.UserID and
// returns its id. The endpoint identifies the browser: saving it again
// refreshes the keys (browsers rotate them), and saving it as another user
// hands the row over to that user with a clean delivery record.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub core.PushSubscription) (int64, error) {
	var id int64
	err := s.write.QueryRowContext(ctx,
		`INSERT INTO push_subscription (user_id, endpoint, p256dh, auth, user_agent, created_at)
		 VALUES (?, ?, ?, ?, ?, unixepoch())
		 ON CONFLICT (endpoint) DO UPDATE SET
		   created_at = CASE WHEN user_id = excluded.user_id THEN created_at ELSE excluded.created_at END,
		   last_success_at = CASE WHEN user_id = excluded.user_id THEN last_success_at END,
		   user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth,
		   user_agent = excluded.user_agent, last_error = '', fail_count = 0
		 RETURNING id`,
		sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth, sub.UserAgent,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert push subscription for user %d: %w", sub.UserID, err)
	}
	return id, nil
}

// ListPushSubscriptions returns one user's browsers, oldest first.
func (s *Store) ListPushSubscriptions(ctx context.Context, userID int64) ([]core.PushSubscription, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+pushSubscriptionColumns+` FROM push_subscription p WHERE p.user_id = ? ORDER BY p.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions for user %d: %w", userID, err)
	}
	defer rows.Close()
	out := make([]core.PushSubscription, 0, 4)
	for rows.Next() {
		sub, err := scanPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetPushSubscription loads one of the user's subscriptions. Someone else's
// is ErrNotFound, the same as one that does not exist.
func (s *Store) GetPushSubscription(ctx context.Context, userID, id int64) (*core.PushSubscription, error) {
	sub, err := scanPushSubscription(s.read.QueryRowContext(ctx,
		`SELECT `+pushSubscriptionColumns+` FROM push_subscription p WHERE p.id = ? AND p.user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// PushTargets returns every subscription whose owner is enabled, with the
// owner's role filled in. Disabled users keep their subscriptions — enabling
// the account again resumes delivery — but receive nothing meanwhile.
func (s *Store) PushTargets(ctx context.Context) ([]core.PushSubscription, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+pushSubscriptionColumns+`, u.role
		   FROM push_subscription p JOIN app_user u ON u.id = p.user_id
		  WHERE u.disabled = 0
		  ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("list push targets: %w", err)
	}
	defer rows.Close()
	out := make([]core.PushSubscription, 0, 8)
	for rows.Next() {
		var role string
		sub, err := scanPushSubscription(rows, &role)
		if err != nil {
			return nil, err
		}
		sub.Role = core.Role(role)
		out = append(out, sub)
	}
	return out, rows.Err()
}

// DeletePushSubscription removes one of the user's subscriptions.
func (s *Store) DeletePushSubscription(ctx context.Context, userID, id int64) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM push_subscription WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete push subscription %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePushSubscriptionByEndpoint removes the subscription of one browser.
// userID scopes it to that user; 0 removes it whoever owns it, which is what
// delivery does when the push service reports the subscription gone. A
// missing row is not an error: the outcome is the same.
func (s *Store) DeletePushSubscriptionByEndpoint(ctx context.Context, userID int64, endpoint string) error {
	q, args := `DELETE FROM push_subscription WHERE endpoint = ?`, []any{endpoint}
	if userID != 0 {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	if _, err := s.write.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("delete push subscription by endpoint: %w", err)
	}
	return nil
}

// MarkPushResult records the outcome of a delivery to one subscription. An
// empty cause is a success and clears the failure streak.
func (s *Store) MarkPushResult(ctx context.Context, id int64, cause string) error {
	q := `UPDATE push_subscription SET last_success_at = unixepoch(), last_error = '', fail_count = 0 WHERE id = ?`
	args := []any{id}
	if cause != "" {
		q = `UPDATE push_subscription SET last_error = ?, fail_count = fail_count + 1 WHERE id = ?`
		args = []any{cause, id}
	}
	if _, err := s.write.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("mark push result %d: %w", id, err)
	}
	return nil
}
