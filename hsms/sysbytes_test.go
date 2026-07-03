package hsms

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSysBytesGen_UniqueMonotonic verifies that successive calls to next() never
// return a repeated value (E37 §8.2.6.8 uniqueness invariant).
func TestSysBytesGen_UniqueMonotonic(t *testing.T) {
	var g sysBytesGen
	seen := map[[4]byte]bool{}
	prev := g.next()
	seen[prev] = true
	for range 100000 {
		cur := g.next()
		require.False(t, seen[cur], "System Bytes must be unique across open transactions (§8.2.6.8)")
		seen[cur] = true
	}
}

// TestSysBytesGen_ConcurrentUnique verifies that concurrent calls to next() from
// multiple goroutines never produce a duplicate value.
func TestSysBytesGen_ConcurrentUnique(t *testing.T) {
	var g sysBytesGen
	const n = 10000
	var mu sync.Mutex
	seen := map[[4]byte]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range n {
				b := g.next()
				mu.Lock()
				require.False(t, seen[b])
				seen[b] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}
