package hsms

import "github.com/puzpuzpuz/xsync/v3"

// replyResult carries the outcome of a pending reply: either a decoded reply
// Message or an error (T3 timeout, connection closed, etc.).
type replyResult struct {
	msg Message
	err error
}

// replyRegistry maps SystemBytes keys to the sender-owned reply channels.
//
// Lifecycle invariant (§5.5): the SENDER owns the entire channel lifetime.
// It calls register before sending and deregisters via defer on return —
// whether the reply arrived, timed-out, or the connection was lost.  The
// receiver (route) only performs a non-blocking send into the buffered
// channel; it never creates, closes, or deletes the channel.  A late reply
// that arrives after deregister either finds no entry (miss → dropped) or
// lands in the cap-1 buffer and is GC'd when the channel itself is GC'd.
// close(ch) is intentionally absent everywhere: this makes the F5
// "send on closed channel" panic class structurally unreachable.
type replyRegistry struct {
	m *xsync.MapOf[[4]byte, chan replyResult]
}

// newReplyRegistry returns an initialised replyRegistry ready for use.
func newReplyRegistry() replyRegistry {
	return replyRegistry{m: xsync.NewMapOf[[4]byte, chan replyResult]()}
}

// register allocates a buffered reply channel for key, stores it, and returns
// it to the sender.  The caller is responsible for calling deregister (via
// defer) when the send operation completes or is abandoned.
func (r replyRegistry) register(key [4]byte) chan replyResult {
	ch := make(chan replyResult, 1)
	r.m.Store(key, ch)

	return ch
}

// deregister removes the channel associated with key from the registry.
// Called by the sender as a deferred cleanup — the channel is NOT closed here.
func (r replyRegistry) deregister(key [4]byte) {
	r.m.Delete(key)
}

// route delivers res to the waiting sender for key using a non-blocking send.
// Returns true if the key was found (hit), false if it was absent (miss).
// On hit, if the channel is already full (a duplicate reply raced in) the
// result is silently discarded via the default branch — no block, no panic.
func (r replyRegistry) route(key [4]byte, res replyResult) bool {
	ch, ok := r.m.Load(key)
	if !ok {
		return false
	}

	select {
	case ch <- res:
	default:
	}

	return true
}

// len returns the number of currently registered pending-reply entries.
// Used by the clean-shutdown gate (Task 27) to confirm no sends are in flight.
func (r replyRegistry) len() int {
	return r.m.Size()
}
