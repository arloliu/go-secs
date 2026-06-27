package secs2

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SetValues must invalidate the lazily-built rawBytes serialization cache.
// Without invalidation, ToBytes() after a SetValues() on an already-serialized
// item returns the stale bytes of the previous value. Covers the four cached
// item types (ASCII, Binary, Boolean, List).
func TestSetValues_InvalidatesRawBytesCache(t *testing.T) {
	tests := []struct {
		name   string
		build  func() Item
		mutate func(Item) error
		fresh  func() Item // an item constructed directly with the mutated value
	}{
		{
			name:   "ascii",
			build:  func() Item { return A("hello") },
			mutate: func(it Item) error { return it.SetValues("world") },
			fresh:  func() Item { return A("world") },
		},
		{
			name:   "binary",
			build:  func() Item { return B(1, 2, 3) },
			mutate: func(it Item) error { return it.SetValues(9, 9) },
			fresh:  func() Item { return B(9, 9) },
		},
		{
			name:   "boolean",
			build:  func() Item { return BOOLEAN(true) },
			mutate: func(it Item) error { return it.SetValues(false) },
			fresh:  func() Item { return BOOLEAN(false) },
		},
		{
			name:   "list",
			build:  func() Item { return L(A("x")) },
			mutate: func(it Item) error { return it.SetValues(A("y"), A("z")) },
			fresh:  func() Item { return L(A("y"), A("z")) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)

			it := tc.build()
			_ = it.ToBytes() // populate the rawBytes cache

			require.NoError(tc.mutate(it))

			// After mutation, ToBytes must reflect the new value, not the cached
			// serialization of the old one.
			require.Equal(tc.fresh().ToBytes(), it.ToBytes(),
				"ToBytes after SetValues returned a stale cached serialization")
		})
	}
}
