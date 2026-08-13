package hsms

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// eventLog is a mutex-guarded ordered record of what the notifier delivered.
// Subscriptions and legacy handlers both append here, tagged,
// so a single log proves both the per-surface content and the relative ordering of the two surfaces.
type eventLog struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	tag   string
	prev  ConnState
	next  ConnState
	cause TransitionCause
}

func (l *eventLog) add(tag string, prev, next ConnState, cause TransitionCause) {
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{tag: tag, prev: prev, next: next, cause: cause})
	l.mu.Unlock()
}

// snapshot returns a copy of the log so assertions never read it while the notifier writes.
func (l *eventLog) snapshot() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)

	return out
}

// filter returns the entries carrying tag, in order.
func (l *eventLog) filter(tag string) []logEntry {
	var out []logEntry
	for _, e := range l.snapshot() {
		if e.tag == tag {
			out = append(out, e)
		}
	}

	return out
}

// requireCount waits (bounded poll — never a Sleep) until the log holds at least n entries with tag.
func (l *eventLog) requireCount(t *testing.T, tag string, n int) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		return len(l.filter(tag)) >= n
	}, 3*time.Second, time.Millisecond, "timeout waiting for %d %q events, have %d", n, tag, len(l.filter(tag)))
}

// openSelected walks c from NotConnected to Selected ONE transition at a time, waiting for each to
// be observed on l before driving the next.
// The shared connect+select mock script races the TCP-up event against the select commit,
// so the supervisor legitimately collapses both into a single NotConnected -> Selected report when the commit wins;
// stepping removes that race so a test can assert on an exact event sequence.
// l must already hold a subscription registered through subscribeLog.
func openSelected(t *testing.T, c *connection, l *eventLog) {
	t.Helper()

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	l.requireCount(t, "sub", 1) // NotConnected -> NotSelected

	require.True(t, c.CommitSelected(), "the select commit must be this call's CAS")
	l.requireCount(t, "sub", 2) // NotSelected -> Selected
	requireSelected(t, c)
}

// subscribeLog registers a recording subscription on c, tagged "sub", and cancels it at test end.
// It returns the cancel so a test that exercises cancellation can call it early;
// t.Cleanup makes the later call a no-op, which is exactly the idempotence the API promises.
func subscribeLog(t *testing.T, c *connection, l *eventLog) func() {
	t.Helper()

	cancel := c.SubscribeLifecycle(func(ev LifecycleEvent) {
		l.add("sub", ev.Previous, ev.Current, ev.Cause)
	})
	t.Cleanup(cancel)

	return cancel
}

func TestTransitionCause_String(t *testing.T) {
	cases := []struct {
		cause TransitionCause
		want  string
	}{
		{CauseUnknown, "Unknown"},
		{CauseLocalOpen, "LocalOpen"},
		{CauseLocalClose, "LocalClose"},
		{CauseSelectAccepted, "SelectAccepted"},
		{CauseSelectRejected, "SelectRejected"},
		{CausePeerSeparate, "PeerSeparate"},
		{CausePeerDeselect, "PeerDeselect"},
		{CauseT6Timeout, "T6Timeout"},
		{CauseT7Timeout, "T7Timeout"},
		{CauseLinktestFail, "LinktestFail"},
		{CauseIOError, "IOError"},
		{TransitionCause(200), "TransitionCause(200)"},
	}

	for _, tc := range cases {
		require.Equal(t, tc.want, tc.cause.String())
	}
}

// A subscription and a legacy AddConnStateChangeHandler must see the SAME transitions in the SAME
// order across one Open/Close cycle — the new surface must not change what the old one observes —
// and the subscription must additionally carry the cause of each transition.
func TestSubscribeLifecycle_MatchesLegacyHandlerAndCarriesCauses(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	c.AddConnStateChangeHandler(func(prev, next ConnState) {
		log.add("handler", prev, next, CauseUnknown)
	})
	subscribeLog(t, c, log)

	openSelected(t, c, log)
	require.NoError(t, c.Close())

	log.requireCount(t, "sub", 3)
	log.requireCount(t, "handler", 3)

	subs := log.filter("sub")
	handlers := log.filter("handler")

	want := []logEntry{
		{prev: NotConnectedState, next: NotSelectedState, cause: CauseLocalOpen},
		{prev: NotSelectedState, next: SelectedState, cause: CauseSelectAccepted},
		{prev: SelectedState, next: NotConnectedState, cause: CauseLocalClose},
	}

	require.Len(t, subs, 3)
	require.Len(t, handlers, 3)

	for i, w := range want {
		require.Equal(t, w.prev, subs[i].prev, "subscription event %d previous", i)
		require.Equal(t, w.next, subs[i].next, "subscription event %d current", i)
		require.Equal(t, w.cause, subs[i].cause, "subscription event %d cause", i)

		// The legacy surface sees the same transition, in the same position of its own stream.
		require.Equal(t, w.prev, handlers[i].prev, "legacy handler event %d previous", i)
		require.Equal(t, w.next, handlers[i].next, "legacy handler event %d current", i)
	}
}

// cancel stops delivery, and calling it again is a no-op rather than a panic or a double removal.
// The cancel happens while the connection is QUIESCENT, after the Selected event has already been observed,
// so "no further events" is a deterministic claim rather than a race against an in-flight dispatch.
func TestSubscribeLifecycle_CancelIsEffectiveAndIdempotent(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	cancel := subscribeLog(t, c, log)

	// A second, never-cancelled subscription is the positive control:
	// it proves the transitions after the cancel really happened, so "zero further events" is not a vacuous assertion.
	control := &eventLog{}
	subscribeLog(t, c, control)

	openSelected(t, c, log)
	// Quiescent: both open transitions are delivered and no further one is in flight.

	cancel()
	cancel() // idempotent: a second cancel must not remove anything else or panic

	before := len(log.filter("sub"))
	require.NoError(t, c.Close())

	control.requireCount(t, "sub", 3) // the terminal transition really was delivered
	require.Len(t, log.filter("sub"), before, "a cancelled subscription must receive nothing further")
}

// A callback that cancels its own subscription from inside the notifier must not deadlock:
// cancellation is a lock-free CAS, never a lock the notifier already holds.
func TestSubscribeLifecycle_CancelFromInsideCallbackDoesNotDeadlock(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}

	var (
		mu     sync.Mutex
		cancel func()
	)

	mu.Lock()
	cancel = c.SubscribeLifecycle(func(ev LifecycleEvent) {
		log.add("sub", ev.Previous, ev.Current, ev.Cause)

		mu.Lock()
		fn := cancel
		mu.Unlock()

		fn() // cancel from inside the delivery this subscription is being run by
	})
	mu.Unlock()

	// The positive control keeps observing,
	// so a wedged notifier shows up as a missing event rather than as a passing test that simply stopped receiving.
	control := &eventLog{}
	subscribeLog(t, c, control)

	openSelected(t, c, control)
	require.NoError(t, c.Close())

	control.requireCount(t, "sub", 3) // the notifier kept delivering after the self-cancel
	require.Len(t, log.filter("sub"), 1, "self-cancelling subscription receives exactly the event it cancelled in")
}

// A subscription outlives an Open/Close cycle and keeps observing the next one, exactly like a
// legacy handler — the supervisor is recreated per Open, the subscription is not.
func TestSubscribeLifecycle_PersistsAcrossReopen(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	subscribeLog(t, c, log)

	for range 2 {
		openSelected(t, c, log)
		require.NoError(t, c.Close())
	}

	log.requireCount(t, "sub", 6)

	got := log.filter("sub")
	require.Len(t, got, 6, "two full cycles deliver three transitions each")

	for cycle := range 2 {
		base := cycle * 3
		require.Equal(t, CauseLocalOpen, got[base].cause)
		require.Equal(t, CauseSelectAccepted, got[base+1].cause)
		require.Equal(t, CauseLocalClose, got[base+2].cause)
	}
}

// Subscribing and cancelling concurrently with live transitions must be race-free — this test earns its keep under -race —
// and must never lose the subscriptions that were not cancelled.
func TestSubscribeLifecycle_ConcurrentSubscribeCancelIsRaceFree(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	survivor := &eventLog{}
	subscribeLog(t, c, survivor)

	var churn sync.WaitGroup

	stop := make(chan struct{})

	for range 8 {
		churn.Add(1)

		go func() {
			defer churn.Done()

			for {
				select {
				case <-stop:
					return
				default:
				}

				cancel := c.SubscribeLifecycle(func(_ LifecycleEvent) {})
				cancel()
				cancel()
			}
		}()
	}

	openSelected(t, c, survivor)
	require.NoError(t, c.Close())

	close(stop)
	churn.Wait()

	survivor.requireCount(t, "sub", 3)
	require.Equal(t, 1, len(*c.lifecycleSubs.Load()), "only the never-cancelled subscription remains")
}

// A panicking subscription is isolated the same way a panicking StateChangeHandler is: the rest of
// the fan-out still runs and the notifier survives.
func TestSubscribeLifecycle_PanicIsIsolated(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	t.Cleanup(c.SubscribeLifecycle(func(_ LifecycleEvent) {
		panic("subscriber blew up")
	}))

	log := &eventLog{}
	subscribeLog(t, c, log)

	openSelected(t, c, log)
	require.NoError(t, c.Close())

	log.requireCount(t, "sub", 3)
}

// A nil callback registers nothing and still returns a usable cancel.
func TestSubscribeLifecycle_NilCallbackIsNoOp(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	cancel := c.SubscribeLifecycle(nil)
	require.NotNil(t, cancel)
	require.Nil(t, c.lifecycleSubs.Load(), "a nil callback must not allocate a subscription slice")

	cancel()
	cancel()
}

// The T7 dwell expiry names its own cause.
// It is driven through the runtime seam rather than a transport script, because T7 belongs to the transport's timer, not to the FSM.
func TestSubscribeLifecycle_T7ExpiryCause(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	subscribeLog(t, c, log)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	log.requireCount(t, "sub", 1) // NotConnected -> NotSelected

	c.T7Expired()
	log.requireCount(t, "sub", 2)

	got := log.filter("sub")
	require.Equal(t, NotSelectedState, got[1].prev)
	require.Equal(t, NotConnectedState, got[1].next)
	require.Equal(t, CauseT7Timeout, got[1].cause)
}

// The peer-Deselect path (Selected -> NotSelected with the link still up) names its own cause.
func TestSubscribeLifecycle_PeerDeselectCause(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	subscribeLog(t, c, log)

	openSelected(t, c, log)

	c.SelectLost() // what the Deselect.req responder drives
	log.requireCount(t, "sub", 3)

	got := log.filter("sub")
	require.Equal(t, SelectedState, got[2].prev)
	require.Equal(t, NotSelectedState, got[2].next)
	require.Equal(t, CausePeerDeselect, got[2].cause)
}

// A transport that reports a drop WITHOUT naming a cause reports CauseUnknown:
// the engine never guesses CauseIOError on its behalf.
func TestSubscribeLifecycle_UnnamedTCPDownIsCauseUnknown(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransportNeverSelects())

	log := &eventLog{}
	subscribeLog(t, c, log)

	openSelected(t, c, log)

	c.TCPDown(ErrConnClosed) // the plain TransportRuntime entry point, no TransitionCause supplied
	log.requireCount(t, "sub", 3)

	got := log.filter("sub")
	require.Equal(t, NotConnectedState, got[2].next)
	require.Equal(t, CauseUnknown, got[2].cause)
}
