package core

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/CoOre/duskrun/internal/plugin"
)

// AlertStore keeps alert episodes (alert_state): an alert fires when its
// episode opens or gets worse, the recovery when it closes, and both survive a
// restart.
type AlertStore interface {
	// OpenAlert records the episode at level and returns the level it had
	// before; 0 means it was closed.
	OpenAlert(ctx context.Context, kind, subject string, level int, at time.Time) (int, error)
	CloseAlert(ctx context.Context, kind, subject string) (bool, error)
	AlertLevel(ctx context.Context, kind, subject string) (int, error)
}

// RunHistoryStore is what the run-derived alerts judge a run against.
type RunHistoryStore interface {
	RecentArtifactSizes(ctx context.Context, taskID, beforeRunID int64, n int) ([]int64, error)
	RecentRunDurations(ctx context.Context, taskID int64, n int) ([]time.Duration, error)
}

// Alert episode kinds and subjects as stored in alert_state.
const (
	alertFailure = "failure"
)

func taskSubject(id int64) string    { return fmt.Sprintf("task:%d", id) }
func storageSubject(id int64) string { return fmt.Sprintf("storage:%d", id) }

const (
	// historyRuns is how many recent successful runs "usual" is taken from.
	historyRuns = 5
	// minHistory is how many of them are needed before anything is judged
	// unusual: two runs are not a habit.
	minHistory = 3
	// sizeAnomalyRatio: a backup below this share of the usual size is
	// reported. Half catches a dropped schema or a dump cut short, and stays
	// clear of the swing compression and ordinary deletes produce.
	sizeAnomalyRatio = 0.5
	// sizeAnomalyFloor ignores shrinkage smaller than this in absolute terms:
	// on a database of a few kilobytes, halving is noise.
	sizeAnomalyFloor = 64 << 10
)

// succeeded reports a successful run: the success itself, a size far below the
// task's usual, and the end of a failure or watchdog episode.
func (p *Pool) succeeded(ctx context.Context, task *Task, runID int64, size int64) {
	p.notify.Send(ctx, task, plugin.EventSuccess, runID, "backup completed")
	if msg, ok := p.sizeAnomaly(ctx, task, runID, size); ok {
		p.log.Warn("worker: backup much smaller than usual", "task", task.Name, "run", runID, "size", size)
		p.notify.Send(ctx, task, plugin.EventSizeAnomaly, runID, msg)
	}
	if msg, ok := p.recovery(ctx, task); ok {
		p.notify.Send(ctx, task, plugin.EventRecovered, runID, msg)
	}
}

// failedTerminally reports a run whose retries are exhausted and opens the
// failure episode its next success will close.
func (p *Pool) failedTerminally(ctx context.Context, task *Task, runID int64, cause error) {
	p.notify.Send(ctx, task, plugin.EventFailure, runID, cause.Error())
	if _, err := p.store.OpenAlert(context.WithoutCancel(ctx), alertFailure, taskSubject(task.ID), 1, p.now()); err != nil {
		p.log.Error("worker: record failure episode", "task", task.Name, "err", err)
	}
}

func (p *Pool) sizeAnomaly(ctx context.Context, task *Task, runID, size int64) (string, bool) {
	sizes, err := p.store.RecentArtifactSizes(ctx, task.ID, runID, historyRuns)
	if err != nil {
		p.log.Error("worker: size history", "task", task.Name, "err", err)
		return "", false
	}
	if len(sizes) < minHistory {
		return "", false
	}
	usual := median(sizes)
	if float64(size) >= float64(usual)*sizeAnomalyRatio || usual-size < sizeAnomalyFloor {
		return "", false
	}
	if size == 0 {
		return fmt.Sprintf("бэкап пустой (0 Б), обычно %s — проверьте, что дамп снят с нужной базы",
			HumanBytes(usual)), true
	}
	return fmt.Sprintf("размер %s — в %.1f раза меньше обычного (%s, медиана %d последних) — проверьте, что дамп полный",
		HumanBytes(size), float64(usual)/float64(size), HumanBytes(usual), len(sizes)), true
}

// recovery closes the task's failure and watchdog episodes and describes what
// it recovered from; ok is false when there was nothing open.
func (p *Pool) recovery(ctx context.Context, task *Task) (string, bool) {
	dctx := context.WithoutCancel(ctx)
	failed, err := p.store.CloseAlert(dctx, alertFailure, taskSubject(task.ID))
	if err != nil {
		p.log.Error("worker: close failure episode", "task", task.Name, "err", err)
	}
	// The success closes the watchdog episode too, here rather than on the
	// watchdog's next tick, so one recovery is reported, not two.
	stale := false
	if alerts, err := p.store.ListWatchdogAlerts(dctx); err != nil {
		p.log.Error("worker: list watchdog alerts", "task", task.Name, "err", err)
	} else if _, ok := alerts[task.ID]; ok {
		stale = true
		if err := p.store.ClearWatchdogAlert(dctx, task.ID); err != nil {
			p.log.Error("worker: clear watchdog alert", "task", task.Name, "err", err)
		}
	}
	switch {
	case failed:
		return "бэкап снова выполнен успешно после сбоя", true
	case stale:
		return "появился свежий успешный бэкап", true
	}
	return "", false
}

// InterruptedStore is what reporting interrupted runs needs.
type InterruptedStore interface {
	AlertStore
	GetRun(ctx context.Context, id int64) (*Run, error)
	GetTask(ctx context.Context, id int64) (*Task, error)
}

// ReportInterrupted tells each task's channels that its run was cut short by a
// daemon restart. runIDs are the runs ReapStale just marked failed; without
// this they would be visible only in the history and the startup log.
func ReportInterrupted(ctx context.Context, store InterruptedStore, notify *Notifications, runIDs []int64, now time.Time, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for _, id := range runIDs {
		run, err := store.GetRun(ctx, id)
		if err != nil {
			log.Error("interrupted run: load", "run", id, "err", err)
			continue
		}
		task, err := store.GetTask(ctx, run.TaskID)
		if err != nil {
			log.Error("interrupted run: load task", "run", id, "err", err)
			continue
		}
		if _, err := store.OpenAlert(ctx, alertFailure, taskSubject(task.ID), 1, now); err != nil {
			log.Error("interrupted run: record failure episode", "task", task.Name, "err", err)
		}
		msg := "Duskrun был остановлен во время бэкапа, запуск помечен неудачным"
		if run.StartedAt != nil {
			msg += fmt.Sprintf(" (шёл с %s UTC)", run.StartedAt.UTC().Format("2006-01-02 15:04"))
		}
		notify.Send(ctx, task, plugin.EventInterrupted, run.ID, msg)
	}
}

// median of a non-empty slice; the input is left as it was.
func median[T int64 | time.Duration](xs []T) T {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// plural picks the Russian form for n: one, few (2–4), many.
func plural(n int64, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}

// HumanBytes renders a size for a message, in binary units with Russian
// suffixes to match the rest of the text.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	units := []string{"КБ", "МБ", "ГБ", "ТБ", "ПБ"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// HumanDuration renders a duration in Russian for a message: "47 мин",
// "2 ч 5 мин".
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		if mins := int(d.Minutes()) % 60; mins != 0 {
			return fmt.Sprintf("%d ч %d мин", h, mins)
		}
		return fmt.Sprintf("%d ч", h)
	default:
		return fmt.Sprintf("%d дн", int(d.Hours())/24)
	}
}
