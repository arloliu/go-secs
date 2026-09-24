package hsms

// Regression coverage for the bounded join of the state-change notifier.
//
// The notifier goroutine runs user code (StateChangeHandler and SubscribeLifecycle callbacks),
// and Close used to join it with no bound.
// A callback that called Close therefore waited for itself forever, holding lifeMu,
// and a callback blocked for any other reason made a concurrent Close wait forever too.
//
// Blocking callbacks claim their one blocking call with an atomic.Bool CAS, never a shared sync.Once:
// the next cycle's notifier calls the same callback, and a Once would park it inside Do behind the blocked call.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// notifierJoinTimeout is the close timeout these tests configure:
// short, so a bounded join expires quickly, and far below closeReturnBound.
const notifierJoinTimeout = 200 * time.Millisecond

// closeReturnBound is how long a test waits for a lifecycle call that must return.
// It is generous against notifierJoinTimeout, so only an unbounded join misses it.
const closeReturnBound = 5 * time.Second

// newShortCloseConn builds a connection on script with the close timeout shortened to notifierJoinTimeout.
// Its cleanup Close is bounded, so a regression fails the test instead of hanging the binary in cleanup.
func newShortCloseConn(t *testing.T, script mockScript) *connection {
	t.Helper()

	cfg := DefaultConnectionConfig()
	require.NoError(t, cfg.apply(WithCloseTimeout(notifierJoinTimeout)))

	conn, err := NewConnection(cfg, &mockTransport{startFn: script})
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			_ = c.Close()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(closeReturnBound):
			t.Error("cleanup Close did not return — the notifier join must be bounded")
		}
	})

	return c
}

// callWithin runs fn on its own goroutine and returns its error,
// failing the test if fn does not return within closeReturnBound.
func callWithin(t *testing.T, what string, fn func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- fn() }()

	return recvWithin(t, done, what)
}

// recvWithin receives one error from ch, failing the test if none arrives within closeReturnBound.
func recvWithin(t *testing.T, ch <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-ch:
		return err
	case <-time.After(closeReturnBound):
		t.Fatalf("%s did not return — the notifier join must be bounded by the close timeout", what)

		return nil
	}
}

// awaitSignal waits for ch to close, failing the test after closeReturnBound.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(closeReturnBound):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// blocker is a callback gate: the first call that claims it signals entered and parks until released.
type blocker struct {
	claimed  atomic.Bool
	entered  chan struct{}
	release  chan struct{}
	released atomic.Bool
}

func newBlocker(t *testing.T) *blocker {
	t.Helper()

	b := &blocker{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(b.unblock)

	return b
}

// block parks the caller if it is the first to claim b; every later call returns immediately.
func (b *blocker) block() {
	if !b.claimed.CompareAndSwap(false, true) {
		return
	}

	close(b.entered)
	<-b.release
}

// unblock releases the parked call; safe to call more than once.
func (b *blocker) unblock() {
	if b.released.CompareAndSwap(false, true) {
		close(b.release)
	}
}

// hasEdge reports whether log holds a tag entry for the prev -> next transition.
func hasEdge(log *eventLog, tag string, prev, next ConnState) bool {
	for _, e := range log.filter(tag) {
		if e.prev == prev && e.next == next {
			return true
		}
	}

	return false
}

// notifierFinished reports, without blocking, whether s's notifier goroutine has returned.
func notifierFinished(s *supervisor) bool {
	select {
	case <-s.notifierDone:
		return true
	default:
		return false
	}
}

func TestClose_FromStateChangeHandlerReturns(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	done := make(chan error, 1)
	var claimed atomic.Bool
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next == SelectedState && claimed.CompareAndSwap(false, true) {
			done <- c.Close()
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	err := recvWithin(t, done, "Close called from a StateChangeHandler")
	require.ErrorIs(t, err, ErrCloseTimeout, "a Close that cannot join its own notifier must report the close timeout")
	require.Equal(t, NotConnectedState, c.State())
}

// Closing from NotSelected skips the farewell, so the epoch join returns at once
// and the notifier join spends its budget on the timed wait rather than the expired-deadline check.
func TestClose_FromLifecycleSubscriberReturns(t *testing.T) {
	c := newShortCloseConn(t, withMockTransportNeverSelects())

	done := make(chan error, 1)
	var claimed atomic.Bool
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == NotSelectedState && claimed.CompareAndSwap(false, true) {
			done <- c.Close()
		}
	})
	t.Cleanup(cancel)

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	err := recvWithin(t, done, "Close called from a lifecycle subscriber")
	require.ErrorIs(t, err, ErrCloseTimeout)
}

// A callback that closes and then reopens must get a working new cycle
// while it is still running on the old cycle's notifier.
func TestClose_ThenOpenFromSameHandler(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	type outcome struct {
		closeErr, openErr error
		sawNewCycle       bool
	}
	result := make(chan outcome, 1)
	newSelected := make(chan struct{})

	var claimed, secondSelected atomic.Bool
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next != SelectedState {
			return
		}

		if claimed.CompareAndSwap(false, true) {
			var o outcome
			o.closeErr = c.Close()
			o.openErr = c.Open(t.Context(), OpenBackground)

			// Still on the old cycle's notifier: the new cycle must progress without it.
			select {
			case <-newSelected:
				o.sawNewCycle = true
			case <-time.After(closeReturnBound):
			}
			result <- o

			return
		}

		if secondSelected.CompareAndSwap(false, true) {
			close(newSelected)
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))

	select {
	case o := <-result:
		require.ErrorIs(t, o.closeErr, ErrCloseTimeout)
		require.NoError(t, o.openErr, "Open from the callback must reopen after its Close returned")
		require.True(t, o.sawNewCycle, "the new cycle must reach Selected and deliver it while the old callback still runs")
	case <-time.After(3 * closeReturnBound):
		t.Fatal("the close-then-open callback never finished")
	}
}

// A handler blocked for its own reasons must not make a Close on ANOTHER goroutine wait past the close timeout.
// Once released, the abandoned notifier still delivers the queued close transition,
// and a second Close keeps reporting the first Close's result.
func TestClose_BlockedHandlerBoundsConcurrentClose(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	b := newBlocker(t)
	log := &eventLog{}
	c.AddConnStateChangeHandler(func(prev, next ConnState) {
		log.add("handler", prev, next, CauseUnknown)
		if next == SelectedState {
			b.block()
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	awaitSignal(t, b.entered, "the handler to block")
	s := c.sup.Load()

	require.ErrorIs(t, callWithin(t, "Close with a blocked StateChangeHandler", c.Close), ErrCloseTimeout)
	require.ErrorIs(t, callWithin(t, "re-Close with the notifier still blocked", c.Close), ErrCloseTimeout,
		"a re-Close before the notifier exits must return the first result")

	b.unblock()
	require.Eventually(t, func() bool { return hasEdge(log, "handler", SelectedState, NotConnectedState) },
		closeReturnBound, time.Millisecond, "the abandoned notifier must still deliver the queued close transition")
	require.Eventually(t, func() bool { return notifierFinished(s) },
		closeReturnBound, time.Millisecond, "the abandoned notifier must exit once its callback returns")

	require.ErrorIs(t, callWithin(t, "re-Close after the notifier exited", c.Close), ErrCloseTimeout,
		"a re-Close after the notifier exits must still return the first result")
}

func TestClose_ConcurrentClosersShareTimeout(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	b := newBlocker(t)
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next == SelectedState {
			b.block()
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	awaitSignal(t, b.entered, "the handler to block")

	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- c.Close() }()
	go func() { second <- c.Close() }()

	require.ErrorIs(t, recvWithin(t, first, "first concurrent Close"), ErrCloseTimeout)
	require.ErrorIs(t, recvWithin(t, second, "second concurrent Close"), ErrCloseTimeout)
}

// Re-Open while the previous cycle's notifier is still stuck in a callback.
// The new cycle must run and close on its own:
// a join shared across cycles would make the new cycle's Close wait on the old straggler.
func TestOpen_AfterAbandonedNotifierIsIsolated(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	b := newBlocker(t)
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next == SelectedState {
			b.block()
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	awaitSignal(t, b.entered, "the handler to block")
	old := c.sup.Load()

	require.ErrorIs(t, callWithin(t, "Close with a blocked StateChangeHandler", c.Close), ErrCloseTimeout)

	log := &eventLog{}
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		log.add("sub", ev.Previous, ev.Current, ev.Cause)
	})
	t.Cleanup(cancel)

	require.NoError(t, callWithin(t, "re-Open with the old notifier still blocked", func() error {
		return c.Open(t.Context(), OpenBackground)
	}), "re-Open must succeed while the old notifier is still stuck")
	requireSelected(t, c)
	fresh := c.sup.Load()
	require.NotSame(t, old, fresh, "re-Open must build a fresh supervisor")

	// The old notifier is parked before it reaches any subscription, so every sub entry comes from the new cycle.
	require.Eventually(t, func() bool {
		for _, e := range log.filter("sub") {
			if e.next == SelectedState {
				return true
			}
		}

		return false
	}, closeReturnBound, time.Millisecond, "the new cycle's notifier must deliver its own transitions")

	require.NoError(t, callWithin(t, "Close of the re-opened cycle", c.Close),
		"the new cycle's Close must join only its own notifier, not the old straggler")
	require.True(t, notifierFinished(fresh), "a clean Close must have joined the new cycle's notifier")
	require.False(t, notifierFinished(old), "the old notifier is still parked in its callback")
}

func TestClose_CleanCloseJoinsNotifier(t *testing.T) {
	c := newShortCloseConn(t, withMockTransport())

	log := &eventLog{}
	c.AddConnStateChangeHandler(func(prev, next ConnState) {
		log.add("handler", prev, next, CauseUnknown)
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	// Wait for the Selected DELIVERY, not just the state:
	// the state commits before its event is queued, and a Close that overtook the event would report a different final edge.
	require.Eventually(t, func() bool {
		for _, e := range log.filter("handler") {
			if e.next == SelectedState {
				return true
			}
		}

		return false
	}, closeReturnBound, time.Millisecond, "the Selected transition must be delivered before Close")
	s := c.sup.Load()

	require.NoError(t, callWithin(t, "clean Close", c.Close))
	require.True(t, notifierFinished(s), "a clean Close must return only after the notifier has exited")
	require.True(t, hasEdge(log, "handler", SelectedState, NotConnectedState),
		"the close transition must already be delivered when a clean Close returns")
}

// Open's rollback joins the supervisor too, so a callback blocked on the rollback's own transitions must not hang Open,
// and a later Close must report what the rollback's join found.
func TestOpen_RollbackBoundedByBlockedHandler(t *testing.T) {
	startErr := errors.New("start failed after TCP up")
	c := newShortCloseConn(t, withMockTransportTCPUpThenStartError(startErr))

	b := newBlocker(t)
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next == NotSelectedState {
			b.block()
		}
	})

	err := callWithin(t, "Open rollback with a blocked StateChangeHandler", func() error {
		return c.Open(t.Context(), OpenBackground)
	})
	require.ErrorIs(t, err, startErr, "the rollback still reports the start failure, not the join timeout")
	awaitSignal(t, b.entered, "the handler to block on the rollback's transition")

	require.ErrorIs(t, callWithin(t, "Close after the rollback", c.Close), ErrCloseTimeout,
		"Close after the rollback must return the rollback's join result")
}

// A failed Open abandons its notifier while still returning the Start error,
// so a caller can retry Open without ever seeing ErrCloseTimeout.
// The retry's cycle must run and close on its own while the old callback is still parked.
func TestOpen_RetryAfterRollbackAbandonsNotifier(t *testing.T) {
	startErr := errors.New("start failed after TCP up")
	var starts atomic.Int64
	succeed := withMockTransport()
	c := newShortCloseConn(t, func(ctx context.Context, rt TransportRuntime) error {
		if starts.Add(1) == 1 {
			return withMockTransportTCPUpThenStartError(startErr)(ctx, rt)
		}

		return succeed(ctx, rt)
	})

	b := newBlocker(t)
	c.AddConnStateChangeHandler(func(_, next ConnState) {
		if next == NotSelectedState {
			b.block()
		}
	})

	require.ErrorIs(t, callWithin(t, "failing Open", func() error {
		return c.Open(t.Context(), OpenBackground)
	}), startErr)
	awaitSignal(t, b.entered, "the handler to block on the rollback's transition")
	old := c.sup.Load()

	log := &eventLog{}
	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		log.add("sub", ev.Previous, ev.Current, ev.Cause)
	})
	t.Cleanup(cancel)

	require.NoError(t, callWithin(t, "retry Open with the rollback's notifier still blocked", func() error {
		return c.Open(t.Context(), OpenBackground)
	}))
	requireSelected(t, c)
	fresh := c.sup.Load()
	require.NotSame(t, old, fresh)

	require.Eventually(t, func() bool {
		for _, e := range log.filter("sub") {
			if e.next == SelectedState {
				return true
			}
		}

		return false
	}, closeReturnBound, time.Millisecond, "the retry's notifier must deliver its own transitions")

	require.NoError(t, callWithin(t, "Close of the retried cycle", c.Close))
	require.True(t, notifierFinished(fresh), "a clean Close must have joined the retry's notifier")
	require.False(t, notifierFinished(old), "the rollback's notifier is still parked in its callback")
}
