package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A clone's child must be an independent object: mutating it must not affect
// the original. Pre-fix ListItem.Clone is shallow (shares child pointers), so
// this fails.
func TestListItem_Clone_DeepIndependence(t *testing.T) {
	require := require.New(t)

	orig := L(A("alpha"), A("beta"))
	clone := orig.Clone()

	cloneChild, err := clone.Get(0)
	require.NoError(err)
	require.NoError(cloneChild.SetValues("MUTATED"))

	origChild, err := orig.Get(0)
	require.NoError(err)
	got, err := origChild.ToASCII()
	require.NoError(err)
	require.Equal("alpha", got, "mutating clone child must not change original")

	// And the clone actually took the mutation.
	gotClone, err := cloneChild.ToASCII()
	require.NoError(err)
	require.Equal("MUTATED", gotClone)
}

// Independence must hold recursively through nested lists.
func TestListItem_Clone_NestedIndependence(t *testing.T) {
	require := require.New(t)

	orig := L(L(A("deep")))
	clone := orig.Clone()

	innerClone, err := clone.Get(0)
	require.NoError(err)
	deepClone, err := innerClone.Get(0)
	require.NoError(err)
	require.NoError(deepClone.SetValues("X"))

	innerOrig, err := orig.Get(0)
	require.NoError(err)
	deepOrig, err := innerOrig.Get(0)
	require.NoError(err)
	got, err := deepOrig.ToASCII()
	require.NoError(err)
	require.Equal("deep", got, "nested clone child must be independent")
}

// SetValues([]byte) must not retain the caller's buffer: mutating the caller's
// slice afterward must not change the item. Fails pre-fix (the single-value
// []byte branch aliases the caller slice).
func TestASCIIItem_SetValues_CopiesByteInput(t *testing.T) {
	require := require.New(t)

	buf := []byte("abc")
	item := A("")
	require.NoError(item.SetValues(buf))

	buf[0] = 'X' // mutate the caller's buffer

	got, err := item.ToASCII()
	require.NoError(err)
	require.Equal("abc", got, "item must not alias the caller's []byte")
}

// A clone of an ASCII item built from []byte must be independent of the
// caller's buffer (covers the Clone backing-share). Fails pre-fix.
func TestASCIIItem_Clone_IndependentFromCallerBytes(t *testing.T) {
	require := require.New(t)

	buf := []byte("abc")
	orig := A("")
	require.NoError(orig.SetValues(buf))
	clone := orig.Clone()

	buf[0] = 'X' // mutate the caller's buffer

	gotOrig, err := orig.ToASCII()
	require.NoError(err)
	require.Equal("abc", gotOrig)

	gotClone, err := clone.ToASCII()
	require.NoError(err)
	require.Equal("abc", gotClone, "clone must not alias the caller's []byte")
}

// Same hazard reached through a ListItem deep clone with a []byte-built child.
func TestListItem_Clone_IndependentFromCallerBytes(t *testing.T) {
	require := require.New(t)

	buf := []byte("abc")
	child := A("")
	require.NoError(child.SetValues(buf))
	orig := L(child)
	clone := orig.Clone()

	buf[0] = 'X'

	cloneChild, err := clone.Get(0)
	require.NoError(err)
	got, err := cloneChild.ToASCII()
	require.NoError(err)
	require.Equal("abc", got)
}

// String-built items stay independent under SetValues (regression guard for the
// branch we did NOT change).
func TestASCIIItem_Clone_IndependentUnderSetValues(t *testing.T) {
	require := require.New(t)

	orig := A("hello")
	clone := orig.Clone()
	require.NoError(clone.SetValues("world"))

	gotOrig, err := orig.ToASCII()
	require.NoError(err)
	require.Equal("hello", gotOrig)

	gotClone, err := clone.ToASCII()
	require.NoError(err)
	require.Equal("world", gotClone)
}
