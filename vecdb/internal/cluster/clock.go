package cluster

import (
	"sync"
	"time"
)

// Clock is the time source. It is an interface for one reason: a failure
// detector is a machine for turning elapsed time into decisions, and a test
// that has to actually sleep to exercise it is slow, flaky, and cannot
// reproduce a specific interleaving.
//
// Everything here uses it for DURATIONS, never for ordering. Ordering comes
// from sequence numbers and epochs. A clock that jumps must be able to cause a
// wrong failure decision, never a wrong write order.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock.
//
// Lease arithmetic should really use a monotonic clock; Go's time.Time carries
// a monotonic reading and time.Sub uses it when both operands have one, so
// durations computed from this are drift-safe even if NTP steps the wall clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a clock the test drives by hand. Advancing it is what makes
// "five seconds passed" a single function call rather than a sleep.
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewManualClock starts a manual clock at an arbitrary fixed instant.
func NewManualClock() *ManualClock {
	return &ManualClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// Now returns the current simulated time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
