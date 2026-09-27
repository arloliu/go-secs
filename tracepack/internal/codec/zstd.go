package codec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// maxWindow is the largest Zstandard window, in bytes, that Decode accepts: 64 MiB.
// A window is history the decoder keeps in memory, and a frame declares its own,
// so a hostile frame could otherwise make the decoder allocate far beyond the decoded length the caller expects.
// 64 MiB covers the windows of zstd compression levels up to 21, well above the 4 MiB blocks the format defaults to.
// It is deliberately not tied to the uncompressed length:
// a conforming streaming encoder may declare its full window, for example 8 MiB, on a small body.
const maxWindow = 64 << 20

// sharedEncoder is safe for concurrent use by multiple goroutines (EncodeAll's own contract in klauspost/compress/zstd).
// Encode shares this one instance instead of allocating an encoder per call.
// A concurrency of 1 keeps a single call's work on one goroutine, since the caller already parallelizes across blocks.
var sharedEncoder = newSharedEncoder()

// decoderPool holds *zstd.Decoder values between calls.
// A streaming decoder is stateful and cannot be shared across concurrent Reset calls,
// so each Decode borrows one, uses it against a single frame, and returns it.
var decoderPool = sync.Pool{
	New: func() any {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(maxWindow))
		if err != nil {
			panic("codec: failed to create zstd decoder: " + err.Error())
		}

		return dec
	},
}

// encodeZstd implements Encode for the Zstd codec:
// it appends one RFC 8878 frame holding src, with no dictionary, to dst (§2).
func encodeZstd(dst, src []byte) []byte {
	return sharedEncoder.EncodeAll(src, dst[:0])
}

// decodeZstd implements Decode for the Zstd codec (§2):
// src must hold exactly one RFC 8878 frame with no dictionary, decoding to exactly uncompressedLen bytes.
//
// Before any decoding, walkZstdFrame measures the frame from its headers:
// it rejects a skippable or foreign frame, a dictionary, and a window above maxWindow,
// and any byte after the frame is reported through ErrTrailingBytes,
// so a second frame is rejected even when it would decode to no bytes at all.
//
// It then decodes with a pooled streaming decoder rather than DecodeAll,
// so memory use is bounded by uncompressedLen instead of by whatever length a malicious or miscounted frame claims.
// After reading uncompressedLen bytes, one more byte is read from the stream:
// a clean io.EOF confirms the frame held exactly that many bytes and nothing more;
// anything else means the frame decoded to more than uncompressedLen bytes.
func decodeZstd(dst, src []byte, uncompressedLen int) ([]byte, error) {
	if uncompressedLen < 0 {
		return dst[:0], fmt.Errorf(
			"codec: zstd: negative uncompressed length %d: %w",
			uncompressedLen, ErrLengthMismatch,
		)
	}
	if len(src) == 0 {
		// §2 requires exactly one frame,
		// and even a frame holding no content is a nonempty byte sequence,
		// so an empty src is never a valid stream, whatever uncompressedLen is.
		return dst[:0], fmt.Errorf("codec: zstd: empty source: %w", ErrIncompleteStream)
	}

	frameLen, _, err := walkZstdFrame(src)
	if err != nil {
		return dst[:0], fmt.Errorf("codec: zstd: %w", err)
	}
	if frameLen != len(src) {
		return dst[:0], fmt.Errorf(
			"codec: zstd: %d bytes after a %d-byte frame: %w",
			len(src)-frameLen, frameLen, ErrTrailingBytes,
		)
	}

	dec, _ := decoderPool.Get().(*zstd.Decoder)
	defer func() {
		_ = dec.Reset(nil)
		decoderPool.Put(dec)
	}()

	if err := dec.Reset(bytes.NewReader(src)); err != nil {
		return dst[:0], fmt.Errorf("codec: zstd: reset decoder: %w", err)
	}

	dst = slices.Grow(dst[:0], uncompressedLen)[:uncompressedLen]
	if _, err := io.ReadFull(dec, dst); err != nil {
		return dst[:0], fmt.Errorf(
			"codec: zstd: decode to %d bytes: %w: %w",
			uncompressedLen, ErrIncompleteStream, err,
		)
	}

	var probe [1]byte
	switch n, err := dec.Read(probe[:]); {
	case n > 0:
		return dst[:0], fmt.Errorf(
			"codec: zstd: decoded past %d bytes: %w",
			uncompressedLen, ErrTrailingBytes,
		)
	case errors.Is(err, io.EOF):
		return dst, nil
	default:
		return dst[:0], fmt.Errorf(
			"codec: zstd: reading past %d bytes: %w: %w",
			uncompressedLen, ErrTrailingBytes, err,
		)
	}
}

// newSharedEncoder builds the package's one zstd encoder.
// It panics only if the fixed options below are themselves invalid,
// which would be a bug in this file, not a runtime condition callers can hit.
func newSharedEncoder() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic("codec: failed to create zstd encoder: " + err.Error())
	}

	return enc
}
