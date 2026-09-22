package core_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/duskrun/duskrun/internal/core"
)

// TestHealthzHandler: the liveness handler returns 200 "ok".
func TestHealthzHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	core.HealthzHandler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}
}

// TestEngineStartStop: the engine starts, then returns cleanly within 2s of ctx
// cancellation.
func TestEngineStartStop(t *testing.T) {
	st, _ := openStore(t)
	disp := core.NewDispatcher(st, nil, nil)
	exec := core.NewExecutor(st, nil, time.Now)
	pool := core.NewPool(st, exec, core.PoolConfig{
		Workers: 2,
		Idle:    time.Millisecond,
		Backoff: func(int) time.Duration { return 0 },
	}, nil)
	eng := core.NewEngine(disp, pool, core.EngineConfig{
		ListenAddr:   "127.0.0.1:0", // random free port
		TickInterval: 20 * time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx) }()

	time.Sleep(150 * time.Millisecond) // let it come up and tick a few times
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Engine.Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Engine.Run did not return within 2s of cancel")
	}
}
