package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

// RetentionStore is the store surface the Retention Manager needs.
type RetentionStore interface {
	ListArtifactsByTask(ctx context.Context, taskID int64) ([]Artifact, error)
	DeleteArtifact(ctx context.Context, id int64) error
}

// Forget partitions artifacts into keep/delete per the retention policy, unioning
// keep_last (the N newest) with GFS (the freshest artifact in each of the last
// daily days / weekly ISO-weeks / monthly months). An artifact kept by ANY rule
// is kept. `now` is accepted for signature parity with a time-anchored policy but
// the buckets are derived from the artifacts themselves (restic's model).
func Forget(ret Retention, artifacts []Artifact, now time.Time) (keep, del []Artifact) {
	_ = now
	sorted := make([]Artifact, len(artifacts))
	copy(sorted, artifacts)
	// Newest first; ties broken by higher ID so ordering is deterministic.
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].ID > sorted[j].ID
		}
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	keepSet := make(map[int64]bool)

	// keep_last: the N newest overall.
	for i := 0; i < ret.KeepLast && i < len(sorted); i++ {
		keepSet[sorted[i].ID] = true
	}

	// GFS buckets.
	if ret.GFS != nil {
		mergeInto(keepSet, keepByPeriod(sorted, ret.GFS.Daily, func(t time.Time) string {
			return t.UTC().Format("2006-01-02")
		}))
		mergeInto(keepSet, keepByPeriod(sorted, ret.GFS.Weekly, func(t time.Time) string {
			y, w := t.UTC().ISOWeek()
			return fmt.Sprintf("%04d-W%02d", y, w)
		}))
		mergeInto(keepSet, keepByPeriod(sorted, ret.GFS.Monthly, func(t time.Time) string {
			return t.UTC().Format("2006-01")
		}))
	}

	for _, a := range sorted {
		if keepSet[a.ID] {
			keep = append(keep, a)
		} else {
			del = append(del, a)
		}
	}
	return keep, del
}

// keepByPeriod keeps the newest artifact in each of the most recent `count`
// distinct periods (period identified by keyFn). sorted must be newest-first.
func keepByPeriod(sorted []Artifact, count int, keyFn func(time.Time) string) map[int64]bool {
	keep := make(map[int64]bool)
	if count <= 0 {
		return keep
	}
	seen := make(map[string]bool)
	for _, a := range sorted {
		k := keyFn(a.CreatedAt)
		if seen[k] {
			continue // an earlier (newer) artifact already covers this period
		}
		seen[k] = true
		keep[a.ID] = true
		if len(seen) >= count {
			break
		}
	}
	return keep
}

func mergeInto(dst, src map[int64]bool) {
	for k := range src {
		dst[k] = true
	}
}

// RetentionManager runs forget→prune and orphan reporting against a storage.
type RetentionManager struct {
	store RetentionStore
	log   *slog.Logger
}

// NewRetentionManager builds a manager over store.
func NewRetentionManager(store RetentionStore, log *slog.Logger) *RetentionManager {
	if log == nil {
		log = slog.Default()
	}
	return &RetentionManager{store: store, log: log}
}

// StorageOpener opens the storage plugin behind a storage id. Retention needs
// one per ARTIFACT, not one per task: a task can be repointed at a new storage
// (PATCH /tasks/{id}) while its older artifacts still live in the old one.
// Deleting those against the new storage would silently succeed — both localfs
// and S3 treat a missing key as deleted — dropping the catalog row while the
// real file stays behind untracked.
type StorageOpener func(ctx context.Context, storageID int64) (plugin.Storage, error)

// storageCache memoises opened storages within a single Prune/Orphans call, so
// a hundred artifacts in one storage do not construct a hundred S3 clients.
type storageCache struct {
	open StorageOpener
	seen map[int64]plugin.Storage
}

func (c *storageCache) get(ctx context.Context, id int64) (plugin.Storage, error) {
	if s, ok := c.seen[id]; ok {
		return s, nil
	}
	s, err := c.open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("storage %d: %w", id, err)
	}
	if c.seen == nil {
		c.seen = make(map[int64]plugin.Storage)
	}
	c.seen[id] = s
	return s, nil
}

// closeAll releases every storage the cache opened. A sweep pass builds a fresh
// storage per pass, and a session-holding one (sftp) would otherwise leave its
// ssh connection behind on every tick of the cleanup cron.
func (c *storageCache) closeAll() {
	for _, s := range c.seen {
		CloseStorage(s)
	}
}

// Prune applies the task's retention policy: it computes the delete set, removes
// each artifact from the storage it was WRITTEN to, then from the catalog. It
// returns the artifacts actually deleted plus the joined failures.
//
// One unremovable object must not wedge the whole task. The delete set is
// recomputed newest-first on every sweep, so a single object under an
// object-lock (or with broken permissions) would be retried first, forever,
// and every older artifact behind it would never be considered — the storage
// would grow without bound. So a failed artifact is skipped, not fatal.
func (m *RetentionManager) Prune(ctx context.Context, task *Task, open StorageOpener, now time.Time) ([]Artifact, error) {
	artifacts, err := m.store.ListArtifactsByTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	_, del := Forget(task.Retention, artifacts, now)

	var deleted []Artifact
	var errs []error
	cache := storageCache{open: open}
	defer cache.closeAll()
	for _, a := range del {
		// A cancelled sweep would otherwise fail every remaining artifact and
		// bury the real cause under hundreds of context errors.
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		storage, err := cache.get(ctx, a.StorageID)
		if err != nil {
			errs = append(errs, fmt.Errorf("artifact %d: %w", a.ID, err))
			continue
		}
		if err := storage.Delete(ctx, a.Key); err != nil {
			// The catalog row stays: dropping it now would strand the object
			// with nothing left pointing at it.
			errs = append(errs, fmt.Errorf("delete object %s: %w", a.Key, err))
			continue
		}
		if err := m.store.DeleteArtifact(ctx, a.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete artifact %d: %w", a.ID, err))
			continue
		}
		deleted = append(deleted, a)
		m.log.Info("retention: pruned artifact", "task", task.Name, "key", a.Key)
	}
	return deleted, errors.Join(errs...)
}

// Orphans reports objects sitting in storage that the catalog does not know
// about — what a crash or a manual copy left behind.
//
// The search scope is derived from the catalog, not from the task's current
// name and storage alone: renaming a task changes the prefix new keys are built
// under (BuildArtifactKey), and repointing it changes the storage, so anything
// written before either change would otherwise fall outside the search and
// never be reported.
func (m *RetentionManager) Orphans(ctx context.Context, task *Task, open StorageOpener) ([]plugin.Object, error) {
	artifacts, err := m.store.ListArtifactsByTask(ctx, task.ID)
	if err != nil {
		return nil, err
	}

	// A key is only "known" within the storage that holds it: the same key in
	// two storages is two different objects.
	type scope struct {
		storageID int64
		prefix    string
	}
	type located struct {
		storageID int64
		key       string
	}
	known := make(map[located]bool, len(artifacts))
	scopes := map[scope]bool{
		{task.StorageID, ArtifactPrefix(task.Name)}: true, // where new artifacts land
	}
	for _, a := range artifacts {
		known[located{a.StorageID, a.Key}] = true
		scopes[scope{a.StorageID, keyPrefix(a.Key)}] = true
	}

	var (
		orphans []plugin.Object
		errs    []error
		cache   = storageCache{open: open}
		seen    = make(map[located]bool)
	)
	defer cache.closeAll()
	for sc := range scopes {
		storage, err := cache.get(ctx, sc.storageID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		objects, err := storage.List(ctx, sc.prefix)
		if err != nil {
			errs = append(errs, fmt.Errorf("list %q: %w", sc.prefix, err))
			continue
		}
		for _, o := range objects {
			at := located{sc.storageID, o.Key}
			if known[at] || seen[at] {
				continue
			}
			seen[at] = true
			orphans = append(orphans, o)
		}
	}
	return orphans, errors.Join(errs...)
}

// keyPrefix is the task segment of an artifact key — the first path element,
// matching what ArtifactPrefix builds. A key with no separator yields "", which
// lists the whole storage; that cannot happen for keys BuildArtifactKey wrote.
func keyPrefix(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i+1]
	}
	return ""
}
