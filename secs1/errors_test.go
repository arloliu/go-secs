package secs1

import (
	"errors"
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestErrSendFailed_IdentityPreservedAfterRetype is the teeth of the retype:
// ErrSendFailed gained a dynamic type (transientError) so it satisfies hsms.TransientError,
// but the var itself is unchanged.
// errors.Is comparisons against it — plain, wrapped, or errors.Join'd — must keep working exactly as they did before the marker existed.
func TestErrSendFailed_IdentityPreservedAfterRetype(t *testing.T) {
	require.ErrorIs(t, ErrSendFailed, ErrSendFailed)
	require.ErrorIs(t, fmt.Errorf("send block: %w", ErrSendFailed), ErrSendFailed)
	require.ErrorIs(t, errors.Join(errors.New("unrelated"), ErrSendFailed), ErrSendFailed)

	// The wire text is unchanged by the retype.
	require.Equal(t, "secs1: block send failed, retries exhausted", ErrSendFailed.Error())
}

// TestErrSendFailed_ClassifiesTransient proves the retype actually reaches hsms.IsTransient: a
// line failure (RTY exhausted) is retryable, but not a timeout — it is diagnosed by retry count,
// not by a timer expiring.
func TestErrSendFailed_ClassifiesTransient(t *testing.T) {
	require.True(t, hsms.IsTransient(ErrSendFailed))
	require.False(t, hsms.IsTimeout(ErrSendFailed))

	wrapped := fmt.Errorf("line: %w", ErrSendFailed)
	require.True(t, hsms.IsTransient(wrapped))
	require.False(t, hsms.IsTimeout(wrapped))
}

// TestSecs1UnmarkedSentinels_ClassifyByDefault proves ErrMessageTooLarge and ErrInvalidHeader,
// left unmarked, fall through hsms's classification default (rule 4): neither transient nor a
// timeout, without secs1 needing its own marker machinery for them.
func TestSecs1UnmarkedSentinels_ClassifyByDefault(t *testing.T) {
	for _, err := range []error{ErrMessageTooLarge, ErrInvalidHeader} {
		require.False(t, hsms.IsTransient(err))
		require.False(t, hsms.IsTimeout(err))
	}
}
