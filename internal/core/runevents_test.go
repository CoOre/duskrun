package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
	"github.com/CoOre/duskrun/internal/store/sqlite"
)

func recordingPool(st *sqlite.Store, rec *recordNotifier) *core.Pool {
	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	return core.NewPool(st, exec, core.PoolConfig{
		Workers: 1,
		Idle:    time.Millisecond,
		Backoff: func(int) time.Duration { return 0 },
		Notify: core.NewNotifications(nil, nil, core.NotificationsConfig{
			Resolve: func(context.Context, *core.Task, plugin.EventKind) []plugin.Notifier {
				return []plugin.Notifier{rec}
			},
		}, nil),
	}, nil)
}

func kinds(evs []plugin.Event) []plugin.EventKind {
	out := make([]plugin.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func runOnce(t *testing.T, st *sqlite.Store, pool *core.Pool, taskID int64) {
	t.Helper()
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if did, err := pool.ProcessNext(context.Background(), "w0"); err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v)", did, err)
	}
}

// seedHistory records n earlier successful runs of the task with artifacts of
// the given size.
func seedHistory(t *testing.T, path string, taskID int64, n int, size int64) {
	t.Helper()
	db := rawDB(t, path)
	for i := range n {
		res, err := db.Exec(`INSERT INTO run (task_id, status, attempt, started_at, finished_at, created_at)
			VALUES (?, 'success', 1, 1000, 1060, 1000)`, taskID)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO artifact (run_id, storage_id, key, size, checksum, created_at)
			VALUES (?, 1, ?, ?, '', 1000)`, id, "old/"+string(rune('a'+i)), size); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWorkerReportsSmallBackup: a run that "worked" but produced a fraction of
// the usual size is the silent failure this event exists for.
func TestWorkerReportsSmallBackup(t *testing.T) {
	st, path := openStore(t)
	taskID := seedFakeTaskFull(t, path, t.TempDir(), "fakeexec", 0, 1800)
	seedHistory(t, path, taskID, 3, 50<<20)

	rec := &recordNotifier{}
	runOnce(t, st, recordingPool(st, rec), taskID)

	evs := rec.all()
	if len(evs) != 2 || evs[0].Kind != plugin.EventSuccess || evs[1].Kind != plugin.EventSizeAnomaly {
		t.Fatalf("events = %v, want success then size_anomaly", kinds(evs))
	}
	if evs[1].RunID == 0 || evs[1].Message == "" {
		t.Fatalf("size anomaly without run or explanation: %+v", evs[1])
	}
}

// TestWorkerSizeNeedsHistory: two earlier runs are not a habit to deviate from.
func TestWorkerSizeNeedsHistory(t *testing.T) {
	st, path := openStore(t)
	taskID := seedFakeTaskFull(t, path, t.TempDir(), "fakeexec", 0, 1800)
	seedHistory(t, path, taskID, 2, 50<<20)

	rec := &recordNotifier{}
	runOnce(t, st, recordingPool(st, rec), taskID)
	if evs := rec.all(); len(evs) != 1 {
		t.Fatalf("events = %v, want only the success", kinds(evs))
	}
}

// TestWorkerReportsRecovery: the first success after a lost run says the task
// is fine again — once, not on every later success.
func TestWorkerReportsRecovery(t *testing.T) {
	st, path := openStore(t)
	taskID := seedFakeTaskFull(t, path, t.TempDir(), "fakefail", 0, 1800)
	rec := &recordNotifier{}
	pool := recordingPool(st, rec)

	runOnce(t, st, pool, taskID)
	if _, err := rawDB(t, path).Exec(`UPDATE connection SET engine = 'fakeexec'`); err != nil {
		t.Fatal(err)
	}
	runOnce(t, st, pool, taskID)
	runOnce(t, st, pool, taskID)

	got := kinds(rec.all())
	want := []plugin.EventKind{plugin.EventFailure, plugin.EventSuccess, plugin.EventRecovered, plugin.EventSuccess}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

// TestWorkerRecoveryClosesWatchdog: a stale task's next success reports the
// recovery itself and clears the watchdog marker, so the watchdog's own tick
// has nothing left to announce.
func TestWorkerRecoveryClosesWatchdog(t *testing.T) {
	st, path := openStore(t)
	taskID := seedFakeTaskFull(t, path, t.TempDir(), "fakeexec", 0, 1800)
	ctx := context.Background()
	if err := st.MarkWatchdogAlerted(ctx, core.WatchdogRecord{TaskID: taskID, AlertedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec := &recordNotifier{}
	runOnce(t, st, recordingPool(st, rec), taskID)

	if got := kinds(rec.all()); len(got) != 2 || got[1] != plugin.EventRecovered {
		t.Fatalf("events = %v, want success then recovered", got)
	}
	alerts, err := st.ListWatchdogAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := alerts[taskID]; ok {
		t.Fatal("watchdog marker left behind")
	}
}

// TestReportInterrupted: a run the restart reaped reaches the task's channels,
// and counts as a failure the next success recovers from.
func TestReportInterrupted(t *testing.T) {
	st, path := openStore(t)
	taskID := seedFakeTaskFull(t, path, t.TempDir(), "fakeexec", 0, 1800)
	ctx := context.Background()
	if _, err := rawDB(t, path).Exec(`INSERT INTO run (task_id, status, attempt, started_at, created_at)
		VALUES (?, 'running', 1, 1800000000, 1800000000)`, taskID); err != nil {
		t.Fatal(err)
	}
	reaped, err := st.ReapStale(ctx)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("ReapStale = %v, %v", reaped, err)
	}

	rec := &recordNotifier{}
	pool := recordingPool(st, rec)
	notes := core.NewNotifications(nil, nil, core.NotificationsConfig{
		Resolve: func(context.Context, *core.Task, plugin.EventKind) []plugin.Notifier {
			return []plugin.Notifier{rec}
		},
	}, nil)
	core.ReportInterrupted(ctx, st, notes, reaped, time.Now(), nil)
	runOnce(t, st, pool, taskID)

	evs := rec.all()
	got := kinds(evs)
	if len(got) != 3 || got[0] != plugin.EventInterrupted || got[2] != plugin.EventRecovered {
		t.Fatalf("events = %v, want interrupted, success, recovered", got)
	}
	if evs[0].RunID != reaped[0] {
		t.Fatalf("interrupted event names run %d, want %d", evs[0].RunID, reaped[0])
	}
}
