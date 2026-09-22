package core

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

var sweepNow = time.Date(2026, 6, 15, 3, 30, 0, 0, time.UTC)

// fakeSweepStore is an in-memory SweepStore: one artifact set per task, plus the
// sweeps that were recorded.
type fakeSweepStore struct {
	tasks    []Task
	arts     map[int64][]Artifact
	storages map[int64]*Storage

	deleted    []int64
	sweeps     []Sweep
	tasksErr   error
	insertErr  error
	storageErr error

	pruneKeep int   // last keep the sweeper asked for (0 = never asked)
	pruneErr  error // makes trimming the delivery log fail
}

func (f *fakeSweepStore) ListArtifactsByTask(_ context.Context, taskID int64) ([]Artifact, error) {
	return f.arts[taskID], nil
}

func (f *fakeSweepStore) DeleteArtifact(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeSweepStore) ListTasks(_ context.Context) ([]Task, error) {
	return f.tasks, f.tasksErr
}

func (f *fakeSweepStore) GetStorage(_ context.Context, id int64) (*Storage, error) {
	if f.storageErr != nil {
		return nil, f.storageErr
	}
	st, ok := f.storages[id]
	if !ok {
		return nil, errors.New("no such storage")
	}
	return st, nil
}

func (f *fakeSweepStore) InsertSweep(_ context.Context, sw Sweep) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	sw.ID = int64(len(f.sweeps) + 1)
	f.sweeps = append(f.sweeps, sw)
	return sw.ID, nil
}

func (f *fakeSweepStore) PruneNotifications(_ context.Context, keep int) (int64, error) {
	f.pruneKeep = keep
	return 0, f.pruneErr
}

// newSweeperOver builds a Sweeper whose tasks all write to the single storage
// backing them, with a frozen clock.
func newSweeperOver(t *testing.T, store *fakeSweepStore, storage plugin.Storage) *Sweeper {
	t.Helper()
	return NewSweeper(store, SweeperConfig{
		Now:         func() time.Time { return sweepNow },
		Notify:      testNotifications(nil),
		OpenStorage: func(context.Context, *Storage) (plugin.Storage, error) { return storage, nil },
	}, nil)
}

func seedObjects(t *testing.T, storage plugin.Storage, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, err := storage.Write(context.Background(), k, bytes.NewReader([]byte("payload-"+k))); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSweepPrunesAndRecords is the happy path: the policy deletes the older
// artifacts, the freed bytes are summed, and one success row is recorded.
func TestSweepPrunesAndRecords(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	seedObjects(t, storage, "t/1", "t/2", "t/3")

	store := &fakeSweepStore{
		tasks:    []Task{{ID: 1, Name: "t", StorageID: 7, Retention: Retention{KeepLast: 1}}},
		storages: map[int64]*Storage{7: {ID: 7, Name: "s", Type: "localfs"}},
		arts: map[int64][]Artifact{1: {
			{ID: 1, StorageID: 7, Key: "t/1", Size: 100, CreatedAt: ymd(2026, 6, 15)},
			{ID: 2, StorageID: 7, Key: "t/2", Size: 200, CreatedAt: ymd(2026, 6, 14)},
			{ID: 3, StorageID: 7, Key: "t/3", Size: 300, CreatedAt: ymd(2026, 6, 13)},
		}},
	}

	sw, err := newSweeperOver(t, store, storage).Sweep(ctx, SweepSchedule)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if sw.Status != SweepSuccess {
		t.Errorf("status = %q, want success (error: %s)", sw.Status, sw.Error)
	}
	if sw.DeletedCount != 2 {
		t.Errorf("deleted = %d, want 2", sw.DeletedCount)
	}
	if sw.FreedBytes != 500 {
		t.Errorf("freed = %d, want 500", sw.FreedBytes)
	}
	if sw.Source != SweepSchedule {
		t.Errorf("source = %q, want schedule", sw.Source)
	}
	if len(store.sweeps) != 1 || store.sweeps[0].Status != SweepSuccess {
		t.Errorf("recorded sweeps = %+v, want one success", store.sweeps)
	}
	// The kept artifact's object survives; the pruned ones are gone.
	if _, err := storage.Read(ctx, "t/1"); err != nil {
		t.Errorf("t/1 should survive: %v", err)
	}
	for _, gone := range []string{"t/2", "t/3"} {
		if _, err := storage.Read(ctx, gone); err == nil {
			t.Errorf("%s should have been pruned", gone)
		}
	}
}

// TestSweepTrimsDeliveryLog: the notification journal is append-only — one row
// per event per channel, successes included — and nothing else deletes from it.
// A prune failure stays out of the sweep's own status: the artifacts it exists
// for were pruned either way.
func TestSweepTrimsDeliveryLog(t *testing.T) {
	storage, _ := newLocalfs(t)
	store := &fakeSweepStore{}

	sw, err := newSweeperOver(t, store, storage).Sweep(context.Background(), SweepSchedule)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if store.pruneKeep != NotificationKeep {
		t.Errorf("prune keep = %d, want %d", store.pruneKeep, NotificationKeep)
	}
	if sw.Status != SweepSuccess {
		t.Errorf("status = %q, want success", sw.Status)
	}

	store = &fakeSweepStore{pruneErr: errors.New("database is locked")}
	sw, err = newSweeperOver(t, store, storage).Sweep(context.Background(), SweepSchedule)
	if err != nil || sw.Status != SweepSuccess {
		t.Errorf("sweep = %+v / %v, want a failed trim kept out of the sweep status", sw, err)
	}
}

// TestSweepSkipsTaskWithoutPolicy guards the data-loss trap: an unconfigured
// retention means "keep everything", and Forget would otherwise mark every
// artifact deletable.
func TestSweepSkipsTaskWithoutPolicy(t *testing.T) {
	storage, _ := newLocalfs(t)
	seedObjects(t, storage, "t/1", "t/2")

	store := &fakeSweepStore{
		tasks:    []Task{{ID: 1, Name: "t", StorageID: 7}}, // zero Retention
		storages: map[int64]*Storage{7: {ID: 7, Type: "localfs"}},
		arts: map[int64][]Artifact{1: {
			{ID: 1, StorageID: 7, Key: "t/1", Size: 100, CreatedAt: ymd(2026, 6, 15)},
			{ID: 2, StorageID: 7, Key: "t/2", Size: 200, CreatedAt: ymd(2026, 6, 14)},
		}},
	}

	sw, err := newSweeperOver(t, store, storage).Sweep(context.Background(), SweepSchedule)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if sw.DeletedCount != 0 || len(store.deleted) != 0 {
		t.Fatalf("deleted %d artifacts (%v), want none for an unconfigured policy", sw.DeletedCount, store.deleted)
	}
}

// TestSweepCountsOrphans: an object under the task's prefix that the catalog
// does not know about is reported, not deleted.
func TestSweepCountsOrphans(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	seedObjects(t, storage, "t/known", "t/orphan")

	store := &fakeSweepStore{
		tasks:    []Task{{ID: 1, Name: "t", StorageID: 7, Retention: Retention{KeepLast: 5}}},
		storages: map[int64]*Storage{7: {ID: 7, Type: "localfs"}},
		arts:     map[int64][]Artifact{1: {{ID: 1, StorageID: 7, Key: "t/known", Size: 10, CreatedAt: ymd(2026, 6, 15)}}},
	}

	sw, err := newSweeperOver(t, store, storage).Sweep(ctx, SweepManual)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if sw.OrphanCount != 1 {
		t.Errorf("orphans = %d, want 1", sw.OrphanCount)
	}
	// Reporting must not delete: the orphan is still there.
	if _, err := storage.Read(ctx, "t/orphan"); err != nil {
		t.Errorf("orphan should be reported, not deleted: %v", err)
	}
}

// TestSweepContinuesAfterTaskError: one broken task must not stop the others,
// and the sweep is recorded as failed with the offending task named.
func TestSweepContinuesAfterTaskError(t *testing.T) {
	storage, _ := newLocalfs(t)
	seedObjects(t, storage, "good/1", "good/2")

	store := &fakeSweepStore{
		tasks: []Task{
			{ID: 1, Name: "broken", StorageID: 99, Retention: Retention{KeepLast: 1}},
			{ID: 2, Name: "good", StorageID: 7, Retention: Retention{KeepLast: 1}},
		},
		storages: map[int64]*Storage{7: {ID: 7, Type: "localfs"}},
		arts: map[int64][]Artifact{2: {
			{ID: 10, StorageID: 7, Key: "good/1", Size: 50, CreatedAt: ymd(2026, 6, 15)},
			{ID: 11, StorageID: 7, Key: "good/2", Size: 70, CreatedAt: ymd(2026, 6, 14)},
		}},
	}

	sw, err := newSweeperOver(t, store, storage).Sweep(context.Background(), SweepSchedule)
	if err == nil {
		t.Fatal("Sweep: want an error for the broken task")
	}
	if sw.Status != SweepFailed {
		t.Errorf("status = %q, want failed", sw.Status)
	}
	if !strings.Contains(sw.Error, "broken") {
		t.Errorf("error %q should name the failing task", sw.Error)
	}
	// The healthy task was still swept.
	if sw.DeletedCount != 1 || sw.FreedBytes != 70 {
		t.Errorf("deleted=%d freed=%d, want the good task pruned (1 / 70)", sw.DeletedCount, sw.FreedBytes)
	}
	if len(store.sweeps) != 1 {
		t.Errorf("recorded sweeps = %d, want 1", len(store.sweeps))
	}
}

// TestSweepRecordsListTasksFailure: if the task listing itself fails there is
// nothing to sweep, but the history must still show the failed attempt.
func TestSweepRecordsListTasksFailure(t *testing.T) {
	storage, _ := newLocalfs(t)
	store := &fakeSweepStore{tasksErr: errors.New("db is down")}

	sw, err := newSweeperOver(t, store, storage).Sweep(context.Background(), SweepSchedule)
	if err == nil {
		t.Fatal("Sweep: want an error")
	}
	if sw.Status != SweepFailed || !strings.Contains(sw.Error, "db is down") {
		t.Errorf("sweep = %+v, want a failed record naming the cause", sw)
	}
	if len(store.sweeps) != 1 {
		t.Fatalf("recorded sweeps = %d, want 1", len(store.sweeps))
	}
}

// TestSweepNotifiesRetentionError: a task's channels hear about its retention
// failure, with the retention_error kind.
func TestSweepNotifiesRetentionError(t *testing.T) {
	storage, _ := newLocalfs(t)
	store := &fakeSweepStore{
		tasks:    []Task{{ID: 1, Name: "broken", StorageID: 99, Retention: Retention{KeepLast: 1}}},
		storages: map[int64]*Storage{},
	}
	spy := &spyNotifier{}
	sweeper := NewSweeper(store, SweeperConfig{
		Now:         func() time.Time { return sweepNow },
		Notify:      testNotifications(spy),
		OpenStorage: func(context.Context, *Storage) (plugin.Storage, error) { return storage, nil },
	}, nil)

	if _, err := sweeper.Sweep(context.Background(), SweepSchedule); err == nil {
		t.Fatal("Sweep: want an error")
	}
	if len(spy.events) != 1 {
		t.Fatalf("events = %d, want 1", len(spy.events))
	}
	if spy.events[0].Kind != plugin.EventRetentionError {
		t.Errorf("kind = %q, want retention_error", spy.events[0].Kind)
	}
	if spy.events[0].Task != "broken" {
		t.Errorf("task = %q, want broken", spy.events[0].Task)
	}
}

type spyNotifier struct{ events []plugin.Event }

func (s *spyNotifier) Name() string { return "spy" }
func (s *spyNotifier) Notify(_ context.Context, ev plugin.Event) error {
	s.events = append(s.events, ev)
	return nil
}

func TestRetentionEnabled(t *testing.T) {
	cases := []struct {
		name string
		ret  Retention
		want bool
	}{
		{"zero", Retention{}, false},
		{"keep_last", Retention{KeepLast: 1}, true},
		{"empty gfs", Retention{GFS: &GFS{}}, false},
		{"gfs daily", Retention{GFS: &GFS{Daily: 1}}, true},
		{"gfs monthly", Retention{GFS: &GFS{Monthly: 3}}, true},
	}
	for _, c := range cases {
		if got := c.ret.Enabled(); got != c.want {
			t.Errorf("%s: Enabled() = %v, want %v", c.name, got, c.want)
		}
	}
}

// newSweeperMulti builds a Sweeper whose storage records map to distinct storage
// plugins by id — the situation after a task is repointed at a new destination.
func newSweeperMulti(t *testing.T, store *fakeSweepStore, byID map[int64]plugin.Storage) *Sweeper {
	t.Helper()
	return NewSweeper(store, SweeperConfig{
		Now:    func() time.Time { return sweepNow },
		Notify: testNotifications(nil),
		OpenStorage: func(_ context.Context, st *Storage) (plugin.Storage, error) {
			s, ok := byID[st.ID]
			if !ok {
				return nil, errors.New("no plugin for storage")
			}
			return s, nil
		},
	}, nil)
}

// TestSweepPrunesArtifactInItsOwnStorage: after a task is repointed from storage
// A to B, its older artifacts still live in A and must be deleted THERE.
// Deleting them against B would silently succeed — both localfs and S3 treat a
// missing key as already gone — dropping the catalog row while the real file
// stayed on A forever, untracked and beyond the reach of orphan reporting.
func TestSweepPrunesArtifactInItsOwnStorage(t *testing.T) {
	ctx := context.Background()
	oldStore, _ := newLocalfs(t)
	newStore, _ := newLocalfs(t)
	seedObjects(t, oldStore, "t/old-1", "t/old-2")
	seedObjects(t, newStore, "t/new-1")

	store := &fakeSweepStore{
		// The task now points at storage 8; artifacts 1-2 predate the move.
		tasks:    []Task{{ID: 1, Name: "t", StorageID: 8, Retention: Retention{KeepLast: 1}}},
		storages: map[int64]*Storage{7: {ID: 7, Type: "localfs"}, 8: {ID: 8, Type: "localfs"}},
		arts: map[int64][]Artifact{1: {
			{ID: 3, StorageID: 8, Key: "t/new-1", Size: 300, CreatedAt: ymd(2026, 6, 15)},
			{ID: 2, StorageID: 7, Key: "t/old-2", Size: 200, CreatedAt: ymd(2026, 6, 14)},
			{ID: 1, StorageID: 7, Key: "t/old-1", Size: 100, CreatedAt: ymd(2026, 6, 13)},
		}},
	}

	sweeper := newSweeperMulti(t, store, map[int64]plugin.Storage{7: oldStore, 8: newStore})
	sw, err := sweeper.Sweep(ctx, SweepSchedule)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if sw.DeletedCount != 2 || sw.FreedBytes != 300 {
		t.Errorf("deleted=%d freed=%d, want the two old artifacts (2 / 300)", sw.DeletedCount, sw.FreedBytes)
	}
	// The objects are actually gone from the OLD storage, not merely delisted.
	for _, gone := range []string{"t/old-1", "t/old-2"} {
		if _, err := oldStore.Read(ctx, gone); err == nil {
			t.Errorf("%s still present in the old storage — it was deleted against the wrong one", gone)
		}
	}
	if _, err := newStore.Read(ctx, "t/new-1"); err != nil {
		t.Errorf("the newest artifact should survive in the current storage: %v", err)
	}
}

// TestPruneContinuesPastUndeletableObject: one object that cannot be removed
// must not wedge the task. The delete set is recomputed newest-first every
// sweep, so a permanently failing key is retried first forever — everything
// older behind it would never be reconsidered and the storage would grow
// without bound.
func TestPruneContinuesPastUndeletableObject(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	seedObjects(t, storage, "t/1", "t/2", "t/3", "t/4")

	blocked := &blockingStorage{Storage: storage, fail: map[string]bool{"t/3": true}}
	store := &fakeRetStore{arts: []Artifact{
		{ID: 1, StorageID: 7, Key: "t/1", Size: 10, CreatedAt: ymd(2026, 6, 15)},
		{ID: 2, StorageID: 7, Key: "t/2", Size: 20, CreatedAt: ymd(2026, 6, 14)},
		{ID: 3, StorageID: 7, Key: "t/3", Size: 30, CreatedAt: ymd(2026, 6, 13)},
		{ID: 4, StorageID: 7, Key: "t/4", Size: 40, CreatedAt: ymd(2026, 6, 12)},
	}}
	task := &Task{ID: 1, Name: "t", StorageID: 7, Retention: Retention{KeepLast: 2}}

	mgr := NewRetentionManager(store, nil)
	deleted, err := mgr.Prune(ctx, task, oneStorage(blocked), retBase)
	if err == nil {
		t.Fatal("Prune: want the undeletable object reported")
	}
	// t/3 failed, but t/4 behind it was still collected.
	wantIDs(t, "deleted", deleted, 4)
	if _, err := storage.Read(ctx, "t/3"); err != nil {
		t.Error("t/3 should still be in storage — its delete failed")
	}
	// Its catalog row must survive too: dropping it would strand the object.
	for _, id := range store.deleted {
		if id == 3 {
			t.Error("catalog row 3 was deleted even though the object removal failed")
		}
	}
}

// blockingStorage refuses to delete specific keys (object-lock, bad permissions).
type blockingStorage struct {
	plugin.Storage
	fail map[string]bool
}

func (b *blockingStorage) Delete(ctx context.Context, key string) error {
	if b.fail[key] {
		return errors.New("access denied")
	}
	return b.Storage.Delete(ctx, key)
}

// TestOrphansFindObjectsUnderFormerTaskName: renaming a task changes the prefix
// new keys are built under, so objects written before the rename sit under the
// old prefix. Searching only the current name would silently stop reporting
// them.
func TestOrphansFindObjectsUnderFormerTaskName(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	// Written as "orders", plus a stray left next to it; the task is now "billing".
	seedObjects(t, storage, "orders/db/known.dump", "orders/db/stray.dump", "billing/db/fresh.dump")

	store := &fakeRetStore{arts: []Artifact{
		{ID: 1, StorageID: 7, Key: "orders/db/known.dump", CreatedAt: ymd(2026, 6, 14)},
		{ID: 2, StorageID: 7, Key: "billing/db/fresh.dump", CreatedAt: ymd(2026, 6, 15)},
	}}
	task := &Task{ID: 1, Name: "billing", StorageID: 7}

	mgr := NewRetentionManager(store, nil)
	orphans, err := mgr.Orphans(ctx, task, oneStorage(storage))
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Key != "orders/db/stray.dump" {
		t.Fatalf("orphans = %v, want the stray under the former name", orphans)
	}
}

// TestTrySweepReportsBusy: a second sweep must be told to come back rather than
// block behind the first for its full duration.
func TestTrySweepReportsBusy(t *testing.T) {
	storage, _ := newLocalfs(t)
	store := &fakeSweepStore{}
	sweeper := newSweeperOver(t, store, storage)

	// Stand in for a sweep in flight by holding the sweeper's lock.
	release, entered, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		sweeper.mu.Lock()
		close(entered)
		<-release
		sweeper.mu.Unlock()
		close(released) // only now is the lock observably free
	}()
	<-entered

	_, ok, err := sweeper.TrySweep(context.Background(), SweepManual)
	if ok || err != nil {
		t.Errorf("TrySweep during a running sweep = (ok %v, err %v), want (false, nil)", ok, err)
	}

	close(release)
	<-released

	if _, ok, err := sweeper.TrySweep(context.Background(), SweepManual); !ok || err != nil {
		t.Errorf("TrySweep once free = (ok %v, err %v), want (true, nil)", ok, err)
	}
}
