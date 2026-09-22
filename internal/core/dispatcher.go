package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ErrAlreadyActive signals that a task already has an active (queued or running)
// run, so a second enqueue was refused by the "≤1 active run per task" invariant
// (TZ §7). The store wraps this; the dispatcher treats it as a benign skip.
var ErrAlreadyActive = errors.New("core: task already has an active run")

// DispatchStore is the slice of the metadata store the dispatcher needs. Keeping
// it an interface lets tests substitute fakes and avoids a hard dependency on
// the concrete sqlite.Store.
type DispatchStore interface {
	ListEnabledTasks(ctx context.Context) ([]Task, error)
	Enqueue(ctx context.Context, taskID int64) (int64, error)
}

// Dispatcher turns cron schedules into queued runs. On each Tick it enqueues
// every enabled task whose next activation fell within (lastTick, now]. The
// clock is injectable so tests are deterministic.
type Dispatcher struct {
	store    DispatchStore
	now      func() time.Time
	log      *slog.Logger
	lastTick time.Time
}

// NewDispatcher builds a Dispatcher. If now is nil it uses time.Now; if log is
// nil it uses slog.Default. lastTick starts at now(), so the first Tick only
// fires schedules that come due after construction — never a backfill storm.
func NewDispatcher(store DispatchStore, now func() time.Time, log *slog.Logger) *Dispatcher {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{store: store, now: now, log: log, lastTick: now()}
}

// Tick enqueues all tasks that became due since the previous Tick. A task is due
// when its next scheduled activation strictly after lastTick is at or before now.
// A refused enqueue for an already-active task is logged as a misfire=skip and
// does not fail the tick. Returns the number of runs enqueued.
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	now := d.now()
	tasks, err := d.store.ListEnabledTasks(ctx)
	if err != nil {
		return 0, err
	}

	enqueued := 0
	for _, t := range tasks {
		next, err := NextRun(t.Cron, d.lastTick)
		if err != nil {
			d.log.Error("dispatcher: bad cron, skipping task",
				"task", t.Name, "cron", t.Cron, "err", err)
			continue
		}
		if next.After(now) {
			continue // not due yet
		}
		if _, err := d.store.Enqueue(ctx, t.ID); err != nil {
			if errors.Is(err, ErrAlreadyActive) {
				// A prior run is still queued/running: misfire policy = skip.
				d.log.Info("dispatcher: task still active, skipping (misfire=skip)",
					"task", t.Name)
				continue
			}
			d.log.Error("dispatcher: enqueue failed", "task", t.Name, "err", err)
			continue
		}
		enqueued++
		d.log.Info("dispatcher: enqueued", "task", t.Name)
	}

	d.lastTick = now
	return enqueued, nil
}
