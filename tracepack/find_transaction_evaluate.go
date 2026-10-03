package tracepack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
)

// txWindow is the final window of a transaction lookup, (p, e), open on both sides
// (the tracepack semantics specification §7.2).
type txWindow struct {
	// p is the primary's seq.
	p uint64
	// e is the window's end, valid iff bounded; an unbounded window is (p, ∞).
	e       uint64
	bounded bool
	// possible is the seq of the first possible same-key primary above p, valid iff hasPossible;
	// it lies below e or at e, where it decides nothing, since every candidate inside the window lies below e.
	possible    uint64
	hasPossible bool
}

// boundsWindow reports whether the kept version v can bound the window:
// a same-key primary, whether or not it conflicts, or a closing record without a conflict.
// Any conflict on a seq above the primary's makes the outcome TxIncomplete,
// so a disputed same-key primary decides nothing.
func boundsWindow(v *TxRecord) bool {
	return v.Class.Has(TxSameKeyPrimary) || v.Class.Has(TxClosing) && !v.Conflict
}

// in reports whether seq lies inside the window.
func (w *txWindow) in(seq uint64) bool {
	return seq > w.p && (!w.bounded || seq < w.e)
}

// evaluate decides the outcome once every scope is read (the tracepack semantics specification §7.2):
// it sorts the kept records, finds the final window, adds the gaps of the window,
// sets each kept version's window and candidate flags,
// adding a TxGapUnavailable gap for each version that could be the reply,
// adds the other gaps that keep absence from being established (completenessGaps),
// and decides the outcome from the valid matches and the gaps.
// Each gap is charged before it is added (addGap), and each loop counts its iterations toward the checks of ctx (tick).
//
// Returns:
//   - error: ctx's error, the error of a charge, wrapping ErrReadLimit, or the error of the observation's Barriers, wrapped;
//     the result holds what was decided until then, without an outcome.
func (l *txLookup) evaluate(ctx context.Context) error {
	if err := l.evaluateWindow(ctx); err != nil {
		return fmt.Errorf("tracepack: find transaction: %w", err)
	}

	return nil
}

// evaluateWindow is evaluate, its errors not wrapped.
func (l *txLookup) evaluateWindow(ctx context.Context) error {
	if err := l.sortRecords(ctx); err != nil {
		return err
	}
	w, err := l.finalWindow(ctx)
	if err != nil {
		return err
	}
	if l.trustClosures != nil {
		l.trustClosures(&w)
	}
	if w.bounded {
		l.res.WindowEnd = new(w.e)
	}
	if err := l.windowGaps(ctx, &w); err != nil {
		return err
	}
	valid, err := l.flagRecords(ctx, &w)
	if err != nil {
		return err
	}

	if err := l.completenessGaps(ctx, &w); err != nil {
		return err
	}
	outcome, err := l.decide(ctx, valid)
	if err != nil {
		return err
	}
	l.res.Outcome = outcome

	return nil
}

// sortRecords sorts the kept versions by seq, then hour of the read, keeping the order in which each read yielded them.
// The versions were added read by read, in the order of the hours read, and each read's in the order it yielded them,
// so a stable sort by seq and hour keeps the version order within a read.
// The sort cannot check ctx, so every version counts once toward the checks of ctx (tick) before it.
//
// Returns:
//   - error: ctx's error, as is, the versions not sorted.
func (l *txLookup) sortRecords(ctx context.Context) error {
	for range l.res.Records {
		if err := l.tick(ctx); err != nil {
			return err
		}
	}
	slices.SortStableFunc(l.res.Records, func(a, b TxRecord) int {
		if c := cmp.Compare(a.Record.Seq, b.Record.Seq); c != 0 {
			return c
		}

		return cmp.Compare(a.Hour, b.Hour)
	})

	return nil
}

// finalWindow finds the final window over the sorted kept versions:
// its end e, the smallest seq above the primary's of a version that can bound it (boundsWindow),
// and the first possible same-key primary inside it, whether or not that version conflicts.
// The search stops at the first version that bounds the window,
// so a possible same-key primary found lies below e, or at e, where it bounds nothing that lies inside the window.
//
// Returns:
//   - txWindow: the window.
//   - error: ctx's error, as is.
func (l *txLookup) finalWindow(ctx context.Context) (txWindow, error) {
	w := txWindow{p: l.key.Seq}
	for i := range l.res.Records {
		if err := l.tick(ctx); err != nil {
			return txWindow{}, err
		}
		v := &l.res.Records[i]
		seq := v.Record.Seq
		if seq <= w.p {
			continue
		}
		if boundsWindow(v) {
			w.e, w.bounded = seq, true
			break
		}
		if !w.hasPossible && v.Class.Has(TxPossiblePrimary) {
			w.possible, w.hasPossible = seq, true
		}
	}

	return w, nil
}

// windowGaps adds the gap of the window:
// a TxGapSeqGap gap at the first seq of a bounded window that no scope read yielded,
// or a TxGapOpenWindow gap when no record read bounds it.
//
// Returns:
//   - error: ctx's error, as is; the gap's charge's error, wrapping ErrReadLimit.
func (l *txLookup) windowGaps(ctx context.Context, w *txWindow) error {
	if !w.bounded {
		return l.addGap(ctx, TxGap{
			Reason: TxGapOpenWindow, Block: -1, Offset: -1,
			Err: fmt.Errorf("no record read in hours [%d, %d) bounds the window", l.key.Hour, l.key.Hour+int64(l.opts.MaxScopes)),
		})
	}
	if l.skipSeqGap {
		return nil
	}
	missing, ok, err := l.firstMissing(ctx, w)
	if err != nil || !ok {
		return err
	}

	return l.addGap(ctx, TxGap{
		Reason: TxGapSeqGap, Block: -1, Offset: -1, Seq: new(missing),
		Err: fmt.Errorf("no scope read yields seq %d of the window (%d, %d)", missing, w.p, w.e),
	})
}

// firstMissing finds the first seq of the bounded window w that lies in no run set of the scope reads.
// It walks from the window's first seq, each step jumping past the run of some read that holds the seq reached.
// Each step, and each probe of a read's run set, counts toward the checks of ctx (tick).
//
// Returns:
//   - uint64: the first seq missing, valid iff the bool is true.
//   - bool: whether a seq of the window is missing; false for an empty window (e = p + 1).
//   - error: ctx's error, as is.
func (l *txLookup) firstMissing(ctx context.Context, w *txWindow) (uint64, bool, error) {
	if w.e-w.p <= 1 {
		return 0, false, nil
	}
	// cur is the first seq of the window not yet known to be visited: w.p < cur < w.e.
	cur := w.p + 1
	for {
		if err := l.tick(ctx); err != nil {
			return 0, false, err
		}
		last, ok, err := l.visitedFrom(ctx, cur)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return cur, true, nil
		}
		// The run may end at the top of the uint64 range, so it is compared before cur passes it.
		if last >= w.e-1 {
			return 0, false, nil
		}
		cur = last + 1
	}
}

// visitedFrom finds a run of a scope read that holds seq.
// Each probe of a non-empty run set counts toward the checks of ctx (tick).
//
// Returns:
//   - uint64: the last seq of the run found, valid iff the bool is true.
//   - bool: whether a read's run set holds seq.
//   - error: ctx's error, as is.
func (l *txLookup) visitedFrom(ctx context.Context, seq uint64) (uint64, bool, error) {
	for _, rd := range l.reads {
		runs := rd.runs.runs
		if len(runs) == 0 {
			continue
		}
		if err := l.tick(ctx); err != nil {
			return 0, false, err
		}
		i, _ := slices.BinarySearchFunc(runs, seq, func(r txRun, seq uint64) int { return cmp.Compare(r.last, seq) })
		if i < len(runs) && runs[i].first <= seq {
			return runs[i].last, true, nil
		}
	}

	return 0, false, nil
}

// flagRecords sets the window and candidate flags of every kept version against w:
// InWindow, Bound for a version at e that bounds the window,
// and, for a candidate, Decidable, Eligible and Valid (the tracepack semantics specification §7.2).
// A version inside the window that could be the reply adds a TxGapUnavailable gap (unavailableGap).
//
// Returns:
//   - int: the number of valid matches.
//   - error: ctx's error, as is; the error of a gap's charge, wrapping ErrReadLimit.
func (l *txLookup) flagRecords(ctx context.Context, w *txWindow) (int, error) {
	valid := 0
	for i := range l.res.Records {
		if err := l.tick(ctx); err != nil {
			return valid, err
		}
		v := &l.res.Records[i]
		seq := v.Record.Seq
		v.InWindow = w.in(seq)
		v.Bound = w.bounded && seq == w.e && boundsWindow(v)
		if v.Class.Has(TxCandidate) {
			l.flagCandidate(v, w)
			if v.Valid {
				valid++
			}
		}
		if why := l.unavailable(v, w); why != "" {
			if err := l.addGap(ctx, TxGap{
				Reason: TxGapUnavailable, Hours: []int64{v.Hour}, Pack: new(v.Pack), Block: v.Block, Offset: -1, Seq: new(seq),
				Err: errors.New(why),
			}); err != nil {
				return valid, err
			}
		}
	}

	return valid, nil
}

// flagCandidate sets the candidate flags of the candidate v against w:
// Decidable when its stream and function and the primary's stream are available;
// Eligible when its seq lies inside the window and below the first possible same-key primary inside it;
// Valid when it is eligible and decidable, with the primary's stream and the primary's function + 1, or function 0.
func (l *txLookup) flagCandidate(v *TxRecord, w *txWindow) {
	h := v.Record.HSMSHeader()
	seq := v.Record.Seq
	v.Decidable = l.prim.streamAvailable && h.Available&FieldValidityStreamAndW != 0 && h.Available&FieldValidityFunction != 0
	v.Eligible = w.in(seq) && (!w.hasPossible || seq < w.possible)
	v.Valid = v.Eligible && v.Decidable && h.Stream == l.prim.stream &&
		(int(h.Function) == int(l.prim.function)+1 || h.Function == 0)
}

// unavailable returns why the kept version v, its flags set, could be the reply although the lookup cannot decide it,
// or "" when it could not:
// inside the window, a possible reply, an eligible candidate that is not decidable,
// or a candidate at or after the first possible same-key primary.
func (l *txLookup) unavailable(v *TxRecord, w *txWindow) string {
	switch {
	case !v.InWindow:
		return ""
	case v.Class.Has(TxPossibleReply):
		return fmt.Sprintf("seq %d could be the reply: a field it would need is unknown or unavailable", v.Record.Seq)
	case v.Class.Has(TxCandidate) && v.Eligible && !v.Decidable:
		return fmt.Sprintf("the candidate at seq %d cannot be decided: a stream or function is unavailable", v.Record.Seq)
	case v.Class.Has(TxCandidate) && !v.Eligible:
		return fmt.Sprintf("the candidate at seq %d lies at or after a possible primary of the same key at seq %d", v.Record.Seq, w.possible)
	default:
		return ""
	}
}

// decide returns the outcome from the valid matches and the gaps listed (the tracepack semantics specification §7.2):
// TxIncomplete beside a TxGapNoKey, TxGapConflict or TxGapIndex gap;
// otherwise TxMatched for one valid match and TxAmbiguous for several, whatever other gap is listed;
// otherwise TxUnmatched when no gap is listed, TxIncomplete when one is.
//
// Returns:
//   - TxOutcome: the outcome.
//   - error: ctx's error, as is.
func (l *txLookup) decide(ctx context.Context, valid int) (TxOutcome, error) {
	for i := range l.res.Gaps {
		if err := l.tick(ctx); err != nil {
			return 0, err
		}
		// Any other gap only prevents TxUnmatched, below.
		if r := l.res.Gaps[i].Reason; r == TxGapNoKey || r == TxGapConflict || r == TxGapIndex {
			return TxIncomplete, nil
		}
	}
	switch {
	case valid == 1:
		return TxMatched, nil
	case valid > 1:
		return TxAmbiguous, nil
	case len(l.res.Gaps) == 0:
		return TxUnmatched, nil
	default:
		return TxIncomplete, nil
	}
}
