package hsms

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidHeaderLength indicates that an invalid header length was provided.
	// The header length should be 10 bytes.
	ErrInvalidHeaderLength = errors.New("hsms: invalid header length, should be 10 bytes")

	// ErrInvalidPType indicates that an invalid PType was provided.
	// The PType should be 0 for SECS-II message.
	ErrInvalidPType = errors.New("hsms: invalid PType, should be 0 for SECS-II message")

	// ErrInvalidControlMsgSType indicates that an invalid SType was provided.
	// The SType should be in range [1, 9] for control messages.
	ErrInvalidControlMsgSType = errors.New("hsms: invalid SType for control message, should be in range of [1, 9]")

	// ErrInvalidRejectMsg indicates that the message is not a valid reject control message.
	ErrInvalidRejectMsg = errors.New("hsms: the message is not a reject control message")

	// ErrInvalidRejectReason indicates that an invalid reject reason was provided.
	ErrInvalidRejectReason = errors.New("hsms: invalid reject reason, should be in range of [1, 4]")

	// ErrInvalidStreamCode indicates that an invalid stream code was provided.
	// Valid stream codes are in the range of 0 to 127.
	ErrInvalidStreamCode = errors.New("hsms: invalid stream code, should be in range of [0, 127]")

	// ErrInvalidRspMsg indicates that the message is not a valid response/secondary message.
	// This is returned when the W-bit (reply expected) is set on a reply (even-function) message.
	ErrInvalidRspMsg = errors.New("hsms: message is not a valid response/secondary message")

	// Connection state and lifecycle errors.
	ErrAlreadyOpen      = errors.New("hsms: connection already open")
	ErrNotOpen          = errors.New("hsms: connection not open")
	ErrNotSelectedState = errors.New("hsms: not in selected state")
	ErrConnClosed       = errors.New("hsms: connection closed")
	ErrT3Timeout        = errors.New("hsms: T3 reply timeout")
	ErrT6Timeout        = errors.New("hsms: T6 control timeout")
	ErrCloseTimeout     = errors.New("hsms: close timeout (tasks still live)")

	// ErrUnrecognizedSessionID indicates a NON-S9F1 inbound data message's SessionID did not match
	// this connection's configured SessionID. Only returned/observable when session-ID validation
	// is enabled via WithSessionIDValidation; the message is dropped and answered with an S9F1,
	// rather than delivered to the registered DataMessageHandlers. An inbound S9F1 is exempted from
	// this check entirely — it is delivered normally regardless of its own SessionID, and this
	// error is never returned for it (see WithSessionIDValidation for why).
	ErrUnrecognizedSessionID = errors.New("hsms: unrecognized session ID")
)

// RejectError is returned by a synchronous send whose transaction the peer answered with an
// HSMS Reject.req (SEMI E37 §7.9) instead of the expected reply. Reason is the E37 reject
// reason code (see RejectSTypeNotSupported..RejectNotSelected).
//
// A Reject terminates only the one transaction; it is never a link failure. Callers inspect it
// with errors.As:
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
