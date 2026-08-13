package hsms

import (
	"strconv"
	"time"
)

// TxOutcome classifies how one synchronous send transaction ended (see [TxEvent]).
type TxOutcome uint8

const (
	// TxReplied indicates a reply-expected (W-bit) send received its correlated reply — a peer
	// Reject.req is reported as [TxRejected] instead, never TxReplied.
	TxReplied TxOutcome = iota
	// TxSent indicates the frame reached the wire and no reply was awaited by this call:
	// either a W-bit-clear data send,
	// or [SECS2Endpoint.ForwardDataMessage],
	// which never opens a local reply-wait transaction regardless of the forwarded message's own W-bit.
	TxSent
	// TxT3Timeout indicates a reply-expected send's T3 reply-wait timer expired before a reply
	// arrived.
	TxT3Timeout
	// TxRejected indicates the peer answered the send with a Reject.req; TxEvent.Err is a
	// [*RejectError].
	TxRejected
	// TxCanceled indicates the send unwound for a lifecycle reason rather than a transport failure:
	// the caller's context was canceled or its deadline expired,
	// or the connection was closed (ErrConnClosed) while the send was outstanding.
	// This mirrors [ConnectionMetrics.DataMsgErrCount]'s own exclusion of both cases from its error count.
	TxCanceled
	// TxSendError indicates the send failed for a reason this package treats as a transaction error:
	// a genuine transport write failure,
	// a B1/B2 not-selected drop (ErrNotSelectedState),
	// an oversized frame (ErrMessageTooLarge),
	// or the connection not being open yet (ErrNotOpen).
	TxSendError
)

// TxEvent reports the outcome of one completed synchronous send transaction — see
// [WithTransactionObserver] for which calls report one and when.
//
// TxEvent is a value, not a pointer, and safe to retain or pass to another goroutine after the
// observer returns: Err is its only field that can itself point at shared state, and errors are
// immutable by convention in this package.
type TxEvent struct {
	Stream      uint8         // the data message's SECS-II stream code
	Function    uint8         // the data message's SECS-II function code
	ID          uint32        // System Bytes as uint32 (matches DataMessage.ID)
	ReplyWaited bool          // true when the message's W-bit asked the peer for a reply
	Duration    time.Duration // wall-clock time from the call's start to its outcome
	Outcome     TxOutcome
	Err         error // nil on TxReplied / TxSent; the classifying error otherwise
}

// String returns the string representation of the TxOutcome.
func (o TxOutcome) String() string {
	switch o {
	case TxReplied:
		return "Replied"
	case TxSent:
		return "Sent"
	case TxT3Timeout:
		return "T3Timeout"
	case TxRejected:
		return "Rejected"
	case TxCanceled:
		return "Canceled"
	case TxSendError:
		return "SendError"
	default:
		return "TxOutcome(" + strconv.FormatUint(uint64(o), 10) + ")"
	}
}
