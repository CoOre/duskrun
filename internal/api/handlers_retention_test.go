package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// retentionDeps seeds a store with one task and builds a Sweeper over it — the
// pieces the retention endpoints need, without an HTTP server.
func retentionDeps(t *testing.T) (*sqlite.Store, *core.Sweeper) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retention.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	storageID, err := st.CreateStorage(ctx, core.Storage{Name: "s", Type: "localfs", Config: []byte(`{"root":"` + t.TempDir() + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	connID, err := st.CreateConnection(ctx, core.Connection{Name: "c", Engine: "postgres", ConnectorType: "direct", ConnectorConfig: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, core.Task{
		Name: "nightly", ConnectionID: connID, StorageID: storageID,
		DumperOpts: []byte(`{}`), CodecChain: []string{"zstd"},
		Cron: "0 2 * * *", Retention: core.Retention{KeepLast: 3}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	return st, core.NewSweeper(st, core.SweeperConfig{}, nil)
}

// retentionServer wraps retentionDeps in a running API server with a retention
// cron configured, so the full surface (read + manual sweep) is exercised.
func retentionServer(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	st, sweeper := retentionDeps(t)
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken, Sweeper: sweeper, RetentionCron: "30 3 * * *",
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestGetRetentionSummary(t *testing.T) {
	srv, _ := retentionServer(t)

	var got retentionDTO
	decodeInto(t, authGet(t, srv.URL+"/api/retention"), &got)

	if got.Cron != "30 3 * * *" {
		t.Errorf("cron = %q, want the configured schedule", got.Cron)
	}
	if got.NextSweep == nil || !got.NextSweep.After(time.Now()) {
		t.Errorf("next_sweep = %v, want a future time", got.NextSweep)
	}
	if got.LastSweep != nil {
		t.Errorf("last_sweep = %+v, want none before any sweep ran", got.LastSweep)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(got.Tasks))
	}
	task := got.Tasks[0]
	if task.Name != "nightly" || task.Retention.KeepLast != 3 {
		t.Errorf("task row = %+v, want nightly with keep_last 3", task)
	}
	if task.Artifacts != 0 || task.Bytes != 0 {
		t.Errorf("footprint = %d artifacts / %d bytes, want zero", task.Artifacts, task.Bytes)
	}
}

// TestGetRetentionCronDisabled: with no schedule configured the summary reports
// an empty cron and no next sweep, rather than inventing one.
func TestGetRetentionCronDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention-off.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(NewRouter(Deps{Store: st, Token: testToken}))
	t.Cleanup(srv.Close)

	var got retentionDTO
	decodeInto(t, authGet(t, srv.URL+"/api/retention"), &got)
	if got.Cron != "" || got.NextSweep != nil {
		t.Errorf("got cron=%q next=%v, want both empty when the sweep is off", got.Cron, got.NextSweep)
	}
	if got.Tasks == nil {
		t.Error("tasks should serialise as [] rather than null")
	}
}

// TestRunSweepRecordsHistory: a manual sweep returns its record and shows up in
// the history and in the summary's last_sweep.
func TestRunSweepRecordsHistory(t *testing.T) {
	srv, _ := retentionServer(t)

	var sweep sweepDTO
	decodeInto(t, authPost(t, srv.URL+"/api/retention/sweep", ""), &sweep)
	if sweep.ID == 0 {
		t.Fatal("sweep id = 0, want a recorded sweep")
	}
	if sweep.Status != "success" {
		t.Errorf("status = %q (error %q), want success", sweep.Status, sweep.Error)
	}
	if sweep.Source != "manual" {
		t.Errorf("source = %q, want manual", sweep.Source)
	}

	var history []sweepDTO
	decodeInto(t, authGet(t, srv.URL+"/api/retention/sweeps"), &history)
	if len(history) != 1 || history[0].ID != sweep.ID {
		t.Fatalf("history = %+v, want the sweep just run", history)
	}

	var summary retentionDTO
	decodeInto(t, authGet(t, srv.URL+"/api/retention"), &summary)
	if summary.LastSweep == nil || summary.LastSweep.ID != sweep.ID {
		t.Errorf("last_sweep = %+v, want the sweep just run", summary.LastSweep)
	}
}

// TestRunSweepWithoutSweeper: the read routes stay available, but the action
// reports that it is not configured rather than 500-ing.
func TestRunSweepWithoutSweeper(t *testing.T) {
	srv, _, _ := seededServer(t)
	resp := authPost(t, srv.URL+"/api/retention/sweep", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestListSweepsRejectsBadLimit(t *testing.T) {
	srv, _ := retentionServer(t)
	resp := authGet(t, srv.URL+"/api/retention/sweeps?limit=0")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListSweepsRejectsOversizedLimit(t *testing.T) {
	srv, _ := retentionServer(t)
	resp := authGet(t, srv.URL+"/api/retention/sweeps?limit=100000000")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — an unbounded limit scans the whole history", resp.StatusCode)
	}
}

// TestRunSweepSurvivesClientDisconnect: the sweep must not ride on the request's
// context. A closed tab or a proxy read timeout would otherwise abort a healthy
// sweep part-way through the task list and record it as failed.
//
// Driven at the handler rather than over the wire: an already-cancelled request
// context is exactly the state a disconnect leaves behind, and a real client
// that aborts never gets its request delivered at all.
func TestRunSweepSurvivesClientDisconnect(t *testing.T) {
	st, sweeper := retentionDeps(t)
	s := &server{d: Deps{Store: st, Sweeper: sweeper, Log: slog.Default()}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/retention/sweep", nil)
	w := httptest.NewRecorder()

	s.runSweep(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 despite the cancelled request context", w.Code)
	}
	var sweep sweepDTO
	if err := json.NewDecoder(w.Body).Decode(&sweep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sweep.Status != "success" {
		t.Errorf("status = %q (error %q), want success — the sweep should ignore the client going away", sweep.Status, sweep.Error)
	}

	// And it is durably recorded, not just reported.
	sweeps, err := st.ListSweeps(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListSweeps: %v", err)
	}
	if len(sweeps) != 1 || sweeps[0].Status != core.SweepSuccess {
		t.Errorf("history = %+v, want one successful sweep", sweeps)
	}
}

// TestRunSweepConflictsWhileBusy: a manual sweep arriving during a scheduled one
// is told to come back, rather than silently blocking the request for the full
// duration of the running sweep.
func TestRunSweepConflictsWhileBusy(t *testing.T) {
	// A store whose task listing stalls holds the sweeper's lock for as long as
	// the test needs, with no timing guesswork.
	store := &stallingSweepStore{entered: make(chan struct{}), release: make(chan struct{})}
	sweeper := core.NewSweeper(store, core.SweeperConfig{}, slog.Default())
	s := &server{d: Deps{Sweeper: sweeper, Log: slog.Default()}}

	go func() { _, _ = sweeper.Sweep(context.Background(), core.SweepSchedule) }()
	<-store.entered // the scheduled sweep now holds the lock

	w := httptest.NewRecorder()
	s.runSweep(w, httptest.NewRequest(http.MethodPost, "/api/retention/sweep", nil))
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 while another sweep is running", w.Code)
	}

	close(store.release)
}

// stallingSweepStore blocks in ListTasks until released, simulating a long sweep.
type stallingSweepStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *stallingSweepStore) ListTasks(context.Context) ([]core.Task, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil, nil
}
func (s *stallingSweepStore) ListArtifactsByTask(context.Context, int64) ([]core.Artifact, error) {
	return nil, nil
}
func (s *stallingSweepStore) DeleteArtifact(context.Context, int64) error { return nil }
func (s *stallingSweepStore) GetStorage(context.Context, int64) (*core.Storage, error) {
	return nil, errors.New("unused")
}
func (s *stallingSweepStore) InsertSweep(context.Context, core.Sweep) (int64, error) { return 1, nil }
func (s *stallingSweepStore) PruneNotifications(context.Context, int) (int64, error) { return 0, nil }
