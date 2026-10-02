package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
)

// openBlock is a block a seqCursor holds open, read in full into buf,
// with the cursor of its next record.
type openBlock struct {
	// pos is the block's position in its cluster's blocks.
	pos int
	buf *blockBuf
	d   *decodedBlock
	// next is the index of the block's next record, and seq that record's seq.
	next int
	seq  uint64
	// origins holds the inputs holding the block, by the caller's index of them:
	// its own input, then the inputs of the blocks dropped as its duplicates.
	origins []int
}

// cursorHeap holds the open blocks that have records left,
// as a binary min-heap by the seq of their next record, then by their position in the cluster.
type cursorHeap []*openBlock

// blockSource is the cluster a seqCursor resolves, and its caller's policy:
// how a block of it is loaded, and what becomes of a block whose records are all resolved.
type blockSource interface {
	// blockCount returns the number of the cluster's blocks.
	blockCount() int
	// firstSeq returns the first seq of the cluster's block at position pos.
	firstSeq(pos int) uint64
	// load loads the cluster's block at position pos, while the blocks of open are open.
	// It returns the block opened, at position pos, its cursor at its first record;
	// nil when the block contributes no records;
	// or an error, which the cursor returns as is.
	load(ctx context.Context, pos int, open []*openBlock) (*openBlock, error)
	// done takes the open block o, whose last record was resolved; the cursor no longer holds it.
	done(o *openBlock)
}

// seqCursor walks the records of one overlap cluster in ascending seq, through the cursors of its open blocks.
//
// The cluster's blocks are loaded in their order, ascending first seq:
// before the records of a seq are resolved, every next block whose first seq is at or below that seq is loaded,
// the smallest seq of the open blocks' next records taken again after each load,
// so a block is loaded only once the walk reaches its first seq.
// The zero seqCursor is at the start of its cluster.
type seqCursor struct {
	// toLoad is the position of the next block to load.
	toLoad int
	open   cursorHeap
	// at holds the open blocks whose next record has the seq being resolved, in position order.
	at []*openBlock
}

// seqVersions are the distinct records of one seq group,
// the records at the cursors of the open blocks a seqCursor gathered for the seq, in position order.
// A seqVersions is reused across groups; group sets it anew.
type seqVersions struct {
	// reps holds the representative of each version: the first block of the group holding it.
	// The versions are in the order of their representatives, the version order.
	reps []*openBlock
	// of holds, for each block of the group, the index into reps of the version it holds.
	of []int
}

// agree reports whether the records at the cursors of the blocks of at are equal, header row and payload byte for byte.
func agree(at []*openBlock) bool {
	for _, o := range at[1:] {
		if !o.sameRecord(at[0]) {
			return false
		}
	}

	return true
}

// conflictOf returns the conflict of seq in the capture captureID,
// whose records the open blocks of at hold at their cursors, in position order, grouped into the versions v:
// its versions are those of v, in version order,
// each with its holders' origins, each once, in ascending order, named by packID.
func conflictOf(captureID UUID, seq uint64, at []*openBlock, v *seqVersions, packID func(origin int) UUID) Conflict {
	holders := make([][]int, len(v.reps))
	for i, o := range at {
		k := v.of[i]
		holders[k] = append(holders[k], o.origins...)
	}

	versions := make([][]UUID, len(holders))
	for k, origins := range holders {
		slices.Sort(origins)
		origins = slices.Compact(origins)
		versions[k] = make([]UUID, len(origins))
		for j, i := range origins {
			versions[k][j] = packID(i)
		}
	}

	return Conflict{CaptureID: captureID, Seq: seq, Versions: versions}
}

// record returns the header row, as stored, and the payload of the block's next record, aliasing its buffer.
func (o *openBlock) record() ([]byte, []byte) {
	rhl, i := int(o.d.env.RecordHeaderLen), o.next

	return o.d.section[i*rhl : (i+1)*rhl : (i+1)*rhl], o.d.payload(i)
}

// seqAt returns the seq of record i, the first field of its header row (the tracepack format specification §7.1).
func (o *openBlock) seqAt(i int) uint64 {
	return binary.LittleEndian.Uint64(o.d.section[i*int(o.d.env.RecordHeaderLen):])
}

// sameRecord reports whether the next records of o and v are equal, header row and payload byte for byte.
func (o *openBlock) sameRecord(v *openBlock) bool {
	orow, opayload := o.record()
	vrow, vpayload := v.record()

	return bytes.Equal(orow, vrow) && bytes.Equal(opayload, vpayload)
}

// less reports whether the open block at i comes before the one at j:
// the seq of its next record is smaller, or equal with an earlier position in the cluster.
func (h cursorHeap) less(i, j int) bool {
	a, b := h[i], h[j]

	return a.seq < b.seq || (a.seq == b.seq && a.pos < b.pos)
}

// push adds o to the heap.
func (h *cursorHeap) push(o *openBlock) {
	*h = append(*h, o)
	s := *h
	for i := len(s) - 1; i > 0; {
		parent := (i - 1) / 2
		if !s.less(i, parent) {
			break
		}
		s[i], s[parent] = s[parent], s[i]
		i = parent
	}
}

// pop removes and returns the heap's first open block; the heap is not empty.
func (h *cursorHeap) pop() *openBlock {
	s := *h
	n := len(s) - 1
	top := s[0]
	s[0], s[n] = s[n], nil
	s = s[:n]
	for i := 0; ; {
		c := 2*i + 1
		if c >= n {
			break
		}
		if r := c + 1; r < n && s.less(r, c) {
			c = r
		}
		if !s.less(c, i) {
			break
		}
		s[i], s[c] = s[c], s[i]
		i = c
	}
	*h = s

	return top
}

// next loads the blocks of src that the walk reaches (loadFrontier),
// then returns the smallest seq of the open blocks' next records,
// and the open blocks whose next record has it, in position order.
// The blocks returned stay the cursor's:
// they are valid until advance, which the caller calls once it is done with their records.
//
// Returns:
//   - uint64: the seq.
//   - []*openBlock: the blocks holding the seq at their cursors, aliasing the cursor's buffer until the next call;
//     nil once every record of the cluster is resolved.
//   - error: the error of a load, as src returned it.
func (c *seqCursor) next(ctx context.Context, src blockSource) (uint64, []*openBlock, error) {
	if err := c.loadFrontier(ctx, src); err != nil {
		return 0, nil, err
	}
	if len(c.open) == 0 {
		return 0, nil, nil
	}

	seq := c.open[0].seq
	c.at = c.at[:0]
	for len(c.open) > 0 && c.open[0].seq == seq {
		c.at = append(c.at, c.open.pop())
	}

	return seq, c.at, nil
}

// loadFrontier loads, in order, every next block of src whose first seq is at or below the smallest seq pending,
// the seq of the next record of an open block, taken again after each load;
// with no block open, it loads the next block, whatever its first seq.
// A block src loads is opened; a block it returns nil for takes no further part.
func (c *seqCursor) loadFrontier(ctx context.Context, src blockSource) error {
	for c.toLoad < src.blockCount() {
		if len(c.open) > 0 && src.firstSeq(c.toLoad) > c.open[0].seq {
			return nil
		}
		pos := c.toLoad
		c.toLoad++
		o, err := src.load(ctx, pos, c.open)
		if err != nil {
			return err
		}
		if o != nil {
			c.open.push(o)
		}
	}

	return nil
}

// advance moves the cursor of every block next returned to its next record and returns the block to the heap,
// or passes a block without a next record to src's done.
func (c *seqCursor) advance(src blockSource) {
	for _, o := range c.at {
		o.next++
		if o.next < o.d.count() {
			o.seq = o.seqAt(o.next)
			c.open.push(o)

			continue
		}
		src.done(o)
	}
	c.at = c.at[:0]
}

// close passes every block the cursor still holds to src's done:
// the blocks of the group next returned, when advance was not called for it, then the open blocks with records left.
// The cursor is then empty, and its walk over, so a later next loads nothing and returns no group.
// close after the cluster's last record was resolved does nothing.
func (c *seqCursor) close(src blockSource) {
	for _, o := range c.at {
		src.done(o)
	}
	clear(c.at)
	c.at = c.at[:0]
	for _, o := range c.open {
		src.done(o)
	}
	clear(c.open)
	c.open = c.open[:0]
	c.toLoad = src.blockCount()
}

// group sets v to the versions of the records the open blocks of at hold at their cursors, in position order:
// records equal header row and payload byte for byte are one version (sameRecord).
func (v *seqVersions) group(at []*openBlock) {
	v.reps, v.of = v.reps[:0], v.of[:0]
	for _, o := range at {
		k := slices.IndexFunc(v.reps, o.sameRecord)
		if k < 0 {
			k = len(v.reps)
			v.reps = append(v.reps, o)
		}
		v.of = append(v.of, k)
	}
}
