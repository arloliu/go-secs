package tracepack

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// closedCaptureSegment writes a segment of closeCapture over closeMeta(1000, masked, thinned) for the whole hour,
// holding data records of seqs 0 to 2:
// in a capture masked at write, seq 1 is a masked S7F3 with its entry; in a thinned capture, seq 2 is an unobserved S1F4.
func closedCaptureSegment(t *testing.T, masked, thinned bool) []byte {
	t.Helper()

	m := closeMeta(1000, masked, thinned)
	m.PeriodStart, m.PeriodEnd = blockTestHour, blockTestHour+hourNs
	recs := make([]Record, 3)
	for seq := range recs {
		recs[seq] = frameRecord(uint64(seq), hsmsFrame(1, 1, nil))
	}
	facts := PackFacts{DurableRecorder: true}
	if masked {
		recs[1] = redactedRecord(1)
		m.Redaction = maskEntries(true, 1)
		facts.AnyRedacted, facts.RedactionPolicyApplied = true, true
	}
	if thinned {
		recs[2] = unobservedRecord(2)
	}

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, CaptureID: closeCapture, Facts: facts})
	require.NoError(t, err)
	for i := range recs {
		// The capture's classifier is not the plain capture's commitment, so no record is classified.
		recs[i].DecodeStatus = DecodeStatusNotAttempted
		require.NoError(t, w.Append(&recs[i]))
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// The close segment of a capture its producer left open merges with the capture's segments,
// for plain, masked, thinned and masked-and-thinned captures, its own policy matching nothing:
// the archive keeps the seq ranges exact, counts the close as one record,
// and carries the capture's policy, entries and thinning at the capture's minor;
// a close-only merge holds the close alone
// (the tracepack storage specification §4, Merge and Closing a capture left open).
func TestMergeClose(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ masked, thinned bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("masked %t, thinned %t", c.masked, c.thinned), func(t *testing.T) {
			t.Parallel()

			segment := closedCaptureSegment(t, c.masked, c.thinned)
			highest := blockTestHour + 2
			closePack := writeCloseBytes(t, closeOptions(closeMeta(1000, c.masked, c.thinned), &highest))
			minor := uint16(0)
			if c.masked || c.thinned {
				minor = 1
			}
			require.Equal(t, minor, layoutHeader(t, closePack).FormatMinor, "the close segment's minor")

			opts := fixedIDs(mergeOpts())
			files := [][]byte{segment, closePack}
			out, rep, err := mergeBytes(t.Context(), inputsOf(files...), opts)
			require.NoError(t, err)
			requireMergeProperties(t, files, out, rep, opts)

			r := mustOpen(t, out, ReaderOptions{})
			h := r.Header()
			assert.Equal(t, minor, h.FormatMinor)
			st, ok := r.Stats()
			require.True(t, ok)
			assert.Equal(t, []SeqRange{{First: 0, Last: 2}, {First: format.MaxU64, Last: format.MaxU64}}, st.SeqRanges, "the seq ranges exact")
			assert.Equal(t, uint64(4), st.RecordCount, "the close counts one record")
			assert.Equal(t, uint64(0), h.Meta.SeqStart)
			if c.masked {
				assert.Equal(t, encryptedPolicy(), h.Meta.RedactionPolicy)
				assert.Equal(t, maskEntries(true, 1), h.Meta.Redaction)
				assert.Equal(t, fileHeaderRedactionPresent, h.Flags)
				assert.Equal(t, []string{"consumer-classifier/1"}, h.Meta.Classifiers)
				assert.Equal(t, []uint64{1 << 20}, h.Meta.MaxFrameLens)
			} else {
				assert.Nil(t, h.Meta.RedactionPolicy)
				assert.Zero(t, h.Flags)
			}
			if c.thinned {
				assert.Equal(t, descriptorMeta().Thinning, h.Meta.Thinning)
			} else {
				assert.Nil(t, h.Meta.Thinning)
			}
			assert.Empty(t, h.RedactionDefects)

			run := iterate(t, r, Query{Payloads: true})
			assert.Equal(t, []uint64{0, 1, 2, format.MaxU64}, run.seqs())
			last := run.items[3].rec
			assert.Equal(t, KindTransportEvent, last.Kind)
			assert.True(t, last.Quality.HasAny(QualityCaptureBoundary))

			// A merge of the close alone.
			alone, rep, err := mergeBytes(t.Context(), inputsOf(closePack), opts)
			require.NoError(t, err)
			requireMergeProperties(t, [][]byte{closePack}, alone, rep, opts)
			ra := mustOpen(t, alone, ReaderOptions{})
			assert.Equal(t, minor, ra.Header().FormatMinor)
			assert.Equal(t, format.MaxU64, ra.Header().Meta.SeqStart)
			aloneRun := iterate(t, ra, Query{})
			assert.Equal(t, []uint64{format.MaxU64}, aloneRun.seqs())
		})
	}
}
