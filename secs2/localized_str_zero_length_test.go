package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A zero-length format-22 item (49 00) is an ordinary zero-length item (SEMI E5 §9.2.1):
// it carries no LSH, so it is a different wire value from LSH 0 with an empty string (49 02 00 00).

func TestLocalizedStr_ZeroLengthDecodes(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		pick func(t *testing.T, it Item) Item
	}{
		{
			name: "alone",
			data: []byte{0x49, 0x00},
			pick: func(_ *testing.T, it Item) Item { return it },
		},
		{
			name: "inside a list",
			data: []byte{0x01, 0x02, 0x49, 0x00, 0x41, 0x01, 'x'},
			pick: func(t *testing.T, it Item) Item {
				t.Helper()

				child, err := it.Get(0)
				require.NoError(t, err)

				return child
			},
		},
	}

	decoders := []struct {
		name   string
		decode func([]byte) (Item, error)
	}{
		{"Decode", Decode},
		{"DecodeOwned", func(b []byte) (Item, error) { return DecodeOwned(append([]byte(nil), b...)) }},
	}

	for _, tt := range tests {
		for _, d := range decoders {
			t.Run(tt.name+"/"+d.name, func(t *testing.T) {
				got, err := d.decode(tt.data)
				require.NoError(t, err)
				require.Equal(t, tt.data, got.ToBytes(), "the body must re-encode byte-exactly")

				w := tt.pick(t, got)
				require.True(t, w.IsLocalizedStr())
				require.Equal(t, 0, w.Size())

				lstr, ok := w.(*LocalizedStrItem)
				require.True(t, ok)
				require.False(t, lstr.HasLocalizedStrHeader())

				s, err := w.ToLocalizedStr()
				require.NoError(t, err)
				require.Empty(t, s)

				lsh, err := w.ToLocalizedStrHeader()
				require.NoError(t, err)
				require.Equal(t, uint16(0), lsh)

				require.Equal(t, []byte{0x49, 0x00}, w.ToBytes())
				require.Equal(t, 2, w.EncodedLen())
				require.Equal(t, "<W[0]>", w.ToSML())
			})
		}
	}
}

func TestLocalizedStr_OneByteBodyStillRejected(t *testing.T) {
	_, err := Decode([]byte{0x49, 0x01, 0x00})
	require.ErrorContains(t, err, "localized string payload too short: 1 bytes")
}

func TestNewEmptyLocalizedStrItem(t *testing.T) {
	it := NewEmptyLocalizedStrItem()
	require.NoError(t, it.Error())
	require.True(t, it.IsLocalizedStr())
	require.Equal(t, 0, it.Size())
	require.Equal(t, 2, it.EncodedLen())
	require.Equal(t, []byte{0x49, 0x00}, it.ToBytes())
	require.Equal(t, []byte{0xAA, 0x49, 0x00}, it.AppendTo([]byte{0xAA}))

	lstr, ok := it.(*LocalizedStrItem)
	require.True(t, ok)
	require.False(t, lstr.HasLocalizedStrHeader())

	withLSH, ok := NewLocalizedStrItem(LSHNone, "").(*LocalizedStrItem)
	require.True(t, ok)
	require.True(t, withLSH.HasLocalizedStrHeader())
	require.Equal(t, 2, withLSH.Size())
}

func TestLocalizedStr_ZeroLengthEquality(t *testing.T) {
	decoded, err := Decode([]byte{0x49, 0x00})
	require.NoError(t, err)

	require.True(t, Equal(decoded, NewEmptyLocalizedStrItem()))
	require.True(t, Equal(NewEmptyLocalizedStrItem(), decoded))
	require.False(t, Equal(decoded, NewLocalizedStrItem(LSHNone, "")),
		"49 00 and 49 02 00 00 are different wire values")
	require.False(t, Equal(NewLocalizedStrItem(LSHNone, ""), decoded))
}

func TestIsReservedLSH(t *testing.T) {
	tests := []struct {
		lsh  uint16
		want bool
	}{
		{0, true},
		{1, false},
		{LSHTraditionalChineseEUCTW, false},
		{15, true},
		{32767, true},
		{32768, false},
		{65535, false},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, IsReservedLSH(tt.lsh), "lsh %d", tt.lsh)
	}
}
