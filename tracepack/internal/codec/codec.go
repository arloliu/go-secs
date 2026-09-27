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

// ErrUnknownCodec reports a codec value outside the registry of the tracepack format specification §2.
// Adding a codec value is a major format version change, so a reader never guesses at an unknown value's meaning.
var ErrUnknownCodec = errors.New("codec: unknown codec")

// ErrIncompleteStream reports a codec stream that ends before producing the requested number of decoded bytes.
// It fires whether the stream was truncated or the caller asked for more bytes than the stream holds.
var ErrIncompleteStream = errors.New("codec: incomplete stream")

// ErrTrailingBytes reports bytes found after the point where a codec stream was expected to end.
// A concatenated second frame is the most common cause; the tracepack format specification §2 allows only one.
var ErrTrailingBytes = errors.New("codec: trailing bytes after stream")

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
// For Zstd, src must hold exactly one RFC 8878 frame with no dictionary whose decoded length is exactly uncompressedLen.
//
// Parameters:
//   - codecID: None or Zstd.
//   - dst: destination buffer; may be nil.
//   - src: bytes to decode.
//   - uncompressedLen: the expected decoded length; never negative.
//
// Returns:
//   - []byte: the decoded bytes, appended to dst.
//   - error: ErrUnknownCodec for a codecID outside the registry,
//     ErrLengthMismatch, ErrIncompleteStream or ErrTrailingBytes for a
//     decoded length that disagrees with uncompressedLen.
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
	if len(src) != uncompressedLen {
		return dst[:0], fmt.Errorf(
			"codec: none: source length %d does not match uncompressed length %d: %w",
			len(src), uncompressedLen, ErrLengthMismatch,
		)
	}

	return append(dst[:0], src...), nil
}
