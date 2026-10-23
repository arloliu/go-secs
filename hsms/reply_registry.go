package hsms

import (
	"sync/atomic"

	"github.com/puzpuzpuz/xsync/v3"
)

// replyResult carries the outcome of a pending reply: either a decoded reply
// Message or an error (T3 timeout, connection closed, etc.).
type replyResult struct {
	msg Message
	err error
}

// The claim states of a replyWaiter.
// Exactly one result completes a reply wait:
// the first to move the state away from replyOpen sends on the waiter's channel,
// and every later candidate finds the wait already claimed and sends nothing.
const (
	// replyOpen means no result has completed the wait yet.
	replyOpen uint32 = iota
	// replyClaimedByReply means a routed reply completed the wait,
	// or a peer Reject.req that route delivered as a *RejectError.
	replyClaimedByReply
	// replyClaimedByS9 means an S9 error message correlated to the transaction completed the wait.
	// A genuine reply that arrives afterwards is not this wait's to drop:
	// route reports it as a miss, so it falls through to the data handlers.
	replyClaimedByS9
)

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
//
// state is the one-shot completion claim (replyOpen, replyClaimedByReply, replyClaimedByS9).
// The registry stores the waiter by pointer, so every Load sees the same claim;
// a copied waiter would carry a claim of its own and let two results complete one wait.
type replyWaiter struct {
	ch       chan replyResult
	state    atomic.Uint32
	stream   uint8
	function uint8
	isData   bool
}

// replyRegistry maps SystemBytes keys to the sender-owned reply waiters.
//
// Lifecycle invariant (§5.5): the SENDER owns the entire channel lifetime.
// It calls register before sending and deregisters via defer on return —
// whether the reply arrived, timed-out, or the connection was lost.
// The receiver (route) only claims the waiter and then performs a non-blocking send into the buffered channel;
// it never creates, closes, or deletes the channel.
// The claim makes that send happen at most once per registration,
// so the one-slot buffer always has room for it.
// A late reply that arrives after deregister finds no entry (miss → dropped);
// one that arrives after another result claimed the wait, but before deregister, sends nothing.
// close(ch) is intentionally absent everywhere: this makes the F5
// "send on closed channel" panic class structurally unreachable.
type replyRegistry struct {
	m *xsync.MapOf[[4]byte, *replyWaiter]
}

// newReplyRegistry returns an initialised replyRegistry ready for use.
func newReplyRegistry() replyRegistry {
	return replyRegistry{m: xsync.NewMapOf[[4]byte, *replyWaiter]()}
}

// register allocates a buffered reply channel for key, stores it alongside the primary's
// stream/function/isData (see replyWaiter) in an open waiter, and returns the channel to the sender.
// stream and function are the primary's own values, meaningless (and ignored by route) when
// isData is false.
// The caller is responsible for calling deregister (via defer) when the send operation
// completes or is abandoned.
func (r replyRegistry) register(key [4]byte, stream, function uint8, isData bool) chan replyResult {
	ch := make(chan replyResult, 1)
	r.m.Store(key, &replyWaiter{ch: ch, stream: stream, function: function, isData: isData})

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
// delivered reports whether res is this registration's to consume:
// true when res claimed the open wait and was sent to it,
// and also true when an earlier reply had already claimed it (a duplicate; see below).
// mismatched reports whether a compared candidate's stream or function diverged from the registered primary —
// a combined stream+function mismatch counts once, not twice —
// regardless of strict, and regardless of whether res wins the claim.
//
// A candidate is compared ONLY when both legs hold:
// the entry was registered for a DATA primary (want.isData) AND the routed result itself carries a *DataMessage.
// This is the E37 §9.4.1 control-transaction exemption:
// Select/Deselect/Linktest responses register with isData false (a *ControlMessage has no Stream()/Function() to compare),
// and an inbound Reject.req answering a data primary routes as a field-less *RejectError (res.msg == nil), never a *DataMessage —
// a terminal rejection (E37 §8.3.11), not a reply to validate.
// Both always deliver uncompared.
//
// A third combination looks similar but is NOT uncompared delivery:
// a DATA registration (want.isData) answered by a *ControlMessage result.
// A control response cannot answer a data primary —
// it has no Stream()/Function() to validate against one in the first place —
// so route reports a miss instead of delivering it: (false, false), unconditionally, regardless of strict.
// The registration is NOT consumed,
// so the genuine data reply can still complete it,
// and the miss lets the caller answer the stray control response with Reject(TransactionNotOpen, reason 3) per SEMI E37 §8.3.20.
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
// Only a candidate that survives those checks claims the wait,
// and only the claim's winner sends on the channel.
// A candidate that finds the wait already claimed sends nothing, and its outcome depends on who won:
//   - an earlier reply: (true, mismatched) — a duplicate reply is reported delivered and dropped;
//   - an S9 error message: (false, mismatched) — the genuine reply misses,
//     so it falls through to the data handlers instead of vanishing into a wait that has already ended.
//
// Either outcome holds whether or not the sender has drained the channel yet,
// since the claim, not the buffer, records that the wait is over.
//
// On a true miss (key absent) it returns (false, false).
func (r replyRegistry) route(key [4]byte, res replyResult, strict bool) (delivered bool, mismatched bool) {
	w, ok := r.m.Load(key)
	if !ok {
		return false, false
	}

	if w.isData {
		// A control response cannot answer a data primary: report a miss,
		// so the transport can Reject it (reason 3, E37 §8.3.20) and the transaction stays open for its genuine reply.
		// A RejectError result (res.msg == nil) still delivers below.
		if _, ok := res.msg.(*ControlMessage); ok {
			return false, false
		}

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
		// A field-less result (RejectError, res.msg == nil) always delivers uncompared — the type assertion above fails
		// and neither flag is set.
	}

	if mismatched && strict {
		return false, true
	}

	if !w.state.CompareAndSwap(replyOpen, replyClaimedByReply) {
		// The wait already has its result; this candidate is not sent.
		if w.state.Load() == replyClaimedByS9 {
			return false, mismatched
		}

		return true, mismatched
	}

	// The claim makes this the registration's only send, so the one-slot buffer has room;
	// the select stays non-blocking anyway, so route can never stall the receive loop.
	select {
	case w.ch <- res:
	default:
	}

	return true, mismatched
}

// len returns the number of currently registered pending-reply entries.
// Used by the clean-shutdown gate to confirm no sends are in flight.
func (r replyRegistry) len() int {
	return r.m.Size()
}
