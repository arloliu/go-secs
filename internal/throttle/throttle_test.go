package throttle

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newWithClock builds a Throttle whose time source is fully controlled by the
// test, so window behavior is deterministic (no time.Sleep, no flakiness).
func newWithClock(interval time.Duration, clock *int64) *Throttle {
	t := New(interval)
	t.now = func() int64 { return atomic.LoadInt64(clock) }

	return t
}

func TestThrottle_FirstCallAlwaysAllowed(t *testing.T) {
	var clock int64 = 1_000_000_000 // arbitrary non-zero start
	th := newWithClock(time.Second, &clock)

	require.True(t, th.Allow(), "the first call must be permitted (first-occurrence)")
}

func TestThrottle_SuppressesWithinWindow(t *testing.T) {
	var clock int64 = 1_000_000_000
	th := newWithClock(time.Second, &clock)

	require.True(t, th.Allow(), "first call permitted")

	// Advance within the window — still suppressed.
	atomic.StoreInt64(&clock, clock+int64(500*time.Millisecond))
	require.False(t, th.Allow(), "a call within the interval must be suppressed")

	// Exactly at the boundary minus 1ns — still suppressed.
	atomic.StoreInt64(&clock, 1_000_000_000+int64(time.Second)-1)
	require.False(t, th.Allow(), "a call just before the interval elapses must be suppressed")
}

func TestThrottle_ReArmsAfterInterval(t *testing.T) {
	var clock int64 = 1_000_000_000
	th := newWithClock(time.Second, &clock)

	require.True(t, th.Allow(), "first call permitted")
	require.False(t, th.Allow(), "second immediate call suppressed")

	// Advance past the interval — permitted again (heartbeat).
	atomic.StoreInt64(&clock, clock+int64(time.Second))
	require.True(t, th.Allow(), "a call after the interval elapses must be permitted again")
	require.False(t, th.Allow(), "and immediately suppressed again")
}

func TestThrottle_NonPositiveIntervalPermitsAll(t *testing.T) {
	var clock int64 = 1_000_000_000
	th := newWithClock(0, &clock)

	require.True(t, th.Allow())
	require.True(t, th.Allow(), "a non-positive interval must permit every call")
}

func TestThrottle_ConcurrentBurstPermitsExactlyOne(t *testing.T) {
	var clock int64 = 1_000_000_000
	th := newWithClock(time.Second, &clock)

	const goroutines = 64

	var permitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start // maximize contention on the same instant
			if th.Allow() {
				permitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int64(1), permitted.Load(),
		"a concurrent burst at one instant must permit exactly one caller")
}

func TestThrottle_DefaultClockPermitsFirstCall(t *testing.T) {
	// No injected clock: exercises the production time.Now path.
	th := New(time.Hour)
	require.True(t, th.Allow(), "first call permitted with the real clock")
	require.False(t, th.Allow(), "second immediate call suppressed with the real clock")
}
