// Package throttle provides a small, concurrency-safe time-window gate used to
// rate-limit high-frequency events (such as repetitive log lines) without
// suppressing the first occurrence.
package throttle

import (
	"sync/atomic"
	"time"
)

// Throttle permits an event at most once per interval. It is safe for
// concurrent use: under a burst, exactly one caller observes Allow()==true per
// interval. The zero value is not usable — construct with New.
type Throttle struct {
	interval int64        // minimum nanoseconds between permitted events
	last     atomic.Int64 // UnixNano of the last permitted event (0 = never)
	now      func() int64 // injectable clock for tests; nil => time.Now().UnixNano
}

// New returns a Throttle that permits one event per interval. A non-positive
// interval permits every call.
func New(interval time.Duration) *Throttle {
	return &Throttle{interval: int64(interval)}
}

// Allow reports whether an event may be emitted now. It returns true at most
// once per interval. The first call always returns true (last == 0), so the
// first occurrence is never suppressed; a long-running burst then re-emits once
// per interval. Safe for concurrent callers — the permit is claimed via a single
// compare-and-swap, so simultaneous callers yield exactly one true.
func (t *Throttle) Allow() bool {
	now := t.nowNano()
	last := t.last.Load()

	return now-last >= t.interval && t.last.CompareAndSwap(last, now)
}

func (t *Throttle) nowNano() int64 {
	if t.now != nil {
		return t.now()
	}

	return time.Now().UnixNano()
}
