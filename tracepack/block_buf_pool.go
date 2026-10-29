package tracepack

import "sync"

// maxPooledBlockBuf is the most bytes of capacity a pooled block buffer keeps: a larger buffer is dropped instead.
const maxPooledBlockBuf = 16 << 20

// blockBufPool lends block buffers to the reads that hold one block at a time (Iterate, ExportJSONL, Verify),
// which take them back once the read returns, and to the held blocks of a read over several packs, taken back when each is dropped,
// so that the next read does not allocate its buffers afresh.
type blockBufPool interface {
	// get returns a buffer with empty slices, of any capacity;
	// hint, when not nil, holds the sizes of the held block the buffer is for, which the package's pool ignores.
	get(hint *blockSizes) *blockBuf
	// put takes back a buffer no read uses any more.
	put(buf *blockBuf)
}

// syncBlockBufPool is the package's blockBufPool, a sync.Pool.
type syncBlockBufPool struct {
	p sync.Pool
}

// defaultBlockBufs is the pool of every Reader that Open returns.
var defaultBlockBufs blockBufPool = &syncBlockBufPool{}

func (s *syncBlockBufPool) get(*blockSizes) *blockBuf {
	if buf, ok := s.p.Get().(*blockBuf); ok {
		return buf
	}

	return new(blockBuf)
}

func (s *syncBlockBufPool) put(buf *blockBuf) {
	if readyForPool(buf) {
		s.p.Put(buf)
	}
}

// readyForPool empties buf (emptyBlockBuf) and reports whether it is small enough to pool,
// blockBufBytes within maxPooledBlockBuf.
func readyForPool(buf *blockBuf) bool {
	if buf.bytes() > maxPooledBlockBuf {
		return false
	}
	emptyBlockBuf(buf)

	return true
}

// emptyBlockBuf empties buf's slices and its decoded block, keeping their storage, and drops its view.
func emptyBlockBuf(buf *blockBuf) {
	buf.raw, buf.decoded, buf.rows, buf.offs = buf.raw[:0], buf.decoded[:0], buf.rows[:0], buf.offs[:0]
	buf.view, buf.retained = nil, false
	buf.blk = decodedBlock{}
}

// bytes returns the bytes of buf's storage (blockBufBytes).
func (buf *blockBuf) bytes() int64 {
	return blockBufBytes(cap(buf.raw), cap(buf.decoded), cap(buf.rows), cap(buf.offs))
}

// blockBufBytes returns the bytes of block buffers of the given capacities, offs counted in offsets,
// summed in int64 so that the sum cannot overflow where int is 32 bits.
func blockBufBytes(raw, decoded, rows, offs int) int64 {
	return int64(raw) + int64(decoded) + int64(rows) + offsetLen*int64(offs)
}

// A held block's buffer, unlike a single-block read's, keeps a pooled slice only when it fits the block (fitBuf):
// at least the size the block needs and at most an eighth more,
// so that the slice capacities of the held blocks in use stay within 9/8 of what MaxHeldBytes reserves for them,
// the reservations themselves remaining the blocks' exact sizes.

// fits reports whether a slice of capacity c may serve n elements of a held block: n ≤ c ≤ n + n/8.
func fits(c, n int) bool {
	return c >= n && c-n <= n/8
}

// fitSlice returns b emptied when its capacity fits n elements, else a fresh empty slice of exactly n.
func fitSlice[T any](b []T, n int) []T {
	if fits(cap(b), n) {
		return b[:0]
	}

	return make([]T, 0, n)
}

// fitBuf sizes the slices of buf, taken from a pool, for a held block of sizes s:
// raw, rows and offs each kept when it fits, else replaced by an exact one;
// decoded kept when it fits, else dropped, the decode allocating it exactly when the block needs it (decodeBody);
// and, for a block within its Reader's retained bytes, raw dropped, the block being read in place there (ownView).
func (s *blockSizes) fitBuf(buf *blockBuf, retained bool) {
	if retained {
		buf.raw = nil
	} else {
		buf.raw = fitSlice(buf.raw, s.raw)
	}
	buf.rows = fitSlice(buf.rows, s.rows)
	buf.offs = fitSlice(buf.offs, s.offs)
	if fits(cap(buf.decoded), s.decoded) {
		buf.decoded = buf.decoded[:0]
	} else {
		buf.decoded = nil
	}
}

// blockBufs returns the pool r's single-block reads and held blocks take their buffer from.
func (r *Reader) blockBufs() blockBufPool {
	if r.bufs == nil {
		return defaultBlockBufs
	}

	return r.bufs
}
