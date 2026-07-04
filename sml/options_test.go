package sml

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestEncoder_Options(t *testing.T) {
	it := secs2.A("hi")
	require.Equal(t, `<A[2] 'hi'>`, NewEncoder(WithASCIIQuote(QuoteSingle)).Encode(it))
	require.Equal(t, `<A[2] "hi">`, NewEncoder(WithASCIIQuote(QuoteNone)).Encode(it)) // None→Double for ASCII

	list := secs2.L(secs2.A("x"))
	require.Contains(t, NewEncoder(WithIndent("\t")).Encode(list), "\t<A[1]")
}

func TestEncoder_WithBinaryStyle(t *testing.T) {
	it := secs2.B(0x03, 0x04)

	require.Equal(t, "<B[2] 0x03 0x04>", NewEncoder().Encode(it), "default must be hex")
	require.Equal(t, "<B[2] 0x03 0x04>", NewEncoder(WithBinaryStyle(BinaryHex)).Encode(it))
	require.Equal(t, "<B[2] 0b11 0b100>", NewEncoder(WithBinaryStyle(BinaryLiteral)).Encode(it))

	// The parser reads both forms regardless of the encoder's chosen style.
	for _, sml := range []string{"S1F1\n<B[2] 0x03 0x04>\n.", "S1F1\n<B[2] 0b11 0b100>\n."} {
		msgs, err := Parse(sml)
		require.NoError(t, err)
		item, err := msgs[0].Item()
		require.NoError(t, err)
		require.True(t, secs2.Equal(it, item), "parser must read %q as the same logical value", sml)
	}
}

// EncodeMessage has no secs2 ToSML to pin against (DataMessage has no SML method
// on v2), so golden-test the canonical header forms explicitly per SF quote style.
func TestEncodeMessage_HeaderGolden(t *testing.T) {
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.A("hi"))
	require.NoError(t, err)

	got, err := NewEncoder().EncodeMessage(msg) // default: UNQUOTED S/F
	require.NoError(t, err)
	require.Equal(t, "S1F1\n<A[2] \"hi\">\n.", got)

	got, _ = NewEncoder(WithSFQuote(QuoteDouble)).EncodeMessage(msg)
	require.Equal(t, "\"S1F1\"\n<A[2] \"hi\">\n.", got)

	got, _ = NewEncoder(WithSFQuote(QuoteSingle)).EncodeMessage(msg)
	require.Equal(t, "'S1F1'\n<A[2] \"hi\">\n.", got)

	// W-bit on an odd function renders " W" after the S/F token.
	wmsg, err := hsms.NewDataMessage(1, 1, true, 0, [4]byte{}, secs2.A("hi"))
	require.NoError(t, err)
	got, _ = NewEncoder().EncodeMessage(wmsg)
	require.Equal(t, "S1F1 W\n<A[2] \"hi\">\n.", got)
}
