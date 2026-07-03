package secs1

import (
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func collect(t *testing.T, body wire.Body, h messageHeader) []block {
	t.Helper()
	seq, err := splitBody(body, h)
	require.NoError(t, err)
	var blocks []block
	for b := range seq {
		blocks = append(blocks, b)
	}

	return blocks
}

func TestSplitBody_NumberingAndEBit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		bodyLen    int
		wantBlocks int
	}{
		{name: "empty header-only", bodyLen: 0, wantBlocks: 1},
		{name: "single", bodyLen: 100, wantBlocks: 1},
		{name: "exact boundary 244", bodyLen: 244, wantBlocks: 1},
		{name: "just over 245", bodyLen: 245, wantBlocks: 2},
		{name: "two full 488", bodyLen: 488, wantBlocks: 2},
		{name: "two full plus one 489", bodyLen: 489, wantBlocks: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := wire.AdoptBody(make([]byte, tc.bodyLen))
			blocks := collect(t, body, messageHeader{deviceID: 1, stream: 6, function: 11})
			require.Len(t, blocks, tc.wantBlocks)
			for i, b := range blocks {
				require.Equal(t, uint16(i+1), b.blockNumber()) // 1..N, E4-strict (header-only = 1)
				require.Equal(t, i == len(blocks)-1, b.eBit()) // E-bit only on the last block
			}
		})
	}
}

func TestSplitBody_Guards(t *testing.T) {
	t.Parallel()
	body := wire.AdoptBody([]byte{1})
	_, err := splitBody(body, messageHeader{deviceID: 0x8000})
	require.ErrorIs(t, err, ErrInvalidHeader)
	_, err = splitBody(body, messageHeader{stream: 128})
	require.ErrorIs(t, err, ErrInvalidHeader)
	oversized := wire.AdoptBody(make([]byte, maxBlockBodySize*maxBlockNumber+1))
	_, err = splitBody(oversized, messageHeader{deviceID: 1})
	require.ErrorIs(t, err, ErrMessageTooLarge)
}

func TestSplitBody_ZeroCopyAlias(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 300) // 2 blocks
	body := wire.AdoptBody(buf)
	blocks := collect(t, body, messageHeader{deviceID: 1, stream: 6, function: 11})
	require.Len(t, blocks, 2)
	buf[0] = 0xAB // mutate the backing buffer AFTER splitting
	frame := blocks[0].appendTo(nil)
	// frame = [len][header(10)][body...]; body[0] is at index 1+blockHeaderSize.
	require.Equal(t, byte(0xAB), frame[1+blockHeaderSize], "block body must alias the shared buffer, not copy")
}

func TestSplitBody_AllocsIndependentOfBlockCount(t *testing.T) {
	hdr := messageHeader{deviceID: 1, stream: 6, function: 11}
	measure := func(blocks int) float64 {
		body := wire.AdoptBody(make([]byte, maxBlockBodySize*blocks))
		count := 0
		seq, _ := splitBody(body, hdr) // warm-up
		for range seq {
			count++
		}

		return testing.AllocsPerRun(50, func() {
			seq, _ := splitBody(body, hdr)
			for range seq {
				count++
			}
		})
	}
	require.Equal(t, measure(4), measure(64), "splitBody allocs must not scale with block count (no per-block body copy)")
}

func TestSplitBody_ConcurrentRace(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 1000)
	for i := range raw {
		raw[i] = byte(i)
	}
	body := wire.AdoptBody(raw)
	hdr := messageHeader{deviceID: 1, stream: 6, function: 11}
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			seq, err := splitBody(body, hdr)
			if err != nil {
				t.Error(err)
				return
			}
			var out []byte
			for b := range seq {
				out = b.appendTo(out)
			}
			_ = out
		}()
	}
	wg.Wait()
}

// helper: split a body, then serialize+parse each block into its OWN buffer (so each block is
// caller-owned, satisfying the parseBlock ownership contract).
func splitParse(t *testing.T, body wire.Body, h messageHeader) ([]block, [][]byte) {
	t.Helper()
	seq, err := splitBody(body, h)
	require.NoError(t, err)
	var parsed []block
	var frames [][]byte
	for b := range seq {
		frame := b.appendTo(nil)
		got, err := parseBlock(frame[0], frame[1:])
		require.NoError(t, err)
		parsed = append(parsed, got)
		frames = append(frames, frame)
	}

	return parsed, frames
}

func TestAssembleBlocks_MultiBlockRoundTrip(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 600) // 3 blocks (244 + 244 + 112)
	for i := range raw {
		raw[i] = byte(i)
	}
	hdr := messageHeader{deviceID: 0x1234, rBit: true, stream: 6, function: 11, systemBytes: [4]byte{9, 8, 7, 6}}
	parsed, _ := splitParse(t, wire.AdoptBody(raw), hdr)
	require.Len(t, parsed, 3)

	gotHdr, gotBody, err := assembleBlocks(parsed)
	require.NoError(t, err)
	require.Equal(t, hdr, gotHdr)
	require.Equal(t, raw, gotBody.AppendTo(nil))
}

func TestAssembleBlocks_DecodesValidSECS2(t *testing.T) {
	t.Parallel()
	// A valid SECS-II ASCII item "HI": format byte 0x41 (ASCII=16<<2 | 1 length byte), len 2, 'H','I'.
	raw := []byte{0x41, 0x02, 'H', 'I'}
	hdr := messageHeader{deviceID: 1, stream: 1, function: 1, waitBit: true}
	parsed, _ := splitParse(t, wire.AdoptBody(raw), hdr)
	require.Len(t, parsed, 1)

	_, gotBody, err := assembleBlocks(parsed)
	require.NoError(t, err)
	require.Equal(t, raw, gotBody.AppendTo(nil))

	item, err := secs2.Decode(gotBody.AppendTo(nil))
	require.NoError(t, err)
	require.Equal(t, raw, item.ToBytes())
}

func TestAssembleBlocks_Validation(t *testing.T) {
	t.Parallel()
	h := messageHeader{deviceID: 1, stream: 1, function: 1}
	mk := func(bn uint16, last bool, hh messageHeader) block { return block{header: buildHeader(hh, bn, last)} }

	_, _, err := assembleBlocks(nil)
	require.ErrorIs(t, err, ErrEmptyBlocks)

	_, _, err = assembleBlocks([]block{mk(1, false, h), mk(3, true, h)}) // number gap
	require.ErrorIs(t, err, ErrBlockNumberMismatch)

	_, _, err = assembleBlocks([]block{mk(1, false, h), mk(2, false, h)}) // no E-bit
	require.ErrorIs(t, err, ErrEBitPlacement)

	_, _, err = assembleBlocks([]block{mk(1, true, h), mk(2, true, h)}) // E-bit before last
	require.ErrorIs(t, err, ErrEBitPlacement)

	h2 := h
	h2.function = 2
	_, _, err = assembleBlocks([]block{mk(1, false, h), mk(2, true, h2)}) // header mismatch
	require.ErrorIs(t, err, ErrHeaderMismatch)
}

func TestAssembleBlocks_IndependentOfBlockBuffers(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 500) // 3 blocks
	for i := range raw {
		raw[i] = byte(i)
	}
	hdr := messageHeader{deviceID: 1, stream: 6, function: 11}
	parsed, frames := splitParse(t, wire.AdoptBody(raw), hdr)

	_, gotBody, err := assembleBlocks(parsed)
	require.NoError(t, err)
	snapshot := gotBody.AppendTo(nil)

	for _, f := range frames { // clobber every per-block buffer
		for i := range f {
			f[i] = 0xFF
		}
	}
	require.Equal(t, snapshot, gotBody.AppendTo(nil), "assembled body must be independent of the per-block buffers")
}
