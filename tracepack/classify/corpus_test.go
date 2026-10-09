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

// corpusFrame is a frame a classify.json of the corpus lists:
// the vector, the expected classification, the vector's single max_frame_len and the record's payload.
type corpusFrame struct {
	vector      string
	want        corpus.ClassifyFrame
	maxFrameLen uint64
	frame       []byte
}

// corpusFrames returns every frame the committed classify.json files of the corpus list, read from the vectors' packs.
func corpusFrames(tb testing.TB) []corpusFrame {
	tb.Helper()

	paths, err := filepath.Glob(filepath.Join(corpusDir, "*", corpus.FileClassify))
	require.NoError(tb, err)
	require.NotEmpty(tb, paths, "the corpus holds classify.json files")

	out := make([]corpusFrame, 0, len(paths))
	for _, p := range paths {
		dir := filepath.Dir(p)
		b, err := os.ReadFile(p)
		require.NoError(tb, err)
		var c corpus.Classify
		require.NoError(tb, corpus.Unmarshal(b, &c), p)
		pack, err := os.ReadFile(filepath.Join(dir, corpus.FilePack))
		require.NoError(tb, err)

		payloads := make(map[uint64][]byte)
		r, err := tracepack.Open(tb.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
		require.NoError(tb, err, dir)
		_, err = r.Iterate(tb.Context(), tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
			payloads[it.Record.Seq] = bytes.Clone(it.Record.Payload)
			return nil
		})
		require.NoError(tb, err, dir)

		for _, f := range c.Frames {
			frame, ok := payloads[uint64(f.Seq)]
			require.True(tb, ok, "%s lists seq %d, which its pack does not hold", p, f.Seq)
			out = append(out, corpusFrame{vector: filepath.Base(dir), want: f, maxFrameLen: uint64(c.MaxFrameLen), frame: frame})
		}
	}

	return out
}

// TestCorpusClassification classifies every frame the corpus's classify.json files list
// and requires the listed decode_status and trailing_bytes (the tracepack corpus specification §5.9),
// written by hand from the tracepack semantics specification §3 and the corpus specification §7.
// trailing_bytes is compared as 0 for every status but ok and ok-with-trailing, for which it has a meaning.
func TestCorpusClassification(t *testing.T) {
	t.Parallel()

	for _, f := range corpusFrames(t) {
		require.LessOrEqual(t, f.maxFrameLen, uint64(math.MaxInt), "%s: max_frame_len", f.vector)
		status, trailing := Frame(f.frame, int(f.maxFrameLen))
		if status != tracepack.DecodeStatusOK && status != tracepack.DecodeStatusOKWithTrailing {
			trailing = 0
		}
		assert.Equal(t, f.want.DecodeStatus, status.String(), "%s seq %d (%s)", f.vector, f.want.Seq, f.want.Rule)
		assert.Equal(t, int(f.want.TrailingBytes), trailing, "%s seq %d (%s): trailing_bytes", f.vector, f.want.Seq, f.want.Rule)
	}
}
