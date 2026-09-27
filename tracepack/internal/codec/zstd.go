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

// sharedEncoder is safe for concurrent use by multiple goroutines (EncodeAll's own contract in klauspost/compress/zstd).
// Encode shares this one instance instead of allocating an encoder per call.
// A concurrency of 1 keeps a single call's work on one goroutine, since the caller already parallelizes across blocks.
var sharedEncoder = newSharedEncoder()

// decoderPool holds *zstd.Decoder values between calls.
// A streaming decoder is stateful and cannot be shared across concurrent Reset calls,
// so each Decode borrows one, uses it against a single frame, and returns it.
var decoderPool = sync.Pool{
	New: func() any {
		dec, err := zstd.NewReader(nil)
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
// It decodes with a pooled streaming decoder rather than DecodeAll,
// so memory use is bounded by uncompressedLen instead of by whatever length a malicious or miscounted frame claims.
// After reading uncompressedLen bytes, one more byte is read from the stream:
// a clean io.EOF confirms the frame held exactly that many bytes and nothing more;
// anything else means the frame decoded to more than uncompressedLen bytes,
// or another frame followed it (§2 allows exactly one).
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
