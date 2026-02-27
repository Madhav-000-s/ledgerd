// Package webhooks delivers events to merchant-controlled endpoints.
//
// Delivery is at-least-once and unordered, and that is stated as a contract rather than
// implied. Exactly-once delivery is not achievable across a network partition; claiming
// it would push the problem onto consumers silently instead of loudly.
package webhooks

import (
	"math/rand"
	"time"
)

// Backoff computes the delay before the next delivery attempt.
type Backoff struct {
	Base time.Duration
	Cap  time.Duration
	// Rand is the source of jitter. Tests replace it; production leaves it nil and gets
	// a per-worker source.
	Rand *rand.Rand
}

// DefaultBackoff matches DESIGN.md §8.4.
var DefaultBackoff = Backoff{Base: 10 * time.Second, Cap: 6 * time.Hour}

// Interval is the deterministic upper bound for an attempt: min(cap, base * 2^attempt).
//
// attempt is zero-based, so Interval(0) is the delay bound after the first failure.
func (b Backoff) Interval(attempt int) time.Duration {
	base, capacity := b.Base, b.Cap
	if base <= 0 {
		base = DefaultBackoff.Base
	}
	if capacity <= 0 {
		capacity = DefaultBackoff.Cap
	}
	if attempt < 0 {
		attempt = 0
	}

	// Shift rather than exponentiate, and stop early: 2^attempt overflows long before
	// the schedule reaches its cap, and an overflowed interval is a negative delay.
	interval := base
	for i := 0; i < attempt; i++ {
		interval *= 2
		if interval >= capacity {
			return capacity
		}
	}
	if interval > capacity {
		return capacity
	}
	return interval
}

// Delay returns the actual wait: a uniform draw from the whole interval.
//
// Full jitter, not "exponential plus a little noise". When a merchant endpoint recovers
// from an outage, thousands of deliveries come due at once; without full jitter they
// arrive as a synchronized herd and knock the endpoint straight back over. Randomizing
// across the entire interval decorrelates them, which matters far more than the average
// delay does.
func (b Backoff) Delay(attempt int) time.Duration {
	interval := b.Interval(attempt)
	if interval <= 0 {
		return 0
	}

	if b.Rand != nil {
		return time.Duration(b.Rand.Int63n(int64(interval) + 1))
	}
	return time.Duration(rand.Int63n(int64(interval) + 1))
}

// NextAttemptAt is when a delivery becomes due again after a failed attempt.
func (b Backoff) NextAttemptAt(now time.Time, attempt int) time.Time {
	return now.Add(b.Delay(attempt))
}

// Schedule returns the interval bound for each attempt, which is what the retry-window
// figures in the documentation are computed from rather than estimated.
func (b Backoff) Schedule(maxAttempts int) []time.Duration {
	out := make([]time.Duration, 0, maxAttempts)
	for i := 0; i < maxAttempts; i++ {
		out = append(out, b.Interval(i))
	}
	return out
}

// WorstCaseWindow is the total elapsed time if every attempt waits its full interval.
func (b Backoff) WorstCaseWindow(maxAttempts int) time.Duration {
	var total time.Duration
	for _, d := range b.Schedule(maxAttempts) {
		total += d
	}
	return total
}

// ExpectedWindow is the mean elapsed time under uniform jitter, which is half the
// worst case.
//
// Both numbers are reported rather than only the flattering one: an endpoint owner
// planning an outage needs to know the tail, not the average.
func (b Backoff) ExpectedWindow(maxAttempts int) time.Duration {
	return b.WorstCaseWindow(maxAttempts) / 2
}
