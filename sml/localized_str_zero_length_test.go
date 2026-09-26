package sml

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// <W[0]> is the SML spelling of a zero-length format-22 item (49 00);
// <W> keeps meaning a UTF-8 item with empty text (49 02 00 02).

func TestParseItem_LocalizedStrZeroLength(t *testing.T) {
	tests := []struct {
		sml    string
		want   []byte
		errStr string
	}{
		{sml: "<W[0]>", want: []byte{0x49, 0x00}},
		{sml: "<W [0] >", want: []byte{0x49, 0x00}},
		{sml: "<W>", want: []byte{0x49, 0x02, 0x00, 0x02}},
		{sml: "<W ''>", want: []byte{0x49, 0x02, 0x00, 0x02}},
		{sml: "<W[2] 'ab'>", want: []byte{0x49, 0x04, 0x00, 0x02, 'a', 'b'}},
		{sml: "<W[0] ''>", errStr: "zero-length Localized string"},
		{sml: "<W[0] 'x'>", errStr: "zero-length Localized string"},
	}

	for _, tt := range tests {
		t.Run(tt.sml, func(t *testing.T) {
			p := NewParser()
			p.input = tt.sml
			p.data = tt.sml
			p.len = len(tt.sml)
			p.pos = 0

			item, err := p.parseItem()
			if tt.errStr != "" {
				require.ErrorContains(t, err, tt.errStr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, item.ToBytes())
		})
	}
}

func TestEncode_LocalizedStrZeroLength(t *testing.T) {
	require.Equal(t, "<W[0]>", Encode(secs2.NewEmptyLocalizedStrItem()))
	require.Equal(t, `<W "">`, Encode(secs2.NewUTF8StrItem("")))
}

func TestParse_LocalizedStrZeroLengthMessage(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []byte
	}{
		{"exact size", "S1F1\n<W[0]>\n.", []byte{0x49, 0x00}},
		{"range 0..0", "S1F1\n<W[0..0]>\n.", []byte{0x49, 0x00}},
		{"max only", "S1F1\n<W[..0]>\n.", []byte{0x49, 0x00}},
		{"comment after size", "S1F1\n<W[0] // not supplied\n>\n.", []byte{0x49, 0x00}},
		{
			"nested",
			"S1F1\n<L[2]\n  <L[1] <W[0]>>\n  <W 'a'>\n>\n.",
			[]byte{0x01, 0x02, 0x01, 0x01, 0x49, 0x00, 0x49, 0x03, 0x00, 0x02, 'a'},
		},
	}

	parsers := []struct {
		name  string
		parse func(string) ([]*hsms.DataMessage, error)
	}{
		{"Parse", Parse},
		{"ParseStrict", ParseStrict},
	}

	for _, tt := range tests {
		for _, p := range parsers {
			t.Run(tt.name+"/"+p.name, func(t *testing.T) {
				msgs, err := p.parse(tt.src)
				require.NoError(t, err)
				require.Len(t, msgs, 1)

				item, err := msgs[0].Item()
				require.NoError(t, err)
				require.Equal(t, tt.want, item.ToBytes())
			})
		}
	}
}
