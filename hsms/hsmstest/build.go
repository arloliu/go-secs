package hsmstest

import (
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// RequireBuild calls b.Build(), fails t (via testify require) if it errors, and returns the built
// message — for test call sites that only care about the happy path and don't want to repeat a
// require.NoError two-liner at every builder use.
func RequireBuild(t testing.TB, b *hsms.DataMessageBuilder) *hsms.DataMessage {
	t.Helper()

	msg, err := b.Build()
	require.NoError(t, err)

	return msg
}
