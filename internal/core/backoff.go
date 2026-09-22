package core

import (
	"math/rand/v2"
	"time"
)

// Backoff parameters (roadmap M2 / PLAN §Дефолты): exponential with full jitter,
// base 30s, factor 2, capped at 8m. Full jitter (sleep ∈ [0, window)) spreads
// retry storms better than equal jitter — the window doubles each attempt.
const (
	backoffBase   = 30 * time.Second
	backoffFactor = 2.0
	backoffCap    = 8 * time.Minute
)

// backoffWindow is the un-jittered upper bound for a given attempt: base·2^attempt
// clamped to cap. attempt 0 → base.
func backoffWindow(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	w := float64(backoffBase)
	capf := float64(backoffCap)
	for i := 0; i < attempt; i++ {
		w *= backoffFactor
		if w >= capf {
			return backoffCap
		}
	}
	if w > capf {
		return backoffCap
	}
	return time.Duration(w)
}

// BackoffWithRand computes a full-jitter backoff for attempt using rnd, which
// must return a value in [0,1). Exposed for deterministic tests.
func BackoffWithRand(attempt int, rnd func() float64) time.Duration {
	return time.Duration(rnd() * float64(backoffWindow(attempt)))
}

// Backoff returns the sleep before the next retry of a failed run: full-jitter
// exponential backoff. attempt is 0-based (0 = first retry).
func Backoff(attempt int) time.Duration {
	return BackoffWithRand(attempt, rand.Float64)
}
