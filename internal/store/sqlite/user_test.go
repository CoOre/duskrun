package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

func mkUser(t *testing.T, st *Store, email string, role core.Role) int64 {
	t.Helper()
	id, err := st.CreateUser(context.Background(), core.User{
		Email: email, Name: email, Role: role, PasswordHash: "$argon2id$stub",
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	return id
}

func mkSession(t *testing.T, st *Store, userID int64, hash string, expires time.Time) int64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := st.CreateSession(context.Background(), core.Session{
		UserID: userID, TokenHash: hash, UserAgent: "test", IP: "127.0.0.1",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return id
}

func TestUserCRUDAndEmailNormalisation(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	id := mkUser(t, st, "  Admin@Corp.IO ", core.RoleAdmin)
	u, err := st.GetUser(ctx, id)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Email != "admin@corp.io" {
		t.Fatalf("email = %q, want it trimmed and lowercased", u.Email)
	}
	// The same address in a different case must resolve to the same row, or a
	// user typing "Admin@..." at the login form gets "no such account".
	byEmail, err := st.GetUserByEmail(ctx, "ADMIN@corp.io")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if byEmail.ID != id {
		t.Fatalf("GetUserByEmail id = %d, want %d", byEmail.ID, id)
	}

	if _, err := st.GetUserByEmail(ctx, "nobody@corp.io"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user err = %v, want ErrNotFound", err)
	}

	n, err := st.CountUsers(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v; want 1, nil", n, err)
	}
}

// TestLastAdminProtected covers the three ways to strand an instance with no
// administrator. Recovering needs DUSKRUN_API_TOKEN, which a deployment is
// allowed not to set, so all three must be refused.
func TestLastAdminProtected(t *testing.T) {
	ctx := context.Background()

	t.Run("demote", func(t *testing.T) {
		st := openTemp(t)
		id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
		mkUser(t, st, "v@corp.io", core.RoleViewer)
		u, _ := st.GetUser(ctx, id)
		u.Role = core.RoleOperator
		if err := st.UpdateUser(ctx, *u); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("demote err = %v, want ErrLastAdmin", err)
		}
	})

	t.Run("disable", func(t *testing.T) {
		st := openTemp(t)
		id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
		u, _ := st.GetUser(ctx, id)
		u.Disabled = true
		if err := st.UpdateUser(ctx, *u); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("disable err = %v, want ErrLastAdmin", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		st := openTemp(t)
		id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
		if err := st.DeleteUser(ctx, id); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("delete err = %v, want ErrLastAdmin", err)
		}
	})

	t.Run("allowed with a second admin", func(t *testing.T) {
		st := openTemp(t)
		first := mkUser(t, st, "a@corp.io", core.RoleAdmin)
		mkUser(t, st, "b@corp.io", core.RoleAdmin)
		if err := st.DeleteUser(ctx, first); err != nil {
			t.Fatalf("delete with a spare admin: %v", err)
		}
	})

	// A disabled admin does not count as cover: leaving only a disabled admin
	// is the same dead end as leaving none.
	t.Run("disabled admin does not count", func(t *testing.T) {
		st := openTemp(t)
		active := mkUser(t, st, "a@corp.io", core.RoleAdmin)
		sleeping := mkUser(t, st, "b@corp.io", core.RoleAdmin)
		u, _ := st.GetUser(ctx, sleeping)
		u.Disabled = true
		if err := st.UpdateUser(ctx, *u); err != nil {
			t.Fatalf("disable spare admin: %v", err)
		}
		if err := st.DeleteUser(ctx, active); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("delete err = %v, want ErrLastAdmin", err)
		}
	})
}

// TestDisableDropsSessions: disabling must end access immediately, not at the
// session's own expiry.
func TestDisableDropsSessions(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	mkUser(t, st, "keep-admin@corp.io", core.RoleAdmin)
	id := mkUser(t, st, "op@corp.io", core.RoleOperator)
	mkSession(t, st, id, "hash-1", time.Now().Add(time.Hour))
	mkSession(t, st, id, "hash-2", time.Now().Add(time.Hour))

	u, _ := st.GetUser(ctx, id)
	u.Disabled = true
	if err := st.UpdateUser(ctx, *u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got, err := st.ListSessions(ctx, id)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("sessions after disable = %d, want 0", len(got))
	}
}

// TestSetUserPasswordKeepsOne covers both callers: the API keeps the calling
// session, the CLI passes 0 and keeps none.
func TestSetUserPasswordKeepsOne(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	keep := mkSession(t, st, id, "keep", time.Now().Add(time.Hour))
	mkSession(t, st, id, "drop", time.Now().Add(time.Hour))

	if err := st.SetUserPassword(ctx, id, "$argon2id$new", keep); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}
	got, _ := st.ListSessions(ctx, id)
	if len(got) != 1 || got[0].ID != keep {
		t.Fatalf("sessions = %+v, want only the kept one (%d)", got, keep)
	}
	u, _ := st.GetUser(ctx, id)
	if u.PasswordHash != "$argon2id$new" {
		t.Fatalf("hash = %q, want the new one", u.PasswordHash)
	}

	if err := st.SetUserPassword(ctx, id, "$argon2id$newer", 0); err != nil {
		t.Fatalf("SetUserPassword(keep=0): %v", err)
	}
	if got, _ := st.ListSessions(ctx, id); len(got) != 0 {
		t.Fatalf("sessions after CLI-style reset = %d, want 0", len(got))
	}
}

func TestSessionLookupAndExpiry(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	live := mkSession(t, st, id, "live", time.Now().Add(time.Hour))
	mkSession(t, st, id, "dead", time.Now().Add(-time.Hour))

	sess, user, err := st.SessionByToken(ctx, "live")
	if err != nil {
		t.Fatalf("SessionByToken: %v", err)
	}
	if sess.ID != live || user.ID != id {
		t.Fatalf("resolved session %d/user %d, want %d/%d", sess.ID, user.ID, live, id)
	}
	if _, _, err := st.SessionByToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}

	n, err := st.DeleteExpiredSessions(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired swept = %d, want 1", n)
	}
	if got, _ := st.ListSessions(ctx, 0); len(got) != 1 {
		t.Fatalf("remaining sessions = %d, want 1", len(got))
	}
}

// TestDeleteUserCascadesSessions relies on the FK: without foreign_keys(1) the
// rows would survive their owner and SessionByToken would fail oddly later.
func TestDeleteUserCascadesSessions(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	mkUser(t, st, "keep-admin@corp.io", core.RoleAdmin)
	id := mkUser(t, st, "gone@corp.io", core.RoleViewer)
	mkSession(t, st, id, "orphan", time.Now().Add(time.Hour))

	if err := st.DeleteUser(ctx, id); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, err := st.SessionByToken(ctx, "orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived its user: err = %v", err)
	}
}

func TestTouchSessionSlidesExpiry(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	sid := mkSession(t, st, id, "tok", time.Now().Add(time.Hour))

	seen := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	exp := seen.Add(24 * time.Hour)
	if err := st.TouchSession(ctx, sid, seen, exp); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	got, err := st.GetSession(ctx, sid)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.LastSeenAt.Equal(seen) || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("last_seen/expires = %v/%v, want %v/%v", got.LastSeenAt, got.ExpiresAt, seen, exp)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	// A missing key yields the default rather than an error: every setting has
	// one, and callers should not have to special-case "never written".
	v, err := st.GetSetting(ctx, "instance_name", "Duskrun")
	if err != nil || v != "Duskrun" {
		t.Fatalf("GetSetting missing = %q, %v; want the default", v, err)
	}
	if err := st.PutSettings(ctx, map[string]string{"instance_name": "prod", "x": "1"}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	if v, _ := st.GetSetting(ctx, "instance_name", "Duskrun"); v != "prod" {
		t.Fatalf("GetSetting = %q, want prod", v)
	}
	// Upsert, not insert: writing the same key twice must not fail.
	if err := st.PutSettings(ctx, map[string]string{"instance_name": "staging"}); err != nil {
		t.Fatalf("PutSettings overwrite: %v", err)
	}
	if v, _ := st.GetSetting(ctx, "instance_name", "Duskrun"); v != "staging" {
		t.Fatalf("GetSetting after overwrite = %q, want staging", v)
	}
	all, err := st.ListSettings(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListSettings = %d rows, %v; want 2", len(all), err)
	}
}
