package errtext

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNamespaceParameter requires every helper to dedup the namespace it is given, and no other.
func TestNamespaceParameter(t *testing.T) {
	t.Parallel()

	cause := errors.New("redact: a: redact: b: tracepack: c: tracepack: d")
	err := Wrapf("redact: ", "redact: seq %d: %w", 3, cause)
	require.EqualError(t, err, "redact: seq 3: a: b: tracepack: c: tracepack: d")
	require.Same(t, cause, errors.Unwrap(err))

	err = Join("redact: ", cause, nil, errors.New("redact: e: redact: f"))
	require.EqualError(t, err, "redact: a: b: tracepack: c: tracepack: d\nredact: e: f")
	require.ErrorIs(t, err, cause)
	require.NoError(t, Join("redact: ", nil))

	require.Equal(t, "tracepack: c: d\nredact: a: redact: b", Dedup("tracepack: ", "tracepack: c: tracepack: d\nredact: a: redact: b"))
}
