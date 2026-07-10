package secs2

// slabChunkSizes is the geometric chunk-size schedule shared by every scalar numeric slab: the
// first chunk holds 1 struct, then chunks grow 4, 16, 64, and cap at 128 thereafter. Cumulative
// capacities are 1, 5, 21, 85, 213, 341, ...
var slabChunkSizes = [...]int{1, 4, 16, 64, 128}

// numericSlab carves scalar numeric item structs (IntItem, UintItem, or FloatItem) from
// geometrically-growing chunks instead of heap-allocating one struct per decoded scalar leaf.
// It exists purely to cut per-leaf allocation count on the decode hot path; item semantics are
// unaffected.
//
// Retention model: a decoded scalar item's pointer-bearing fields (values, itemErr) are nil on
// the scalar decode branch — only rawPtr is set, and it points into the owned wire buffer that
// the surrounding item tree already keeps alive. Retaining one struct out of a chunk therefore
// pins that chunk (at most 128 structs) plus the same wire buffer the tree already pins; it
// never transitively pins a sibling's value slice or subtree the way slabbing a pointer-rich
// type would.
//
// Chunks are never grown in place — exhaustion allocates a brand-new chunk, so pointers already
// handed out by next remain valid for as long as the caller retains them.
type numericSlab[T any] struct {
	chunk    []T
	pos      int
	chunkIdx int
}

// decodeSlab bundles the three per-type numeric slabs used by a single Decode/DecodeOwned call.
// Decode and DecodeOwned each construct their own decodeSlab and thread it down decodeItem's
// recursion by pointer; nothing is shared across calls, so decodeSlab adds no concurrency
// surface and needs no synchronization.
type decodeSlab struct {
	ints   numericSlab[IntItem]
	uints  numericSlab[UintItem]
	floats numericSlab[FloatItem]
}

// next returns a pointer to a fresh zero-value T carved from the slab's current chunk,
// allocating a new chunk first if the current one is full or doesn't exist yet (first use).
func (s *numericSlab[T]) next() *T {
	if s.pos >= len(s.chunk) {
		s.chunk = make([]T, slabChunkSizes[s.chunkIdx])
		s.pos = 0

		if s.chunkIdx < len(slabChunkSizes)-1 {
			s.chunkIdx++
		}
	}

	item := &s.chunk[s.pos]
	s.pos++

	return item
}

// nextInt carves a fresh *IntItem from the int slab.
func (s *decodeSlab) nextInt() *IntItem { return s.ints.next() }

// nextUint carves a fresh *UintItem from the uint slab.
func (s *decodeSlab) nextUint() *UintItem { return s.uints.next() }

// nextFloat carves a fresh *FloatItem from the float slab.
func (s *decodeSlab) nextFloat() *FloatItem { return s.floats.next() }
