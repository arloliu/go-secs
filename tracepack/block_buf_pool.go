package tracepack

import "sync"

// maxPooledBlockBuf is the most bytes of capacity a pooled block buffer keeps: a larger buffer is dropped instead.
const maxPooledBlockBuf = 16 << 20

// blockBufPool lends block buffers to the reads that hold one block at a time (Iterate, ExportJSONL, Verify)
// and takes them back once the read returns, so that the next read does not allocate its buffers afresh.
type blockBufPool interface {
	// get returns a buffer with empty slices, of any capacity.
	get() *blockBuf
	// put takes back a buffer no read uses any more.
	put(buf *blockBuf)
}

// syncBlockBufPool is the package's blockBufPool, a sync.Pool.
type syncBlockBufPool struct {
	p sync.Pool
}

// defaultBlockBufs is the pool of every Reader that Open returns.
var defaultBlockBufs blockBufPool = &syncBlockBufPool{}

func (s *syncBlockBufPool) get() *blockBuf {
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

// emptyBlockBuf empties buf's slices and its decoded block, keeping their storage.
func emptyBlockBuf(buf *blockBuf) {
	buf.raw, buf.decoded, buf.rows, buf.offs = buf.raw[:0], buf.decoded[:0], buf.rows[:0], buf.offs[:0]
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

// blockBufs returns the pool r's single-block reads take their buffer from.
func (r *Reader) blockBufs() blockBufPool {
	if r.bufs == nil {
		return defaultBlockBufs
	}

	return r.bufs
}
