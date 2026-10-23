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
// route deliberately does NOT compare a reply's SessionID —
// WithSessionIDValidation already owns inbound SessionID policy at an earlier seam (DeliverOwnedFrame's checkSessionID),
// so a registry-level SessionID rule would be dead code behind that drop.
//
// isData records whether the primary was a *DataMessage, as opposed to a *ControlMessage
// (Select/Deselect/Linktest).
// Only a data primary has a stream/function to compare in the first place —
// *ControlMessage has no Stream()/Function() methods at all —
// and the registration site (sendWaitReply) already knows which kind it registered,
// since it calls registerData or registerControl after a type assertion,
// so storing the flag here keeps route's control-transaction exemption explicit
// rather than re-deriving the kind from the routed result.
//
// header is the data primary's 10-byte header as it was sent, session ID included:
// the identity an S9 error message's MHEAD body is compared against (see failS9).
// It is zero for a control registration, which failS9 never matches.
//
// state is the one-shot completion claim (replyOpen, replyClaimedByReply, replyClaimedByS9).
// The registry stores the waiter by pointer, so every Load sees the same claim;
// a copied waiter would carry a claim of its own and let two results complete one wait.
type replyWaiter struct {
	ch       chan replyResult
	state    atomic.Uint32
	header   [10]byte
	stream   uint8
	function uint8
	isData   bool
}

// replyRegistry maps SystemBytes keys to the sender-owned reply waiters.
//
// Lifecycle invariant (§5.5): the SENDER owns the entire channel lifetime.
// It registers (registerData or registerControl) before sending and deregisters via defer on return —
// whether the reply arrived, timed-out, or the connection was lost.
// The receiver (route or failS9) only claims the waiter and then performs a non-blocking send into the buffered channel;
// it never creates, closes, or deletes the channel.
// The claim makes that send happen at most once per registration,
// so the one-slot buffer always has room for it.
// A late reply that arrives after deregister finds no entry (miss → dropped);
// one that arrives after another result claimed the wait, but before deregister, sends nothing.
// A receiver that loaded the waiter just before deregister can still claim the open wait
// and send into the buffer the sender has abandoned:
// it is reported delivered, and the result is garbage-collected with the channel.
// close(ch) is intentionally absent everywhere: this makes the F5
// "send on closed channel" panic class structurally unreachable.
type replyRegistry struct {
	m *xsync.MapOf[[4]byte, *replyWaiter]
}

// newReplyRegistry returns an initialised replyRegistry ready for use.
func newReplyRegistry() replyRegistry {
	return replyRegistry{m: xsync.NewMapOf[[4]byte, *replyWaiter]()}
}

// registerData opens a reply wait for the data primary whose sent header is header,
// and returns the wait's buffered reply channel to the sender.
// The key, stream, and function all come from that one header,
// so the identity route compares a reply against
// and the identity failS9 compares an MHEAD against can never disagree.
// The caller is responsible for calling deregister (via defer)
// when the send operation completes or is abandoned.
func (r replyRegistry) registerData(header [10]byte) chan replyResult {
	return r.store([4]byte(header[6:10]), &replyWaiter{
		header:   header,
		stream:   header[2] & 0x7F,
		function: header[3],
		isData:   true,
	})
}

// registerControl opens a reply wait for the control primary (Select, Deselect, or Linktest) whose System Bytes are key,
// and returns the wait's buffered reply channel to the sender.
// A control waiter carries no stream, function, or header:
// route never compares a candidate against it, and failS9 never matches it.
// The caller is responsible for calling deregister (via defer)
// when the send operation completes or is abandoned.
func (r replyRegistry) registerControl(key [4]byte) chan replyResult {
	return r.store(key, &replyWaiter{})
}

// store gives w a fresh one-slot reply channel, publishes w as the open waiter for key, and returns the channel.
func (r replyRegistry) store(key [4]byte, w *replyWaiter) chan replyResult {
	w.ch = make(chan replyResult, 1)
	r.m.Store(key, w)

	return w.ch
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

// failS9 completes the open data wait that an S9 error message reports a fault in, with res.
//
// mhead is the S9's body: the header of our message as the peer received it (SEMI E5 §10.13).
// Its System Bytes (mhead[6:10]) are the key, never the S9 envelope's own.
// It matches only a waiter registered for a data primary,
// and only when mhead identifies that primary under policy (see mheadPolicy).
// The match then claims the open wait with replyClaimedByS9 and sends res to the sender.
//
// It reports whether res completed the wait.
// It reports false, and sends nothing, when no data waiter matches,
// or when another result already claimed the wait.
func (r replyRegistry) failS9(mhead [10]byte, policy mheadPolicy, res replyResult) bool {
	w, ok := r.m.Load([4]byte(mhead[6:10]))
	if !ok || !w.isData || !policy.matches(mhead, w.header) {
		return false
	}

	if !w.state.CompareAndSwap(replyOpen, replyClaimedByS9) {
		return false
	}

	// The claim makes this the registration's only send, so the one-slot buffer has room.
	select {
	case w.ch <- res:
	default:
	}

	return true
}

// mheadPolicy selects how failS9 compares an S9's MHEAD body against the header a data primary was sent with.
// A connection chooses it once, from its transport, when it is constructed.
type mheadPolicy uint8

const (
	// mheadExact requires all 10 bytes to be equal.
	// An HSMS peer's MHEAD is the 10-byte HSMS message header (SEMI E37 §9.4.2.1),
	// so the session ID, the W-bit, stream, function, PType, SType, and System Bytes must all be the ones sent.
	mheadExact mheadPolicy = iota

	// mheadBlock compares a SECS-I peer's MHEAD, the block header it received (SEMI E4 §8),
	// against the HSMS-form header that was sent.
	// The device ID must be equal, ignoring the R-bit (the top bit of byte 0),
	// and so must the W-bit with stream, function, and System Bytes.
	// Bytes 4 and 5, the E-bit and block number, are ignored:
	// they describe the block the peer read, not the message.
	mheadBlock
)

// matches reports whether mhead identifies the message that was sent with header under p.
func (p mheadPolicy) matches(mhead, header [10]byte) bool {
	if p == mheadExact {
		return mhead == header
	}

	return mhead[0]&0x7F == header[0]&0x7F &&
		mhead[1] == header[1] &&
		mhead[2] == header[2] &&
		mhead[3] == header[3] &&
		[4]byte(mhead[6:10]) == [4]byte(header[6:10])
}

// len returns the number of currently registered pending-reply entries.
// Used by the clean-shutdown gate to confirm no sends are in flight.
func (r replyRegistry) len() int {
	return r.m.Size()
}
