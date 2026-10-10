package corpus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

func TestNewClassifyFrame(t *testing.T) {
	t.Parallel()

	f, err := NewClassifyFrame(10, "trailing", tracepack.DecodeStatusOKWithTrailing, 3)
	require.NoError(t, err)
	require.Equal(t, ClassifyFrame{Seq: 10, Rule: "trailing", DecodeStatus: tracepack.DecodeStatusOKWithTrailing, TrailingBytes: 3}, f)
	for _, s := range []tracepack.DecodeStatus{
		tracepack.DecodeStatusNotAttempted, tracepack.DecodeStatusReconstructedOK, tracepack.DecodeStatusParseFailed,
		tracepack.DecodeStatusBuildRejected, tracepack.DecodeStatusNotApplicable, tracepack.DecodeStatus(99),
	} {
		_, err := NewClassifyFrame(1, "x", s, 0)
		require.Error(t, err, s.String())
	}
	_, err = NewClassifyFrame(1, "x", tracepack.DecodeStatusOK, 1)
	require.Error(t, err, "trailing bytes on ok")
	_, err = NewClassifyFrame(1, "", tracepack.DecodeStatusOK, 0)
	require.Error(t, err, "no case")
}

// TestClassifierVectors checks that the classifier expectations are the sem group's two classified vectors,
// each with every frame of its recipe and its format-only records, sorted by seq,
// and copies that an edit leaves the recipes' own untouched.
func TestClassifierVectors(t *testing.T) {
	t.Parallel()

	vs, err := ClassifierVectors()
	require.NoError(t, err)
	ids := make([]string, 0, len(vs))
	for _, v := range vs {
		ids = append(ids, v.ID)
		require.EqualValues(t, semMaxFrameLen, v.Classify.MaxFrameLen, v.ID)
		require.NotEmpty(t, v.Classify.Frames, v.ID)
		for i := 1; i < len(v.Classify.Frames); i++ {
			require.Less(t, v.Classify.Frames[i-1].Seq, v.Classify.Frames[i].Seq, v.ID)
		}
	}
	require.Equal(t, []string{"sem-decode-status-classified", "sem-item-validity"}, ids)
	require.Equal(t, []uint64{0}, vs[0].Classify.FormatOnly, "the not-attempted record")
	require.Len(t, vs[0].Classify.Frames, 18)
	require.Empty(t, vs[1].Classify.FormatOnly)
	require.Len(t, vs[1].Classify.Frames, 50)

	vs[0].Classify.Frames[0].DecodeStatus = tracepack.DecodeStatusOK
	again, err := ClassifierVectors()
	require.NoError(t, err)
	require.NotEqual(t, tracepack.DecodeStatusOK, again[0].Classify.Frames[0].DecodeStatus)
}

// TestClassifierVectorsRefuseASeqTwice checks that the classifier expectations of a recipe that list a seq twice,
// as two frames or as a frame and a format-only record, are refused.
func TestClassifierVectorsRefuseASeqTwice(t *testing.T) {
	t.Parallel()

	frame := must(NewClassifyFrame(1, "ok", tracepack.DecodeStatusOK, 0))(t)
	for name, c := range map[string]Classify{
		"a frame twice":             {MaxFrameLen: 40, Frames: []ClassifyFrame{frame, frame}},
		"a frame and a format-only": {MaxFrameLen: 40, Frames: []ClassifyFrame{frame}, FormatOnly: []uint64{1}},
	} {
		_, err := expectationsOf([]Recipe{{ID: "sem-x", Classify: &c}})
		require.ErrorContains(t, err, "lists a seq twice", name)
	}

	got, err := expectationsOf([]Recipe{{ID: "basic-x"}, {ID: "sem-x", Classify: &Classify{MaxFrameLen: 40, Frames: []ClassifyFrame{frame}}}})
	require.NoError(t, err)
	require.Len(t, got, 1, "a recipe without expectations has none")
}
