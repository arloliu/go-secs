package wire

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

func TestOwnedBytes_RawFrame(t *testing.T) {
	src := []byte{1, 2, 3}
	got, ok := OwnedBytes(AdoptBody(src))
	require.True(t, ok)
	require.Equal(t, src, got)
	src[0] = 9
	require.Equal(t, byte(9), got[0], "must alias, not copy")
}

func TestOwnedBytes_TreeBodyReturnsFalse(t *testing.T) {
	_, ok := OwnedBytes(FromItem(secs2.NewEmptyItem()))
	require.False(t, ok)
}
