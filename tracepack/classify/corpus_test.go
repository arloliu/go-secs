package classify

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/corpus"
)

// corpusDir is the root of the conformance corpus, from this package's directory.
const corpusDir = "../testdata/corpus"

// corpusFrame is a classifier case of the corpus:
// the vector, the expected classification, the vector's single max_frame_len and the record's payload.
type corpusFrame struct {
	vector      string
	want        corpus.ClassifyFrame
	maxFrameLen uint64
	frame       []byte
}

// storedRecord is a data or control record of a committed pack:
// its stored decode_status and trailing_bytes, and its payload.
type storedRecord struct {
	status   tracepack.DecodeStatus
	trailing uint32
	payload  []byte
}

// corpusFrames returns every classifier case the corpus recipes expect (corpus.ClassifierVectors),
// each with its record's payload from the vector's committed pack.
// It requires the committed pack to hold each case's record, storing the expected decode_status and trailing_bytes,
// under the vector's max_frame_len,
// and every data and control record of the pack to be a case or one of the recipe's format-only records.
// The cases are the recipes' own: none is selected by whether the classifier agrees with it.
func corpusFrames(tb testing.TB) []corpusFrame {
	tb.Helper()

	vectors, err := corpus.ClassifierVectors()
	require.NoError(tb, err)
	require.NotEmpty(tb, vectors, "the corpus recipes hold classifier cases")

	var out []corpusFrame
	for _, v := range vectors {
		require.NotEmpty(tb, v.Classify.Frames, v.ID)
		records := storedRecords(tb, v.ID, v.Classify.MaxFrameLen)
		covered := make(map[uint64]bool, len(records))
		for _, f := range v.Classify.Frames {
			r, ok := records[f.Seq]
			require.True(tb, ok, "%s: the committed pack holds no data or control record of seq %d (%s)", v.ID, f.Seq, f.Rule)
			require.Equal(tb, f.DecodeStatus.String(), r.status.String(), "%s seq %d (%s): stored decode_status", v.ID, f.Seq, f.Rule)
			require.Equal(tb, f.TrailingBytes, r.trailing, "%s seq %d (%s): stored trailing_bytes", v.ID, f.Seq, f.Rule)
			covered[f.Seq] = true
			out = append(out, corpusFrame{vector: v.ID, want: f, maxFrameLen: v.Classify.MaxFrameLen, frame: r.payload})
		}
		for _, seq := range v.Classify.FormatOnly {
			require.Contains(tb, records, seq, "%s: the format-only seq %d is no data or control record of the committed pack", v.ID, seq)
			require.False(tb, covered[seq], "%s: seq %d is both a case and format-only", v.ID, seq)
			covered[seq] = true
		}
		for seq := range records {
			require.True(tb, covered[seq], "%s: the data or control record of seq %d is neither a case nor format-only", v.ID, seq)
		}
	}

	return out
}

// storedRecords returns the data and control records of the committed pack of the vector id, by seq,
// after requiring that the pack's single max_frame_len is maxFrameLen.
func storedRecords(tb testing.TB, id string, maxFrameLen uint64) map[uint64]storedRecord {
	tb.Helper()

	pack, err := os.ReadFile(filepath.Join(corpusDir, id, corpus.FilePack))
	require.NoError(tb, err)
	r, err := tracepack.Open(tb.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.NoError(tb, err, id)
	require.Equal(tb, []uint64{maxFrameLen}, r.Header().Meta.MaxFrameLens, "%s: max_frame_len", id)

	out := make(map[uint64]storedRecord)
	_, err = r.Iterate(tb.Context(), tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		rec := &it.Record
		if rec.Kind == tracepack.KindData || rec.Kind == tracepack.KindControl {
			out[rec.Seq] = storedRecord{status: rec.DecodeStatus, trailing: rec.TrailingBytes, payload: bytes.Clone(rec.Payload)}
		}

		return nil
	})
	require.NoError(tb, err, id)

	return out
}

// TestClassifierCases classifies every classifier case of the corpus recipes,
// on its record's payload in the committed pack,
// and requires the expected decode_status and trailing_bytes,
// written by hand from the tracepack semantics specification §3
// and the checklist of the tracepack corpus specification §7.
// trailing_bytes is compared as 0 for every status but ok and ok-with-trailing, for which it has a meaning.
func TestClassifierCases(t *testing.T) {
	t.Parallel()

	for _, f := range corpusFrames(t) {
		require.LessOrEqual(t, f.maxFrameLen, uint64(math.MaxInt), "%s: max_frame_len", f.vector)
		status, trailing := New(f.maxFrameLen).Frame(f.frame)
		if status != tracepack.DecodeStatusOK && status != tracepack.DecodeStatusOKWithTrailing {
			trailing = 0
		}
		assert.Equal(t, f.want.DecodeStatus.String(), status.String(), "%s seq %d (%s)", f.vector, f.want.Seq, f.want.Rule)
		assert.Equal(t, int(f.want.TrailingBytes), trailing, "%s seq %d (%s): trailing_bytes", f.vector, f.want.Seq, f.want.Rule)
	}
}
