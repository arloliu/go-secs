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

// mergeView returns the active view of the scope of files, computed by ActiveView from each pack's Reader.Info.
func mergeView(t testing.TB, commits CommitSet, files ...[]byte) View {
	t.Helper()

	infos := make([]PackInfo, len(files))
	for i, f := range files {
		infos[i] = mustOpen(t, f, ReaderOptions{}).Info()
	}
	v, err := ActiveView(infos, commits)
	require.NoError(t, err)

	return v
}

// soleView returns a view whose one pack, and one lineage entry, is the pack of file,
// of viewCapture in the merge test hour, as a caller could build it by hand.
func soleView(t testing.TB, file []byte) View {
	t.Helper()

	id := UUID(layoutHeader(t, file).PackID)

	return View{Capture: viewCapture, Hour: mergeHour, Packs: []UUID{id}, CompactedFrom: []UUID{id}}
}

// cloneView returns a copy of v that shares no memory with it, so a test may change any of its fields.
func cloneView(v View) View {
	c := v
	if v.Generation != nil {
		g := *v.Generation
		c.Generation = &g
	}
	c.Packs, c.CompactedFrom = slices.Clone(v.Packs), slices.Clone(v.CompactedFrom)
	c.Deletable, c.Excluded = slices.Clone(v.Deletable), slices.Clone(v.Excluded)

	return c
}

// filesOf returns the files that v.Packs names, in the order of v.Packs.
func filesOf(t testing.TB, v View, files ...[]byte) [][]byte {
	t.Helper()

	byID := make(map[UUID][]byte, len(files))
	for _, f := range files {
		byID[UUID(layoutHeader(t, f).PackID)] = f
	}
	out := make([][]byte, len(v.Packs))
	for i, id := range v.Packs {
		f, ok := byID[id]
		require.True(t, ok, "no file holds pack %s", id)
		out[i] = f
	}

	return out
}

// inputsOf returns the files that v.Packs names, as merge inputs in the order of v.Packs.
func inputsOf(t testing.TB, v View, files ...[]byte) []MergeInput {
	t.Helper()

	ordered := filesOf(t, v, files...)
	out := make([]MergeInput, len(ordered))
	for i, f := range ordered {
		out[i] = MergeInput{ReaderAt: bytes.NewReader(f), Size: int64(len(f))}
	}

	return out
}

// mergeOpts returns the MergeOptions of the tests: a writer tag, generation 1 of epoch 1, and a conflict sink.
func mergeOpts() MergeOptions {
	return MergeOptions{Writer: "merge-test", ScopeGeneration: 1, PublisherEpoch: 1, OnConflict: func(Conflict) {}}
}

// requireMergeRefused runs Merge and requires it to fail before it touches dst,
// with an error holding msg and wrapping every error of want;
// unless want holds ErrMergeInput, the error must not wrap it.
func requireMergeRefused(t *testing.T, v View, inputs []MergeInput, opts MergeOptions, msg string, want ...error) {
	t.Helper()

	var dst touchWriter
	rep, err := Merge(t.Context(), &dst, v, inputs, opts)
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
func mustPlanMerge(t *testing.T, v View, inputs []MergeInput, opts MergeOptions) *mergePlan {
	t.Helper()

	p, err := planMerge(t.Context(), &v, inputs, &opts)
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

// generationScope returns a scope with a generation: its member memA, of set setA ranked (2, 3),
// at compaction_level level and with compacted_from {0xC1}, holding seqs 10 to 13, edited by member;
// and segment seg1, holding seqs 20 to 23. setA is committed.
func generationScope(t testing.TB, level uint8, member func(m *PackMeta)) (View, [][]byte) {
	t.Helper()

	mem := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 2, 3, []UUID{{0xC1}})
		m.CompactionLevel = level
		if member != nil {
			member(m)
		}
	}, hourSteps(10, 13, 2))
	seg := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	files := [][]byte{seg, mem}

	return mergeView(t, commitSet(setA), files...), files
}

// generationOpts returns mergeOpts ranked (3, 1), above the generation of generationScope.
func generationOpts() MergeOptions {
	opts := mergeOpts()
	opts.PublisherEpoch, opts.ScopeGeneration = 3, 1

	return opts
}

func TestMergeOptionErrors(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	v := mergeView(t, nil, a, b)
	notPacks := []MergeInput{{ReaderAt: bytes.NewReader([]byte("x")), Size: 1}, {ReaderAt: bytes.NewReader([]byte("y")), Size: 1}}

	tests := []struct {
		name   string
		edit   func(o *MergeOptions)
		view   *View
		inputs []MergeInput
		msg    string
	}{
		{name: "no writer", edit: func(o *MergeOptions) { o.Writer = "" }, msg: "Writer is empty"},
		{name: "no conflict sink", edit: func(o *MergeOptions) { o.OnConflict = nil }, msg: "OnConflict is nil"},
		{name: "no conflict sink before an unreadable input", edit: func(o *MergeOptions) { o.OnConflict = nil }, inputs: notPacks, msg: "OnConflict"},
		{name: "generation 0", edit: func(o *MergeOptions) { o.ScopeGeneration = 0 }, msg: "ScopeGeneration is 0"},
		{name: "generation above 2^63-1", edit: func(o *MergeOptions) { o.ScopeGeneration = 1 << 63 }, msg: "rank"},
		{name: "epoch above 2^63-1", edit: func(o *MergeOptions) { o.PublisherEpoch = 1 << 63 }, msg: "rank"},
		{name: "a codec outside the registry", edit: func(o *MergeOptions) { o.Codec = Codec(9) }, msg: "Codec"},
		{name: "negative block threshold", edit: func(o *MergeOptions) { o.BlockThreshold = -1 }, msg: "BlockThreshold"},
		{name: "block threshold above 2^31-1", edit: func(o *MergeOptions) { o.BlockThreshold = int(format.MaxLen32) + 1 }, msg: "BlockThreshold"},
		{name: "negative open blocks", edit: func(o *MergeOptions) { o.MaxOpenBlocks = -1 }, msg: "MaxOpenBlocks"},
		{name: "negative footer budget", edit: func(o *MergeOptions) { o.MaxFooterLen = -1 }, msg: "MaxFooterLen"},
		{name: "a view without packs", view: &View{Capture: viewCapture, Hour: mergeHour}, inputs: []MergeInput{}, msg: "no packs"},
		{name: "an input without a ReaderAt", inputs: []MergeInput{inputsOf(t, v, a, b)[0], {Size: 1}}, msg: "input 1 has no ReaderAt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			if tt.edit != nil {
				tt.edit(&opts)
			}
			view, inputs := v, inputsOf(t, v, a, b)
			if tt.view != nil {
				view = *tt.view
			}
			if tt.inputs != nil {
				inputs = tt.inputs
			}
			requireMergeRefused(t, view, inputs, opts, tt.msg)
		})
	}
}

func TestMergeIdentities(t *testing.T) {
	t.Parallel()

	v, files := generationScope(t, 1, nil)
	require.Equal(t, &Generation{ReplacementSetID: setA, PublisherEpoch: 2, ScopeGeneration: 3, Member: memA}, v.Generation)
	require.Equal(t, []UUID{memA, seg1}, v.Packs)
	require.Equal(t, []UUID{{0xC1}, seg1}, v.CompactedFrom)
	inputs := inputsOf(t, v, files...)

	refused := []struct {
		name string
		edit func(o *MergeOptions)
		msg  string
	}{
		{name: "pack id of an input", edit: func(o *MergeOptions) { o.PackID = seg1 }, msg: "PackID"},
		{name: "pack id of the generation's member", edit: func(o *MergeOptions) { o.PackID = memA }, msg: "PackID"},
		{name: "pack id in the lineage", edit: func(o *MergeOptions) { o.PackID = UUID{0xC1} }, msg: "PackID"},
		{name: "the generation's replacement set", edit: func(o *MergeOptions) { o.ReplacementSetID = setA }, msg: "ReplacementSetID"},
		{name: "the generation's rank", edit: func(o *MergeOptions) { o.PublisherEpoch, o.ScopeGeneration = 2, 3 }, msg: "rank (2, 3)"},
		{name: "a lower generation", edit: func(o *MergeOptions) { o.PublisherEpoch, o.ScopeGeneration = 2, 2 }, msg: "rank (2, 2)"},
		{name: "a lower epoch with a higher generation", edit: func(o *MergeOptions) { o.PublisherEpoch, o.ScopeGeneration = 1, 9 }, msg: "rank (1, 9)"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := generationOpts()
			tt.edit(&opts)
			requireMergeRefused(t, v, inputs, opts, tt.msg)
		})
	}

	t.Run("a higher epoch with a lower generation", func(t *testing.T) {
		t.Parallel()

		opts := mergeOpts()
		opts.PublisherEpoch, opts.ScopeGeneration = 3, 1
		p := mustPlanMerge(t, v, inputs, opts)
		assert.Equal(t, new(uint64(3)), p.meta.PublisherEpoch)
		assert.Equal(t, new(uint64(1)), p.meta.ScopeGeneration)
	})

	t.Run("omitted ids generated", func(t *testing.T) {
		t.Parallel()

		p := mustPlanMerge(t, v, inputs, generationOpts())
		ids := append(slices.Clone(v.Packs), v.CompactedFrom...)
		assert.False(t, p.packID.IsZero())
		assert.NotContains(t, ids, p.packID)
		require.NotNil(t, p.meta.ReplacementSetID)
		assert.False(t, p.meta.ReplacementSetID.IsZero())
		assert.NotEqual(t, setA, *p.meta.ReplacementSetID)
		assert.NotEqual(t, p.packID, *p.meta.ReplacementSetID)
	})

	t.Run("given ids kept", func(t *testing.T) {
		t.Parallel()

		opts := generationOpts()
		opts.PackID, opts.ReplacementSetID = UUID{0xAC}, UUID{0x5E, 0xE}
		p := mustPlanMerge(t, v, inputs, opts)
		assert.Equal(t, UUID{0xAC}, p.packID)
		assert.Equal(t, &UUID{0x5E, 0xE}, p.meta.ReplacementSetID)
	})

	t.Run("any rank without a generation", func(t *testing.T) {
		t.Parallel()

		seg := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
		sv := mergeView(t, nil, seg)
		opts := mergeOpts()
		opts.PublisherEpoch = 0
		mustPlanMerge(t, sv, inputsOf(t, sv, seg), opts)
	})
}

func TestMergeTrustChecks(t *testing.T) {
	t.Parallel()

	v, files := generationScope(t, 1, nil)
	inputs := inputsOf(t, v, files...)
	segGen1 := mergePack(t, seg2, func(m *PackMeta) {
		m.ScopeGeneration, m.PublisherEpoch = new(uint64(1)), new(uint64(1))
		m.ReplacementSetID, m.ReplacementSetSize, m.ReplacementSetIndex = new(setB), new(uint64(1)), new(uint64(0))
	}, hourSteps(30, 31, 2))
	segView := soleView(t, segGen1)

	tests := []struct {
		name   string
		view   func(v *View)
		inputs []MergeInput
		msg    string
	}{
		{name: "fewer inputs than packs", inputs: inputs[:1], msg: "1 inputs for the 2 packs"},
		{name: "more inputs than packs", inputs: append(slices.Clone(inputs), inputs[0]), msg: "3 inputs for the 2 packs"},
		{name: "inputs out of the view's order", inputs: []MergeInput{inputs[1], inputs[0]}, msg: "input 0, pack " + memA.String() + ": the input is pack " + seg1.String()},
		{name: "a generation-0 pack outside the view's lineage", view: func(v *View) {
			v.CompactedFrom = []UUID{{0xC1}}
		}, msg: "input 1, pack " + seg1.String() + ": a generation-0 pack that the view's compacted_from does not list"},
		{name: "the generation's lineage outside the view's", view: func(v *View) {
			v.CompactedFrom = []UUID{seg1}
		}, msg: "compacted_from lists " + UUID{0xC1}.String()},
		{name: "an archive without the view's generation", view: func(v *View) {
			v.Generation = nil
		}, msg: "not the member of the view's generation"},
		{name: "an archive of another rank", view: func(v *View) {
			v.Generation.ScopeGeneration = 2
		}, msg: "ranked (2, 3) that is not the member"},
		{name: "an archive of another replacement set", view: func(v *View) {
			v.Generation.ReplacementSetID = setB
		}, msg: "not the member"},
		{name: "a segment of generation 1", view: func(v *View) {
			*v = cloneView(segView)
		}, inputs: inputsOf(t, segView, segGen1), msg: "pack_role segment at scope_generation 1, not 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			view := cloneView(v)
			if tt.view != nil {
				tt.view(&view)
			}
			in := inputs
			if tt.inputs != nil {
				in = tt.inputs
			}
			requireMergeRefused(t, view, in, generationOpts(), tt.msg, ErrMergeInput)
		})
	}
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
	extract := mergePack(t, seg0, func(m *PackMeta) {
		m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("all")
	}, hourSteps(10, 13, 2))

	type vector struct {
		name  string
		files [][]byte
		// view, when set, is the view of the vector; else ActiveView's over files.
		view *View
		// inputs, when set, are the vector's inputs; else the files of the view's packs.
		inputs []MergeInput
		opts   *MergeOptions
		// cause, when set, is an error the input error wraps.
		cause error
		msg   string
	}
	sole := func(name string, file []byte, msg string) vector {
		v := soleView(t, file)

		return vector{name: name, files: [][]byte{file}, view: &v, msg: msg}
	}
	lv, lf := generationScope(t, math.MaxUint8, nil)
	gOpts := generationOpts()
	gv := soleView(t, good)
	flipped := flipByte(good, 20)
	tests := []vector{
		sole("an extract", extract, "pack_role extract takes no part"),
		sole("an unknown role", setMetaRole(t, good, 0), "pack_role unknown takes no part"),
		sole("the retired role", setMetaRole(t, good, packRoleRetired), "pack_role unknown(5) takes no part"),
		sole("another capture", withIDs(t, good, seg0, UUID{0xCC}), "capture_id "+UUID{0xCC}.String()),
		sole("not finalized", withIDs(t, writeRepairPack(t, CodecZstd, nil, true, hourSteps(10, 13, 2)).file, seg0, viewCapture), "not finalized"),
		sole("no valid footer", invalidFooterFile(t, good), "no valid footer"),
		sole("an empty period", mergePack(t, seg0, period(h, h), nil), "period"),
		sole("a period over two hours", mergePack(t, seg0, period(h+hourNs/2, h+3*hourNs/2), nil), "period"),
		sole("a period in another hour", mergePack(t, seg0, period(h+hourNs, h+2*hourNs), nil), "is not inside the view's UTC hour"),
		sole("a correctly indexed block in another hour", nextHour, "block 0 at offset"),
		sole("blocks in two hours", twoHours, "block 1 at offset"),
		sole("a redacted record", redacted, "a redacted record, which no stored pack holds"),
		sole("seq_start before the first record", lying, "seq_start 19, but its first record's seq is 20"),
		{name: "a borrowed classifier", files: [][]byte{good, unclassified},
			msg: "input 1, pack " + seg1.String() + ": a classified record without classifier"},
		{name: "a borrowed max_frame_len", files: [][]byte{oversized, unbounded},
			msg: "input 1, pack " + seg1.String() + ": an oversized record without max_frame_len"},
		{name: "a later input's seq_start", files: [][]byte{good, lying},
			msg: "input 1, pack " + seg1.String() + ": seq_start 19"},
		{name: "compaction_level 255", files: lf, view: &lv, opts: &gOpts,
			msg: "input 0, pack " + memA.String() + ": compaction_level 255"},
		{name: "an unreadable file header", files: [][]byte{good}, view: &gv,
			inputs: []MergeInput{{ReaderAt: bytes.NewReader(flipped), Size: int64(len(flipped))}},
			cause:  ErrChecksum, msg: "input 0, pack " + seg0.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var v View
			if tt.view != nil {
				v = *tt.view
			} else {
				v = mergeView(t, nil, tt.files...)
			}
			inputs := tt.inputs
			if inputs == nil {
				inputs = inputsOf(t, v, tt.files...)
			}
			opts := mergeOpts()
			if tt.opts != nil {
				opts = *tt.opts
			}
			// Every refusal of an input is an input error.
			want := []error{ErrMergeInput}
			if tt.cause != nil {
				want = append(want, tt.cause)
			}
			requireMergeRefused(t, v, inputs, opts, tt.msg, want...)
		})
	}
}

func TestMergeInputOverReaderBudget(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	v := mergeView(t, nil, a, b)
	// The zstd footers are shorter on disk than decoded, so a budget between the two passes the read and fails the decode.
	la, lb := layoutOf(t, a).tr, layoutOf(t, b).tr
	onDisk := max(la.FooterLen, lb.FooterLen)
	require.Less(t, onDisk, min(la.FooterUncompressedLen, lb.FooterUncompressedLen))
	tests := []struct {
		name   string
		budget ReaderOptions
		msg    string
	}{
		{name: "pack metadata", budget: ReaderOptions{MaxPackMetadataLen: 8}, msg: "exceed MaxPackMetadataLen"},
		{name: "footer on disk", budget: ReaderOptions{MaxFooterLen: 16}, msg: "footer_len"},
		{name: "decoded footer", budget: ReaderOptions{MaxFooterLen: int64(onDisk)}, msg: "footer_uncompressed_len"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := mergeOpts()
			opts.Reader = tt.budget
			var dst touchWriter
			rep, err := Merge(t.Context(), &dst, v, inputsOf(t, v, a, b), opts)
			// A budget is not the input's defect, so the error is not an input error.
			require.ErrorIs(t, err, ErrReadLimit)
			require.NotErrorIs(t, err, ErrMergeInput)
			require.ErrorContains(t, err, "input 0, pack "+seg0.String())
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
	v := mergeView(t, nil, file)
	opts := mergeOpts()
	opts.Reader.MaxFooterLen = int64(lists) - 1
	requireMergeRefused(t, v, inputsOf(t, v, file), opts, "input 0, pack "+seg0.String(), ErrReadLimit)
	_, _, err := mergeBytes(t.Context(), v, inputsOf(t, v, file), opts)
	require.ErrorContains(t, err, "the F-3 lists of blocks 0 to 7 ")

	opts.Reader.MaxFooterLen = int64(lists)
	_, rep := mustMerge(t, v, inputsOf(t, v, file), opts)
	assert.Equal(t, uint64(8), rep.Records)
}

func TestMergeInputReadFailures(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	b := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	v := mergeView(t, nil, a, b)

	t.Run("a failed read", func(t *testing.T) {
		t.Parallel()

		failing := newGatedReader(b)
		failing.fail = func(int64, int) bool { return true }
		inputs := inputsOf(t, v, a, b)
		inputs[1].ReaderAt = failing
		// A failed read is not the input's defect: a caller retries it instead of repairing the pack.
		requireMergeRefused(t, v, inputs, mergeOpts(), "input 1, pack "+seg1.String(), errInjected)
	})

	t.Run("an object shorter than its size", func(t *testing.T) {
		t.Parallel()

		inputs := inputsOf(t, v, a, b)
		inputs[1].Size += 100
		requireMergeRefused(t, v, inputs, mergeOpts(), "input 1, pack "+seg1.String(), ErrMergeInput, io.ErrUnexpectedEOF)
	})
}

func TestMergeCancelledBeforeWriting(t *testing.T) {
	t.Parallel()

	a := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	v := mergeView(t, nil, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var dst touchWriter
	_, err := Merge(ctx, &dst, v, inputsOf(t, v, a), mergeOpts())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrMergeInput)
	assert.Zero(t, dst.calls)
}

func TestMergeAcceptsSameHourRecordsOutsideTheSegmentPeriod(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	// A same-hour clock step puts records before the segment's flush interval.
	seg := mergePack(t, seg0, func(m *PackMeta) { m.PeriodStart, m.PeriodEnd = h+hourNs/2, h+2*hourNs/3 }, hourSteps(10, 13, 2))
	v := mergeView(t, nil, seg)
	p := mustPlanMerge(t, v, inputsOf(t, v, seg), mergeOpts())
	assert.Equal(t, [2]int64{h, h + hourNs}, [2]int64{p.meta.PeriodStart, p.meta.PeriodEnd}, "the archive's period is the hour")
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
			v := mergeView(t, nil, a, b)
			requireMergeRefused(t, v, inputsOf(t, v, a, b), mergeOpts(),
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
		v := mergeView(t, nil, absent, empty)
		requireMergeRefused(t, v, inputsOf(t, v, absent, empty), mergeOpts(), "capture-level tag hsms_timers differs", ErrMergeInput)
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

	v := mergeView(t, nil, a, b)
	p := mustPlanMerge(t, v, inputsOf(t, v, a, b), mergeOpts())
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
	notCarried := func(m *PackMeta) {
		m.FlushIntervalNs, m.Notes, m.ExtractFilter = new(uint64(60_000_000_000)), new("note"), new("filter")
		m.RedactionPolicy = &RedactionPolicy{PolicyID: "p", PolicyVersion: 1, KeyID: "k", DigestAlgorithm: DigestAlgorithmHMACSHA256}
		m.Redaction = []RedactionEntry{{Seq: 1, MaskedRanges: []MaskedRange{{Offset: 0, Length: 2}}, Domain: "d", Digest: bytes.Repeat([]byte{0xD1}, 32)}}
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
	replaced := mergePack(t, seg2, nil, hourSteps(30, 31, 2))
	patch := mergePack(t, patP1, func(m *PackMeta) {
		m.PackRole, m.PatchBase, m.Supersedes = PackRoleRepair, new(setA), []UUID{seg2}
		m.Coverage = []Coverage{cov(30)}
	}, hourSteps(30, 30, 2))
	files := [][]byte{mem, seg, replaced, patch}
	v := mergeView(t, commitSet(setA, patP1), files...)
	require.Equal(t, []UUID{memA, seg1, patP1}, v.Packs)
	require.Equal(t, []UUID{{0xC1}, {0xC2}, seg1, seg2, patP1}, v.CompactedFrom)

	opts := generationOpts()
	opts.ReplacementSetID = UUID{0x5E, 0xE}
	p := mustPlanMerge(t, v, inputsOf(t, v, files...), opts)
	m := p.meta

	assert.Equal(t, PackRoleArchive, m.PackRole)
	assert.Equal(t, new(uint64(1)), m.ScopeGeneration)
	assert.Equal(t, new(uint64(3)), m.PublisherEpoch)
	assert.Equal(t, new(UUID{0x5E, 0xE}), m.ReplacementSetID)
	assert.Equal(t, new(uint64(1)), m.ReplacementSetSize)
	assert.Equal(t, new(uint64(0)), m.ReplacementSetIndex)
	assert.Equal(t, [2]int64{blockTestHour, blockTestHour + hourNs}, [2]int64{m.PeriodStart, m.PeriodEnd})
	assert.Equal(t, uint8(3), m.CompactionLevel, "one above the member's level 2")
	assert.Equal(t, v.CompactedFrom, m.CompactedFrom)
	assert.Equal(t, []UUID{memA}, m.Supersedes)
	assert.Equal(t, []Coverage{cov(1), cov(3, u1, u2), cov(3, u2, u1), cov(5), cov(30)}, m.Coverage,
		"every entry once, in input order, an entry of another encoding but the same nested entries dropped")
	assert.Equal(t, []string{"a", "b", "c"}, m.Classifiers)
	assert.Equal(t, []uint64{100, 4096}, m.MaxFrameLens)
	assert.Equal(t, "merge-test", m.Writer)
	assert.Equal(t, uint64(10), m.SeqStart, "the first record's seq")

	assert.Nil(t, m.FlushIntervalNs)
	assert.Nil(t, m.Notes)
	assert.Nil(t, m.PatchBase)
	assert.Nil(t, m.RedactionPolicy)
	assert.Nil(t, m.Redaction)
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
	v := mergeView(t, nil, a, b)
	inputs := inputsOf(t, v, a, b)

	// The default budget accepts ordinary inputs.
	raw, err := mustPlanMerge(t, v, inputs, mergeOpts()).meta.MarshalBinary()
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

		mustPlanMerge(t, v, inputs, withBudget(archiveLen))
	})
	for name, budget := range map[string]int64{"one byte less": archiveLen - 1, "the longest input's length": inputLen} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			opts := withBudget(budget)
			requireMergeRefused(t, v, inputs, opts,
				fmt.Sprintf("the archive's pack metadata of %d bytes exceeds MaxPackMetadataLen %d", archiveLen, budget), ErrMergeLimit)
			_, err := planMerge(t.Context(), &v, inputs, &opts)
			assert.NotErrorIs(t, err, ErrReadLimit, "every input fits the budget")
		})
	}
}

func TestMergeArchiveLineageWithoutGenerationZeroPacks(t *testing.T) {
	t.Parallel()

	// A converter's archive: a generation at compaction_level 0 without compacted_from.
	converter := mergePack(t, memA, func(m *PackMeta) {
		generationMeta(m, setA, 1, 1, nil)
		m.CompactionLevel = 0
	}, hourSteps(10, 13, 2))
	v := mergeView(t, commitSet(setA), converter)
	require.Empty(t, v.CompactedFrom)

	opts := mergeOpts()
	opts.ScopeGeneration = 2
	m := mustPlanMerge(t, v, inputsOf(t, v, converter), opts).meta
	assert.Nil(t, m.CompactedFrom, "absent when the lineage list is empty")
	assert.Equal(t, uint8(1), m.CompactionLevel)
	assert.Equal(t, []UUID{memA}, m.Supersedes)

	seg := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	sv := mergeView(t, nil, seg)
	m = mustPlanMerge(t, sv, inputsOf(t, sv, seg), mergeOpts()).meta
	assert.Nil(t, m.Supersedes, "absent without a generation")
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
		// A patch holding only coverage.
		empty(patP1, 7, func(m *PackMeta) {
			m.PackRole, m.Supersedes = PackRoleRepair, []UUID{{0xD0}}
			m.Coverage = []Coverage{{CaptureID: new(viewCapture), SeqFirst: new(uint64(7)), TimeStart: new(blockTestHour), TimeEnd: new(blockTestHour)}}
		}),
	}
	v := mergeView(t, commitSet(patP1), files...)
	require.Len(t, v.Packs, 3)
	p := mustPlanMerge(t, v, inputsOf(t, v, files...), mergeOpts())
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
			v := mergeView(t, nil, seg)
			m := mustPlanMerge(t, v, inputsOf(t, v, seg), mergeOpts()).meta
			assert.Equal(t, tt.want, [2]int64{m.PeriodStart, m.PeriodEnd})
			assert.Less(t, m.PeriodEnd-m.PeriodStart, hourNs, "a bound was clamped")
			hour, ok := periodHour(m.PeriodStart, m.PeriodEnd)
			assert.True(t, ok)
			assert.Equal(t, v.Hour, hour)
		})
	}
}

func TestMergeKeepsANilCapture(t *testing.T) {
	t.Parallel()

	seg := withIDs(t, mergePack(t, seg0, nil, hourSteps(10, 13, 2)), seg0, UUID{})
	v := mergeView(t, nil, seg)
	require.True(t, v.Capture.IsZero())
	p := mustPlanMerge(t, v, inputsOf(t, v, seg), mergeOpts())
	assert.True(t, p.captureID.IsZero(), "the archive keeps the scope's capture exactly")
	out, _ := mustMerge(t, v, inputsOf(t, v, seg), mergeOpts())
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
	v := mergeView(t, nil, files...)
	require.Equal(t, []UUID{seg0, seg1, seg2}, v.Packs)

	p := mustPlanMerge(t, v, inputsOf(t, v, files...), mergeOpts())
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
