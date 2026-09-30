package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
)

// resolveCtxRecords is the number of records the record-by-record resolution resolves between two checks of ctx.
const resolveCtxRecords = 4096

// unitBuilder accumulates the records of a block to encode, the resolution's next block or a coalescing group's:
// their header rows one after another, all headerLen bytes long, and their payloads one after another.
type unitBuilder struct {
	headerLen int
	// size is the decoded size of the records, Σ record_header_len + payload_len.
	size uint64
	rows []byte
	// payloads holds the payloads, the payload of record i ending at ends[i].
	payloads []byte
	ends     []int
	// rowViews and payloadViews hold the records as encodeRawBlock takes them, slices of rows and payloads.
	rowViews     [][]byte
	payloadViews [][]byte
}

// openBlock is an input block the resolution of a cluster holds, read in full into buf,
// with the cursor of its next record.
type openBlock struct {
	// b is the block, and pos its position in the cluster's blocks.
	b   *mergeBlock
	pos int
	buf *blockBuf
	d   *decodedBlock
	// sum is the summary of the block's records (summaryOf), for a block copied verbatim after all.
	sum blockSummary
	// next is the index of the block's next record, and seq that record's seq.
	next int
	seq  uint64
	// origins holds the inputs holding the block, by their index in the view's Packs:
	// its own input, then the inputs of the blocks dropped as its duplicates.
	origins []int
}

// cursorHeap holds the open blocks that have records left,
// as a binary min-heap by the seq of their next record, then by their position in the cluster.
type cursorHeap []*openBlock

// resolution is the record-by-record resolution of one overlap cluster (the tracepack storage specification §4 Merge).
//
// The cluster's blocks are loaded in their order, ascending first seq, then input order:
// before the records of a seq are resolved, every next block whose first seq is at or below that seq is loaded,
// the smallest seq of the open blocks' next records taken again after each load,
// so a block is loaded only once the resolution reaches its first seq.
// A block is released once its last record is resolved.
type resolution struct {
	m *mergeRun
	c *mergeCluster
	// next is the position in c.blocks of the next block to load.
	next int
	open cursorHeap
	// at holds the open blocks whose next record has the seq being resolved, in position order.
	at []*openBlock
}

// add takes a resolved record, its header row and payload, into the unit being filled,
// after encoding and emitting that unit first when the record does not fit it:
// a unit holds records of one record_header_len while its decoded size, Σ record_header_len + payload_len,
// stays within the threshold, so a record whose decoded size alone reaches the threshold is a unit of its own
// (the tracepack storage specification §4 Merge).
// Once a conflict was found it takes nothing, since nothing more is written.
//
// A record of an input block fits the 2^31-1 limits of the tracepack format specification §2 by itself,
// and a unit of several records stays within the threshold, which Merge's options bound by the same limit;
// encodeRawBlock checks them again before it allocates the block.
func (m *mergeRun) add(row, payload []byte) error {
	if m.rep.Conflicts > 0 {
		return nil
	}

	u := &m.unit
	size := uint64(len(row)) + uint64(len(payload))
	if len(u.ends) > 0 && (len(row) != u.headerLen || u.size+size > uint64(m.p.threshold)) {
		if err := m.flushUnit(); err != nil {
			return err
		}
	}
	u.add(row, payload, size)

	return nil
}

// flushUnit encodes the records of the unit being filled, when it holds any, as a new block, and emits it.
// Once a conflict was found it drops them instead, since nothing more is written.
// A new block that fails its check fails the merge (the tracepack storage specification §4 Merge).
func (m *mergeRun) flushUnit() error {
	u := &m.unit
	defer u.reset()
	if len(u.ends) == 0 || m.rep.Conflicts > 0 {
		return nil
	}

	rows, payloads := u.records()
	raw, sum, err := encodeRawBlock(&m.enc, m.codec, u.headerLen, rows, payloads)
	if err != nil {
		// The error names the new block by its first seq, as the Writer's errors writeUnit returns name a block.
		return err
	}
	m.rep.ResolvedEncodings++
	unit := mergeUnit{raw: raw, sum: sum, headerLen: u.headerLen, size: u.size, rows: rows, payloads: payloads, encoded: true}

	return m.emit(&unit)
}

// conflict reports the conflict of seq, whose records the open blocks at hold at their cursors, in position order:
// it builds a new Conflict, whose versions are the distinct records in the order of the first block holding each,
// each with its holders' origins in the order of the view's Packs, each once,
// passes it to OnConflict, and counts it.
func (m *mergeRun) conflict(seq uint64, at []*openBlock) {
	var firsts []*openBlock
	var holders [][]int
	for _, o := range at {
		k := slices.IndexFunc(firsts, o.sameRecord)
		if k < 0 {
			k = len(firsts)
			firsts = append(firsts, o)
			holders = append(holders, nil)
		}
		holders[k] = append(holders[k], o.origins...)
	}

	versions := make([][]UUID, len(holders))
	for k, inputs := range holders {
		slices.Sort(inputs)
		inputs = slices.Compact(inputs)
		versions[k] = make([]UUID, len(inputs))
		for j, i := range inputs {
			versions[k][j] = m.p.inputID(i)
		}
	}

	m.onConflict(Conflict{CaptureID: m.p.captureID, Seq: seq, Versions: versions})
	m.rep.Conflicts++
}

// countResolved counts one record resolved, and checks ctx after every resolveCtxRecords records.
func (m *mergeRun) countResolved(ctx context.Context) error {
	m.resolved++
	if m.resolved%resolveCtxRecords != 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: merge: %w", err)
	}

	return nil
}

// add appends a record, whose decoded size is size, to the unit; its header row sets the unit's headerLen.
func (u *unitBuilder) add(row, payload []byte, size uint64) {
	u.headerLen = len(row)
	u.size += size
	u.rows = append(u.rows, row...)
	u.payloads = append(u.payloads, payload...)
	u.ends = append(u.ends, len(u.payloads))
}

// records returns the unit's records as encodeRawBlock takes them, aliasing the unit's buffers.
func (u *unitBuilder) records() ([][]byte, [][]byte) {
	u.rowViews, u.payloadViews = u.rowViews[:0], u.payloadViews[:0]
	start := 0
	for i, end := range u.ends {
		u.rowViews = append(u.rowViews, u.rows[i*u.headerLen:(i+1)*u.headerLen:(i+1)*u.headerLen])
		u.payloadViews = append(u.payloadViews, u.payloads[start:end:end])
		start = end
	}

	return u.rowViews, u.payloadViews
}

// reset empties the unit, keeping its buffers.
func (u *unitBuilder) reset() {
	u.headerLen, u.size = 0, 0
	u.rows, u.payloads, u.ends = u.rows[:0], u.payloads[:0], u.ends[:0]
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

// run resolves the cluster's records in ascending seq, loading its blocks as the resolution reaches them,
// then encodes and emits the last unit of its records.
// The records of one seq, one at the cursor of each open block that holds the seq, are resolved together:
// records equal header row and payload byte for byte are one record, which the unit being filled takes;
// records that differ are a conflict.
// Each open block is counted as resolved once its last record is resolved.
func (r *resolution) run(ctx context.Context) error {
	for {
		if err := r.loadFrontier(ctx); err != nil {
			return err
		}
		if len(r.open) == 0 {
			break
		}

		seq := r.gather()
		row, payload := r.at[0].record()
		if !r.agree() {
			r.m.conflict(seq, r.at)
		} else if err := r.m.add(row, payload); err != nil {
			return err
		}
		r.advance()
		if err := r.m.countResolved(ctx); err != nil {
			return err
		}
	}

	return r.m.flushUnit()
}

// loadFrontier loads, in order, every next block of the cluster whose first seq is at or below the smallest seq pending,
// the seq of the next record of an open block, taken again after each load;
// with no block open, it loads the next block, whatever its first seq.
func (r *resolution) loadFrontier(ctx context.Context) error {
	for r.next < len(r.c.blocks) {
		if len(r.open) > 0 && r.c.blocks[r.next].first > r.open[0].seq {
			return nil
		}
		if err := r.load(ctx); err != nil {
			return err
		}
	}

	return nil
}

// load reads the cluster's next block in full and checks it, as readInputBlock does.
// A block whose envelope and on-disk body equal those of an open block with the same first seq
// is dropped as a duplicate, its input added to that block's origins;
// any other block is opened, which fails with ErrMergeLimit when MaxOpenBlocks blocks are open already.
// The open blocks then all hold the block's first seq, so their number is the overlap depth at that seq.
func (r *resolution) load(ctx context.Context) error {
	m, b, pos := r.m, &r.c.blocks[r.next], r.next
	r.next++
	if m.p.loadHook != nil {
		m.p.loadHook(b, len(r.open), m.resolved)
	}

	buf := m.buffer()
	d, s, err := m.p.readInputBlock(ctx, b, buf)
	if err != nil {
		return err
	}
	for _, o := range r.open {
		if o.b.first == b.first && bytes.Equal(o.buf.raw, buf.raw) {
			o.origins = append(o.origins, b.input)
			m.rep.Duplicates++
			m.release(buf)

			return nil
		}
	}
	if len(r.open) >= m.p.maxOpenBlocks {
		return fmt.Errorf("tracepack: merge: the blocks of seqs %d to %d overlap %d deep at seq %d, above MaxOpenBlocks %d: %w",
			r.c.first, r.c.last, len(r.open)+1, b.first, m.p.maxOpenBlocks, ErrMergeLimit)
	}

	// The block's first record has the block's first seq (I-2).
	r.open.push(&openBlock{b: b, pos: pos, buf: buf, d: d, sum: s, seq: b.first, origins: []int{b.input}})

	return nil
}

// gather moves every open block whose next record has the smallest seq pending from the heap into r.at,
// in position order, and returns that seq.
func (r *resolution) gather() uint64 {
	seq := r.open[0].seq
	r.at = r.at[:0]
	for len(r.open) > 0 && r.open[0].seq == seq {
		r.at = append(r.at, r.open.pop())
	}

	return seq
}

// agree reports whether the records at the cursors of the blocks of r.at are equal, header row and payload byte for byte.
func (r *resolution) agree() bool {
	for _, o := range r.at[1:] {
		if !o.sameRecord(r.at[0]) {
			return false
		}
	}

	return true
}

// advance moves the cursor of every block of r.at to its next record and returns the block to the heap,
// or releases a block without a next record, counting it as resolved.
func (r *resolution) advance() {
	for _, o := range r.at {
		o.next++
		if o.next < o.d.count() {
			o.seq = o.seqAt(o.next)
			r.open.push(o)

			continue
		}
		r.m.rep.Resolved++
		r.m.release(o.buf)
	}
}
