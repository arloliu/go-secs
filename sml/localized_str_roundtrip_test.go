package sml

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// W items round-trip through SML:
// the encoder writes double-quoted runs that escape only `"` and `\`,
// 0xHH tokens for bytes that are not printable UTF-8,
// and a leading decimal LSH token when the LSH is not UTF-8 (2).

// parseWItem parses one item from src with the given strict mode.
func parseWItem(t *testing.T, src string, strict bool) (secs2.Item, error) {
	t.Helper()

	p := NewParser(WithParserStrictMode(strict))
	p.input = src
	p.data = src
	p.len = len(src)
	p.pos = 0

	return p.parseItem()
}

func TestEncode_LocalizedStr(t *testing.T) {
	tests := []struct {
		name string
		item secs2.Item
		want string
	}{
		{"plain", secs2.NewUTF8StrItem("héllo"), `<W "héllo">`},
		{"empty", secs2.NewUTF8StrItem(""), `<W "">`},
		{"quote", secs2.NewUTF8StrItem(`a"b`), `<W "a\"b">`},
		{"single quote", secs2.NewUTF8StrItem("it's"), `<W "it's">`},
		{"backslash", secs2.NewUTF8StrItem(`C:\dir`), `<W "C:\\dir">`},
		{"angle bracket", secs2.NewUTF8StrItem("a>b"), `<W "a>b">`},
		{"newline", secs2.NewUTF8StrItem("a\nb"), `<W "a" 0x0A "b">`},
		{"only control", secs2.NewUTF8StrItem("\t"), `<W 0x09>`},
		{"invalid UTF-8", secs2.NewUTF8StrItem("a\xffb"), `<W "a" 0xFF "b">`},
		{"real U+FFFD", secs2.NewUTF8StrItem("\uFFFD"), "<W \"\uFFFD\">"},
		{"Shift-JIS", secs2.NewLocalizedStrItem(secs2.LSHShiftJIS, "\x82\xa0"), `<W 8 0x82 0xA0>`},
		{"ASCII LSH", secs2.NewLocalizedStrItem(secs2.LSHASCII, "test"), `<W 3 "test">`},
		{"LSH 0 empty", secs2.NewLocalizedStrItem(secs2.LSHNone, ""), `<W 0 "">`},
		{"zero-length", secs2.NewEmptyLocalizedStrItem(), `<W[0]>`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.item.ToSML())
			require.Equal(t, tt.want, Encode(tt.item), "the encoder must match ToSML")
		})
	}
}

func TestLocalizedStr_SMLRoundTrip(t *testing.T) {
	items := []secs2.Item{
		secs2.NewUTF8StrItem(""),
		secs2.NewUTF8StrItem("héllo 你好"),
		secs2.NewUTF8StrItem(`a"b`),
		secs2.NewUTF8StrItem("it's"),
		secs2.NewUTF8StrItem(`C:\dir\`),
		secs2.NewUTF8StrItem(`\'\"\\`),
		secs2.NewUTF8StrItem("a>b"),
		secs2.NewUTF8StrItem("a\nb\x00"),
		secs2.NewUTF8StrItem("a\xffb"),
		secs2.NewLocalizedStrItem(secs2.LSHShiftJIS, "\x82\xa0\x83\x5c"),
		secs2.NewLocalizedStrItem(secs2.LSHNone, "x"),
		secs2.NewLocalizedStrItem(65535, "custom"),
		secs2.NewEmptyLocalizedStrItem(),
	}

	for _, strict := range []bool{false, true} {
		for _, it := range items {
			src := it.ToSML()
			got, err := parseWItem(t, src, strict)
			require.NoError(t, err, "strict=%v %s", strict, src)
			require.Equal(t, it.ToBytes(), got.ToBytes(), "strict=%v %s", strict, src)
		}
	}
}

func TestParse_LocalizedStrGrammar(t *testing.T) {
	type want struct {
		bytes  []byte // nil means an error is expected
		errStr string
	}

	w := func(lsh uint16, s string) []byte { return secs2.NewLocalizedStrItem(lsh, s).ToBytes() }

	tests := []struct {
		src       string
		nonStrict want
		strict    want
	}{
		// new grammar, same result in both modes
		{`<W "a\"b">`, want{bytes: w(2, `a"b`)}, want{bytes: w(2, `a"b`)}},
		{`<W 'a\'b'>`, want{bytes: w(2, "a'b")}, want{bytes: w(2, "a'b")}},
		{`<W "a\\b">`, want{bytes: w(2, `a\b`)}, want{bytes: w(2, `a\b`)}},
		{`<W 'it\'s'>`, want{bytes: w(2, "it's")}, want{bytes: w(2, "it's")}},
		{`<W "it's">`, want{bytes: w(2, "it's")}, want{bytes: w(2, "it's")}},
		{`<W 'C:\dir'>`, want{bytes: w(2, `C:\dir`)}, want{bytes: w(2, `C:\dir`)}},
		{`<W "a\nb">`, want{bytes: w(2, `a\nb`)}, want{bytes: w(2, `a\nb`)}},
		{`<W "\x82\u00e9">`, want{bytes: w(2, `\x82\u00e9`)}, want{bytes: w(2, `\x82\u00e9`)}},
		{`<W "a>b">`, want{bytes: w(2, "a>b")}, want{bytes: w(2, "a>b")}},
		{`<W 'a' 0x0A "b">`, want{bytes: w(2, "a\nb")}, want{bytes: w(2, "a\nb")}},
		{"<W\n  'a'\n  0x0a\n>", want{bytes: w(2, "a\n")}, want{bytes: w(2, "a\n")}},
		{`<W 8 0x82 0xA0>`, want{bytes: w(8, "\x82\xa0")}, want{bytes: w(8, "\x82\xa0")}},
		{`<W 8>`, want{bytes: w(8, "")}, want{bytes: w(8, "")}},
		{`<W 2 'x'>`, want{bytes: w(2, "x")}, want{bytes: w(2, "x")}},
		{`<W 0 ''>`, want{bytes: w(0, "")}, want{bytes: w(0, "")}},
		{`<W 65535 'x'>`, want{bytes: w(65535, "x")}, want{bytes: w(65535, "x")}},
		{`<W 'a' 'b'>`, want{bytes: w(2, "ab")}, want{bytes: w(2, "ab")}},
		{`<W>`, want{bytes: w(2, "")}, want{bytes: w(2, "")}},
		{`<W[0]>`, want{bytes: []byte{0x49, 0x00}}, want{bytes: []byte{0x49, 0x00}}},

		// not in the grammar: non-strict falls back to the verbatim rule, strict rejects
		{`<W 'it's'>`, want{bytes: w(2, "it's")}, want{errStr: "Localized string"}},
		{`<W 'C:\'>`, want{bytes: w(2, `C:\`)}, want{errStr: "Localized string"}},
		{`<W 'a' 8>`, want{errStr: "Localized string"}, want{errStr: "Localized string"}},
		{`<W 65536 'x'>`, want{errStr: "Localized string"}, want{errStr: "Localized string"}},
		{`<W 0x100>`, want{errStr: "Localized string"}, want{errStr: "Localized string"}},
		{`<W[1] abcd'>`, want{errStr: "invalid quote for Localized string"}, want{errStr: "Localized string"}},
		{`<W[0] 8>`, want{errStr: "zero-length Localized string"}, want{errStr: "zero-length Localized string"}},
		{`<W "abc`, want{errStr: "Localized string"}, want{errStr: "Localized string"}},
	}

	for _, tt := range tests {
		for _, mode := range []struct {
			strict bool
			want   want
		}{{false, tt.nonStrict}, {true, tt.strict}} {
			item, err := parseWItem(t, tt.src, mode.strict)
			if mode.want.bytes == nil {
				require.ErrorContains(t, err, mode.want.errStr, "strict=%v %s", mode.strict, tt.src)

				continue
			}

			require.NoError(t, err, "strict=%v %s", mode.strict, tt.src)
			require.Equal(t, mode.want.bytes, item.ToBytes(), "strict=%v %s", mode.strict, tt.src)
		}
	}
}

// TestParse_LocalizedStrInMessage checks that a W item ends exactly at its own `>`,
// so the items after it in a list parse normally in both modes.
func TestParse_LocalizedStrInMessage(t *testing.T) {
	src := "S1F1\n<L[3]\n  <W 'a>b' 0x0A>\n  <W 8 0x82 0xA0>\n  <A 'x'>\n>\n."
	want := secs2.NewListItem(
		secs2.NewUTF8StrItem("a>b\n"),
		secs2.NewLocalizedStrItem(secs2.LSHShiftJIS, "\x82\xa0"),
		secs2.NewASCIIItem("x"),
	).ToBytes()

	for _, parse := range []func(string) ([]*hsms.DataMessage, error){Parse, ParseStrict} {
		msgs, err := parse(src)
		require.NoError(t, err)
		require.Len(t, msgs, 1)

		item, err := msgs[0].Item()
		require.NoError(t, err)
		require.Equal(t, want, item.ToBytes())
	}
}

// FuzzLocalizedStr_SMLRoundTrip checks that any W item survives ToSML and a parse in both modes byte for byte.
func FuzzLocalizedStr_SMLRoundTrip(f *testing.F) {
	f.Add(uint16(2), []byte("hello"))
	f.Add(uint16(2), []byte(`a"b\'c>d`))
	f.Add(uint16(8), []byte{0x82, 0xa0, 0x83, 0x5c})
	f.Add(uint16(0), []byte{})
	f.Add(uint16(2), []byte{0x00, '\n', 0xff})

	f.Fuzz(func(t *testing.T, lsh uint16, value []byte) {
		if len(value)+2 > secs2.MaxByteSize {
			return
		}

		it := secs2.NewLocalizedStrItem(lsh, string(value))
		src := it.ToSML()

		for _, strict := range []bool{false, true} {
			got, err := parseWItem(t, src, strict)
			require.NoError(t, err, "strict=%v %s", strict, src)
			require.Equal(t, it.ToBytes(), got.ToBytes(), "strict=%v %s", strict, src)
		}
	})
}
