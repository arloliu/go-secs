package secs1

import (
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// SECS-I block-framing and line-transfer sentinel errors.
var (
	// ErrInvalidLength is returned by parseBlock when the length byte or the data length is out of the SECS-I range (header 10 .. header+244 = 254 bytes).
	ErrInvalidLength = errors.New("secs1: invalid block length")
	// ErrChecksumMismatch is returned by parseBlock when the trailing checksum does not match the computed 16-bit sum of the header
	// and body.
	ErrChecksumMismatch = errors.New("secs1: block checksum mismatch")
	// ErrDeviceIDMismatch is returned by the inbound assembler when a received block's device ID does not match this connection's configured device ID (SEMI E4 §9.4.1 routing check).
	ErrDeviceIDMismatch = errors.New("secs1: block device ID mismatch")
	// ErrInvalidFirstBlock is returned by the inbound assembler when a block that would start a new message is neither block number 1 nor a lone block 0 with the E-bit set (SEMI E4 §9.4.4.2).
	ErrInvalidFirstBlock = errors.New("secs1: invalid first block")
	// ErrEmptyBlocks is returned by assembleBlocks when given no blocks.
	ErrEmptyBlocks = errors.New("secs1: no blocks to assemble")
	// ErrBlockNumberMismatch is returned by assembleBlocks when block numbers are not the contiguous sequence 1..N.
	ErrBlockNumberMismatch = errors.New("secs1: block number mismatch")
	// ErrEBitPlacement is returned by assembleBlocks when the E-bit (last-block flag) is not set exactly on the final block.
	ErrEBitPlacement = errors.New("secs1: E-bit not set exactly on the last block")
	// ErrHeaderMismatch is returned by assembleBlocks when the block-invariant header fields differ across blocks of the same message.
	ErrHeaderMismatch = errors.New("secs1: block header fields differ across blocks")
	// ErrMessageTooLarge is returned by splitBody when the body exceeds the maximum SECS-I message size (244 * 32767 bytes).
	//
	// Classification: unmarked, so hsms.IsTransient and hsms.IsTimeout both report false by default.
	// The same body exceeds the ceiling on every retry — it is never a link failure.
	ErrMessageTooLarge = errors.New("secs1: message body exceeds maximum SECS-I size")
	// ErrInvalidHeader is returned by splitBody when deviceID > 0x7FFF or stream > 0x7F.
	//
	// Classification: unmarked, so hsms.IsTransient and hsms.IsTimeout both report false by default.
	// This is a caller-side construction error; the same header fields fail on every retry.
	ErrInvalidHeader = errors.New("secs1: invalid SECS-I header field")
	// ErrT1Timeout is returned by the line transfer when the T1 inter-character timeout elapses between bytes of a block (SEMI E4 §7.3.1).
	ErrT1Timeout = errors.New("secs1: T1 inter-character timeout")
	// ErrT2Timeout is returned by the line transfer when the T2 protocol timeout elapses while waiting for a handshake reply such as EOT, the length byte, or ACK (SEMI E4 §7.8).
	ErrT2Timeout = errors.New("secs1: T2 protocol timeout")
	// ErrSendFailed is returned when a block's RTY retry limit is exhausted without an ACK (SEMI E4 §7.8.2).
	//
	// The connection treats it as a line failure: it tears the link down and reconnects.
	//
	// Classification: implements hsms.TransientError (Transient() reports true), so hsms.IsTransient(err) is true;
	// the identity is preserved — errors.Is(err, ErrSendFailed) still holds, only the dynamic type gained the marker.
	// hsms.IsTimeout reports false: RTY exhaustion is a line failure diagnosed by retry count, not by a timer expiring.
	ErrSendFailed error = transientError{errors.New("secs1: block send failed, retries exhausted")}
)

// transientError narrows a secs1 sentinel to also satisfy hsms.TransientError,
// marking it safe to retry without changing the sentinel's own identity or wire text.
// The sentinel var keeps its name, and errors.Is compares by value, not by dynamic type,
// so errors.Is(err, ErrSendFailed) still holds for a wrapped or errors.Join'd form —
// only the var's dynamic type gains a Transient() method.
type transientError struct {
	error
}

var _ hsms.TransientError = transientError{}

// Transient implements hsms.TransientError: a SECS-I line failure may clear on the next attempt
// (the physical link recovers, or the peer stops NAKing), so the failed send is safe to retry.
func (transientError) Transient() bool { return true }
