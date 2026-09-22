// Package api is the minimal REST surface (M5): a chi router with bearer-token
// auth over the metadata store. /healthz is unauthenticated; everything under
// /api requires the token.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
	"github.com/duskrun/duskrun/internal/web"
)

// Deps are the API server's dependencies.
type Deps struct {
	Store *sqlite.Store
	Token string         // DUSKRUN_API_TOKEN
	Log   *slog.Logger   // optional
	Exec  *core.Executor // optional (run-now); may be nil
	// Hub streams live run progress to GET /runs/{id}/events. Nil disables the
	// live path — the endpoint then only replays terminal runs from the store.
	Hub *core.Hub
	// Sweeper backs POST /retention/sweep. Nil makes the endpoint return 503;
	// the read-only retention routes still work.
	Sweeper *core.Sweeper
	// RetentionCron is the sweep schedule, reported by GET /retention so the UI
	// can show the next sweep. Empty means the scheduled sweep is off.
	RetentionCron string
	// Watchdog backs GET /watchdog. Nil makes the endpoint return 503.
	Watchdog *core.Watchdog
	// Notify backs POST /notifiers/{id}/test, which builds a channel the same
	// way a real delivery does. Nil makes that endpoint return 503; the channel
	// CRUD endpoints still work.
	Notify *core.Notifications
	// Secrets seals plaintext secret values for storage (POST /secrets). Nil
	// disables the create endpoint (503) — e.g. when no master key is configured.
	Secrets Sealer
	// ToolCheck validates that the external CLI a task's engine needs is present.
	// Nil defaults to a real PATH check (task creation returns 400 when the tool
	// is missing). Tests inject a permissive or always-failing stub.
	ToolCheck func(engine string) error
	// DatabaseList performs a live best-effort database catalog lookup for the
	// selected connection. Nil defaults to the built-in postgres/mysql lister.
	DatabaseList func(ctx context.Context, conn core.Connection) ([]string, error)
	// ConnectionCheck runs a staged live probe for POST /connections/test,
	// returning the databases or a *connCheckError identifying the failed stage.
	// Nil defaults to the built-in staged checker (connector → auth → catalog).
	ConnectionCheck func(ctx context.Context, conn core.Connection) ([]string, error)
	// TrustProxy makes login throttling honour X-Forwarded-For. Off by default:
	// trusting the header when nothing sets it lets a caller pick its own
	// throttling bucket, and honouring RemoteAddr behind a proxy would throttle
	// every user as one. See clientIP.
	TrustProxy bool
	// Now is the clock, injectable for tests. Nil defaults to time.Now.
	Now func() time.Time
	// Instance describes the read-only "Instance" block of the settings page:
	// values that come from the environment and cannot be changed over HTTP.
	Instance InstanceInfo
}

// InstanceInfo is the read-only deployment description shown on the settings
// page. It is reported, never written: these come from env vars and a restart.
type InstanceInfo struct {
	Version       string
	DBPath        string
	Workers       int
	RetentionCron string
	ListenAddr    string
}

// Sealer encrypts a plaintext secret value into a persistable core.Secret.
// *secret.Box implements it; the interface keeps the api package free of a
// direct dependency on package secret.
type Sealer interface {
	Seal(name, typ string, plaintext []byte) (core.Secret, error)
}

// defaultToolCheck enforces that the engine's required external tool is on PATH.
func defaultToolCheck(engine string) error {
	tool := core.RequiredTool(engine)
	if tool == "" {
		return nil
	}
	if _, err := core.CheckTool(tool); err != nil {
		return fmt.Errorf("required tool %q for engine %q is not installed", tool, engine)
	}
	return nil
}

// NewRouter builds the HTTP handler. /healthz is public; /api/* is guarded by
// either the static token or a session (see authenticate).
func NewRouter(d Deps) http.Handler {
	h, _ := newRouter(d)
	return h
}

// newRouter is NewRouter plus the server it built, so the role-coverage test can
// compare the recorded route roles against what chi actually registered.
func newRouter(d Deps) (http.Handler, *server) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ToolCheck == nil {
		d.ToolCheck = defaultToolCheck
	}
	// The Sealer that writes secrets also opens them (*secret.Box does both), so
	// the resolver every plugin config goes through is built from it here rather
	// than passed separately.
	opener, _ := d.Secrets.(core.SecretOpener)
	res := core.NewSecretResolver(d.Store, opener)
	if d.DatabaseList == nil {
		d.DatabaseList = func(ctx context.Context, conn core.Connection) ([]string, error) {
			return listDatabases(ctx, res, conn)
		}
	}
	if d.ConnectionCheck == nil {
		toolCheck := d.ToolCheck
		d.ConnectionCheck = func(ctx context.Context, conn core.Connection) ([]string, error) {
			return checkConnection(ctx, res, toolCheck, conn)
		}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &server{
		d:       d,
		res:     res,
		roles:   map[string]core.Role{},
		pwSem:   make(chan struct{}, pwSemSize()),
		limiter: newLoginLimiter(d.Now),
	}

	r := chi.NewRouter()
	r.Get("/healthz", core.HealthzHandler())

	r.Route("/api", func(r chi.Router) {
		// Bound every request body before anything reads it. Individual
		// handlers could each wrap their own decoder, but then the protection
		// is only as good as the next handler someone adds; here it cannot be
		// forgotten. It sits above authenticate deliberately, so an anonymous
		// caller cannot spend the server's memory on POST /login either.
		r.Use(limitBody)

		// POST /login is the one public route under /api: it is how a caller
		// obtains the credential every other route demands, so it must sit
		// outside the authentication middleware rather than under it.
		r.Post("/login", s.login)

		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)

			// Unknown /api paths return a JSON 404, not the SPA's HTML.
			// Registered inside this group so they inherit authenticate: chi
			// wraps a Group's fallback handlers in that group's middleware, so
			// an anonymous caller gets 401 everywhere rather than a 404/405
			// that maps out which routes and methods exist.
			r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusNotFound, "not found")
			})
			r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			})

			// A trivial authenticated probe (also the auth smoke-test target).
			s.get(r, "/ping", core.RoleViewer, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			})
			s.routes(r)
		})
	})

	// The embedded SPA is mounted last: /healthz and /api/* above take
	// precedence; everything else (/, /tasks, /assets/*) hits the SPA handler,
	// which falls back to index.html for client-side routes.
	r.Handle("/*", web.Handler())
	return r, s
}

// limitBody caps how much of a request body any handler can read. Handlers that
// use decode() turn the resulting error into a 413; the few that run their own
// decoder report it as a malformed body, which is imprecise but still refuses
// the request — the point is that no handler can read without a bound.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// server holds handler dependencies.
type server struct {
	d Deps
	// res resolves `<key>_ref` in a plugin config against the secret store, so
	// the API builds a storage the same way the executor does.
	res *core.SecretResolver
	// roles records the minimum role of every registered route, keyed
	// "METHOD /pattern". Written only during NewRouter, read by the guard test.
	roles map[string]core.Role
	// pwSem bounds concurrent argon2 hashing (see pwSemSize).
	pwSem   chan struct{}
	limiter *loginLimiter
}

func (s *server) now() time.Time { return s.d.Now() }

// routes registers the authenticated /api endpoints. Read/write/action handlers
// are attached here as later milestones land.
func (s *server) routes(r chi.Router) {
	s.registerReadRoutes(r)
	s.registerWriteRoutes(r)
	s.registerDeleteRoutes(r)
	s.registerActionRoutes(r)
	s.registerRetentionRoutes(r)
	s.registerWatchdogRoutes(r)
	s.registerNotifierRoutes(r)
	s.registerAuthRoutes(r)
	s.registerUserRoutes(r)
	s.registerSettingRoutes(r)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes {"error": msg} with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
