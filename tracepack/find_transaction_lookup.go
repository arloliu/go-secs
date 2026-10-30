package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// txLookup is the state of one transaction lookup over one source of hours.
type txLookup struct {
	key  TxKey
	opts TxOptions
	// hours gives the packs of each hour and their read: the source's as they are (liveHours), or a replay of reads made once.
	hours txHourSource
	res   TxResult
	// conflicts counts the conflicts reserved over the whole lookup against opts.MaxConflicts.
	conflicts int
	// primary holds the versions of the primary's seq that the read of hour key.Hour yielded, cloned as they arrived,
	// and primaryFlags the record_flags of each, as stored, which the version's charge covers.
	primary      []TxRecord
	primaryFlags []uint8
	// flags receives the record_flags, as stored, of the item a read passes to its callback,
	// and header its record's HSMS header when the read's source holds it decoded.
	flags  uint8
	header txCachedHeader
	// prim is what the records above the primary are classified against, taken from primary's first version.
	prim txPrimaryKey
	// early counts the records above the primary's seq that the read of hour key.Hour yielded before any version of the primary,
	// which only a block disagreeing with its F-2 entry brings about; they are not classified.
	early int
	// coverage holds the coverage entries of the packs read, each evaluated against the lookup's query once the reads are done.
	coverage []txCoverage
	// crash holds the stop-unclean boundaries the reads reported, each boundary value once, in the order found,
	// each evaluated against the lookup's time range once the reads are done;
	// crashSeen holds the key of each.
	crash     []txCrashGap
	crashSeen map[crashGapKey]struct{}
	// reads holds each hour's read, in read order, from the charge of its hour on.
	reads []*txScopeRead
	// cross maps each seq in conflict across hour reads to the hours that yielded it.
	cross map[uint64]*txCrossConflict
	// conflicted holds each seq in conflict within an hour's read, above the primary or in a later hour.
	conflicted map[uint64]struct{}
	// uncertain maps each epoch of a record of the primary's capture with ordering-uncertain that a read yielded, at any seq,
	// to where the first such record was seen.
	uncertain map[uint32]txSeen
	// boundaries holds every version of a capture-boundary record of the primary's epoch above the primary that a read yielded.
	boundaries []txBoundaryRecord
	// state charges the lookup's state against opts.MaxStateBytes.
	state txBudget
	// ticks counts the iterations of the lookup's loops toward the checks of ctx.
	ticks ctxCounter
	// onTick, set only by tests, runs at each check of ctx that tick makes, before it.
	onTick func()
	// skipSeqGap, breachFromPrimary and hourCoverageOnly, set only by tests,
	// each take a rule out of the lookup, so that a test shows the checks that catch its absence.
	// skipSeqGap leaves a bounded window's seq gap unlisted.
	// breachFromPrimary checks only the versions at or above the primary's seq for a scope breach.
	// hourCoverageOnly evaluates only the coverage entries each hour's read reports (mapOtherCoverage).
	skipSeqGap        bool
	breachFromPrimary bool
	hourCoverageOnly  bool
}

// txScopeRead is one hour a transaction lookup reads: the packs the source gave for it and the status of its read.
// Once the read's status is added to the result, its readers, its Result and its group buffer are dropped.
type txScopeRead struct {
	hour int64
	// start and end bound the hour's ts_utc_ns, [start, end).
	start, end int64
	readers    []*Reader
	// packs holds the pack_id of each reader, in the order the source gave them.
	packs []UUID
	res   Result
	// defect reports that the read has a defect other than a coverage entry or a crash gap,
	// and breach that it yielded a version outside the hour.
	defect, breach bool
	// runs is the read's run set, of the primary's capture.
	runs *txRuns
	// group is the seq group the read is yielding.
	group txGroup
	// kept holds the versions the read kept, and gaps the conflicts it found, until they are added to the result.
	kept []TxRecord
	gaps []txPendingGap
}

// txCoverage is a coverage entry of a pack a transaction lookup read.
type txCoverage struct {
	hour int64
	pack UUID
	// capture is the capture the entry belongs to: its capture_id, or else the capture of its pack.
	capture UUID
	// cov is the entry, a copy the read made for the lookup.
	cov *Coverage
}

// txCrashGap is a stop-unclean boundary an hour's read reported (IncompleteReasonCrashGap), which the lookup evaluates itself.
type txCrashGap struct {
	// hour is the hour of the first read that reported the boundary, and pack the pack_id of its representative in that read.
	hour int64
	pack UUID
	// b is the boundary, the read's own copy.
	b *Boundary
}

// newTxLookup returns the state of a lookup of key with opts, defaulted, over the hours of hours.
func newTxLookup(key TxKey, opts TxOptions, hours txHourSource) *txLookup {
	return &txLookup{
		key: key, opts: opts, hours: hours,
		state: txBudget{max: opts.MaxStateBytes}, ticks: ctxCounter{step: txCtxStep},
	}
}

// coverageMeets reports whether the coverage entry c meets the query of the seqs [first, last]
// and the time range [from, to), by the tracepack format specification §5:
// its seq range and its time interval, both inclusive, an absent bound unbounded, meet the query's;
// an entry whose first bound exceeds its last, on either side, meets every query of its capture.
// The caller checks that the entry belongs to the capture queried.
func coverageMeets(c *Coverage, first, last uint64, from, to int64) bool {
	if inverted(c.SeqFirst, c.SeqLast) || inverted(c.TimeStart, c.TimeEnd) {
		return true
	}
	seqMeets := (c.SeqFirst == nil || *c.SeqFirst <= last) && (c.SeqLast == nil || *c.SeqLast >= first)

	return seqMeets && overlaps(c.TimeStart, c.TimeEnd, &from, &to)
}

// run performs the lookup: it reads hour key.Hour and settles the primary;
// with a key, it reads the later hours and evaluates the window;
// without one, it ends after that hour, with the crash gaps that meet it (an early return).
//
// Returns:
//   - error: an error of the source or of a read, wrapped; an error wrapping ErrNotPrimary;
//     ctx's error, wrapped.
func (l *txLookup) run(ctx context.Context) error {
	rd, err := l.readScope(ctx, l.key.Hour)
	if err != nil {
		return err
	}
	keyed, err := l.settlePrimary(ctx, rd)
	if err != nil {
		return err
	}
	if !keyed {
		// A primary without a key ends the lookup after its hour, not-found and incomplete
		// (the tracepack semantics specification §7.2):
		// what the read classified above it was classified against no key, and is dropped, its charge with the lookup,
		// and only the crash gaps that meet the primary's hour are reported.
		if err := l.crashGapGaps(ctx, l.key.Hour*hourNs, (l.key.Hour+1)*hourNs); err != nil {
			return fmt.Errorf("tracepack: find transaction: %w", err)
		}
		l.res.Outcome = TxNotFound

		return nil
	}
	if err := l.commitRead(ctx, rd); err != nil {
		return fmt.Errorf("tracepack: find transaction: %w", err)
	}

	return l.searchWindow(ctx)
}

// readScope reads hour with one MergeIterate in capture order, a zero filter and payloads,
// over the readers the source gives for it, passing each item to collect with the read;
// it then ends the read's last seq group and adds the read's status to the result (mapScope), also after the read failed.
// The conflicts of the read are reserved against the lookup's MaxConflicts,
// and the hour, with a pack_id for each reader, is charged against MaxStateBytes before its pack_ids are taken;
// each reader counts toward the checks of ctx (tick).
//
// Returns:
//   - *txScopeRead: the read; nil, its charge released, when the source could not answer the hour,
//     the hour's charge failed, a reader is nil, or ctx is done while the readers' pack_ids are taken.
//   - error: ctx's error, wrapped; the source's error, wrapped with the hour;
//     the read's error, else the error of its mapping, wrapped with the hour;
//     an error wrapping ErrReadLimit when a charge passes MaxStateBytes.
func (l *txLookup) readScope(ctx context.Context, hour int64) (*txScopeRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: %w", err)
	}
	readers, err := l.hours.readers(ctx, hour)
	if err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: hour %d: readers: %w", hour, err)
	}

	// The hour is charged before its pack_ids are allocated; a failure after the charge releases it.
	cost := scopeCost(len(readers))
	if err := l.state.reserve(cost); err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: hour %d: %w", hour, err)
	}
	start, end := txHourBounds(hour)
	rd := &txScopeRead{hour: hour, start: start, end: end, readers: readers, runs: &txRuns{hour: hour}}
	if len(readers) > 0 {
		rd.packs = make([]UUID, len(readers))
		for i, r := range readers {
			if err := l.tick(ctx); err != nil {
				l.state.release(cost)

				return nil, fmt.Errorf("tracepack: find transaction: hour %d: %w", hour, err)
			}
			if r == nil {
				l.state.release(cost)

				return nil, fmt.Errorf("tracepack: find transaction: hour %d: readers: reader %d is nil", hour, i)
			}
			rd.packs[i] = UUID(r.hdr.PackID)
		}
	}
	l.reads = append(l.reads, rd)
	rd.res, err = l.hours.read(ctx, hour, readers, l.opts.MaxHeldBytes, l.reserveConflict, &l.flags, &l.header,
		func(it *Item) error { return l.collect(ctx, rd, it) })
	gerr := l.endGroup(ctx, rd)
	if err == nil {
		err = gerr
	}
	// The read's own error comes first; the mapping stops at its first error, so that the state stays within its budget.
	if merr := l.mapScope(ctx, rd, err == nil); err == nil {
		err = merr
	}
	// What the lookup needs of the read is in the result and in rd's own state now,
	// so the read's Result and group buffer do not live until the lookup ends;
	// a group whose end was cancelled keeps the versions it still holds, charged.
	rd.readers, rd.res = nil, Result{}
	if gerr == nil {
		rd.group = txGroup{}
	}
	if err != nil {
		return rd, fmt.Errorf("tracepack: find transaction: hour %d: %w", hour, err)
	}

	return rd, nil
}

// captureOf returns the capture_id of the pack of item it, which the read rd yielded.
func captureOf(rd *txScopeRead, it *Item) UUID {
	return UUID(rd.readers[it.Pack].hdr.CaptureID)
}

// reserveConflict reserves one conflict of the lookup against opts.MaxConflicts.
//
// Returns:
//   - error: an error wrapping ErrReadLimit, reserving nothing, when MaxConflicts conflicts are reserved already.
func (l *txLookup) reserveConflict() error {
	if l.conflicts >= l.opts.MaxConflicts {
		return fmt.Errorf("the lookup's conflicts exceed MaxConflicts %d: %w", l.opts.MaxConflicts, ErrReadLimit)
	}
	l.conflicts++

	return nil
}

// mapScope adds the status of the read rd to the result, in this order:
// a TxGapRead gap for each defect of the read, or TxGapIndex for an IncompleteReasonIndexMismatch one,
// the coverage defects kept as coverage entries to evaluate and the crash-gap defects as crash gaps to evaluate,
// then the coverage entries of the primary's capture the read did not report (mapOtherCoverage),
// the footer errors, a TxGapUnevaluated gap for each pack whose quality_evaluated is false,
// the read's conflicts, and, when the read completed, the hour as searched.
// Each item is charged against MaxStateBytes before it is added, the hour excepted, which readScope charged.
//
// Returns:
//   - error: the first charge that passes MaxStateBytes, wrapping ErrReadLimit, or ctx's error, as is;
//     the items before it are added, none after it.
func (l *txLookup) mapScope(ctx context.Context, rd *txScopeRead, completed bool) error {
	if err := l.mapDefects(ctx, rd); err != nil {
		return err
	}
	for _, fe := range rd.res.FooterErrs {
		if err := l.grow(ctx, txPackErrorCharge); err != nil {
			return err
		}
		l.res.FooterErrs = append(l.res.FooterErrs, TxPackError{Hour: rd.hour, Pack: rd.packs[fe.Pack], Err: fe.Err})
	}
	for i, r := range rd.readers {
		// Every pack counts toward the checks of ctx, an evaluated one too.
		if err := l.tick(ctx); err != nil {
			return err
		}
		if r.meta.QualityEvaluated {
			continue
		}
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapUnevaluated, Hours: []int64{rd.hour}, Pack: new(rd.packs[i]), Block: -1, Offset: -1,
			Err: errors.New("the pack's quality was not evaluated"),
		}); err != nil {
			return err
		}
	}
	for i := range rd.res.Conflicts {
		n, err := l.conflictCharge(ctx, &rd.res.Conflicts[i])
		if err != nil {
			return err
		}
		if err := l.grow(ctx, n); err != nil {
			return err
		}
		l.res.Conflicts = append(l.res.Conflicts, cloneConflict(&rd.res.Conflicts[i]))
	}
	if completed {
		l.res.Searched = append(l.res.Searched, TxScope{Hour: rd.hour, Packs: rd.packs})
	}

	return nil
}

// mapDefects adds the defects of the read rd: a TxGapRead or TxGapIndex gap for each,
// or, for a coverage defect, a coverage entry to evaluate, and, for a crash-gap defect, a crash gap to evaluate (noteCrashGap),
// each charged before it is added;
// then the coverage entries of the primary's capture that the read did not report (mapOtherCoverage).
// Neither a coverage entry nor a crash gap marks the read as having a defect:
// the lookup evaluates each once, against its own bounds (the tracepack semantics specification §7.2).
//
// Returns:
//   - error: the error of a charge, wrapping ErrReadLimit, or ctx's error, as is.
func (l *txLookup) mapDefects(ctx context.Context, rd *txScopeRead) error {
	for i := range rd.res.Incomplete {
		d := &rd.res.Incomplete[i]
		if d.Reason == IncompleteReasonCrashGap {
			if err := l.noteCrashGap(ctx, rd, d); err != nil {
				return err
			}

			continue
		}
		if d.Reason == IncompleteReasonCoverage {
			n, err := l.coverageCharge(ctx, d.Coverage)
			if err != nil {
				return err
			}
			if err := l.grow(ctx, txPendingCoverageCharge+n); err != nil {
				return err
			}
			capture := UUID(rd.readers[d.Pack].hdr.CaptureID)
			if d.Coverage.CaptureID != nil {
				capture = *d.Coverage.CaptureID
			}
			l.coverage = append(l.coverage, txCoverage{hour: rd.hour, pack: rd.packs[d.Pack], capture: capture, cov: cloneCoverage(d.Coverage)})

			continue
		}
		rd.defect = true
		reason := TxGapRead
		if d.Reason == IncompleteReasonIndexMismatch {
			reason = TxGapIndex
		}
		if err := l.addGap(ctx, TxGap{
			Reason: reason, Hours: []int64{rd.hour}, Pack: new(rd.packs[d.Pack]), Block: d.Block, Offset: d.Offset,
			Defect: d.Reason, Err: d.Err,
		}); err != nil {
			return err
		}
	}

	return l.mapOtherCoverage(ctx, rd)
}

// mapOtherCoverage keeps, as coverage entries to evaluate, the coverage entries naming the primary's capture
// that the packs of the read rd hold when none of its packs is of that capture, each a copy, charged before it is kept.
// The read reports only the entries of the captures it reads (appendCoverageDefects),
// so it reported none of them,
// while the lookup evaluates every entry of the primary's capture in every pack it reads
// (the tracepack semantics specification §7.2).
// An entry without capture_id belongs to its pack's capture, so it is never one of them.
//
// Returns:
//   - error: the error of a charge, wrapping ErrReadLimit, or ctx's error, as is.
func (l *txLookup) mapOtherCoverage(ctx context.Context, rd *txScopeRead) error {
	if l.hourCoverageOnly {
		return nil
	}
	for _, r := range rd.readers {
		if UUID(r.hdr.CaptureID) == l.key.CaptureID {
			return nil
		}
	}
	for i, r := range rd.readers {
		for k := range r.meta.Coverage {
			if err := l.tick(ctx); err != nil {
				return err
			}
			c := &r.meta.Coverage[k]
			if c.CaptureID == nil || *c.CaptureID != l.key.CaptureID {
				continue
			}
			n, err := l.coverageCharge(ctx, c)
			if err != nil {
				return err
			}
			if err := l.grow(ctx, txPendingCoverageCharge+n); err != nil {
				return err
			}
			l.coverage = append(l.coverage, txCoverage{hour: rd.hour, pack: rd.packs[i], capture: l.key.CaptureID, cov: cloneCoverage(c)})
		}
	}

	return nil
}

// noteCrashGap keeps the boundary of d, a crash-gap defect of the read rd, as a crash gap to evaluate,
// charged before it is kept, unless an earlier read reported the same boundary value:
// a boundary is evaluated once, and boundaries whose gap bounds differ are each kept
// (the tracepack semantics specification §7.4, Crash gaps).
//
// Returns:
//   - error: ctx's error, as is; the charge's error, wrapping ErrReadLimit.
func (l *txLookup) noteCrashGap(ctx context.Context, rd *txScopeRead, d *Defect) error {
	k := keyOfCrashGap(d.Boundary)
	if _, ok := l.crashSeen[k]; ok {
		return l.tick(ctx)
	}
	if err := l.grow(ctx, txCrashGapCharge); err != nil {
		return err
	}
	if l.crashSeen == nil {
		l.crashSeen = make(map[crashGapKey]struct{})
	}
	l.crashSeen[k] = struct{}{}
	b := cloneBoundary(*d.Boundary)
	l.crash = append(l.crash, txCrashGap{hour: rd.hour, pack: rd.packs[d.Pack], b: &b})

	return nil
}

// grow counts one iteration of a loop that grows the state, checking ctx (tick), and charges n bytes.
//
// Returns:
//   - error: ctx's error, as is; the charge's error, wrapping ErrReadLimit.
func (l *txLookup) grow(ctx context.Context, n int64) error {
	if err := l.tick(ctx); err != nil {
		return err
	}

	return l.state.reserve(n)
}

// addGap charges g, then adds it to the result's gaps.
//
// Returns:
//   - error: ctx's error, as is; the charge's error, wrapping ErrReadLimit.
func (l *txLookup) addGap(ctx context.Context, g TxGap) error {
	n, err := l.gapCharge(ctx, &g)
	if err != nil {
		return err
	}
	if err := l.grow(ctx, n); err != nil {
		return err
	}
	l.res.Incomplete = append(l.res.Incomplete, g)

	return nil
}

// cloneVersion charges the version of item it, which the read rd yielded, against MaxStateBytes (versionCost),
// then returns it with class, its payload and header extension cloned (keepVersion).
//
// Returns:
//   - error: the charge's error, wrapping ErrReadLimit, nothing cloned.
func (l *txLookup) cloneVersion(rd *txScopeRead, it *Item, class TxClass) (TxRecord, error) {
	if err := l.state.reserve(versionCost(&it.Record, it.HeaderExtra)); err != nil {
		return TxRecord{}, err
	}

	return keepVersion(rd, it, class), nil
}

// keepVersion returns the version of item it, which the read rd yielded, with class,
// its payload and header extension cloned.
func keepVersion(rd *txScopeRead, it *Item, class TxClass) TxRecord {
	rec := it.Record
	rec.Payload = bytes.Clone(rec.Payload)

	return TxRecord{
		Record: rec, HeaderExtra: bytes.Clone(it.HeaderExtra), Hour: rd.hour, Pack: rd.packs[it.Pack], Block: it.Block,
		Conflict: it.Conflict, Class: class,
	}
}

// settlePrimary settles the primary once hour key.Hour is read (the tracepack semantics specification §7.2).
//
// With no version, the primary is missing: a TxGapNoKey gap when the read has a defect other than a coverage entry or a crash gap,
// yielded a version outside the hour, holds a coverage entry of the primary's capture that meets the primary's seq and hour,
// or reported a crash gap of the primary's capture that meets that hour; ErrNotPrimary otherwise.
// The gap carries a copy of the first coverage entry that meets the primary's seq and hour, if any.
// A version of the primary after a record above it, in the same read, is a TxGapIndex gap: the order was broken.
// Copies of the primary that the read yielded uncompared, from clusters that a block disagreeing with its F-2 entry split,
// are compared here, byte for byte, as stored: identical copies are one version, kept once.
// Several versions, or a version the read marked as a conflict, are a conflict at the primary:
// a TxGapConflict and a TxGapNoKey gap, every version kept and marked as a conflict;
// when the read listed no conflict at the primary, because the versions arrived uncompared, the conflict is reserved here.
// One version that is not a data record with an available odd function is ErrNotPrimary.
// Otherwise the key is derived from it, and it is kept.
// It has no key, a TxGapNoKey gap, unless its direction is host-to-equipment or equipment-to-host
// and its SessionID and System Bytes are available;
// a primary with correlation-incomplete adds a TxGapCorrelation gap.
//
// Returns:
//   - bool: whether the primary has a key, so that its window is to be searched.
//   - error: an error wrapping ErrNotPrimary;
//     the error of the reservation of a conflict the read did not list, wrapping ErrReadLimit;
//     an error wrapping ErrReadLimit when a gap's charge passes MaxStateBytes; ctx's error; each wrapped with the hour.
func (l *txLookup) settlePrimary(ctx context.Context, rd *txScopeRead) (bool, error) {
	keyed, err := l.settle(ctx, rd)
	if err != nil && !errors.Is(err, ErrNotPrimary) {
		err = fmt.Errorf("tracepack: find transaction: hour %d: %w", l.key.Hour, err)
	}

	return keyed, err
}

// settle is settlePrimary, its errors other than ErrNotPrimary not wrapped.
func (l *txLookup) settle(ctx context.Context, rd *txScopeRead) (bool, error) {
	p := l.key.Seq
	if len(l.primary) == 0 {
		at, err := l.coverageAtPrimary(ctx)
		if err != nil {
			return false, err
		}
		gap, err := l.crashGapAtPrimary(ctx)
		if err != nil {
			return false, err
		}
		if at < 0 && !gap && !rd.defect && !rd.breach {
			return false, fmt.Errorf("%w: hour %d holds no seq %d of capture %s", ErrNotPrimary, l.key.Hour, p, l.key.CaptureID)
		}
		g := l.keyGap(TxGapNoKey, "the primary is missing where a gap explains it")
		if at >= 0 {
			// Charged as a copy, then copied.
			g.Coverage = l.coverage[at].cov
		}
		if err := l.addGap(ctx, g); err != nil {
			return false, err
		}
		if at >= 0 {
			l.res.Incomplete[len(l.res.Incomplete)-1].Coverage = cloneCoverage(g.Coverage)
		}

		return false, nil
	}
	if l.early > 0 {
		if err := l.addGap(ctx, l.keyGap(TxGapIndex, fmt.Sprintf("%d records above the primary arrived before it", l.early))); err != nil {
			return false, err
		}
	}

	// A conflict the read listed stands whatever the comparison below finds.
	listed, err := l.distinctVersions(ctx)
	if err != nil {
		return false, err
	}
	if len(l.primary) > 1 || listed {
		return false, l.settleConflict(ctx, listed)
	}
	first := &l.primary[0].Record
	h := first.HSMSHeader()
	if first.Kind != KindData || h.Available&FieldValidityFunction == 0 || h.Function%2 == 0 {
		return false, fmt.Errorf("%w: seq %d of capture %s in hour %d is a %v record with function %d, available %t",
			ErrNotPrimary, p, l.key.CaptureID, l.key.Hour, first.Kind, h.Function, h.Available&FieldValidityFunction != 0)
	}
	l.deriveKey(first)
	l.res.Records = append(l.res.Records, l.primary[0])
	if first.Quality&QualityCorrelationIncomplete != 0 {
		if err := l.addGap(ctx, l.keyGap(TxGapCorrelation, "the primary carries correlation-incomplete")); err != nil {
			return false, err
		}
	}
	if why := keyMissing(first.Dir, h.Available); why != "" {
		return false, l.addGap(ctx, l.keyGap(TxGapNoKey, why))
	}

	return true, nil
}

// settleConflict settles a primary whose versions conflict:
// the key derived from the first version, every version kept and marked as a conflict,
// a TxGapConflict and a TxGapNoKey gap;
// the conflict is reserved here unless the read listed it, which listed reports.
//
// Returns:
//   - error: the reservation's error, wrapping ErrReadLimit; the error of a gap's charge, wrapping ErrReadLimit; ctx's error.
func (l *txLookup) settleConflict(ctx context.Context, listed bool) error {
	if !listed {
		if err := l.reserveConflict(); err != nil {
			return fmt.Errorf("the conflict at seq %d: %w", l.key.Seq, err)
		}
	}
	l.deriveKey(&l.primary[0].Record)
	for i := range l.primary {
		if err := l.tick(ctx); err != nil {
			return err
		}
		l.primary[i].Conflict = true
	}
	l.res.Records = append(l.res.Records, l.primary...)
	if err := l.addGap(ctx, l.keyGap(TxGapConflict, fmt.Sprintf("the primary has %d versions", len(l.primary)))); err != nil {
		return err
	}

	return l.addGap(ctx, l.keyGap(TxGapNoKey, "the primary conflicts"))
}

// keyMissing returns why a primary of direction dir whose available HSMS header fields are avail has no association key,
// or "" when it has one.
func keyMissing(dir Dir, avail FieldValidity) string {
	switch {
	case dir != DirHostToEquipment && dir != DirEquipmentToHost:
		return fmt.Sprintf("the primary's direction is %v", dir)
	case avail&FieldValiditySessionID == 0:
		return "the primary's SessionID is unavailable"
	case avail&FieldValiditySystemBytes == 0:
		return "the primary's System Bytes are unavailable"
	default:
		return ""
	}
}

// distinctVersions keeps, in l.primary and l.primaryFlags, the versions of the primary that differ from every earlier one, in order:
// two versions are one when their records are equal as stored (sameStoredRecord).
// The charge of each version left out is released once every version is compared.
// Each version and each comparison counts toward the checks of ctx (tick).
//
// Returns:
//   - bool: whether the read marked a version of the primary as a conflict (Item.Conflict).
//   - error: ctx's error, as is, nothing released and l.primary unchanged.
func (l *txLookup) distinctVersions(ctx context.Context) (bool, error) {
	vs, flags := l.primary, l.primaryFlags
	out, outFlags := vs[:0:0], flags[:0:0]
	var dropped int64
	listed := false
	for i := range vs {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		listed = listed || vs[i].Conflict
		same := false
		for j := range out {
			if err := l.tick(ctx); err != nil {
				return false, err
			}
			if sameStoredRecord(&out[j], &vs[i], outFlags[j], flags[i]) {
				same = true
				break
			}
		}
		if same {
			dropped += versionCost(&vs[i].Record, vs[i].HeaderExtra)
		} else {
			out, outFlags = append(out, vs[i]), append(outFlags, flags[i])
		}
	}
	l.state.release(dropped)
	l.primary, l.primaryFlags = out, outFlags

	return listed, nil
}

// sameStoredRecord reports whether a, whose record_flags as stored are fa, and b, whose are fb, hold the same record as stored:
// every byte of the record header and the payload.
// Record holds every record header field as stored but record_flags, of which it keeps only mono_present,
// and the payload's length is payload_len.
func sameStoredRecord(a, b *TxRecord, fa, fb uint8) bool {
	x, y := &a.Record, &b.Record

	return x.Seq == y.Seq && x.TSUTCNs == y.TSUTCNs && x.MonoNs == y.MonoNs && fa == fb &&
		x.Epoch == y.Epoch && x.Kind == y.Kind && x.Dir == y.Dir && x.Fidelity == y.Fidelity &&
		x.DecodeStatus == y.DecodeStatus && x.TrailingBytes == y.TrailingBytes && x.Quality == y.Quality &&
		x.FieldValidity == y.FieldValidity && bytes.Equal(x.Payload, y.Payload) && bytes.Equal(a.HeaderExtra, b.HeaderExtra)
}

// coverageAtPrimary finds the first coverage entry read in hour key.Hour, of the primary's capture,
// that meets the primary's seq and hour, which explains why the read yielded no version of the primary.
//
// Returns:
//   - int: the entry's index in the lookup's coverage entries; -1 when there is none.
//   - error: ctx's error, as is.
func (l *txLookup) coverageAtPrimary(ctx context.Context) (int, error) {
	from, to := l.key.Hour*hourNs, (l.key.Hour+1)*hourNs
	for i, c := range l.coverage {
		if err := l.tick(ctx); err != nil {
			return -1, err
		}
		if c.hour == l.key.Hour && c.capture == l.key.CaptureID && coverageMeets(c.cov, l.key.Seq, l.key.Seq, from, to) {
			return i, nil
		}
	}

	return -1, nil
}

// crashGapAtPrimary reports whether a crash gap of the primary's capture that the read of hour key.Hour reported
// meets that hour, which explains why the read yielded no version of the primary, as a read defect does.
//
// Returns:
//   - bool: whether there is one.
//   - error: ctx's error, as is.
func (l *txLookup) crashGapAtPrimary(ctx context.Context) (bool, error) {
	from, to := l.key.Hour*hourNs, (l.key.Hour+1)*hourNs
	for i := range l.crash {
		if err := l.tick(ctx); err != nil {
			return false, err
		}
		c := &l.crash[i]
		if c.hour == l.key.Hour && c.b.CaptureID == l.key.CaptureID && crashGapMeets(c.b, &from, &to) {
			return true, nil
		}
	}

	return false, nil
}

// deriveKey sets the result's association key and match fields from the primary's version rec,
// each HSMS header field zero when unavailable.
func (l *txLookup) deriveKey(rec *Record) {
	h := rec.HSMSHeader()
	l.res.Epoch, l.res.Dir = rec.Epoch, rec.Dir
	if h.Available&FieldValiditySessionID != 0 {
		l.res.SessionID = h.SessionID
	}
	if h.Available&FieldValiditySystemBytes != 0 {
		l.res.SystemBytes = h.SystemBytes
	}
	if h.Available&FieldValidityFunction != 0 {
		l.res.Function = h.Function
	}
	if h.Available&FieldValidityStreamAndW != 0 {
		l.res.Stream, l.res.W = h.Stream, h.W
		l.res.StreamAvailable, l.res.WAvailable = true, true
	}
}

// keyGap returns a gap of reason at the primary's seq in hour key.Hour, described by why.
func (l *txLookup) keyGap(reason TxGapReason, why string) TxGap {
	return TxGap{
		Reason: reason, Hours: []int64{l.key.Hour}, Block: -1, Offset: -1, Seq: new(l.key.Seq), Err: errors.New(why),
	}
}

// outside reports whether ts lies outside the hour rd reads, as hourOf(ts) != rd.hour does.
func (rd *txScopeRead) outside(ts int64) bool {
	return ts < rd.start || ts >= rd.end
}

// txHourBounds returns the ts_utc_ns range of hour, [start, end): a ts lies in it exactly when hourOf(ts) is hour.
// hour lies in [MinTxHour, MaxTxHour], as every hour a lookup reads does, so both bounds fit an int64.
func txHourBounds(hour int64) (start, end int64) {
	return hour * hourNs, (hour + 1) * hourNs
}
