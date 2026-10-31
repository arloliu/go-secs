package tracepack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
)

// completenessGaps adds, in this order, the reasons beside the window's own that make a lookup incomplete
// (the tracepack semantics specification §7.2, `incomplete` reasons):
// the coverage entries of the primary's capture that meet the lookup's query, the crash gaps that meet the hours scheduled,
// the capture-boundary records of the primary's epoch, and its ordering-uncertain records.
// The time range is the one of the hours scheduled, [key.Hour, key.Hour + MaxHours), in nanoseconds:
// evaluation runs only once every one of them is read.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) completenessGaps(ctx context.Context, w *txWindow) error {
	// checkFindTransaction bounded the last hour by MaxTxHour, so the range fits.
	from, to := l.key.Hour*hourNs, (l.key.Hour+int64(l.opts.MaxHours))*hourNs
	if err := l.coverageGaps(ctx, w, from, to); err != nil {
		return err
	}
	if err := l.crashGapGaps(ctx, from, to); err != nil {
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

	return nil
}

// coverageGaps adds a TxGapCoverage gap for each coverage entry of a pack read, of the primary's capture,
// that meets the lookup's query (coverageMeetsWindow), the gap holding a copy of the entry.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) coverageGaps(ctx context.Context, w *txWindow, from, to int64) error {
	for i := range l.coverage {
		if err := l.tick(ctx); err != nil {
			return err
		}
		c := &l.coverage[i]
		if c.capture != l.key.CaptureID || !coverageMeetsWindow(c.cov, w, from, to) {
			continue
		}
		// Charged as a copy, then copied.
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapCoverage, Hours: []int64{c.hour}, Pack: new(c.pack), Block: -1, Offset: -1, Coverage: c.cov,
			Err: errors.New("a coverage entry of the pack meets the window and the hours scheduled"),
		}); err != nil {
			return err
		}
		l.res.Incomplete[len(l.res.Incomplete)-1].Coverage = cloneCoverage(c.cov)
	}

	return nil
}

// coverageMeetsWindow reports whether the coverage entry c, of the primary's capture, meets the lookup's query
// (the tracepack format specification §5):
// the seqs of the window w, (p, e), or (p, ∞) while it is unbounded, and the time range [from, to).
// An empty window, e = p + 1, holds no seq to meet, so only an entry inverted on either side meets it,
// since such an entry meets every query of its capture.
func coverageMeetsWindow(c *Coverage, w *txWindow, from, to int64) bool {
	if w.bounded && w.e-w.p <= 1 {
		return inverted(c.SeqFirst, c.SeqLast) || inverted(c.TimeStart, c.TimeEnd)
	}
	last := uint64(math.MaxUint64)
	if w.bounded {
		last = w.e - 1
	}

	return coverageMeets(c, w.p+1, last, from, to)
}

// crashGapGaps adds a TxGapCrashGap gap for each crash gap the reads reported, of any capture,
// whose gap meets the time range [from, to) by the matching rule of a coverage entry (crashGapMeets),
// in the order the reads reported them, each boundary value once (noteCrashGap):
// the range is that of the hours scheduled, or of the primary's hour on an early return,
// never the unbounded selection of the hour reads (the tracepack semantics specification §7.2).
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) crashGapGaps(ctx context.Context, from, to int64) error {
	for i := range l.crash {
		if err := l.tick(ctx); err != nil {
			return err
		}
		c := &l.crash[i]
		if !crashGapMeets(c.b, &from, &to) {
			continue
		}
		if err := l.addBoundaryGap(ctx, TxGap{
			Reason: TxGapCrashGap, Hours: []int64{c.hour}, Pack: new(c.pack), Block: -1, Offset: -1,
			Err: fmt.Errorf("the gap of the stop-unclean boundary at seq %d of capture %s meets the lookup's hours: its records may be lost",
				c.b.Seq, c.b.CaptureID),
		}, c.b); err != nil {
			return err
		}
	}

	return nil
}

// captureBoundaryGaps adds a TxGapCaptureBoundary gap for each capture-boundary record of the primary's epoch
// that an hour's read yielded above the primary (the tracepack semantics specification §7.2, the epoch-boundary condition),
// a stop-unclean included.
// The clean stop of the primary's epoch that bounds the window is not one (epochStopBounds).
// A clean stop of another epoch that bounds the window still bounds it,
// but it is not the boundary of the primary's epoch that a record at e would be.
//
// Returns:
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) captureBoundaryGaps(ctx context.Context, w *txWindow) error {
	stopBounds, err := l.epochStopBounds(ctx, w)
	if err != nil {
		return err
	}
	for i := range l.boundaries {
		if err := l.tick(ctx); err != nil {
			return err
		}
		r := &l.boundaries[i]
		b := &r.boundary
		if stopBounds && b.Seq == w.e && b.Kind == BoundaryKindStop {
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

// epochStopBounds reports whether the version that bounds the window w
// is a capture-boundary record of kind stop of the primary's epoch,
// the one clean stop whose boundary at e captureBoundaryGaps does not report
// (the tracepack semantics specification §7.2).
// A boundary is identified with its epoch (the tracepack corpus specification §5.11),
// so a bounding stop of another epoch is not the boundary of the primary's epoch at e.
//
// Returns:
//   - bool: whether it is.
//   - error: ctx's error, as is.
func (l *txLookup) epochStopBounds(ctx context.Context, w *txWindow) (bool, error) {
	if !w.bounded {
		return false, nil
	}
	for i := l.firstRecordAt(w.e); i < len(l.res.Records) && l.res.Records[i].Record.Seq == w.e; i++ {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		if v := &l.res.Records[i]; v.Bound && v.Record.Epoch == l.prim.epoch && isStopRecord(&v.Record) {
			return true, nil
		}
	}

	return false, nil
}

// addBoundaryGap charges g with a copy of b as its Boundary, then adds it with that copy.
//
// Returns:
//   - error: ctx's error, as is; the charge's error, wrapping ErrReadLimit.
func (l *txLookup) addBoundaryGap(ctx context.Context, g TxGap, b *Boundary) error {
	// Charged as a copy, then copied.
	g.Boundary = b
	if err := l.addGap(ctx, g); err != nil {
		return err
	}
	l.res.Incomplete[len(l.res.Incomplete)-1].Boundary = new(cloneBoundary(*b))

	return nil
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
