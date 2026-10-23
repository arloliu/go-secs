package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/stretchr/testify/require"
)

// TestAbort checks that Abort builds a header-only, W-bit-clear function 0 of every stream E5 can name.
func TestAbort(t *testing.T) {
	t.Parallel()

	for stream := byte(1); stream <= 127; stream++ {
		msg := gem.Abort(stream)

		require.Equal(t, stream, msg.StreamCode(), "stream %d", stream)
		require.Zero(t, msg.FunctionCode(), "stream %d", stream)
		require.False(t, msg.WaitBit(), "stream %d", stream)
		require.True(t, msg.Item().IsEmpty(), "stream %d", stream)
		require.Empty(t, msg.Item().ToBytes(), "stream %d", stream)
		require.NoError(t, msg.Item().Error(), "stream %d", stream)
	}
}
