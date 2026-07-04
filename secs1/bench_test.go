package secs1

import (
	"testing"

	"github.com/arloliu/go-secs/v2/internal/wire"
)

// Fixed shapes used across all secs1 benchmarks.
// bodyPayload: 500 bytes → 3 blocks (2 × 244 + 12), small enough for -benchtime=1x.
var (
	benchHeader      messageHeader
	benchBodyPayload []byte // raw payload used to build wire.Body in each bench

	// benchBlocks holds a pre-assembled []block slice for BenchmarkAssembleBlocks.
	// Built once; the blocks alias benchBlockFrame (owned, never mutated after init).
	benchBlocks     []block
	benchBlockFrame []byte // wire frame used to parse benchBlocks

	// benchSingleBlock is one parsed block for BenchmarkBlockAppendTo.
	benchSingleBlock block
)

func init() {
	benchHeader = messageHeader{
		deviceID:    0x0001,
		stream:      6,
		function:    11,
		waitBit:     true,
		systemBytes: [4]byte{0x01, 0x02, 0x03, 0x04},
	}

	// 500-byte payload: 3 blocks (244 + 244 + 12).
	benchBodyPayload = make([]byte, 500)
	for i := range benchBodyPayload {
		benchBodyPayload[i] = byte(i & 0xFF)
	}

	// Pre-build the blocks slice by splitting and appending each block's wire frame,
	// then parsing it back. This gives benchBlocks a stable set of blocks whose
	// bodies alias owned memory that never changes — safe to reuse across iterations.
	body := wire.AdoptBody(benchBodyPayload)
	seq, err := splitBody(body, benchHeader)
	if err != nil {
		panic("secs1 bench init: " + err.Error())
	}

	// Encode all blocks into a single frame buffer, then parse them back so
	// benchBlocks contains properly formed block values with owned body sub-slices.
	var frameBuf []byte
	for blk := range seq {
		start := len(frameBuf)
		frameBuf = blk.appendTo(frameBuf)
		end := len(frameBuf)
		frame := frameBuf[start:end]
		parsed, perr := parseBlock(frame[0], frame[1:])
		if perr != nil {
			panic("secs1 bench init parse: " + perr.Error())
		}
		benchBlocks = append(benchBlocks, parsed)
	}
	benchBlockFrame = frameBuf // keep alive

	// Single-block shape for BenchmarkBlockAppendTo: a full 244-byte body block.
	singleBody := wire.AdoptBody(make([]byte, maxBlockBodySize))
	singleSeq, err := splitBody(singleBody, benchHeader)
	if err != nil {
		panic("secs1 bench init single: " + err.Error())
	}
	for blk := range singleSeq {
		benchSingleBlock = blk
		break
	}
}

// BenchmarkSplitBody measures splitBody — a zero-copy iterator that emits block
// values without copying the payload. Expected: ~O(1) allocs (one closure
// allocation for the iterator), independent of block count.
func BenchmarkSplitBody(b *testing.B) {
	body := wire.AdoptBody(benchBodyPayload)
	b.ReportAllocs()
	for b.Loop() {
		seq, _ := splitBody(body, benchHeader)
		var n int
		for range seq {
			n++
		}
		_ = n
	}
}

// BenchmarkAssembleBlocks measures assembleBlocks — it validates block ordering
// and concatenates block bodies into one freshly-owned buffer (one inherent alloc
// for the coalesce buffer that escapes inside the returned wire.Body). Nothing is
// poolable: the result is caller-owned.
func BenchmarkAssembleBlocks(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _, _ = assembleBlocks(benchBlocks)
	}
}

// BenchmarkBlockAppendTo measures block.appendTo with a pre-sized, reused dst
// buffer. appendTo writes the length byte, header, body bytes, and the 16-bit
// checksum (computed in-place over the bytes already in dst). Expected: ~0 extra
// allocs/op when dst has sufficient capacity.
func BenchmarkBlockAppendTo(b *testing.B) {
	// Pre-size dst for one full block: 1 (length byte) + 254 (max body frame) + 2 (checksum).
	dst := make([]byte, 0, 1+maxBlockLength+checksumSize)
	b.ReportAllocs()
	for b.Loop() {
		dst = benchSingleBlock.appendTo(dst[:0])
	}
	_ = dst
}
