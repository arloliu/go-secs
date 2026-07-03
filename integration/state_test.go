package integration

import (
	"sync"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// stateEdge is one observed connection-state transition, the (prev, next) pair a
// StateChangeHandler is handed.
type stateEdge struct {
	prev hsms.ConnState
	next hsms.ConnState
}

// stateRecorder captures the ordered sequence of connection-state transitions a connection
// emits. It registers as a StateChangeHandler (returned by newStateRecorder) and is safe to read
// from the test goroutine while the connection's notifier goroutine records concurrently.
//
// It is deliberately general: the lifecycle-parity scenarios use it to await a fresh re-Select
// after an involuntary drop and to assert a transport's full open/close sequence, and the
// connection-state matrix reuses it unchanged.
type stateRecorder struct {
	mu    sync.Mutex
	cond  *sync.Cond
	edges []stateEdge
}

// newStateRecorder returns a recorder and the StateChangeHandler that feeds it. Register the
// handler with conn.AddConnStateChangeHandler BEFORE Open so the connect transitions are observed.
func newStateRecorder() (*stateRecorder, hsms.StateChangeHandler) {
	r := &stateRecorder{}
	r.cond = sync.NewCond(&r.mu)

	return r, r.record
}

// record appends one transition and wakes any awaitState waiters. It runs on the connection's
// notifier goroutine.
func (r *stateRecorder) record(prev, next hsms.ConnState) {
	r.mu.Lock()
	r.edges = append(r.edges, stateEdge{prev: prev, next: next})
	r.cond.Broadcast()
	r.mu.Unlock()
}

// pairs returns a snapshot copy of the recorded transition sequence, safe to inspect while the
// connection keeps running.
func (r *stateRecorder) pairs() []stateEdge {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]stateEdge(nil), r.edges...)
}

// awaitState blocks until a transition whose next state equals target is recorded AFTER this call
// began, or timeout elapses. It reports whether such a fresh transition occurred.
//
// "Fresh" is the load-bearing detail: it snapshots the current edge count on entry and only scans
// transitions recorded from that point forward, so a target already reached before the call (for
// example the initial Selected) does NOT satisfy the wait — only a genuinely new edge to target
// (for example the re-Selected that follows an involuntary drop) does. It parks on a condition
// variable, woken by record or by a one-shot timer at the deadline, so it never busy-loops.
func (r *stateRecorder) awaitState(target hsms.ConnState, timeout time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Only edges recorded from here forward count as a fresh transition to target.
	scanned := len(r.edges)

	deadline := time.Now().Add(timeout)

	// sync.Cond has no timed wait; a one-shot timer broadcasts at the deadline so a parked waiter
	// wakes to re-check the deadline even when no further transition arrives.
	timer := time.AfterFunc(timeout, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	defer timer.Stop()

	for {
		for ; scanned < len(r.edges); scanned++ {
			if r.edges[scanned].next == target {
				return true
			}
		}

		if !time.Now().Before(deadline) {
			return false
		}

		r.cond.Wait()
	}
}
