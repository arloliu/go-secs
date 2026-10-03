package tracepack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
)

// completenessGaps adds, in this order, the gaps that keep a lookup from establishing absence beside the window's own
// (the tracepack semantics specification §7.2, the tracepack storage specification §5, Completeness):
// the coverage entries that meet the lookup's query, the epoch barrier, the time barriers,
// the capture-boundaries of the primary's epoch, its ordering-uncertain records, partial evidence and the contradictions.
// The searched time range is the one of the hours read, [key.Hour, key.Hour + MaxScopes), in nanoseconds:
// evaluation runs only once every one of them is read.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit;
//     the error of the observation's Barriers, wrapped.
func (l *txLookup) completenessGaps(ctx context.Context, w *txWindow) error {
	// checkFindTransaction bounded the last hour by MaxTxHour, so the range fits.
	from, to := l.key.Hour*hourNs, (l.key.Hour+int64(l.opts.MaxScopes))*hourNs
	if err := l.coverageGaps(ctx, w, from, to); err != nil {
		return err
	}
	if err := l.epochBarrierGaps(ctx); err != nil {
		return err
	}
	if err := l.timeBarrierGaps(ctx, from, to); err != nil {
		return err
	}
	if err := l.captureBoundaryGaps(ctx, w); err != nil {
		return err
	}
	if seen, ok := l.uncertain[l.prim.epoch]; ok {
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapOrderingUncertain, Hours: []int64{seen.hour}, Pack: new(seen.pack), Block: seen.block, Offset: -1,
			Seq: new(seen.seq), Err: fmt.Errorf("a record of epoch %d carries ordering-uncertain", l.prim.epoch),
		}); err != nil {
			return err
		}
	}
	if l.evidence.Partial {
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapEvidence, Block: -1, Offset: -1, Err: errors.New("the source marks the per-capture evidence partial"),
		}); err != nil {
			return err
		}
	}
	if l.trustClosures != nil {
		return nil
	}

	return l.contradictionGaps(ctx)
}

// coverageGaps adds a TxGapCoverage gap for each coverage entry of a pack read that meets the lookup's query
// (coverageMeetsWindow), the gap holding a copy of the entry.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) coverageGaps(ctx context.Context, w *txWindow, from, to int64) error {
	for i := range l.coverage {
		if err := l.tick(ctx); err != nil {
			return err
		}
		c := &l.coverage[i]
		if !coverageMeetsWindow(c.cov, l.key.Capture, w, from, to) {
			continue
		}
		// Charged as a copy, then copied.
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapCoverage, Hours: []int64{c.hour}, Pack: new(c.pack), Block: -1, Offset: -1, Coverage: c.cov,
			Err: errors.New("a coverage entry of the pack meets the window and the hours read"),
		}); err != nil {
			return err
		}
		l.res.Gaps[len(l.res.Gaps)-1].Coverage = cloneCoverage(c.cov)
	}

	return nil
}

// coverageMeetsWindow reports whether the coverage entry c of a pack of capture meets the lookup's query
// (the tracepack format specification §5):
// the seqs of the window w, (p, e), or (p, ∞) while it is unbounded, and the time range [from, to).
// An empty window, e = p + 1, holds no seq to meet, so only an entry inverted on either side meets it,
// since such an entry meets every query of its capture.
func coverageMeetsWindow(c *Coverage, capture UUID, w *txWindow, from, to int64) bool {
	if w.bounded && w.e-w.p <= 1 {
		return (c.CaptureID == nil || *c.CaptureID == capture) && (inverted(c.SeqFirst, c.SeqLast) || inverted(c.TimeStart, c.TimeEnd))
	}
	last := uint64(math.MaxUint64)
	if w.bounded {
		last = w.e - 1
	}

	return coverageMeets(c, capture, w.p+1, last, from, to)
}

// epochBarrierGaps adds the epoch barrier (the tracepack storage specification §5, Completeness):
// a TxGapBarrier gap for each stop-unclean boundary of the primary's capture in its per-capture evidence,
// unless the lookup read a closing record of the primary's epoch (epochClosed).
// A recorded close_seq never exempts the epoch by itself.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) epochBarrierGaps(ctx context.Context) error {
	checked := false
	for i := range l.evidence.Boundaries {
		if err := l.tick(ctx); err != nil {
			return err
		}
		b := &l.evidence.Boundaries[i]
		if b.Kind != BoundaryKindStopUnclean {
			continue
		}
		if !checked {
			closed, err := l.epochClosed(ctx)
			if err != nil || closed {
				return err
			}
			checked = true
		}
		if err := l.addBoundaryGap(ctx, TxGap{
			Reason: TxGapBarrier, Block: -1, Offset: -1,
			Err: fmt.Errorf("the capture ended unclean while no record read closes epoch %d", l.prim.epoch),
		}, b); err != nil {
			return err
		}
	}

	return nil
}

// epochClosed reports whether the lookup read a record that closes the primary's epoch:
// a version above the primary, without a conflict, that is a closing record of the primary's epoch,
// whether or not it bounds the window.
// A capture-boundary stop is a closing record of any epoch, so its epoch is checked.
//
// Returns:
//   - bool: whether such a version was read.
//   - error: ctx's error, as is.
func (l *txLookup) epochClosed(ctx context.Context) (bool, error) {
	for i := range l.res.Records {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		v := &l.res.Records[i]
		if v.Record.Seq > l.key.Seq && v.Class.Has(TxClosing) && !v.Conflict && v.Record.Epoch == l.prim.epoch {
			return true, nil
		}
	}

	return false, nil
}

// timeBarrierGaps adds the time barriers (the tracepack storage specification §5, Completeness):
// a TxGapBarrier gap for each stop-unclean boundary of a capture of the tool
// whose gap interval meets [from, to) (barrierMeets),
// as the observation's Barriers returns them; a boundary it returns that is not one is passed over.
// ctx is checked before the call.
//
// Returns:
//   - error: ctx's error, as is; the observation's error, wrapped; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) timeBarrierGaps(ctx context.Context, from, to int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bs, err := l.obs.Barriers(ctx, from, to)
	if err != nil {
		return fmt.Errorf("barriers: %w", err)
	}
	for i := range bs {
		if err := l.tick(ctx); err != nil {
			return err
		}
		b := &bs[i]
		if b.Kind != BoundaryKindStopUnclean || !barrierMeets(b, from, to) {
			continue
		}
		if err := l.addBoundaryGap(ctx, TxGap{
			Reason: TxGapBarrier, Block: -1, Offset: -1,
			Err: fmt.Errorf("a stop-unclean boundary of capture %s has a gap that meets the hours read", b.Capture),
		}, b); err != nil {
			return err
		}
	}

	return nil
}

// captureBoundaryGaps adds a TxGapCaptureBoundary gap for each capture-boundary of the primary's epoch
// (the tracepack semantics specification §7.2):
// each entry of the per-capture evidence, anywhere in the epoch,
// then each capture-boundary record of the epoch a scope read yielded above the primary,
// except one whose seq and kind an entry of the evidence already has.
// The clean stop that bounds the window is not one: a version at e, the bound, whose bytes are a capture-boundary stop.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) captureBoundaryGaps(ctx context.Context, w *txWindow) error {
	stopBound, err := l.stopBound(ctx, w)
	if err != nil {
		return err
	}
	bounds := func(b *Boundary) bool { return stopBound && b.Seq == w.e && b.Kind == BoundaryKindStop }
	for i := range l.evidence.Boundaries {
		if err := l.tick(ctx); err != nil {
			return err
		}
		b := &l.evidence.Boundaries[i]
		if b.Epoch != l.prim.epoch || bounds(b) {
			continue
		}
		if err := l.addBoundaryGap(ctx, TxGap{
			Reason: TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: new(b.Seq),
			Err: fmt.Errorf("the per-capture evidence has a %v boundary in epoch %d", b.Kind, b.Epoch),
		}, b); err != nil {
			return err
		}
	}
	for i := range l.boundaries {
		if err := l.tick(ctx); err != nil {
			return err
		}
		r := &l.boundaries[i]
		b := &r.boundary
		if bounds(b) {
			continue
		}
		recorded, err := l.evidenceHasBoundary(ctx, b)
		if err != nil {
			return err
		}
		if recorded {
			continue
		}
		if err := l.addBoundaryGap(ctx, TxGap{
			Reason: TxGapCaptureBoundary, Hours: []int64{r.at.hour}, Pack: new(r.at.pack), Block: r.at.block, Offset: -1, Seq: new(b.Seq),
			Err: fmt.Errorf("a %v boundary record of epoch %d was read", b.Kind, b.Epoch),
		}, b); err != nil {
			return err
		}
	}

	return nil
}

// stopBound reports whether the version that bounds the window w is a capture-boundary record of kind stop.
//
// Returns:
//   - bool: whether it is.
//   - error: ctx's error, as is.
func (l *txLookup) stopBound(ctx context.Context, w *txWindow) (bool, error) {
	if !w.bounded {
		return false, nil
	}
	for i := l.firstRecordAt(w.e); i < len(l.res.Records) && l.res.Records[i].Record.Seq == w.e; i++ {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		if v := &l.res.Records[i]; v.Bound && isStopRecord(&v.Record) {
			return true, nil
		}
	}

	return false, nil
}

// evidenceHasBoundary reports whether the per-capture evidence has an entry of b's seq, kind and epoch.
// The entries are in ascending seq; each entry of b's seq counts toward the checks of ctx (tick),
// since a source may repeat an entry.
//
// Returns:
//   - bool: whether the evidence has such an entry.
//   - error: ctx's error, as is.
func (l *txLookup) evidenceHasBoundary(ctx context.Context, b *Boundary) (bool, error) {
	bs := l.evidence.Boundaries
	i, _ := slices.BinarySearchFunc(bs, b.Seq, func(e Boundary, seq uint64) int { return cmp.Compare(e.Seq, seq) })
	for ; i < len(bs) && bs[i].Seq == b.Seq; i++ {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		if bs[i].Kind == b.Kind && bs[i].Epoch == b.Epoch {
			return true, nil
		}
	}

	return false, nil
}

// addBoundaryGap charges g with a copy of b as its Barrier, then adds it with that copy.
//
// Returns:
//   - error: ctx's error, as is; the charge's error, wrapping ErrReadLimit.
func (l *txLookup) addBoundaryGap(ctx context.Context, g TxGap, b *Boundary) error {
	// Charged as a copy, then copied.
	g.Barrier = b
	if err := l.addGap(ctx, g); err != nil {
		return err
	}
	l.res.Gaps[len(l.res.Gaps)-1].Barrier = new(cloneBoundary(*b))

	return nil
}

// contradictionGaps checks the per-capture evidence's closures against the records read
// (the tracepack semantics specification §7.2):
// the close_seq of the primary's epoch, which claims a record that ends the epoch,
// and each clean stop boundary, which claims a capture-boundary record of kind stop in the boundary's epoch.
// A claim at or below the primary, or at a seq the lookup read where no version is what it claims,
// adds a TxGapContradiction gap.
// A claim at a seq the lookup did not read is neither used nor checked.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) contradictionGaps(ctx context.Context) error {
	for i := range l.evidence.Closures {
		if err := l.tick(ctx); err != nil {
			return err
		}
		c := &l.evidence.Closures[i]
		if c.Epoch != l.prim.epoch {
			continue
		}
		ends := func(v *TxRecord) bool { return v.Class.Has(TxClosing) && v.Record.Epoch == c.Epoch }
		if err := l.checkClaim(ctx, c.CloseSeq, ends, fmt.Sprintf("the close_seq %d of epoch %d", c.CloseSeq, c.Epoch)); err != nil {
			return err
		}
	}
	for i := range l.evidence.Boundaries {
		if err := l.tick(ctx); err != nil {
			return err
		}
		b := &l.evidence.Boundaries[i]
		if b.Kind != BoundaryKindStop {
			continue
		}
		stops := func(v *TxRecord) bool { return isStopRecord(&v.Record) && v.Record.Epoch == b.Epoch }
		if err := l.checkClaim(ctx, b.Seq, stops, fmt.Sprintf("the stop boundary of epoch %d at seq %d", b.Epoch, b.Seq)); err != nil {
			return err
		}
	}

	return nil
}

// checkClaim checks an evidence closure, named by claim, which claims that seq holds a version matching is:
// a claim at or below the primary, or at a seq the lookup read where no kept version is one, adds a TxGapContradiction gap.
// Every version that could be one plays a role, so the kept versions of a seq are the ones to look at,
// except at a seq that the read of scope key.Hour yielded before the primary, whose versions it neither classified nor kept:
// a claim there is not checked, the order of that read being broken.
//
// Returns:
//   - error: ctx's error, as is; the error of the gap's charge, wrapping ErrReadLimit.
func (l *txLookup) checkClaim(ctx context.Context, seq uint64, is func(v *TxRecord) bool, claim string) error {
	var why string
	if seq <= l.key.Seq {
		why = fmt.Sprintf("%s of the per-capture evidence is at or below the primary's seq %d", claim, l.key.Seq)
	} else {
		_, visited, err := l.visitedFrom(ctx, seq)
		if err != nil || !visited {
			return err
		}
		for i := l.firstRecordAt(seq); i < len(l.res.Records) && l.res.Records[i].Record.Seq == seq; i++ {
			if err := l.tick(ctx); err != nil {
				return err
			}
			if is(&l.res.Records[i]) {
				return nil
			}
		}
		if l.earlyRuns.contains(seq) {
			return nil
		}
		why = fmt.Sprintf("%s of the per-capture evidence names a seq the lookup read, where no version is the closing record claimed", claim)
	}

	return l.addGap(ctx, TxGap{Reason: TxGapContradiction, Block: -1, Offset: -1, Seq: new(seq), Err: errors.New(why)})
}

// firstRecordAt returns the index of the first kept version of seq in the sorted records, or of the first above it.
func (l *txLookup) firstRecordAt(seq uint64) int {
	i, _ := slices.BinarySearchFunc(l.res.Records, seq, func(v TxRecord, seq uint64) int { return cmp.Compare(v.Record.Seq, seq) })

	return i
}

// isStopRecord reports whether rec is a capture-boundary record of kind stop:
// a transport-event record whose payload decodes with event capture-boundary and boundary_kind stop.
func isStopRecord(rec *Record) bool {
	if rec.Kind != KindTransportEvent {
		return false
	}
	ev, err := UnmarshalTransportEvent(rec.Payload)

	return err == nil && ev.Event == EventCaptureBoundary && ev.BoundaryKind != nil && *ev.BoundaryKind == BoundaryKindStop
}
