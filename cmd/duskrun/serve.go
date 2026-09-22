package main

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/duskrun/duskrun/internal/api"
	"github.com/duskrun/duskrun/internal/config"
	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/secret"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// cmdServe runs the daemon: scheduler (dispatcher on a 30s ticker) + worker pool
// + a minimal HTTP /healthz. It reaps stale runs on startup and shuts down
// gracefully on SIGINT/SIGTERM.
func cmdServe(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The daemon is the exposed path, so it checks the API token too — not just
	// that a master key exists.
	if err := cfg.RequireUsableCredentials(); err != nil {
		return err
	}
	box, err := secret.NewBox(cfg.MasterKey, "env:v1")
	if err != nil {
		return err
	}

	st, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Startup housekeeping (TZ §7 misfire): free runs orphaned by a crash.
	reaped, err := st.ReapStale(ctx)
	if err != nil {
		return err
	}
	// Sessions have no background reaper of their own: expired rows are dropped
	// here and lazily when one is presented, which is enough for a table that
	// grows by one row per login.
	expired, err := st.DeleteExpiredSessions(ctx, time.Now())
	if err != nil {
		return err
	}
	users, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	log.Info("store ready",
		"db", cfg.DBPath,
		"workers", cfg.WorkerLimit,
		"reaped_stale_runs", len(reaped),
		"expired_sessions", expired,
		"users", users,
	)
	if users == 0 && cfg.APIToken == "" {
		// Neither way in exists: say so at startup rather than letting every
		// request answer 401 with no explanation.
		log.Warn("no users and no DUSKRUN_API_TOKEN — the API will reject everything; run `duskrun user add`")
	}

	// The Hub carries live run progress (phases, bytes, log lines) from the
	// executor to the SSE endpoint. In-memory only; nothing durable lives here.
	hub := core.NewHub(time.Now)
	exec := core.NewExecutor(st, box, time.Now)
	exec.SetPublisher(hub)
	// One delivery service for every event source: worker, sweeper and watchdog
	// resolve the same configured channels and record the same delivery log.
	notes := core.NewNotifications(st, box, core.NotificationsConfig{Now: time.Now}, log)

	disp := core.NewDispatcher(st, time.Now, log)
	pool := core.NewPool(st, exec, core.PoolConfig{Workers: cfg.WorkerLimit, Notify: notes}, log)
	// Retention runs on its own schedule, independent of the dispatcher tick:
	// without it artifacts accumulate forever regardless of each task's policy.
	sweeper := core.NewSweeper(st, core.SweeperConfig{
		Now: time.Now, Notify: notes,
		// The sweep opens the same storages a run writes to, so it needs the same
		// secret resolution: without it retention would fail on every destination
		// whose credentials live in the secret store.
		Secrets: core.NewSecretResolver(st, box),
	}, log)
	// The watchdog is what turns "the backup silently stopped happening" into a
	// notification instead of something nobody notices until a restore.
	watchdog := core.NewWatchdog(st, core.WatchdogConfig{Now: time.Now, Notify: notes}, log)

	// The HTTP surface is the API router: /healthz (public), token-guarded /api/*,
	// and the embedded SPA with client-routing fallback — all on one listener.
	router := api.NewRouter(api.Deps{
		Store:   st,
		Token:   cfg.APIToken,
		Log:     log,
		Exec:    exec,
		Secrets: box,
		Hub:     hub,
		Sweeper: sweeper,
		// Surfaced on the retention page so the UI can show when the next
		// sweep lands without re-deriving the schedule.
		RetentionCron: cfg.RetentionCron,
		Watchdog:      watchdog,
		Notify:        notes,
		TrustProxy:    cfg.TrustProxy,
		Now:           time.Now,
		Instance: api.InstanceInfo{
			Version:       resolveVersion(),
			DBPath:        cfg.DBPath,
			Workers:       cfg.WorkerLimit,
			RetentionCron: cfg.RetentionCron,
			ListenAddr:    cfg.ListenAddr,
		},
	})
	eng := core.NewEngine(disp, pool, core.EngineConfig{
		ListenAddr:    cfg.ListenAddr,
		TickInterval:  30 * time.Second,
		Handler:       router,
		Sweeper:       sweeper,
		RetentionCron: cfg.RetentionCron,
		Watchdog:      watchdog,
	}, log)

	return eng.Run(ctx)
}
