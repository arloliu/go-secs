package tracepack

import (
	"fmt"
	"math"
)

// offsetLen is the size in bytes of one payload offset of a decoded block.
const offsetLen = 4

// heldBudget accounts for the bytes of the block buffers a read over several packs holds at once,
// against MergeIterateOptions.MaxHeldBytes.
// A block's buffers are reserved before they are allocated and released when they are dropped,
// so held never exceeds limit.
type heldBudget struct {
	// limit is the bound, MaxHeldBytes with its default applied; never negative.
	limit int64
	// held is the bytes reserved and not yet released; 0 ≤ held ≤ limit.
	held int64
}

// reserve reserves n more bytes.
// Reaching the limit exactly is admitted.
//
// Returns:
//   - error: nil, or an error wrapping ErrReadLimit, reserving nothing, when held + n would exceed the limit.
func (b *heldBudget) reserve(n int64) error {
	// held ≤ limit, so limit - held cannot overflow, while held + n could.
	if n < 0 || n > b.limit-b.held {
		return fmt.Errorf("tracepack: holding %d bytes more beside the %d held exceeds MaxHeldBytes %d: %w",
			n, b.held, b.limit, ErrReadLimit)
	}
	b.held += n

	return nil
}

// release releases n bytes reserved before.
func (b *heldBudget) release(n int64) {
	b.held -= n
}

// blockSizes are the exact sizes of the buffers a full read of one block fills,
// as the block's BlockInfo states them.
type blockSizes struct {
	// raw is the length of the block as read, envelope and on-disk body: on_disk_len.
	raw int
	// decoded is uncompressed_len, reserved also for a None block, whose body is used in place in raw,
	// since BlockInfo does not carry the codec;
	// the buffer itself is allocated only when the decode needs it, after the envelope's checks.
	decoded int
	// rows is the length of the header section: record_count × record_header_len.
	rows int
	// offs is the number of payload offsets: record_count + 1.
	offs int
	// total is the bytes of all four buffers.
	total int64
}

// preflightBlock checks the dimensions BlockInfo states for block i before anything is reserved or allocated for it,
// and returns the exact sizes of its buffers.
//
// It checks, in this order: the on-disk body length and uncompressed_len against MaxBlockLen (checkBlockBudget);
// that record_count × record_header_len fits uncompressed_len;
// and that the bytes of the record_count + 1 payload offsets fit the int range.
// readBlock finds the same faults only after reading the block, from its envelope, which must agree with BlockInfo.
//
// Returns:
//   - blockSizes: the block's buffer sizes; zero with a defect.
//   - *Defect: nil; ReasonLimit, wrapping ErrReadLimit, for a block over MaxBlockLen;
//     ReasonCorruptBlock for inconsistent dimensions.
//     Its Pack is left for the caller to set.
func (r *Reader) preflightBlock(i int) (blockSizes, *Defect) {
	info := &r.blocks[i]
	if err := r.checkBlockBudget(info); err != nil {
		return blockSizes{}, blockDefect(i, info, ReasonLimit, err)
	}

	rows, err := headerSectionLen(info.RecordCount, info.RecordHeaderLen, info.UncompressedLen)
	if err != nil {
		return blockSizes{}, blockDefect(i, info, ReasonCorruptBlock, err)
	}
	offs := uint64(info.RecordCount) + 1
	if offs > math.MaxInt/offsetLen {
		return blockSizes{}, blockDefect(i, info, ReasonCorruptBlock,
			fmt.Errorf("the %d payload offsets of %d records take more bytes than the int range holds", offs, info.RecordCount))
	}

	// checkBlockBudget keeps on_disk_len and uncompressed_len within the int range,
	// and each of the four sizes is below 2^35, so their sum fits an int64.
	s := blockSizes{raw: int(info.OnDiskLen), decoded: int(info.UncompressedLen), rows: rows, offs: int(offs)}
	s.total = int64(s.raw) + int64(s.decoded) + int64(s.rows) + int64(s.offs)*offsetLen

	return s, nil
}

// newBuf returns a blockBuf of empty buffers with exactly the capacities of s, but for decoded:
// decodeBody allocates it at exactly s.decoded bytes for a block whose codec is not None, and never for a None block.
func (s *blockSizes) newBuf() *blockBuf {
	return &blockBuf{
		raw:  make([]byte, 0, s.raw),
		rows: make([]byte, 0, s.rows),
		offs: make([]uint32, 0, s.offs),
	}
}

// heldBlock is a block read in full into buffers of its own, which stay reserved against a heldBudget until dropped.
type heldBlock struct {
	// pack is the index of the block's Reader among the readers of the read, and block its index into that Reader's blocks.
	pack  int
	block int
	buf   *blockBuf
	// d is the validated block, aliasing buf.
	d *decodedBlock
	// size is the bytes reserved for buf; 0 once the block is dropped.
	size int64
}

// heldLoader reads blocks into held buffers, each reserved against budget before it is allocated.
type heldLoader struct {
	budget heldBudget
	// readHook, set only by tests, receives every block the loader reads in full and its buffers, before the read:
	// a block failing its preflight or its reservation is not read.
	readHook func(pack, block int, buf *blockBuf)
}

// load reads block i of r, the Reader at index pack among the readers of the read, into buffers of its own.
//
// It runs preflightBlock, reserves the block's buffer sizes against the budget,
// allocates the buffers at exactly those capacities, decoded only once the decode needs it,
// and reads the block with readBlock.
// The read never grows a buffer past its capacity:
// readBlock fills the buffers only to the sizes of the block's envelope,
// and only after checking that the envelope agrees with the block's BlockInfo.
//
// Returns:
//   - *heldBlock: the usable block, holding its reservation until drop; nil with a failed block or an error.
//   - *Defect: the block's defect, with Pack set:
//     a failed block's (no block), or ReasonIndexMismatch beside the block, as readBlock reports them.
//   - error: an error wrapping ErrReadLimit when the reservation would exceed the budget;
//     the ReadAt error, wrapped with the pack and block index.
//     Nothing stays reserved for the block after a defect without a block or an error.
func (l *heldLoader) load(r *Reader, pack, i int) (*heldBlock, *Defect, error) {
	s, def := r.preflightBlock(i)
	if def != nil {
		def.Pack = pack

		return nil, def, nil
	}
	if err := l.budget.reserve(s.total); err != nil {
		return nil, nil, fmt.Errorf("tracepack: pack %d, block %d: %w", pack, i, err)
	}

	h := &heldBlock{pack: pack, block: i, buf: s.newBuf(), size: s.total}
	if l.readHook != nil {
		l.readHook(pack, i, h.buf)
	}

	d, def, err := r.readBlock(i, h.buf)
	if def != nil {
		def.Pack = pack
	}
	switch {
	case err != nil:
		l.drop(h)

		return nil, nil, fmt.Errorf("tracepack: pack %d, block %d: %w", pack, i, err)
	case d == nil:
		l.drop(h)

		return nil, def, nil
	}
	h.d = d

	return h, def, nil
}

// drop drops the buffers of h for the garbage collector, then releases their reservation.
// A second drop of h does nothing.
func (l *heldLoader) drop(h *heldBlock) {
	h.d = nil
	*h.buf = blockBuf{}
	l.budget.release(h.size)
	h.size = 0
}
