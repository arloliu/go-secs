package codec_test

import (
	"errors"
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
