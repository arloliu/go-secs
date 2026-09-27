package codec

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// frameMagic is the little-endian encoding of the RFC 8878 §3.1.1 frame magic 0xFD2FB528.
var frameMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// Block types of RFC 8878 §3.1.1.2, as test inputs for zstdBlock.
const (
	rawBlock        = 0
	rleBlock        = 1
	compressedBlock = 2
	reservedBlock   = 3
)

// zstdBlock builds one block: a three-byte little-endian Block_Header followed by the bytes its type puts on disk.
// The content bytes are arbitrary, since the walker measures blocks without decoding them.
func zstdBlock(last bool, blockType, blockSize int) []byte {
	header := blockType<<1 | blockSize<<3
	if last {
		header |= 1
	}
	onDisk := blockSize
	if blockType == rleBlock {
		onDisk = 1
	}

	out := make([]byte, 3+onDisk)
	out[0], out[1], out[2] = byte(header), byte(header>>8), byte(header>>16)

	return out
}

// zstdFrameBytes concatenates a frame from its parts, starting with the frame magic.
func zstdFrameBytes(parts ...[]byte) []byte {
	return slices.Concat(append([][]byte{frameMagic}, parts...)...)
}

// validWalkerFrames returns hand-built frames the walker accepts,
// each with its expected window size, one per header shape of RFC 8878 §3.1.1.1.
func validWalkerFrames() []struct {
	name   string
	frame  []byte
	window uint64
} {
	return []struct {
		name   string
		frame  []byte
		window uint64
	}{
		{
			// Frame_Header_Descriptor 0x20: single segment, one-byte Frame_Content_Size.
			name:   "single segment, 1-byte FCS, raw block",
			frame:  zstdFrameBytes([]byte{0x20, 0x05}, zstdBlock(true, rawBlock, 5)),
			window: 5,
		},
		{
			// 0x60: single segment, two-byte Frame_Content_Size, which carries a 256 offset: 0x012C + 256 = 556.
			name:   "single segment, 2-byte FCS, RLE block",
			frame:  zstdFrameBytes([]byte{0x60, 0x2C, 0x01}, zstdBlock(true, rleBlock, 556)),
			window: 556,
		},
		{
			// 0xA0: single segment, four-byte Frame_Content_Size of 65536.
			name: "single segment, 4-byte FCS, compressed then raw blocks",
			frame: zstdFrameBytes(
				[]byte{0xA0, 0x00, 0x00, 0x01, 0x00},
				zstdBlock(false, compressedBlock, 10),
				zstdBlock(true, rawBlock, 0),
			),
			window: 65536,
		},
		{
			// 0xE0: single segment, eight-byte Frame_Content_Size of 1 MiB.
			name:   "single segment, 8-byte FCS",
			frame:  zstdFrameBytes([]byte{0xE0, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00}, zstdBlock(true, rawBlock, 3)),
			window: 1 << 20,
		},
		{
			// 0x00 with Window_Descriptor 0x00: exponent 0, mantissa 0, the 1 KiB minimum window; no Frame_Content_Size.
			name:   "window descriptor, no FCS",
			frame:  zstdFrameBytes([]byte{0x00, 0x00}, zstdBlock(true, compressedBlock, 7)),
			window: 1 << 10,
		},
		{
			// 0x40 with Window_Descriptor 0x5B: exponent 11, mantissa 3, so 2 MiB + 3 × 256 KiB; two-byte FCS.
			name:   "window descriptor with mantissa, 2-byte FCS",
			frame:  zstdFrameBytes([]byte{0x40, 0x5B, 0x00, 0x00}, zstdBlock(true, rleBlock, 256)),
			window: 2<<20 + 3*(256<<10),
		},
		{
			// 0x84 with Window_Descriptor 0x18: exponent 3, so 8 KiB; four-byte FCS; content checksum.
			name: "window descriptor, 4-byte FCS, checksum",
			frame: zstdFrameBytes(
				[]byte{0x84, 0x18, 0x0E, 0x00, 0x00, 0x00},
				zstdBlock(false, compressedBlock, 4),
				zstdBlock(true, rleBlock, 10),
				[]byte{0xAA, 0xBB, 0xCC, 0xDD},
			),
			window: 8 << 10,
		},
		{
			// 0xC0 with Window_Descriptor 0x80: exponent 16, exactly the 64 MiB limit; eight-byte FCS.
			name:   "window at the limit, 8-byte FCS",
			frame:  zstdFrameBytes([]byte{0xC0, 0x80, 1, 0, 0, 0, 0, 0, 0, 0}, zstdBlock(true, rawBlock, 1)),
			window: 64 << 20,
		},
		{
			// 0x24: single segment, one-byte FCS, content checksum.
			name:   "single segment, checksum",
			frame:  zstdFrameBytes([]byte{0x24, 0x00}, zstdBlock(true, rawBlock, 0), []byte{0x99, 0xE9, 0xD8, 0x51}),
			window: 0,
		},
		{
			// Written by the zstd v1.5.5 CLI for a file of 300 'x' bytes:
			// single segment with a two-byte FCS of 0x002C + 256 = 300, one compressed block, and a checksum.
			name: "zstd CLI 300-byte file",
			frame: []byte{
				0x28, 0xB5, 0x2F, 0xFD, 0x64, 0x2C, 0x00, 0x4D, 0x00, 0x00, 0x10, 0x78,
				0x78, 0x01, 0x00, 0x27, 0x2A, 0xC0, 0x02, 0xBF, 0xD5, 0x0F, 0xC8,
			},
			window: 300,
		},
	}
}

func TestWalkZstdFrameAccepts(t *testing.T) {
	for _, tt := range validWalkerFrames() {
		t.Run(tt.name, func(t *testing.T) {
			frameLen, window, err := walkZstdFrame(tt.frame)
			require.NoError(t, err)
			require.Equal(t, len(tt.frame), frameLen)
			require.Equal(t, tt.window, window)

			// Bytes after the frame are not the walker's to judge: it reports where the frame ends.
			frameLen, _, err = walkZstdFrame(append(slices.Clone(tt.frame), 0x00, 0x01))
			require.NoError(t, err)
			require.Equal(t, len(tt.frame), frameLen)
		})
	}
}

func TestWalkZstdFrameRejectsTruncation(t *testing.T) {
	for _, tt := range validWalkerFrames() {
		t.Run(tt.name, func(t *testing.T) {
			for n := range len(tt.frame) {
				_, _, err := walkZstdFrame(tt.frame[:n])
				require.ErrorIsf(t, err, ErrIncompleteStream, "frame truncated to %d of %d bytes", n, len(tt.frame))
			}
		})
	}
}

func TestWalkZstdFrameRejects(t *testing.T) {
	tests := []struct {
		name    string
		frame   []byte
		wantErr error
	}{
		{name: "empty", frame: nil, wantErr: ErrIncompleteStream},
		{name: "partial magic", frame: frameMagic[:3], wantErr: ErrIncompleteStream},
		{name: "magic only", frame: frameMagic, wantErr: ErrIncompleteStream},
		{name: "wrong magic", frame: []byte{0x28, 0xB5, 0x2F, 0xFE, 0x20, 0x00, 0x01, 0x00, 0x00}, wantErr: ErrNotZstdFrame},
		{name: "skippable frame, first magic", frame: []byte{0x50, 0x2A, 0x4D, 0x18, 0x00, 0x00, 0x00, 0x00}, wantErr: ErrNotZstdFrame},
		{name: "skippable frame, last magic", frame: []byte{0x5F, 0x2A, 0x4D, 0x18, 0x00, 0x00, 0x00, 0x00}, wantErr: ErrNotZstdFrame},
		{
			name:    "1-byte dictionary id",
			frame:   zstdFrameBytes([]byte{0x21, 0x07, 0x00}, zstdBlock(true, rawBlock, 0)),
			wantErr: ErrDictionary,
		},
		{
			name:    "2-byte dictionary id",
			frame:   zstdFrameBytes([]byte{0x02, 0x00, 0x07, 0x00}, zstdBlock(true, rawBlock, 0)),
			wantErr: ErrDictionary,
		},
		{
			name:    "4-byte dictionary id",
			frame:   zstdFrameBytes([]byte{0x23, 0x07, 0x00, 0x00, 0x00, 0x00}, zstdBlock(true, rawBlock, 0)),
			wantErr: ErrDictionary,
		},
		{
			name:    "dictionary id field holding zero",
			frame:   zstdFrameBytes([]byte{0x21, 0x00, 0x00}, zstdBlock(true, rawBlock, 0)),
			wantErr: ErrDictionary,
		},
		{
			// Window_Descriptor 0x81: exponent 16, mantissa 1, so 72 MiB; the header ends and no block follows.
			name:    "window descriptor above the limit",
			frame:   zstdFrameBytes([]byte{0x00, 0x81}),
			wantErr: ErrWindowTooLarge,
		},
		{
			// Window_Descriptor 0xFF: exponent 31, mantissa 7, the largest window the field can declare.
			name:    "largest window descriptor",
			frame:   zstdFrameBytes([]byte{0x00, 0xFF}),
			wantErr: ErrWindowTooLarge,
		},
		{
			// A single-segment window is the Frame_Content_Size: here 64 MiB + 1, with no block following.
			name:    "single segment above the limit",
			frame:   zstdFrameBytes([]byte{0xE0, 0x01, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00}),
			wantErr: ErrWindowTooLarge,
		},
		{
			name:    "reserved block type",
			frame:   zstdFrameBytes([]byte{0x20, 0x00}, zstdBlock(true, reservedBlock, 0)),
			wantErr: ErrNotZstdFrame,
		},
		{
			name:    "reserved block type after a valid block",
			frame:   zstdFrameBytes([]byte{0x20, 0x04}, zstdBlock(false, rawBlock, 4), zstdBlock(true, reservedBlock, 0)),
			wantErr: ErrNotZstdFrame,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := walkZstdFrame(tt.frame)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}
