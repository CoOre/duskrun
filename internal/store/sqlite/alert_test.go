package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestAlertEpisodeLifecycle: opening reports the previous level so a caller
// alerts only when things got worse, and closing says whether there was
// anything to recover from.
func TestAlertEpisodeLifecycle(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	at := time.Unix(1_800_000_000, 0)

	if prev, err := st.OpenAlert(ctx, "storage_low", "storage:1", 1, at); err != nil || prev != 0 {
		t.Fatalf("first open: prev=%d err=%v, want 0", prev, err)
	}
	if prev, err := st.OpenAlert(ctx, "storage_low", "storage:1", 2, at); err != nil || prev != 1 {
		t.Fatalf("escalate: prev=%d err=%v, want 1", prev, err)
	}
	if lvl, err := st.AlertLevel(ctx, "storage_low", "storage:1"); err != nil || lvl != 2 {
		t.Fatalf("level = %d err=%v, want 2", lvl, err)
	}
	// Same subject, other kind: an independent episode.
	if lvl, _ := st.AlertLevel(ctx, "storage_unreachable", "storage:1"); lvl != 0 {
		t.Fatalf("other kind level = %d, want 0", lvl)
	}
	if was, err := st.CloseAlert(ctx, "storage_low", "storage:1"); err != nil || !was {
		t.Fatalf("close: was=%v err=%v, want true", was, err)
	}
	if was, _ := st.CloseAlert(ctx, "storage_low", "storage:1"); was {
		t.Fatal("second close reported an open episode")
	}
}

func TestRecentArtifactSizesAndDurations(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	taskID := seedTask(t, st)
	mk := func(status string, size int64, dur int64) int64 {
		t.Helper()
		res, err := st.write.ExecContext(ctx,
			`INSERT INTO run (task_id, status, attempt, started_at, finished_at, created_at)
			 VALUES (?, ?, 1, 1000, 1000 + ?, 1000)`, taskID, status, dur)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if status == "success" {
			if _, err := st.write.ExecContext(ctx,
				`INSERT INTO artifact (run_id, storage_id, key, size, checksum, created_at) VALUES (?, 1, ?, ?, '', 1000)`,
				id, "k"+string(rune('a'+id)), size); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	mk("success", 100, 60)
	mk("failed", 0, 5)
	mk("success", 200, 120)
	last := mk("success", 300, 180)

	sizes, err := st.RecentArtifactSizes(ctx, taskID, last, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sizes, []int64{200, 100}) {
		t.Fatalf("sizes before the last run = %v, want [200 100]", sizes)
	}
	durs, err := st.RecentRunDurations(ctx, taskID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(durs, []time.Duration{3 * time.Minute, 2 * time.Minute}) {
		t.Fatalf("durations = %v", durs)
	}
}

// TestKnownDevices: the first device a user ever signs in from is not "new"
// in the alerting sense; a second one is, and only the first time.
func TestKnownDevices(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	uid := mkUser(t, st, "a@x.io", "admin")
	at := time.Unix(1_800_000_000, 0)

	isNew, hadAny, err := st.TouchKnownDevice(ctx, uid, "fp1", "Chrome · macOS", at)
	if err != nil || !isNew || hadAny {
		t.Fatalf("first device: new=%v had=%v err=%v", isNew, hadAny, err)
	}
	if isNew, _, _ := st.TouchKnownDevice(ctx, uid, "fp1", "Chrome · macOS", at); isNew {
		t.Fatal("a known device reported as new")
	}
	isNew, hadAny, _ = st.TouchKnownDevice(ctx, uid, "fp2", "Firefox · Linux", at.Add(time.Hour))
	if !isNew || !hadAny {
		t.Fatalf("second device: new=%v had=%v", isNew, hadAny)
	}

	gone, err := st.PruneKnownDevices(ctx, at.Add(time.Minute))
	if err != nil || gone != 1 {
		t.Fatalf("pruned %d err=%v, want 1", gone, err)
	}
}

// TestMigrationSubscribesExistingChannels: a channel that reported failures
// picks up the new alerts on upgrade; one that did not is left alone, and
// security is never switched on by the migration.
func TestMigrationSubscribesExistingChannels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO notifier (name, type, events, created_at) VALUES ('tg', 'telegram', '["failure","watchdog"]', 1)`,
		`INSERT INTO notifier (name, type, events, created_at) VALUES ('ok', 'webhook', '["success"]', 1)`,
		`UPDATE notifier SET events = '["failure"]' WHERE name = 'log'`,
		`DELETE FROM schema_migrations WHERE name = '0010_alerts.sql'`,
		`DROP TABLE alert_state`,
		`DROP TABLE known_device`,
	} {
		if _, err := st.write.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	events := func(name string) []string {
		var raw string
		if err := st.read.QueryRowContext(ctx, `SELECT events FROM notifier WHERE name = ?`, name).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var out []string
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := []string{"failure", "watchdog", "recovered", "size_anomaly", "slow_run", "interrupted", "storage_low", "storage_unreachable"}
	if got := events("tg"); !slices.Equal(got, want) {
		t.Errorf("tg = %v\nwant %v", got, want)
	}
	if got := events("ok"); !slices.Equal(got, []string{"success"}) {
		t.Errorf("ok = %v, want untouched", got)
	}
	if got := events("log"); !slices.Contains(got, "security") || !slices.Contains(got, "retention_done") {
		t.Errorf("log = %v, want every kind", got)
	}
}
