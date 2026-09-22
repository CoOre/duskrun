package core

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

var retBase = time.Date(2026, 6, 15, 3, 0, 0, 0, time.UTC)

// art builds an artifact with a fixed date (days before retBase not needed; we
// pass explicit dates via ymd).
func art(id int64, key string, t time.Time) Artifact {
	return Artifact{ID: id, Key: key, CreatedAt: t}
}

func ymd(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 2, 0, 0, 0, time.UTC)
}

func idset(arts []Artifact) map[int64]bool {
	s := make(map[int64]bool)
	for _, a := range arts {
		s[a.ID] = true
	}
	return s
}

func wantIDs(t *testing.T, label string, got []Artifact, ids ...int64) {
	t.Helper()
	gs := idset(got)
	if len(gs) != len(ids) {
		t.Fatalf("%s: got ids %v, want %v", label, gs, ids)
	}
	for _, id := range ids {
		if !gs[id] {
			t.Fatalf("%s: missing id %d in %v", label, id, gs)
		}
	}
}

func TestForgetKeepLast(t *testing.T) {
	arts := []Artifact{
		art(1, "a1", ymd(2026, 6, 15)),
		art(2, "a2", ymd(2026, 6, 14)),
		art(3, "a3", ymd(2026, 6, 13)),
		art(4, "a4", ymd(2026, 6, 12)),
		art(5, "a5", ymd(2026, 6, 11)),
	}
	keep, del := Forget(Retention{KeepLast: 2}, arts, retBase)
	wantIDs(t, "keep", keep, 1, 2)
	wantIDs(t, "del", del, 3, 4, 5)
}

func TestForgetGFS(t *testing.T) {
	arts := []Artifact{
		art(1, "a1", ymd(2026, 6, 15)), // Mon W25, June
		art(2, "a2", ymd(2026, 6, 14)), // Sun W24
		art(3, "a3", ymd(2026, 6, 13)), // Sat W24
		art(4, "a4", ymd(2026, 6, 7)),  // Sun W23
		art(5, "a5", ymd(2026, 5, 15)), // May
		art(6, "a6", ymd(2026, 5, 10)), // May
	}
	ret := Retention{GFS: &GFS{Daily: 2, Weekly: 3, Monthly: 2}}
	keep, del := Forget(ret, arts, retBase)
	// daily{1,2} ∪ weekly{1,2,4} ∪ monthly{1,5} = {1,2,4,5}
	wantIDs(t, "keep", keep, 1, 2, 4, 5)
	wantIDs(t, "del", del, 3, 6)
}

func TestForgetUnion(t *testing.T) {
	arts := []Artifact{
		art(1, "a1", ymd(2026, 6, 15)),
		art(2, "a2", ymd(2026, 6, 14)),
		art(3, "a3", ymd(2026, 6, 13)),
		art(4, "a4", ymd(2026, 6, 12)),
		art(5, "a5", ymd(2026, 5, 15)),
		art(6, "a6", ymd(2026, 4, 15)),
	}
	// keep_last=3 → {1,2,3}; monthly=2 → {1 (June), 5 (May)}. Union {1,2,3,5}.
	ret := Retention{KeepLast: 3, GFS: &GFS{Monthly: 2}}
	keep, del := Forget(ret, arts, retBase)
	wantIDs(t, "keep", keep, 1, 2, 3, 5)
	wantIDs(t, "del", del, 4, 6)
}

func TestRetentionValidate(t *testing.T) {
	cases := []struct {
		name string
		ret  Retention
		ok   bool
	}{
		{"empty", Retention{}, true},
		{"keep_last only", Retention{KeepLast: 7}, true},
		{"gfs only", Retention{GFS: &GFS{Daily: 7, Weekly: 4, Monthly: 12}}, true},
		{"gfs all zero", Retention{GFS: &GFS{}}, true},
		{"negative keep_last", Retention{KeepLast: -1}, false},
		{"negative daily", Retention{GFS: &GFS{Daily: -1}}, false},
		{"negative weekly", Retention{GFS: &GFS{Weekly: -2}}, false},
		{"negative monthly", Retention{GFS: &GFS{Monthly: -3}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.ret.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !c.ok && err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
		})
	}
}

func TestRetentionNormalized(t *testing.T) {
	// An all-zero block keeps nothing, so it must not survive as a GFS policy.
	if got := (Retention{KeepLast: 5, GFS: &GFS{}}).Normalized(); got.GFS != nil {
		t.Fatalf("all-zero GFS survived normalisation: %+v", got.GFS)
	}
	// A block that keeps something is untouched, including its zero fields.
	got := (Retention{GFS: &GFS{Daily: 7}}).Normalized()
	if got.GFS == nil || got.GFS.Daily != 7 || got.GFS.Weekly != 0 {
		t.Fatalf("active GFS mangled: %+v", got.GFS)
	}
	// keep_last alone is not a GFS policy and must not grow one.
	if got := (Retention{KeepLast: 3}).Normalized(); got.GFS != nil || got.KeepLast != 3 {
		t.Fatalf("keep_last policy changed: %+v", got)
	}
}

// oneStorage is a StorageOpener that serves the same storage for every id — the
// common case where a task never moved.
func oneStorage(s plugin.Storage) StorageOpener {
	return func(context.Context, int64) (plugin.Storage, error) { return s, nil }
}

// fakeRetStore is an in-memory RetentionStore.
type fakeRetStore struct {
	arts    []Artifact
	deleted []int64
}

func (f *fakeRetStore) ListArtifactsByTask(_ context.Context, _ int64) ([]Artifact, error) {
	return f.arts, nil
}
func (f *fakeRetStore) DeleteArtifact(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestRetentionPrune(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	// Seed three stored objects.
	for _, k := range []string{"k/1", "k/2", "k/3"} {
		if _, err := storage.Write(ctx, k, bytes.NewReader([]byte("payload-"+k))); err != nil {
			t.Fatal(err)
		}
	}
	store := &fakeRetStore{arts: []Artifact{
		art(1, "k/1", ymd(2026, 6, 15)),
		art(2, "k/2", ymd(2026, 6, 14)),
		art(3, "k/3", ymd(2026, 6, 13)),
	}}
	task := &Task{ID: 1, Name: "t", Retention: Retention{KeepLast: 1}}

	mgr := NewRetentionManager(store, nil)
	deleted, err := mgr.Prune(ctx, task, oneStorage(storage), retBase)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	wantIDs(t, "deleted", deleted, 2, 3)

	// k/1 survives; k/2 and k/3 are gone from storage.
	if _, err := storage.Read(ctx, "k/1"); err != nil {
		t.Errorf("k/1 should survive: %v", err)
	}
	for _, gone := range []string{"k/2", "k/3"} {
		if _, err := storage.Read(ctx, gone); err == nil {
			t.Errorf("%s should have been pruned from storage", gone)
		}
	}
	// Catalog rows deleted too.
	if len(store.deleted) != 2 {
		t.Errorf("catalog deletes = %v, want 2", store.deleted)
	}
}

// countingStorage wraps a storage and counts Close, standing in for one that
// holds a live session (sftp).
type countingStorage struct {
	plugin.Storage
	closed int
}

func (c *countingStorage) Close() error {
	c.closed++
	return nil
}

// TestRetentionClosesStorages guards the leak: a sweep opens a storage per pass,
// and a session-holding one would keep its ssh connection for the life of the
// daemon if Prune/Orphans walked away from it.
func TestRetentionClosesStorages(t *testing.T) {
	ctx := context.Background()
	base, _ := newLocalfs(t)
	for _, k := range []string{"k/1", "k/2"} {
		if _, err := base.Write(ctx, k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	storage := &countingStorage{Storage: base}
	store := &fakeRetStore{arts: []Artifact{
		art(1, "k/1", ymd(2026, 6, 15)),
		art(2, "k/2", ymd(2026, 6, 14)),
	}}
	task := &Task{ID: 1, Name: "t", Retention: Retention{KeepLast: 1}}

	mgr := NewRetentionManager(store, nil)
	if _, err := mgr.Prune(ctx, task, oneStorage(storage), retBase); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if storage.closed != 1 {
		t.Fatalf("Prune closed the storage %d times, want 1", storage.closed)
	}
	if _, err := mgr.Orphans(ctx, task, oneStorage(storage)); err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if storage.closed != 2 {
		t.Fatalf("Orphans left the storage open: closed = %d, want 2", storage.closed)
	}
}

func TestOrphanReport(t *testing.T) {
	ctx := context.Background()
	storage, _ := newLocalfs(t)
	for _, k := range []string{"k/known", "k/orphan"} {
		if _, err := storage.Write(ctx, k, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatal(err)
		}
	}
	store := &fakeRetStore{arts: []Artifact{art(1, "k/known", ymd(2026, 6, 15))}}
	task := &Task{ID: 1, Name: "t"}

	mgr := NewRetentionManager(store, nil)
	orphans, err := mgr.Orphans(ctx, task, oneStorage(storage))
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Key != "k/orphan" {
		t.Fatalf("orphans = %v, want [k/orphan]", orphans)
	}
}
