package tracepack

import (
	"sync"
	"unsafe"
)

// Most bytes of storage a pooled scratch keeps: a larger scratch is dropped instead.
const (
	maxPooledWriterScratch = 16 << 20
	maxPooledMergeScratch  = 64 << 20
)

// scratchPool lends the buffers a finished Writer or merge left, so that the next one does not allocate them afresh.
type scratchPool[T any] interface {
	// get returns a scratch, empty or left by an earlier user.
	get() *T
	// put takes back a scratch no one uses any more.
	put(s *T)
}

// syncWriterScratch and syncMergeScratch are the package's scratch pools, each a sync.Pool.
type (
	syncWriterScratch struct{ p sync.Pool }
	syncMergeScratch  struct{ p sync.Pool }
)

func (s *syncWriterScratch) get() *writerScratch {
	if v, ok := s.p.Get().(*writerScratch); ok {
		return v
	}

	return new(writerScratch)
}

func (s *syncWriterScratch) put(v *writerScratch) {
	s.p.Put(v)
}

func (s *syncMergeScratch) get() *mergeScratch {
	if v, ok := s.p.Get().(*mergeScratch); ok {
		return v
	}

	return new(mergeScratch)
}

func (s *syncMergeScratch) put(v *mergeScratch) {
	s.p.Put(v)
}

// The pools of every Writer and merge but a test's.
var (
	defaultWriterScratch scratchPool[writerScratch] = &syncWriterScratch{}
	defaultMergeScratch  scratchPool[mergeScratch]  = &syncMergeScratch{}
)

// writerScratch is what a Writer reuses across its blocks: the open block's headers and payloads,
// and the buffers its blocks are built, encoded and validated in.
type writerScratch struct {
	headers, payloads, body, enc []byte
	val                          validateBuf
}

// bytes returns the bytes of s's storage, summed in int64.
func (s *writerScratch) bytes() int64 {
	return int64(cap(s.headers)) + int64(cap(s.payloads)) + int64(cap(s.body)) + int64(cap(s.enc)) +
		blockBufBytes(0, cap(s.val.decoded), cap(s.val.rows), cap(s.val.offs))
}

// mergeScratch is what a merge run reuses across its blocks:
// its free block buffers, the unit its resolution fills, the buffers that unit is encoded in, and its coalescing group.
type mergeScratch struct {
	free  []*blockBuf
	unit  unitBuilder
	enc   encodeBuf
	group coalesceGroup
}

// Sizes of the elements a mergeScratch holds slices of.
const (
	ptrBytes       = int64(unsafe.Sizeof((*blockBuf)(nil)))
	sliceBytes     = int64(unsafe.Sizeof([]byte(nil)))
	intBytes       = int64(unsafe.Sizeof(0))
	groupUnitBytes = int64(unsafe.Sizeof(groupUnit{}))
)

// bytes returns the bytes of s's storage, every slice counted by its capacity and element size, summed in int64.
func (s *mergeScratch) bytes() int64 {
	n := int64(cap(s.free)) * ptrBytes
	for _, buf := range s.free {
		n += buf.bytes()
	}

	return n + s.unit.bytes() + s.enc.bytes() + s.group.records.bytes() + s.group.enc.bytes() +
		int64(cap(s.group.raw)) + int64(cap(s.group.units))*groupUnitBytes
}

// bytes returns the bytes of u's storage, summed in int64.
func (u *unitBuilder) bytes() int64 {
	return int64(cap(u.rows)) + int64(cap(u.payloads)) + int64(cap(u.ends))*intBytes +
		(int64(cap(u.rowViews))+int64(cap(u.payloadViews)))*sliceBytes
}

// bytes returns the bytes of e's storage, summed in int64.
func (e *encodeBuf) bytes() int64 {
	return int64(cap(e.raw)) + int64(cap(e.body)) + int64(cap(e.enc)) + e.check.bytes()
}

// clearViews empties u's views, clearing them over their whole capacity, so that the views hold no record's storage.
func (u *unitBuilder) clearViews() {
	clear(u.rowViews[:cap(u.rowViews)])
	clear(u.payloadViews[:cap(u.payloadViews)])
	u.rowViews, u.payloadViews = u.rowViews[:0], u.payloadViews[:0]
}

// detach empties e's buffers and drops its check's alias of the last block built, its decoded block and its hook.
func (e *encodeBuf) detach() {
	e.raw, e.body, e.enc = e.raw[:0], e.body[:0], e.enc[:0]
	emptyBlockBuf(&e.check)
	e.check.raw = nil
	e.hook = nil
}
