package secs1

import (
	"encoding/binary"
	"fmt"

	"github.com/arloliu/go-secs/v2/internal/wire"
)

// SECS-I block size constants (SEMI E4 §7).
const (
	maxBlockBodySize = 244                                // max SECS-II body bytes per block
	blockHeaderSize  = 10                                 // fixed 10-byte block header (SEMI E4 §8)
	checksumSize     = 2                                  // 16-bit checksum
	minBlockLength   = blockHeaderSize                    // 10 (header-only)
	maxBlockLength   = blockHeaderSize + maxBlockBodySize // 254
	maxBlockNumber   = 32767                              // 15-bit block number (SEMI E4 §8)
)

// messageHeader holds the per-message (block-invariant) SECS-I header fields — identical across
// every block and retransmission of one message (SEMI E4 §8). Block number and the E-bit are
// per-block and assigned by the splitter, not carried here.
type messageHeader struct {
	deviceID    uint16  // 15-bit (0..0x7FFF)
	rBit        bool    // direction: false = to equipment, true = to host
	stream      uint8   // 0..127 (7-bit)
	function    uint8   // 0..255
	waitBit     bool    // reply expected
	systemBytes [4]byte // transaction id
}

// buildHeader packs a messageHeader plus the per-block blockNumber and last-block flag into the
// 10-byte SECS-I header (SEMI E4 §8). deviceID and blockNumber are 15-bit, big-endian; the R-bit
// and E-bit occupy the top bit of bytes 0 and 4 respectively.
func buildHeader(h messageHeader, blockNumber uint16, last bool) [10]byte {
	var b [10]byte
	b[0] = byte(h.deviceID >> 8)
	if h.rBit {
		b[0] |= 0x80
	}
	b[1] = byte(h.deviceID)
	b[2] = h.stream & 0x7F
	if h.waitBit {
		b[2] |= 0x80
	}
	b[3] = h.function
	b[4] = byte(blockNumber >> 8)
	if last {
		b[4] |= 0x80
	}
	b[5] = byte(blockNumber)
	copy(b[6:10], h.systemBytes[:])

	return b
}

// block is an immutable SECS-I transport block: a 10-byte header value plus a zero-copy body
// sub-view of the shared frame. It is a value type (no locks/pointers) so it can be yielded by
// iter.Seq and copied freely.
type block struct {
	header [10]byte
	body   wire.Chunk
}

func (b block) deviceID() uint16     { return binary.BigEndian.Uint16(b.header[0:2]) & 0x7FFF }
func (b block) rBit() bool           { return b.header[0]&0x80 != 0 }
func (b block) stream() uint8        { return b.header[2] & 0x7F }
func (b block) waitBit() bool        { return b.header[2]&0x80 != 0 }
func (b block) function() uint8      { return b.header[3] }
func (b block) blockNumber() uint16  { return binary.BigEndian.Uint16(b.header[4:6]) & 0x7FFF }
func (b block) eBit() bool           { return b.header[4]&0x80 != 0 }
func (b block) systemBytes() [4]byte { return [4]byte(b.header[6:10]) }

// messageHeader extracts the block-invariant header fields from this block.
func (b block) messageHeader() messageHeader {
	return messageHeader{
		deviceID:    b.deviceID(),
		rBit:        b.rBit(),
		stream:      b.stream(),
		function:    b.function(),
		waitBit:     b.waitBit(),
		systemBytes: b.systemBytes(),
	}
}

// appendTo appends the block's wire form to dst and returns the extended slice:
//
//	[lengthByte][header(10)][body][checksum hi][checksum lo]
//
// lengthByte = 10 + len(body) (range 10..254). The checksum is the 16-bit arithmetic sum of the
// header+body bytes (SEMI E4 §8, NOT the length byte), computed in place over the bytes just
// written to dst, and written big-endian. There is no standalone checksum() over wire.Chunk —
// that would force an AppendTo(nil) copy; the body is materialized exactly once (the Chunk.AppendTo
// that must happen to put it on the wire) and summed where it already sits in dst.
func (b block) appendTo(dst []byte) []byte {
	bodyLen := b.body.Len()
	dst = append(dst, byte(blockHeaderSize+bodyLen)) // length byte
	start := len(dst)                                // index of header[0] in dst
	dst = append(dst, b.header[:]...)                // header
	dst = b.body.AppendTo(dst)                       // body (one materialization)

	var sum uint32
	for _, v := range dst[start:] { // header + body, summed in place
		sum += uint32(v)
	}
	cs := uint16(sum & 0xFFFF)

	return append(dst, byte(cs>>8), byte(cs))
}

// parseBlock deserializes one wire block from a length byte and the remaining bytes
// rest = header(10) + body + checksum(2). It validates the length range and length/data agreement,
// wraps the body zero-copy via wire.ChunkOf, and verifies the checksum.
//
// OWNERSHIP: rest must be caller-owned and not mutated or reused while the returned block is live;
// the block body aliases rest until assembleBlocks coalesces it (or the block is discarded).
func parseBlock(lengthByte byte, rest []byte) (block, error) {
	n := int(lengthByte) // convert first: lengthByte is a byte, so lengthByte+2 would wrap at 254
	if n < minBlockLength || n > maxBlockLength {
		return block{}, fmt.Errorf("%w: length byte %d out of [%d, %d]", ErrInvalidLength, n, minBlockLength, maxBlockLength)
	}
	if len(rest) != n+checksumSize {
		return block{}, fmt.Errorf("%w: have %d bytes, want %d", ErrInvalidLength, len(rest), n+checksumSize)
	}

	var sum uint32
	for _, v := range rest[:n] { // header + body
		sum += uint32(v)
	}
	if uint16(sum&0xFFFF) != binary.BigEndian.Uint16(rest[n:n+checksumSize]) {
		return block{}, ErrChecksumMismatch
	}

	var hdr [10]byte
	copy(hdr[:], rest[:blockHeaderSize])

	return block{header: hdr, body: wire.ChunkOf(rest[blockHeaderSize:n])}, nil
}
