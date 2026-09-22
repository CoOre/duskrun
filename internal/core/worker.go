package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// ErrNoQueuedRun signals an empty queue: ClaimNext found nothing to hand out.
// The store aliases this so the worker can match it across the package boundary.
var ErrNoQueuedRun = errors.New("core: no queued run")

// (Channel construction lives in Notifications: it resolves configured channels
// from the store and decrypts their secret refs, so the worker, the retention
// sweeper and the watchdog all deliver the same way.)

// WorkerStore is the store surface a Pool needs: the Executor's needs plus the
// queue operations (claim, task lookup, requeue). *sqlite.Store satisfies it.
type WorkerStore interface {
	ExecutorStore
	ClaimNext(ctx context.Context, worker string) (*Run, error)
	GetTask(ctx context.Context, id int64) (*Task, error)
	Requeue(ctx context.Context, taskID int64, nextAttempt, maxAttempts int) (int64, error)
}

// PoolConfig configures a worker Pool. Zero values fall back to sane defaults.
type PoolConfig struct {
	Workers int                             // number of concurrent workers (default 1)
	Idle    time.Duration                   // poll interval when the queue is empty (default 500ms)
	Backoff func(attempt int) time.Duration // retry delay (default core.Backoff); tests inject 0
	Now     func() time.Time                // clock for event timestamps (default time.Now)
	// Notify delivers run events to the task's configured channels. Nil disables
	// delivery — the run itself is unaffected either way.
	Notify *Notifications
}

// Pool runs N workers that claim queued runs and execute them via the shared
// Executor, honouring per-task timeouts and retrying failed runs with backoff.
type Pool struct {
	store   WorkerStore
	exec    *Executor
	workers int
	idle    time.Duration
	backoff func(attempt int) time.Duration
	now     func() time.Time
	notify  *Notifications
	log     *slog.Logger
}

// NewPool builds a Pool. exec must be constructed over the same store.
func NewPool(store WorkerStore, exec *Executor, cfg PoolConfig, log *slog.Logger) *Pool {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Idle <= 0 {
		cfg.Idle = 500 * time.Millisecond
	}
	if cfg.Backoff == nil {
		cfg.Backoff = Backoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		store: store, exec: exec, workers: cfg.Workers,
		idle: cfg.Idle, backoff: cfg.Backoff, now: cfg.Now,
		notify: cfg.Notify, log: log,
	}
}

// Run starts the workers and blocks until ctx is cancelled, then waits for all
// workers to finish their current run and returns. Graceful shutdown.
func (p *Pool) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		name := fmt.Sprintf("w%d", i)
		go func() {
			defer wg.Done()
			p.loop(ctx, name)
		}()
	}
	wg.Wait()
	return nil
}

func (p *Pool) loop(ctx context.Context, worker string) {
	for {
		if ctx.Err() != nil {
			return
		}
		did, err := p.ProcessNext(ctx, worker)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			p.log.Error("worker: claim error", "worker", worker, "err", err)
			p.sleep(ctx, p.idle)
		case !did:
			p.sleep(ctx, p.idle) // queue empty — poll
		}
	}
}

// ProcessNext claims one queued run and executes it. It returns false (nil err)
// when the queue is empty. Exposed for deterministic tests that drive one run at
// a time without spinning up goroutines.
func (p *Pool) ProcessNext(ctx context.Context, worker string) (bool, error) {
	run, err := p.store.ClaimNext(ctx, worker)
	if errors.Is(err, ErrNoQueuedRun) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p.execute(ctx, worker, run)
	return true, nil
}

func (p *Pool) execute(ctx context.Context, worker string, run *Run) {
	task, err := p.store.GetTask(ctx, run.TaskID)
	if err != nil {
		_ = p.store.Finish(context.WithoutCancel(ctx), run.ID, StatusFailed, "", "load task: "+err.Error())
		p.log.Error("worker: load task failed", "worker", worker, "run", run.ID, "err", err)
		return
	}

	// Per-run timeout: a stuck dump must not pin a worker forever.
	runCtx := ctx
	var cancel context.CancelFunc
	if task.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, task.Timeout)
	}
	_, runErr := p.exec.Run(runCtx, task, run.ID)
	if cancel != nil {
		cancel()
	}

	if runErr == nil {
		p.log.Info("worker: run success", "worker", worker, "run", run.ID, "task", task.Name)
		p.notify.Send(ctx, task, plugin.EventSuccess, run.ID, "backup completed")
		return
	}
	p.log.Error("worker: run failed", "worker", worker, "run", run.ID, "task", task.Name, "err", runErr)

	// Executor already recorded the run as failed. Retry if attempts remain;
	// only notify a FAILURE once retries are exhausted (the run is terminally lost).
	maxAttempts := task.Retries + 1
	nextAttempt := run.Attempt + 1
	if nextAttempt > maxAttempts {
		p.notify.Send(ctx, task, plugin.EventFailure, run.ID, runErr.Error())
		return
	}
	if d := p.backoff(run.Attempt); d > 0 {
		p.sleep(ctx, d)
	}
	if _, err := p.store.Requeue(context.WithoutCancel(ctx), task.ID, nextAttempt, maxAttempts); err != nil {
		p.log.Error("worker: requeue failed", "worker", worker, "task", task.Name, "err", err)
	}
}

// sleep waits d or until ctx is cancelled, whichever comes first.
func (p *Pool) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
