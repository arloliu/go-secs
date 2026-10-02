package tracepack

import (
	"bytes"
	"context"
	"fmt"
)

// Checks of ctx in a read over several packs.
const (
	// resolveCtxCopies is the step of the count of copies of records the read gathers into seq groups:
	// ctx is checked after a seq group is gathered, whenever the count reaches or passes a multiple of it.
	resolveCtxCopies = 4096
	// yieldCtxCalls is the number of calls of fn between two checks of ctx:
	// ctx is checked before the 4096th call, the 8192nd, and so on.
	yieldCtxCalls = 4096
)

// mergeIterateRun is one read over several packs once planned:
// its plan, its query, and what the resolutions of its clusters share.
type mergeIterateRun struct {
	p *mergeIteratePlan
	q *Query
	// fields is set when the query's filter tests an HSMS header field, read from the payload.
	fields bool
	// copies counts every copy of a record gathered into a seq group, over every cluster resolved:
	// identical copies held by distinct open blocks each count.
	// A block dropped at load as a duplicate never enters a group, so its records are not counted.
	copies ctxCounter
	// yields counts the calls of fn.
	yields ctxCounter
}

// ctxCounter counts toward the checks of ctx at every multiple of a step,
// keeping only the count since the last multiple, so that no count of a read can overflow it.
type ctxCounter struct {
	// step is the step of the checks, positive.
	step int
	// n is the count modulo step.
	n int
}

// candidate is a version of a record that the query selects, not yet yielded:
// the record of the version's representative, which the candidate owns until it is released
// (the tracepack semantics specification §7.4).
type candidate struct {
	// h is the representative's held block, of which the candidate is one owner until it is released.
	h *heldBlock
	// rec is the index of the record in its block.
	rec int
	// version is the index of the version in the version order of the record's seq.
	version int
	// conflict is set when the record's seq has more than one version.
	conflict bool
	// seq is the record's seq, and ts its ts_utc_ns.
	seq uint64
	ts  int64
	// blockTSMin is the ts_min of the representative's block, as its F-2 entry or its computed summary states it.
	blockTSMin int64
}

// clusterResolver resolves the records of one overlap cluster of a read over several packs, seq by seq,
// through a seqCursor over the cluster's blocks, for which it is the blockSource
// (the tracepack semantics specification §7.4).
//
// For each seq it compares the copies, groups them into versions, lists a conflict when there are several,
// and makes a candidate of each version the query selects.
// A block it loads is held, its reservation kept, while the cursor or a candidate owns it.
type clusterResolver struct {
	run     *mergeIterateRun
	capture UUID
	c       *planCluster
	cur     seqCursor
	// held holds the held block of each open block, by position in the cluster; nil for a block not open.
	held     []*heldBlock
	versions seqVersions
}

var _ blockSource = (*clusterResolver)(nil)

// newMergeIterateRun returns the run of the read of q that p planned.
func newMergeIterateRun(p *mergeIteratePlan, q *Query) *mergeIterateRun {
	copiesStep, yieldStep := resolveCtxCopies, yieldCtxCalls
	if p.copiesStep > 0 {
		copiesStep = p.copiesStep
	}
	if p.yieldStep > 0 {
		yieldStep = p.yieldStep
	}

	return &mergeIterateRun{
		p: p, q: q, fields: q.Filter.hasFieldPredicate(),
		copies: ctxCounter{step: copiesStep}, yields: ctxCounter{step: yieldStep},
	}
}

// newClusterResolver returns the resolver of c, at the start of the cluster.
// c is an overlap cluster that run's plan does not exclude, of the capture whose capture_id is capture.
func newClusterResolver(run *mergeIterateRun, capture UUID, c *planCluster) *clusterResolver {
	return &clusterResolver{run: run, capture: capture, c: c, held: make([]*heldBlock, len(c.blocks))}
}

// item sets it to the candidate's record as the read yields it, for q:
// the record as stored in its representative, with its header extension, pack and block;
// its payload only when q asks for payloads.
// The Item aliases the representative's block, valid while the candidate is not released.
func (c *candidate) item(q *Query, it *Item) {
	d := c.h.d
	h := d.header(c.rec)
	rec := storedRecord(&h, d.payload(c.rec))
	if !q.Payloads {
		rec.Payload = nil
	}
	*it = Item{Record: rec, HeaderExtra: h.Extra, Pack: c.h.pack, Block: c.h.block, Conflict: c.conflict}
}

// release drops the candidate's ownership of its representative's block, through l, the loader that loaded it.
// A candidate is released exactly once, and is unusable after.
func (c *candidate) release(l *heldLoader) {
	l.release(c.h)
	c.h = nil
}

// next resolves the next seq of the cluster and appends to dst a candidate of each of its versions that the query selects,
// in version order.
//
// It loads the blocks the cursor reaches, gathers the copies of the seq, counts them (countCopies),
// and groups them into versions, records equal header row and payload byte for byte being one version.
// With more than one version it lists the conflict in the read's Result, before the filter is applied.
// The filter tests each version's representative: its record header (matchHeader),
// then, only when the filter tests an HSMS header field, its payload's fields (matchFields).
// Each candidate made takes ownership of its representative's block before the cursor moves past the seq,
// and a selected version of a conflicting seq is marked so;
// a seq can give no candidate.
//
// Returns:
//   - []candidate: dst with the seq's candidates appended; dst itself with an error or once the cluster is resolved.
//   - bool: whether a seq was resolved; false once every record of the cluster is resolved, or with an error.
//   - error: the error of a load (load); ctx's error, wrapped;
//     an error wrapping ErrReadLimit when one more conflict would exceed MaxConflicts, the Result keeping those listed.
//     After an error the seq's copies stay with the cursor, which close releases.
func (cr *clusterResolver) next(ctx context.Context, dst []candidate) ([]candidate, bool, error) {
	seq, at, err := cr.cur.next(ctx, cr)
	if err != nil || at == nil {
		return dst, false, err
	}
	if err := cr.run.countCopies(ctx, len(at)); err != nil {
		return dst, false, err
	}

	v := &cr.versions
	v.group(at)
	conflict := len(v.reps) > 1
	if conflict {
		if err := cr.addConflict(seq, at); err != nil {
			return dst, false, err
		}
	}
	for k, o := range v.reps {
		if c, ok := cr.candidateOf(o, k, conflict); ok {
			dst = append(dst, c)
		}
	}
	cr.cur.advance(cr)

	return dst, true, nil
}

// close releases the blocks the resolver's cursor still holds, those of a seq next did not finish included,
// and ends the cluster's walk.
// It is called on every exit from the cluster, and does nothing once the cluster is resolved.
// The candidates made are not the resolver's: their owner releases them.
func (cr *clusterResolver) close() {
	cr.cur.close(cr)
}

// addConflict lists in the read's Result the conflict of seq, whose copies at hold the versions cr.versions grouped,
// its versions naming the packs holding each in reader order.
//
// Returns:
//   - error: an error wrapping ErrReadLimit, listing nothing, when MaxConflicts conflicts are listed already.
func (cr *clusterResolver) addConflict(seq uint64, at []*openBlock) error {
	p := cr.run.p
	if len(p.res.Conflicts) >= p.opts.MaxConflicts {
		return fmt.Errorf("tracepack: merge iterate: the conflict at seq %d of capture %s exceeds MaxConflicts %d: %w",
			seq, cr.capture, p.opts.MaxConflicts, ErrReadLimit)
	}
	p.res.Conflicts = append(p.res.Conflicts, conflictOf(cr.capture, seq, at, &cr.versions, cr.run.packID))

	return nil
}

// candidateOf returns the candidate of version, whose representative is o, the record at o's cursor,
// when the query selects that record; the candidate then owns o's held block.
//
// Returns:
//   - candidate: the candidate; zero when the query does not select the record.
//   - bool: whether the query selects the record.
func (cr *clusterResolver) candidateOf(o *openBlock, version int, conflict bool) (candidate, bool) {
	f := &cr.run.q.Filter
	h := o.d.header(o.next)
	if !f.matchHeader(&h) {
		return candidate{}, false
	}
	if cr.run.fields {
		rec := storedRecord(&h, o.d.payload(o.next))
		if fields := rec.HSMSHeader(); !f.matchFields(&fields) {
			return candidate{}, false
		}
	}

	held := cr.held[o.pos]
	held.hold()

	return candidate{
		h: held, rec: o.next, version: version, conflict: conflict,
		seq: h.Seq, ts: h.TSUTCNs, blockTSMin: cr.c.blocks[o.pos].span.tsMin,
	}, true
}

// blockCount returns the number of the cluster's blocks.
func (cr *clusterResolver) blockCount() int {
	return len(cr.c.blocks)
}

// firstSeq returns the first seq of the cluster's block at position pos, as its F-2 entry or its computed summary states it.
func (cr *clusterResolver) firstSeq(pos int) uint64 {
	return cr.c.blocks[pos].first
}

// load reads the cluster's block at position pos in full into a held block, after checking ctx.
//
// A defect of the block is added to the read's Result.
// A block that failed, or holds no record, contributes no records:
// load returns no block, and the cluster's other blocks resolve its seqs.
// A block with a ReasonIndexMismatch defect is used, its records where they are, even outside the cluster's seqs.
// A block is dropped at once as a duplicate
// when its bytes, envelope and on-disk body, equal those of an open block with the same first seq;
// its pack is added to that block's origins, and it holds no memory.
// Any other block is opened, the cursor its one owner.
//
// Returns:
//   - *openBlock: the block opened; nil when the block contributes no records.
//   - error: ctx's error, wrapped; the loader's error, as is:
//     an error wrapping ErrReadLimit when the block's reservation would exceed MaxHeldBytes,
//     or a ReadAt error, wrapped with the pack and block index.
func (cr *clusterResolver) load(ctx context.Context, pos int, open []*openBlock) (*openBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: merge iterate: %w", err)
	}

	p, b := cr.run.p, &cr.c.blocks[pos]
	h, def, err := p.loader.load(p.readers[b.pack], b.pack, b.block)
	if err != nil {
		return nil, err
	}
	if def != nil {
		p.res.Incomplete = append(p.res.Incomplete, *def)
	}
	if h == nil {
		return nil, nil //nolint:nilnil // a failed block contributes no records, which load returns as nil
	}
	if h.d.count() == 0 {
		p.loader.release(h)

		return nil, nil //nolint:nilnil // a block without records contributes none
	}
	for _, o := range open {
		if cr.c.blocks[o.pos].first == b.first && bytes.Equal(o.buf.raw, h.buf.raw) {
			o.origins = append(o.origins, b.pack)
			p.loader.release(h)

			return nil, nil //nolint:nilnil // a duplicate contributes no records, which load returns as nil
		}
	}

	cr.held[pos] = h
	o := &openBlock{pos: pos, buf: h.buf, d: h.d, origins: []int{b.pack}}
	o.seq = o.seqAt(0)

	return o, nil
}

// done drops the cursor's ownership of the open block o, whose last record was resolved or which the cursor gave up.
func (cr *clusterResolver) done(o *openBlock) {
	h := cr.held[o.pos]
	cr.held[o.pos] = nil
	cr.run.p.loader.release(h)
}

// countCopies counts the n copies of a seq group just gathered,
// and checks ctx when the count reaches or passes a multiple of resolveCtxCopies.
// A group whose copies take the count across several multiples gets one check:
// gathering a group reads nothing, each block load checking ctx itself,
// and a group is no larger than the copies the held blocks hold.
//
// Returns:
//   - error: ctx's error, wrapped, when the check fails.
func (r *mergeIterateRun) countCopies(ctx context.Context, n int) error {
	if !r.copies.add(n) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: merge iterate: %w", err)
	}

	return nil
}

// packID returns the pack_id of the reader at index i of the read.
func (r *mergeIterateRun) packID(i int) UUID {
	return UUID(r.p.readers[i].hdr.PackID)
}

// yield passes c's record to fn in *it, as the read yields it, and counts the call,
// after checking ctx when the call is the read's 4096th, 8192nd, and so on.
// It leaves c unreleased, and resets *it to the zero Item once fn returns,
// so the Item keeps no block the read releases.
//
// Returns:
//   - error: ctx's error, wrapped, fn not called; fn's error, as is.
func (r *mergeIterateRun) yield(ctx context.Context, c *candidate, it *Item, fn func(*Item) error) error {
	if r.yields.add(1) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: merge iterate: %w", err)
		}
	}
	c.item(r.q, it)
	err := fn(it)
	*it = Item{}

	return err
}

// add adds n, not negative, to the count,
// and reports whether the count reached or passed a multiple of k.step.
func (k *ctxCounter) add(n int) bool {
	if n < k.step-k.n {
		k.n += n

		return false
	}
	// k.n and n%k.step are both below k.step, so their sum cannot overflow.
	k.n = (k.n + n%k.step) % k.step

	return true
}
