package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
	"github.com/duskrun/duskrun/internal/plugin"
)

// recordNotifier captures the events it receives.
type recordNotifier struct {
	mu     sync.Mutex
	events []plugin.Event
}

func (r *recordNotifier) Name() string { return "record" }
func (r *recordNotifier) Notify(_ context.Context, ev plugin.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordNotifier) all() []plugin.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]plugin.Event(nil), r.events...)
}

// TestWorkerNotifiesOnFailure: a failing run with retries exhausted emits an
// EventFailure to the task's notifier channels.
func TestWorkerNotifiesOnFailure(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakefail", 0, 1800) // no retries
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	rec := &recordNotifier{}
	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	pool := core.NewPool(st, exec, core.PoolConfig{
		Workers: 1,
		Idle:    time.Millisecond,
		Backoff: func(int) time.Duration { return 0 },
		Notify: core.NewNotifications(nil, nil, core.NotificationsConfig{
			Resolve: func(context.Context, *core.Task, plugin.EventKind) []plugin.Notifier {
				return []plugin.Notifier{rec}
			},
		}, nil),
	}, nil)

	if did, err := pool.ProcessNext(context.Background(), "w0"); err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", did, err)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].Kind != plugin.EventFailure {
		t.Fatalf("event kind = %q, want failure", events[0].Kind)
	}
	if events[0].Task == "" || events[0].RunID == 0 {
		t.Fatalf("event missing task/run: %+v", events[0])
	}
}

// TestWorkerNotifiesOnSuccess: a successful run emits EventSuccess.
func TestWorkerNotifiesOnSuccess(t *testing.T) {
	st, path := openStore(t)
	dir := t.TempDir()
	taskID := seedFakeTaskFull(t, path, dir, "fakeexec", 0, 1800)
	if _, err := st.Enqueue(context.Background(), taskID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	rec := &recordNotifier{}
	exec := core.NewExecutor(st, nil, func() time.Time {
		return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)
	})
	pool := core.NewPool(st, exec, core.PoolConfig{
		Workers: 1,
		Backoff: func(int) time.Duration { return 0 },
		Notify: core.NewNotifications(nil, nil, core.NotificationsConfig{
			Resolve: func(context.Context, *core.Task, plugin.EventKind) []plugin.Notifier {
				return []plugin.Notifier{rec}
			},
		}, nil),
	}, nil)

	if did, err := pool.ProcessNext(context.Background(), "w0"); err != nil || !did {
		t.Fatalf("ProcessNext = (%v, %v), want (true, nil)", did, err)
	}
	events := rec.all()
	if len(events) != 1 || events[0].Kind != plugin.EventSuccess {
		t.Fatalf("events = %+v, want one EventSuccess", events)
	}
}
