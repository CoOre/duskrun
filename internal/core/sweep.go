package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// ManualSweepTimeout bounds a sweep started from the API. It is deliberately
// generous — a large S3 catalog takes minutes — but finite, so a wedged storage
// cannot hold the sweeper's lock indefinitely.
const ManualSweepTimeout = 15 * time.Minute

// SweepStore is the store surface the Sweeper needs: the Retention Manager's
// artifact access plus task/storage lookup and sweep history. *sqlite.Store
// satisfies it.
type SweepStore interface {
	RetentionStore
	ListTasks(ctx context.Context) ([]Task, error)
	GetStorage(ctx context.Context, id int64) (*Storage, error)
	InsertSweep(ctx context.Context, s Sweep) (int64, error)
	PruneNotifications(ctx context.Context, keep int) (int64, error)
}

// SweeperConfig configures a Sweeper. Zero values fall back to sane defaults.
type SweeperConfig struct {
	Now func() time.Time // clock (default time.Now)
	// Notify delivers retention failures to the task's channels, so they reach
	// the same places a backup failure would. Nil disables delivery.
	Notify *Notifications
	// Secrets resolves `<key>_ref` in a storage config before the plugin is
	// built. Without it a storage that keeps its credentials in the secret store
	// (sftp's private key, s3's access key) cannot be opened, and the sweep would
	// fail on exactly the destinations a run writes to successfully.
	Secrets *SecretResolver
	// OpenStorage builds the storage plugin for a task's destination. Default
	// goes through the plugin registry; tests inject a fake.
	OpenStorage func(ctx context.Context, st *Storage) (plugin.Storage, error)
}

// Sweeper applies every task's retention policy in one pass and records the
// outcome as a Sweep. It is driven by the retention cron in the Engine, and by
// POST /api/retention/sweep on demand.
type Sweeper struct {
	store       SweepStore
	mgr         *RetentionManager
	now         func() time.Time
	notify      *Notifications
	openStorage func(ctx context.Context, st *Storage) (plugin.Storage, error)
	log         *slog.Logger
	// mu serialises sweeps. Two overlapping passes (the cron firing while a
	// manual sweep is mid-flight) would compute their delete sets from the same
	// catalog and then race to delete the same objects.
	mu sync.Mutex
}

// NewSweeper builds a Sweeper over store.
func NewSweeper(store SweepStore, cfg SweeperConfig, log *slog.Logger) *Sweeper {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OpenStorage == nil {
		res := cfg.Secrets
		cfg.OpenStorage = func(ctx context.Context, st *Storage) (plugin.Storage, error) {
			config, err := res.Resolve(ctx, st.Config)
			if err != nil {
				return nil, fmt.Errorf("storage config: %w", err)
			}
			return plugin.Storages.Create(st.Type, config)
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{
		store: store, mgr: NewRetentionManager(store, log),
		now: cfg.Now, notify: cfg.Notify, openStorage: cfg.OpenStorage, log: log,
	}
}

// Sweep prunes every task's artifacts per its retention policy and counts the
// orphan objects left in storage. One task's failure never aborts the pass: the
// remaining tasks are still swept and every error is folded into the recorded
// Sweep. The returned Sweep is what was persisted; the error is the same folded
// failure, so a caller that only wants the record can ignore it.
func (s *Sweeper) Sweep(ctx context.Context, source SweepSource) (Sweep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked(ctx, source)
}

// TrySweep is Sweep for callers that must not block: it reports ok=false and
// does nothing when a sweep is already running. An HTTP request would otherwise
// hang for the full duration of a scheduled sweep with no way to say so.
func (s *Sweeper) TrySweep(ctx context.Context, source SweepSource) (sw Sweep, ok bool, err error) {
	if !s.mu.TryLock() {
		return Sweep{}, false, nil
	}
	defer s.mu.Unlock()
	sw, err = s.sweepLocked(ctx, source)
	return sw, true, err
}

// sweepLocked is the sweep body; callers hold s.mu.
func (s *Sweeper) sweepLocked(ctx context.Context, source SweepSource) (Sweep, error) {
	started := s.now()

	tasks, err := s.store.ListTasks(ctx)
	if err != nil {
		// Nothing was swept and we cannot say what would have been — record the
		// failure so the history shows the gap rather than silently skipping it.
		return s.record(ctx, Sweep{StartedAt: started, Source: source}, err)
	}

	var sw Sweep
	sw.StartedAt = started
	sw.Source = source
	var errs []error

	for i := range tasks {
		task := &tasks[i]
		deleted, freed, orphans, err := s.sweepTask(ctx, task)
		sw.DeletedCount += deleted
		sw.FreedBytes += freed
		sw.OrphanCount += orphans
		if err != nil {
			errs = append(errs, fmt.Errorf("task %q: %w", task.Name, err))
			s.notifyError(ctx, task, err)
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}

	s.pruneNotifications(ctx)

	return s.record(ctx, sw, errors.Join(errs...))
}

// sweepTask prunes one task and counts its orphan objects. Pruning is skipped
// for a task with no retention policy: an empty policy means "keep everything",
// NOT "delete everything" — Forget would otherwise return every artifact as
// deletable. Orphan reporting still runs, since it only reads.
func (s *Sweeper) sweepTask(ctx context.Context, task *Task) (deleted int, freed int64, orphans int, err error) {
	open := s.storageOpener()

	var errs []error
	if task.Retention.Enabled() {
		gone, pruneErr := s.mgr.Prune(ctx, task, open, s.now())
		for _, a := range gone {
			freed += a.Size
		}
		deleted = len(gone)
		if pruneErr != nil {
			errs = append(errs, pruneErr)
		}
	}

	found, orphanErr := s.mgr.Orphans(ctx, task, open)
	if orphanErr != nil {
		errs = append(errs, fmt.Errorf("orphans: %w", orphanErr))
	}
	orphans = len(found)

	return deleted, freed, orphans, errors.Join(errs...)
}

// storageOpener adapts the store + plugin registry into the StorageOpener the
// Retention Manager resolves each artifact's own storage through.
func (s *Sweeper) storageOpener() StorageOpener {
	return func(ctx context.Context, id int64) (plugin.Storage, error) {
		stor, err := s.store.GetStorage(ctx, id)
		if err != nil {
			return nil, err
		}
		storage, err := s.openStorage(ctx, stor)
		if err != nil {
			return nil, fmt.Errorf("storage %q: %w", stor.Name, err)
		}
		return storage, nil
	}
}

// record persists the sweep with its terminal status and returns it. A failure
// to write the history row is folded into the returned error but never discards
// the sweep the caller just performed.
func (s *Sweeper) record(ctx context.Context, sw Sweep, cause error) (Sweep, error) {
	sw.FinishedAt = s.now()
	sw.Status = SweepSuccess
	if cause != nil {
		sw.Status = SweepFailed
		sw.Error = truncateErr(cause.Error(), maxSweepErrLen)
	}

	// Detached: a cancelled context must not lose the record of work already done.
	id, err := s.store.InsertSweep(context.WithoutCancel(ctx), sw)
	if err != nil {
		s.log.Error("retention: record sweep failed", "err", err)
		return sw, errors.Join(cause, err)
	}
	sw.ID = id

	s.log.Info("retention: sweep finished",
		"status", string(sw.Status), "source", string(sw.Source),
		"deleted", sw.DeletedCount, "freed_bytes", sw.FreedBytes, "orphans", sw.OrphanCount)
	return sw, cause
}

// NotificationKeep is how many delivery-log entries a sweep leaves behind. The
// log is append-only otherwise — one row per event per channel, successes
// included — so it is the sweep's job to keep it from growing without bound.
const NotificationKeep = 5000

// pruneNotifications trims the delivery log. Its failure is logged rather than
// folded into the sweep: the artifacts the sweep exists for were pruned either
// way, and a red sweep would point at the wrong thing.
func (s *Sweeper) pruneNotifications(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	gone, err := s.store.PruneNotifications(ctx, NotificationKeep)
	if err != nil {
		s.log.Error("retention: prune delivery log failed", "err", err)
		return
	}
	if gone > 0 {
		s.log.Info("retention: delivery log trimmed", "deleted", gone, "kept", NotificationKeep)
	}
}

// notifyError delivers a retention failure to the task's channels.
func (s *Sweeper) notifyError(ctx context.Context, task *Task, cause error) {
	s.notify.Send(ctx, task, plugin.EventRetentionError, 0, cause.Error())
}

// maxSweepErrLen caps the stored error text: a sweep over many broken tasks
// joins one message per task, and the history row is a summary, not a log.
const maxSweepErrLen = 2000

func truncateErr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "… (truncated)"
}
