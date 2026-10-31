package tracepack

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSentinelParents requires each sentinel of a family to match its parent, directly and wrapped,
// and no parent, sibling or other family to match it.
func TestSentinelParents(t *testing.T) {
	t.Parallel()

	families := []struct {
		parent   error
		children []error
	}{
		{ErrCorrupt, []error{ErrChecksum, ErrInvalidFooter}},
		{ErrInvalidArgument, []error{ErrInvalidQuery, ErrInvalidKey}},
	}
	for i, f := range families {
		require.NoError(t, errors.Unwrap(f.parent), "%v has no parent", f.parent)
		for _, child := range f.children {
			require.ErrorIs(t, child, f.parent)
			require.ErrorIs(t, fmt.Errorf("context: %w", child), f.parent)
			require.NotErrorIs(t, f.parent, child, "no reverse relation")
			require.Same(t, f.parent, errors.Unwrap(child), "the parent link of %v", child)
			for j, other := range families {
				if j == i {
					continue
				}
				require.NotErrorIs(t, child, other.parent, "%v is outside %v's family", child, other.parent)
				for _, o := range other.children {
					require.NotErrorIs(t, child, o)
				}
			}
		}
		require.NotErrorIs(t, f.children[0], f.children[1], "siblings are distinct")
		require.NotErrorIs(t, f.children[1], f.children[0], "siblings are distinct")
	}

	for _, s := range []error{ErrNotFinalized, ErrNotTracepack, ErrUnsupportedFormat, ErrReadLimit, ErrRedactionDefect} {
		require.NotErrorIs(t, s, ErrCorrupt, "%v is not corrupt", s)
	}
	require.NotErrorIs(t, ErrNotFinalized, ErrChecksum)
	require.NoError(t, errors.Unwrap(ErrNotFinalized), "ErrNotFinalized has no parent")
}

// TestSentinelTexts requires the existing sentinels that gained a parent to keep their texts,
// and the new ones to read in the module's namespace.
func TestSentinelTexts(t *testing.T) {
	t.Parallel()

	for want, err := range map[string]error{
		"tracepack: checksum mismatch":  ErrChecksum,
		"tracepack: invalid footer":     ErrInvalidFooter,
		"tracepack: invalid query":      ErrInvalidQuery,
		"tracepack: invalid object key": ErrInvalidKey,
		"tracepack: corrupt data":       ErrCorrupt,
		"tracepack: pack not finalized": ErrNotFinalized,
		"tracepack: invalid argument":   ErrInvalidArgument,
	} {
		require.EqualError(t, err, want)
	}
}

// TestSentinelComparable requires a sentinel with a parent to stay one comparable value:
// a bare return compares equal to it, and to nothing else.
func TestSentinelComparable(t *testing.T) {
	t.Parallel()

	err := ErrChecksum
	require.True(t, err == ErrChecksum) //nolint:errorlint // the comparison a caller may make of a bare return
	require.False(t, err == ErrCorrupt) //nolint:errorlint // the comparison a caller may make of a bare return
	require.NotErrorIs(t, newChildSentinel(ErrChecksum.Error(), ErrCorrupt), ErrChecksum, "identity, not text")
}

// TestArgumentMarker requires argument to match ErrInvalidArgument without changing the text,
// and refuse to mark it unless an external error or the writer's prior state comes with it.
func TestArgumentMarker(t *testing.T) {
	t.Parallel()

	inner := errors.New("tracepack: WriterOptions.Meta is nil")
	err := argument(inner)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorIs(t, err, inner)
	require.EqualError(t, err, inner.Error())
	requireRefused(t, refuse(err), ErrInvalidArgument)

	for name, e := range map[string]error{
		"external ErrInvalidQuery": external(fmt.Errorf("sink: %w", ErrInvalidQuery)),
		"external ErrInvalidKey":   external(fmt.Errorf("sink: %w", ErrInvalidKey)),
		"external beside argument": errors.Join(argument(inner), external(errInjected)),
		"ErrWriterFailed":          fmt.Errorf("%w: %w", ErrWriterFailed, argument(inner)),
		"ErrClosed":                fmt.Errorf("%w: %w", ErrClosed, argument(inner)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := refuse(e)
			require.Same(t, e, got, "returned unchanged")
			requireNotRefused(t, got)
			require.ErrorIs(t, got, ErrInvalidArgument)
		})
	}
}
