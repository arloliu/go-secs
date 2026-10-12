package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// mergeHour is the UTC hour of the merge test scopes, the hour of blockTestHour.
var mergeHour = hourOf(blockTestHour)

// touchWriter counts the calls to its Write, so a test can require that Merge never touched its output.
type touchWriter struct {
	calls int
}

// Write counts the call and takes p.
func (w *touchWriter) Write(p []byte) (int, error) {
	w.calls++

	return len(p), nil
}

// withIDs returns file with its file header's pack_id set to id and its capture_id to capture.
func withIDs(t testing.TB, file []byte, id, capture UUID) []byte {
	t.Helper()

	return patchHeader(t, file, func(h *format.FileHeader) {
		h.PackID, h.CaptureID = format.UUID(id), format.UUID(capture)
	})
}

// mergePack writes a finalized pack id of viewCapture with writeRepairPack:
// repairTestMeta changed by meta, holding the records of steps.
func mergePack(t testing.TB, id UUID, meta func(m *PackMeta), steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, meta, false, steps).file, id, viewCapture)
}

// seqSteps returns data records of seqs first to last, the record of seq s at ts0 + s,
// with a block closed after every perBlock records and after the last.
func seqSteps(first, last uint64, perBlock int, ts0 int64) []footerTestStep {
	steps := make([]footerTestStep, 0, last-first+1)
	for seq := first; seq <= last; seq++ {
		n := int(seq - first + 1)
		steps = append(steps, footerTestStep{rec: testDataRecord(seq, ts0+int64(seq), 1), flush: n%perBlock == 0 || seq == last})
	}

	return steps
}

// hourSteps returns seqSteps of seqs first to last in the merge test hour, a block closed after every perBlock records.
func hourSteps(first, last uint64, perBlock int) []footerTestStep {
	return seqSteps(first, last, perBlock, blockTestHour)
}

// inputsOf returns files as merge inputs, in their order.
func inputsOf(files ...[]byte) []MergeInput {
	inputs := make([]MergeInput, len(files))
	for i, f := range files {
		inputs[i] = MergeInput{ReaderAt: bytes.NewReader(f), Size: int64(len(f))}
	}

	return inputs
}

// mergeOpts returns the MergeOptions of the tests: a writer tag, generation 1 of epoch 1, and a conflict sink.
func mergeOpts() MergeOptions {
	return MergeOptions{Writer: "merge-test", ScopeGeneration: 1, PublisherEpoch: 1, OnConflict: func(Conflict) {}}
}

// uncachedMergeOpts returns mergeOpts with windows that hold no block (uncachedWindows),
// so every block read of the merge is a ReadAt of its input, which a test can count or make fail.
func uncachedMergeOpts() MergeOptions {
	opts := mergeOpts()
	opts.Reader = uncachedWindows()

	return opts
}

// requireMergeRefused runs Merge and requires it to fail before it touches dst,
// with an error holding msg and wrapping every error of want;
// unless want holds ErrMergeInput, the error must not wrap it.
func requireMergeRefused(t *testing.T, inputs []MergeInput, opts MergeOptions, msg string, want ...error) {
	t.Helper()

	var dst touchWriter
	rep, err := Merge(t.Context(), &dst, inputs, opts)
	require.Error(t, err)
	require.ErrorContains(t, err, msg)
	for _, w := range want {
		require.ErrorIs(t, err, w)
	}
	if !slices.Contains(want, ErrMergeInput) {
		require.NotErrorIs(t, err, ErrMergeInput)
	}
	assert.Zero(t, dst.calls, "dst untouched")
	assert.Equal(t, MergeReport{}, rep)
}

// mustPlanMerge requires the checks of Merge before it writes to pass, and returns the plan they made.
func mustPlanMerge(t *testing.T, inputs []MergeInput, opts MergeOptions) *mergePlan {
	t.Helper()

	p, err := planMerge(t.Context(), inputs, &opts)
	require.NoError(t, err)

	return p
}

// retagMeta returns file with the first pack metadata entry of tag retagged in place as to,
// so the metadata holds the entry's value under another tag.
func retagMeta(t testing.TB, file []byte, tag, to uint16) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		list, err := tlv.Decode(meta)
		require.NoError(t, err)
		binary.LittleEndian.PutUint16(meta[nthEntry(t, list, tag, 0).Offset:], to)
	})
}

// withSeqStart returns file with its pack metadata's seq_start set to seq in place.
func withSeqStart(t testing.TB, file []byte, seq uint64) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		overwriteMetaValue(t, meta, tagSeqStart, binary.LittleEndian.AppendUint64(nil, seq))
	})
}

// reindexed returns file, a finalized pack, with its footer rebuilt by the Writer's footer builder from its blocks' records,
// so the footer indexes records that rebuildPack changed and still validates.
func reindexed(t testing.TB, file []byte) []byte {
	t.Helper()

	return reindexedWith(t, file, nil)
}

// reindexedWith is reindexed with each block's summary changed by edit, when set, before the footer is built:
// F-5 is the aggregate of the changed summaries, so the footer still validates while it disagrees with the records.
func reindexedWith(t testing.TB, file []byte, edit func(i int, s *blockSummary)) []byte {
	t.Helper()

	r, blocks := readSourceBlocks(t, file)
	sums := make([]blockSummary, len(blocks))
	for i := range blocks {
		info, s := &r.blocks[i], blocks[i].sum
		s.offset, s.onDiskLen, s.uncompressedLen = info.Offset, info.OnDiskLen, info.UncompressedLen
		s.bodyCRC, s.recordHeaderLen = info.BodyCRC, info.RecordHeaderLen
		if edit != nil {
			edit(i, &s)
		}
		sums[i] = s
	}
	footer, _, err := buildFooter(sums, math.MaxUint32, 0)
	require.NoError(t, err)

	return refooter(t, file, footer)
}

// requireConsistent requires that file verifies finalized-consistent: a pack no reader rejects.
func requireConsistent(t testing.TB, file []byte) {
	t.Helper()

	v := mustVerify(t, file)
	require.Equal(t, OutcomeFinalizedConsistent, v.Outcome, "failed %v, disagreements %v", v.Failed, v.Disagreements)
}

// oversizedSteps returns hourSteps whose records are classified oversized.
func oversizedSteps(first, last uint64, perBlock int) []footerTestStep {
	steps := hourSteps(first, last, perBlock)
	for i := range steps {
		steps[i].rec.DecodeStatus = DecodeStatusOversized
	}

	return steps
}

// generationScope returns the inputs of a scope with an archive, in their order:
// the archive memA, of set setA at (publisher_epoch 2, scope_generation 3),
// at compaction_level level and with compacted_from {0xC1}, holding seqs 10 to 13, edited by member;
// then segment seg1, holding seqs 20 to 23.
func generationScope(t testing.TB, level uint8, member func(m *PackMeta)) [][]byte {
	t.Helper()

	mem := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 2, 3, []UUID{{0xC1}})
		m.CompactionLevel = level
		if member != nil {
			member(m)
		}
	}, hourSteps(10, 13, 2))
	seg := mergePack(t, seg1, nil, hourSteps(20, 23, 2))

	return [][]byte{mem, seg}
}

// generationOpts returns mergeOpts at (publisher_epoch 3, scope_generation 1).
func generationOpts() MergeOptions {
	opts := mergeOpts()
	opts.PublisherEpoch, opts.ScopeGeneration = 3, 1

	return opts
}

func TestMergeOptionErrors(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	notPacks := []MergeInput{{ReaderAt: bytes.NewReader([]byte("x")), Size: 1}, {ReaderAt: bytes.NewReader([]byte("y")), Size: 1}}

	tests := []struct {
		name   string
		edit   func(o *MergeOptions)
		inputs []MergeInput
		msg    string
	}{
		{name: "no writer", edit: func(o *MergeOptions) { o.Writer = "" }, msg: "Writer is empty"},
		{name: "no conflict sink", edit: func(o *MergeOptions) { o.OnConflict = nil }, msg: "OnConflict is nil"},
		{name: "no conflict sink before an unreadable input", edit: func(o *MergeOptions) { o.OnConflict = nil }, inputs: notPacks, msg: "OnConflict"},
		{name: "generation 0", edit: func(o *MergeOptions) { o.ScopeGeneration = 0 }, msg: "ScopeGeneration is 0"},
		{name: "generation above 2^63-1", edit: func(o *MergeOptions) { o.ScopeGeneration = 1 << 63 }, msg: "above"},
		{name: "epoch above 2^63-1", edit: func(o *MergeOptions) { o.PublisherEpoch = 1 << 63 }, msg: "above"},
		{name: "negative block threshold", edit: func(o *MergeOptions) { o.BlockThreshold = -1 }, msg: "BlockThreshold"},
		{name: "block threshold above 2^31-1", edit: func(o *MergeOptions) {
			// A variable, so the test compiles where int is 32 bits:
			// there no int lies above 2^31-1, and the conversion wraps to a negative threshold the same check refuses.
			above := int64(format.MaxLen32) + 1
			o.BlockThreshold = int(above)
		}, msg: "BlockThreshold"},
		{name: "negative open blocks", edit: func(o *MergeOptions) { o.MaxOpenBlocks = -1 }, msg: "MaxOpenBlocks"},
		{name: "negative footer budget", edit: func(o *MergeOptions) { o.MaxFooterLen = -1 }, msg: "MaxFooterLen"},
		{name: "no inputs", inputs: []MergeInput{}, msg: "no inputs"},
		{name: "an input without a ReaderAt", inputs: []MergeInput{inputsOf(a)[0], {Size: 1}}, msg: "input 1 has no ReaderAt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			if tt.edit != nil {
				tt.edit(&opts)
			}
			inputs := inputsOf(a, b)
			if tt.inputs != nil {
				inputs = tt.inputs
			}
			requireMergeRefused(t, inputs, opts, tt.msg)
		})
	}
}

func TestMergeIdentities(t *testing.T) {
	t.Parallel()

	inputs := inputsOf(generationScope(t, 1, nil)...)

	t.Run("omitted ids generated", func(t *testing.T) {
		t.Parallel()

		p := mustPlanMerge(t, inputs, generationOpts())
		assert.False(t, p.packID.IsZero())
		assert.NotContains(t, []UUID{memA, seg1, {0xC1}}, p.packID)
		require.NotNil(t, p.meta.ReplacementSetID)
		assert.False(t, p.meta.ReplacementSetID.IsZero())
		assert.NotEqual(t, setA, *p.meta.ReplacementSetID)
		assert.NotEqual(t, p.packID, *p.meta.ReplacementSetID)
	})

	t.Run("given ids kept", func(t *testing.T) {
		t.Parallel()

		opts := generationOpts()
		opts.PackID, opts.ReplacementSetID = UUID{0xAC}, UUID{0x5E, 0xE}
		p := mustPlanMerge(t, inputs, opts)
		assert.Equal(t, UUID{0xAC}, p.packID)
		assert.Equal(t, &UUID{0x5E, 0xE}, p.meta.ReplacementSetID)
	})

	t.Run("any epoch without an archive input", func(t *testing.T) {
		t.Parallel()

		seg := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
		opts := mergeOpts()
		opts.PublisherEpoch = 0
		mustPlanMerge(t, inputsOf(seg), opts)
	})
}

// TestMergeDerivesScopeFromInputs merges segments without naming their scope:
// the archive takes the capture and the UTC hour of its inputs.
func TestMergeDerivesScopeFromInputs(t *testing.T) {
	t.Parallel()

	capture := UUID{0xCA, 0x5C}
	next := blockTestHour + hourNs
	inNextHour := func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = next+hourNs/4, next+hourNs/2 }
	a := withIDs(t, mergePack(t, seg0, inNextHour, seqSteps(10, 13, 2, next)), seg0, capture)
	b := withIDs(t, mergePack(t, seg1, inNextHour, seqSteps(20, 23, 2, next)), seg1, capture)

	out, rep := mustMerge(t, inputsOf(a, b), mergeOpts())
	assert.Equal(t, uint64(8), rep.Records)
	h := mustOpen(t, out, ReaderOptions{}).Header()
	assert.Equal(t, capture, h.CaptureID, "the inputs' capture")
	assert.Equal(t, [2]int64{next, next + hourNs}, [2]int64{h.Meta.PeriodStart, h.Meta.PeriodEnd}, "the inputs' UTC hour")
	assert.Equal(t, []UUID{seg0, seg1}, h.Meta.CompactedFrom)
	assert.Nil(t, h.Meta.Supersedes)
}

// TestMergeRefusesRole merges, beside a valid segment, an input whose pack_role is neither segment nor archive,
// or whose scope_generation contradicts its role (the tracepack storage specification §2 and §4 Merge):
// Merge refuses it with ErrMergeInput naming the input, before it touches dst,
// whether it is the first input or the second.
// MergeIterate given the same packs reads every one of them (the tracepack storage specification §8).
func TestMergeRefusesRole(t *testing.T) {
	t.Parallel()

	good := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	other := func(id UUID, edit func(m *PackMeta)) []byte {
		return mergePack(t, id, edit, hourSteps(20, 23, 2))
	}
	// No writer writes pack_role 5 or 9, nor, for new output, 4,
	// so those packs are segments with the role byte set in place;
	// the segment for role 4 already lists the supersedes a repair requires.
	listing := other(patP1, func(m *PackMeta) { m.Supersedes = []UUID{{0xD0}} })
	tests := []struct {
		name string
		file []byte
		id   UUID
		msg  string
	}{
		{name: "repair", file: setMetaRole(t, listing, uint8(PackRoleRepair)), id: patP1,
			msg: "pack_role repair is neither segment nor archive"},
		{name: "retired", file: setMetaRole(t, other(seg1, nil), packRoleRetired), id: seg1,
			msg: "pack_role unknown(5) is neither segment nor archive"},
		{name: "unknown", file: setMetaRole(t, other(seg1, nil), 9), id: seg1,
			msg: "pack_role unknown(9) is neither segment nor archive"},
		{name: "unknown zero", file: setMetaRole(t, other(seg1, nil), 0), id: seg1,
			msg: "pack_role unknown is neither segment nor archive"},
		{name: "extract", file: other(seg1, func(m *PackMeta) {
			m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("all")
		}), id: seg1, msg: "pack_role extract is neither segment nor archive"},
		{name: "a segment at generation 1", file: other(seg1, func(m *PackMeta) {
			m.ScopeGeneration, m.PublisherEpoch = new(uint64(1)), new(uint64(1))
			m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(setB), new(uint64(1)), new(uint64(0))
		}), id: seg1, msg: "pack_role segment at scope_generation 1, not 0"},
		{name: "an archive at generation 0", file: other(memA, func(m *PackMeta) {
			m.PackRole = PackRoleArchive
		}), id: memA, msg: "pack_role archive at scope_generation 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requireConsistent(t, tt.file)
			second := "input 1, pack " + tt.id.String() + ": " + tt.msg
			requireMergeRefused(t, inputsOf(good, tt.file), mergeOpts(), second, ErrMergeInput)
			first := "input 0, pack " + tt.id.String() + ": " + tt.msg
			requireMergeRefused(t, inputsOf(tt.file, good), mergeOpts(), first, ErrMergeInput)

			readers := []*Reader{mustOpen(t, good, ReaderOptions{}), mustOpen(t, tt.file, ReaderOptions{})}
			items, _, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: OrderCapture})
			require.NoError(t, err)
			seqs := make([]uint64, 0, len(items))
			packs := make(map[int]bool)
			for _, it := range items {
				seqs, packs[it.pack] = append(seqs, it.rec.Seq), true
			}
			assert.Equal(t, []uint64{10, 11, 12, 13, 20, 21, 22, 23}, seqs, "a read yields every record of both packs")
			assert.Equal(t, map[int]bool{0: true, 1: true}, packs, "a read reads both packs")
		})
	}
}

// TestMergeLineage merges three archives of distinct scope_generation values, given in the order 2, 3, 1,
// with disjoint records and compacted_from lists that share packs, and two segments:
// supersedes lists every archive input in input order,
// and compacted_from the inputs' lists in input order and then the segment inputs, each pack at its first occurrence
// (the tracepack storage specification §4 Merge).
// Segments alone leave supersedes absent,
// and archives without compacted_from, a converter's, leave it absent at level 1 and again at level 2.
func TestMergeLineage(t *testing.T) {
	t.Parallel()

	archive := func(id, set UUID, gen uint64, from []UUID, first uint64) []byte {
		return mergePack(t, id, func(m *PackMeta) { generationMeta(m, set, 1, gen, from) }, hourSteps(first, first+3, 2))
	}
	g2 := archive(memB, setB, 2, []UUID{{0xC1}, {0xC2}}, 10)
	g3 := archive(memC, setC, 3, []UUID{{0xC2}, {0xC3}, seg1}, 20)
	g1 := archive(memA, setA, 1, []UUID{{0xC1}, {0xC4}}, 30)
	s0 := mergePack(t, seg0, nil, hourSteps(40, 43, 2))
	s1 := mergePack(t, seg1, nil, hourSteps(50, 53, 2))

	t.Run("archives and segments", func(t *testing.T) {
		t.Parallel()

		out, rep := mustMerge(t, inputsOf(g2, s0, g3, g1, s1), mergeOpts())
		assert.Equal(t, uint64(20), rep.Records, "every record of every input")
		m := mustOpen(t, out, ReaderOptions{}).Header().Meta
		assert.Equal(t, []UUID{memB, memC, memA}, m.Supersedes, "every archive input, in input order")
		assert.Equal(t, []UUID{{0xC1}, {0xC2}, {0xC3}, seg1, {0xC4}, seg0}, m.CompactedFrom,
			"the inputs' lists in input order, then the segment inputs, each pack once")
		assert.Equal(t, uint8(2), m.CompactionLevel)
	})

	t.Run("segments only", func(t *testing.T) {
		t.Parallel()

		out, _ := mustMerge(t, inputsOf(s1, s0), mergeOpts())
		m := mustOpen(t, out, ReaderOptions{}).Header().Meta
		assert.Nil(t, m.Supersedes)
		assert.Equal(t, []UUID{seg1, seg0}, m.CompactedFrom)
	})

	t.Run("converter archives, levels 1 then 2", func(t *testing.T) {
		t.Parallel()

		converter := func(id, set UUID, first uint64) []byte {
			return mergePack(t, id, func(m *PackMeta) {
				generationMeta(m, set, 1, 1, nil)
				m.CompactionLevel = 0
			}, hourSteps(first, first+3, 2))
		}
		opts := mergeOpts()
		opts.ScopeGeneration, opts.PackID, opts.ReplacementSetID = 2, memC, setC
		first, _ := mustMerge(t, inputsOf(converter(memA, setA, 10), converter(memB, setB, 20)), opts)
		opts.ScopeGeneration, opts.PackID, opts.ReplacementSetID = 3, memD, setD
		second, _ := mustMerge(t, inputsOf(first), opts)

		for i, tt := range []struct {
			file       []byte
			supersedes []UUID
		}{{first, []UUID{memA, memB}}, {second, []UUID{memC}}} {
			m := mustOpen(t, tt.file, ReaderOptions{}).Header().Meta
			assert.Nil(t, m.CompactedFrom, "level %d", i+1)
			assert.Equal(t, uint8(i+1), m.CompactionLevel)
			assert.Equal(t, tt.supersedes, m.Supersedes, "level %d", i+1)
		}
	})
}

// TestMergeGenerationValues checks that the archive takes MergeOptions.ScopeGeneration and PublisherEpoch as given,
// whatever the generation values of its archive inputs, and that a ScopeGeneration of 0 is refused.
func TestMergeGenerationValues(t *testing.T) {
	t.Parallel()

	input := mergePack(t, memA, func(m *PackMeta) { generationMeta(m, setA, 5, 7, []UUID{seg0}) }, hourSteps(10, 13, 2))
	tests := []struct {
		name       string
		epoch, gen uint64
	}{
		{name: "above the input's", epoch: 6, gen: 8},
		{name: "equal to the input's", epoch: 5, gen: 7},
		{name: "below the input's", epoch: 1, gen: 2},
		{name: "a lower epoch with a higher generation", epoch: 4, gen: 9},
		{name: "a higher epoch with a lower generation", epoch: 6, gen: 1},
		{name: "epoch 0", epoch: 0, gen: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			opts.PublisherEpoch, opts.ScopeGeneration = tt.epoch, tt.gen
			out, _ := mustMerge(t, inputsOf(input), opts)
			m := mustOpen(t, out, ReaderOptions{}).Header().Meta
			assert.Equal(t, new(tt.gen), m.ScopeGeneration)
			assert.Equal(t, new(tt.epoch), m.PublisherEpoch)
			assert.Equal(t, []UUID{memA}, m.Supersedes)
		})
	}

	t.Run("generation 0", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.ScopeGeneration = 0
		requireMergeRefused(t, inputsOf(input), opts, "ScopeGeneration is 0")
	})
}

// TestMergeInputSet merges input sets Merge refuses before it touches dst:
// none (a plain error), a pack given twice, packs of two captures or of two UTC hours (each ErrMergeInput),
// and a supplied PackID or ReplacementSetID that an input already uses (a plain error).
func TestMergeInputSet(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	next := blockTestHour + hourNs
	nextHour := mergePack(t, seg1, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = next, next+hourNs }, seqSteps(20, 23, 2, next))
	archive := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 1, 2, []UUID{{0xC1}})
		m.Supersedes = []UUID{memB}
	}, hourSteps(30, 33, 2))

	t.Run("no inputs", func(t *testing.T) {
		t.Parallel()

		requireMergeRefused(t, nil, mergeOpts(), "no inputs")
	})

	refused := []struct {
		name   string
		inputs []MergeInput
		msg    string
	}{
		{name: "one input twice", inputs: inputsOf(a, b, a), msg: "input 2, pack " + seg0.String() + ": the pack_id of input 0"},
		{name: "a repeated pack_id", inputs: inputsOf(a, withIDs(t, b, seg0, viewCapture)),
			msg: "input 1, pack " + seg0.String() + ": the pack_id of input 0"},
		{name: "two captures", inputs: inputsOf(a, withIDs(t, b, seg1, UUID{0xCC})),
			msg: "input 1, pack " + seg1.String() + ": capture_id " + UUID{0xCC}.String() + ", not input 0's " + viewCapture.String()},
		{name: "two hours", inputs: inputsOf(a, nextHour),
			msg: "input 1, pack " + seg1.String() + fmt.Sprintf(": period %d to %d is not inside input 0's UTC hour %d", next, next+hourNs, mergeHour)},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requireMergeRefused(t, tt.inputs, mergeOpts(), tt.msg, ErrMergeInput)
		})
	}

	ids := []struct {
		name string
		edit func(o *MergeOptions)
		msg  string
	}{
		{name: "the pack_id of a segment input", edit: func(o *MergeOptions) { o.PackID = seg1 }, msg: "PackID"},
		{name: "the pack_id of an archive input", edit: func(o *MergeOptions) { o.PackID = memA }, msg: "PackID"},
		{name: "a pack of an input's compacted_from", edit: func(o *MergeOptions) { o.PackID = UUID{0xC1} }, msg: "PackID"},
		{name: "a pack of an input's supersedes", edit: func(o *MergeOptions) { o.PackID = memB }, msg: "PackID"},
		{name: "the replacement set of an archive input", edit: func(o *MergeOptions) { o.ReplacementSetID = setA }, msg: "ReplacementSetID"},
	}
	for _, tt := range ids {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			tt.edit(&opts)
			requireMergeRefused(t, inputsOf(archive, b), opts, tt.msg)
		})
	}

	// Only an archive input's replacement_set_id binds the archive's;
	// a segment's, a tag the role does not require, does not.
	t.Run("the replacement_set_id a segment carries", func(t *testing.T) {
		t.Parallel()

		tagged := mergePack(t, seg2, func(m *PackMeta) {
			m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(setC), new(uint64(1)), new(uint64(0))
		}, hourSteps(40, 43, 2))
		opts := mergeOpts()
		opts.ReplacementSetID = setC
		out, _ := mustMerge(t, inputsOf(archive, tagged), opts)
		assert.Equal(t, new(setC), mustOpen(t, out, ReaderOptions{}).Header().Meta.ReplacementSetID)
	})
}

func TestMergeInputChecks(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	good := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	period := func(start, end int64) func(m *PackMeta) {
		return func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = start, end }
	}
	// Records of seqs 10 to 13 in the hour after the period's, in blocks the footer indexes correctly.
	nextHour := mergePack(t, seg0, nil, seqSteps(10, 13, 2, h+hourNs))
	// Block 0 in the period's hour, block 1 in the next.
	twoHours := mergePack(t, seg0, nil, append(hourSteps(10, 11, 2), seqSteps(12, 13, 2, h+hourNs)...))
	// The Writer refuses a classified record without classifier, so the fixture hides the tag after writing:
	// retagged to a private tag, it reads back as an unknown entry.
	unclassified := retagMeta(t, mergePack(t, seg1, nil, hourSteps(20, 23, 2)), tagClassifier, 0x8008)
	frameLen := func(m *PackMeta) { m.MaxFrameLens = []uint64{4096} }
	oversized := mergePack(t, seg0, frameLen, oversizedSteps(10, 13, 2))
	// The Writer refuses an oversized record without max_frame_len, so the fixture hides the tag the same way.
	unbounded := retagMeta(t, mergePack(t, seg1, frameLen, oversizedSteps(20, 23, 2)), tagMaxFrameLen, 0x800F)
	redacted := reindexed(t, editBlock(t, mergePack(t, seg0, nil, hourSteps(10, 13, 2)), 1, func(b *testBlock) {
		b.headers[0].Quality |= uint16(QualityRedacted)
	}))
	lying := withSeqStart(t, mergePack(t, seg1, nil, hourSteps(20, 23, 2)), 19)
	for name, file := range map[string][]byte{
		"next hour": nextHour, "two hours": twoHours, "unclassified": unclassified, "unbounded": unbounded,
		"redacted": redacted, "lying seq_start": lying,
	} {
		t.Run("fixture "+name, func(t *testing.T) {
			t.Parallel()

			requireConsistent(t, file)
			require.NoError(t, mustOpen(t, file, ReaderOptions{}).Header().FooterErr)
		})
	}

	type vector struct {
		name  string
		files [][]byte
		// inputs, when set, are the vector's inputs; else files, in their order.
		inputs []MergeInput
		// cause, when set, is an error the input error wraps.
		cause error
		msg   string
	}
	sole := func(name string, file []byte, msg string) vector {
		return vector{name: name, files: [][]byte{file}, msg: msg}
	}
	flipped := flipByte(good, 20)
	tests := []vector{
		sole("not finalized", withIDs(t, writeRepairPack(t, CodecZstd, nil, true, hourSteps(10, 13, 2)).file, seg0, viewCapture), "not finalized"),
		sole("no valid footer", invalidFooterFile(t, good), "no valid footer"),
		sole("an empty period", mergePack(t, seg0, period(h, h), nil), "is empty or not inside one UTC hour"),
		sole("a period over two hours", mergePack(t, seg0, period(h+hourNs/2, h+3*hourNs/2), nil), "is empty or not inside one UTC hour"),
		sole("a correctly indexed block in another hour", nextHour, "block 0 at offset"),
		sole("blocks in two hours", twoHours, "block 1 at offset"),
		sole("a redacted record", redacted, "a redacted record without redaction_policy in the pack metadata"),
		sole("seq_start before the first record", lying, "seq_start 19, but its first record's seq is 20"),
		{name: "a borrowed classifier", files: [][]byte{good, unclassified},
			msg: "input 1, pack " + seg1.String() + ": a classified record without classifier"},
		{name: "a borrowed max_frame_len", files: [][]byte{oversized, unbounded},
			msg: "input 1, pack " + seg1.String() + ": an oversized record without max_frame_len"},
		{name: "a later input's seq_start", files: [][]byte{good, lying},
			msg: "input 1, pack " + seg1.String() + ": seq_start 19"},
		{name: "compaction_level 255", files: generationScope(t, math.MaxUint8, nil),
			msg: "input 0, pack " + memA.String() + ": compaction_level 255"},
		// The input's pack_id is not known when its file header cannot be read, so the error names its position only.
		{name: "an unreadable file header", inputs: []MergeInput{{ReaderAt: bytes.NewReader(flipped), Size: int64(len(flipped))}},
			cause: ErrChecksum, msg: "input 0: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			inputs := tt.inputs
			if inputs == nil {
				inputs = inputsOf(tt.files...)
			}
			// Every refusal of an input is an input error.
			want := []error{ErrMergeInput}
			if tt.cause != nil {
				want = append(want, tt.cause)
			}
			requireMergeRefused(t, inputs, mergeOpts(), tt.msg, want...)
		})
	}
}

func TestMergeInputOverReaderBudget(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	// The zstd footers are shorter on disk than decoded, so a budget between the two passes the read and fails the decode.
	la, lb := layoutOf(t, a).tr, layoutOf(t, b).tr
	onDisk := max(la.FooterLen, lb.FooterLen)
	require.Less(t, onDisk, min(la.FooterUncompressedLen, lb.FooterUncompressedLen))
	// The pack metadata's budget fails Open, before the input's pack_id is known;
	// a footer's leaves the pack open, without a footer index.
	tests := []struct {
		name   string
		budget ReaderOptions
		input  string
		msg    string
	}{
		{name: "pack metadata", budget: ReaderOptions{MaxPackMetadataLen: 8}, input: "input 0: ", msg: "exceed MaxPackMetadataLen"},
		{name: "footer on disk", budget: ReaderOptions{MaxFooterLen: 16}, input: "input 0, pack " + seg0.String(), msg: "footer_len"},
		{name: "decoded footer", budget: ReaderOptions{MaxFooterLen: int64(onDisk)}, input: "input 0, pack " + seg0.String(),
			msg: "footer_uncompressed_len"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			opts.Reader = tt.budget
			var dst touchWriter
			rep, err := Merge(t.Context(), &dst, inputsOf(a, b), opts)
			// A budget is not the input's defect, so the error is not an input error.
			require.ErrorIs(t, err, ErrReadLimit)
			require.NotErrorIs(t, err, ErrMergeInput)
			require.ErrorContains(t, err, tt.input)
			require.ErrorContains(t, err, tt.msg+" ")
			assert.Zero(t, dst.calls, "dst untouched")
			assert.Equal(t, MergeReport{}, rep)
		})
	}
}

// TestMergeInputSharedF3ListsOverReaderBudget merges an input whose 8 blocks share one kind_counts array (sharedCountsF3Pack)
// under a reader budget that holds its footer but not its blocks' lists, each counted once per block:
// the merge fails before it touches dst with ErrReadLimit naming the input, as for any input over a reader budget.
// Under a budget that holds the lists the input merges.
func TestMergeInputSharedF3ListsOverReaderBudget(t *testing.T) {
	t.Parallel()

	file, lists := sharedCountsF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, 8, 1)), 4<<10)
	l := layoutOf(t, file).tr
	require.Less(t, max(l.FooterLen, l.FooterUncompressedLen), lists-1, "the budget holds the footer")
	opts := mergeOpts()
	opts.Reader.MaxFooterLen = int64(lists) - 1
	requireMergeRefused(t, inputsOf(file), opts, "input 0, pack "+seg0.String(), ErrReadLimit)
	_, _, err := mergeBytes(t.Context(), inputsOf(file), opts)
	require.ErrorContains(t, err, "the F-3 lists of blocks 0 to 7 ")

	opts.Reader.MaxFooterLen = int64(lists)
	_, rep := mustMerge(t, inputsOf(file), opts)
	assert.Equal(t, uint64(8), rep.Records)
}

func TestMergeInputReadFailures(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))

	t.Run("a failed read", func(t *testing.T) {
		t.Parallel()

		failing := newGatedReader(b)
		failing.fail = func(int64, int) bool { return true }
		inputs := inputsOf(a, b)
		inputs[1].ReaderAt = failing
		// A failed read is not the input's defect: a caller retries it instead of repairing the pack.
		// It fails the file header's read, before the input's pack_id is known.
		requireMergeRefused(t, inputs, mergeOpts(), "input 1: ", errInjected)
	})

	t.Run("an object shorter than its size", func(t *testing.T) {
		t.Parallel()

		inputs := inputsOf(a, b)
		inputs[1].Size += 100
		requireMergeRefused(t, inputs, mergeOpts(), "input 1: ", ErrMergeInput, io.ErrUnexpectedEOF)
	})
}

func TestMergeCancelledBeforeWriting(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var dst touchWriter
	_, err := Merge(ctx, &dst, inputsOf(a), mergeOpts())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrMergeInput)
	assert.Zero(t, dst.calls)
}

func TestMergeAcceptsSameHourRecordsOutsideTheSegmentPeriod(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	// A same-hour clock step puts records before the segment's flush interval.
	seg := mergePack(t, seg0, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = h+hourNs/2, h+2*hourNs/3 }, hourSteps(10, 13, 2))
	p := mustPlanMerge(t, inputsOf(seg), mergeOpts())
	assert.Equal(t, [2]int64{h, h + hourNs}, [2]int64{p.meta.PeriodStart, p.meta.PeriodEnd}, "the archive's period is the hour")
}

// TestMergeSameHourClockStepRead merges two segments of one UTC hour whose flush periods a backward clock step crosses
// (the tracepack storage specification §8):
// the second segment's records, seqs after the first's, carry timestamps before the first's and before its own period.
// In both input orders the merge succeeds,
// and MergeIterate over the segments yields the records the archive yields, in the same order,
// in capture order as Iterate reads the archive, and in time order as MergeIterate reads the archive alone,
// each read complete.
func TestMergeSameHourClockStepRead(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	minute := int64(60_000_000_000)
	early := mergePack(t, seg0, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = h+10*minute, h+20*minute },
		seqSteps(10, 13, 2, h+10*minute))
	// The clock stepped back 15 minutes at the flush that opened the second segment.
	late := mergePack(t, seg1, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = h+20*minute, h+30*minute },
		seqSteps(14, 17, 2, h+5*minute))

	records := func(items []iterItem) []Record {
		out := make([]Record, len(items))
		for i := range items {
			out[i] = items[i].rec
		}

		return out
	}
	read := func(t *testing.T, readers []*Reader, order Order) []Record {
		t.Helper()

		items, res, err := collectMergeIterate(t.Context(), readers, Query{}, MergeIterateOptions{Order: order})
		require.NoError(t, err)
		assert.Empty(t, res.Incomplete, "a complete read")

		return records(items)
	}

	for name, files := range map[string][][]byte{"early first": {early, late}, "late first": {late, early}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, rep := mustMerge(t, inputsOf(files...), mergeOpts())
			require.Equal(t, uint64(8), rep.Records)
			archive := mustOpen(t, out, ReaderOptions{})
			segments := []*Reader{mustOpen(t, files[0], ReaderOptions{}), mustOpen(t, files[1], ReaderOptions{})}

			run := iterate(t, archive, Query{})
			assert.Empty(t, run.res.Incomplete, "a complete read")
			bySeq := read(t, segments, OrderCapture)
			assert.Equal(t, records(run.items), bySeq, "capture order")
			assert.Equal(t, []uint64{10, 11, 12, 13, 14, 15, 16, 17}, run.seqs())

			byTime := read(t, segments, OrderTime)
			assert.Equal(t, read(t, []*Reader{archive}, OrderTime), byTime, "time order")
			seqs := make([]uint64, len(byTime))
			for i := range byTime {
				seqs[i] = byTime[i].Seq
			}
			assert.Equal(t, []uint64{14, 15, 16, 17, 10, 11, 12, 13}, seqs, "the stepped records first")
		})
	}
}

// captureMeta gives m a value in every capture-level tag (the tracepack storage specification §4 Merge).
func captureMeta(m *PackMeta) {
	m.ToolID, m.Transport, m.CaptureMethod, m.Vantage = "tool", TransportHSMSSS, CaptureMethodLog, VantageHost
	m.Recorder, m.TimeSource, m.LifecycleCoverage, m.QualityEvaluated = "rec", TimeSourceCaptureClock, LifecycleCoverageSubscribed, true
	m.RecorderInstanceID = UUID{0x1A}
	m.CaptureOriginUTCNs, m.CaptureOriginMonoNs, m.ClockStepToleranceNs = new(blockTestHour), new(int64(5)), new(uint64(1000))
	m.PreviousCaptureID = new(UUID{0xCB})
	m.SiteID, m.EquipmentModel, m.EquipmentSWRev = new("fab"), new("model"), new("rev")
	m.HostSoftware, m.HostEndpoint, m.EquipmentEndpoint = new("eap"), new("10.0.0.1:5000"), new("10.0.0.2:5000")
	m.EquipmentConnectMode, m.DeviceID = new(SocketRolePassive), new(uint64(7))
	m.HSMSTimers = &HSMSTimers{T1: new(uint64(1)), T3: new(uint64(45_000))}
	m.SourceTZ, m.SourceDialect, m.SourceRefs = new("UTC"), new("dialect"), []string{"a.log", "b.log"}
}

func TestMergeCaptureLevelTags(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, captureMeta, hourSteps(10, 13, 2))
	tests := []struct {
		tag  string
		name string
		edit func(m *PackMeta)
	}{
		{"tool_id", "", func(m *PackMeta) { m.ToolID = "tool2" }},
		{"transport", "", func(m *PackMeta) { m.Transport = TransportSECS1Normalised }},
		{"capture_method", "", func(m *PackMeta) { m.CaptureMethod = CaptureMethodRawStream }},
		{"vantage", "", func(m *PackMeta) { m.Vantage = VantageEquipment }},
		{"recorder", "", func(m *PackMeta) { m.Recorder = "rec2" }},
		{"time_source", "", func(m *PackMeta) { m.TimeSource = TimeSourceGenerator }},
		{"lifecycle_coverage", "", func(m *PackMeta) { m.LifecycleCoverage = LifecycleCoverageNone }},
		{"quality_evaluated", "", func(m *PackMeta) { m.QualityEvaluated = false }},
		{"recorder_instance_id", "", func(m *PackMeta) { m.RecorderInstanceID = UUID{0x1B} }},
		{"capture_origin_utc_ns", "", func(m *PackMeta) { *m.CaptureOriginUTCNs++ }},
		{"capture_origin_mono_ns", "", func(m *PackMeta) { *m.CaptureOriginMonoNs++ }},
		{"clock_step_tolerance_ns", "", func(m *PackMeta) { *m.ClockStepToleranceNs++ }},
		{"previous_capture_id", "", func(m *PackMeta) { m.PreviousCaptureID = new(UUID{0xCD}) }},
		{"previous_capture_id", "absent", func(m *PackMeta) { m.PreviousCaptureID = nil }},
		{"site_id", "", func(m *PackMeta) { m.SiteID = new("fab2") }},
		{"site_id", "absent", func(m *PackMeta) { m.SiteID = nil }},
		{"equipment_model", "", func(m *PackMeta) { m.EquipmentModel = new("model2") }},
		{"equipment_sw_rev", "", func(m *PackMeta) { m.EquipmentSWRev = new("rev2") }},
		{"host_software", "", func(m *PackMeta) { m.HostSoftware = new("eap2") }},
		{"host_endpoint", "", func(m *PackMeta) { m.HostEndpoint = new("10.0.0.1:5001") }},
		{"equipment_endpoint", "", func(m *PackMeta) { m.EquipmentEndpoint = new("10.0.0.2:5001") }},
		{"equipment_connect_mode", "", func(m *PackMeta) { m.EquipmentConnectMode = new(SocketRoleActive) }},
		{"device_id", "", func(m *PackMeta) { *m.DeviceID++ }},
		{"hsms_timers", "a timer changed", func(m *PackMeta) { *m.HSMSTimers.T3++ }},
		{"hsms_timers", "a timer added", func(m *PackMeta) { m.HSMSTimers.T8 = new(uint64(8)) }},
		{"hsms_timers", "absent", func(m *PackMeta) { m.HSMSTimers = nil }},
		{"hsms_timers", "present without timers", func(m *PackMeta) { m.HSMSTimers = &HSMSTimers{} }},
		{"source_tz", "", func(m *PackMeta) { m.SourceTZ = new("Asia/Taipei") }},
		{"source_dialect", "", func(m *PackMeta) { m.SourceDialect = new("dialect2") }},
		{"source_ref", "reordered", func(m *PackMeta) { m.SourceRefs = []string{"b.log", "a.log"} }},
		{"source_ref", "with duplicates", func(m *PackMeta) { m.SourceRefs = []string{"a.log", "a.log", "b.log"} }},
		{"source_ref", "extended", func(m *PackMeta) { m.SourceRefs = []string{"a.log", "b.log", "c.log"} }},
	}

	covered := make(map[string]bool)
	for _, tt := range tests {
		covered[tt.tag] = true
		name := tt.tag
		if tt.name != "" {
			name += " " + tt.name
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			b := mergePack(t, seg1, func(m *PackMeta) {
				captureMeta(m)
				tt.edit(m)
			}, hourSteps(20, 23, 2))
			requireMergeRefused(t, inputsOf(a, b), mergeOpts(),
				"input 1, pack "+seg1.String()+": capture-level tag "+tt.tag+" differs from pack "+seg0.String(), ErrMergeInput)
		})
	}
	for _, tag := range captureTags {
		assert.True(t, covered[tag.name], "no vector for %s", tag.name)
	}

	t.Run("hsms_timers absent and present without timers", func(t *testing.T) {
		t.Parallel()

		absent := mergePack(t, seg0, func(m *PackMeta) {
			captureMeta(m)
			m.HSMSTimers = nil
		}, hourSteps(10, 13, 2))
		empty := mergePack(t, seg1, func(m *PackMeta) {
			captureMeta(m)
			m.HSMSTimers = &HSMSTimers{}
		}, hourSteps(20, 23, 2))
		requireMergeRefused(t, inputsOf(absent, empty), mergeOpts(), "capture-level tag hsms_timers differs", ErrMergeInput)
	})
}

// TestMergeCarriesTheCaptureLevelTags checks that inputs with equal capture-level tags are merged
// and that the archive carries them; an hsms_timers nested entry a reader does not know is neither compared nor carried.
func TestMergeCarriesTheCaptureLevelTags(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, captureMeta, hourSteps(10, 13, 2))
	// b's hsms_timers holds T1, T3 and an unknown nested entry, tag 9: its T8 retagged in place.
	b := patchMeta(t, mergePack(t, seg1, func(m *PackMeta) {
		captureMeta(m)
		m.HSMSTimers.T8 = new(uint64(8))
	}, hourSteps(20, 23, 2)), func(meta []byte) {
		list, err := tlv.Decode(meta)
		require.NoError(t, err)
		timers := nthEntry(t, list, tagHSMSTimers, 0)
		nested, err := tlv.Decode(timers.Value)
		require.NoError(t, err)
		// timers.Value aliases meta.
		binary.LittleEndian.PutUint16(timers.Value[nthEntry(t, nested, 8, 0).Offset:], 9)
	})
	bMeta := mustOpen(t, b, ReaderOptions{}).Header().Meta
	require.Equal(t, &HSMSTimers{T1: new(uint64(1)), T3: new(uint64(45_000))}, bMeta.HSMSTimers, "the retagged timer is unknown")

	p := mustPlanMerge(t, inputsOf(a, b), mergeOpts())
	want := &PackMeta{}
	captureMeta(want)
	m := p.meta
	for _, tag := range captureTags {
		assert.True(t, tag.equal(want, m), "the archive carries %s", tag.name)
	}
	raw, err := m.MarshalBinary()
	require.NoError(t, err)
	list, err := tlv.Decode(raw)
	require.NoError(t, err)
	nested, err := tlv.Decode(nthEntry(t, list, tagHSMSTimers, 0).Value)
	require.NoError(t, err)
	assert.Len(t, nested, 2, "the unknown timer entry is not carried")
}

// reorderCoverage returns file with the first coverage entry of its pack metadata re-encoded in place:
// its nested entries in reverse order, and every reserved byte of the entry and its nested entries set,
// an encoding of the same entry that no Writer writes.
func reorderCoverage(t testing.TB, file []byte) []byte {
	t.Helper()

	return patchMeta(t, file, func(meta []byte) {
		list, err := tlv.Decode(meta)
		require.NoError(t, err)
		e := nthEntry(t, list, tagCoverage, 0)
		nested, err := tlv.Decode(e.Value)
		require.NoError(t, err)

		var value []byte
		for k := len(nested) - 1; k >= 0; k-- {
			at := len(value)
			value = tlv.AppendEntry(value, nested[k])
			value[at+3] = 1
		}
		require.Len(t, value, len(e.Value))
		require.NotEqual(t, e.Value, value)
		// e.Value aliases meta.
		copy(e.Value, value)
		meta[e.Offset+3] = 1
	})
}

func TestMergeArchiveMetadata(t *testing.T) {
	t.Parallel()

	u1, u2 := RawEntry{Tag: 0x0050, Type: 7, Value: []byte{1}}, RawEntry{Tag: 0x0051, Type: 7, Value: []byte{2}}
	cov := func(first uint64, unknown ...RawEntry) Coverage {
		return Coverage{
			CaptureID: new(viewCapture), SeqFirst: new(first), SeqLast: new(first + 1),
			TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour + 1), Unknown: unknown,
		}
	}
	// notCarried gives m a value in every tag the archive does not carry.
	// The inputs stay unmasked, without redaction_policy or redaction entries, so their classifiers are a union.
	notCarried := func(m *PackMeta) {
		m.FlushIntervalNs, m.Notes, m.ExtractFilter = new(uint64(60_000_000_000)), new("note"), new("filter")
		m.Unknown = []RawEntry{{Tag: 0x8001, Type: 7, Value: []byte{1}}}
	}

	mem := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 2, 3, []UUID{{0xC1}, {0xC2}})
		m.CompactionLevel = 2
		m.Classifiers, m.MaxFrameLens = []string{"b", "a"}, []uint64{4096}
		m.Coverage = []Coverage{cov(1), cov(1), cov(3, u1, u2)}
		notCarried(m)
	}, hourSteps(10, 13, 2))
	// seg1's first coverage entry is mem's first, stored in another encoding; its second holds mem's unknown entries reordered.
	seg := reorderCoverage(t, mergePack(t, seg1, func(m *PackMeta) {
		m.Classifiers, m.MaxFrameLens = []string{"a", "c"}, []uint64{100, 4096}
		m.Coverage = []Coverage{cov(1), cov(3, u2, u1), cov(5)}
		notCarried(m)
	}, hourSteps(20, 23, 2)))
	// A second archive, at compaction_level 1, carrying the coverage of records it lost.
	other := mergePack(t, memB, func(m *PackMeta) {
		generationMeta(m, setB, 1, 1, []UUID{seg2})
		m.Coverage = []Coverage{cov(30)}
	}, hourSteps(30, 30, 2))
	files := [][]byte{mem, seg, other}

	opts := generationOpts()
	opts.ReplacementSetID = UUID{0x5E, 0xE}
	p := mustPlanMerge(t, inputsOf(files...), opts)
	m := p.meta

	assert.Equal(t, PackRoleArchive, m.PackRole)
	assert.Equal(t, new(uint64(1)), m.ScopeGeneration)
	assert.Equal(t, new(uint64(3)), m.PublisherEpoch)
	assert.Equal(t, new(UUID{0x5E, 0xE}), m.ReplacementSetID)
	assert.Equal(t, new(uint64(1)), m.ReplacementSetSize)
	assert.Equal(t, new(uint64(0)), m.ReplacementSetIndex)
	assert.Equal(t, [2]int64{blockTestHour, blockTestHour + hourNs}, [2]int64{m.PeriodStart, m.PeriodEnd})
	assert.Equal(t, uint8(3), m.CompactionLevel, "one above memA's level 2")
	assert.Equal(t, []UUID{{0xC1}, {0xC2}, seg2, seg1}, m.CompactedFrom)
	assert.Equal(t, []UUID{memA, memB}, m.Supersedes)
	assert.Equal(t, []Coverage{cov(1), cov(3, u1, u2), cov(3, u2, u1), cov(5), cov(30)}, m.Coverage,
		"every entry once, in input order, an entry of another encoding but the same nested entries dropped")
	assert.Equal(t, []string{"a", "b", "c"}, m.Classifiers)
	assert.Equal(t, []uint64{100, 4096}, m.MaxFrameLens)
	assert.Equal(t, "merge-test", m.Writer)
	assert.Equal(t, uint64(10), m.SeqStart, "the first record's seq")

	assert.Nil(t, m.FlushIntervalNs)
	assert.Nil(t, m.Notes)
	assert.Nil(t, m.PatchBase)
	assert.Nil(t, m.ExtractFilter)
	assert.Nil(t, m.Unknown)

	first := mustOpen(t, mem, ReaderOptions{}).Header().Meta
	for _, tag := range captureTags {
		assert.True(t, tag.equal(first, m), "the archive carries %s", tag.name)
	}
	assert.Equal(t, viewCapture, p.captureID)
	assert.Equal(t, PackFacts{AnyClassified: true, GenerationHasPredecessor: true}, p.facts)
}

func TestMergeArchivePackMetadataBudget(t *testing.T) {
	t.Parallel()

	// coverage returns n distinct coverage entries, one per seq from first.
	coverage := func(first uint64, n int) []Coverage {
		out := make([]Coverage, 0, n)
		for k := range n {
			seq := first + uint64(k)
			out = append(out, Coverage{
				CaptureID: new(viewCapture), SeqFirst: new(seq), SeqLast: new(seq),
				TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour),
			})
		}

		return out
	}
	a := mergePack(t, seg0, func(m *PackMeta) { m.Coverage = coverage(100, 20) }, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, func(m *PackMeta) { m.Coverage = coverage(200, 20) }, hourSteps(20, 23, 2))
	inputs := inputsOf(a, b)

	// The default budget accepts ordinary inputs.
	raw, err := mustPlanMerge(t, inputs, mergeOpts()).meta.MarshalBinary()
	require.NoError(t, err)
	archiveLen := int64(len(raw))
	inputLen := int64(max(layoutHeader(t, a).PackMetadataLen, layoutHeader(t, b).PackMetadataLen))
	require.Greater(t, archiveLen-1, inputLen, "the union of the inputs' coverage outgrows each input's pack metadata")

	withBudget := func(n int64) MergeOptions {
		opts := mergeOpts()
		opts.Reader.MaxPackMetadataLen = n

		return opts
	}
	t.Run("exactly the archive's length", func(t *testing.T) {
		t.Parallel()

		mustPlanMerge(t, inputs, withBudget(archiveLen))
	})
	for name, budget := range map[string]int64{"one byte less": archiveLen - 1, "the longest input's length": inputLen} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			opts := withBudget(budget)
			requireMergeRefused(t, inputs, opts,
				fmt.Sprintf("the archive's pack metadata of %d bytes exceeds MaxPackMetadataLen %d", archiveLen, budget), ErrMergeLimit)
			_, err := planMerge(t.Context(), inputs, &opts)
			assert.NotErrorIs(t, err, ErrReadLimit, "every input fits the budget")
		})
	}
}

func TestMergeArchiveLineageWithoutGenerationZeroPacks(t *testing.T) {
	t.Parallel()

	// A converter's archive: at compaction_level 0 without compacted_from.
	converter := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 1, 1, nil)
		m.CompactionLevel = 0
	}, hourSteps(10, 13, 2))

	opts := mergeOpts()
	opts.ScopeGeneration = 2
	m := mustPlanMerge(t, inputsOf(converter), opts).meta
	assert.Nil(t, m.CompactedFrom, "absent when the lineage list is empty")
	assert.Equal(t, uint8(1), m.CompactionLevel)
	assert.Equal(t, []UUID{memA}, m.Supersedes)

	seg := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	m = mustPlanMerge(t, inputsOf(seg), mergeOpts()).meta
	assert.Nil(t, m.Supersedes, "absent without an archive input")
	assert.Equal(t, []UUID{seg0}, m.CompactedFrom)
	assert.Equal(t, uint8(1), m.CompactionLevel)
}

func TestMergeArchiveSeqStartWithoutRecords(t *testing.T) {
	t.Parallel()

	empty := func(id UUID, seq uint64, edit func(m *PackMeta)) []byte {
		return mergePack(t, id, func(m *PackMeta) {
			m.SeqStart = seq
			if edit != nil {
				edit(m)
			}
		}, nil)
	}
	files := [][]byte{
		empty(seg0, 5, nil),
		empty(seg1, 9, nil),
		// An archive holding only coverage.
		empty(memA, 7, func(m *PackMeta) {
			generationMeta(m, setA, 1, 1, []UUID{{0xD0}})
			m.Coverage = []Coverage{{CaptureID: new(viewCapture), SeqFirst: new(uint64(7)), TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour)}}
		}),
	}
	p := mustPlanMerge(t, inputsOf(files...), mergeOpts())
	assert.Empty(t, p.clusters)
	assert.Equal(t, uint64(9), p.meta.SeqStart, "the largest seq_start of the inputs")
	assert.Len(t, p.meta.Coverage, 1)
}

func TestMergeArchivePeriodAtTheInt64Extremes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		start, end int64
		want       [2]int64
	}{
		{name: "the first hour", start: math.MinInt64, end: math.MinInt64 + 1,
			want: [2]int64{math.MinInt64, (hourOf(math.MinInt64) + 1) * hourNs}},
		{name: "the last hour", start: math.MaxInt64 - 1, end: math.MaxInt64,
			want: [2]int64{hourOf(math.MaxInt64-1) * hourNs, math.MaxInt64}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seg := mergePack(t, seg0, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = tt.start, tt.end }, nil)
			m := mustPlanMerge(t, inputsOf(seg), mergeOpts()).meta
			assert.Equal(t, tt.want, [2]int64{m.PeriodStart, m.PeriodEnd})
			assert.Less(t, m.PeriodEnd-m.PeriodStart, hourNs, "a bound was clamped")
			hour, ok := periodHour(m.PeriodStart, m.PeriodEnd)
			assert.True(t, ok)
			assert.Equal(t, hourOf(tt.start), hour, "the input's hour")
		})
	}
}

func TestMergeKeepsANilCapture(t *testing.T) {
	t.Parallel()

	seg := withIDs(t, mergePack(t, seg0, nil, hourSteps(10, 13, 2)), seg0, UUID{})
	p := mustPlanMerge(t, inputsOf(seg), mergeOpts())
	assert.True(t, p.captureID.IsZero(), "the archive keeps the scope's capture exactly")
	out, _ := mustMerge(t, inputsOf(seg), mergeOpts())
	assert.Equal(t, format.UUID{}, layoutHeader(t, out).CaptureID, "the archive's file header keeps the nil capture")
}

func TestMergePlanClusters(t *testing.T) {
	t.Parallel()

	// steps returns one block per range, each of the seqs first to last.
	steps := func(ranges ...[2]uint64) []footerTestStep {
		out := make([]footerTestStep, 0, len(ranges))
		for _, r := range ranges {
			out = append(out, hourSteps(r[0], r[1], math.MaxInt32)...)
		}

		return out
	}
	a := mergePack(t, seg0, nil, steps([2]uint64{1, 5}, [2]uint64{6, 10}, [2]uint64{20, 25}, [2]uint64{40, 40}, [2]uint64{50, 52}))
	b := mergePack(t, seg1, nil, steps([2]uint64{5, 5}, [2]uint64{11, 12}, [2]uint64{21, 22}, [2]uint64{30, 35}, [2]uint64{50, 51}))
	c := mergePack(t, seg2, nil, steps([2]uint64{35, 45}))
	files := [][]byte{a, b, c}

	p := mustPlanMerge(t, inputsOf(files...), mergeOpts())
	type ref struct{ input, block int }
	want := []struct {
		first, last uint64
		blocks      []ref
	}{
		{1, 5, []ref{{0, 0}, {1, 0}}},           // a shared endpoint
		{6, 10, []ref{{0, 1}}},                  // alone
		{11, 12, []ref{{1, 1}}},                 // adjacent to [6, 10], sharing no seq
		{20, 25, []ref{{0, 2}, {1, 2}}},         // containment
		{30, 45, []ref{{1, 3}, {2, 0}, {0, 3}}}, // a chain through a shared endpoint and a containment
		{50, 52, []ref{{0, 4}, {1, 4}}},         // equal first seqs, in input order
	}
	require.Len(t, p.clusters, len(want))
	f3 := make([][][]byte, len(files))
	for i, f := range files {
		_, decoded, _ := splitPack(t, f)
		f3[i] = splitFooter(t, decoded).f3Raw
	}
	for k, w := range want {
		c := p.clusters[k]
		assert.Equal(t, [2]uint64{w.first, w.last}, [2]uint64{c.first, c.last}, "cluster %d", k)
		got := make([]ref, len(c.blocks))
		for j, b := range c.blocks {
			got[j] = ref{b.input, b.block}
			info := p.inputs[b.input].blocks[b.block]
			assert.Equal(t, [2]uint64{info.FirstSeq, info.LastSeq}, [2]uint64{b.first, b.last}, "cluster %d block %d", k, j)
			assert.Equal(t, f3[b.input][b.block], b.f3, "cluster %d block %d: its F-3 list as stored", k, j)
		}
		assert.Equal(t, w.blocks, got, "cluster %d", k)
	}
	assert.Equal(t, uint64(1), p.meta.SeqStart)
}
