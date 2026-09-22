package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/auth"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// Session lifetime. The window slides on use (touchInterval), but never past
// sessionMaxAge from creation — otherwise a client that polls the dashboard
// keeps one session alive forever and "log out everywhere" is the only way to
// ever end it.
const (
	sessionTTL    = 30 * 24 * time.Hour
	sessionMaxAge = 90 * 24 * time.Hour
	touchInterval = time.Minute
)

// principal is who is making the request. Exactly one of the two paths from
// TZ §3 produced it: the static DUSKRUN_API_TOKEN (a machine account, always
// admin, no session row) or a session token belonging to a user.
type principal struct {
	Static  bool // authenticated by DUSKRUN_API_TOKEN
	Role    core.Role
	User    *core.User    // nil when Static
	Session *core.Session // nil when Static
}

// IsAdmin is the common check for "may act on other people's things".
func (p *principal) IsAdmin() bool { return p != nil && p.Role.AtLeast(core.RoleAdmin) }

// UserID is the acting user's id, or 0 for the static token.
func (p *principal) UserID() int64 {
	if p == nil || p.User == nil {
		return 0
	}
	return p.User.ID
}

// Label identifies the principal in logs and in the UI. The static token is
// reported as such rather than dressed up as a person.
func (p *principal) Label() string {
	if p == nil {
		return "anonymous"
	}
	if p.Static {
		return "api-token"
	}
	return p.User.Email
}

type principalCtxKey struct{}

func withPrincipal(ctx context.Context, p *principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// principalFrom returns the authenticated caller, or nil on an unauthenticated
// request. Handlers behind authenticate can rely on it being non-nil.
func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalCtxKey{}).(*principal)
	return p
}

// bearer extracts the token from an Authorization header, "" when absent or
// not a Bearer scheme.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// authenticate resolves the caller and rejects when it cannot.
//
// Both paths use the same header, so no client change is needed. The static
// token is checked first and in constant time; anything else is looked up as a
// session. When no token is configured AND no users exist the API is closed —
// the original fail-closed behaviour, kept so an unconfigured deployment is
// never wide open.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		if s.d.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.d.Token)) == 1 {
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), &principal{
				Static: true, Role: core.RoleAdmin,
			})))
			return
		}

		p, err := s.resolveSession(r, tok)
		if err != nil {
			// Only "this token identifies no live session" is a 401. The SPA
			// treats 401 as proof the credential is dead and deletes it from
			// localStorage, so answering one on a transient store failure would
			// sign every current user out for real.
			if !errors.Is(err, errNoSession) {
				s.d.Log.Error("resolve session", "err", err)
				writeError(w, http.StatusServiceUnavailable, "user store unavailable")
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// errNoSession marks the "this token identifies no live session" outcomes —
// unknown, expired, or belonging to a disabled account. Any other error out of
// resolveSession is an infrastructure failure, and the two must not be conflated
// because only the first one may answer 401.
var errNoSession = errors.New("no live session for this token")

// resolveSession validates a session token and slides its expiry.
func (s *server) resolveSession(r *http.Request, tok string) (*principal, error) {
	if s.d.Store == nil {
		// Not a failure to look the token up: without a store no session can
		// exist, so a token that is not the static one is definitively invalid.
		return nil, fmt.Errorf("%w: no session store", errNoSession)
	}
	ctx := r.Context()
	sess, user, err := s.d.Store.SessionByToken(ctx, auth.TokenHash(tok))
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			return nil, errNoSession
		}
		return nil, err
	}

	now := s.now()
	if !now.Before(sess.ExpiresAt) {
		// Lazy cleanup: the row is dead weight and would otherwise sit until
		// the next daemon start.
		_ = s.d.Store.DeleteSession(ctx, sess.ID)
		return nil, fmt.Errorf("%w: expired", errNoSession)
	}
	// Checked on every request, not just at login: disabling an account has to
	// take effect immediately, without waiting for the session to lapse.
	if user.Disabled {
		return nil, fmt.Errorf("%w: user disabled", errNoSession)
	}

	if now.Sub(sess.LastSeenAt) >= touchInterval {
		if err := s.d.Store.TouchSession(ctx, sess.ID, now, slideExpiry(sess.CreatedAt, now)); err != nil {
			// A failed touch must not deny an otherwise valid request — the
			// worst case is the session expiring on its old schedule.
			s.d.Log.Warn("touch session", "session", sess.ID, "err", err)
		}
	}
	return &principal{Role: user.Role, User: user, Session: sess}, nil
}

// slideExpiry extends a session by sessionTTL from now, clamped to
// sessionMaxAge from when it was created.
func slideExpiry(created, now time.Time) time.Time {
	exp := now.Add(sessionTTL)
	if cap := created.Add(sessionMaxAge); exp.After(cap) {
		return cap
	}
	return exp
}

// sessionAlive re-checks a long-lived connection's authorisation. SSE streams
// are authorised once, at accept, and then hold for hours; without this a
// revoked session or a disabled user would keep receiving run output until they
// closed the tab. Called from the stream heartbeat.
func (s *server) sessionAlive(ctx context.Context, p *principal) bool {
	if p == nil {
		return false
	}
	if p.Static || p.Session == nil || s.d.Store == nil {
		return true // the static token has no session to revoke
	}
	sess, user, err := s.d.Store.SessionByToken(ctx, p.Session.TokenHash)
	if err != nil {
		return false
	}
	return !user.Disabled && s.now().Before(sess.ExpiresAt)
}

// guard wraps a handler with the minimum role it requires.
func guard(want core.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !p.Role.AtLeast(want) {
			writeError(w, http.StatusForbidden, "forbidden: requires role "+string(want))
			return
		}
		h(w, r)
	}
}

// route registers a handler and records the role it demands.
//
// Roles are attached per route, not per group: the register* functions are
// grouped by handler kind, and those groups cut across the permission matrix
// (PATCH /notifiers/{id} is operator while POST /notifiers is admin). A
// group-level middleware would silently over- or under-grant. The recorded map
// also lets TestEveryRouteHasARole catch a new endpoint added without a role.
func (s *server) route(r chi.Router, method, pattern string, role core.Role, h http.HandlerFunc) {
	s.roles[method+" "+pattern] = role
	r.Method(method, pattern, guard(role, h))
}

func (s *server) get(r chi.Router, pattern string, role core.Role, h http.HandlerFunc) {
	s.route(r, http.MethodGet, pattern, role, h)
}

func (s *server) post(r chi.Router, pattern string, role core.Role, h http.HandlerFunc) {
	s.route(r, http.MethodPost, pattern, role, h)
}

func (s *server) patch(r chi.Router, pattern string, role core.Role, h http.HandlerFunc) {
	s.route(r, http.MethodPatch, pattern, role, h)
}

func (s *server) del(r chi.Router, pattern string, role core.Role, h http.HandlerFunc) {
	s.route(r, http.MethodDelete, pattern, role, h)
}

// --- login throttling ---------------------------------------------------

// loginLimiter counts recent failures per key and both delays and eventually
// blocks. argon2 alone only makes guessing expensive; it does not stop a
// patient attacker, and the cost lands on this server either way.
type loginLimiter struct {
	mu      sync.Mutex
	fails   map[string]*loginFails
	now     func() time.Time
	window  time.Duration
	maxFail int
}

type loginFails struct {
	n    int
	last time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{fails: map[string]*loginFails{}, now: now, window: 15 * time.Minute, maxFail: 10}
}

// blocked reports whether the key is locked out, and for how long.
func (l *loginLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	if f == nil {
		return false, 0
	}
	now := l.now()
	if now.Sub(f.last) >= l.window {
		delete(l.fails, key)
		return false, 0
	}
	if f.n < l.maxFail {
		return false, 0
	}
	return true, l.window - now.Sub(f.last)
}

// fail records a failed attempt and returns the delay to apply before
// answering. The delay grows with consecutive failures but stays bounded, so a
// flood costs the attacker latency without pinning this process indefinitely.
func (l *loginLimiter) fail(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now, worst := l.now(), 0
	for _, k := range keys {
		f := l.fails[k]
		if f == nil || now.Sub(f.last) >= l.window {
			f = &loginFails{}
			l.fails[k] = f
		}
		f.n++
		f.last = now
		if f.n > worst {
			worst = f.n
		}
	}
	d := time.Duration(worst) * 100 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// succeed clears the counters for the keys of a successful login.
func (l *loginLimiter) succeed(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}

// sweep drops counters older than the window so the map cannot grow without
// bound under a flood of distinct emails.
func (l *loginLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, f := range l.fails {
		if now.Sub(f.last) >= l.window {
			delete(l.fails, k)
		}
	}
}

// clientIP is the throttling key for the caller's address.
//
// Behind a reverse proxy RemoteAddr is the proxy, so an IP limit would lock out
// every user at once — hence X-Forwarded-For is honoured only when the operator
// has said the deployment is behind a trusted proxy. Returns "" when no usable
// address is available, and the caller then throttles by email alone.
//
// The right-most entry is taken, not the left-most: nginx's standard
// `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for` appends to
// whatever the client sent, so everything to the left of the last hop is
// attacker-controlled. Honouring it would let one caller mint a fresh throttling
// key per request and never trip the IP limit at all.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndex(xff, ","); i >= 0 {
				return strings.TrimSpace(xff[i+1:])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// --- argon2 admission control -------------------------------------------

// pwSemSize bounds concurrent argon2 hashing. Each call reserves 64 MiB, and
// POST /login is unauthenticated: without a cap, a few dozen parallel requests
// with distinct emails (which per-email throttling does not merge) exhaust
// container memory. Queued callers wait; callers that wait too long get 503.
func pwSemSize() int {
	if n := runtime.GOMAXPROCS(0); n < 4 {
		return n
	}
	return 4
}

const pwWait = 5 * time.Second

// acquirePW takes a hashing slot, or reports false if the queue is too long.
func (s *server) acquirePW(ctx context.Context) bool {
	t := time.NewTimer(pwWait)
	defer t.Stop()
	select {
	case s.pwSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

func (s *server) releasePW() { <-s.pwSem }

// storeErrStatus maps store sentinels onto HTTP statuses shared by the user and
// session handlers.
func storeErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, sqlite.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, sqlite.ErrLastAdmin):
		return http.StatusConflict, "refusing to leave the instance without an enabled admin"
	case errors.Is(err, sqlite.ErrSelfTarget):
		return http.StatusConflict, "cannot apply this change to your own account"
	default:
		return http.StatusInternalServerError, err.Error()
	}
}
