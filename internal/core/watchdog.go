package core

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/duskrun/duskrun/internal/plugin"
)

// WatchdogStore is the store surface the Watchdog needs.
type WatchdogStore interface {
	ListEnabledTasks(ctx context.Context) ([]Task, error)
	// LastSuccessByTask returns the finish time of each task's most recent
	// successful run. Tasks that never succeeded are absent from the map.
	LastSuccessByTask(ctx context.Context) (map[int64]time.Time, error)
	ListWatchdogAlerts(ctx context.Context) (map[int64]WatchdogRecord, error)
	MarkWatchdogAlerted(ctx context.Context, rec WatchdogRecord) error
	ClearWatchdogAlert(ctx context.Context, taskID int64) error
}

// WatchdogRecord is the persisted "this task is currently in a stale episode"
// marker. It exists so the alert fires once per episode instead of on every
// tick, and so a daemon restart does not re-alert for a gap already reported.
type WatchdogRecord struct {
	TaskID      int64
	AlertedAt   time.Time
	LastSuccess *time.Time // nil when the task had never succeeded
}

// WatchdogAlert describes a task whose last successful backup is older than its
// threshold — what the dashboard shows and what the notifier sends.
type WatchdogAlert struct {
	TaskID      int64
	Task        string
	Threshold   time.Duration
	LastSuccess *time.Time // nil = never succeeded
	// Since is how long the task has been without a successful backup, measured
	// from the last success or, failing that, from when the task was created.
	Since time.Duration
}

// Reason renders the alert as a short human-readable cause.
func (a WatchdogAlert) Reason() string {
	if a.LastSuccess == nil {
		return fmt.Sprintf("no successful backup yet (threshold %s)", humanDur(a.Threshold))
	}
	return fmt.Sprintf("no successful backup for %s (threshold %s)",
		humanDur(a.Since), humanDur(a.Threshold))
}

// Watchdog reports tasks whose backups have gone stale. Evaluation is pure and
// side-effect free (Alerts); Check adds the notify-once-per-episode bookkeeping.
type Watchdog struct {
	store  WatchdogStore
	now    func() time.Time
	notify *Notifications
	log    *slog.Logger
}

// WatchdogConfig configures a Watchdog. Zero values fall back to defaults.
type WatchdogConfig struct {
	Now func() time.Time
	// Notify delivers watchdog alerts to the task's channels. Nil disables
	// delivery; the alert is still logged and served by GET /watchdog.
	Notify *Notifications
}

// NewWatchdog builds a Watchdog over store.
func NewWatchdog(store WatchdogStore, cfg WatchdogConfig, log *slog.Logger) *Watchdog {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watchdog{store: store, now: cfg.Now, notify: cfg.Notify, log: log}
}

// Alerts returns the tasks that are currently stale, newest gap last. It only
// reads, so the API can serve it on demand without touching alert state.
//
// Only ENABLED tasks are considered: a paused task is not expected to produce
// backups, and paging about one would be noise.
func (w *Watchdog) Alerts(ctx context.Context) ([]WatchdogAlert, error) {
	tasks, err := w.store.ListEnabledTasks(ctx)
	if err != nil {
		return nil, err
	}
	lastSuccess, err := w.store.LastSuccessByTask(ctx)
	if err != nil {
		return nil, err
	}

	now := w.now()
	var out []WatchdogAlert
	for i := range tasks {
		if alert, stale := evaluateWatchdog(&tasks[i], lastSuccess, now); stale {
			out = append(out, alert)
		}
	}
	// Worst first: the longest gap is the one that needs attention.
	sort.Slice(out, func(i, j int) bool { return out[i].Since > out[j].Since })
	return out, nil
}

// Check evaluates every task and notifies about newly stale ones. A task that
// was already reported stays quiet until a successful run closes the episode —
// otherwise every tick would re-send the same alert.
func (w *Watchdog) Check(ctx context.Context) ([]WatchdogAlert, error) {
	tasks, err := w.store.ListEnabledTasks(ctx)
	if err != nil {
		return nil, err
	}
	lastSuccess, err := w.store.LastSuccessByTask(ctx)
	if err != nil {
		return nil, err
	}
	alerted, err := w.store.ListWatchdogAlerts(ctx)
	if err != nil {
		return nil, err
	}

	now := w.now()
	var fired []WatchdogAlert
	live := make(map[int64]bool, len(tasks))
	for i := range tasks {
		task := &tasks[i]
		live[task.ID] = true
		alert, stale := evaluateWatchdog(task, lastSuccess, now)
		_, wasAlerted := alerted[task.ID]

		switch {
		case stale && !wasAlerted:
			rec := WatchdogRecord{TaskID: task.ID, AlertedAt: now, LastSuccess: alert.LastSuccess}
			if err := w.store.MarkWatchdogAlerted(ctx, rec); err != nil {
				// Without the marker the next tick would alert again, so treat a
				// failed write as a reason not to notify at all.
				w.log.Error("watchdog: record alert failed", "task", task.Name, "err", err)
				continue
			}
			w.log.Warn("watchdog: task is stale", "task", task.Name, "reason", alert.Reason())
			w.notify.Send(ctx, task, plugin.EventWatchdog, 0, alert.Reason())
			fired = append(fired, alert)

		case !stale && wasAlerted:
			if err := w.store.ClearWatchdogAlert(ctx, task.ID); err != nil {
				w.log.Error("watchdog: clear alert failed", "task", task.Name, "err", err)
				continue
			}
			w.log.Info("watchdog: task recovered", "task", task.Name)
		}
	}

	// Both branches above only see enabled tasks, so a task paused mid-episode
	// would keep its marker forever — and once re-enabled, wasAlerted would stay
	// true and silence every future episode. Drop the markers of tasks that are
	// no longer being watched.
	for taskID := range alerted {
		if live[taskID] {
			continue
		}
		if err := w.store.ClearWatchdogAlert(ctx, taskID); err != nil {
			w.log.Error("watchdog: clear stale marker failed", "task_id", taskID, "err", err)
		}
	}
	return fired, nil
}

// evaluateWatchdog decides whether one task is stale right now.
func evaluateWatchdog(task *Task, lastSuccess map[int64]time.Time, now time.Time) (WatchdogAlert, bool) {
	threshold, ok := WatchdogThreshold(task)
	if !ok {
		return WatchdogAlert{}, false
	}

	alert := WatchdogAlert{TaskID: task.ID, Task: task.Name, Threshold: threshold}
	// With no success on record the clock runs from task creation, so a task
	// that has never worked is caught instead of being exempt forever.
	since := task.CreatedAt
	if at, found := lastSuccess[task.ID]; found {
		alert.LastSuccess = &at
		since = at
	}
	// A task coming back from a pause gets a fresh window. Its last success may
	// predate the pause by weeks, and judging it by that would fire the instant
	// it is switched on — before it has had any chance to run.
	if task.EnabledAt.After(since) {
		since = task.EnabledAt
	}
	alert.Since = now.Sub(since)
	return alert, alert.Since > threshold
}

// WatchdogThreshold resolves how stale a task's last success may get. It
// reports ok=false when the watchdog is disabled for the task, or when the
// threshold cannot be derived because the cron does not parse.
func WatchdogThreshold(task *Task) (time.Duration, bool) {
	if task.Watchdog < 0 {
		return 0, false // WatchdogOff
	}
	if task.Watchdog > 0 {
		return task.Watchdog, true
	}
	interval, err := cronMaxInterval(task.Cron, task.CreatedAt)
	if err != nil {
		return 0, false
	}
	// Two missed windows, so a single hiccup (one failed run that the next
	// scheduled run recovers) does not page anyone.
	threshold := 2 * interval
	if threshold < minWatchdogThreshold {
		threshold = minWatchdogThreshold
	}
	return threshold, true
}

// minWatchdogThreshold floors the derived threshold. A task running every
// minute would otherwise get a two-minute window, where a single slow backup
// reads as an outage.
const minWatchdogThreshold = time.Hour

const (
	// cronSampleWindow is how far ahead the derivation looks. It must span a
	// whole week: a business-hours schedule like "0 9-18 * * 1-5" has nothing
	// but one-hour gaps inside a working day, and its real idle stretches — 15h
	// overnight, 63h over the weekend — only appear once a weekend is in view.
	cronSampleWindow = 8 * 24 * time.Hour
	// cronMinSamples keeps sparse schedules honest: a weekly or monthly cron has
	// no activation inside the window at all, so a few gaps are taken regardless.
	cronMinSamples = 8
	// cronSampleLimit bounds the activation walk. A per-minute cron would need
	// ~11500 steps to cross the window, and the watchdog runs every minute over
	// every task, so the walk gives up instead — cronProbeStep takes over.
	cronSampleLimit = 400
	// cronProbeStep is the resolution of the fallback scan for dense schedules.
	// Probing costs a fixed ~1150 lookups over the window no matter how many
	// activations it contains, where walking them one by one does not terminate
	// in useful time.
	cronProbeStep = 10 * time.Minute
)

// cronMaxInterval is the LARGEST gap between upcoming activations of expr, over
// a window wide enough to see weekly structure. The maximum, not the first gap:
// a clustered schedule like "0 1,2 * * *" starts with a one-hour gap and then
// waits 23 hours, and deriving from the short gap would alert every night.
func cronMaxInterval(expr string, from time.Time) (time.Duration, error) {
	sch, err := ParseCron(expr)
	if err != nil {
		return 0, err
	}
	if sch.Next(from).IsZero() {
		return 0, fmt.Errorf("cron %q: no upcoming activation", expr)
	}

	max, covered := walkCronGaps(sch, from)
	if !covered {
		// A dense schedule ran out of budget before crossing the window, so the
		// walk has only seen gaps from one small stretch of it. "*/1 * * * 1-5"
		// looks like nothing but one-minute gaps for thousands of steps while
		// the real idle stretch is the weekend.
		if probed := probeCronGaps(sch, from); probed > max {
			max = probed
		}
	}
	if max <= 0 {
		return 0, fmt.Errorf("cron %q: could not derive an interval", expr)
	}
	return max, nil
}

// walkCronGaps steps activation by activation, returning the widest gap seen and
// whether it got far enough to trust that answer. Exact, and cheap for the
// ordinary case: a daily cron settles in eight steps.
func walkCronGaps(sch cron.Schedule, from time.Time) (max time.Duration, covered bool) {
	deadline := from.Add(cronSampleWindow)
	prev := sch.Next(from)
	for seen := 0; seen < cronSampleLimit; seen++ {
		next := sch.Next(prev)
		if next.IsZero() {
			return max, true // the schedule simply ends; nothing more to see
		}
		if gap := next.Sub(prev); gap > max {
			max = gap
		}
		prev = next
		if !prev.Before(deadline) && seen+1 >= cronMinSamples {
			return max, true
		}
	}
	return max, false
}

// probeCronGaps finds the widest idle stretch without visiting every activation.
// The wait from an instant just after an activation IS the gap that follows it,
// so the widest wait over evenly spaced probes is the widest gap — understated
// by at most one probe step, which is immaterial against a threshold that is
// then doubled and floored at an hour.
func probeCronGaps(sch cron.Schedule, from time.Time) time.Duration {
	var max time.Duration
	end := from.Add(cronSampleWindow)
	for at := from; at.Before(end); at = at.Add(cronProbeStep) {
		next := sch.Next(at)
		if next.IsZero() {
			break
		}
		if wait := next.Sub(at); wait > max {
			max = wait
		}
	}
	return max
}

// humanDur renders a duration for a human-facing message. Duration.String()
// would give "48h0m0s"; these strings go out to Telegram and webhooks, so the
// zero components are noise.
func humanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
