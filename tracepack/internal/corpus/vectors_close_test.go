package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestCloseVectors checks the close vectors through the reader (the tracepack storage specification §4):
// a retried close writes the same bytes; a close segment holds one record at seq 2^63-1 and leaves its capture no continuation;
// the close of a masked and thinned capture inherits its commitments at minor 1;
// and an archive of a capture's records and its close counts the close as one record in a second seq range.
func TestCloseVectors(t *testing.T) {
	t.Parallel()

	staged := closeStagedMs
	once := must(writeClose("close-plain", IDFor(rolePackID, "close-plain"), closeMeta("close-plain"), &staged))(t)
	again := must(writeClose("close-plain", IDFor(rolePackID, "close-plain"), closeMeta("close-plain"), &staged))(t)
	assert.Equal(t, once, again, "a retried close is byte-identical")

	for id, minor := range map[string]uint16{"close-plain": 0, "close-no-start-bound": 0, "close-masked-thinned": 1} {
		rd, items := builtItems(t, id)
		h := rd.Header()
		assert.Equal(t, minor, h.FormatMinor, id)
		assert.Equal(t, closeSeq, h.Meta.SeqStart, id)
		require.Len(t, items, 1, id)
		assert.Equal(t, closeSeq, items[0].Record.Seq, id)
		st, ok := rd.Stats()
		require.True(t, ok, id)
		assert.Equal(t, uint64(1), st.RecordCount, id)
	}
	rd, _ := builtItems(t, "close-masked-thinned")
	h := rd.Header()
	assert.NotNil(t, h.Meta.RedactionPolicy)
	assert.NotEmpty(t, h.Meta.Thinning)
	assert.Equal(t, []string{corpusClassifier}, h.Meta.Classifiers)
	assert.Equal(t, []uint64{redactMaxFrameLen}, h.Meta.MaxFrameLens)

	rd, items := builtItems(t, "close-merged-archive")
	st, ok := rd.Stats()
	require.True(t, ok)
	assert.Equal(t, uint64(4), st.RecordCount)
	assert.Equal(t, []tracepack.SeqRange{{First: 0, Last: 2}, {First: closeSeq, Last: closeSeq}}, st.SeqRanges)
	require.Len(t, items, 4)
	assert.Equal(t, closeSeq, items[3].Record.Seq)
}
