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
	// SEMI E37 §9.3.3.1 specifies that control frames (SType 1–7, 9) must have
	// a message length of exactly 10 bytes (header only, no body). Data messages
	// (SType 0) may carry a body.
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
	ErrInvalidStreamCode = errors.New("hsms: invalid stream code, should be in range of [0, 127]")

	// ErrInvalidRspMsg indicates that the message is not a valid response/secondary message.
	//
	// This is returned when the W-bit (reply expected) is set on a reply (even-function) message.
	ErrInvalidRspMsg = errors.New("hsms: message is not a valid response/secondary message")

	// ErrMessageTooLarge indicates that a data message's on-wire frame size (10-byte header +
	// body) would exceed MaxMessageSize.
	//
	// Returned by the send path before the frame is written; the message is never put on the
	// wire.
	// A control message can never trigger this — its frame is always the fixed 14 bytes.
	ErrMessageTooLarge = errors.New("hsms: message exceeds maximum frame size")

	// Connection state and lifecycle errors.
	ErrAlreadyOpen      = errors.New("hsms: connection already open")
	ErrNotOpen          = errors.New("hsms: connection not open")
	ErrNotSelectedState = errors.New("hsms: not in selected state")
	ErrConnClosed       = errors.New("hsms: connection closed")
	ErrT3Timeout        = errors.New("hsms: T3 reply timeout")
	ErrT6Timeout        = errors.New("hsms: T6 control timeout")
	ErrCloseTimeout     = errors.New("hsms: close timeout (tasks still live)")
	ErrNilMessage       = errors.New("hsms: nil data message")

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
	ErrEvenFunctionPrimary = errors.New("hsms: primary function code must be odd")

	// ErrAsyncReplyExpected indicates SendDataMessageAsync was called with replyExpected true.
	//
	// SendAsync enqueues a message without beginning a reply timer (SEMI E37 §9.4.1.2 requires a
	// primary that expects a reply to begin one), so a W-bit primary sent through the async path
	// can never be conformantly answered.
	// Use SendDataMessage for a reply-bearing transaction.
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
type RejectError struct {
	Reason byte
}

// Error implements the error interface.
func (e *RejectError) Error() string {
	return fmt.Sprintf("hsms: peer rejected message (reason %d)", e.Reason)
}
