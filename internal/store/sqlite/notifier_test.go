package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrationReconcilesTaskNotifiers: before the notifier table, a task's
// notifiers named plugin TYPES and "telegram" was a valid choice. They now name
// configured channels, of which the migration seeds exactly one, so a leftover
// type name makes the task unsaveable — cron, retention, even pausing it. The
// reconcile migration drops those names; they delivered nothing anyway, since a
// telegram channel built with a nil config could never be constructed.
func TestMigrationReconcilesTaskNotifiers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reconcile.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	taskID := seedTask(t, st)

	// Stand in for a pre-0005 row and rewind the reconcile migration so the
	// reopen below applies it the way an upgrade would.
	if _, err := st.write.ExecContext(ctx,
		`UPDATE task SET notifiers = '["log","telegram","webhook"]' WHERE id = ?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE name = '0006_notifier_reconcile.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Notifiers) != 1 || task.Notifiers[0] != "log" {
		t.Fatalf("notifiers = %v, want only the configured log channel", task.Notifiers)
	}
}
