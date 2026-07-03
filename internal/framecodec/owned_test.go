package framecodec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnedSECS2Body_AdoptIsZeroCopy(t *testing.T) {
	src := []byte{0x01, 0x02, 0x03}
	tok := AdoptSECS2Body(src)
	require.Equal(t, 3, tok.Len())
	require.Equal(t, src, tok.Bytes())
	// Zero-copy: Bytes() must return the SAME backing array, not a copy.
	src[0] = 0xFF
	require.Equal(t, byte(0xFF), tok.Bytes()[0], "AdoptSECS2Body must retain the slice, not clone it")
}

func TestOwnedSECS2Body_Nil(t *testing.T) {
	tok := AdoptSECS2Body(nil)
	require.Equal(t, 0, tok.Len())
	require.Nil(t, tok.Bytes())
}
