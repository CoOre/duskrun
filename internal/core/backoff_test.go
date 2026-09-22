package core

import (
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	max := func() float64 { return 1.0 }
	zero := func() float64 { return 0.0 }
	half := func() float64 { return 0.5 }

	// Full-jitter upper bound equals the (doubling, capped) window.
	if got := BackoffWithRand(0, max); got != backoffBase {
		t.Fatalf("attempt 0 max = %s, want %s", got, backoffBase)
	}
	if got := BackoffWithRand(1, max); got != 2*backoffBase {
		t.Fatalf("attempt 1 max = %s, want %s", got, 2*backoffBase)
	}
	if got := BackoffWithRand(50, max); got != backoffCap {
		t.Fatalf("attempt 50 max = %s, want cap %s", got, backoffCap)
	}

	// attempt 0 stays within [0, base].
	if got := BackoffWithRand(0, zero); got != 0 {
		t.Fatalf("attempt 0 zero-jitter = %s, want 0", got)
	}
	for _, r := range []func() float64{zero, half, max} {
		if got := BackoffWithRand(0, r); got < 0 || got > backoffBase {
			t.Fatalf("attempt 0 = %s, want within [0,%s]", got, backoffBase)
		}
	}

	// Median (fixed 0.5 jitter) grows with attempt, never exceeds cap, and
	// eventually saturates at 0.5·cap.
	var prev time.Duration
	for a := 0; a < 12; a++ {
		got := BackoffWithRand(a, half)
		if got > backoffCap {
			t.Fatalf("attempt %d = %s exceeds cap %s", a, got, backoffCap)
		}
		if a > 0 && got < prev {
			t.Fatalf("attempt %d median %s < previous %s (not monotonic)", a, got, prev)
		}
		prev = got
	}
	if want := backoffCap / 2; prev != want {
		t.Fatalf("saturated median = %s, want %s", prev, want)
	}

	// The default Backoff stays within the cap for a range of attempts.
	for a := 0; a < 20; a++ {
		if got := Backoff(a); got < 0 || got > backoffCap {
			t.Fatalf("Backoff(%d) = %s, want within [0,%s]", a, got, backoffCap)
		}
	}
}
