package secs1

import "errors"

// SECS-I block-framing and line-transfer sentinel errors.
var (
	// ErrInvalidLength is returned by parseBlock when the length byte or the data length is out
	// of the SECS-I range (header 10 .. header+244 = 254 bytes).
	ErrInvalidLength = errors.New("secs1: invalid block length")
	// ErrChecksumMismatch is returned by parseBlock when the trailing checksum does not match the
	// computed 16-bit sum of the header and body.
	ErrChecksumMismatch = errors.New("secs1: block checksum mismatch")
	// ErrEmptyBlocks is returned by assembleBlocks when given no blocks.
	ErrEmptyBlocks = errors.New("secs1: no blocks to assemble")
	// ErrBlockNumberMismatch is returned by assembleBlocks when block numbers are not the
	// contiguous sequence 1..N.
	ErrBlockNumberMismatch = errors.New("secs1: block number mismatch")
	// ErrEBitPlacement is returned by assembleBlocks when the E-bit (last-block flag) is not set
	// exactly on the final block.
	ErrEBitPlacement = errors.New("secs1: E-bit not set exactly on the last block")
	// ErrHeaderMismatch is returned by assembleBlocks when the block-invariant header fields
	// differ across blocks of the same message.
	ErrHeaderMismatch = errors.New("secs1: block header fields differ across blocks")
	// ErrMessageTooLarge is returned by splitBody when the body exceeds the maximum SECS-I message
	// size (244 * 32767 bytes).
	ErrMessageTooLarge = errors.New("secs1: message body exceeds maximum SECS-I size")
	// ErrInvalidHeader is returned by splitBody when deviceID > 0x7FFF or stream > 0x7F.
	ErrInvalidHeader = errors.New("secs1: invalid SECS-I header field")
	// ErrT1Timeout is returned by the line transfer when the T1 inter-character timeout elapses
	// between bytes of a block (SEMI E4 §7.3.1).
	ErrT1Timeout = errors.New("secs1: T1 inter-character timeout")
	// ErrT2Timeout is returned by the line transfer when the T2 protocol timeout elapses while
	// waiting for a handshake reply such as EOT, the length byte, or ACK (SEMI E4 §7.8).
	ErrT2Timeout = errors.New("secs1: T2 protocol timeout")
	// ErrSendFailed is returned when a block's RTY retry limit is exhausted without an ACK
	// (SEMI E4 §7.8.2). The connection treats it as a line failure: it tears the link down and reconnects.
	ErrSendFailed = errors.New("secs1: block send failed, retries exhausted")
)
