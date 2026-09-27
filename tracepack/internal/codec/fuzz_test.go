// Fuzz targets for Decode, one per codec.
//
// Both take the uncompressed length as a fuzz input, because Decode sizes its output from it before reading src.
// The harness caps that length, so the fuzzer never asks for gigabytes:
// see capUncompressedLen.
// Each target asserts that Decode never panics,
// that a successful decode has exactly the requested length and re-encodes with Encode to a stream that decodes equal,
// and that the returned slice's capacity stays within a small multiple of the requested length.
package codec_test

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// maxFuzzUncompressedLen is the largest uncompressed length a fuzz iteration requests: 4 MiB.
const maxFuzzUncompressedLen = 4 << 20

// capUncompressedLen folds a fuzzed uncompressed length above min(4 MiB, 64×srcLen+64) back into [0, that limit],
// so a mutation of a large value still explores distinct lengths instead of collapsing onto the cap.
// A negative length is returned unchanged: Decode must reject it itself.
func capUncompressedLen(n, srcLen int) int {
	limit := min(maxFuzzUncompressedLen, 64*srcLen+64)
	if n > limit {
		return n % (limit + 1)
	}

	return n
}

// requireSameBytes asserts that got holds the same bytes as want, treating nil and empty as equal:
// a decoded body of length zero may come back as either.
func requireSameBytes(t *testing.T, want, got []byte, msg string) {
	t.Helper()

	require.Truef(t, bytes.Equal(want, got), "%s: %d bytes %x, want %d bytes %x", msg, len(got), got, len(want), want)
}

// fuzzPayloads returns the plaintexts the seeds are built from.
// Each compresses by less than 64×, so its true length survives capUncompressedLen.
func fuzzPayloads() [][]byte {
	noise := make([]byte, 1024)
	x := uint32(2463534242)
	for i := range noise {
		// xorshift32: deterministic, incompressible-looking bytes.
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		noise[i] = byte(x)
	}

	return [][]byte{
		{},
		{0x00},
		[]byte("the tracepack format specification section 2 names zstd as codec 1"),
		[]byte(strings.Repeat("repetitive block body ", 40)),
		noise,
	}
}

// addLengthSeeds adds src paired with the true length n and the lengths around and beyond it.
func addLengthSeeds(f *testing.F, src []byte, n int) {
	f.Helper()

	for _, l := range []int{n, n - 1, n + 1, 0, -1, math.MinInt, math.MaxInt} {
		f.Add(src, l)
	}
}

// FuzzDecodeNone fuzzes Decode with the None codec, with a fuzzed uncompressed length capped by capUncompressedLen.
//
// It succeeds exactly when the length equals len(src), and returns a copy of src that does not alias it.
// Allocation bound: cap(result) ≤ 2×len(src)+64, since the copy is rounded up to an allocator size class.
func FuzzDecodeNone(f *testing.F) {
	for _, p := range fuzzPayloads() {
		addLengthSeeds(f, p, len(p))
	}

	f.Fuzz(func(t *testing.T, src []byte, uncompressedLen int) {
		uncompressedLen = capUncompressedLen(uncompressedLen, len(src))

		out, err := codec.Decode(codec.None, nil, src, uncompressedLen)
		if err != nil {
			require.ErrorIs(t, err, codec.ErrLengthMismatch)
			require.NotEqual(t, len(src), uncompressedLen)
			require.Empty(t, out)

			return
		}

		requireSameBytes(t, src, out, "the None result must equal src")
		require.LessOrEqual(t, cap(out), 2*len(src)+64, "cap(result) must stay within 2×len(src)+64")
		if len(out) > 0 {
			require.NotSame(t, &src[0], &out[0], "the result must not alias src")
		}

		enc, err := codec.Encode(codec.None, nil, out)
		require.NoError(t, err)
		got, err := codec.Decode(codec.None, nil, enc, len(out))
		require.NoError(t, err)
		requireSameBytes(t, out, got, "the re-encoded result must decode equal")
	})
}

// FuzzDecodeZstd fuzzes Decode with the Zstd codec, with a fuzzed uncompressed length capped by capUncompressedLen.
// The seeds are frames produced by Encode, whole and damaged.
//
// On success the result has exactly the requested length,
// matches what an independent decoder of the same library makes of src,
// and re-encodes with Encode to a frame that decodes equal.
// Allocation bound: cap(result) ≤ 2×uncompressedLen+64,
// which the harness cap keeps within 2×(64×len(src)+64)+64 = 128×len(src)+192.
// On error the result is empty, with the same capacity bound.
func FuzzDecodeZstd(f *testing.F) {
	payloads := fuzzPayloads()
	frames := make([][]byte, 0, len(payloads))
	for _, p := range payloads {
		frame, err := codec.Encode(codec.Zstd, nil, p)
		require.NoError(f, err)
		require.LessOrEqual(f, len(p), capUncompressedLen(len(p), len(frame)), "a seed payload compresses by more than the harness cap allows")
		frames = append(frames, frame)
		addLengthSeeds(f, frame, len(p))
	}

	short := frames[2]
	shortLen := len(payloads[2])
	f.Add(short[:len(short)-1], shortLen)
	f.Add(short[:4], shortLen)
	f.Add([]byte{}, 0)

	flipped := append([]byte(nil), short...)
	flipped[len(flipped)/2] ^= 0xFF
	f.Add(flipped, shortLen)

	concatenated := append(append([]byte(nil), short...), short...)
	f.Add(concatenated, shortLen)
	f.Add(concatenated, 2*shortLen)

	reference, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	require.NoError(f, err)
	f.Cleanup(reference.Close)

	f.Fuzz(func(t *testing.T, src []byte, uncompressedLen int) {
		uncompressedLen = capUncompressedLen(uncompressedLen, len(src))
		capBound := 2*max(uncompressedLen, 0) + 64

		out, err := codec.Decode(codec.Zstd, nil, src, uncompressedLen)
		require.LessOrEqual(t, cap(out), capBound, "cap(result) must stay within 2×uncompressedLen+64")
		if err != nil {
			require.Empty(t, out)

			return
		}

		require.Len(t, out, uncompressedLen)

		want, err := reference.DecodeAll(src, nil)
		require.NoError(t, err, "an independent decoder must accept a frame Decode accepts")
		requireSameBytes(t, want, out, "the result must match the independent decoder")

		enc, err := codec.Encode(codec.Zstd, nil, out)
		require.NoError(t, err)
		got, err := codec.Decode(codec.Zstd, nil, enc, len(out))
		require.NoError(t, err)
		requireSameBytes(t, out, got, "the re-encoded result must decode equal")
	})
}
