package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

func TestDecodeSECS2Item_JIS8RoundTrip(t *testing.T) {
	require := require.New(t)
	src := secs2.J("abc")
	got, err := DecodeSECS2Item(src.ToBytes())
	require.NoError(err)
	require.Equal(src.ToBytes(), got.ToBytes())
	s, err := got.ToJIS8()
	require.NoError(err)
	require.Equal("abc", s)
}

func TestDecodeSECS2Item_LocalizedStrRoundTrip(t *testing.T) {
	require := require.New(t)
	// secs2.W is NewUTF8StrItem(value) — use NewLocalizedStrItem to set a specific LSH
	src := secs2.NewLocalizedStrItem(0x1234, "héllo")
	got, err := DecodeSECS2Item(src.ToBytes())
	require.NoError(err)
	require.Equal(src.ToBytes(), got.ToBytes())
	s, err := got.ToLocalizedStr()
	require.NoError(err)
	require.Equal("héllo", s)
	lsh, err := got.ToLocalizedStrHeader()
	require.NoError(err)
	require.Equal(uint16(0x1234), lsh)
}
