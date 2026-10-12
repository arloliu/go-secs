package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// TestThinFDCPolls checks thin-fdc-polls through the reader (the tracepack semantics specification §2):
// the thinned polls are unobserved, their texts zero-filled and their headers kept;
// the poll observed in full, the reply with a genuinely empty text and the S9F7 are not.
func TestThinFDCPolls(t *testing.T) {
	t.Parallel()

	rd, items := builtItems(t, "thin-fdc-polls")
	h := rd.Header()
	assert.Equal(t, uint16(1), h.FormatMinor)
	assert.Equal(t, []tracepack.Thinning{{Stream: 1, Function: 3, Source: fdcPollSource}, {Stream: 1, Function: 4, Source: fdcPollSource}}, h.Meta.Thinning)
	require.Len(t, items, 5)
	for i, it := range items {
		r := it.Record
		want := i < 2
		assert.Equal(t, want, rd.Unobserved(&r), "seq %d", r.Seq)
		if want {
			assert.Equal(t, tracepack.DecodeStatusNotAttempted, r.DecodeStatus)
			assert.Zero(t, r.TrailingBytes)
			assert.Equal(t, make([]byte, len(r.Payload)-hsmsPrefixLen), r.Payload[hsmsPrefixLen:], "seq %d: a zero-filled text", r.Seq)
			captured := tracepack.Record{Kind: tracepack.KindData, Payload: r.Payload}
			captured.SetCapturedFieldValidity()
			assert.Equal(t, captured.FieldValidity, r.FieldValidity, "seq %d: the validity of the captured header", r.Seq)
		}
	}
	assert.Len(t, items[3].Record.Payload, hsmsPrefixLen, "a genuinely empty text")
}
