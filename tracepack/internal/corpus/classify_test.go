package corpus

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

func TestClassifyMarshal(t *testing.T) {
	t.Parallel()

	f1, err := NewClassifyFrame(10, "trailing", tracepack.DecodeStatusOKWithTrailing, 3)
	require.NoError(t, err)
	f2, err := NewClassifyFrame(9, "short-frame", tracepack.DecodeStatusShortFrame, 0)
	require.NoError(t, err)
	c := Classify{MaxFrameLen: 4096, Frames: []ClassifyFrame{f1, f2}}
	b, err := c.Marshal()
	require.NoError(t, err)
	require.Equal(t, `{
  "max_frame_len": "4096",
  "frames": [
    {
      "seq": "9",
      "rule": "short-frame",
      "decode_status": "short-frame",
      "trailing_bytes": 0
    },
    {
      "seq": "10",
      "rule": "trailing",
      "decode_status": "ok-with-trailing",
      "trailing_bytes": 3
    }
  ]
}
`, string(b))

	c.Frames = append(c.Frames, f2)
	_, err = c.Marshal()
	require.Error(t, err, "a seq listed twice")
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
