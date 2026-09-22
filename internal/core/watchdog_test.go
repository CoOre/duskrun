package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/plugin"
)

var wdNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// fakeWatchdogStore is an in-memory WatchdogStore.
type fakeWatchdogStore struct {
	tasks       []Task
	lastSuccess map[int64]time.Time
	alerts      map[int64]WatchdogRecord

	tasksErr error
	markErr  error
	cleared  []int64
}

func (f *fakeWatchdogStore) ListEnabledTasks(context.Context) ([]Task, error) {
	return f.tasks, f.tasksErr
}
func (f *fakeWatchdogStore) LastSuccessByTask(context.Context) (map[int64]time.Time, error) {
	if f.lastSuccess == nil {
		return map[int64]time.Time{}, nil
	}
	return f.lastSuccess, nil
}
func (f *fakeWatchdogStore) ListWatchdogAlerts(context.Context) (map[int64]WatchdogRecord, error) {
	if f.alerts == nil {
		f.alerts = map[int64]WatchdogRecord{}
	}
	return f.alerts, nil
}
func (f *fakeWatchdogStore) MarkWatchdogAlerted(_ context.Context, rec WatchdogRecord) error {
	if f.markErr != nil {
		return f.markErr
	}
	if f.alerts == nil {
		f.alerts = map[int64]WatchdogRecord{}
	}
	f.alerts[rec.TaskID] = rec
	return nil
}
func (f *fakeWatchdogStore) ClearWatchdogAlert(_ context.Context, taskID int64) error {
	f.cleared = append(f.cleared, taskID)
	delete(f.alerts, taskID)
	return nil
}

func newWatchdog(store *fakeWatchdogStore, spy *spyNotifier) *Watchdog {
	return NewWatchdog(store, WatchdogConfig{
		Now:    func() time.Time { return wdNow },
		Notify: testNotifications(spy),
	}, nil)
}

// dailyTask is a task on a daily cron, so the derived threshold is 48h.
func dailyTask(id int64, name string) Task {
	return Task{ID: id, Name: name, Cron: "0 2 * * *", Enabled: true, CreatedAt: wdNow.Add(-30 * 24 * time.Hour)}
}

func TestWatchdogThresholdDerivedFromCron(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want time.Duration
		ok   bool
	}{
		{"daily → two windows", Task{Cron: "0 2 * * *"}, 48 * time.Hour, true},
		{"hourly → two windows", Task{Cron: "0 * * * *"}, 2 * time.Hour, true},
		{"weekly → two windows", Task{Cron: "0 3 * * 0"}, 14 * 24 * time.Hour, true},
		// Every 5 minutes would derive a 10-minute window; the floor keeps one
		// slow backup from reading as an outage.
		{"sub-hour is floored", Task{Cron: "*/5 * * * *"}, time.Hour, true},
		// A clustered schedule waits 23h between the evening and morning runs.
		// Deriving from the FIRST gap (1h) would alert every single night.
		{"clustered uses the widest gap", Task{Cron: "0 1,2 * * *"}, 46 * time.Hour, true},
		// Business hours: every gap inside a working day is 1h, so a short sample
		// derives 2h and alerts every single night. The real idle stretches only
		// appear once the window reaches the weekend — Fri 18:00 → Mon 09:00.
		{"business hours sees the weekend", Task{Cron: "0 9-18 * * 1-5"}, 2 * 63 * time.Hour, true},
		{"weekday nights", Task{Cron: "0 9,18 * * 1-5"}, 2 * 63 * time.Hour, true},
		{"monthly", Task{Cron: "0 3 1 * *"}, 2 * 31 * 24 * time.Hour, true},
		// Dense schedules with a restricted range: walking activation by
		// activation never reaches the idle stretch (a per-minute weekday cron
		// needs ~6500 steps to reach Friday evening), so the probe pass has to
		// find it. Understated by at most one probe step, hence the tolerance.
		{"per-minute business hours", Task{Cron: "*/1 8-20 * * *"}, 22 * time.Hour, true},
		// All hours Mon-Fri: the idle stretch is Fri 23:59 → Mon 00:00 = 48h,
		// unlike the business-hours case where the daily restriction widens it.
		{"per-minute weekdays", Task{Cron: "*/1 * * * 1-5"}, 96 * time.Hour, true},
		{"explicit override wins", Task{Cron: "0 2 * * *", Watchdog: 6 * time.Hour}, 6 * time.Hour, true},
		{"off disables", Task{Cron: "0 2 * * *", Watchdog: WatchdogOff}, 0, false},
		{"unparseable cron disables", Task{Cron: "not a cron"}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := c.task
			task.CreatedAt = wdNow
			got, ok := WatchdogThreshold(&task)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			// The probe fallback can understate by up to one step (doubled), so
			// exact equality is only required of the exact walk.
			if ok && absDur(got-c.want) > 2*cronProbeStep {
				t.Errorf("threshold = %s, want ~%s", got, c.want)
			}
		})
	}
}

func TestWatchdogAlertsOnStaleTask(t *testing.T) {
	store := &fakeWatchdogStore{
		tasks: []Task{dailyTask(1, "fresh"), dailyTask(2, "stale")},
		lastSuccess: map[int64]time.Time{
			1: wdNow.Add(-3 * time.Hour),  // well inside the 48h window
			2: wdNow.Add(-72 * time.Hour), // past it
		},
	}

	alerts, err := newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Task != "stale" {
		t.Fatalf("alerts = %+v, want only the stale task", alerts)
	}
	if alerts[0].Since != 72*time.Hour {
		t.Errorf("since = %s, want 72h", alerts[0].Since)
	}
	if alerts[0].Threshold != 48*time.Hour {
		t.Errorf("threshold = %s, want the derived 48h", alerts[0].Threshold)
	}
}

// TestWatchdogAlertsOnNeverSucceeded: a task that has never produced a backup
// is the most important case — measuring from task creation catches it instead
// of exempting it forever for lack of a reference point.
func TestWatchdogAlertsOnNeverSucceeded(t *testing.T) {
	task := dailyTask(1, "never-worked")
	store := &fakeWatchdogStore{tasks: []Task{task}}

	alerts, err := newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want the never-successful task", alerts)
	}
	if alerts[0].LastSuccess != nil {
		t.Errorf("last_success = %v, want nil", alerts[0].LastSuccess)
	}
	if got := alerts[0].Reason(); got != "no successful backup yet (threshold 2d)" {
		t.Errorf("reason = %q", got)
	}
}

// TestWatchdogIgnoresDisabledWatchdog: an explicit opt-out silences a task that
// would otherwise be reported. (Paused tasks are excluded upstream, by
// ListEnabledTasks — a task on hold is not expected to produce backups.)
func TestWatchdogIgnoresDisabledWatchdog(t *testing.T) {
	task := dailyTask(1, "opted-out")
	task.Watchdog = WatchdogOff
	store := &fakeWatchdogStore{tasks: []Task{task}}

	alerts, err := newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("alerts = %+v, want none when the watchdog is off", alerts)
	}
}

// TestWatchdogNotifiesOncePerEpisode: the check runs every minute, so a stale
// task must not re-send its alert on every tick.
func TestWatchdogNotifiesOncePerEpisode(t *testing.T) {
	ctx := context.Background()
	store := &fakeWatchdogStore{
		tasks:       []Task{dailyTask(1, "stale")},
		lastSuccess: map[int64]time.Time{1: wdNow.Add(-72 * time.Hour)},
	}
	spy := &spyNotifier{}
	wd := newWatchdog(store, spy)

	fired, err := wd.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("first check fired %d alerts, want 1", len(fired))
	}
	if len(spy.events) != 1 {
		t.Fatalf("events = %d, want 1", len(spy.events))
	}
	if spy.events[0].Kind != plugin.EventWatchdog {
		t.Errorf("kind = %q, want watchdog", spy.events[0].Kind)
	}

	// Second tick, nothing changed: silence.
	fired, err = wd.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fired) != 0 || len(spy.events) != 1 {
		t.Errorf("second check fired %d alerts / %d events, want 0 / 1", len(fired), len(spy.events))
	}
}

// TestWatchdogRecoversAndCanAlertAgain: a success closes the episode, and a
// later gap opens a new one.
func TestWatchdogRecoversAndCanAlertAgain(t *testing.T) {
	ctx := context.Background()
	store := &fakeWatchdogStore{
		tasks:       []Task{dailyTask(1, "flaky")},
		lastSuccess: map[int64]time.Time{1: wdNow.Add(-72 * time.Hour)},
	}
	spy := &spyNotifier{}
	wd := newWatchdog(store, spy)

	if _, err := wd.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}

	// A backup succeeds: the episode closes.
	store.lastSuccess[1] = wdNow.Add(-time.Hour)
	if _, err := wd.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(store.cleared) != 1 || store.cleared[0] != 1 {
		t.Fatalf("cleared = %v, want the task's alert cleared on recovery", store.cleared)
	}

	// It goes stale again: a new episode alerts.
	store.lastSuccess[1] = wdNow.Add(-96 * time.Hour)
	fired, err := wd.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fired) != 1 || len(spy.events) != 2 {
		t.Errorf("fired %d / events %d, want a second alert after recovery", len(fired), len(spy.events))
	}
}

// TestWatchdogStaysSilentIfRecordFails: without a persisted marker the next tick
// would alert again, so a failed write must suppress the notification rather
// than start a repeating page.
func TestWatchdogStaysSilentIfRecordFails(t *testing.T) {
	store := &fakeWatchdogStore{
		tasks:       []Task{dailyTask(1, "stale")},
		lastSuccess: map[int64]time.Time{1: wdNow.Add(-72 * time.Hour)},
		markErr:     errors.New("db is down"),
	}
	spy := &spyNotifier{}

	fired, err := newWatchdog(store, spy).Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fired) != 0 || len(spy.events) != 0 {
		t.Errorf("fired %d / events %d, want silence when the marker cannot be stored", len(fired), len(spy.events))
	}
}

// TestWatchdogAlertsSortedWorstFirst: the dashboard shows a few rows, so the
// longest outage must not be the one that gets truncated away.
func TestWatchdogAlertsSortedWorstFirst(t *testing.T) {
	store := &fakeWatchdogStore{
		tasks: []Task{dailyTask(1, "bad"), dailyTask(2, "worse")},
		lastSuccess: map[int64]time.Time{
			1: wdNow.Add(-72 * time.Hour),
			2: wdNow.Add(-200 * time.Hour),
		},
	}
	alerts, err := newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 2 || alerts[0].Task != "worse" {
		t.Fatalf("alerts = %+v, want the longest gap first", alerts)
	}
}

// TestHumanDur pins the message formatting: these strings go to Telegram and
// webhooks, where Duration.String()'s "48h0m0s" reads as noise.
func TestHumanDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{90 * time.Minute, "1h"},
		{31 * time.Hour, "31h"},
		{48 * time.Hour, "2d"},
		{200 * time.Hour, "8d"},
	}
	for _, c := range cases {
		if got := humanDur(c.d); got != c.want {
			t.Errorf("humanDur(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestWatchdogGivesGraceAfterEnable: a task returning from a pause has not had
// a chance to run yet. Judging it by a success from before the pause would fire
// the moment it is switched back on.
func TestWatchdogGivesGraceAfterEnable(t *testing.T) {
	task := dailyTask(1, "just-resumed")
	task.EnabledAt = wdNow.Add(-2 * time.Hour) // switched on two hours ago
	store := &fakeWatchdogStore{
		tasks:       []Task{task},
		lastSuccess: map[int64]time.Time{1: wdNow.Add(-30 * 24 * time.Hour)}, // long before the pause
	}

	alerts, err := newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none within the grace window after enabling", alerts)
	}

	// Once the fresh window elapses without a success, it does alert.
	task.EnabledAt = wdNow.Add(-72 * time.Hour)
	store.tasks = []Task{task}
	alerts, err = newWatchdog(store, nil).Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Errorf("alerts = %+v, want one once the grace window passed", alerts)
	}
}

// TestWatchdogClearsMarkerForUnwatchedTask: a task paused mid-episode drops out
// of the enabled set. Its marker must go too — otherwise re-enabling it leaves
// wasAlerted true and silences every future episode.
func TestWatchdogClearsMarkerForUnwatchedTask(t *testing.T) {
	ctx := context.Background()
	store := &fakeWatchdogStore{
		tasks:       []Task{dailyTask(1, "stale")},
		lastSuccess: map[int64]time.Time{1: wdNow.Add(-72 * time.Hour)},
	}
	spy := &spyNotifier{}
	wd := newWatchdog(store, spy)

	if _, err := wd.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(store.alerts) != 1 {
		t.Fatalf("alerts = %v, want the episode recorded", store.alerts)
	}

	// The operator pauses the task: it leaves the enabled set.
	store.tasks = nil
	if _, err := wd.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(store.alerts) != 0 {
		t.Fatalf("alerts = %v, want the marker dropped for an unwatched task", store.alerts)
	}

	// Re-enabled and still stale (enabled long ago): a new episode must alert.
	task := dailyTask(1, "stale")
	task.EnabledAt = wdNow.Add(-96 * time.Hour)
	store.tasks = []Task{task}
	fired, err := wd.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(fired) != 1 || len(spy.events) != 2 {
		t.Errorf("fired %d / events %d, want an alert after re-enabling", len(fired), len(spy.events))
	}
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
