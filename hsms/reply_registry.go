package hsms

import "github.com/puzpuzpuz/xsync/v3"

// replyResult carries the outcome of a pending reply: either a decoded reply
// Message or an error (T3 timeout, connection closed, etc.).
type replyResult struct {
	msg Message
	err error
}

// replyWaiter is the registry's value type: the sender-owned reply channel plus enough of the
// primary's identity to validate a candidate reply against SEMI E37 §9.4.1's stream/function
// fields.
// SessionID is deliberately NOT carried here — WithSessionIDValidation already owns
// inbound SessionID policy at an earlier seam (DeliverOwnedFrame's checkSessionID), so a
// registry-level SessionID rule would be dead code behind that drop.
//
// isData records whether the primary was a *DataMessage, as opposed to a *ControlMessage
// (Select/Deselect/Linktest).
// Only a data primary has a stream/function to compare in the first
// place — *ControlMessage has no Stream()/Function() methods at all — and the registration site
// (sendWaitReply) already knows which kind it registered via a type assertion, so storing the
// flag here keeps route's control-transaction exemption explicit rather than re-deriving the
// kind from the routed result.
type replyWaiter struct {
	ch       chan replyResult
	stream   uint8
	function uint8
	isData   bool
}

// replyRegistry maps SystemBytes keys to the sender-owned reply waiters.
//
// Lifecycle invariant (§5.5): the SENDER owns the entire channel lifetime.
// It calls register before sending and deregisters via defer on return —
// whether the reply arrived, timed-out, or the connection was lost.
// The receiver (route) only performs a non-blocking send into the buffered
// channel; it never creates, closes, or deletes the channel.
// A late reply that arrives after deregister either finds no entry (miss → dropped) or
// lands in the cap-1 buffer and is GC'd when the channel itself is GC'd.
// close(ch) is intentionally absent everywhere: this makes the F5
// "send on closed channel" panic class structurally unreachable.
type replyRegistry struct {
	m *xsync.MapOf[[4]byte, replyWaiter]
}

// newReplyRegistry returns an initialised replyRegistry ready for use.
func newReplyRegistry() replyRegistry {
	return replyRegistry{m: xsync.NewMapOf[[4]byte, replyWaiter]()}
}

// register allocates a buffered reply channel for key, stores it alongside the primary's
// stream/function/isData (see replyWaiter), and returns the channel to the sender.
// stream and function are the primary's own values, meaningless (and ignored by route) when
// isData is false.
// The caller is responsible for calling deregister (via defer) when the send operation
// completes or is abandoned.
func (r replyRegistry) register(key [4]byte, stream, function uint8, isData bool) chan replyResult {
	ch := make(chan replyResult, 1)
	r.m.Store(key, replyWaiter{ch: ch, stream: stream, function: function, isData: isData})

	return ch
}

// deregister removes the waiter associated with key from the registry.
// Called by the sender as a deferred cleanup — the channel is NOT closed here.
func (r replyRegistry) deregister(key [4]byte) {
	r.m.Delete(key)
}

// route delivers res to the waiting sender for key using a non-blocking send (SEMI E37 §9.4.1).
//
// It returns (delivered, mismatched).
// delivered reports whether the result was handed to the sender's channel:
// true on every registry hit under the default (observe) posture, and true on a strict-mode hit UNLESS the candidate mismatched.
// mismatched reports whether a compared candidate's stream or function diverged from the registered primary —
// a combined stream+function mismatch counts once, not twice —
// regardless of strict.
//
// A candidate is compared ONLY when both legs hold:
// the entry was registered for a DATA primary (want.isData) AND the routed result itself carries a *DataMessage.
// This is the E37 §9.4.1 control-transaction exemption:
// Select/Deselect/Linktest responses register with isData false (a *ControlMessage has no Stream()/Function() to compare),
// and an inbound Reject.req answering a data primary routes as a field-less *RejectError (res.msg == nil), never a *DataMessage —
// a terminal rejection (E37 §8.3.11), not a reply to validate.
// Both always deliver uncompared.
//
// Function matches primary+1 OR 0 —
// SEMI E5 §7.2/§10.4.1 reserve SxF0 as the transaction-abort secondary, admitted explicitly by E37 §9.4.1.
// The F0 exception is function-only: a wrong-stream F0 is still a stream mismatch.
//
// strict selects the outcome on mismatch:
// false (default) still delivers (observe-only);
// true makes it a miss (delivered stays false) so the message falls through to RouteData as unsolicited —
// the registration is NOT consumed, so a later conforming reply can still complete the transaction.
//
// On a true miss (key absent) it returns (false, false).
// On hit, if the channel is already full (a duplicate reply raced in) the result is silently discarded via the default branch —
// no block, no panic.
func (r replyRegistry) route(key [4]byte, res replyResult, strict bool) (delivered bool, mismatched bool) {
	w, ok := r.m.Load(key)
	if !ok {
		return false, false
	}

	if w.isData {
		if dm, ok := res.msg.(*DataMessage); ok {
			streamMismatch := dm.Stream() != w.stream
			// w.function is uint8, so a primary with function 255 makes w.function+1 wrap to 0 —
			// this coincides harmlessly with the F0 exception, since F0 is the only legal
			// secondary for a function-255 primary anyway.
			functionMismatch := dm.Function() != w.function+1 && dm.Function() != 0
			if streamMismatch || functionMismatch {
				mismatched = true
			}
		}
		// A field-less result (RejectError, res.msg == nil) and any non-data result always
		// deliver uncompared — the type assertion above fails and neither flag is set.
	}

	if mismatched && strict {
		return false, true
	}

	select {
	case w.ch <- res:
	default:
	}

	return true, mismatched
}

// len returns the number of currently registered pending-reply entries.
// Used by the clean-shutdown gate (Task 27) to confirm no sends are in flight.
func (r replyRegistry) len() int {
	return r.m.Size()
}
