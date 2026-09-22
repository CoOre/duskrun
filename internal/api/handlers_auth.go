package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// registerAuthRoutes wires logout, identity, self-service password change and
// the session list. POST /login is registered separately in NewRouter — it is
// the only route that must be reachable without a credential.
func (s *server) registerAuthRoutes(r chi.Router) {
	s.post(r, "/logout", core.RoleViewer, s.logout)
	s.get(r, "/me", core.RoleViewer, s.me)
	s.post(r, "/me/password", core.RoleViewer, s.changeOwnPassword)
	s.get(r, "/sessions", core.RoleViewer, s.listSessions)
	s.del(r, "/sessions/{id}", core.RoleViewer, s.deleteSession)
}

// userDTO is the safe JSON shape of an account — never password_hash.
type userDTO struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func toUserDTO(u core.User) userDTO {
	return userDTO{
		ID: u.ID, Email: u.Email, Name: u.Name, Role: string(u.Role),
		Disabled: u.Disabled, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
}

// meDTO is what the sidebar renders. Static is true when the caller came in on
// DUSKRUN_API_TOKEN: the UI says so plainly instead of inventing a person.
type meDTO struct {
	Static    bool      `json:"static"`
	Role      string    `json:"role"`
	Email     string    `json:"email,omitempty"`
	Name      string    `json:"name,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	SessionID int64     `json:"session_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	out := meDTO{Static: p.Static, Role: string(p.Role)}
	if p.User != nil {
		out.Email, out.Name, out.UserID = p.User.Email, p.User.Name, p.User.ID
	}
	if p.Session != nil {
		out.SessionID, out.ExpiresAt = p.Session.ID, p.Session.ExpiresAt
	}
	writeJSON(w, http.StatusOK, out)
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      userDTO   `json:"user"`
}

// login exchanges email + password for a session token.
//
// Failures are deliberately uniform: a wrong password and an unknown email
// return the same status and message, and both pay the same argon2 cost (see
// auth.VerifyDummy), so the endpoint cannot be used to discover which addresses
// have accounts.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	keys := []string{"email:" + email}
	if ip := clientIP(r, s.d.TrustProxy); ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	for _, k := range keys {
		if blocked, retry := s.limiter.blocked(k); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
			return
		}
	}
	s.limiter.sweep()

	ctx := r.Context()
	if !s.acquirePW(ctx) {
		writeError(w, http.StatusServiceUnavailable, "server busy, retry shortly")
		return
	}
	user, err := s.d.Store.GetUserByEmail(ctx, email)
	// A lookup that failed for any reason other than "no such row" says nothing
	// about the credential, so it must not be answered as one: counting it as a
	// wrong password would let a few seconds of store trouble lock out the email
	// and the IP for the full window.
	lookupErr := err
	if errors.Is(lookupErr, sqlite.ErrNotFound) {
		lookupErr = nil
	}
	ok := false
	switch {
	case err == nil && !user.Disabled:
		ok, err = auth.Verify(user.PasswordHash, req.Password)
		if err != nil {
			s.d.Log.Error("login: stored hash unreadable", "user", user.ID, "err", err)
			ok = false
		}
	case lookupErr != nil:
		// Answering 503 rather than 401, so there is no 401 to equalise here.
	default:
		// No such user, or a disabled one. Burn the same work so the answer
		// takes comparable time either way.
		auth.VerifyDummy(req.Password)
	}
	s.releasePW()

	if lookupErr != nil {
		s.d.Log.Error("login: user lookup", "err", lookupErr)
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}

	if !ok {
		delay := s.limiter.fail(keys...)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	s.limiter.succeed(keys...)

	token, err := auth.NewSessionToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := s.now()
	sess := core.Session{
		UserID:     user.ID,
		TokenHash:  auth.TokenHash(token),
		UserAgent:  truncate(r.UserAgent(), 200),
		IP:         clientIP(r, s.d.TrustProxy),
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(sessionTTL),
	}
	if _, err := s.d.Store.CreateSession(ctx, sess); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.d.Store.TouchUserLogin(ctx, user.ID, now); err != nil {
		// Cosmetic field; a failure here must not cost the user their login.
		s.d.Log.Warn("stamp last_login_at", "user", user.ID, "err", err)
	}

	// The token is returned exactly once — only its hash is persisted.
	writeJSON(w, http.StatusOK, loginResp{Token: token, ExpiresAt: sess.ExpiresAt, User: toUserDTO(*user)})
}

// logout ends the calling session. The static token has no session, so it
// succeeds as a no-op rather than erroring: the SPA calls this on every logout
// and must not be blocked by it.
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.Session != nil && s.d.Store != nil {
		if err := s.d.Store.DeleteSession(r.Context(), p.Session.ID); err != nil && !errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// changeOwnPassword lets any role rotate their own password.
//
// The old password is required even though the caller is already authenticated:
// the session could have been taken (the token lives in localStorage, TZ §5.5),
// and without this check stealing it would be enough to lock the owner out.
func (s *server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.User == nil {
		writeError(w, http.StatusBadRequest, "the static API token has no password to change")
		return
	}
	var req changePasswordReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if !s.acquirePW(ctx) {
		writeError(w, http.StatusServiceUnavailable, "server busy, retry shortly")
		return
	}
	ok, err := auth.Verify(p.User.PasswordHash, req.OldPassword)
	var hash string
	if err == nil && ok {
		hash, err = auth.Hash(req.NewPassword)
	}
	s.releasePW()

	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "current password is incorrect")
		return
	}

	// Keep this session alive, drop the rest: a password change is how a user
	// reacts to a suspected leak, and it should not log them out of the tab
	// they are changing it from.
	keep := int64(0)
	if p.Session != nil {
		keep = p.Session.ID
	}
	if err := s.d.Store.SetUserPassword(ctx, p.User.ID, hash, keep); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// sessionDTO never carries token_hash: it is not a secret worth leaking, but it
// is also of no use to a client, and shipping it invites treating it as an id.
type sessionDTO struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Email      string    `json:"email,omitempty"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// listSessions returns the caller's own sessions; an admin gets every session,
// each labelled with its owner. Filtering rather than forbidding keeps the
// settings page useful to non-admins, who otherwise could not see or end their
// own logins.
func (s *server) listSessions(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	p := principalFrom(r.Context())
	ctx := r.Context()

	var owner int64
	if !p.IsAdmin() {
		owner = p.UserID()
		if owner == 0 {
			writeJSON(w, http.StatusOK, []sessionDTO{})
			return
		}
	}
	sessions, err := s.d.Store.ListSessions(ctx, owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	emails := map[int64]string{}
	if p.IsAdmin() {
		users, err := s.d.Store.ListUsers(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, u := range users {
			emails[u.ID] = u.Email
		}
	}

	cur := int64(0)
	if p.Session != nil {
		cur = p.Session.ID
	}
	out := make([]sessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionDTO{
			ID: sess.ID, UserID: sess.UserID, Email: emails[sess.UserID],
			UserAgent: sess.UserAgent, IP: sess.IP,
			CreatedAt: sess.CreatedAt, LastSeenAt: sess.LastSeenAt, ExpiresAt: sess.ExpiresAt,
			Current: sess.ID == cur,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteSession ends one session. Ending your own last session is allowed —
// that is the deliberate "log out everywhere".
func (s *server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "user store unavailable")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	ctx := r.Context()

	sess, err := s.d.Store.GetSession(ctx, id)
	if err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	// 403 rather than 404: session ids are sequential and not secret, so
	// pretending the row is missing would only confuse an honest caller.
	if !p.IsAdmin() && sess.UserID != p.UserID() {
		writeError(w, http.StatusForbidden, "forbidden: not your session")
		return
	}
	if err := s.d.Store.DeleteSession(ctx, id); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// truncate caps a string at n bytes without splitting a UTF-8 sequence, so a
// cut user agent stays valid text rather than becoming a replacement char.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
