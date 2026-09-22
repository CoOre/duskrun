package core

import (
	"testing"
	"time"
)

// clock returns a func() time.Time advancing by step on each call, starting at base.
func clock(base time.Time, step time.Duration) func() time.Time {
	cur := base
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// TestHubSubscribeReceivesSnapshotAndLive checks a subscriber gets the pre-existing
// tail as a snapshot and every subsequent event live, in order.
func TestHubSubscribeReceivesSnapshotAndLive(t *testing.T) {
	h := NewHub(clock(time.Unix(0, 0), time.Second))
	h.Phase(7, PhaseResolve)
	h.Log(7, PhaseResolve, "started")

	snap, ch, cancel, ok := h.Subscribe(7)
	if !ok {
		t.Fatal("Subscribe ok = false, want true for a live run")
	}
	defer cancel()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Kind != EventPhase || snap[1].Kind != EventLog {
		t.Fatalf("snapshot kinds = %q,%q", snap[0].Kind, snap[1].Kind)
	}

	h.Bytes(7, 1024)
	ev := <-ch
	if ev.Kind != EventBytes || ev.Bytes != 1024 {
		t.Fatalf("live event = %+v, want bytes=1024", ev)
	}
	if ev.Seq != 3 {
		t.Fatalf("Seq = %d, want 3 (monotonic)", ev.Seq)
	}
}

// TestHubBytesThroughput verifies throughput is derived from the byte/time delta
// between consecutive Bytes calls (0 on the first, computed after).
func TestHubBytesThroughput(t *testing.T) {
	// step 1s per clock read; each Bytes reads the clock once.
	h := NewHub(clock(time.Unix(0, 0), time.Second))
	h.Bytes(3, 1000) // t=0, no prior → bps 0
	snap, ch, cancel, _ := h.Subscribe(3)
	_ = snap
	defer cancel()
	h.Bytes(3, 3000) // t=1s, delta 2000 bytes over 1s → 2000 bps
	ev := <-ch
	if ev.ThroughputBps != 2000 {
		t.Fatalf("throughput = %v, want 2000", ev.ThroughputBps)
	}
}

// TestHubCloseEndsSubscribers checks Close drains the terminal status then closes
// the channel, and that the run is forgotten (a later Subscribe returns ok=false).
func TestHubCloseEndsSubscribers(t *testing.T) {
	h := NewHub(clock(time.Unix(0, 0), time.Second))
	h.Phase(9, PhaseStream)
	_, ch, cancel, ok := h.Subscribe(9)
	if !ok {
		t.Fatal("Subscribe ok = false")
	}
	defer cancel()

	h.Status(9, StatusSuccess)
	h.Close(9)

	got := drain(t, ch)
	if len(got) == 0 || got[len(got)-1].Kind != EventStatus {
		t.Fatalf("expected a trailing status event, got %+v", got)
	}
	if got[len(got)-1].Status != StatusSuccess {
		t.Fatalf("terminal status = %q, want success", got[len(got)-1].Status)
	}
	if _, _, _, ok := h.Subscribe(9); ok {
		t.Fatal("Subscribe after Close ok = true, want false (run forgotten)")
	}
}

// TestHubNilSafe ensures a nil *Hub is a no-op Publisher and yields no live sub.
func TestHubNilSafe(t *testing.T) {
	var h *Hub
	h.Phase(1, PhaseResolve)
	h.Log(1, PhaseResolve, "x")
	h.Bytes(1, 1)
	h.Status(1, StatusFailed)
	h.Close(1)
	if _, _, _, ok := h.Subscribe(1); ok {
		t.Fatal("nil Hub Subscribe ok = true, want false")
	}
}

// drain reads a channel to close (with a timeout guard) and returns the events.
func drain(t *testing.T, ch <-chan ProgressEvent) []ProgressEvent {
	t.Helper()
	var out []ProgressEvent
	timeout := time.After(time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("timed out waiting for channel close")
			return out
		}
	}
}
