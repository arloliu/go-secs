package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestBlockOffsets checks that a block offset in a verification or a query that is not the block's envelope is reported.
func TestBlockOffsets(t *testing.T) {
	t.Parallel()

	pack := editPack(t, tracepack.CodecNone)
	l := must(Locate(pack))(t)
	at := U64(l.Blocks[1].Offset)

	ok := &Verify{FailedBlocks: []FailedBlock{{Block: 1, Offset: at}}}
	assert.Empty(t, blockOffsets(&l, ok, nil))

	for name, v := range map[string]*Verify{
		"failed block":     {FailedBlocks: []FailedBlock{{Block: 1, Offset: at + 1}}},
		"disagreeing":      {DisagreeingBlocks: []BlockAt{{Block: 1, Offset: at - 1}}},
		"writer defect":    {WriterDefects: []WriterDefect{{Block: 0, Offset: at}}},
		"block not walked": {FailedBlocks: []FailedBlock{{Block: 3, Offset: at}}},
	} {
		assert.NotEmpty(t, blockOffsets(&l, v, nil), name)
	}

	query := func(off U64) []QueryVector {
		return []QueryVector{{ID: "all", Expect: Expect{Incomplete: []Incomplete{
			{Reason: tracepack.ReasonCorruptBlock.String(), Block: new(1), Offset: &off},
		}}}}
	}
	assert.Empty(t, blockOffsets(&l, &Verify{}, query(at)))
	assert.NotEmpty(t, blockOffsets(&l, &Verify{}, query(at+1)))
}

// TestCheckRecordCount checks the export's record count against a verification without loss, and only then.
func TestCheckRecordCount(t *testing.T) {
	t.Parallel()

	seqs := []uint64{0, 1, 2}
	require.NoError(t, checkRecordCount(seqs, &Verify{Records: 3}))
	require.Error(t, checkRecordCount(seqs, &Verify{Records: 4}))
	require.NoError(t, checkRecordCount(seqs, &Verify{Records: 4, FailedBlocks: []FailedBlock{{Block: 1}}}))
	require.NoError(t, checkRecordCount(seqs, &Verify{Records: 4, WalkStop: new(U64(100))}))
}

// TestCheckFooter checks that footer.json's accepted must be verify.json's footer_valid,
// and that the statistics of an accepted footer must be its stored F-5.
func TestCheckFooter(t *testing.T) {
	t.Parallel()

	f5 := FooterF5{Epochs: []F5Epoch{{Epoch: 1, CloseSeq: new(U64(2))}, {Epoch: 2}}, Boundaries: []Boundary{{Seq: 3, Kind: "stop"}}}
	accepted := func() (*readOutputs, *Footer) {
		stats := tracepack.PackStats{
			Epochs:     []tracepack.EpochStats{{Epoch: 1, CloseSeq: new(uint64(2))}, {Epoch: 2}},
			Boundaries: []tracepack.Boundary{{Seq: 3, Kind: tracepack.BoundaryKindStop}},
		}
		out := &readOutputs{verify: Verify{FooterValid: true}, stats: stats, statsOK: true}

		return out, &Footer{Stored: &FooterProjection{F5: f5}, Accepted: true}
	}

	out, f := accepted()
	require.NoError(t, out.checkFooter(f))

	out, f = accepted()
	out.verify.FooterValid = false
	require.Error(t, out.checkFooter(f), "accepted against footer_valid")

	out, f = accepted()
	out.stats.Epochs[0].CloseSeq = new(uint64(3))
	require.Error(t, out.checkFooter(f), "a close_seq the stored F-5 does not state")

	out, f = accepted()
	out.stats.Boundaries = nil
	require.Error(t, out.checkFooter(f), "a boundary missing from the statistics")

	out, f = accepted()
	out.statsOK = false
	require.Error(t, out.checkFooter(f), "an accepted footer without statistics")

	rejected := &readOutputs{}
	require.NoError(t, rejected.checkFooter(&Footer{}), "a rejected footer's statistics are not compared")
}

// TestExpectationChecksVerifyKeys checks that an expectation fails a read whose verify.json differs from it
// in a key no expectation field names alone: blocks_validated, a block's offset, a lost entry's capture_id.
func TestExpectationChecksVerifyKeys(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "basic-corrupt-middle")
	built := must(r.Build(r.seed()))(t)
	read := func() *readOutputs {
		return must(readVector(t.Context(), built.Pack, &readInputs{class: r.Class, queries: built.Queries}))(t)
	}
	require.NoError(t, r.Expect.check(read(), built.Pack))

	for name, edit := range map[string]func(v *Verify){
		"blocks_validated":        func(v *Verify) { v.BlocksValidated++ },
		"a failed block's offset": func(v *Verify) { v.FailedBlocks[0].Offset++ },
		"a lost capture_id":       func(v *Verify) { v.Lost[0].CaptureID = new(tracepack.UUID{1}.String()) },
	} {
		out := read()
		edit(&out.verify)
		assert.Error(t, r.Expect.check(out, built.Pack), name)
	}
}
