package codec_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/stretchr/testify/require"
)

// zstdFrameMagic is the four-byte signature every RFC 8878 frame starts with,
// independent of this package's own encoder.
var zstdFrameMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// errCorrupted flags a concurrent decode whose result did not match the source,
// distinct from a decode error.
var errCorrupted = errors.New("codec_test: decoded result does not match source")

func TestEncodeDecodeZstdRoundTripCompressible(t *testing.T) {
	src := []byte(strings.Repeat("the tracepack format specification section 2 ", 200))

	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))
	require.Less(t, len(encoded), len(src), "a repetitive buffer must compress smaller")

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

func TestEncodeDecodeZstdRoundTripRandom(t *testing.T) {
	src := make([]byte, 4096)
	_, err := rand.Read(src)
	require.NoError(t, err)

	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

func TestEncodeDecodeZstdEmptyBody(t *testing.T) {
	encoded, err := codec.Encode(codec.Zstd, nil, []byte{})
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(encoded, zstdFrameMagic))

	decoded, err := codec.Decode(codec.Zstd, nil, encoded, 0)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

func TestDecodeZstdWrongUncompressedLenTooSmall(t *testing.T) {
	src := []byte(strings.Repeat("payload ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	_, err = codec.Decode(codec.Zstd, nil, encoded, len(src)-1)
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdWrongUncompressedLenTooLarge(t *testing.T) {
	src := []byte(strings.Repeat("payload ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	_, err = codec.Decode(codec.Zstd, nil, encoded, len(src)+1)
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

func TestDecodeZstdConcatenatedFrames(t *testing.T) {
	first := []byte(strings.Repeat("first frame ", 32))
	second := []byte(strings.Repeat("second frame ", 32))

	encodedFirst, err := codec.Encode(codec.Zstd, nil, first)
	require.NoError(t, err)
	encodedSecond, err := codec.Encode(codec.Zstd, nil, second)
	require.NoError(t, err)

	concatenated := append(append([]byte{}, encodedFirst...), encodedSecond...)

	_, err = codec.Decode(codec.Zstd, nil, concatenated, len(first))
	require.ErrorIs(t, err, codec.ErrTrailingBytes)
}

func TestDecodeZstdTruncatedFrame(t *testing.T) {
	src := []byte(strings.Repeat("truncate me ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 4)

	truncated := encoded[:len(encoded)-4]
	_, err = codec.Decode(codec.Zstd, nil, truncated, len(src))
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

func TestDecodeRejectsNegativeUncompressedLenZstd(t *testing.T) {
	src := []byte("payload")
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err := codec.Decode(codec.Zstd, nil, encoded, -1)
		require.ErrorIs(t, err, codec.ErrLengthMismatch)
	})
}

func TestDecodeZstdRejectsEmptySource(t *testing.T) {
	_, err := codec.Decode(codec.Zstd, nil, []byte{}, 0)
	require.ErrorIs(t, err, codec.ErrIncompleteStream)
}

// TestDecodeZstdBoundedMemory proves the streaming-decoder design's reason to exist.
// Decoding a small requested length out of a frame
// whose real content is huge must not allocate anywhere near that huge size.
// A DecodeAll-based implementation would materialize the whole decoded frame before this package could reject it.
// That would defeat the bound uncompressedLen is supposed to give the caller.
func TestDecodeZstdBoundedMemory(t *testing.T) {
	const bombUncompressedLen = 64 << 20 // 64 MiB of zeros

	src := make([]byte, bombUncompressedLen)
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)
	require.Less(t, len(encoded), 64<<10, "a run of zeros must compress to well under 64 KiB")

	const requestedLen = 4 << 10 // far less than the bomb's real decoded length

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, err = codec.Decode(codec.Zstd, nil, encoded, requestedLen)
	require.ErrorIs(t, err, codec.ErrTrailingBytes)

	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("decoding a %d-byte bomb to a %d-byte request allocated %d bytes", bombUncompressedLen, requestedLen, delta)
	require.Less(t, delta, uint64(bombUncompressedLen)/2, "decode must not allocate close to the bomb's full decoded size")
}

func TestDecodeZstdConcurrent(t *testing.T) {
	src := []byte(strings.Repeat("concurrent decode from a pooled decoder ", 128))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	const goroutines = 32

	results := make(chan error, goroutines)
	for range goroutines {
		go func() {
			decoded, decErr := codec.Decode(codec.Zstd, nil, encoded, len(src))
			if decErr != nil {
				results <- decErr

				return
			}
			if !bytes.Equal(decoded, src) {
				results <- errCorrupted
			} else {
				results <- nil
			}
		}()
	}

	for range goroutines {
		require.NoError(t, <-results)
	}
}

// TestDecodeZstdCLIProducedFrame decodes a frame built by the system zstd binary.
// That binary is an implementation independent of this package's own encoder.
// It is skipped when that binary is not on PATH.
func TestDecodeZstdCLIProducedFrame(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	src := []byte(strings.Repeat("independent cross-check with the system zstd binary ", 50))

	cmd := exec.Command("zstd", "-q", "-c")
	cmd.Stdin = bytes.NewReader(src)
	var encoded bytes.Buffer
	cmd.Stdout = &encoded
	require.NoError(t, cmd.Run())

	decoded, err := codec.Decode(codec.Zstd, nil, encoded.Bytes(), len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

// TestEncodeZstdDecodableByCLI feeds this package's Encode output to the system zstd binary.
// That checks that an implementation independent of this package's own decoder accepts it.
// It is skipped when that binary is not on PATH.
func TestEncodeZstdDecodableByCLI(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd CLI not found on PATH; skipping independent cross-check")
	}

	src := []byte(strings.Repeat("independent cross-check with the system zstd binary ", 50))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	cmd := exec.Command("zstd", "-q", "-d", "-c")
	cmd.Stdin = bytes.NewReader(encoded)
	var decoded bytes.Buffer
	cmd.Stdout = &decoded
	require.NoError(t, cmd.Run())
	require.Equal(t, src, decoded.Bytes())
}
