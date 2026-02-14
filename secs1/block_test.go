package secs1

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Header accessor tests ---

func TestBlock_RBit(t *testing.T) {
	var b Block
	assert.False(t, b.RBit(), "default R-bit should be false")

	b.SetRBit(true)
	assert.True(t, b.RBit())
	assert.Equal(t, byte(0x80), b.Header[0]&0x80, "R-bit should be bit 7 of byte 0")

	b.SetRBit(false)
	assert.False(t, b.RBit())
}

func TestBlock_DeviceID(t *testing.T) {
	var b Block
	assert.Equal(t, uint16(0), b.DeviceID())

	b.SetDeviceID(1000)
	assert.Equal(t, uint16(1000), b.DeviceID())

	// Verify R-bit is preserved when setting device ID.
	b.SetRBit(true)
	b.SetDeviceID(2000)
	assert.Equal(t, uint16(2000), b.DeviceID())
	assert.True(t, b.RBit(), "R-bit must be preserved after SetDeviceID")

	// Max 15-bit value: 32767
	b.SetDeviceID(32767)
	assert.Equal(t, uint16(32767), b.DeviceID())
	assert.True(t, b.RBit())

	// Values above 15 bits are masked.
	b.SetDeviceID(0xFFFF) // only lower 15 bits kept
	assert.Equal(t, uint16(0x7FFF), b.DeviceID())
}

func TestBlock_WBit(t *testing.T) {
	var b Block
	assert.False(t, b.WBit())

	b.SetWBit(true)
	assert.True(t, b.WBit())
	assert.Equal(t, byte(0x80), b.Header[2]&0x80)

	b.SetWBit(false)
	assert.False(t, b.WBit())
}

func TestBlock_StreamCode(t *testing.T) {
	var b Block
	assert.Equal(t, byte(0), b.StreamCode())

	b.SetStreamCode(1)
	assert.Equal(t, byte(1), b.StreamCode())

	b.SetStreamCode(127)
	assert.Equal(t, byte(127), b.StreamCode())

	// Verify W-bit is preserved.
	b.SetWBit(true)
	b.SetStreamCode(42)
	assert.Equal(t, byte(42), b.StreamCode())
	assert.True(t, b.WBit(), "W-bit must be preserved after SetStreamCode")

	// Values above 7 bits are masked.
	b.SetStreamCode(0xFF)
	assert.Equal(t, byte(0x7F), b.StreamCode())
}

func TestBlock_FunctionCode(t *testing.T) {
	var b Block
	assert.Equal(t, byte(0), b.FunctionCode())

	b.SetFunctionCode(1)
	assert.Equal(t, byte(1), b.FunctionCode())

	b.SetFunctionCode(255)
	assert.Equal(t, byte(255), b.FunctionCode())
}

func TestBlock_EBit(t *testing.T) {
	var b Block
	assert.False(t, b.EBit())

	b.SetEBit(true)
	assert.True(t, b.EBit())
	assert.Equal(t, byte(0x80), b.Header[4]&0x80)

	b.SetEBit(false)
	assert.False(t, b.EBit())
}

func TestBlock_BlockNumber(t *testing.T) {
	var b Block
	assert.Equal(t, uint16(0), b.BlockNumber())

	b.SetBlockNumber(1)
	assert.Equal(t, uint16(1), b.BlockNumber())

	b.SetBlockNumber(32767) // max 15-bit
	assert.Equal(t, uint16(32767), b.BlockNumber())

	// Verify E-bit is preserved.
	b.SetEBit(true)
	b.SetBlockNumber(100)
	assert.Equal(t, uint16(100), b.BlockNumber())
	assert.True(t, b.EBit(), "E-bit must be preserved after SetBlockNumber")
}

func TestBlock_SystemBytes(t *testing.T) {
	var b Block
	sb := b.SystemBytes()
	assert.Equal(t, []byte{0, 0, 0, 0}, sb)

	b.SetSystemBytes([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	assert.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, b.SystemBytes())
	assert.Equal(t, uint32(0xDEADBEEF), b.SystemBytesUint32())

	// Returned slice is a copy — mutating it doesn't affect the block.
	sb = b.SystemBytes()
	sb[0] = 0x00
	assert.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, b.SystemBytes())
}

func TestBlock_SetSystemBytesUint32(t *testing.T) {
	var b Block
	b.SetSystemBytesUint32(0x12345678)
	assert.Equal(t, uint32(0x12345678), b.SystemBytesUint32())
	assert.Equal(t, []byte{0x12, 0x34, 0x56, 0x78}, b.SystemBytes())
}

func TestBlock_SetSystemBytes_Panics(t *testing.T) {
	var b Block
	assert.Panics(t, func() { b.SetSystemBytes([]byte{1, 2, 3}) }, "should panic on len != 4")
	assert.Panics(t, func() { b.SetSystemBytes([]byte{1, 2, 3, 4, 5}) }, "should panic on len != 4")
}

// --- Length and checksum tests ---

func TestBlock_Length(t *testing.T) {
	tests := []struct {
		name    string
		bodyLen int
		wantLen byte
	}{
		{"header only", 0, 10},
		{"1-byte body", 1, 11},
		{"max body", MaxBlockBodySize, MaxBlockLength},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Block{Body: make([]byte, tt.bodyLen)}
			assert.Equal(t, tt.wantLen, b.Length())
		})
	}
}

func TestBlock_Checksum(t *testing.T) {
	// All zeros → checksum 0.
	var b Block
	assert.Equal(t, uint16(0), b.Checksum())

	// Known value: header all 0xFF, no body → 10 * 0xFF = 2550.
	for i := range b.Header {
		b.Header[i] = 0xFF
	}
	assert.Equal(t, uint16(2550), b.Checksum())

	// With body bytes added.
	b.Body = []byte{0x01, 0x02, 0x03}
	assert.Equal(t, uint16(2550+1+2+3), b.Checksum())
}

func TestBlock_Checksum_Overflow(t *testing.T) {
	// Large values that exceed 16 bits — should wrap.
	var b Block
	for i := range b.Header {
		b.Header[i] = 0xFF
	}
	b.Body = make([]byte, MaxBlockBodySize)
	for i := range b.Body {
		b.Body[i] = 0xFF
	}
	// (10 + 244) * 255 = 254 * 255 = 64770 → fits in uint16
	assert.Equal(t, uint16(64770), b.Checksum())
}

// --- Pack / ParseBlock round-trip tests ---

func TestBlock_PackAndParse_HeaderOnly(t *testing.T) {
	b := &Block{}
	b.SetDeviceID(1000)
	b.SetRBit(true)
	b.SetWBit(true)
	b.SetStreamCode(1)
	b.SetFunctionCode(1)
	b.SetEBit(true)
	b.SetBlockNumber(1)
	b.SetSystemBytesUint32(0x00000001)

	wire := b.Pack()

	// Wire format: [length(1)][header(10)][checksum(2)] = 13 bytes
	require.Len(t, wire, 13)
	assert.Equal(t, byte(10), wire[0], "length byte should be 10 for header-only")

	// Round-trip parse.
	parsed, err := ParseBlock(wire[0], wire[1:])
	require.NoError(t, err)

	assert.Equal(t, b.Header, parsed.Header)
	assert.Empty(t, parsed.Body)
	assert.True(t, parsed.RBit())
	assert.Equal(t, uint16(1000), parsed.DeviceID())
	assert.True(t, parsed.WBit())
	assert.Equal(t, byte(1), parsed.StreamCode())
	assert.Equal(t, byte(1), parsed.FunctionCode())
	assert.True(t, parsed.EBit())
	assert.Equal(t, uint16(1), parsed.BlockNumber())
	assert.Equal(t, uint32(1), parsed.SystemBytesUint32())
}

func TestBlock_PackAndParse_WithBody(t *testing.T) {
	body := make([]byte, 100)
	for i := range body {
		body[i] = byte(i)
	}

	b := &Block{Body: body}
	b.SetDeviceID(500)
	b.SetStreamCode(6)
	b.SetFunctionCode(11)
	b.SetEBit(false)
	b.SetBlockNumber(3)
	b.SetSystemBytesUint32(0xAABBCCDD)

	wire := b.Pack()

	// Wire: [1 + 110 + 2] = 113
	require.Len(t, wire, 113)
	assert.Equal(t, byte(110), wire[0])

	parsed, err := ParseBlock(wire[0], wire[1:])
	require.NoError(t, err)

	assert.Equal(t, b.Header, parsed.Header)
	assert.Equal(t, body, parsed.Body)
	assert.Equal(t, uint16(500), parsed.DeviceID())
	assert.Equal(t, byte(6), parsed.StreamCode())
	assert.Equal(t, byte(11), parsed.FunctionCode())
	assert.False(t, parsed.EBit())
	assert.Equal(t, uint16(3), parsed.BlockNumber())
	assert.Equal(t, uint32(0xAABBCCDD), parsed.SystemBytesUint32())
}

func TestBlock_PackAndParse_MaxBody(t *testing.T) {
	body := make([]byte, MaxBlockBodySize)
	for i := range body {
		body[i] = 0xAB
	}

	b := &Block{Body: body}
	b.SetDeviceID(32767)
	b.SetRBit(true)
	b.SetWBit(true)
	b.SetStreamCode(127)
	b.SetFunctionCode(255)
	b.SetEBit(true)
	b.SetBlockNumber(32767)
	b.SetSystemBytesUint32(0xFFFFFFFF)

	wire := b.Pack()
	require.Len(t, wire, 1+MaxBlockLength+checksumSize) // 1 + 254 + 2 = 257

	parsed, err := ParseBlock(wire[0], wire[1:])
	require.NoError(t, err)

	assert.Equal(t, b.Header, parsed.Header)
	assert.Equal(t, body, parsed.Body)
}

// --- ParseBlock error cases ---

func TestParseBlock_InvalidLength_TooSmall(t *testing.T) {
	_, err := ParseBlock(9, make([]byte, 9+checksumSize))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidLength)
}

func TestParseBlock_InvalidLength_TooLarge(t *testing.T) {
	_, err := ParseBlock(255, make([]byte, 255+checksumSize))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidLength)
}

func TestParseBlock_DataLengthMismatch(t *testing.T) {
	_, err := ParseBlock(10, make([]byte, 15)) // expects 12 bytes, got 15
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data length mismatch")
}

func TestParseBlock_ChecksumMismatch(t *testing.T) {
	b := &Block{}
	b.SetStreamCode(1)
	b.SetFunctionCode(1)

	wire := b.Pack()
	// Corrupt the checksum.
	wire[len(wire)-1] ^= 0xFF

	_, err := ParseBlock(wire[0], wire[1:])
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChecksumMismatch)
}

func TestParseBlock_CorruptedBody(t *testing.T) {
	b := &Block{Body: []byte{0x01, 0x02, 0x03}}
	b.SetStreamCode(1)
	b.SetFunctionCode(1)
	b.SetEBit(true)
	b.SetBlockNumber(1)

	wire := b.Pack()
	// Corrupt a body byte (the checksum is now wrong).
	wire[12] ^= 0xFF

	_, err := ParseBlock(wire[0], wire[1:])
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrChecksumMismatch)
}

// --- Header field interaction tests ---

func TestBlock_RBitPreservedBySetDeviceID(t *testing.T) {
	var b Block
	b.SetRBit(true)
	b.SetDeviceID(0)
	assert.True(t, b.RBit())
	assert.Equal(t, uint16(0), b.DeviceID())

	b.SetRBit(false)
	b.SetDeviceID(32767)
	assert.False(t, b.RBit())
	assert.Equal(t, uint16(32767), b.DeviceID())
}

func TestBlock_WBitPreservedBySetStreamCode(t *testing.T) {
	var b Block
	b.SetWBit(true)
	b.SetStreamCode(0)
	assert.True(t, b.WBit())
	assert.Equal(t, byte(0), b.StreamCode())
}

func TestBlock_EBitPreservedBySetBlockNumber(t *testing.T) {
	var b Block
	b.SetEBit(true)
	b.SetBlockNumber(0)
	assert.True(t, b.EBit())
	assert.Equal(t, uint16(0), b.BlockNumber())
}

// --- Typical S1F1 (Are You There) block test ---

func TestBlock_S1F1_AreYouThere(t *testing.T) {
	// S1F1 W from host (R=0) to equipment device 1, single block.
	b := &Block{}
	b.SetRBit(false) // Host → Equipment
	b.SetDeviceID(1)
	b.SetWBit(true)
	b.SetStreamCode(1)
	b.SetFunctionCode(1)
	b.SetEBit(true) // single-block message
	b.SetBlockNumber(1)
	b.SetSystemBytesUint32(0x00000001)

	wire := b.Pack()

	// Verify header bytes directly.
	// Byte 0: R=0, DevID upper = 0x00 → 0x00
	assert.Equal(t, byte(0x00), wire[1])
	// Byte 1: DevID lower = 0x01
	assert.Equal(t, byte(0x01), wire[2])
	// Byte 2: W=1, Stream=1 → 0x81
	assert.Equal(t, byte(0x81), wire[3])
	// Byte 3: Function=1 → 0x01
	assert.Equal(t, byte(0x01), wire[4])
	// Byte 4: E=1, BlockNo upper = 0x00 → 0x80
	assert.Equal(t, byte(0x80), wire[5])
	// Byte 5: BlockNo lower = 0x01
	assert.Equal(t, byte(0x01), wire[6])

	// Round-trip.
	parsed, err := ParseBlock(wire[0], wire[1:])
	require.NoError(t, err)
	assert.False(t, parsed.RBit())
	assert.Equal(t, uint16(1), parsed.DeviceID())
	assert.True(t, parsed.WBit())
	assert.Equal(t, byte(1), parsed.StreamCode())
	assert.Equal(t, byte(1), parsed.FunctionCode())
	assert.True(t, parsed.EBit())
	assert.Equal(t, uint16(1), parsed.BlockNumber())
}

// --- Checksum endianness test ---

func TestBlock_Checksum_WireOrder(t *testing.T) {
	// Verify that checksum is written big-endian in Pack().
	b := &Block{}
	for i := range b.Header {
		b.Header[i] = 0xFF
	}
	// Checksum = 10 * 255 = 2550 = 0x09F6

	wire := b.Pack()
	// Checksum is the last 2 bytes.
	csHi := wire[len(wire)-2]
	csLo := wire[len(wire)-1]
	gotCS := binary.BigEndian.Uint16([]byte{csHi, csLo})
	assert.Equal(t, uint16(2550), gotCS)
}
