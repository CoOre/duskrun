package core_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
	"github.com/CoOre/duskrun/internal/store/sqlite"
)

// fakeVolume is a storage whose reachability and free space a test sets.
type fakeVolume struct {
	mu       sync.Mutex
	probeErr error
	capa     plugin.Capacity
	opened   int
}

func (f *fakeVolume) set(probeErr error, capa plugin.Capacity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeErr, f.capa = probeErr, capa
}

func (f *fakeVolume) Probe(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeErr
}

func (f *fakeVolume) Capacity(context.Context) (plugin.Capacity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capa, nil
}

func (f *fakeVolume) Write(context.Context, string, io.Reader) (plugin.ObjectMeta, error) {
	return plugin.ObjectMeta{}, errors.New("not used")
}
func (f *fakeVolume) Read(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (f *fakeVolume) List(context.Context, string) ([]plugin.Object, error) { return nil, nil }
func (f *fakeVolume) Delete(context.Context, string) error                  { return nil }

type monitorRig struct {
	st     *sqlite.Store
	path   string
	taskID int64
	vol    *fakeVolume
	rec    *recordNotifier
	now    time.Time
	mon    *core.Monitor
}

func newMonitorRig(t *testing.T) *monitorRig {
	t.Helper()
	st, path := openStore(t)
	r := &monitorRig{
		st: st, path: path, vol: &fakeVolume{}, rec: &recordNotifier{},
		now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	}
	r.taskID = seedFakeTaskFull(t, path, t.TempDir(), "fakeexec", 0, 1800)
	resolve := func(context.Context, *core.Task, plugin.EventKind) []plugin.Notifier { return []plugin.Notifier{r.rec} }
	notes := core.NewNotifications(nil, nil, core.NotificationsConfig{
		Resolve:       resolve,
		ResolveSystem: func(context.Context, plugin.EventKind) []plugin.Notifier { return []plugin.Notifier{r.rec} },
	}, nil)
	r.mon = core.NewMonitor(st, core.MonitorConfig{
		Now:    func() time.Time { return r.now },
		Notify: notes,
		OpenStorage: func(context.Context, *core.Storage) (plugin.Storage, error) {
			r.vol.mu.Lock()
			r.vol.opened++
			r.vol.mu.Unlock()
			return r.vol, nil
		},
	}, nil)
	return r
}

func (r *monitorRig) kinds() []plugin.EventKind { return kinds(r.rec.all()) }

func TestMonitorUnreachableStorage(t *testing.T) {
	r := newMonitorRig(t)
	ctx := context.Background()
	big := plugin.Capacity{Total: 100 << 30, Free: 90 << 30}

	r.vol.set(errors.New("dial tcp: connection refused"), big)
	r.mon.CheckStorages(ctx)
	if got := r.kinds(); len(got) != 0 {
		t.Fatalf("one failed probe alerted: %v", got)
	}
	r.mon.CheckStorages(ctx)
	r.mon.CheckStorages(ctx)
	evs := r.rec.all()
	if len(evs) != 1 || evs[0].Kind != plugin.EventStorageUnreachable || evs[0].Subject != "fs" {
		t.Fatalf("events = %+v, want one storage_unreachable about fs", evs)
	}

	r.vol.set(nil, big)
	r.mon.CheckStorages(ctx)
	r.mon.CheckStorages(ctx)
	if got := r.kinds(); len(got) != 2 || got[1] != plugin.EventRecovered {
		t.Fatalf("events = %v, want unreachable then one recovered", got)
	}
}

func TestMonitorStorageLow(t *testing.T) {
	r := newMonitorRig(t)
	ctx := context.Background()
	const gb = int64(1) << 30
	check := func(free int64) {
		r.vol.set(nil, plugin.Capacity{Total: 100 * gb, Free: free})
		r.mon.CheckStorages(ctx)
	}

	check(50 * gb)
	check(8 * gb) // under 10%: warn
	check(7 * gb) // still warn: no repeat
	check(4 * gb) // under 5%: critical
	check(5 * gb) // above 5%, below the 6% needed to leave critical
	check(9 * gb) // back to warn: quieter, no event
	check(11 * gb)
	check(13 * gb) // above the 12% needed to leave warn: recovered

	got := r.kinds()
	want := []plugin.EventKind{plugin.EventStorageLow, plugin.EventStorageLow, plugin.EventRecovered}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestMonitorStorageLowByRounds: a big volume is still short when it cannot
// take the next few backups.
func TestMonitorStorageLowByRounds(t *testing.T) {
	r := newMonitorRig(t)
	seedHistory(t, r.path, r.taskID, 1, 40<<30) // one 40 GB backup per round
	r.vol.set(nil, plugin.Capacity{Total: 10 << 40, Free: 100 << 30})
	r.mon.CheckStorages(context.Background())
	evs := r.rec.all()
	if len(evs) != 1 || evs[0].Kind != plugin.EventStorageLow {
		t.Fatalf("events = %v, want storage_low", kinds(evs))
	}
}

// TestMonitorSkipsUnusedStorage: nobody writes there, so nobody needs to hear.
func TestMonitorSkipsUnusedStorage(t *testing.T) {
	r := newMonitorRig(t)
	if _, err := rawDB(t, r.path).Exec(`UPDATE task SET enabled = 0`); err != nil {
		t.Fatal(err)
	}
	r.vol.set(errors.New("down"), plugin.Capacity{})
	r.mon.CheckStorages(context.Background())
	if r.vol.opened != 0 {
		t.Fatalf("an unused storage was opened %d times", r.vol.opened)
	}
}

func TestMonitorSlowRun(t *testing.T) {
	r := newMonitorRig(t)
	ctx := context.Background()
	seedHistory(t, r.path, r.taskID, 3, 1<<20) // usual run: 60s
	started := r.now.Add(-10 * time.Minute).Unix()
	if _, err := rawDB(t, r.path).Exec(`INSERT INTO run (task_id, status, attempt, started_at, created_at)
		VALUES (?, 'running', 1, ?, ?)`, r.taskID, started, started); err != nil {
		t.Fatal(err)
	}

	// 10 minutes against a 60s habit is over 3× but under usual + 15m.
	if err := r.mon.CheckRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.kinds(); len(got) != 0 {
		t.Fatalf("alerted inside the margin: %v", got)
	}
	r.now = r.now.Add(10 * time.Minute)
	if err := r.mon.CheckRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.mon.CheckRuns(ctx); err != nil {
		t.Fatal(err)
	}
	evs := r.rec.all()
	if len(evs) != 1 || evs[0].Kind != plugin.EventSlowRun || evs[0].RunID == 0 {
		t.Fatalf("events = %+v, want one slow_run", evs)
	}
}
