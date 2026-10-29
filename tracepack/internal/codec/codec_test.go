package codec_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeNoneRoundTrip(t *testing.T) {
	src := []byte("the tracepack format specification section 2 names none as codec 0")

	encoded, err := codec.Encode(codec.None, nil, src)
	require.NoError(t, err)
	require.Equal(t, src, encoded)

	decoded, err := codec.Decode(codec.None, nil, encoded, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
}

func TestDecodeNoneWrongLength(t *testing.T) {
	src := []byte("twelve bytes")

	_, err := codec.Decode(codec.None, nil, src, len(src)-1)
	require.ErrorIs(t, err, codec.ErrLengthMismatch)

	_, err = codec.Decode(codec.None, nil, src, len(src)+1)
	require.ErrorIs(t, err, codec.ErrLengthMismatch)
}

func TestEncodeDecodeNoneEmptyBody(t *testing.T) {
	encoded, err := codec.Encode(codec.None, nil, []byte{})
	require.NoError(t, err)
	require.Empty(t, encoded)

	decoded, err := codec.Decode(codec.None, nil, encoded, 0)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

func TestEncodeUnknownCodec(t *testing.T) {
	_, err := codec.Encode(7, nil, []byte("payload"))
	require.ErrorIs(t, err, codec.ErrUnknownCodec)
	require.True(t, errors.Is(err, codec.ErrUnknownCodec))
}

func TestDecodeUnknownCodec(t *testing.T) {
	_, err := codec.Decode(7, nil, []byte("payload"), 7)
	require.ErrorIs(t, err, codec.ErrUnknownCodec)
}

func TestDecodeRejectsNegativeUncompressedLenNone(t *testing.T) {
	require.NotPanics(t, func() {
		_, err := codec.Decode(codec.None, nil, []byte("x"), -1)
		require.ErrorIs(t, err, codec.ErrLengthMismatch)
	})
}

func TestDecodeAppendsToDstCapacity(t *testing.T) {
	src := []byte("reused buffer contents")
	dst := make([]byte, 0, 256)

	decoded, err := codec.Decode(codec.None, dst, src, len(src))
	require.NoError(t, err)
	require.Equal(t, src, decoded)
	// The append-style contract reuses dst's backing array when it has enough capacity,
	// so decoded and dst must share the same array.
	require.Same(t, &dst[:1][0], &decoded[:1][0])
}

func TestEncodeLevelUnknown(t *testing.T) {
	for _, level := range []int{-1, 3, 255} {
		_, err := codec.EncodeLevel(codec.Zstd, level, nil, []byte("payload"))
		require.ErrorIs(t, err, codec.ErrUnknownLevel, "level %d", level)
	}
	// None ignores the level.
	enc, err := codec.EncodeLevel(codec.None, 9, nil, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, []byte("payload"), enc)
}

// Each level's encoder is built once on its first use, from whichever goroutines use it first at once,
// and encodes the same bytes from every goroutine; under -race, a racy build is reported.
func TestEncodeLevelConcurrentFirstUse(t *testing.T) {
	body := bytes.Repeat([]byte("the zstd levels of the tracepack writers; "), 4096)
	for _, level := range []int{codec.LevelFastest, codec.LevelBest, codec.LevelDefault} {
		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		out := make([][]byte, n)
		errs := make([]error, n)
		for g := range n {
			wg.Go(func() {
				<-start
				out[g], errs[g] = codec.EncodeLevel(codec.Zstd, level, nil, body)
			})
		}
		close(start)
		wg.Wait()
		for g := range n {
			require.NoError(t, errs[g])
			require.Equal(t, out[0], out[g], "level %d, goroutine %d", level, g)
		}
		decoded, err := codec.Decode(codec.Zstd, nil, out[0], len(body))
		require.NoError(t, err)
		require.Equal(t, body, decoded)
	}
}
