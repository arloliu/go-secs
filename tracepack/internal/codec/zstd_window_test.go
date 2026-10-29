package codec_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// zstdWindowFloor and zstdWindowCap are the window bounds of the tracepack format specification §2:
// a frame declares no window larger than the larger of zstdWindowFloor and its decoded length,
// and never one above zstdWindowCap.
const (
	zstdWindowFloor = 8 << 20
	zstdWindowCap   = 64 << 20
)

// declaredZstdWindow returns the window the Zstandard frame at the start of frame declares,
// read by the zstd library's own header parser rather than by this package's frame walker:
// the Frame_Content_Size of a single-segment frame, else the value of its Window_Descriptor (RFC 8878 §3.1.1.1).
func declaredZstdWindow(t *testing.T, frame []byte) uint64 {
	t.Helper()

	var h zstd.Header
	require.NoError(t, h.Decode(frame))
	if h.SingleSegment {
		require.True(t, h.HasFCS, "a single-segment frame carries its content size")

		return h.FrameContentSize
	}

	return h.WindowSize
}

// requireZstdWindowBound checks that frame, the encoding of a body of decodedLen bytes,
// declares a window within the bounds of the tracepack format specification §2.
func requireZstdWindowBound(t *testing.T, frame []byte, decodedLen int) {
	t.Helper()

	window := declaredZstdWindow(t, frame)
	require.LessOrEqual(t, window, max(uint64(zstdWindowFloor), uint64(decodedLen)),
		"window above the larger of 8 MiB and the decoded length")
	require.LessOrEqual(t, window, uint64(zstdWindowCap), "window above 64 MiB")
}

// TestZstdEncoderWindowBound checks that every frame Encode writes, and EncodeLevel at each level, declares a window
// no larger than the larger of 8 MiB and its decoded length, and never one above 64 MiB,
// at the sizes around the 8 MiB floor and above the 64 MiB cap,
// and that Decode accepts each frame.
func TestZstdEncoderWindowBound(t *testing.T) {
	// One compressible buffer serves every size, so the largest body is allocated once.
	const largest = 65 << 20
	pattern := []byte("the tracepack format specification section 2, the zstd window bound; ")
	all := bytes.Repeat(pattern, largest/len(pattern)+1)[:largest]

	tests := []struct {
		name string
		size int
	}{
		{name: "0 B", size: 0},
		{name: "1 KiB", size: 1 << 10},
		{name: "8 MiB - 1", size: zstdWindowFloor - 1},
		{name: "8 MiB", size: zstdWindowFloor},
		{name: "8 MiB + 1", size: zstdWindowFloor + 1},
		{name: "65 MiB", size: largest},
	}
	for _, level := range []int{codec.LevelDefault, codec.LevelFastest, codec.LevelBest} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("level %d/%s", level, tt.name), func(t *testing.T) {
				body := all[:tt.size]
				frame, err := codec.EncodeLevel(codec.Zstd, level, nil, body)
				require.NoError(t, err)
				if level == codec.LevelDefault {
					same, err := codec.Encode(codec.Zstd, nil, body)
					require.NoError(t, err)
					require.True(t, bytes.Equal(same, frame), "Encode is EncodeLevel at LevelDefault")
				}

				requireZstdWindowBound(t, frame, len(body))

				decoded, err := codec.Decode(codec.Zstd, nil, frame, len(body))
				require.NoError(t, err)
				require.True(t, bytes.Equal(body, decoded), "round trip")
			})
		}
	}
}

// TestZstdDecoderWindowBound checks the window bound Decode enforces at its edges,
// beside the cases of TestDecodeZstdRejectsWindowAboveLimit and TestDecodeZstdWindowBudget:
// a window of exactly 8 MiB is accepted for a small body, and one of exactly 64 MiB for a body that large;
// a window just above the 8 MiB floor is rejected for a smaller body and accepted for a body that large;
// and a window above 64 MiB is rejected even for a decoded length that covers it.
func TestZstdDecoderWindowBound(t *testing.T) {
	// Each frame declares its window and then holds one last Raw block of 3 bytes,
	// so a decoded length other than 3 that passes the window check fails later as an incomplete stream.
	rawBlock := []byte{0x19, 0x00, 0x00, 'a', 'b', 'c'}
	frame := func(header ...byte) []byte {
		return append(append([]byte{0x28, 0xB5, 0x2F, 0xFD}, header...), rawBlock...)
	}

	// Window_Descriptor 0x68: exponent 13, mantissa 0, so exactly the 8 MiB floor.
	window8MiB := frame(0x00, 0x68)
	// Window_Descriptor 0x80: exponent 16, mantissa 0, so exactly the 64 MiB cap.
	window64MiB := frame(0x00, 0x80)
	// Window_Descriptor 0x69: exponent 13, mantissa 1, so 8 MiB + 1 MiB.
	window9MiB := frame(0x00, 0x69)
	// Single segment with a four-byte Frame_Content_Size of 9 MiB, which is the window.
	single9MiB := frame(0xA0, 0x00, 0x00, 0x90, 0x00)
	// Window_Descriptor 0x81: exponent 16, mantissa 1, so 64 MiB + 8 MiB.
	window72MiB := frame(0x00, 0x81)

	tests := []struct {
		name       string
		frame      []byte
		decodedLen int
		wantErr    error
	}{
		{name: "8 MiB window for a 4-byte body", frame: window8MiB, decodedLen: 4, wantErr: codec.ErrIncompleteStream},
		{name: "64 MiB window for a 64 MiB body", frame: window64MiB, decodedLen: zstdWindowCap, wantErr: codec.ErrIncompleteStream},
		{name: "9 MiB window for a 3-byte body", frame: window9MiB, decodedLen: 3, wantErr: codec.ErrWindowTooLarge},
		{name: "9 MiB window for an 8 MiB body", frame: window9MiB, decodedLen: zstdWindowFloor, wantErr: codec.ErrWindowTooLarge},
		{name: "9 MiB window for a 9 MiB body", frame: window9MiB, decodedLen: 9 << 20, wantErr: codec.ErrIncompleteStream},
		{name: "single segment 9 MiB for a 3-byte body", frame: single9MiB, decodedLen: 3, wantErr: codec.ErrWindowTooLarge},
		{name: "72 MiB window for a 72 MiB body", frame: window72MiB, decodedLen: 72 << 20, wantErr: codec.ErrWindowTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := codec.Decode(codec.Zstd, nil, tt.frame, tt.decodedLen)
			require.ErrorIs(t, err, tt.wantErr)
			if !errors.Is(tt.wantErr, codec.ErrWindowTooLarge) {
				require.NotErrorIs(t, err, codec.ErrWindowTooLarge)
			}
		})
	}
}
