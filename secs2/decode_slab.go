package secs2

// slabChunkSizes is the geometric chunk-size schedule used by all item slabs: the
// first chunk holds 1 struct, then chunks grow 4, 16, 64, and cap at 128 thereafter. Cumulative
// capacities are 1, 5, 21, 85, 213, 341, ...
var slabChunkSizes = [...]int{1, 4, 16, 64, 128}

// itemSlab carves item structs (IntItem, UintItem, FloatItem, ASCIIItem, JIS8Item,
// LocalizedStrItem, BinaryItem, or scalar BooleanItem) from geometrically-growing chunks
// instead of heap-allocating one struct per decoded leaf. It exists purely to cut per-leaf
// allocation count on the decode hot path; item semantics are unaffected.
//
// Retention model: none of the slabbed item types adds a pointer-bearing field that
// references a separately-allocated object on the branch that carves from the slab. Scalar
// numerics and scalar Boolean carry no values/itemErr — only rawPtr is set, pointing into the
// owned wire buffer the surrounding item tree already keeps alive. ASCII, JIS8, and
// LocalizedStr store a Go string built by ownedString, an unsafe view over the same owned
// buffer, not a separate allocation. Binary's values field aliases a sub-slice of that same
// buffer. Retaining one struct out of a chunk therefore pins that chunk (at most 128 structs)
// plus the same wire buffer the tree already pins; it never transitively pins a sibling's
// value slice or subtree the way slabbing a pointer-rich type would.
//
// Chunks are never grown in place — exhaustion allocates a brand-new chunk, so pointers already
// handed out by next remain valid for as long as the caller retains them.
type itemSlab[T any] struct {
	chunk    []T
	pos      int
	chunkIdx int
}

// decodeSlab bundles the per-type item slabs used by a single Decode/DecodeOwned call. Decode
// and DecodeOwned each construct their own decodeSlab and thread it down decodeItem's
// recursion by pointer; nothing is shared across calls, so decodeSlab adds no concurrency
// surface and needs no synchronization.
type decodeSlab struct {
	ints   itemSlab[IntItem]
	uints  itemSlab[UintItem]
	floats itemSlab[FloatItem]
	asciis itemSlab[ASCIIItem]
	jis8s  itemSlab[JIS8Item]
	lstrs  itemSlab[LocalizedStrItem]
	bins   itemSlab[BinaryItem]
	bools  itemSlab[BooleanItem]
}

// next returns a pointer to a fresh zero-value T carved from the slab's current chunk,
// allocating a new chunk first if the current one is full or doesn't exist yet (first use).
func (s *itemSlab[T]) next() *T {
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

// nextASCII carves a fresh *ASCIIItem from the ASCII slab.
func (s *decodeSlab) nextASCII() *ASCIIItem { return s.asciis.next() }

// nextJIS8 carves a fresh *JIS8Item from the JIS8 slab.
func (s *decodeSlab) nextJIS8() *JIS8Item { return s.jis8s.next() }

// nextLocalizedStr carves a fresh *LocalizedStrItem from the localized-string slab.
func (s *decodeSlab) nextLocalizedStr() *LocalizedStrItem { return s.lstrs.next() }

// nextBinary carves a fresh *BinaryItem from the binary slab.
func (s *decodeSlab) nextBinary() *BinaryItem { return s.bins.next() }

// nextBoolean carves a fresh *BooleanItem from the scalar-boolean slab.
func (s *decodeSlab) nextBoolean() *BooleanItem { return s.bools.next() }
