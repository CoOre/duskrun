package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/CoOre/duskrun/internal/plugin"
)

// MonitorStore is the store surface the Monitor needs.
type MonitorStore interface {
	AlertStore
	RunHistoryStore
	ListRuns(ctx context.Context, taskID int64, status string) ([]Run, error)
	GetTask(ctx context.Context, id int64) (*Task, error)
	ListEnabledTasks(ctx context.Context) ([]Task, error)
	ListStorages(ctx context.Context) ([]Storage, error)
	LastSuccessfulArtifactSize(ctx context.Context, taskID int64) (int64, error)
}

// MonitorConfig configures a Monitor. Zero values fall back to defaults.
type MonitorConfig struct {
	Now    func() time.Time
	Notify *Notifications
	// Secrets resolves `<key>_ref` in storage configs, as for a run.
	Secrets *SecretResolver
	// OpenStorage builds a storage plugin; tests inject fakes.
	OpenStorage func(ctx context.Context, st *Storage) (plugin.Storage, error)
	// RunInterval is how often running runs are checked (default 1m).
	RunInterval time.Duration
	// StorageInterval is how often storages are probed and measured
	// (default 15m). Each check dials every sftp host, so not every minute.
	StorageInterval time.Duration
	// ProbeTimeout bounds one storage's check (default 30s).
	ProbeTimeout time.Duration
}

// Monitor watches what no single run reports: a run going far longer than
// usual, a storage that stopped answering or is filling up.
type Monitor struct {
	store       MonitorStore
	now         func() time.Time
	notify      *Notifications
	openStorage func(ctx context.Context, st *Storage) (plugin.Storage, error)
	runTick     time.Duration
	storageTick time.Duration
	timeout     time.Duration
	log         *slog.Logger

	mu sync.Mutex
	// slow holds the runs already reported as slow. In memory is enough: a
	// restart reaps every running run anyway.
	slow map[int64]bool
	// misses counts consecutive failed probes per storage. One miss is a blip;
	// the alert waits for the second.
	misses map[int64]int
}

const (
	// slowFactor and slowMargin: a run is slow past max(factor × usual,
	// usual + margin). The margin keeps a 20-second backup from alerting at
	// one minute.
	slowFactor = 3
	slowMargin = 15 * time.Minute
	// unreachableAfter is how many consecutive failed probes make an alert.
	unreachableAfter = 2
)

// Storage-low thresholds: a share of the volume, or a number of "rounds" —
// one more backup of every task writing there, at its last size.
const (
	lowWarn         = 1
	lowCritical     = 2
	lowWarnShare    = 0.10
	lowWarnRounds   = 3
	lowCritShare    = 0.05
	lowCritRounds   = 1
	lowRecoverSlack = 1.2 // leaving a level needs 20% more than entering it
)

// NewMonitor builds a Monitor over store.
func NewMonitor(store MonitorStore, cfg MonitorConfig, log *slog.Logger) *Monitor {
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
	if cfg.RunInterval <= 0 {
		cfg.RunInterval = time.Minute
	}
	if cfg.StorageInterval <= 0 {
		cfg.StorageInterval = 15 * time.Minute
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 30 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{
		store: store, now: cfg.Now, notify: cfg.Notify, openStorage: cfg.OpenStorage,
		runTick: cfg.RunInterval, storageTick: cfg.StorageInterval, timeout: cfg.ProbeTimeout,
		log: log, slow: map[int64]bool{}, misses: map[int64]int{},
	}
}

// Run checks runs and storages on their tickers until ctx is cancelled.
// Storages are checked once right away, so a destination that went missing
// while the daemon was down is known before the first backup needs it.
func (m *Monitor) Run(ctx context.Context) {
	runs := time.NewTicker(m.runTick)
	defer runs.Stop()
	storages := time.NewTicker(m.storageTick)
	defer storages.Stop()

	m.CheckStorages(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-runs.C:
			if err := m.CheckRuns(ctx); err != nil && ctx.Err() == nil {
				m.log.Error("monitor: run check failed", "err", err)
			}
		case <-storages.C:
			m.CheckStorages(ctx)
		}
	}
}

// CheckRuns reports each running run that has gone well past its task's usual
// duration, once per run.
func (m *Monitor) CheckRuns(ctx context.Context) error {
	running, err := m.store.ListRuns(ctx, 0, string(StatusRunning))
	if err != nil {
		return err
	}
	now := m.now()
	live := make(map[int64]bool, len(running))
	for i := range running {
		run := &running[i]
		live[run.ID] = true
		if run.StartedAt == nil || m.reported(run.ID) {
			continue
		}
		durs, err := m.store.RecentRunDurations(ctx, run.TaskID, historyRuns)
		if err != nil {
			return err
		}
		if len(durs) < minHistory {
			continue
		}
		usual := median(durs)
		limit := max(usual*slowFactor, usual+slowMargin)
		elapsed := now.Sub(*run.StartedAt)
		if elapsed < limit {
			continue
		}
		task, err := m.store.GetTask(ctx, run.TaskID)
		if err != nil {
			return err
		}
		m.markReported(run.ID)
		m.log.Warn("monitor: run is slow", "task", task.Name, "run", run.ID, "elapsed", elapsed, "usual", usual)
		m.notify.Send(ctx, task, plugin.EventSlowRun, run.ID,
			fmt.Sprintf("идёт уже %s, обычно %s (медиана %d последних)", HumanDuration(elapsed), HumanDuration(usual), len(durs)))
	}
	m.forgetFinished(live)
	return nil
}

func (m *Monitor) reported(runID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slow[runID]
}

func (m *Monitor) markReported(runID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slow[runID] = true
}

func (m *Monitor) forgetFinished(live map[int64]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.slow {
		if !live[id] {
			delete(m.slow, id)
		}
	}
}

// CheckStorages probes and measures every storage an enabled task writes to.
// Storages no task uses are left alone: an alert about one would be noise.
func (m *Monitor) CheckStorages(ctx context.Context) {
	tasks, err := m.store.ListEnabledTasks(ctx)
	if err != nil {
		m.log.Error("monitor: list tasks", "err", err)
		return
	}
	storages, err := m.store.ListStorages(ctx)
	if err != nil {
		m.log.Error("monitor: list storages", "err", err)
		return
	}
	users := map[int64][]Task{}
	for _, t := range tasks {
		users[t.StorageID] = append(users[t.StorageID], t)
	}

	var wg sync.WaitGroup
	for i := range storages {
		st := &storages[i]
		if len(users[st.ID]) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.checkStorage(ctx, st, users[st.ID])
		}()
	}
	wg.Wait()
}

func (m *Monitor) checkStorage(ctx context.Context, st *Storage, tasks []Task) {
	cctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	storage, err := m.openStorage(cctx, st)
	if err == nil {
		defer CloseStorage(storage)
		if p, ok := storage.(plugin.Prober); ok {
			err = p.Probe(cctx)
		}
	}
	if ctx.Err() != nil {
		return // shutting down: says nothing about the storage
	}
	if err != nil {
		m.unreachable(ctx, st, err)
		return
	}
	m.reachable(ctx, st)

	cr, ok := storage.(plugin.CapacityReporter)
	if !ok {
		return
	}
	capa, err := cr.Capacity(cctx)
	if err != nil || capa.Total <= 0 {
		// Reachable but unmeasurable (an sftp server without statvfs): there
		// is nothing to judge, which is not a problem to report.
		return
	}
	var round int64
	for _, t := range tasks {
		size, err := m.store.LastSuccessfulArtifactSize(ctx, t.ID)
		if err != nil {
			m.log.Error("monitor: last artifact size", "task", t.Name, "err", err)
			continue
		}
		round += size
	}
	m.judgeCapacity(ctx, st, capa, round)
}

func (m *Monitor) unreachable(ctx context.Context, st *Storage, cause error) {
	m.mu.Lock()
	m.misses[st.ID]++
	misses := m.misses[st.ID]
	m.mu.Unlock()
	m.log.Warn("monitor: storage probe failed", "storage", st.Name, "misses", misses, "err", cause)
	if misses < unreachableAfter {
		return
	}
	prev, err := m.store.OpenAlert(ctx, string(plugin.EventStorageUnreachable), storageSubject(st.ID), 1, m.now())
	if err != nil {
		m.log.Error("monitor: record unreachable storage", "storage", st.Name, "err", err)
		return
	}
	if prev == 0 {
		m.notify.SendSystem(ctx, plugin.EventStorageUnreachable, st.Name,
			fmt.Sprintf("хранилище %s (%s) не отвечает: %v", st.Name, st.Type, cause))
	}
}

func (m *Monitor) reachable(ctx context.Context, st *Storage) {
	m.mu.Lock()
	delete(m.misses, st.ID)
	m.mu.Unlock()
	was, err := m.store.CloseAlert(ctx, string(plugin.EventStorageUnreachable), storageSubject(st.ID))
	if err != nil {
		m.log.Error("monitor: close unreachable storage", "storage", st.Name, "err", err)
		return
	}
	if was {
		m.notify.SendSystem(ctx, plugin.EventRecovered, st.Name, fmt.Sprintf("хранилище %s снова доступно", st.Name))
	}
}

func (m *Monitor) judgeCapacity(ctx context.Context, st *Storage, capa plugin.Capacity, round int64) {
	subject := storageSubject(st.ID)
	kind := string(plugin.EventStorageLow)
	prev, err := m.store.AlertLevel(ctx, kind, subject)
	if err != nil {
		m.log.Error("monitor: storage low level", "storage", st.Name, "err", err)
		return
	}
	level := lowLevel(capa, round, prev)
	if level == prev {
		return
	}
	switch level {
	case 0:
		if _, err := m.store.CloseAlert(ctx, kind, subject); err != nil {
			m.log.Error("monitor: close storage low", "storage", st.Name, "err", err)
			return
		}
		m.notify.SendSystem(ctx, plugin.EventRecovered, st.Name, "место освободилось: "+describeSpace(capa, round))
	default:
		if _, err := m.store.OpenAlert(ctx, kind, subject, level, m.now()); err != nil {
			m.log.Error("monitor: record storage low", "storage", st.Name, "err", err)
			return
		}
		if level < prev {
			return // better, still low: the alert already went out
		}
		msg := "заканчивается место: "
		if level == lowCritical {
			msg = "места почти не осталось: "
		}
		m.log.Warn("monitor: storage low", "storage", st.Name, "free", capa.Free, "total", capa.Total, "level", level)
		m.notify.SendSystem(ctx, plugin.EventStorageLow, st.Name, msg+describeSpace(capa, round))
	}
}

// lowLevel grades free space: 0 fine, lowWarn, lowCritical. A level already
// reached is only left with lowRecoverSlack to spare, so a volume hovering at
// a threshold does not alert on every check.
func lowLevel(c plugin.Capacity, round int64, prev int) int {
	below := func(level int, share float64, rounds int64) bool {
		slack := 1.0
		if prev >= level {
			slack = lowRecoverSlack
		}
		free := float64(c.Free)
		if free < float64(c.Total)*share*slack {
			return true
		}
		return round > 0 && free < float64(round*rounds)*slack
	}
	switch {
	case below(lowCritical, lowCritShare, lowCritRounds):
		return lowCritical
	case below(lowWarn, lowWarnShare, lowWarnRounds):
		return lowWarn
	}
	return 0
}

func describeSpace(c plugin.Capacity, round int64) string {
	s := fmt.Sprintf("свободно %s из %s (%.0f%%)", HumanBytes(c.Free), HumanBytes(c.Total), float64(c.Free)*100/float64(c.Total))
	if round > 0 {
		n := c.Free / round
		s += fmt.Sprintf(", этого хватит примерно на %d %s всех задач", n, plural(n, "бэкап", "бэкапа", "бэкапов"))
	}
	return s
}
