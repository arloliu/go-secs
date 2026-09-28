package codec_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/stretchr/testify/require"
)

// compressibleBody returns n bytes that zstd compresses into several blocks of the compressed type:
// lines carrying a counter, so no two 128 KiB blocks are identical.
func compressibleBody(n int) []byte {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		sb.WriteString("record header section line ")
		sb.WriteString(strings.Repeat("x", i%17))
		sb.WriteByte('\n')
	}

	return []byte(sb.String()[:n])
}

func TestDecodePrefixNone(t *testing.T) {
	src := []byte("header section, then payload section")

	tests := []struct {
		name      string
		prefixLen int
	}{
		{name: "empty prefix", prefixLen: 0},
		{name: "inner prefix", prefixLen: 14},
		{name: "whole body", prefixLen: len(src)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codec.DecodePrefix(codec.None, nil, src, tt.prefixLen, len(src))
			require.NoError(t, err)
			requireSameBytes(t, src[:tt.prefixLen], got, "prefix")
		})
	}
}

func TestDecodePrefixRejectsBadLengths(t *testing.T) {
	src := []byte("twelve bytes")
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	tests := []struct {
		name            string
		codecID         uint8
		src             []byte
		prefixLen       int
		uncompressedLen int
	}{
		{name: "none source shorter than the body", codecID: codec.None, src: src, prefixLen: 4, uncompressedLen: len(src) + 1},
		{name: "none source longer than the body", codecID: codec.None, src: src, prefixLen: 4, uncompressedLen: len(src) - 1},
		{name: "none prefix past the body", codecID: codec.None, src: src, prefixLen: len(src) + 1, uncompressedLen: len(src)},
		{name: "none negative prefix", codecID: codec.None, src: src, prefixLen: -1, uncompressedLen: len(src)},
		{name: "zstd prefix past the body", codecID: codec.Zstd, src: encoded, prefixLen: len(src) + 1, uncompressedLen: len(src)},
		{name: "zstd negative prefix", codecID: codec.Zstd, src: encoded, prefixLen: -1, uncompressedLen: len(src)},
		{name: "zstd negative body", codecID: codec.Zstd, src: encoded, prefixLen: 0, uncompressedLen: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				out, err := codec.DecodePrefix(tt.codecID, nil, tt.src, tt.prefixLen, tt.uncompressedLen)
				require.ErrorIs(t, err, codec.ErrLengthMismatch)
				require.Empty(t, out)
			})
		})
	}
}

func TestDecodePrefixUnknownCodec(t *testing.T) {
	_, err := codec.DecodePrefix(7, nil, []byte("payload"), 3, 7)
	require.ErrorIs(t, err, codec.ErrUnknownCodec)
}

func TestDecodePrefixZstd(t *testing.T) {
	src := compressibleBody(300 << 10)
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	for _, n := range []int{0, 1, 56, 4096, 200 << 10, len(src)} {
		got, err := codec.DecodePrefix(codec.Zstd, nil, encoded, n, len(src))
		require.NoError(t, err, "prefix %d", n)
		requireSameBytes(t, src[:n], got, "prefix")
	}
}

// TestDecodePrefixZstdIgnoresStreamAfterPrefix is the header-only read of the tracepack format specification §6:
// the stream after the prefix is left unchecked,
// so a frame whose last block is damaged still yields a prefix that lies in its earlier blocks,
// while Decode, which needs the whole stream, rejects it.
func TestDecodePrefixZstdIgnoresStreamAfterPrefix(t *testing.T) {
	src := compressibleBody(300 << 10)
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	// The byte before the 4-byte content checksum is the last byte of the last block's content:
	// block headers stay intact, so the frame walk still measures the frame.
	damaged := append([]byte(nil), encoded...)
	damaged[len(damaged)-5] ^= 0xFF

	_, err = codec.Decode(codec.Zstd, nil, damaged, len(src))
	require.ErrorIs(t, err, codec.ErrIncompleteStream, "a full decode must reject the damaged stream")

	got, err := codec.DecodePrefix(codec.Zstd, nil, damaged, 4096, len(src))
	require.NoError(t, err)
	requireSameBytes(t, src[:4096], got, "prefix before the damage")
}

// TestDecodePrefixZstdChecksFrame keeps the frame checks of Decode:
// they bound the decoder's memory and reject what the tracepack format specification §2 forbids,
// whatever length is requested.
func TestDecodePrefixZstdChecksFrame(t *testing.T) {
	src := []byte(strings.Repeat("frame checks ", 64))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	tests := []struct {
		name string
		src  []byte
		want error
	}{
		{name: "empty source", src: []byte{}, want: codec.ErrIncompleteStream},
		{name: "appended frame", src: append(append([]byte(nil), encoded...), emptyZstdFrame...), want: codec.ErrTrailingBytes},
		{name: "leading skippable frame", src: append(append([]byte(nil), skippableZstdFrame...), encoded...), want: codec.ErrNotZstdFrame},
		{name: "dictionary", src: []byte{0x28, 0xB5, 0x2F, 0xFD, 0x21, 0x07, 0x00, 0x01, 0x00, 0x00}, want: codec.ErrDictionary},
		{name: "window above the limit", src: []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x88, 0x01, 0x00, 0x00}, want: codec.ErrWindowTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := codec.DecodePrefix(codec.Zstd, nil, tt.src, 0, len(src))
			require.ErrorIs(t, err, tt.want)
			require.Empty(t, out)
		})
	}
}

func TestDecodePrefixZstdShortStream(t *testing.T) {
	src := []byte(strings.Repeat("short stream ", 16))
	encoded, err := codec.Encode(codec.Zstd, nil, src)
	require.NoError(t, err)

	got, err := codec.DecodePrefix(codec.Zstd, nil, encoded, len(src), len(src)+100)
	require.NoError(t, err, "a prefix the frame holds decodes although the frame is shorter than the declared body")
	requireSameBytes(t, src, got, "prefix")

	_, err = codec.DecodePrefix(codec.Zstd, nil, encoded, len(src)+1, len(src)+100)
	require.ErrorIs(t, err, codec.ErrIncompleteStream, "a frame holding fewer bytes than the prefix is incomplete")
}

// TestDecodePrefixZstdBoundedMemory checks that a header-only read allocates for its prefix and the decoder's window,
// not for the whole body.
func TestDecodePrefixZstdBoundedMemory(t *testing.T) {
	const bodyLen = 64 << 20

	encoded, err := codec.Encode(codec.Zstd, nil, make([]byte, bodyLen))
	require.NoError(t, err)

	const prefixLen = 4 << 10

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	got, err := codec.DecodePrefix(codec.Zstd, nil, encoded, prefixLen, bodyLen)
	require.NoError(t, err)
	require.Len(t, got, prefixLen)

	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("a %d-byte prefix of a %d-byte body allocated %d bytes", prefixLen, bodyLen, delta)
	require.Less(t, delta, uint64(bodyLen)/4, "a prefix decode must not allocate close to the whole body")
}
