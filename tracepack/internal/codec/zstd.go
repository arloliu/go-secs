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
const maxWindow = 64 << 20

// windowFloor is the window every frame may declare regardless of its decoded length: 8 MiB.
// A conforming streaming encoder that does not know its content size declares its full window on a small body,
// and 8 MiB covers the windows of zstd compression levels up to 19.
// Above the floor, a frame may declare a window no larger than the decoded length the caller expects,
// so the decoder's history stays bounded by that length rather than by the frame's own claim.
const windowFloor = 8 << 20

// sharedEncoder is safe for concurrent use by multiple goroutines (EncodeAll's own contract in klauspost/compress/zstd).
// Encode shares this one instance instead of allocating an encoder per call.
// A concurrency of 1 keeps a single call's work on one goroutine, since the caller already parallelizes across blocks.
var sharedEncoder = newSharedEncoder()

// pooledDecoder is a streaming zstd decoder and the reader of the source it decodes, pooled together.
type pooledDecoder struct {
	*zstd.Decoder
	src bytes.Reader
}

// decoderPool holds *pooledDecoder values between calls.
// A streaming decoder is stateful and cannot be shared across concurrent Reset calls,
// so each Decode borrows one, uses it against a single frame, and returns it.
// A concurrency of 1 makes the decoder decode synchronously in the calling goroutine,
// starting no goroutine per Reset, since the caller already parallelizes across blocks.
var decoderPool = sync.Pool{
	New: func() any {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(maxWindow), zstd.WithDecoderConcurrency(1))
		if err != nil {
			panic("codec: failed to create zstd decoder: " + err.Error())
		}

		return &pooledDecoder{Decoder: dec}
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
// openZstd checks the frame before any decoding,
// so memory use is bounded by uncompressedLen plus the decoder window openZstd allows,
// instead of by whatever length a malicious or miscounted frame claims.
// After reading uncompressedLen bytes, one more byte is read from the stream:
// a clean io.EOF confirms the frame held exactly that many bytes and nothing more;
// anything else means the frame decoded to more than uncompressedLen bytes.
func decodeZstd(dst, src []byte, uncompressedLen int) ([]byte, error) {
	dec, err := openZstd(src, uncompressedLen)
	if err != nil {
		return dst[:0], err
	}
	defer releaseDecoder(dec)

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

// openZstd checks src as the single Zstandard frame of a body of uncompressedLen decoded bytes (§2)
// and returns a pooled decoder positioned at its start; the caller returns it with releaseDecoder.
//
// Before any decoding, walkZstdFrame measures the frame from its headers:
// it rejects a skippable or foreign frame, a dictionary, and a window above maxWindow;
// a window above windowFloor is also rejected when it exceeds uncompressedLen,
// and any byte after the frame is reported through ErrTrailingBytes,
// so a second frame is rejected even when it would decode to no bytes at all.
// Decoding then uses a streaming decoder rather than DecodeAll,
// so memory use is bounded by what the caller reads instead of by whatever length a malicious or miscounted frame claims.
func openZstd(src []byte, uncompressedLen int) (*pooledDecoder, error) {
	if uncompressedLen < 0 {
		return nil, fmt.Errorf(
			"codec: zstd: negative uncompressed length %d: %w",
			uncompressedLen, ErrLengthMismatch,
		)
	}
	if len(src) == 0 {
		// §2 requires exactly one frame,
		// and even a frame holding no content is a nonempty byte sequence,
		// so an empty src is never a valid stream, whatever uncompressedLen is.
		return nil, fmt.Errorf("codec: zstd: empty source: %w", ErrIncompleteStream)
	}

	frameLen, window, err := walkZstdFrame(src)
	if err != nil {
		return nil, fmt.Errorf("codec: zstd: %w", err)
	}
	if budget := max(uint64(uncompressedLen), windowFloor); window > budget {
		return nil, fmt.Errorf(
			"codec: zstd: frame window %d exceeds the %d-byte budget for a %d-byte body: %w",
			window, budget, uncompressedLen, ErrWindowTooLarge,
		)
	}
	if frameLen != len(src) {
		return nil, fmt.Errorf(
			"codec: zstd: %d bytes after a %d-byte frame: %w",
			len(src)-frameLen, frameLen, ErrTrailingBytes,
		)
	}

	dec, _ := decoderPool.Get().(*pooledDecoder)
	dec.src.Reset(src)
	if err := dec.Reset(&dec.src); err != nil {
		releaseDecoder(dec)
		return nil, fmt.Errorf("codec: zstd: reset decoder: %w", err)
	}

	return dec, nil
}

// releaseDecoder detaches dec from its source and returns it to the pool.
func releaseDecoder(dec *pooledDecoder) {
	_ = dec.Reset(nil)
	dec.src.Reset(nil)
	decoderPool.Put(dec)
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
