package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/CoOre/duskrun/internal/core"
)

// TestVAPIDKeysFirstWriterWins: concurrent starts must agree on one key pair —
// browsers subscribe against the public key, so two would orphan half of them.
func TestVAPIDKeysFirstWriterWins(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	if _, err := st.VAPIDKeys(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh db: err = %v, want ErrNotFound", err)
	}

	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := st.InitVAPIDKeys(ctx, core.VAPIDKeys{
				PublicKey: string(rune('a' + i)), PrivateKey: []byte{byte(i)}, KeyID: "k",
			})
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = k.PublicKey
		}()
	}
	wg.Wait()
	for _, pk := range got[1:] {
		if pk != got[0] {
			t.Fatalf("callers disagree on the key: %q", got)
		}
	}
	stored, err := st.VAPIDKeys(ctx)
	if err != nil || stored.PublicKey != got[0] || len(stored.PrivateKey) != 1 {
		t.Fatalf("stored = %+v, %v; want public key %q", stored, err, got[0])
	}
}

func mkSub(t *testing.T, st *Store, userID int64, endpoint string) int64 {
	t.Helper()
	id, err := st.UpsertPushSubscription(context.Background(), core.PushSubscription{
		UserID: userID, Endpoint: endpoint, P256dh: "p", Auth: "a", UserAgent: "ua",
	})
	if err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
	return id
}

// TestPushSubscriptionMovesToNewOwner: the same browser subscribing as another
// user must stop delivering the previous owner's alerts there, and must not
// inherit their failure record.
func TestPushSubscriptionMovesToNewOwner(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	a := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	b := mkUser(t, st, "b@corp.io", core.RoleViewer)

	id := mkSub(t, st, a, "https://push.example/1")
	if err := st.MarkPushResult(ctx, id, "boom"); err != nil {
		t.Fatal(err)
	}
	// Same user, rotated keys: same row, keys refreshed.
	if again := mkSub(t, st, a, "https://push.example/1"); again != id {
		t.Fatalf("resubscribe id = %d, want %d", again, id)
	}
	if again, err := st.UpsertPushSubscription(ctx, core.PushSubscription{
		UserID: b, Endpoint: "https://push.example/1", P256dh: "p2", Auth: "a2",
	}); err != nil || again != id {
		t.Fatalf("handover = %d, %v; want row %d", again, err, id)
	}

	if subs, _ := st.ListPushSubscriptions(ctx, a); len(subs) != 0 {
		t.Fatalf("previous owner still has %d subscriptions", len(subs))
	}
	sub, err := st.GetPushSubscription(ctx, b, id)
	if err != nil {
		t.Fatal(err)
	}
	if sub.P256dh != "p2" || sub.Auth != "a2" || sub.FailCount != 0 || sub.LastError != "" {
		t.Fatalf("handed-over row = %+v; want new keys and a clean record", sub)
	}
}

func TestPushTargetsSkipDisabledUsers(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	admin := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	viewer := mkUser(t, st, "v@corp.io", core.RoleViewer)
	mkSub(t, st, admin, "https://push.example/a")
	mkSub(t, st, viewer, "https://push.example/v")

	if err := st.UpdateUser(ctx, core.User{ID: viewer, Name: "v", Role: core.RoleViewer, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	targets, err := st.PushTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].UserID != admin || targets[0].Role != core.RoleAdmin {
		t.Fatalf("targets = %+v; want only the admin's browser, with its role", targets)
	}
	// Disabling keeps the row: enabling the account resumes delivery.
	if subs, _ := st.ListPushSubscriptions(ctx, viewer); len(subs) != 1 {
		t.Fatalf("disabled user's subscriptions = %d, want 1 kept", len(subs))
	}
}

func TestPushSubscriptionOwnership(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	a := mkUser(t, st, "a@corp.io", core.RoleAdmin)
	b := mkUser(t, st, "b@corp.io", core.RoleViewer)
	id := mkSub(t, st, a, "https://push.example/1")

	if _, err := st.GetPushSubscription(ctx, b, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get someone else's: err = %v, want ErrNotFound", err)
	}
	if err := st.DeletePushSubscription(ctx, b, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete someone else's: err = %v, want ErrNotFound", err)
	}
	if err := st.DeletePushSubscriptionByEndpoint(ctx, b, "https://push.example/1"); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.ListPushSubscriptions(ctx, a); len(subs) != 1 {
		t.Fatal("a scoped endpoint delete removed another user's subscription")
	}
	if err := st.DeletePushSubscriptionByEndpoint(ctx, 0, "https://push.example/1"); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.ListPushSubscriptions(ctx, a); len(subs) != 0 {
		t.Fatal("an unscoped endpoint delete left the subscription")
	}
}

func TestPushSubscriptionsGoWithUser(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	mkUser(t, st, "admin@corp.io", core.RoleAdmin) // the last admin cannot be deleted
	u := mkUser(t, st, "u@corp.io", core.RoleViewer)
	mkSub(t, st, u, "https://push.example/1")
	if err := st.DeleteUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if targets, _ := st.PushTargets(ctx); len(targets) != 0 {
		t.Fatalf("deleted user's subscription survived: %+v", targets)
	}
}

func TestMarkPushResult(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	u := mkUser(t, st, "u@corp.io", core.RoleAdmin)
	id := mkSub(t, st, u, "https://push.example/1")

	for range 2 {
		if err := st.MarkPushResult(ctx, id, "status 500"); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ := st.GetPushSubscription(ctx, u, id)
	if sub.FailCount != 2 || sub.LastError != "status 500" || sub.LastSuccessAt != nil {
		t.Fatalf("after failures = %+v", sub)
	}
	if err := st.MarkPushResult(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	sub, _ = st.GetPushSubscription(ctx, u, id)
	if sub.FailCount != 0 || sub.LastError != "" || sub.LastSuccessAt == nil {
		t.Fatalf("after success = %+v", sub)
	}
}
