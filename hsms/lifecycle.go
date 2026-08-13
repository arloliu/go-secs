package hsms

import "strconv"

// TransitionCause names why a connection state transition happened.
//
// It is a closed set: every value the engine can report is listed below.
// A transition whose origin the engine cannot name reports [CauseUnknown],
// which is an honest fallback rather than an error marker.
//
// A cause is a classification, not a retained error:
// it is a plain enumerated value, so observing one never keeps a failed transaction's error alive.
type TransitionCause uint8

const (
	// CauseUnknown means the engine could not name what drove the transition.
	//
	// A transport that reports a lost link without naming a reason lands here.
	CauseUnknown TransitionCause = iota

	// CauseLocalOpen means Open — or the automatic reconnect that follows a drop — established the transport link.
	CauseLocalOpen

	// CauseLocalClose means a local Close initiated the transition.
	//
	// The rollback of an Open that failed after the link was already up reports this cause too:
	// the teardown is locally initiated either way.
	CauseLocalClose

	// CauseSelectAccepted means the select handshake completed and the session became selected (SEMI E37 §7.4).
	//
	// A SECS-I connection reports this cause as well:
	// that transport has no select handshake, so it commits to the selected state as soon as the line is up.
	CauseSelectAccepted

	// CauseSelectRejected means the peer answered our Select.req with a failure select-status,
	// so the link was dropped and will be re-established (SEMI E37 §7.4).
	CauseSelectRejected

	// CausePeerSeparate means the peer sent Separate.req to announce it is leaving (SEMI E37.1 §7.6).
	CausePeerSeparate

	// CausePeerDeselect means the peer sent Deselect.req,
	// so the session left the selected state while the transport link stayed up (SEMI E37 §7.7).
	CausePeerDeselect

	// CauseT6Timeout means a control transaction did not complete within T6.
	CauseT6Timeout

	// CauseT7Timeout means the not-selected dwell expired: the link stayed unselected for T7 (SEMI E37 §9.2.2).
	CauseT7Timeout

	// CauseLinktestFail means consecutive linktest probes timed out up to the configured failure threshold (see [WithLinktestFailThreshold]).
	CauseLinktestFail

	// CauseIOError means a transport read or write failed, or the peer closed the socket without announcing it.
	CauseIOError
)

// LifecycleEvent is one observed connection state transition together with the cause that drove it.
//
// It is delivered to the callbacks registered through [Connection.SubscribeLifecycle].
type LifecycleEvent struct {
	// Previous is the state the connection left.
	Previous ConnState

	// Current is the state the connection entered.
	Current ConnState

	// Cause names what drove the transition.
	Cause TransitionCause
}

// lifecycleSub is one cancellable subscription: the caller's callback plus the id its cancel function removes.
// Subscriptions live on the connection (like the legacy state-change handlers) so they survive the per-Open supervisor.
type lifecycleSub struct {
	id uint64
	fn func(LifecycleEvent)
}

// String returns the string representation of the TransitionCause.
func (c TransitionCause) String() string {
	switch c {
	case CauseUnknown:
		return "Unknown"
	case CauseLocalOpen:
		return "LocalOpen"
	case CauseLocalClose:
		return "LocalClose"
	case CauseSelectAccepted:
		return "SelectAccepted"
	case CauseSelectRejected:
		return "SelectRejected"
	case CausePeerSeparate:
		return "PeerSeparate"
	case CausePeerDeselect:
		return "PeerDeselect"
	case CauseT6Timeout:
		return "T6Timeout"
	case CauseT7Timeout:
		return "T7Timeout"
	case CauseLinktestFail:
		return "LinktestFail"
	case CauseIOError:
		return "IOError"
	default:
		return "TransitionCause(" + strconv.FormatUint(uint64(c), 10) + ")"
	}
}

// SubscribeLifecycle registers fn to observe state transitions and returns the function that cancels the subscription.
//
// See the [Connection] interface for the full contract.
func (c *connection) SubscribeLifecycle(fn func(LifecycleEvent)) (cancel func()) {
	if fn == nil {
		return func() {}
	}

	// Ids come from a per-connection monotonic counter starting at 1,
	// so a cancel closure always names exactly the entry its own Subscribe call appended,
	// even after the slice has been rebuilt many times.
	id := c.lifecycleSeq.Add(1)

	// Lock-free CAS append onto an immutable slice, the same pattern AddConnStateChangeHandler uses.
	// The notifier reads the pointer once per event and iterates that snapshot, so it never observes a half-built slice.
	for {
		old := c.lifecycleSubs.Load()

		var updated []lifecycleSub
		if old != nil {
			updated = make([]lifecycleSub, len(*old), len(*old)+1)
			copy(updated, *old)
		} else {
			updated = make([]lifecycleSub, 0, 1)
		}
		updated = append(updated, lifecycleSub{id: id, fn: fn})

		if c.lifecycleSubs.CompareAndSwap(old, &updated) {
			break
		}
	}

	return func() { c.cancelLifecycle(id) }
}

// cancelLifecycle removes the subscription carrying id, if it is still registered.
//
// It is a lock-free CAS rebuild, never a mutex,
// so a callback that cancels its own subscription from inside the notifier cannot deadlock against the delivery running it.
// An id that is already gone makes this a no-op, which is what makes a repeated cancel harmless.
func (c *connection) cancelLifecycle(id uint64) {
	for {
		old := c.lifecycleSubs.Load()
		if old == nil {
			return
		}

		idx := -1

		for i := range *old {
			if (*old)[i].id == id {
				idx = i

				break
			}
		}

		if idx < 0 {
			return // already cancelled
		}

		updated := make([]lifecycleSub, 0, len(*old)-1)
		updated = append(updated, (*old)[:idx]...)
		updated = append(updated, (*old)[idx+1:]...)

		if c.lifecycleSubs.CompareAndSwap(old, &updated) {
			return
		}
	}
}
