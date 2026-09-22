package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/store/sqlite"
)

// watchdogServer seeds a store with one daily task and serves the watchdog
// endpoint over it. The raw handle lets a test backdate created_at, which is
// otherwise stamped by the store.
func watchdogServer(t *testing.T) (*httptest.Server, *sqlite.Store, *sql.DB, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchdog.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	storageID, err := st.CreateStorage(ctx, core.Storage{Name: "s", Type: "localfs", Config: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	connID, err := st.CreateConnection(ctx, core.Connection{Name: "c", Engine: "postgres", ConnectorType: "direct", ConnectorConfig: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(ctx, core.Task{
		Name: "nightly", ConnectionID: connID, StorageID: storageID,
		DumperOpts: []byte(`{}`), CodecChain: []string{}, Cron: "0 2 * * *", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	wd := core.NewWatchdog(st, core.WatchdogConfig{}, nil)
	srv := httptest.NewServer(NewRouter(Deps{
		Store: st, Token: testToken, Watchdog: wd,
		ToolCheck: func(string) error { return nil },
	}))
	t.Cleanup(srv.Close)
	return srv, st, db, taskID
}

// backdateTask ages a task, standing in for one that has existed and been
// enabled long enough to be judged. Both stamps move: the watchdog measures
// from the later of the last success and enabled_at.
func backdateTask(t *testing.T, db *sql.DB, taskID int64, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d).Unix()
	mustExec(t, db, `UPDATE task SET created_at = ?, enabled_at = ? WHERE id = ?`, at, at, taskID)
}

// TestWatchdogSparesFreshTask: a task created moments ago has no successful run
// yet, but it is not late either — alerting immediately on creation would make
// the watchdog cry wolf on every new task.
func TestWatchdogSparesFreshTask(t *testing.T) {
	srv, _, _, _ := watchdogServer(t)

	var alerts []watchdogAlertDTO
	decodeInto(t, authGet(t, srv.URL+"/api/watchdog"), &alerts)
	if len(alerts) != 0 {
		t.Errorf("alerts = %+v, want none for a just-created task", alerts)
	}
}

// TestWatchdogReportsNeverSucceededTask: once a task has outlived its window
// without ever succeeding, it is reported — measuring from creation is what
// catches a backup that never worked at all.
func TestWatchdogReportsNeverSucceededTask(t *testing.T) {
	srv, _, db, taskID := watchdogServer(t)
	backdateTask(t, db, taskID, 72*time.Hour) // past the derived 48h window

	var alerts []watchdogAlertDTO
	decodeInto(t, authGet(t, srv.URL+"/api/watchdog"), &alerts)

	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want the never-successful task", alerts)
	}
	if alerts[0].TaskID != taskID || alerts[0].Task != "nightly" {
		t.Errorf("alert = %+v, want the seeded task", alerts[0])
	}
	// The daily cron derives a 48h window; the UI must not have to re-derive it.
	if alerts[0].ThresholdSec != int64((48 * time.Hour).Seconds()) {
		t.Errorf("threshold_sec = %d, want 172800 (48h derived from cron)", alerts[0].ThresholdSec)
	}
	if alerts[0].LastSuccess != nil {
		t.Errorf("last_success = %v, want omitted when there was never a success", alerts[0].LastSuccess)
	}
}

// TestWatchdogClearsAfterRecentSuccess: a fresh successful run takes the task
// out of the alert list even though it was overdue a moment earlier.
func TestWatchdogClearsAfterRecentSuccess(t *testing.T) {
	srv, st, db, taskID := watchdogServer(t)
	backdateTask(t, db, taskID, 72*time.Hour)
	ctx := context.Background()

	// StartManualRun inserts directly as running — no dispatcher claim involved.
	runID, err := st.StartManualRun(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Finish(ctx, runID, core.StatusSuccess, "", ""); err != nil {
		t.Fatal(err)
	}

	var alerts []watchdogAlertDTO
	decodeInto(t, authGet(t, srv.URL+"/api/watchdog"), &alerts)
	if len(alerts) != 0 {
		t.Errorf("alerts = %+v, want none right after a successful run", alerts)
	}
}

// TestWatchdogRespectsOptOut: watchdog_sec = -1 silences a task that would
// otherwise be reported.
func TestWatchdogRespectsOptOut(t *testing.T) {
	srv, _, db, taskID := watchdogServer(t)
	backdateTask(t, db, taskID, 72*time.Hour)
	mustExec(t, db, `UPDATE task SET watchdog_sec = -1 WHERE id = ?`, taskID)

	var alerts []watchdogAlertDTO
	decodeInto(t, authGet(t, srv.URL+"/api/watchdog"), &alerts)
	if len(alerts) != 0 {
		t.Errorf("alerts = %+v, want none when the task opted out", alerts)
	}
}

// TestWatchdogWithoutWatchdogConfigured: the endpoint says so rather than 500.
func TestWatchdogWithoutWatchdogConfigured(t *testing.T) {
	srv, _, _ := seededServer(t)
	resp := authGet(t, srv.URL+"/api/watchdog")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestTaskWatchdogSecRoundTrips: the threshold survives create → read.
func TestTaskWatchdogSecRoundTrips(t *testing.T) {
	srv, _, _, _ := watchdogServer(t)

	body := `{"name":"wd","connection_id":1,"storage_id":1,"cron":"0 2 * * *","codec_chain":[],"watchdog_sec":7200}`
	resp := authPost(t, srv.URL+"/api/tasks", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	var tasks []taskDTO
	decodeInto(t, authGet(t, srv.URL+"/api/tasks"), &tasks)
	var found *taskDTO
	for i := range tasks {
		if tasks[i].Name == "wd" {
			found = &tasks[i]
		}
	}
	if found == nil {
		t.Fatal("created task not listed")
	}
	if found.WatchdogSec != 7200 {
		t.Errorf("watchdog_sec = %d, want 7200", found.WatchdogSec)
	}
}

// TestTaskRejectsBogusWatchdogSec: only 0 (auto) and -1 (off) are sentinels, so
// another negative is a typo rather than a quiet "off".
func TestTaskRejectsBogusWatchdogSec(t *testing.T) {
	srv, _, _, _ := watchdogServer(t)
	body := `{"name":"bad-wd","connection_id":1,"storage_id":1,"cron":"0 2 * * *","codec_chain":[],"watchdog_sec":-3600}`
	resp := authPost(t, srv.URL+"/api/tasks", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestPatchWithoutWatchdogSecKeepsThreshold: a PATCH that omits watchdog_sec
// must not silently reset an explicit threshold back to cron-derived auto.
func TestPatchWithoutWatchdogSecKeepsThreshold(t *testing.T) {
	srv, st, _, taskID := watchdogServer(t)
	ctx := context.Background()

	// Set an explicit six-hour threshold.
	body := `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 2 * * *","codec_chain":[],"watchdog_sec":21600}`
	resp := authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body)
	resp.Body.Close()

	// A later edit that says nothing about the watchdog leaves it alone.
	body = `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 4 * * *","codec_chain":[]}`
	resp = authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", resp.StatusCode)
	}

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Watchdog != 6*time.Hour {
		t.Errorf("watchdog = %s, want the explicit 6h preserved", task.Watchdog)
	}
	// An explicit value still overwrites.
	body = `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 4 * * *","codec_chain":[],"watchdog_sec":0}`
	resp2 := authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body)
	resp2.Body.Close()
	task, err = st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Watchdog != 0 {
		t.Errorf("watchdog = %s, want an explicit 0 to reset to auto", task.Watchdog)
	}
}

// TestWatchdogGraceAfterReEnable: a long-existing task that was only just
// switched back on is not reported — it has had no chance to run yet.
func TestWatchdogGraceAfterReEnable(t *testing.T) {
	srv, _, db, taskID := watchdogServer(t)
	backdateTask(t, db, taskID, 72*time.Hour)
	// Re-enabled a minute ago, everything else still ancient.
	mustExec(t, db, `UPDATE task SET enabled_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).Unix(), taskID)

	var alerts []watchdogAlertDTO
	decodeInto(t, authGet(t, srv.URL+"/api/watchdog"), &alerts)
	if len(alerts) != 0 {
		t.Errorf("alerts = %+v, want none right after the task was re-enabled", alerts)
	}
}

// TestEnabledAtStampedOnlyOnResume: editing an already-enabled task must not
// reset its window — otherwise every save would grant a fresh grace period and
// a permanently broken task could never trip the watchdog.
func TestEnabledAtStampedOnlyOnResume(t *testing.T) {
	srv, st, db, taskID := watchdogServer(t)
	backdateTask(t, db, taskID, 72*time.Hour)
	ctx := context.Background()

	before, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 4 * * *","codec_chain":[],"enabled":true}`
	resp := authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body)
	resp.Body.Close()

	after, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.EnabledAt.Equal(before.EnabledAt) {
		t.Errorf("enabled_at moved %s → %s on a plain edit of an enabled task", before.EnabledAt, after.EnabledAt)
	}

	// Pausing and resuming DOES restart the window.
	body = `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 4 * * *","codec_chain":[],"enabled":false}`
	authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body).Body.Close()
	body = `{"name":"nightly","connection_id":1,"storage_id":1,"cron":"0 4 * * *","codec_chain":[],"enabled":true}`
	authPatch(t, srv.URL+"/api/tasks/"+itoa(taskID), body).Body.Close()

	resumed, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.EnabledAt.After(before.EnabledAt) {
		t.Errorf("enabled_at = %s, want it restamped on resume (was %s)", resumed.EnabledAt, before.EnabledAt)
	}
}
