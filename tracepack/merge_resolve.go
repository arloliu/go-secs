package tracepack

import (
	"bytes"
	"context"
	"fmt"
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

// resolution is the record-by-record resolution of one overlap cluster (the tracepack storage specification §4 Merge),
// through a seqCursor over the cluster's blocks, which the resolution loads as readInputBlock reads them.
// A block is released once its last record is resolved.
type resolution struct {
	m *mergeRun
	c *mergeCluster
	// seqCursor is embedded so that the cursor's open blocks are r.open;
	// the resolution is the blockSource it passes to its own cursor.
	seqCursor
	// sums holds the summaries of the open blocks (summaryOf), by position,
	// for a block copied verbatim after all;
	// a block's summary is dropped with the block.
	sums []blockSummary
}

var _ blockSource = (*resolution)(nil)

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
// it groups the records into versions and builds a new Conflict of them (conflictOf), whose versions name their holders' inputs in the order of the view's Packs,
// passes it to OnConflict, and counts it.
func (m *mergeRun) conflict(seq uint64, at []*openBlock) {
	var v seqVersions
	v.group(at)
	m.onConflict(conflictOf(m.p.captureID, seq, at, &v, m.p.inputID))
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

// run resolves the cluster's records in ascending seq, loading its blocks as the resolution reaches them,
// then encodes and emits the last unit of its records.
// The records of one seq, one at the cursor of each open block that holds the seq, are resolved together:
// records equal header row and payload byte for byte are one record, which the unit being filled takes;
// records that differ are a conflict.
// Each open block is counted as resolved once its last record is resolved.
func (r *resolution) run(ctx context.Context) error {
	for {
		seq, at, err := r.next(ctx, r)
		if err != nil {
			return err
		}
		if at == nil {
			break
		}

		row, payload := at[0].record()
		if !agree(at) {
			r.m.conflict(seq, at)
		} else if err := r.m.add(row, payload); err != nil {
			return err
		}
		r.advance(r)
		if err := r.m.countResolved(ctx); err != nil {
			return err
		}
	}

	return r.m.flushUnit()
}

// loadFrontier loads every next block of the cluster that the resolution reaches, as seqCursor.loadFrontier does,
// the resolution being the cursor's blockSource.
func (r *resolution) loadFrontier(ctx context.Context) error {
	return r.seqCursor.loadFrontier(ctx, r)
}

// blockCount returns the number of the cluster's blocks.
func (r *resolution) blockCount() int {
	return len(r.c.blocks)
}

// firstSeq returns the F-2 first seq of the cluster's block at position pos.
func (r *resolution) firstSeq(pos int) uint64 {
	return r.c.blocks[pos].first
}

// load reads the cluster's block at position pos in full and checks it, as readInputBlock does.
// A block whose envelope and on-disk body equal those of an open block with the same first seq
// is dropped as a duplicate, its input added to that block's origins;
// any other block is opened, which fails with ErrMergeLimit when MaxOpenBlocks blocks are open already.
// The open blocks then all hold the block's first seq, so their number is the overlap depth at that seq.
func (r *resolution) load(ctx context.Context, pos int, open []*openBlock) (*openBlock, error) {
	m, b := r.m, &r.c.blocks[pos]
	if m.p.loadHook != nil {
		m.p.loadHook(b, len(open), m.resolved)
	}

	buf := m.buffer()
	d, s, err := m.p.readInputBlock(ctx, b, buf)
	if err != nil {
		return nil, err
	}
	for _, o := range open {
		if r.c.blocks[o.pos].first == b.first && bytes.Equal(o.buf.raw, buf.raw) {
			o.origins = append(o.origins, b.input)
			m.rep.Duplicates++
			m.release(buf)

			return nil, nil //nolint:nilnil // a duplicate contributes no records, which load returns as nil
		}
	}
	if len(open) >= m.p.maxOpenBlocks {
		return nil, fmt.Errorf("tracepack: merge: the blocks of seqs %d to %d overlap %d deep at seq %d, above MaxOpenBlocks %d: %w",
			r.c.first, r.c.last, len(open)+1, b.first, m.p.maxOpenBlocks, ErrMergeLimit)
	}

	if r.sums == nil {
		r.sums = make([]blockSummary, len(r.c.blocks))
	}
	r.sums[pos] = s

	// The block's first record has the block's first seq (I-2).
	return &openBlock{pos: pos, buf: buf, d: d, seq: b.first, origins: []int{b.input}}, nil
}

// done counts the open block o, whose last record was resolved, as resolved, and releases it and its summary.
func (r *resolution) done(o *openBlock) {
	r.m.rep.Resolved++
	r.m.release(o.buf)
	r.sums[o.pos] = blockSummary{}
}
