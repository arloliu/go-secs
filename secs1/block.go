package secs1

import (
	"encoding/binary"
	"fmt"
)

// MaxBlockBodySize is the maximum number of SECS-II data bytes in a single block body.
// Per SEMI E4 §7, the block body can carry 0–244 bytes.
const MaxBlockBodySize = 244

// MinBlockLength is the minimum valid Length Byte value (header only, no body).
const MinBlockLength = 10

// MaxBlockLength is the maximum valid Length Byte value (10-byte header + 244-byte body).
const MaxBlockLength = 254

// blockHeaderSize is the fixed size of the SECS-I block header.
const blockHeaderSize = 10

// checksumSize is the size of the trailing checksum in bytes.
const checksumSize = 2

// SECS-I handshake bytes (SEMI E4 §7).
// These single-byte control characters are exchanged on the wire
// to coordinate block-transfer direction.
const (
	// ENQ (Request to Send) is sent by the initiator to request line control.
	ENQ byte = 0x05

	// EOT (Ready to Receive) is sent in response to ENQ, granting line control.
	EOT byte = 0x04

	// ACK (Correct Reception) is sent after a block is received and validated.
	ACK byte = 0x06

	// NAK (Incorrect Reception) is sent when a received block fails validation.
	NAK byte = 0x15
)

// Block represents a single SECS-I block (SEMI E4 §7, §8).
//
// A block on the wire is: [LengthByte][Header(10)][Body(0–244)][Checksum(2)].
// The Length Byte equals len(Header) + len(Body).
type Block struct {
	Header [blockHeaderSize]byte // 10-byte header
	Body   []byte                // 0-244 bytes of SECS-II data
}

// --- Header accessors ---

// RBit returns the R-bit (header byte 0, bit 7).
// R=0 means Host→Equipment, R=1 means Equipment→Host.
func (b *Block) RBit() bool {
	return b.Header[0]&0x80 != 0
}

// SetRBit sets or clears the R-bit.
func (b *Block) SetRBit(v bool) {
	if v {
		b.Header[0] |= 0x80
	} else {
		b.Header[0] &^= 0x80
	}
}

// DeviceID returns the 15-bit device ID from header bytes 0–1.
func (b *Block) DeviceID() uint16 {
	return binary.BigEndian.Uint16(b.Header[0:2]) & 0x7FFF
}

// SetDeviceID sets the 15-bit device ID, preserving the R-bit.
func (b *Block) SetDeviceID(id uint16) {
	rbit := b.Header[0] & 0x80
	binary.BigEndian.PutUint16(b.Header[0:2], id&0x7FFF)
	b.Header[0] |= rbit
}

// WBit returns the W-bit (header byte 2, bit 7).
// W=1 means a reply is expected.
func (b *Block) WBit() bool {
	return b.Header[2]&0x80 != 0
}

// SetWBit sets or clears the W-bit.
func (b *Block) SetWBit(v bool) {
	if v {
		b.Header[2] |= 0x80
	} else {
		b.Header[2] &^= 0x80
	}
}

// StreamCode returns the 7-bit stream code from header byte 2.
func (b *Block) StreamCode() byte {
	return b.Header[2] & 0x7F
}

// SetStreamCode sets the 7-bit stream code, preserving the W-bit.
func (b *Block) SetStreamCode(s byte) {
	b.Header[2] = (b.Header[2] & 0x80) | (s & 0x7F)
}

// FunctionCode returns the 8-bit function code from header byte 3.
func (b *Block) FunctionCode() byte {
	return b.Header[3]
}

// SetFunctionCode sets the 8-bit function code.
func (b *Block) SetFunctionCode(f byte) {
	b.Header[3] = f
}

// EBit returns the E-bit (header byte 4, bit 7).
// E=1 means this is the last block of the message.
func (b *Block) EBit() bool {
	return b.Header[4]&0x80 != 0
}

// SetEBit sets or clears the E-bit.
func (b *Block) SetEBit(v bool) {
	if v {
		b.Header[4] |= 0x80
	} else {
		b.Header[4] &^= 0x80
	}
}

// BlockNumber returns the 15-bit block number from header bytes 4–5.
func (b *Block) BlockNumber() uint16 {
	return binary.BigEndian.Uint16(b.Header[4:6]) & 0x7FFF
}

// SetBlockNumber sets the 15-bit block number, preserving the E-bit.
func (b *Block) SetBlockNumber(n uint16) {
	ebit := b.Header[4] & 0x80
	binary.BigEndian.PutUint16(b.Header[4:6], n&0x7FFF)
	b.Header[4] |= ebit
}

// SystemBytes returns a copy of the 4-byte system bytes from header bytes 6–9.
func (b *Block) SystemBytes() []byte {
	out := make([]byte, 4)
	copy(out, b.Header[6:10])
	return out
}

// SystemBytesUint32 returns the system bytes as a uint32.
func (b *Block) SystemBytesUint32() uint32 {
	return binary.BigEndian.Uint32(b.Header[6:10])
}

// SetSystemBytes sets the 4-byte system bytes. Panics if len(sb) != 4.
func (b *Block) SetSystemBytes(sb []byte) {
	if len(sb) != 4 {
		panic("secs1: system bytes must be exactly 4 bytes")
	}
	copy(b.Header[6:10], sb)
}

// SetSystemBytesUint32 sets the system bytes from a uint32.
func (b *Block) SetSystemBytesUint32(v uint32) {
	binary.BigEndian.PutUint32(b.Header[6:10], v)
}

// --- Length and checksum ---

// Length returns the Length Byte value: len(Header) + len(Body).
func (b *Block) Length() byte {
	return byte(blockHeaderSize + len(b.Body))
}

// Checksum computes the 16-bit checksum over Header + Body.
// Per SEMI E4 §7.6, this is the arithmetic sum of all unsigned byte values,
// truncated to 16 bits. The Length Byte is NOT included.
func (b *Block) Checksum() uint16 {
	var sum uint32
	for _, v := range b.Header {
		sum += uint32(v)
	}
	for _, v := range b.Body {
		sum += uint32(v)
	}

	return uint16(sum & 0xFFFF) //nolint:gosec // intentional truncation per SEMI E4 §7.6
}

// --- Wire encoding ---

// Pack serializes the block to its wire format:
//
//	[LengthByte(1)][Header(10)][Body(0–244)][Checksum_Hi(1)][Checksum_Lo(1)]
//
// The returned slice has length 1 + Length() + 2.
func (b *Block) Pack() []byte {
	length := b.Length()
	wireLen := 1 + int(length) + checksumSize
	buf := make([]byte, wireLen)

	buf[0] = length
	copy(buf[1:1+blockHeaderSize], b.Header[:])
	if len(b.Body) > 0 {
		copy(buf[1+blockHeaderSize:], b.Body)
	}

	cs := b.Checksum()
	buf[wireLen-2] = byte(cs >> 8)
	buf[wireLen-1] = byte(cs)

	return buf
}

// --- Wire decoding ---

// ParseBlock deserializes a block from its wire components.
//
// lengthByte is the first byte read from the wire (the Length Byte).
// data must contain exactly lengthByte + checksumSize bytes
// (i.e. Header + Body + 2-byte checksum, without the length byte itself).
//
// ParseBlock validates:
//   - The length byte is within [MinBlockLength, MaxBlockLength].
//   - data has the correct size (lengthByte + 2).
//   - The checksum matches.
func ParseBlock(lengthByte byte, data []byte) (*Block, error) {
	length := int(lengthByte)

	if length < MinBlockLength || length > MaxBlockLength {
		return nil, fmt.Errorf("%w: got %d, want %d–%d", ErrInvalidLength, length, MinBlockLength, MaxBlockLength)
	}

	expectedDataLen := length + checksumSize
	if len(data) != expectedDataLen {
		return nil, fmt.Errorf("secs1: data length mismatch: got %d bytes, want %d", len(data), expectedDataLen)
	}

	blk := &Block{}
	copy(blk.Header[:], data[:blockHeaderSize])

	bodyLen := length - blockHeaderSize
	if bodyLen > 0 {
		blk.Body = make([]byte, bodyLen)
		copy(blk.Body, data[blockHeaderSize:length])
	}

	// Verify checksum
	wireChecksum := binary.BigEndian.Uint16(data[length : length+checksumSize])
	calcChecksum := blk.Checksum()
	if wireChecksum != calcChecksum {
		return nil, fmt.Errorf("%w: wire=0x%04X, computed=0x%04X", ErrChecksumMismatch, wireChecksum, calcChecksum)
	}

	return blk, nil
}
