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
	// TxAborted indicates the peer answered a reply-expected send with function 0 (SxF0, SEMI E5 §10.4.1)
	// while [WithAbortReplyError] is enabled; TxEvent.Err is [ErrTransactionAborted].
	// With the option disabled, an SxF0 reply is reported as [TxReplied].
	TxAborted
	// TxPeerError indicates the peer answered a reply-expected send with a Stream 9 error message
	// while [WithS9Correlation] is enabled; TxEvent.Err is an [*S9Error].
	// With the option disabled, such a send waits out T3 and is reported as [TxT3Timeout].
	TxPeerError
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
	ReplyWaited bool          // the message's W-bit for SendDataMessage/SendSECS2Message, even on a failure before waiting; always false for ForwardDataMessage
	Duration    time.Duration // wall-clock time from the call's start to its outcome
	Outcome     TxOutcome
	Err         error // nil on TxReplied / TxSent; the classifying error otherwise

	// SessionID is the data message's session id, as its header carries it.
	SessionID uint16

	// Socket identifies the socket of the generation the send was bound to, as [SocketEvent.Socket] does.
	// A send is bound to the generation current when it started, and keeps that binding to the end,
	// so a send that completes after a reconnect still names that generation's socket,
	// even though the socket has since been closed.
	// It is 0 when that generation never acquired a socket, when the transport does not identify its sockets,
	// and when the connection was not open ([ErrNotOpen]).
	Socket uint64

	// Generation identifies the connection generation the send was bound to, as [WireEvent.Generation] does.
	// It is 0 only when the connection was not open ([ErrNotOpen]), so no generation was bound.
	Generation uint64
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
	case TxAborted:
		return "Aborted"
	case TxPeerError:
		return "PeerError"
	default:
		return "TxOutcome(" + strconv.FormatUint(uint64(o), 10) + ")"
	}
}
