package codec

import (
	"errors"
	"fmt"
)

// None and Zstd are the codec registry of the tracepack format specification §2.
// A block envelope or trailer names one of these values;
// any other value is unknown and reported through ErrUnknownCodec.
const (
	None uint8 = 0
	Zstd uint8 = 1
)

// Known reports whether id is a codec of the registry of the tracepack format specification §2: None or Zstd.
func Known(id uint8) bool {
	return id == None || id == Zstd
}

// ErrUnknownCodec reports a codec value outside the registry of the tracepack format specification §2.
// Adding a codec value is a major format version change, so a reader never guesses at an unknown value's meaning.
var ErrUnknownCodec = errors.New("codec: unknown codec")

// ErrIncompleteStream reports a codec stream that ends before producing the requested number of decoded bytes.
// It fires whether the stream was truncated or the caller asked for more bytes than the stream holds.
var ErrIncompleteStream = errors.New("codec: incomplete stream")

// ErrTrailingBytes reports bytes found after the point where a codec stream was expected to end.
// A concatenated second frame is the most common cause; the tracepack format specification §2 allows only one.
var ErrTrailingBytes = errors.New("codec: trailing bytes after stream")

// ErrNotZstdFrame reports a Zstd source that is not a well-formed RFC 8878 §3.1.1 Zstandard frame:
// its first four bytes are not the Zstandard frame magic, or a block header names the reserved block type.
// A skippable frame is reported through this sentinel too,
// because the tracepack format specification §2 allows exactly one Zstandard frame and nothing else.
// A source that ends before its frame does is reported through ErrIncompleteStream instead.
var ErrNotZstdFrame = errors.New("codec: not a zstd frame")

// ErrDictionary reports a Zstd frame whose header sets the Dictionary_ID_flag of RFC 8878 §3.1.1.1.1.
// The tracepack format specification §2 forbids dictionaries,
// so a frame is rejected even when the Dictionary_ID it carries is zero.
var ErrDictionary = errors.New("codec: zstd frame names a dictionary")

// ErrWindowTooLarge reports a Zstd frame whose header declares a window larger than this package decodes.
// A window is history the decoder must keep in memory,
// so the limit is checked from the frame header before any block is decoded.
var ErrWindowTooLarge = errors.New("codec: zstd window too large")

// ErrLengthMismatch reports a decoded length that does not match the uncompressed length the caller supplied.
// A negative uncompressed length is always invalid and is also reported through this sentinel.
var ErrLengthMismatch = errors.New("codec: length mismatch")

// Encode compresses src with the named codec and appends the result to dst.
//
// The returned slice is dst[:0] grown as needed;
// callers that reuse dst across calls avoid repeated allocation.
//
// Parameters:
//   - codecID: None or Zstd.
//   - dst: destination buffer; may be nil.
//   - src: bytes to encode.
//
// Returns:
//   - []byte: the encoded bytes, appended to dst.
//   - error: ErrUnknownCodec for a codecID outside the registry.
func Encode(codecID uint8, dst, src []byte) ([]byte, error) {
	switch codecID {
	case None:
		return append(dst[:0], src...), nil
	case Zstd:
		return encodeZstd(dst, src), nil
	default:
		return dst[:0], fmt.Errorf("codec: encode: value %d: %w", codecID, ErrUnknownCodec)
	}
}

// Decode decompresses src with the named codec and appends the result to dst.
//
// The returned slice is dst[:0] grown as needed;
// callers that reuse dst across calls avoid repeated allocation.
// For None, src must be exactly uncompressedLen bytes.
// For Zstd, src must hold exactly one RFC 8878 frame with no dictionary whose decoded length is exactly uncompressedLen,
// and the frame's declared window must not exceed 64 MiB;
// the frame header is checked before any block is decoded.
//
// Parameters:
//   - codecID: None or Zstd.
//   - dst: destination buffer; may be nil.
//   - src: bytes to decode.
//   - uncompressedLen: the expected decoded length; never negative.
//
// Returns:
//   - []byte: the decoded bytes, appended to dst.
//   - error: ErrUnknownCodec for a codecID outside the registry;
//     ErrLengthMismatch, ErrIncompleteStream or ErrTrailingBytes for a decoded length that disagrees with uncompressedLen;
//     for Zstd, also ErrIncompleteStream when src ends before its frame does, even inside the frame header,
//     ErrTrailingBytes for any byte after the frame, ErrNotZstdFrame for a skippable or otherwise malformed frame,
//     ErrDictionary for a frame naming a dictionary, and ErrWindowTooLarge for a window above 64 MiB
//     or above both 8 MiB and uncompressedLen.
func Decode(codecID uint8, dst, src []byte, uncompressedLen int) ([]byte, error) {
	switch codecID {
	case None:
		return decodeNone(dst, src, uncompressedLen)
	case Zstd:
		return decodeZstd(dst, src, uncompressedLen)
	default:
		return dst[:0], fmt.Errorf("codec: decode: value %d: %w", codecID, ErrUnknownCodec)
	}
}

// decodeNone implements Decode for the None codec: src must be exactly uncompressedLen bytes, copied verbatim (§2).
func decodeNone(dst, src []byte, uncompressedLen int) ([]byte, error) {
	if err := CheckNone(src, uncompressedLen); err != nil {
		return dst[:0], err
	}

	return append(dst[:0], src...), nil
}

// CheckNone checks src as a None body of uncompressedLen decoded bytes:
// src must be exactly uncompressedLen bytes, since None stores the decoded body verbatim (§2).
// It is the check Decode runs for None,
// for a caller that uses src in place instead of copying it.
//
// Returns:
//   - error: nil, or ErrLengthMismatch for a src of another length.
func CheckNone(src []byte, uncompressedLen int) error {
	if len(src) != uncompressedLen {
		return fmt.Errorf(
			"codec: none: source length %d does not match uncompressed length %d: %w",
			len(src), uncompressedLen, ErrLengthMismatch,
		)
	}

	return nil
}
