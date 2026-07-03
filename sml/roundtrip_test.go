package sml

import (
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_MessageLevel(t *testing.T) {
	srcs := []string{
		"S1F1\n<A[5] \"hello\">\n.",
		"S2F3 W\n<L[2]\n  <I4[3] 1 2 3>\n  <B[2] 0b1 0b11111111>\n>\n.", // odd function: W-bit allowed
		"S6F11\n<L[0]>\n.",
		"S1F13\n<BOOLEAN[2] True False>\n.",
		"S5F1\n<J[5] \"hello\">\n.", // JIS8 round-trip (requires the Step 6b parseJIS8 fix)
		"S7F1\n<L[2]\n  <U2[2] 10 20>\n  <F8[1] 1.5>\n>\n.",
		"S9F1\n<W \"loc\">\n.", // localized-string (W) round-trip — every item type per spec §10
	}
	enc := NewEncoder()
	for _, src := range srcs {
		msgs, err := Parse(src)
		require.NoError(t, err, "parse %q", src)
		require.Len(t, msgs, 1)

		out, err := enc.EncodeMessage(msgs[0])
		require.NoError(t, err)

		// re-parse the encoded output; header fields AND body must match
		msgs2, err := Parse(out)
		require.NoError(t, err, "reparse %q", out)
		require.Len(t, msgs2, 1)

		require.Equal(t, msgs[0].Stream(), msgs2[0].Stream(), "stream round-trip for %q", src)
		require.Equal(t, msgs[0].Function(), msgs2[0].Function(), "function round-trip for %q", src)
		require.Equal(t, msgs[0].WaitBit(), msgs2[0].WaitBit(), "wbit round-trip for %q", src)

		b1, err := msgs[0].Item()
		require.NoError(t, err, "Item() on parsed msg for %q", src)
		b2, err := msgs2[0].Item()
		require.NoError(t, err, "Item() on reparsed msg for %q", src)
		require.Equal(t, b1.ToBytes(), b2.ToBytes(), "body round-trip for %q", src)
	}
}

func TestParse_RejectsWaitBitOnEvenFunction(t *testing.T) {
	// v2 hsms.NewDataMessage rejects W-bit on an even function (E37 §8.3.3.3).
	_, err := Parse("S2F2 W\n<A[0] \"\">\n.")
	require.Error(t, err)
}

func TestRoundTrip_EmptyBody(t *testing.T) {
	// §10 "empty body" at the MESSAGE level: EncodeMessage(empty) -> Parse -> empty.
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)
	out, err := NewEncoder().EncodeMessage(msg)
	require.NoError(t, err)
	msgs, err := Parse(out)
	require.NoError(t, err, "reparse empty-body message %q", out)
	require.Len(t, msgs, 1)
	require.Equal(t, uint8(1), msgs[0].Stream())
	require.Equal(t, uint8(1), msgs[0].Function())
	body, err := msgs[0].Item()
	require.NoError(t, err)
	require.True(t, body.IsEmpty(), "empty body must round-trip to an empty item")
}

func TestRoundTrip_QuotedHeader(t *testing.T) {
	// §10 "all S/F/W header forms": the parser must read back every SF quote style.
	msg, err := hsms.NewDataMessage(1, 1, true, 0, [4]byte{}, secs2.A("x")) // F1 odd -> W legal
	require.NoError(t, err)
	for _, q := range []QuoteStyle{QuoteDouble, QuoteSingle, QuoteNone} {
		out, err := NewEncoder(WithSFQuote(q)).EncodeMessage(msg)
		require.NoError(t, err)
		msgs, err := Parse(out)
		require.NoError(t, err, "reparse SF-quote %v header %q", q, out)
		require.Len(t, msgs, 1)
		require.Equal(t, uint8(1), msgs[0].Stream())
		require.Equal(t, uint8(1), msgs[0].Function())
		require.True(t, msgs[0].WaitBit())
	}
}

func TestRoundTrip_StrictNonPrintable(t *testing.T) {
	// Strings with non-printable bytes, INCLUDING ones that END in a non-printable
	// (exercises the >-terminated numeric-token return fixed in Task 1 Step 6).
	cases := []string{"a\nb", "\n", "a\n", "\n\n", "\tx\r"}
	enc := NewEncoder(WithEncoderStrictMode(true))
	for _, s := range cases {
		item := secs2.A(s)
		src := "S1F1\n" + enc.Encode(item) + "\n."
		msgs, err := ParseStrict(src)
		require.NoError(t, err, "strict parse of %q (encoded %q)", s, enc.Encode(item))
		require.Len(t, msgs, 1)
		got, err := msgs[0].Item()
		require.NoError(t, err)
		gotStr, _ := got.ToASCII()
		require.Equal(t, s, gotStr, "strict round-trip for %q", s)
	}
}

func TestEncoder_ConcurrentSafe(t *testing.T) {
	enc := NewEncoder()
	it := secs2.L(secs2.A("x"), secs2.I4(1, 2, 3))
	want := it.ToSML()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() { require.Equal(t, want, enc.Encode(it)) })
	}
	wg.Wait()
}

func TestParse_NoGlobalState(t *testing.T) {
	// A strict-only ASCII form ("a" 0x0A "b") must parse under ParseStrict and
	// reproduce the non-printable byte; the non-strict parser interprets 0x0A as
	// literal text and returns a DIFFERENT value (not "a\nb"). Interleaving the
	// two must not leak strictness between instances (proves per-instance, not
	// package-global, strict state).
	strictSrc := "S1F1\n<A[3] \"a\" 0x0A \"b\">\n."
	for i := 0; i < 3; i++ {
		// Strict parse: 0x0A is decoded as a byte, producing "a\nb".
		ms, err := ParseStrict(strictSrc)
		require.NoError(t, err)
		body, err := ms[0].Item()
		require.NoError(t, err)
		s, _ := body.ToASCII()
		require.Equal(t, "a\nb", s, "strict parse must interpret 0x0A as a byte")

		// Non-strict parse of the same input: succeeds but 0x0A is NOT decoded —
		// the value is a literal string including quotes and "0x0A" text, not "a\nb".
		// If a leaked global strict flag ever caused non-strict to decode 0x0A, this
		// assertion would catch it.
		ns, err := Parse(strictSrc)
		require.NoError(t, err, "non-strict Parse must not error on 0xHH tokens")
		nsBody, err := ns[0].Item()
		require.NoError(t, err)
		nsStr, _ := nsBody.ToASCII()
		require.NotEqual(t, "a\nb", nsStr, "non-strict must not interpret 0xHH tokens as bytes (leaked global strict flag?)")
	}
}
