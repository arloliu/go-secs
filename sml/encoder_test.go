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

// TestEncode_PackageShortcut verifies the package-level Encode(item) shortcut equals
// item.ToSML() (the documented byte-for-byte oracle) and, for a representative subset,
// a hand-written literal SML string — so the test is not purely circular against ToSML.
func TestEncode_PackageShortcut(t *testing.T) {
	tests := []struct {
		name string
		item secs2.Item
		want string
	}{
		{"ascii", secs2.A("hello"), `<A[5] "hello">`},
		{"uint4", secs2.U4(1, 2, 3), "<U4[3] 1 2 3>"},
		{"list", secs2.L(secs2.A("a"), secs2.U4(1, 2), secs2.I1(-1)),
			"<L[3]\n  <A[1] \"a\">\n  <U4[2] 1 2>\n  <I1[1] -1>\n>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Encode(tt.item)
			require.Equal(t, tt.want, got, "hand-written literal SML")
			require.Equal(t, tt.item.ToSML(), got, "must equal item.ToSML() byte-for-byte")
		})
	}
}

// TestEncodeStrict_PackageShortcut verifies EncodeStrict(item):
//   - for items with no non-printable bytes, is identical to the non-strict Encode(item)
//     and to item.ToSML();
//   - for an ASCII item containing a non-printable byte, escapes it as a 0xHH token
//     (per WithEncoderStrictMode's doc: "escapes/hex-encodes non-printable ASCII bytes
//     so the output is valid, round-trippable SML"), diverging from the non-strict form
//     which embeds the raw byte.
func TestEncodeStrict_PackageShortcut(t *testing.T) {
	t.Run("matches non-strict when nothing needs escaping", func(t *testing.T) {
		item := secs2.L(secs2.A("hello"), secs2.U4(1, 2, 3))
		require.Equal(t, Encode(item), EncodeStrict(item))
		require.Equal(t, item.ToSML(), EncodeStrict(item))
	})

	t.Run("escapes non-printable bytes as 0xHH tokens", func(t *testing.T) {
		item := secs2.A("a\nb")
		nonStrict := Encode(item)
		strict := EncodeStrict(item)

		require.Equal(t, `<A[3] "a`+"\n"+`b">`, nonStrict, "non-strict embeds the raw byte")
		require.Equal(t, `<A[3] "a" 0x0A "b">`, strict, "strict escapes the non-printable byte")
		require.NotEqual(t, nonStrict, strict)

		// Strict output must round-trip through ParseStrict back to the original value.
		msgs, err := ParseStrict("S1F1\n" + strict + "\n.")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		gotItem, err := msgs[0].Item()
		require.NoError(t, err)
		gotStr, err := gotItem.ToASCII()
		require.NoError(t, err)
		require.Equal(t, "a\nb", gotStr)
	})
}

// TestAppendEncode verifies (*Encoder).AppendEncode appends the item's encoded SML text
// to dst and returns the grown slice, matching Encode(item)'s bytes, while preserving
// whatever was already in dst (append semantics, not overwrite).
func TestAppendEncode(t *testing.T) {
	enc := NewEncoder()
	item := secs2.L(secs2.A("a"), secs2.U4(1, 2))
	want := enc.Encode(item)

	t.Run("empty dst", func(t *testing.T) {
		got := enc.AppendEncode(nil, item)
		require.Equal(t, want, string(got))
	})

	t.Run("preserves existing prefix", func(t *testing.T) {
		prefix := []byte("PREFIX:")
		got := enc.AppendEncode(prefix, item)
		require.Equal(t, "PREFIX:"+want, string(got))
		// The original prefix bytes must be untouched.
		require.Equal(t, "PREFIX:", string(prefix))
	})

	t.Run("strict encoder", func(t *testing.T) {
		strictEnc := NewEncoder(WithEncoderStrictMode(true))
		nonPrintable := secs2.A("a\nb")
		got := strictEnc.AppendEncode([]byte("X"), nonPrintable)
		require.Equal(t, "X"+strictEnc.Encode(nonPrintable), string(got))
	})
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
