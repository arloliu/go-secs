package format

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOffsetError(t *testing.T) {
	t.Parallel()

	err := &OffsetError{Structure: "file header", Field: "header_crc", Offset: 76, Err: ErrCRC}

	require.ErrorIs(t, err, ErrCRC)
	require.Contains(t, err.Error(), "file header")
	require.Contains(t, err.Error(), "header_crc")
	require.Contains(t, err.Error(), "76")
}

func TestOffsetError_Unwrap(t *testing.T) {
	t.Parallel()

	err := &OffsetError{Structure: "block envelope", Field: "magic", Offset: 0, Err: ErrBadMagic}

	require.Equal(t, ErrBadMagic, errors.Unwrap(err))
}

// Sentinel errors are distinct so callers can tell decode failure modes apart with errors.Is.
func TestSentinelErrors_Distinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{ErrBadMagic, ErrCRC, ErrLimit, ErrSchema, ErrShort, ErrCorrupt}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}

			require.NotErrorIs(t, a, b)
		}
	}
}
