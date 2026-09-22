package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// EngineConfig configures the serve engine.
type EngineConfig struct {
	ListenAddr   string        // HTTP listen address (default ":8080")
	TickInterval time.Duration // dispatcher tick cadence (default 30s)
	// Handler, when set, is served for all HTTP routes (the API router, which
	// already exposes /healthz, /api/* and the embedded SPA). When nil the
	// engine falls back to a minimal mux exposing only /healthz.
	Handler http.Handler
	// Sweeper applies retention on the RetentionCron schedule. Nil disables the
	// sweep loop entirely (artifacts then accumulate until swept by hand).
	Sweeper *Sweeper
	// RetentionCron is the standard 5-field cron for the retention sweep. Empty
	// disables the loop even when Sweeper is set.
	RetentionCron string
	// Watchdog reports tasks whose last successful backup has gone stale. Nil
	// disables the check.
	Watchdog *Watchdog
	// WatchdogInterval is how often the watchdog re-evaluates (default 1m). The
	// check is a two-query scan, and alerting is once-per-episode, so a short
	// interval costs little and shortens time-to-notice.
	WatchdogInterval time.Duration
}

// Engine is the long-running daemon: it drives the Dispatcher on a ticker, runs
// the worker Pool, and serves a minimal HTTP surface (/healthz). Run blocks
// until its context is cancelled, then shuts everything down gracefully.
type Engine struct {
	dispatcher *Dispatcher
	pool       *Pool
	listenAddr string
	tick       time.Duration
	handler    http.Handler
	sweeper    *Sweeper
	sweepCron  string
	watchdog   *Watchdog
	watchTick  time.Duration
	log        *slog.Logger
}

// NewEngine wires a dispatcher and pool into an Engine.
func NewEngine(dispatcher *Dispatcher, pool *Pool, cfg EngineConfig, log *slog.Logger) *Engine {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 30 * time.Second
	}
	if cfg.WatchdogInterval <= 0 {
		cfg.WatchdogInterval = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		dispatcher: dispatcher, pool: pool,
		listenAddr: cfg.ListenAddr, tick: cfg.TickInterval,
		handler: cfg.Handler,
		sweeper: cfg.Sweeper, sweepCron: cfg.RetentionCron,
		watchdog: cfg.Watchdog, watchTick: cfg.WatchdogInterval,
		log: log,
	}
}

// HTTP server timeouts. Every phase a client controls is bounded, because the
// default listen address is :8080 on every interface and an unbounded phase is
// a goroutine any single socket can hold for as long as it likes.
const (
	// readHeaderTimeout is the one that stops slowloris: a connection that
	// opens and then dribbles header bytes forever.
	readHeaderTimeout = 10 * time.Second
	// readTimeout covers a body arriving just as slowly. Generous, because
	// bodies are small and a slow client on a bad link is not an attacker.
	readTimeout = 30 * time.Second
	// idleTimeout closes kept-alive connections nobody is using.
	idleTimeout = 120 * time.Second
	// maxHeaderBytes is Go's own default, set explicitly so it is a decision.
	maxHeaderBytes = 1 << 20
)

// newHTTPServer builds the daemon's HTTP server with those bounds applied.
//
// WriteTimeout is deliberately absent. It is a deadline on the entire response,
// and two of this server's responses are legitimately long: the SSE run stream
// stays open for the length of a backup, and an artifact download takes as long
// as the storage backend and the dump size demand. A write deadline would sever
// both mid-flight, which an operator reads as a failed backup rather than as a
// timeout. Slow-read attacks against the response body are left to the reverse
// proxy, which is where a deployment exposed enough to care already terminates
// TLS.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// Run starts the HTTP server, dispatcher loop, and worker pool, and blocks until
// ctx is cancelled (SIGINT/SIGTERM upstream) or the HTTP server fails to bind.
// On exit it cancels the workers/dispatcher and drains the HTTP server.
func (e *Engine) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Validate the retention schedule before binding anything: a typo in the
	// cron must fail the daemon loudly, not silently skip every sweep.
	var sweepSchedule cron.Schedule
	if e.sweepEnabled() {
		sch, err := ParseCron(e.sweepCron)
		if err != nil {
			return fmt.Errorf("retention cron: %w", err)
		}
		sweepSchedule = sch
	}

	handler := e.handler
	if handler == nil {
		mux := http.NewServeMux()
		mux.Handle("/healthz", HealthzHandler())
		handler = mux
	}
	srv := newHTTPServer(e.listenAddr, handler)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = e.pool.Run(runCtx)
	}()
	go func() {
		defer wg.Done()
		e.dispatchLoop(runCtx)
	}()
	if sweepSchedule != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.sweepLoop(runCtx, sweepSchedule)
		}()
	}
	if e.watchdog != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.watchdogLoop(runCtx)
		}()
	}

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	e.log.Info("engine started", "listen", e.listenAddr, "tick", e.tick.String())

	var retErr error
	select {
	case <-ctx.Done():
	case err := <-srvErr:
		retErr = err // server bind/serve failed; tear the rest down
	}

	cancel() // stop pool + dispatcher

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)

	wg.Wait()
	e.log.Info("engine stopped")
	return retErr
}

// dispatchLoop calls Dispatcher.Tick every tick until ctx is cancelled.
func (e *Engine) dispatchLoop(ctx context.Context) {
	t := time.NewTicker(e.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := e.dispatcher.Tick(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				e.log.Error("dispatcher tick failed", "err", err)
			}
		}
	}
}

// sweepEnabled reports whether a retention sweep loop should run.
func (e *Engine) sweepEnabled() bool {
	return e.sweeper != nil && e.sweepCron != ""
}

// sweepLoop runs the retention sweep at each cron activation until ctx is
// cancelled. A failing sweep is logged and the loop continues — retention must
// keep trying tomorrow rather than dying on one bad storage.
func (e *Engine) sweepLoop(ctx context.Context, sch cron.Schedule) {
	e.log.Info("retention sweep scheduled", "cron", e.sweepCron, "next", sch.Next(time.Now()).UTC().Format(time.RFC3339))
	for {
		wait := time.Until(sch.Next(time.Now()))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			if _, err := e.sweeper.Sweep(ctx, SweepSchedule); err != nil {
				if ctx.Err() != nil {
					return
				}
				e.log.Error("retention sweep failed", "err", err)
			}
		}
	}
}

// watchdogLoop re-evaluates stale tasks on a ticker until ctx is cancelled. A
// failing check is logged and the loop continues: the watchdog exists to report
// problems, so it must not become one.
func (e *Engine) watchdogLoop(ctx context.Context) {
	t := time.NewTicker(e.watchTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := e.watchdog.Check(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				e.log.Error("watchdog check failed", "err", err)
			}
		}
	}
}

// HealthzHandler returns 200 "ok" — a liveness probe with no auth (used by the
// serve engine and the API router alike).
func HealthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}
}
