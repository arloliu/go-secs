package tracepack

import (
	"bytes"
	"cmp"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// withMinor returns file with its file header's format_minor set to minor.
func withMinor(t testing.TB, file []byte, minor uint16) []byte {
	t.Helper()

	return patchHeader(t, file, func(h *format.FileHeader) { h.FormatMinor = minor })
}

// requireMergeUnfinalized runs Merge and requires it to fail with an error holding msg and wrapping every error of want,
// leaving in dst no finalized pack and a report that holds nothing.
func requireMergeUnfinalized(t *testing.T, inputs []MergeInput, opts MergeOptions, msg string, want ...error) {
	t.Helper()

	out, rep, err := mergeBytes(t.Context(), inputs, opts)
	for _, w := range want {
		require.ErrorIs(t, err, w)
	}
	require.ErrorContains(t, err, msg)
	assert.Equal(t, MergeReport{}, rep)
	requireNothingFinalized(t, out)
}

// readMergedRecords reads every record of file, a finalized pack, with its payload.
func readMergedRecords(t *testing.T, file []byte) (*Reader, []Record) {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	run := iterate(t, r, Query{Payloads: true})
	recs := make([]Record, 0, len(run.items))
	for _, it := range run.items {
		recs = append(recs, it.rec)
	}

	return r, recs
}

// A merge writes the largest format_minor of its inputs, refuses an input of a newer minor than it implements,
// and refuses a minor-0 input whose quality bit 7 a minor-1 archive would read as unobserved,
// whichever way the archive would hold the block (the tracepack format specification §14).
func TestMergeMinor(t *testing.T) {
	t.Parallel()

	plain := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	other := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	// The record of seq 10 of a minor-0 segment carries the reserved quality bit 7.
	bit7 := reindexed(t, editBlock(t, plain, 0, func(b *testBlock) { b.headers[0].Quality |= uint16(QualityUnobserved) }))
	// Featureless minor-1 segments: one of seqs no other input holds, one whose blocks overlap plain's.
	minor1 := withMinor(t, other, 1)
	overlapping := withMinor(t, mergePack(t, seg2, nil, hourSteps(11, 15, 2)), 1)
	for _, f := range [][]byte{bit7, minor1, overlapping} {
		requireConsistent(t, f)
	}

	t.Run("an input of a newer minor", func(t *testing.T) {
		t.Parallel()

		newer := withMinor(t, other, formatMinorNewest+1)
		requireMergeRefused(t, inputsOf(newer), mergeOpts(), "format_minor 2", ErrMergeInput)
		requireMergeRefused(t, inputsOf(plain, newer), mergeOpts(), "input 1", ErrMergeInput)
	})

	t.Run("bit 7 of a minor-0 input beside a minor-1 input", func(t *testing.T) {
		t.Parallel()

		// The block of seq 10 alone in its cluster would be copied; beside overlapping it would be resolved.
		for _, inputs := range [][]MergeInput{inputsOf(bit7, minor1), inputsOf(minor1, bit7), inputsOf(bit7, overlapping)} {
			requireMergeRefused(t, inputs, mergeOpts(), "quality bit 7", ErrMergeInput)
		}
	})

	t.Run("minor-0 inputs keep bit 7 reserved", func(t *testing.T) {
		t.Parallel()

		out, _ := mustMerge(t, inputsOf(bit7, other), mergeOpts())
		r, recs := readMergedRecords(t, out)
		assert.Equal(t, uint16(0), r.Header().FormatMinor)
		require.Equal(t, uint64(10), recs[0].Seq)
		assert.True(t, recs[0].Quality.Has(QualityUnobserved), "the stored bit is kept")
		assert.False(t, r.Unobserved(&recs[0]), "and has no meaning")
	})

	t.Run("a featureless minor-1 input", func(t *testing.T) {
		t.Parallel()

		noCoalesce := mergeOpts()
		noCoalesce.NoCoalesce = true
		tests := []struct {
			name   string
			inputs func() []MergeInput
			opts   MergeOptions
			check  func(t *testing.T, rep MergeReport)
		}{
			{"copied", func() []MergeInput { return inputsOf(minor1) }, noCoalesce,
				func(t *testing.T, rep MergeReport) { t.Helper(); assert.Equal(t, 2, rep.Copied) }},
			{"coalesced", func() []MergeInput { return inputsOf(minor1) }, mergeOpts(),
				func(t *testing.T, rep MergeReport) { t.Helper(); assert.Equal(t, 1, rep.CoalescedEncodings) }},
			{"resolved, beside a minor-0 input", func() []MergeInput { return inputsOf(plain, overlapping) }, noCoalesce,
				func(t *testing.T, rep MergeReport) { t.Helper(); assert.Positive(t, rep.Resolved) }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				out, rep := mustMerge(t, tt.inputs(), tt.opts)
				tt.check(t, rep)
				assert.Equal(t, uint16(1), layoutHeader(t, out).FormatMinor)
				// An ordinary Writer of the archive's pack metadata writes minor 0: the minor is the inputs'.
				assert.Equal(t, uint16(0), formatMinorOf(mustPlanMerge(t, tt.inputs(), fixedIDs(tt.opts)).meta))
			})
		}

		t.Run("to an output that cannot seek", func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			_, err := Merge(t.Context(), struct{ io.Writer }{&buf}, inputsOf(minor1), fixedIDs(mergeOpts()))
			require.NoError(t, err)
			assert.Equal(t, uint16(1), layoutHeader(t, buf.Bytes()).FormatMinor)
			requireConsistent(t, buf.Bytes())
		})
	})
}

// mergeGolden reads the file name of the merge_minor0_v040 golden fixture, which tracepack v0.4.0 wrote.
func mergeGolden(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", "merge_minor0_v040", name))
	require.NoError(t, err)

	return b
}

// Plain minor-0 inputs merge to a minor-0 archive whose export, under schema tracepack-jsonl/1,
// is the export tracepack v0.4.0 wrote for the same merge, byte for byte, and so is the archive
// (the tracepack format specification §14 and §15).
func TestMergeMinor0V040(t *testing.T) {
	t.Parallel()

	a, b := mergeGolden(t, "a.tpk"), mergeGolden(t, "b.tpk")
	now := time.Unix(0, 1_789_999_200_000_000_000+int64(30*time.Minute)).UTC()
	opts := mergeOpts()
	opts.Writer, opts.ReplacementSetID, opts.PackID = "merger/golden", UUID{0x5E, 0x75}, UUID{0xA4, 0xC1}
	opts.Now = func() time.Time { return now }
	out, _ := mustMerge(t, inputsOf(a, b), opts)
	assert.Equal(t, uint16(0), layoutHeader(t, out).FormatMinor)

	var export bytes.Buffer
	_, err := ExportJSONL(t.Context(), mustOpen(t, out, ReaderOptions{}), &export)
	require.NoError(t, err)
	first, _, _ := strings.Cut(export.String(), "\n")
	assert.Contains(t, first, `"schema":"tracepack-jsonl/1"`)
	require.Equal(t, string(mergeGolden(t, "export.jsonl")), export.String(), "the export v0.4.0 wrote")
	assert.Equal(t, mergeGolden(t, "archive.tpk"), out, "the archive v0.4.0 wrote")
}

// thinnedPack writes a finalized segment of viewCapture under thinnedMeta changed by meta, when set,
// holding a data record at each seq of data and an unobserved S1F4 record at each seq of unobserved, in ascending seq,
// a block closed after every two records.
func thinnedPack(t *testing.T, meta func(m *PackMeta), data, unobserved []uint64) []byte {
	t.Helper()

	m := thinnedMeta()
	if meta != nil {
		meta(m)
	}
	recs := make([]Record, 0, len(data)+len(unobserved))
	for _, seq := range data {
		recs = append(recs, frameRecord(seq, hsmsFrame(1, 3, nil)))
	}
	for _, seq := range unobserved {
		recs = append(recs, unobservedRecord(seq))
	}
	slices.SortFunc(recs, func(a, b Record) int { return cmp.Compare(a.Seq, b.Seq) })
	m.SeqStart = recs[0].Seq

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, CaptureID: viewCapture})
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]))
		if i%2 == 1 {
			require.NoError(t, w.Flush())
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// Inputs carrying equal thinning lists merge into an archive carrying the list,
// whose unobserved records keep their meaning;
// inputs whose lists differ, in their entries, their order or the presence of the tag, are refused
// (the tracepack storage specification §4 Merge).
func TestMergeThinning(t *testing.T) {
	t.Parallel()

	a := thinnedPack(t, nil, []uint64{10}, []uint64{11})
	b := thinnedPack(t, nil, []uint64{12}, []uint64{13})
	out, _ := mustMerge(t, inputsOf(a, b), mergeOpts())
	r, recs := readMergedRecords(t, out)
	assert.Equal(t, uint16(1), r.Header().FormatMinor)
	assert.Equal(t, thinnedMeta().Thinning, r.Header().Meta.Thinning)
	require.Len(t, recs, 4)
	assert.Equal(t, []bool{false, true, false, true},
		[]bool{r.Unobserved(&recs[0]), r.Unobserved(&recs[1]), r.Unobserved(&recs[2]), r.Unobserved(&recs[3])})

	reordered := thinnedPack(t, func(m *PackMeta) { m.Thinning[0], m.Thinning[1] = m.Thinning[1], m.Thinning[0] }, []uint64{12, 13}, nil)
	fewer := thinnedPack(t, func(m *PackMeta) { m.Thinning = m.Thinning[:1] }, []uint64{12, 13}, nil)
	otherSource := thinnedPack(t, func(m *PackMeta) { m.Thinning[1].Source = "another poll" }, []uint64{12, 13}, nil)
	unthinned := mergePack(t, seg1, nil, hourSteps(12, 13, 2))
	for name, file := range map[string][]byte{
		"reordered": reordered, "an entry fewer": fewer, "another source": otherSource, "no thinning": unthinned,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			requireMergeRefused(t, inputsOf(a, file), mergeOpts(), "capture-level tag thinning", ErrMergeInput)
		})
	}
}

// A merge refuses an input holding a record carrying quality.unobserved that is not a data record unobserved by design
// (the tracepack semantics specification §2), or whose stream and function its thinning does not declare,
// through the copied-block and the resolved paths, publishing nothing (the tracepack storage specification §4 Merge):
// the records of the blocks it appends meet every rule the Writer applies to a record it appends.
// The Writer refuses such a record, so each input is built by surgery.
func TestMergeUnobservedCommitment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// edit changes the unobserved S1F4 record of seq 11, record 1 of block 0.
		edit func(h *format.RecordHeader, payload *[]byte)
		msg  string
		// want is the error the refusal wraps beside ErrMergeInput.
		want error
	}{
		{"an undeclared stream and function", func(_ *format.RecordHeader, p *[]byte) { (*p)[7] = 6 }, "thinning: unobserved S1F6", ErrMetadataCommitment},
		{"a control record", func(h *format.RecordHeader, _ *[]byte) { h.Kind = uint8(KindControl) },
			"unobserved record with a control record", ErrInvalidRecord},
		{"a payload without message text", func(h *format.RecordHeader, p *[]byte) {
			*p = (*p)[:fieldSystemBytesEnd]
			h.PayloadLen = fieldSystemBytesEnd
		}, "unobserved record with a payload of 14 bytes", ErrInvalidRecord},
		{"its function unavailable", func(h *format.RecordHeader, _ *[]byte) { h.FieldValidity &^= uint8(FieldValidityFunction) },
			"unobserved record with its stream or function unavailable", ErrInvalidRecord},
		{"a message text not zero-filled", func(_ *format.RecordHeader, p *[]byte) { (*p)[fieldSystemBytesEnd+2] = 1 },
			"unobserved record with a message text that is not zero-filled", ErrInvalidRecord},
		{"a decode_status other than not-attempted", func(h *format.RecordHeader, _ *[]byte) { h.DecodeStatus = uint8(DecodeStatusOK) },
			"unobserved record with decode_status " + DecodeStatusOK.String(), ErrInvalidRecord},
		{"trailing bytes", func(h *format.RecordHeader, _ *[]byte) { h.TrailingBytes = 1 },
			"unobserved record with 1 trailing bytes", ErrInvalidRecord},
	}
	// A segment whose block of seqs 9 and 10 overlaps the defective block of seqs 10 and 11, so that block is resolved.
	overlapping := thinnedPack(t, nil, []uint64{9, 10}, nil)
	noCoalesce := mergeOpts()
	noCoalesce.NoCoalesce = true
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Block 0 holds the data record of seq 10 and the unobserved S1F4 record of seq 11.
			defective := reindexed(t, editBlock(t, thinnedPack(t, nil, []uint64{10, 12, 13}, []uint64{11}), 0, func(b *testBlock) {
				tt.edit(&b.headers[1], &b.payloads[1])
			}))
			requireConsistent(t, defective)
			msg := "block 0: seq 11: tracepack: " + tt.msg
			requireMergeUnfinalized(t, inputsOf(defective), noCoalesce, msg, ErrMergeInput, tt.want)
			requireMergeUnfinalized(t, inputsOf(overlapping, defective), noCoalesce, msg, ErrMergeInput, tt.want)
		})
	}
}
