package tracepack

import (
	"bytes"
	"context"
	"fmt"
	"slices"
)

// planBlock describes one block of a read over several packs as its index states it:
// its F-2 entry and F-3 summary for an indexed block,
// its computed summary, taken from its records by a read before the first record is yielded, for a walked one
// (the tracepack semantics specification §7.4).
type planBlock struct {
	// pack is the index of the block's Reader among the readers of the read, and block its index into that Reader's blocks.
	pack  int
	block int
	// first is the block's first seq.
	first uint64
	// span holds the block's last seq and its ranges of ts_utc_ns and epoch.
	span recordSpan
	// kindCounts and dirCounts count the block's records per raw kind and dir value, a missing element counting 0.
	// For an indexed block they alias the Reader's validated footer, which is never modified.
	kindCounts []uint32
	dirCounts  []uint32
}

// planCluster is one overlap cluster of the blocks of one capture across the readers of a read,
// in ascending first seq, then in reader order, then in block order.
type planCluster struct {
	seqCluster[planBlock]
	// tsMin is the smallest ts_min of the cluster's blocks.
	tsMin int64
	// excluded is set when the query excludes every block of the cluster, which is then not read.
	excluded bool
}

// planCapture is one capture of a read over several packs: its capture_id and its overlap clusters in ascending first seq.
type planCapture struct {
	id       UUID
	clusters []planCluster
}

// mergeIteratePlan is what MergeIterate builds before it yields the first record:
// the status of the read so far, the overlap clusters of every capture with their exclusion,
// and the loader whose budget every block read of the read reserves against
// (the tracepack semantics specification §7.4).
type mergeIteratePlan struct {
	// opts are the options with their defaults applied.
	opts    MergeIterateOptions
	readers []*Reader
	// res holds the defects and footer errors found so far.
	res Result
	// captures holds the read's captures in ascending capture_id.
	captures []planCapture
	loader   heldLoader
	// reserveConflict is mergeIterateOptions.reserveConflict; nil checks MaxConflicts instead.
	reserveConflict func() error
	// copiesStep and yieldStep, set only by tests, replace resolveCtxCopies and yieldCtxCalls in the read's checks of ctx;
	// zero keeps them.
	copiesStep int
	yieldStep  int
}

// checkMergeIterate validates the arguments of MergeIterate before anything is read
// and applies the defaults of opts.
//
// It checks, in this order: fn is not nil; opts.Order is an order; q is valid;
// no reader is nil; no *Reader is given twice; no two readers hold the same pack_id;
// then ctx is not done.
// An empty readers is valid.
//
// Returns:
//   - MergeIterateOptions: opts with MaxHeldBytes and MaxConflicts defaulted when zero or negative.
//   - error: an error wrapping ErrInvalidQuery, naming the readers concerned for a fault of readers;
//     ctx's error, wrapped.
func checkMergeIterate(ctx context.Context, readers []*Reader, q *Query, opts MergeIterateOptions, fn func(*Item) error) (MergeIterateOptions, error) {
	if fn == nil {
		return opts, fmt.Errorf("%w: fn is nil", ErrInvalidQuery)
	}
	if opts.Order != OrderCapture && opts.Order != OrderTime {
		return opts, fmt.Errorf("%w: order %v", ErrInvalidQuery, opts.Order)
	}
	if err := q.validate(); err != nil {
		return opts, err
	}
	for i, r := range readers {
		if r == nil {
			return opts, fmt.Errorf("%w: reader %d is nil", ErrInvalidQuery, i)
		}
	}
	seen := make(map[*Reader]int, len(readers))
	for i, r := range readers {
		if k, ok := seen[r]; ok {
			return opts, fmt.Errorf("%w: readers %d and %d are the same Reader", ErrInvalidQuery, k, i)
		}
		seen[r] = i
	}
	ids := make(map[UUID]int, len(readers))
	for i, r := range readers {
		id := UUID(r.hdr.PackID)
		if k, ok := ids[id]; ok {
			return opts, fmt.Errorf("%w: readers %d and %d hold the same pack_id %s", ErrInvalidQuery, k, i, id)
		}
		ids[id] = i
	}
	if err := ctx.Err(); err != nil {
		return opts, fmt.Errorf("tracepack: merge iterate: %w", err)
	}

	if opts.MaxHeldBytes <= 0 {
		opts.MaxHeldBytes = DefaultMaxHeldBytes
	}
	if opts.MaxConflicts <= 0 {
		opts.MaxConflicts = DefaultMaxConflicts
	}

	return opts, nil
}

// newMergeIteratePlan returns an empty plan of a read with opts, whose defaults checkMergeIterate applied;
// build fills it.
func newMergeIteratePlan(opts MergeIterateOptions) *mergeIteratePlan {
	return &mergeIteratePlan{opts: opts, loader: heldLoader{budget: heldBudget{limit: opts.MaxHeldBytes}}}
}

// build plans the read of q over readers, which checkMergeIterate accepted
// (the tracepack semantics specification §7.4).
//
// It never modifies a Reader, since Readers are shared across concurrent reads.
// First it adds to the result, in reader order,
// each reader's defects of every read and its coverage defects for the query, as Reader.Iterate does,
// with Pack set to the reader's index,
// and its footer error to FooterErrs when its footer was not used.
// Then it reads each walked block once, in reader order then file order, checking ctx before each:
// a block that fails the read adds its defect and takes no further part;
// a usable block gives its computed summary and is dropped at once.
// Then it groups the blocks that take part by their reader's capture_id, captures in ascending capture_id,
// chains each capture's blocks into overlap clusters,
// and marks a cluster excluded when the footer or the computed summaries exclude every block of it from the query.
//
// Returns:
//   - error: ctx's error, wrapped;
//     an error wrapping ErrReadLimit when a walked block's reservation would exceed MaxHeldBytes;
//     a ReadAt error, wrapped with the reader and block index.
//     p.res then holds what was found before the error.
//     Nothing stays reserved after build, whatever its outcome.
func (p *mergeIteratePlan) build(ctx context.Context, readers []*Reader, q *Query) error {
	p.readers = readers
	f := &q.Filter
	for i, r := range readers {
		for _, d := range r.openDefects {
			d.Pack = i
			p.res.Incomplete = append(p.res.Incomplete, d)
		}
		n := len(p.res.Incomplete)
		p.res.Incomplete = r.appendCoverageDefects(p.res.Incomplete, f)
		for k := n; k < len(p.res.Incomplete); k++ {
			p.res.Incomplete[k].Pack = i
		}
		if r.footerErr != nil {
			p.res.FooterErrs = append(p.res.FooterErrs, PackError{Pack: i, Err: r.footerErr})
		}
	}

	byCapture := make(map[UUID][]planBlock)
	for i, r := range readers {
		capture := UUID(r.hdr.CaptureID)
		for j := range r.blocks {
			if r.blocks[j].Indexed {
				byCapture[capture] = append(byCapture[capture], r.indexedBlock(i, j))
				continue
			}

			b, ok, err := p.preRead(ctx, r, i, j)
			if err != nil {
				return err
			}
			if ok {
				byCapture[capture] = append(byCapture[capture], b)
			}
		}
	}

	ids := make([]UUID, 0, len(byCapture))
	for id := range byCapture {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b UUID) int { return bytes.Compare(a[:], b[:]) })

	p.captures = make([]planCapture, len(ids))
	for k, id := range ids {
		// Each capture's blocks were gathered in reader order, then block order, which the sort keeps among equal first seqs.
		chained := chainClusters(byCapture[id], func(b *planBlock) (uint64, uint64) { return b.first, b.span.lastSeq })
		clusters := make([]planCluster, len(chained))
		for c := range chained {
			clusters[c] = planClusterOf(chained[c], f)
		}
		p.captures[k] = planCapture{id: id, clusters: clusters}
	}

	return nil
}

// preRead reads walked block i of r, the Reader at index pack, once before the read yields anything,
// and returns its computed summary (the tracepack semantics specification §7.4).
// The block is dropped before preRead returns, whatever its outcome.
//
// Returns:
//   - planBlock: the block's computed summary.
//   - bool: whether the block takes part in the read; false when its read failed, its defect added to p.res.
//   - error: ctx's error, wrapped; the loader's error, as is.
func (p *mergeIteratePlan) preRead(ctx context.Context, r *Reader, pack, i int) (planBlock, bool, error) {
	if err := ctx.Err(); err != nil {
		return planBlock{}, false, fmt.Errorf("tracepack: merge iterate: %w", err)
	}

	h, def, err := p.loader.load(r, pack, i)
	if err != nil {
		return planBlock{}, false, err
	}
	// A walked block never has an index mismatch; this keeps any defect the loader returns beside a usable block.
	if def != nil {
		p.res.Incomplete = append(p.res.Incomplete, *def)
	}
	if h == nil {
		return planBlock{}, false, nil
	}

	b := computedBlock(pack, i, h.d)
	p.loader.drop(h)

	return b, true, nil
}

// computedBlock returns the computed summary of block i of the Reader at index pack, read as d:
// its first and last seq, its ranges of ts_utc_ns and epoch, and its kind and dir counts, taken from its records.
// It shares no memory with d.
func computedBlock(pack, i int, d *decodedBlock) planBlock {
	b := planBlock{pack: pack, block: i}
	for j := range d.count() {
		h := d.header(j)
		if j == 0 {
			b.first = h.Seq
		}
		b.span.add(j, &h)
		b.kindCounts = countEnum(b.kindCounts, h.Kind)
		b.dirCounts = countEnum(b.dirCounts, h.Dir)
	}

	return b
}

// indexedBlock returns the description of indexed block i of r, the Reader at index pack,
// from its F-2 entry and its F-3 summary in the validated footer.
func (r *Reader) indexedBlock(pack, i int) planBlock {
	info := &r.blocks[i]
	s := &r.footer.blocks[i]

	return planBlock{
		pack:  pack,
		block: i,
		first: info.FirstSeq,
		span: recordSpan{
			lastSeq: info.LastSeq,
			tsMin:   info.TSMin, tsMax: info.TSMax,
			epochMin: info.EpochMin, epochMax: info.EpochMax,
		},
		kindCounts: s.kindCounts,
		dirCounts:  s.dirCounts,
	}
}

// excludedBy reports whether the block's description excludes it from the query of f:
// its time range misses the query's, its epoch range holds none of f.Epochs,
// or its kind or dir counts are 0 for every value of f.Kinds or f.Dirs, a missing element counting 0
// (the tracepack format specification §10).
func (b *planBlock) excludedBy(f *Filter) bool {
	if !overlaps(&b.span.tsMin, &b.span.tsMax, f.TimeFrom, f.TimeTo) {
		return true
	}
	if len(f.Epochs) > 0 && !slices.ContainsFunc(f.Epochs, func(e uint32) bool { return e >= b.span.epochMin && e <= b.span.epochMax }) {
		return true
	}

	return !anyCounted(b.kindCounts, f.Kinds) || !anyCounted(b.dirCounts, f.Dirs)
}

// planClusterOf returns the cluster c with its ts_min,
// excluded when f excludes every block of it (the tracepack semantics specification §7.4).
func planClusterOf(c seqCluster[planBlock], f *Filter) planCluster {
	out := planCluster{seqCluster: c, tsMin: c.blocks[0].span.tsMin, excluded: true}
	for k := range c.blocks {
		b := &c.blocks[k]
		out.tsMin = min(out.tsMin, b.span.tsMin)
		if !b.excludedBy(f) {
			out.excluded = false
		}
	}

	return out
}
