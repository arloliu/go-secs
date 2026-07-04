package sml

import (
	"encoding/binary"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestEncoder_DefaultEqualsToSML(t *testing.T) {
	items := []secs2.Item{
		// Populated across the full type matrix (T3-m1): every SECS-II leaf type plus a nested list.
		secs2.A("hello"),
		secs2.B(0x01, 0x02, 0xFF),
		secs2.BOOLEAN(true, false, true),
		secs2.I1(1, -2),
		secs2.I2(1, -2, 3),
		secs2.I4(1, -2, 3),
		secs2.I8(1, -2, 3),
		secs2.U1(1, 2),
		secs2.U2(10, 20),
		secs2.U4(1, 2, 3),
		secs2.U8(1, 2, 3),
		secs2.F4(1.5, -2.25),
		secs2.F8(1.5, -2.25),
		secs2.J("jis"),
		secs2.W("utf8"),
		secs2.L(secs2.A("a"), secs2.I1(1, 2), secs2.L(secs2.U1(9))),

		// Empty variants of every type (T3-m1): the empty-payload rendering is a distinct code path
		// and a common source of Encode/ToSML drift.
		secs2.A(""),
		secs2.B(),
		secs2.BOOLEAN(),
		secs2.I1(),
		secs2.I2(),
		secs2.I4(),
		secs2.I8(),
		secs2.U1(),
		secs2.U2(),
		secs2.U4(),
		secs2.U8(),
		secs2.F4(),
		secs2.F8(),
		secs2.J(""),
		secs2.W(""),
		secs2.L(),
		secs2.NewEmptyItem(),
	}
	enc := NewEncoder()
	for _, it := range items {
		require.Equal(t, it.ToSML(), enc.Encode(it), "type %s", it.Type())
	}
}

func TestEncodeMessage_PackageShortcut(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.A("hi"))
	require.NoError(t, err)

	got, err := EncodeMessage(msg)
	require.NoError(t, err)
	want, err := NewEncoder().EncodeMessage(msg)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// Options are honored.
	got, err = EncodeMessage(msg, WithSFQuote(QuoteSingle))
	require.NoError(t, err)
	require.Equal(t, "'S1F1'\n<A[2] \"hi\">\n.", got)
}

// TestEncodeMessage_DecodeErrorPropagates verifies that EncodeMessage returns the lazy body-decode
// error (rather than swallowing it) and that MustEncodeMessage turns it into a diagnostic string
// instead of panicking.
func TestEncodeMessage_DecodeErrorPropagates(t *testing.T) {
	header := [10]byte{0, 1, 1, 1, 0, 0, 0, 0, 0, 1}
	// Malformed list: format byte 0x03 (list, 3-byte length field) claiming 0xFFFFFF children
	// with no following bytes.
	body := []byte{0x03, 0xFF, 0xFF, 0xFF}
	msgLen := uint32(10 + len(body))
	frame := make([]byte, 0, 4+msgLen)
	frame = binary.BigEndian.AppendUint32(frame, msgLen)
	frame = append(frame, header[:]...)
	frame = append(frame, body...)

	badMsg, err := hsms.DecodeHSMSMessage(frame)
	require.NoError(t, err, "frame-level decode must succeed; the body decode error is lazy")
	badDM, ok := badMsg.(*hsms.DataMessage)
	require.True(t, ok)

	_, err = EncodeMessage(badDM)
	require.Error(t, err)

	got := MustEncodeMessage(badDM)
	require.Contains(t, got, "<!sml encode error:")
}

// TestEncodeMessage_RoundTrip verifies EncodeMessage -> Parse -> Equal for a representative message.
func TestEncodeMessage_RoundTrip(t *testing.T) {
	orig, err := hsms.NewDataMessage(2, 3, true, 7, [4]byte{0, 0, 0, 1},
		secs2.L(secs2.A("hello"), secs2.I2(1, 2, 3), secs2.B(0x01, 0xFF)))
	require.NoError(t, err)

	out, err := EncodeMessage(orig)
	require.NoError(t, err)

	msgs, err := Parse(out)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	require.Equal(t, orig.Stream(), msgs[0].Stream())
	require.Equal(t, orig.Function(), msgs[0].Function())
	require.Equal(t, orig.WaitBit(), msgs[0].WaitBit())

	origItem, err := orig.Item()
	require.NoError(t, err)
	gotItem, err := msgs[0].Item()
	require.NoError(t, err)
	require.True(t, secs2.Equal(origItem, gotItem))
}
