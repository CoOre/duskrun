package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// testPassword is long enough to clear minPasswordLen everywhere it is used.
const testPassword = "correct horse battery staple"

// authServer returns a server backed by a temp store, seeded with one user per
// role. Passwords are all testPassword.
func authServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	return authServerWith(t, Deps{})
}

func authServerWith(t *testing.T, extra Deps) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// One cheap hash reused for every seeded account: argon2 at the real cost
	// would add seconds to a test that is about routing, not hashing.
	hash, err := auth.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, r := range []core.Role{core.RoleAdmin, core.RoleOperator, core.RoleViewer} {
		if _, err := st.CreateUser(ctx, core.User{
			Email: string(r) + "@corp.io", Name: string(r), Role: r, PasswordHash: hash,
		}); err != nil {
			t.Fatalf("seed %s: %v", r, err)
		}
	}

	extra.Store = st
	if extra.Token == "" {
		extra.Token = testToken
	}
	srv := httptest.NewServer(NewRouter(extra))
	t.Cleanup(srv.Close)
	return srv, st
}

// loginAs performs a real login and returns the session token.
func loginAs(t *testing.T, srv *httptest.Server, role core.Role) string {
	t.Helper()
	resp := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": string(role) + "@corp.io", "password": testPassword,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login as %s: status %d", role, resp.StatusCode)
	}
	var out loginResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("login returned an empty token")
	}
	return out.Token
}

// doJSON issues a request with an optional bearer token and JSON body.
func doJSON(t *testing.T, srv *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestLoginNeedsNoCredential is the regression for mounting /api/login outside
// the authentication middleware: it lives under /api, which is otherwise fully
// guarded, so it is one `r.Use` away from being unreachable.
func TestLoginNeedsNoCredential(t *testing.T) {
	srv, _ := authServer(t)
	resp := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": "admin@corp.io", "password": testPassword,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 without an Authorization header", resp.StatusCode)
	}
}

// TestLoginFailuresAreIndistinguishable: a wrong password, an unknown address
// and a disabled account must be one answer, or the endpoint reveals which
// addresses have accounts.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	srv, st := authServer(t)
	ctx := context.Background()
	u, err := st.GetUserByEmail(ctx, "viewer@corp.io")
	if err != nil {
		t.Fatal(err)
	}
	u.Disabled = true
	if err := st.UpdateUser(ctx, *u); err != nil {
		t.Fatal(err)
	}

	cases := map[string]map[string]string{
		"wrong password": {"email": "admin@corp.io", "password": "not the password"},
		"unknown email":  {"email": "ghost@corp.io", "password": testPassword},
		"disabled user":  {"email": "viewer@corp.io", "password": testPassword},
	}
	var seen []string
	for name, body := range cases {
		resp := doJSON(t, srv, http.MethodPost, "/api/login", "", body)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, resp.StatusCode)
		}
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		seen = append(seen, e["error"])
	}
	for _, msg := range seen {
		if msg != seen[0] {
			t.Fatalf("failure messages differ: %q vs %q — that is an enumeration oracle", msg, seen[0])
		}
	}
}

func TestLoginRejectsDisabledUserImmediately(t *testing.T) {
	srv, st := authServer(t)
	token := loginAs(t, srv, core.RoleOperator)

	// Session works…
	resp := doJSON(t, srv, http.MethodGet, "/api/ping", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("before disable: status = %d, want 200", resp.StatusCode)
	}

	// …and stops the moment the account is switched off, without waiting for
	// the session to expire.
	ctx := context.Background()
	u, _ := st.GetUserByEmail(ctx, "operator@corp.io")
	u.Disabled = true
	if err := st.UpdateUser(ctx, *u); err != nil {
		t.Fatal(err)
	}
	resp2 := doJSON(t, srv, http.MethodGet, "/api/ping", token, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after disable: status = %d, want 401", resp2.StatusCode)
	}
}

// TestSessionTokenNotStoredInClear is acceptance criterion §11.6: a copy of the
// database must not hand over live sessions.
func TestSessionTokenNotStoredInClear(t *testing.T) {
	srv, st := authServer(t)
	token := loginAs(t, srv, core.RoleAdmin)

	sessions, err := st.ListSessions(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].TokenHash == token {
		t.Fatal("the session token itself is stored")
	}
	if sessions[0].TokenHash != auth.TokenHash(token) {
		t.Fatal("stored value is not the token's SHA-256")
	}
}

func TestStaticTokenIsAdminAndCreatesNoSession(t *testing.T) {
	srv, st := authServer(t)
	resp := doJSON(t, srv, http.MethodGet, "/api/me", testToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var me meDTO
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if !me.Static {
		t.Fatal("static token did not report itself as such")
	}
	if me.Role != string(core.RoleAdmin) {
		t.Fatalf("role = %q, want admin", me.Role)
	}
	if me.Email != "" {
		t.Fatalf("email = %q, want empty — the token is not a person", me.Email)
	}
	sessions, _ := st.ListSessions(context.Background(), 0)
	if len(sessions) != 0 {
		t.Fatalf("sessions = %d, want 0: the static token must not create one", len(sessions))
	}
}

func TestMeReportsRealUser(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, core.RoleOperator)
	resp := doJSON(t, srv, http.MethodGet, "/api/me", token, nil)
	defer resp.Body.Close()
	var me meDTO
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Static || me.Email != "operator@corp.io" || me.Role != "operator" {
		t.Fatalf("me = %+v, want the operator account", me)
	}
	if me.SessionID == 0 {
		t.Fatal("me did not report a session id")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, core.RoleViewer)

	resp := doJSON(t, srv, http.MethodPost, "/api/logout", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", resp.StatusCode)
	}
	resp2 := doJSON(t, srv, http.MethodGet, "/api/ping", token, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: status = %d, want 401", resp2.StatusCode)
	}
}

// TestLogoutWithStaticTokenSucceeds: the SPA calls logout unconditionally, and
// the static token has no session to end. It must not be an error.
func TestLogoutWithStaticTokenSucceeds(t *testing.T) {
	srv, _ := authServer(t)
	resp := doJSON(t, srv, http.MethodPost, "/api/logout", testToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestChangePasswordKeepsCurrentSession is acceptance criterion §11.5.
func TestChangePasswordKeepsCurrentSession(t *testing.T) {
	srv, _ := authServer(t)
	keep := loginAs(t, srv, core.RoleViewer)
	other := loginAs(t, srv, core.RoleViewer)

	resp := doJSON(t, srv, http.MethodPost, "/api/me/password", keep, map[string]string{
		"old_password": testPassword, "new_password": "a whole new passphrase",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change password status = %d, want 200", resp.StatusCode)
	}

	r1 := doJSON(t, srv, http.MethodGet, "/api/ping", keep, nil)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("calling session died: status = %d, want 200", r1.StatusCode)
	}
	r2 := doJSON(t, srv, http.MethodGet, "/api/ping", other, nil)
	r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session survived: status = %d, want 401", r2.StatusCode)
	}

	// The new password is the one that works now.
	bad := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": "viewer@corp.io", "password": testPassword,
	})
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still logs in: status = %d", bad.StatusCode)
	}
}

func TestChangePasswordRequiresOldPassword(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, core.RoleViewer)
	resp := doJSON(t, srv, http.MethodPost, "/api/me/password", token, map[string]string{
		"old_password": "wrong", "new_password": "a whole new passphrase",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestChangePasswordEnforcesLength(t *testing.T) {
	srv, _ := authServer(t)
	token := loginAs(t, srv, core.RoleViewer)
	resp := doJSON(t, srv, http.MethodPost, "/api/me/password", token, map[string]string{
		"old_password": testPassword, "new_password": "short",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSessionExpiryRejects drives the clock past the session's expiry.
func TestSessionExpiryRejects(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	srv, _ := authServerWith(t, Deps{Now: func() time.Time { return clock() }})

	token := loginAs(t, srv, core.RoleAdmin)
	now = now.Add(sessionTTL + time.Minute)

	resp := doJSON(t, srv, http.MethodGet, "/api/ping", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 after expiry", resp.StatusCode)
	}
}

// TestSessionAbsoluteCap: sliding renewal must not outlive sessionMaxAge, or an
// actively polled session never ends.
func TestSessionAbsoluteCap(t *testing.T) {
	now := time.Now()
	srv, _ := authServerWith(t, Deps{Now: func() time.Time { return now }})
	token := loginAs(t, srv, core.RoleAdmin)

	// Keep using it well past the sliding window but inside the cap; each call
	// slides the expiry forward.
	for i := 0; i < 10; i++ {
		now = now.Add(20 * 24 * time.Hour)
		resp := doJSON(t, srv, http.MethodGet, "/api/ping", token, nil)
		status := resp.StatusCode
		resp.Body.Close()
		if i < 4 && status != http.StatusOK {
			t.Fatalf("day %d: status = %d, want 200 (still inside the 90d cap)", (i+1)*20, status)
		}
		if i >= 4 && status == http.StatusOK {
			t.Fatalf("day %d: session still valid past the %v cap", (i+1)*20, sessionMaxAge)
		}
	}
}

// TestLoginThrottleBlocks: repeated failures for one email eventually get 429
// rather than an endless stream of argon2 work.
func TestLoginThrottleBlocks(t *testing.T) {
	srv, _ := authServer(t)
	body := map[string]string{"email": "admin@corp.io", "password": "wrong"}
	var last int
	for i := 0; i < 12; i++ {
		resp := doJSON(t, srv, http.MethodPost, "/api/login", "", body)
		last = resp.StatusCode
		resp.Body.Close()
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("final status = %d, want 429 after repeated failures", last)
	}
	// The correct password is refused too while the lockout holds — that is the
	// point, and it is why the window is short.
	resp := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": "admin@corp.io", "password": testPassword,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 during lockout", resp.StatusCode)
	}
}

func TestClientIPHonoursProxyOnlyWhenTrusted(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	if got := clientIP(req, false); got != "10.0.0.9" {
		t.Fatalf("untrusted: ip = %q, want the socket address", got)
	}
	// The right-most entry is the one the trusted proxy appended; everything
	// left of it was sent by the caller and would otherwise be a free throttling
	// key per request.
	if got := clientIP(req, true); got != "10.0.0.1" {
		t.Fatalf("trusted: ip = %q, want the right-most forwarded address", got)
	}
}

// TestClientIPIgnoresSpoofedForwardedPrefix: a caller prepending its own
// X-Forwarded-For cannot move itself to a fresh throttling bucket.
func TestClientIPIgnoresSpoofedForwardedPrefix(t *testing.T) {
	first, _ := http.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "10.0.0.9:5555"
	first.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.5")

	second, _ := http.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "10.0.0.9:5555"
	second.Header.Set("X-Forwarded-For", "198.51.100.2, 203.0.113.5")

	if a, b := clientIP(first, true), clientIP(second, true); a != b {
		t.Fatalf("spoofed prefix changed the key: %q vs %q", a, b)
	}
}

// --- sessions endpoints --------------------------------------------------

func TestListSessionsScopedByRole(t *testing.T) {
	srv, _ := authServer(t)
	viewer := loginAs(t, srv, core.RoleViewer)
	loginAs(t, srv, core.RoleOperator)
	admin := loginAs(t, srv, core.RoleAdmin)

	var mine []sessionDTO
	resp := doJSON(t, srv, http.MethodGet, "/api/sessions", viewer, nil)
	if err := json.NewDecoder(resp.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(mine) != 1 {
		t.Fatalf("viewer sees %d sessions, want only its own", len(mine))
	}
	if !mine[0].Current {
		t.Fatal("viewer's own session is not marked current")
	}

	var all []sessionDTO
	resp2 := doJSON(t, srv, http.MethodGet, "/api/sessions", admin, nil)
	if err := json.NewDecoder(resp2.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if len(all) != 3 {
		t.Fatalf("admin sees %d sessions, want 3", len(all))
	}
	for _, s := range all {
		if s.Email == "" {
			t.Fatalf("admin view is missing the owner email: %+v", s)
		}
	}
}

func TestDeleteSessionOwnership(t *testing.T) {
	srv, st := authServer(t)
	viewer := loginAs(t, srv, core.RoleViewer)
	operator := loginAs(t, srv, core.RoleOperator)

	sessions, _ := st.ListSessions(context.Background(), 0)
	var opSession int64
	for _, s := range sessions {
		u, _ := st.GetUser(context.Background(), s.UserID)
		if u.Email == "operator@corp.io" {
			opSession = s.ID
		}
	}

	// 403, not 404: session ids are sequential and not a secret.
	resp := doJSON(t, srv, http.MethodDelete, fmt.Sprintf("/api/sessions/%d", opSession), viewer, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer deleting another session: status = %d, want 403", resp.StatusCode)
	}

	// Ending your own last session is allowed — "log out everywhere".
	var own []sessionDTO
	r := doJSON(t, srv, http.MethodGet, "/api/sessions", operator, nil)
	_ = json.NewDecoder(r.Body).Decode(&own)
	r.Body.Close()
	resp2 := doJSON(t, srv, http.MethodDelete, fmt.Sprintf("/api/sessions/%d", own[0].ID), operator, nil)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("deleting own session: status = %d, want 204", resp2.StatusCode)
	}
	resp3 := doJSON(t, srv, http.MethodGet, "/api/ping", operator, nil)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token still works after ending its session: %d", resp3.StatusCode)
	}
}

// --- settings ------------------------------------------------------------

func TestSettingsRoundTripAndInstanceBlock(t *testing.T) {
	srv, _ := authServerWith(t, Deps{Instance: InstanceInfo{
		Version: "1.2.3", DBPath: "/data/duskrun.db", Workers: 7, RetentionCron: "30 3 * * *",
	}})
	admin := loginAs(t, srv, core.RoleAdmin)

	resp := doJSON(t, srv, http.MethodPatch, "/api/settings", admin, map[string]string{
		"instance_name": "prod-eu", "ssh_host_key_mode_default": "strict",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", resp.StatusCode)
	}
	var got settingsDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.InstanceName != "prod-eu" || got.HostKeyMode != "strict" {
		t.Fatalf("settings = %+v, want the values just written", got)
	}
	if got.Instance.Version != "1.2.3" || got.Instance.Workers != 7 {
		t.Fatalf("instance block = %+v, want the process configuration", got.Instance)
	}
	if got.Instance.Timezone == "" {
		t.Fatal("instance block does not report a timezone")
	}
}

func TestSettingsRejectUnknownHostKeyMode(t *testing.T) {
	srv, _ := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	resp := doJSON(t, srv, http.MethodPatch, "/api/settings", admin, map[string]string{
		"ssh_host_key_mode_default": "whatever",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHostKeyDefaultAppliesToNewConnectionsOnly: the setting is a default for
// new rows, never a retroactive rewrite of tunnels that already verify a host.
func TestApplyHostKeyDefault(t *testing.T) {
	cases := []struct {
		name, connType, cfg, mode, want string
	}{
		{"ssh without a mode", "ssh-tunnel", `{"host":"h"}`, "strict", "strict"},
		{"ssh with an explicit mode", "ssh-tunnel", `{"host":"h","host_key_mode":"tofu"}`, "strict", "tofu"},
		{"ssh with an empty mode", "ssh-tunnel", `{"host":"h","host_key_mode":""}`, "strict", "strict"},
		// A literal null decodes into a nil map; stamping into that used to
		// panic and take the connection down with it.
		{"ssh with a null config", "ssh-tunnel", `null`, "strict", "strict"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := applyHostKeyDefault(c.connType, []byte(c.cfg), c.mode)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(out, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["host_key_mode"] != c.want {
				t.Fatalf("host_key_mode = %v, want %q", fields["host_key_mode"], c.want)
			}
		})
	}

	// A non-ssh connector is returned byte-for-byte: nothing to stamp.
	in := []byte(`{"host":"db"}`)
	out, err := applyHostKeyDefault("direct", in, "strict")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Fatalf("direct config was rewritten: %s", out)
	}
}

// --- users ---------------------------------------------------------------

func TestCreateUserAndLogin(t *testing.T) {
	srv, _ := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)

	resp := doJSON(t, srv, http.MethodPost, "/api/users", admin, map[string]string{
		"email": "New.Person@Corp.IO", "name": "New", "role": "operator", "password": testPassword,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var u userDTO
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		t.Fatal(err)
	}
	if u.Email != "new.person@corp.io" {
		t.Fatalf("email = %q, want it normalised", u.Email)
	}

	login := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": "new.person@corp.io", "password": testPassword,
	})
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("new user cannot log in: status = %d", login.StatusCode)
	}
}

func TestCreateUserRejectsDuplicateAndBadInput(t *testing.T) {
	srv, _ := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)

	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"duplicate email", map[string]string{"email": "admin@corp.io", "role": "viewer", "password": testPassword}, http.StatusConflict},
		{"unknown role", map[string]string{"email": "x@corp.io", "role": "root", "password": testPassword}, http.StatusBadRequest},
		{"short password", map[string]string{"email": "y@corp.io", "role": "viewer", "password": "short"}, http.StatusBadRequest},
		{"invalid email", map[string]string{"email": "not-an-address", "role": "viewer", "password": testPassword}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doJSON(t, srv, http.MethodPost, "/api/users", admin, c.body)
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

// TestAdminCannotLockThemselvesOut is acceptance criterion §11.8.
func TestAdminCannotLockThemselvesOut(t *testing.T) {
	srv, st := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	me, _ := st.GetUserByEmail(context.Background(), "admin@corp.io")

	t.Run("demote self", func(t *testing.T) {
		resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", me.ID), admin,
			map[string]string{"role": "viewer"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
	t.Run("disable self", func(t *testing.T) {
		resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", me.ID), admin,
			map[string]bool{"disabled": true})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
	t.Run("delete self", func(t *testing.T) {
		resp := doJSON(t, srv, http.MethodDelete, fmt.Sprintf("/api/users/%d", me.ID), admin, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	// And the store-level guard, reached via the static token, which is not
	// "self" for anyone: demoting the only admin is still refused.
	t.Run("demote the only admin as the static token", func(t *testing.T) {
		resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", me.ID), testToken,
			map[string]string{"role": "viewer"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})
}

// TestPatchUserKeepsOmittedFields mirrors PATCH /notifiers/{id}.
func TestPatchUserKeepsOmittedFields(t *testing.T) {
	srv, st := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	target, _ := st.GetUserByEmail(context.Background(), "viewer@corp.io")

	resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", target.ID), admin,
		map[string]string{"name": "Renamed"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got userDTO
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("name = %q, want Renamed", got.Name)
	}
	if got.Role != "viewer" || got.Disabled {
		t.Fatalf("omitted fields changed: %+v", got)
	}
}

// TestAdminPasswordResetEndsEverySession: unlike a self-service change, an
// admin reset spares nothing — including the client that prompted it.
func TestAdminPasswordResetEndsEverySession(t *testing.T) {
	srv, st := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	victim := loginAs(t, srv, core.RoleViewer)
	target, _ := st.GetUserByEmail(context.Background(), "viewer@corp.io")

	resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", target.ID), admin,
		map[string]string{"password": "brand new passphrase"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	after := doJSON(t, srv, http.MethodGet, "/api/ping", victim, nil)
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("victim session survived the reset: %d", after.StatusCode)
	}
}

// TestPatchUserRejectsShortPasswordBeforeApplyingRole: the settings page sends
// name, role and password in one request, so a rejected password must leave the
// role alone rather than reporting a failure that half happened.
func TestPatchUserRejectsShortPasswordBeforeApplyingRole(t *testing.T) {
	srv, st := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	target, _ := st.GetUserByEmail(context.Background(), "viewer@corp.io")

	resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", target.ID), admin,
		map[string]string{"name": "Promoted", "role": "admin", "password": "short"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	after, err := st.GetUser(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != core.RoleViewer {
		t.Fatalf("role = %q, want viewer: the rejected request still committed it", after.Role)
	}
	if after.Name == "Promoted" {
		t.Fatalf("name was committed by a request that returned 400")
	}
}

// TestAdminSelfPasswordResetKeepsCurrentSession: resetting your own password
// from the user modal should no more log you out than POST /me/password does.
func TestAdminSelfPasswordResetKeepsCurrentSession(t *testing.T) {
	srv, st := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	self, _ := st.GetUserByEmail(context.Background(), "admin@corp.io")
	other := loginAs(t, srv, core.RoleAdmin)

	resp := doJSON(t, srv, http.MethodPatch, fmt.Sprintf("/api/users/%d", self.ID), admin,
		map[string]string{"password": "another long passphrase"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	mine := doJSON(t, srv, http.MethodGet, "/api/ping", admin, nil)
	mine.Body.Close()
	if mine.StatusCode != http.StatusOK {
		t.Fatalf("own session died on a self reset: %d", mine.StatusCode)
	}
	// Every other session of that account still goes, as for any reset.
	rest := doJSON(t, srv, http.MethodGet, "/api/ping", other, nil)
	rest.Body.Close()
	if rest.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session survived the reset: %d", rest.StatusCode)
	}
}

// --- store failures are not credential failures --------------------------

// TestLoginStoreFailureIsNotACredentialFailure: a broken store must not answer
// 401, because that answer is also counted by the throttle and would lock the
// email and the IP out over an outage the caller did not cause.
func TestLoginStoreFailureIsNotACredentialFailure(t *testing.T) {
	srv, st := authServer(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, srv, http.MethodPost, "/api/login", "", map[string]string{
		"email": "admin@corp.io", "password": testPassword,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a store failure", resp.StatusCode)
	}
}

// TestSessionStoreFailureDoesNotSignOut: the SPA deletes its stored token on
// any 401, so a transient store error answered as one signs every user out for
// real rather than failing a single request.
func TestSessionStoreFailureDoesNotSignOut(t *testing.T) {
	srv, st := authServer(t)
	tok := loginAs(t, srv, core.RoleViewer)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, srv, http.MethodGet, "/api/me", tok, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a store failure", resp.StatusCode)
	}
}

// TestUnknownAPIPathRequiresAuth: 404 and 405 answered before authentication
// let an anonymous caller map out which routes and methods exist.
func TestUnknownAPIPathRequiresAuth(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/nope"},
		{http.MethodGet, "/api/login"}, // wrong method on a real route
	} {
		resp := doJSON(t, srv, tc.method, tc.path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	// Each к is two bytes, so a 5-byte cut lands mid-rune.
	got := truncate("кккк", 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != "кк" {
		t.Fatalf("truncate = %q, want the last whole rune kept", got)
	}
	if truncate("abc", 10) != "abc" {
		t.Fatalf("truncate shortened a string under the cap")
	}
}

func TestUserDTONeverCarriesHash(t *testing.T) {
	srv, _ := authServer(t)
	admin := loginAs(t, srv, core.RoleAdmin)
	resp := doJSON(t, srv, http.MethodGet, "/api/users", admin, nil)
	defer resp.Body.Close()
	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 3 {
		t.Fatalf("users = %d, want 3", len(raw))
	}
	for _, u := range raw {
		for k := range u {
			if strings.Contains(k, "password") || strings.Contains(k, "hash") {
				t.Fatalf("user JSON exposes %q", k)
			}
		}
	}
}
