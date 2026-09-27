package codec

import (
	"encoding/binary"
	"fmt"
)

// zstdFrameMagic is the Zstandard frame magic of RFC 8878 §3.1.1, read as a little-endian uint32.
const zstdFrameMagic uint32 = 0xFD2FB528

// Field sizes and values of the Zstandard frame format, RFC 8878 §3.1.1.
const (
	zstdFrameMagicLen       = 4
	zstdBlockHeaderLen      = 3
	zstdContentChecksumLen  = 4
	zstdMinWindowLog        = 10
	zstdBlockTypeRaw        = 0
	zstdBlockTypeRLE        = 1
	zstdBlockTypeCompressed = 2
)

// zstdDictionaryIDLen maps a Dictionary_ID_flag to the Dictionary_ID field's size in bytes (RFC 8878 §3.1.1.1.1).
var zstdDictionaryIDLen = [4]int{0, 1, 2, 4}

// zstdContentSizeLen maps a Frame_Content_Size_flag to the Frame_Content_Size field's size in bytes,
// for a frame without the Single_Segment_flag;
// a single-segment frame with flag 0 has a one-byte field instead (RFC 8878 §3.1.1.1.1).
var zstdContentSizeLen = [4]int{0, 2, 4, 8}

// zstdFrameHeader is what the walker needs from a parsed RFC 8878 §3.1.1.1 frame header.
type zstdFrameHeader struct {
	// headerLen counts the bytes from the frame magic through the end of the Frame_Content_Size field.
	headerLen int
	// window is the declared Window_Size:
	// the Frame_Content_Size for a single-segment frame, otherwise the value the Window_Descriptor encodes.
	window        uint64
	hasDictionary bool
	hasChecksum   bool
}

// walkZstdFrame measures the one RFC 8878 §3.1.1 Zstandard frame at the start of src without decompressing it.
// It parses the frame header, steps over each block by its Block_Header, and steps over the content checksum when present.
//
// It rejects what the tracepack format specification §2 forbids before any decoder sees the frame:
// a frame that names a dictionary, and a frame whose window exceeds maxWindow.
// Bytes after the frame are not examined; the caller compares frameLen with len(src).
//
// Returns:
//   - frameLen: the frame's total length in bytes, at most len(src).
//   - window: the frame's declared window size in bytes.
//   - err: ErrIncompleteStream when src ends before the frame does, including a src shorter than the frame magic;
//     ErrNotZstdFrame for a magic other than the Zstandard frame magic, skippable frames included, or a reserved block type;
//     ErrDictionary when the Dictionary_ID_flag is set; ErrWindowTooLarge for a window above maxWindow.
func walkZstdFrame(src []byte) (frameLen int, window uint64, err error) {
	hdr, err := parseZstdFrameHeader(src)
	if err != nil {
		return 0, 0, err
	}
	if hdr.hasDictionary {
		return 0, 0, fmt.Errorf("frame header names a dictionary: %w", ErrDictionary)
	}
	if hdr.window > maxWindow {
		return 0, 0, fmt.Errorf("frame window %d exceeds %d bytes: %w", hdr.window, maxWindow, ErrWindowTooLarge)
	}

	end, err := walkZstdBlocks(src, hdr.headerLen)
	if err != nil {
		return 0, 0, err
	}
	if hdr.hasChecksum {
		if len(src)-end < zstdContentChecksumLen {
			return 0, 0, fmt.Errorf("content checksum at offset %d: %w", end, ErrIncompleteStream)
		}
		end += zstdContentChecksumLen
	}

	return end, hdr.window, nil
}

// parseZstdFrameHeader parses the frame magic and the frame header of RFC 8878 §3.1.1.1.
// It reads every field the Frame_Header_Descriptor announces, the Dictionary_ID included,
// and leaves the judgement of their values to walkZstdFrame.
func parseZstdFrameHeader(src []byte) (zstdFrameHeader, error) {
	if len(src) < zstdFrameMagicLen {
		return zstdFrameHeader{}, fmt.Errorf("frame magic: %d of %d bytes: %w", len(src), zstdFrameMagicLen, ErrIncompleteStream)
	}
	if magic := binary.LittleEndian.Uint32(src); magic != zstdFrameMagic {
		return zstdFrameHeader{}, fmt.Errorf("magic %#08x: %w", magic, ErrNotZstdFrame)
	}
	if len(src) == zstdFrameMagicLen {
		return zstdFrameHeader{}, fmt.Errorf("frame header descriptor: %w", ErrIncompleteStream)
	}

	fhd := src[zstdFrameMagicLen]
	singleSegment := fhd&(1<<5) != 0
	dictionaryIDLen := zstdDictionaryIDLen[fhd&0x03]
	contentSizeLen := zstdContentSizeLen[fhd>>6]
	if contentSizeLen == 0 && singleSegment {
		contentSizeLen = 1
	}
	windowDescriptorLen := 1
	if singleSegment {
		windowDescriptorLen = 0
	}

	hdr := zstdFrameHeader{
		headerLen:     zstdFrameMagicLen + 1 + windowDescriptorLen + dictionaryIDLen + contentSizeLen,
		hasDictionary: dictionaryIDLen != 0,
		hasChecksum:   fhd&(1<<2) != 0,
	}
	if len(src) < hdr.headerLen {
		return zstdFrameHeader{}, fmt.Errorf("frame header: %d of %d bytes: %w", len(src), hdr.headerLen, ErrIncompleteStream)
	}

	if singleSegment {
		hdr.window = zstdContentSize(src[hdr.headerLen-contentSizeLen : hdr.headerLen])
	} else {
		descriptor := src[zstdFrameMagicLen+1]
		exponent := descriptor >> 3
		mantissa := uint64(descriptor & 0x07)
		windowBase := uint64(1) << (zstdMinWindowLog + exponent)
		hdr.window = windowBase + (windowBase/8)*mantissa
	}

	return hdr, nil
}

// zstdContentSize decodes a little-endian Frame_Content_Size field of 1, 2, 4 or 8 bytes.
// A two-byte field is offset by 256 (RFC 8878 §3.1.1.1.4).
func zstdContentSize(field []byte) uint64 {
	switch len(field) {
	case 1:
		return uint64(field[0])
	case 2:
		return uint64(binary.LittleEndian.Uint16(field)) + 256
	case 4:
		return uint64(binary.LittleEndian.Uint32(field))
	default:
		return binary.LittleEndian.Uint64(field)
	}
}

// walkZstdBlocks steps over the blocks of RFC 8878 §3.1.1.2 starting at offset off,
// and returns the offset just past the block whose Last_Block bit is set.
// A Raw or Compressed block occupies Block_Size bytes after its header and an RLE block one byte;
// Block_Size limits and block contents are left to the decoder.
func walkZstdBlocks(src []byte, off int) (int, error) {
	for {
		if len(src)-off < zstdBlockHeaderLen {
			return 0, fmt.Errorf("block header at offset %d: %w", off, ErrIncompleteStream)
		}
		header := int(src[off]) | int(src[off+1])<<8 | int(src[off+2])<<16
		last := header&1 != 0
		blockType := (header >> 1) & 0x03
		blockSize := header >> 3

		var contentLen int
		switch blockType {
		case zstdBlockTypeRaw, zstdBlockTypeCompressed:
			contentLen = blockSize
		case zstdBlockTypeRLE:
			contentLen = 1
		default:
			return 0, fmt.Errorf("reserved block type at offset %d: %w", off, ErrNotZstdFrame)
		}

		off += zstdBlockHeaderLen
		if len(src)-off < contentLen {
			return 0, fmt.Errorf("block content at offset %d: %d of %d bytes: %w", off, len(src)-off, contentLen, ErrIncompleteStream)
		}
		off += contentLen
		if last {
			return off, nil
		}
	}
}
