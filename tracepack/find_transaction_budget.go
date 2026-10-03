package tracepack

import (
	"context"
	"fmt"
	"unsafe"
)

// The charges of a transaction lookup's state against TxOptions.MaxStateBytes.
// Each item of the state is charged a fixed size, the size of what it holds rounded up to a multiple of 16 bytes,
// plus the variable bytes it copies; the cost functions below are the only place these are summed.
// The charges are nominal: slice capacity, the copies between buffers and the text of errors are not charged,
// so the live memory of the state stays within a small constant factor of the charged total.
const (
	// txAlignMask is the mask of the rounding of every fixed charge.
	txAlignMask = 16 - 1
	// txRecordCharge is the fixed charge of a kept version, beside its payload and header-extension bytes.
	txRecordCharge = (int64(unsafe.Sizeof(TxRecord{})) + txAlignMask) &^ txAlignMask
	// txRunCharge is the charge of one run of a scope read's run set.
	txRunCharge = (int64(unsafe.Sizeof(txRun{})) + txAlignMask) &^ txAlignMask
	// txCrossCharge is the fixed charge of a seq in conflict across scope reads, its map entry included;
	// each hour it lists adds txHourCharge.
	txCrossCharge = (int64(unsafe.Sizeof(txCrossConflict{})+unsafe.Sizeof(uint64(0))+unsafe.Sizeof(uintptr(0))) +
		txAlignMask) &^ txAlignMask
	// txHourCharge is the charge of one hour a conflict across scope reads or a gap lists.
	txHourCharge = int64(unsafe.Sizeof(int64(0)))
	// txGapCharge is the fixed charge of a gap, its seq and pack_id included; each hour of its own Hours adds txHourCharge.
	txGapCharge = (int64(unsafe.Sizeof(TxGap{})+unsafe.Sizeof(uint64(0))+unsafe.Sizeof(UUID{})) +
		txAlignMask) &^ txAlignMask
	// txScopeCharge is the fixed charge of a scope read: its TxScope and its run set;
	// each pack of its view adds txPackCharge.
	txScopeCharge = (int64(unsafe.Sizeof(TxScope{})+unsafe.Sizeof(txRuns{})+unsafe.Sizeof(uintptr(0))) +
		txAlignMask) &^ txAlignMask
	// txPackCharge is the charge of one pack_id.
	txPackCharge = int64(unsafe.Sizeof(UUID{}))
	// txPackErrorCharge is the charge of a footer error.
	txPackErrorCharge = (int64(unsafe.Sizeof(TxPackError{})) + txAlignMask) &^ txAlignMask
	// txConflictedCharge is the charge of a seq in conflict within a scope read, its map entry included.
	txConflictedCharge = (int64(unsafe.Sizeof(uint64(0))) + txAlignMask) &^ txAlignMask
	// txUncertainCharge is the charge of an epoch with ordering-uncertain, its map entry included.
	txUncertainCharge = (int64(unsafe.Sizeof(uint32(0))+unsafe.Sizeof(txSeen{})) + txAlignMask) &^ txAlignMask
	// txPendingCoverageCharge is the fixed charge of a pending coverage entry, beside the copy of its Coverage.
	txPendingCoverageCharge = (int64(unsafe.Sizeof(txCoverage{})) + txAlignMask) &^ txAlignMask
	// txCoverageCharge is the fixed charge of a copied Coverage, the values of its five bounds included;
	// each Unknown entry adds txRawEntryCharge and its value bytes.
	txCoverageCharge = (int64(unsafe.Sizeof(Coverage{})+unsafe.Sizeof(UUID{})+4*unsafe.Sizeof(uint64(0))) +
		txAlignMask) &^ txAlignMask
	// txRawEntryCharge is the fixed charge of an Unknown entry of a copied Coverage.
	txRawEntryCharge = (int64(unsafe.Sizeof(RawEntry{})) + txAlignMask) &^ txAlignMask
	// txBoundaryCharge is the charge of a copied Boundary: the Boundary and the values of its two gap bounds.
	txBoundaryCharge = (int64(unsafe.Sizeof(Boundary{}))+txAlignMask)&^txAlignMask +
		2*int64(unsafe.Sizeof(int64(0)))
	// txBoundaryRecordCharge is the charge of a capture-boundary record noted by the lookup, its Boundary included.
	txBoundaryRecordCharge = (int64(unsafe.Sizeof(txSeen{}))+txAlignMask)&^txAlignMask + txBoundaryCharge
	// txConflictCharge is the fixed charge of a listed Conflict;
	// each version adds txVersionCharge, and each pack_id of a version txPackCharge.
	txConflictCharge = (int64(unsafe.Sizeof(Conflict{})) + txAlignMask) &^ txAlignMask
	// txVersionCharge is the charge of the slice header of one version of a listed Conflict.
	txVersionCharge = int64(unsafe.Sizeof([]UUID(nil)))
)

// txCtxStep is the number of iterations of the loops of a transaction lookup between two checks of ctx.
const txCtxStep = 4096

// txBudget is the state budget of a transaction lookup: the bytes charged against TxOptions.MaxStateBytes.
type txBudget struct {
	max  int64
	used int64
}

// reserve charges n bytes, not negative, before the state grows by them.
//
// Returns:
//   - error: an error wrapping ErrReadLimit, charging nothing, when the charge would pass max.
func (b *txBudget) reserve(n int64) error {
	if n > b.max-b.used {
		return fmt.Errorf("the lookup's state of %d bytes and %d more exceeds MaxStateBytes %d: %w", b.used, n, b.max, ErrReadLimit)
	}
	b.used += n

	return nil
}

// release returns n bytes that reserve charged, once the state they charged is dropped.
func (b *txBudget) release(n int64) {
	b.used -= n
}

// versionCost returns the charge of a kept version of rec whose header extension is ext.
func versionCost(rec *Record, ext []byte) int64 {
	return txRecordCharge + int64(len(rec.Payload)) + int64(len(ext))
}

// coverageCost returns the charge of a copy of c.
func coverageCost(c *Coverage) int64 {
	n := txCoverageCharge
	for i := range c.Unknown {
		n += rawEntryCost(&c.Unknown[i])
	}

	return n
}

// rawEntryCost returns the charge of a copy of e, an Unknown entry of a Coverage.
func rawEntryCost(e *RawEntry) int64 {
	return txRawEntryCharge + int64(len(e.Value))
}

// gapCost returns the charge of g: its fixed charge, one hour for each of its own Hours,
// and the copies of its Coverage and Barrier.
// A gap of a conflict across scope reads is charged before its Hours are set, since they are the conflict's.
func gapCost(g *TxGap) int64 {
	n := gapOwnCost(g)
	if g.Coverage != nil {
		n += coverageCost(g.Coverage)
	}

	return n
}

// gapOwnCost returns the charge of g but the copy of its Coverage.
func gapOwnCost(g *TxGap) int64 {
	n := txGapCharge + int64(len(g.Hours))*txHourCharge
	if g.Barrier != nil {
		n += txBoundaryCharge
	}

	return n
}

// conflictCost returns the charge of the listed conflict c.
func conflictCost(c *Conflict) int64 {
	n := txConflictCharge
	for _, v := range c.Versions {
		n += conflictVersionCost(v)
	}

	return n
}

// conflictVersionCost returns the charge of v, a version of a listed conflict: the pack_ids that hold it.
func conflictVersionCost(v []UUID) int64 {
	return txVersionCharge + int64(len(v))*txPackCharge
}

// coverageCharge returns coverageCost(c), each Unknown entry of c counting toward the checks of ctx (tick).
//
// Returns:
//   - error: ctx's error, as is.
func (l *txLookup) coverageCharge(ctx context.Context, c *Coverage) (int64, error) {
	n := txCoverageCharge
	for i := range c.Unknown {
		if err := l.tick(ctx); err != nil {
			return 0, err
		}
		n += rawEntryCost(&c.Unknown[i])
	}

	return n, nil
}

// gapCharge returns gapCost(g), each Unknown entry of its Coverage counting toward the checks of ctx (coverageCharge).
//
// Returns:
//   - error: ctx's error, as is.
func (l *txLookup) gapCharge(ctx context.Context, g *TxGap) (int64, error) {
	n := gapOwnCost(g)
	if g.Coverage != nil {
		m, err := l.coverageCharge(ctx, g.Coverage)
		if err != nil {
			return 0, err
		}
		n += m
	}

	return n, nil
}

// conflictCharge returns conflictCost(c), each version of c counting toward the checks of ctx (tick).
//
// Returns:
//   - error: ctx's error, as is.
func (l *txLookup) conflictCharge(ctx context.Context, c *Conflict) (int64, error) {
	n := txConflictCharge
	for _, v := range c.Versions {
		if err := l.tick(ctx); err != nil {
			return 0, err
		}
		n += conflictVersionCost(v)
	}

	return n, nil
}

// scopeCost returns the charge of a scope read whose view holds packs packs.
func scopeCost(packs int) int64 {
	return txScopeCharge + int64(packs)*txPackCharge
}

// tick counts one iteration of a loop of the lookup and checks ctx at every txCtxStep iterations, over all its loops.
//
// Returns:
//   - error: ctx's error, as is, at a check.
func (l *txLookup) tick(ctx context.Context) error {
	if !l.ticks.add(1) {
		return nil
	}
	if l.onTick != nil {
		l.onTick()
	}

	return ctx.Err()
}
