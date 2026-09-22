package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// TestDeleteConnectionInUse: a connection referenced by a task is protected by
// the ON DELETE RESTRICT FK and must surface ErrInUse, not delete.
func TestDeleteConnectionInUse(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedTaskNamed(t, st, "nightly", true) // creates connection id 1 + a task using it

	err := st.DeleteConnection(ctx, 1)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
	// The connection is still there.
	if _, err := st.GetConnection(ctx, 1); err != nil {
		t.Fatalf("connection should survive a blocked delete: %v", err)
	}
}

// TestDeleteConnectionOK: an unreferenced connection deletes cleanly; a second
// delete of the same id reports ErrNotFound.
func TestDeleteConnectionOK(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id, err := st.CreateConnection(ctx, coreConn("spare", "postgres", "direct"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteConnection(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteConnection(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on second delete, got %v", err)
	}
}

// TestDeleteStorageInUse mirrors the connection case for storages.
func TestDeleteStorageInUse(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	seedTaskNamed(t, st, "nightly", true) // creates storage id 1 + a task using it

	if err := st.DeleteStorage(ctx, 1); !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
}

// TestDeleteTaskCascades: deleting a task removes its run history too, and an
// active (running) run blocks the delete.
func TestDeleteTaskCascades(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := seedTaskNamed(t, st, "nightly", true)

	// A finished run in history — cascade should remove it.
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO run (task_id, status, attempt, created_at) VALUES (?, 'success', 1, unixepoch())`, id,
	); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTask(ctx, id); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if _, err := st.GetTask(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task should be gone, got %v", err)
	}
	var runs int
	if err := st.read.QueryRowContext(ctx, `SELECT count(*) FROM run WHERE task_id = ?`, id).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("runs should cascade-delete, got %d", runs)
	}
}

func TestDeleteTaskBlockedByActiveRun(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	id := seedTaskNamed(t, st, "nightly", true)
	if _, err := st.Enqueue(ctx, id); err != nil { // queued == active
		t.Fatal(err)
	}
	if err := st.DeleteTask(ctx, id); !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse while a run is active, got %v", err)
	}
}

// TestSecretRefsAndDelete: a secret referenced by a connection (via secret_ref)
// or a storage config (via access_key_ref) is reported by SecretRefs; once the
// referrers drop the ref, the secret deletes.
func TestSecretRefsAndDelete(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	secID, err := st.PutSecret(ctx, core.Secret{Name: "db/pg", Type: "db-password", Ciphertext: []byte("x"), KeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	// Connection references it by "secret://db/pg".
	if _, err := st.CreateConnection(ctx, core.Connection{
		Name: "pg", Engine: "postgres", ConnectorType: "direct", SecretRef: "secret://db/pg",
	}); err != nil {
		t.Fatal(err)
	}
	// Storage references it inside its config as access_key_ref (bare name).
	if _, err := st.CreateStorage(ctx, core.Storage{
		Name: "s3", Type: "s3", Config: json.RawMessage(`{"access_key_ref":"db/pg"}`),
	}); err != nil {
		t.Fatal(err)
	}

	refs, err := st.SecretRefs(ctx, "db/pg")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("want 2 referrers, got %d: %v", len(refs), refs)
	}

	// Look up by id round-trips the name.
	sec, err := st.GetSecretByID(ctx, secID)
	if err != nil || sec.Name != "db/pg" {
		t.Fatalf("GetSecretByID: %v / %+v", err, sec)
	}

	// With no referrers the same helper returns empty and the delete succeeds.
	empty, err := st.SecretRefs(ctx, "nobody-uses-me")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("want no referrers, got %v", empty)
	}
	if err := st.DeleteSecret(ctx, secID); err != nil {
		t.Fatalf("delete secret: %v", err)
	}
	if err := st.DeleteSecret(ctx, secID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on second delete, got %v", err)
	}
}

// TestSecretRefsCoversNotifierChannels: a channel config is the third place a
// secret is referenced from. Missing it means the Secrets page happily deletes
// the bot token, and the alerting dies where nobody is watching — in the path
// that only runs when a backup has already failed.
func TestSecretRefsCoversNotifierChannels(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	if _, err := st.PutSecret(ctx, core.Secret{
		Name: "tg/bot", Type: "telegram-token", Ciphertext: []byte("x"), KeyID: "k1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNotifier(ctx, core.NotifierChannel{
		Name: "ops-telegram", Type: "telegram",
		Config: json.RawMessage(`{"chat_id":"-100","token_ref":"secret://tg/bot"}`),
		Events: []string{"failure"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	refs, err := st.SecretRefs(ctx, "tg/bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || !strings.Contains(refs[0], "ops-telegram") {
		t.Fatalf("refs = %v, want the channel holding the token named", refs)
	}
}

// TestPruneNotifications: the delivery log is append-only otherwise, so the
// retention sweep is the only thing standing between a five-minute task and an
// unbounded database file.
func TestPruneNotifications(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()

	base := time.Unix(1_780_000_000, 0)
	for i := 0; i < 10; i++ {
		if _, err := st.InsertNotification(ctx, core.Notification{
			Kind: "success", Task: "t", Channel: "log",
			Status: core.NotificationSent, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	gone, err := st.PruneNotifications(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if gone != 7 {
		t.Errorf("deleted = %d, want 7", gone)
	}

	kept, err := st.ListNotifications(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %d rows, want 3", len(kept))
	}
	// Newest survive: an operator looks at the log to explain what just happened.
	if !kept[0].CreatedAt.Equal(base.Add(9 * time.Minute)) {
		t.Errorf("newest kept = %s, want the last insert", kept[0].CreatedAt)
	}
}
