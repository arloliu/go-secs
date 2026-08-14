package hsms

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidHeaderLength indicates that an invalid header length was provided.
	//
	// The header length should be 10 bytes.
	ErrInvalidHeaderLength = errors.New("hsms: invalid header length, should be 10 bytes")

	// ErrInvalidPType indicates that an invalid PType was provided.
	//
	// The PType should be 0 for SECS-II message.
	ErrInvalidPType = errors.New("hsms: invalid PType, should be 0 for SECS-II message")

	// ErrInvalidControlMsgSType indicates that an invalid SType was provided.
	//
	// The SType should be in range [1, 9] for control messages.
	ErrInvalidControlMsgSType = errors.New("hsms: invalid SType for control message, should be in range of [1, 9]")

	// ErrControlFrameWithBody indicates that a control frame carries a message body.
	//
	// SEMI E37 §9.3.3.1 specifies that control frames (SType 1–7, 9) must have a message length of exactly 10 bytes (header only, no body).
	// Data messages (SType 0) may carry a body.
	ErrControlFrameWithBody = errors.New("hsms: control frame must not carry a body (E37 §9.3.3.1)")

	// ErrInvalidRejectMsg indicates that the message is not a valid reject control message.
	ErrInvalidRejectMsg = errors.New("hsms: the message is not a reject control message")

	// ErrInvalidRejectReason indicates that an invalid reject reason was provided.
	//
	// SEMI E37 Table 9 defines reason codes 1-4, reserves 5-127 for subsidiary standards, and
	// reserves 128-255 for local entities — so every non-zero byte is a valid reason code, and
	// only 0 is invalid.
	ErrInvalidRejectReason = errors.New("hsms: invalid reject reason, should be in range of [1, 255]")

	// ErrInvalidStreamCode indicates that an invalid stream code was provided.
	//
	// Valid stream codes are in the range of 0 to 127.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// This is a caller-side construction error, and the same stream code fails on every retry.
	ErrInvalidStreamCode = errors.New("hsms: invalid stream code, should be in range of [0, 127]")

	// ErrInvalidRspMsg indicates that the message is not a valid response/secondary message.
	//
	// This is returned when the W-bit (reply expected) is set on a reply (even-function) message.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// This is a caller-side construction error, and the same message fails on every retry.
	ErrInvalidRspMsg = errors.New("hsms: message is not a valid response/secondary message")

	// ErrMessageTooLarge indicates that a data message's on-wire frame size (10-byte header + body) would exceed MaxMessageSize.
	//
	// Returned by the send path before the frame is written; the message is never put on the wire.
	// A control message can never trigger this — its frame is always the fixed 14 bytes.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// The same payload exceeds the ceiling on every retry.
	// Only a smaller message or a larger MaxMessageSize changes the outcome.
	ErrMessageTooLarge = errors.New("hsms: message exceeds maximum frame size")

	// ErrAlreadyOpen indicates Open was called on a connection that is already open (spec §5.2, H6).
	//
	// Classification: IsTransient and IsTimeout both report false.
	// The connection is already usable, so retrying Open is never the right response — using the existing connection is.
	ErrAlreadyOpen = errors.New("hsms: connection already open")

	// ErrNotOpen indicates a call was made before Open,
	// or after Close on a connection that was never reopened.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// The same call fails until the caller opens the connection.
	// Retrying without an intervening Open never changes the outcome.
	ErrNotOpen = errors.New("hsms: connection not open")

	// ErrNotSelectedState indicates a data send was refused because the session is not in the Selected state (spec §5.5, the B1/B2 gates).
	//
	// Classification: IsTransient reports true.
	// A reconnect or Select handshake may be in progress, and the same send can succeed once it completes.
	// IsTimeout reports false.
	ErrNotSelectedState = errors.New("hsms: not in selected state")

	// ErrConnClosed indicates the generation backing a send or wait was torn down while the call was in flight (spec §5.2/§5.5):
	// a voluntary Close, an involuntary drop, or a wait that lost the race against teardown.
	//
	// Classification: IsTransient reports true.
	// After an involuntary drop, the connection's own reconnect loop re-establishes the link.
	// The same call can succeed once it does.
	// (After a voluntary Close, the caller must Open again first.)
	// IsTimeout reports false.
	ErrConnClosed = errors.New("hsms: connection closed")

	// ErrT3Timeout indicates a data transaction's reply did not arrive within the configured T3 interval (SEMI E37 §9.4.1, the reply-expected timer).
	//
	// Classification: IsTransient and IsTimeout both report true.
	// A T3 expiry is a protocol timer firing, and the peer may answer the next attempt.
	ErrT3Timeout = errors.New("hsms: T3 reply timeout")

	// ErrT6Timeout indicates a control transaction (Select.req, Linktest.req) did not receive its response within the configured T6 interval
	// (SEMI E37 §9.4.1, the control-transaction timer).
	//
	// Classification: IsTransient and IsTimeout both report true, for the same reason as ErrT3Timeout.
	// A T6 expiry is a protocol timer firing, and the peer may answer the next attempt.
	ErrT6Timeout = errors.New("hsms: T6 control timeout")

	// ErrCloseTimeout indicates Close's bounded shutdown join exceeded the configured close timeout with tasks still live (spec §5.2, §7.A);
	// the straggler is abandoned rather than awaited further.
	//
	// Classification: IsTransient reports false.
	// Close is idempotent, so a second call returns this same cached result rather than re-attempting the join.
	// Retrying can never produce a different outcome.
	// IsTimeout reports true: it names a real deadline expiry,
	// even though — unlike ErrT3Timeout/ErrT6Timeout — that expiry does not make a retry worthwhile.
	ErrCloseTimeout = errors.New("hsms: close timeout (tasks still live)")

	// ErrNilMessage indicates SendSECS2Message, ReplyDataMessage, ForwardDataMessage,
	// or ForwardDataMessageAsync was called with a nil message.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// This is a caller-side argument error, and the same nil argument fails on every retry.
	ErrNilMessage = errors.New("hsms: nil data message")

	// ErrUnrecognizedSessionID indicates a NON-S9F1 inbound data message's SessionID did not match this connection's configured SessionID.
	//
	// Only returned/observable when session-ID validation is enabled via WithSessionIDValidation; the message is dropped
	// and answered with an S9F1, rather than delivered to the registered DataMessageHandlers.
	// An inbound S9F1 is exempted from this check entirely — it is delivered normally regardless of its own SessionID,
	// and this error is never returned for it (see WithSessionIDValidation for why).
	ErrUnrecognizedSessionID = errors.New("hsms: unrecognized session ID")

	// ErrEvenFunctionPrimary indicates a primary send was attempted with an even function code.
	//
	// SendDataMessage, SendDataMessageAsync, and SendSECS2Message each mint fresh System Bytes for
	// every call, so every message sent through them always opens a new transaction and is always a
	// primary (SEMI E5 §7.2: a primary function must be odd). A reply message must instead go through
	// ReplyDataMessage, which derives its even function from the primary it answers.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// This is a caller bug, and the same function code fails on every retry.
	ErrEvenFunctionPrimary = errors.New("hsms: primary function code must be odd")

	// ErrAsyncReplyExpected indicates SendDataMessageAsync was called with replyExpected true.
	//
	// SendAsync enqueues a message without beginning a reply timer (SEMI E37 §9.4.1.2 requires a
	// primary that expects a reply to begin one), so a W-bit primary sent through the async path
	// can never be conformantly answered.
	// Use SendDataMessage for a reply-bearing transaction.
	//
	// Classification: IsTransient and IsTimeout both report false.
	// This is a caller bug, and the same call fails on every retry.
	ErrAsyncReplyExpected = errors.New("hsms: async send cannot expect a reply")
)

// RejectError is returned by a synchronous send whose transaction the peer answered with an HSMS Reject.req (SEMI E37 §7.10) instead of the expected reply.
//
// Reason is the E37 reject reason code the peer sent, verbatim.
// RejectSTypeNotSupported..RejectNotSelected are the codes this package defines;
// SEMI E37 Table 9 reserves the rest for subsidiary standards and local entities.
//
// A Reject terminates only the one transaction; it is never a link failure.
// Callers inspect it with errors.As:
//
//	var re *hsms.RejectError
//	if errors.As(err, &re) {
//		// re.Reason is the E37 reject reason code
//	}
//
// Classification: RejectError has no built-in IsTransient/IsTimeout entry.
// Reason codes span causes that retry differently — a stale transaction vs. a not-yet-Selected peer.
// It falls through to the conservative default:
// both report false unless the caller wraps or replaces it with an error implementing TransientError/TimeoutError.
type RejectError struct {
	Reason byte
}

// Error implements the error interface.
func (e *RejectError) Error() string {
	return fmt.Sprintf("hsms: peer rejected message (reason %d)", e.Reason)
}
