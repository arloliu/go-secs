package secs1

import (
	"testing"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/stretchr/testify/require"
)

func TestBuildHeader_RoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    messageHeader
		bn   uint16
		last bool
	}{
		{name: "max fields", h: messageHeader{deviceID: 0x7FFF, rBit: true, stream: 127, function: 255, waitBit: true, systemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}}, bn: 0x7FFF, last: true},
		{name: "zero fields", h: messageHeader{}, bn: 1, last: false},
		{name: "mixed", h: messageHeader{deviceID: 0x1234, rBit: false, stream: 6, function: 12, waitBit: false, systemBytes: [4]byte{1, 2, 3, 4}}, bn: 7, last: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := block{header: buildHeader(tc.h, tc.bn, tc.last)}
			require.Equal(t, tc.h.deviceID, b.deviceID())
			require.Equal(t, tc.h.rBit, b.rBit())
			require.Equal(t, tc.h.stream, b.stream())
			require.Equal(t, tc.h.function, b.function())
			require.Equal(t, tc.h.waitBit, b.waitBit())
			require.Equal(t, tc.bn, b.blockNumber())
			require.Equal(t, tc.last, b.eBit())
			require.Equal(t, tc.h.systemBytes, b.systemBytes())
			require.Equal(t, tc.h, b.messageHeader())
		})
	}
}

// TestBuildHeader_GoldenVector pins the SEMI E4 §8 bit layout with an INDEPENDENT hand-computed
// expected [10]byte (not a round-trip through the accessors), so a wrong bit position fails here.
func TestBuildHeader_GoldenVector(t *testing.T) {
	t.Parallel()
	h := messageHeader{deviceID: 0x1234, rBit: true, stream: 6, function: 0x11, waitBit: true, systemBytes: [4]byte{0xAA, 0xBB, 0xCC, 0xDD}}
	got := buildHeader(h, 0x0203, true)
	// byte0 = 0x12|0x80(R)=0x92; byte1=0x34; byte2=6|0x80(W)=0x86; byte3=0x11;
	// byte4=0x02|0x80(E)=0x82; byte5=0x03; bytes6-9 = system bytes.
	want := [10]byte{0x92, 0x34, 0x86, 0x11, 0x82, 0x03, 0xAA, 0xBB, 0xCC, 0xDD}
	require.Equal(t, want, got)
}

// TestBlock_AppendTo_GoldenFrame pins the full wire frame incl. the hand-computed checksum
// (sum of header+body, length byte excluded): 1264 (header) + 131 ('A'+'B') = 1395 = 0x0573.
func TestBlock_AppendTo_GoldenFrame(t *testing.T) {
	t.Parallel()
	h := messageHeader{deviceID: 0x1234, rBit: true, stream: 6, function: 0x11, waitBit: true, systemBytes: [4]byte{0xAA, 0xBB, 0xCC, 0xDD}}
	blk := block{header: buildHeader(h, 0x0203, true), body: wire.AdoptBody([]byte{0x41, 0x42}).Chunk(0, 2)}
	want := []byte{0x0C, 0x92, 0x34, 0x86, 0x11, 0x82, 0x03, 0xAA, 0xBB, 0xCC, 0xDD, 0x41, 0x42, 0x05, 0x73}
	require.Equal(t, want, blk.appendTo(nil))
}

// TestParseBlock_GoldenFrame parses a hard-coded frame (not produced by appendTo) through the
// accessors — independent of the encode path.
func TestParseBlock_GoldenFrame(t *testing.T) {
	t.Parallel()
	frame := []byte{0x0C, 0x92, 0x34, 0x86, 0x11, 0x82, 0x03, 0xAA, 0xBB, 0xCC, 0xDD, 0x41, 0x42, 0x05, 0x73}
	got, err := parseBlock(frame[0], frame[1:])
	require.NoError(t, err)
	require.Equal(t, uint16(0x1234), got.deviceID())
	require.True(t, got.rBit())
	require.Equal(t, uint8(6), got.stream())
	require.True(t, got.waitBit())
	require.Equal(t, uint8(0x11), got.function())
	require.Equal(t, uint16(0x0203), got.blockNumber())
	require.True(t, got.eBit())
	require.Equal(t, [4]byte{0xAA, 0xBB, 0xCC, 0xDD}, got.systemBytes())
	require.Equal(t, []byte{0x41, 0x42}, got.body.AppendTo(nil))
}

func TestBlock_AppendParseRoundTrip(t *testing.T) {
	t.Parallel()
	h := messageHeader{deviceID: 0x1234, rBit: true, stream: 6, function: 11, waitBit: true, systemBytes: [4]byte{1, 2, 3, 4}}
	body := wire.AdoptBody([]byte("hello"))
	orig := block{header: buildHeader(h, 1, true), body: body.Chunk(0, 5)}

	frame := orig.appendTo(nil)
	require.Equal(t, byte(blockHeaderSize+5), frame[0]) // length byte = 15

	got, err := parseBlock(frame[0], frame[1:])
	require.NoError(t, err)
	require.Equal(t, orig.header, got.header)
	require.Equal(t, []byte("hello"), got.body.AppendTo(nil))
	require.Equal(t, h, got.messageHeader())
}

func TestParseBlock_ChecksumMismatch(t *testing.T) {
	t.Parallel()
	body := wire.AdoptBody([]byte("hi"))
	frame := block{header: buildHeader(messageHeader{}, 1, true), body: body.Chunk(0, 2)}.appendTo(nil)
	frame[len(frame)-1] ^= 0xFF // corrupt the checksum low byte
	_, err := parseBlock(frame[0], frame[1:])
	require.ErrorIs(t, err, ErrChecksumMismatch)
}

func TestParseBlock_BodyAliasesRest(t *testing.T) {
	t.Parallel()
	// parseBlock is zero-copy: the block body aliases rest (= frame[1:]); body[0] sits at
	// frame[1+blockHeaderSize]. Mutating it is observed through the parsed block (no copy).
	body := wire.AdoptBody([]byte("hello"))
	frame := block{header: buildHeader(messageHeader{}, 1, true), body: body.Chunk(0, 5)}.appendTo(nil)
	got, err := parseBlock(frame[0], frame[1:])
	require.NoError(t, err)
	frame[1+blockHeaderSize] = 'J' // 'h' -> 'J'
	require.Equal(t, []byte("Jello"), got.body.AppendTo(nil))
}

func TestParseBlock_LengthBoundaryAndErrors(t *testing.T) {
	t.Parallel()
	// Max length 254 must not wrap (lengthByte is a byte; 254+2 = 256 wraps to 0).
	body := wire.AdoptBody(make([]byte, maxBlockBodySize)) // 244
	frame := block{header: buildHeader(messageHeader{}, 1, true), body: body.Chunk(0, maxBlockBodySize)}.appendTo(nil)
	require.Equal(t, byte(maxBlockLength), frame[0]) // 254
	got, err := parseBlock(frame[0], frame[1:])
	require.NoError(t, err)
	require.Equal(t, maxBlockBodySize, got.body.Len())

	// Out-of-range length byte (< 10).
	_, err = parseBlock(9, make([]byte, 11))
	require.ErrorIs(t, err, ErrInvalidLength)

	// Out-of-range length byte (255 > 254 max — the only byte value above the max).
	_, err = parseBlock(255, make([]byte, 255+checksumSize))
	require.ErrorIs(t, err, ErrInvalidLength)

	// Length/data-length mismatch.
	_, err = parseBlock(20, make([]byte, 5))
	require.ErrorIs(t, err, ErrInvalidLength)
}

func TestBlock_AppendTo_AllocFreeChecksum(t *testing.T) {
	// Note: t.Parallel() is intentionally absent — testing.AllocsPerRun panics in parallel tests.
	body := wire.AdoptBody(make([]byte, maxBlockBodySize))
	blk := block{header: buildHeader(messageHeader{}, 1, true), body: body.Chunk(0, maxBlockBodySize)}
	dst := make([]byte, 0, maxBlockLength+1+checksumSize)
	_ = blk.appendTo(dst[:0]) // warm-up
	allocs := testing.AllocsPerRun(100, func() {
		_ = blk.appendTo(dst[:0])
	})
	require.Equal(t, float64(0), allocs, "appendTo into a pre-sized dst must not allocate (no AppendTo(nil) checksum copy)")
}
