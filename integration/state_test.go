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

// mark returns the current edge count, to be passed to a later awaitStateFrom as its scan origin.
//
// Take the mark BEFORE the action that triggers the transition, never after.
// The notifier records on its own goroutine,
// so a teardown can be recorded while the test goroutine is still descheduled between the trigger and the wait.
// secs1 tears down within one linePollInterval (10ms), which is well inside a scheduling hiccup under -race.
// A mark taken after the trigger can therefore sit past the very edge the wait is looking for,
// and the wait then blocks for its full timeout on a transition that already happened.
func (r *stateRecorder) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.edges)
}

// awaitStateFrom blocks until a transition whose next state equals target is recorded at or after edge index from,
// or timeout elapses.
// It returns the index the match was found at and whether a match occurred;
// on timeout it returns from unchanged and false.
//
// Chain sequential waits with the returned index — pass matched+1 to the next call —
// so each wait scans strictly past its predecessor's match.
// Restarting a follow-up wait from the ORIGINAL mark would let an edge recorded before the trigger satisfy it:
// after an involuntary drop,
// a Selected wait rooted at the pre-drop mark matches the INITIAL Selected and returns immediately,
// passing without any reconnect having occurred.
func (r *stateRecorder) awaitStateFrom(from int, target hsms.ConnState, timeout time.Duration) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	scanned := from
	if scanned < 0 {
		scanned = 0
	}

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
				return scanned, true
			}
		}

		if !time.Now().Before(deadline) {
			return from, false
		}

		r.cond.Wait()
	}
}
