package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
)

// txLookup is the state of one FindTransaction call over one Observation.
type txLookup struct {
	key  TxKey
	opts TxOptions
	obs  Observation
	res  TxResult
	// evidence is the capture's per-capture evidence, taken once from the observation.
	evidence CaptureEvidence
	// conflicts counts the conflicts reserved over the whole lookup against opts.MaxConflicts.
	conflicts int
	// primary holds the versions of the primary's seq that the read of scope key.Hour yielded, cloned as they arrived.
	primary []TxRecord
	// prim is what the records above the primary are classified against, taken from primary's first version.
	prim txPrimaryKey
	// early counts the records above the primary's seq that the read of scope key.Hour yielded before any version of the primary,
	// and earlyRuns holds their seqs, none of them classified or kept.
	early     int
	earlyRuns txRuns
	// coverage holds the coverage entries of the packs read, each evaluated against the lookup's query once the reads are done.
	coverage []txCoverage
	// reads holds each scope read, in read order, from the charge of its scope on.
	reads []*txScopeRead
	// cross maps each seq in conflict across scope reads to the hours that yielded it.
	cross map[uint64]*txCrossConflict
	// uncertain maps each epoch of a record with ordering-uncertain that a read yielded, at any seq,
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
}

// txScopeRead is one scope a transaction lookup reads: its fixed view and the status of its read.
// Once the read's status is added to the result, its readers, its Result and its group buffer are dropped.
type txScopeRead struct {
	hour    int64
	indexed bool
	readers []*Reader
	// packs holds the pack_id of each reader, in view order.
	packs []UUID
	res   Result
	// defect reports that the read has a defect other than a coverage entry.
	defect bool
	// runs is the read's run set.
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
	// cov is the entry, a copy the read made for the lookup.
	cov *Coverage
}

// newTxLookup returns the state of a lookup of key with opts, defaulted, over obs.
func newTxLookup(key TxKey, opts TxOptions, obs Observation) *txLookup {
	return &txLookup{
		key: key, opts: opts, obs: obs,
		state: txBudget{max: opts.MaxStateBytes}, ticks: ctxCounter{step: txCtxStep},
	}
}

// coverageMeets reports whether the coverage entry c of a pack of capture meets the query of the seqs [first, last]
// and the time range [from, to) of capture, by the tracepack format specification §5:
// its seq range and its time interval, both inclusive, an absent bound unbounded, meet the query's;
// an entry whose first bound exceeds its last, on either side, meets every query of its capture,
// and an entry without capture_id belongs to the pack's capture.
func coverageMeets(c *Coverage, capture UUID, first, last uint64, from, to int64) bool {
	if c.CaptureID != nil && *c.CaptureID != capture {
		return false
	}
	if inverted(c.SeqFirst, c.SeqLast) || inverted(c.TimeStart, c.TimeEnd) {
		return true
	}
	seqMeets := (c.SeqFirst == nil || *c.SeqFirst <= last) && (c.SeqLast == nil || *c.SeqLast >= first)

	return seqMeets && overlaps(c.TimeStart, c.TimeEnd, &from, &to)
}

// run performs the lookup: it takes the evidence, reads scope key.Hour and settles the primary.
//
// Returns:
//   - error: an error of the observation or of a read, wrapped; an error wrapping ErrNotPrimary;
//     ctx's error, wrapped.
func (l *txLookup) run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: find transaction: %w", err)
	}
	ev, err := l.obs.Evidence(ctx)
	if err != nil {
		return fmt.Errorf("tracepack: find transaction: evidence: %w", err)
	}
	l.evidence = ev

	rd, err := l.readScope(ctx, l.key.Hour, l.collect)
	if err != nil {
		return err
	}
	keyed, err := l.settlePrimary(ctx, rd)
	if err != nil {
		return err
	}
	if !keyed {
		// A primary without a key makes the outcome incomplete, and no further scope is read
		// (the tracepack semantics specification §7.2).
		// What the read classified above it was classified against no key, and is dropped, its charge with the lookup.
		l.res.Outcome = TxIncomplete

		return nil
	}
	if err := l.commitRead(ctx, rd); err != nil {
		return fmt.Errorf("tracepack: find transaction: %w", err)
	}

	return l.searchWindow(ctx)
}

// readScope reads the scope of hour with one MergeIterate in capture order, a zero filter and payloads,
// passing each item to fn with the read;
// it then ends the read's last seq group and adds the read's status to the result (mapScope), also after the read failed.
// The conflicts of the read are reserved against the lookup's MaxConflicts,
// and the scope, with a pack_id for each pack of its view, is charged against MaxStateBytes before it is read.
//
// Returns:
//   - *txScopeRead: the read; nil when the observation could not answer the scope or the scope's charge failed.
//   - error: the observation's error, wrapped with the hour; ctx's error, wrapped;
//     the read's error, else the error of its mapping, wrapped with the hour;
//     an error wrapping ErrReadLimit when a charge passes MaxStateBytes.
func (l *txLookup) readScope(ctx context.Context, hour int64,
	fn func(ctx context.Context, rd *txScopeRead, it *Item) error,
) (*txScopeRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: %w", err)
	}
	sc, err := l.obs.Scope(ctx, hour)
	if err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: hour %d: scope: %w", hour, err)
	}

	rd := &txScopeRead{hour: hour, indexed: sc.Indexed, readers: sc.Readers, runs: &txRuns{hour: hour}}
	if len(sc.Readers) > 0 {
		rd.packs = make([]UUID, len(sc.Readers))
		for i, r := range sc.Readers {
			if r == nil {
				return nil, fmt.Errorf("tracepack: find transaction: hour %d: scope: reader %d is nil", hour, i)
			}
			rd.packs[i] = UUID(r.hdr.PackID)
		}
	}
	if err := l.state.reserve(scopeCost(len(rd.packs))); err != nil {
		return nil, fmt.Errorf("tracepack: find transaction: hour %d: %w", hour, err)
	}
	l.reads = append(l.reads, rd)
	rd.res, err = mergeIterateWith(ctx, sc.Readers, Query{Payloads: true},
		MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: l.opts.MaxHeldBytes},
		mergeIterateOptions{reserveConflict: l.reserveConflict},
		func(it *Item) error { return fn(ctx, rd, it) })
	l.endGroup(rd)
	// The read's own error comes first; the mapping stops at its first error, so that the state stays within its budget.
	if merr := l.mapScope(ctx, rd, err == nil); err == nil {
		err = merr
	}
	// What the lookup needs of the read is in the result and in rd's own state now,
	// so the read's Result and group buffer do not live until the lookup ends.
	rd.readers, rd.res, rd.group = nil, Result{}, txGroup{}
	if err != nil {
		return rd, fmt.Errorf("tracepack: find transaction: hour %d: %w", hour, err)
	}

	return rd, nil
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
// a TxGapRead gap for each defect of the read, or TxGapIndex for a ReasonIndexMismatch one,
// the coverage defects kept as coverage entries to evaluate, the footer errors,
// a TxGapUnevaluated gap for each pack whose quality_evaluated is false, TxGapCold when the scope is not indexed,
// the read's conflicts, and, when the read completed, the scope as searched.
// Each item is charged against MaxStateBytes before it is added, the scope excepted, which readScope charged.
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
	if !rd.indexed {
		if err := l.addGap(ctx, TxGap{
			Reason: TxGapCold, Hours: []int64{rd.hour}, Block: -1, Offset: -1, Err: errors.New("the scope is not indexed"),
		}); err != nil {
			return err
		}
	}
	for i := range rd.res.Conflicts {
		if err := l.grow(ctx, conflictCost(&rd.res.Conflicts[i])); err != nil {
			return err
		}
		l.res.Conflicts = append(l.res.Conflicts, rd.res.Conflicts[i])
	}
	if completed {
		l.res.Searched = append(l.res.Searched, TxScope{Hour: rd.hour, Indexed: rd.indexed, Packs: rd.packs})
	}

	return nil
}

// mapDefects adds the defects of the read rd: a TxGapRead or TxGapIndex gap for each,
// or, for a coverage defect, a coverage entry to evaluate, each charged before it is added.
//
// Returns:
//   - error: the error of a charge, wrapping ErrReadLimit, or ctx's error, as is.
func (l *txLookup) mapDefects(ctx context.Context, rd *txScopeRead) error {
	for i := range rd.res.Incomplete {
		d := &rd.res.Incomplete[i]
		rd.defect = rd.defect || d.Reason != ReasonCoverage
		if d.Reason == ReasonCoverage {
			if err := l.grow(ctx, txPendingCoverageCharge+coverageCost(d.Coverage)); err != nil {
				return err
			}
			l.coverage = append(l.coverage, txCoverage{hour: rd.hour, pack: rd.packs[d.Pack], cov: d.Coverage})

			continue
		}
		reason := TxGapRead
		if d.Reason == ReasonIndexMismatch {
			reason = TxGapIndex
		}
		if err := l.addGap(ctx, TxGap{
			Reason: reason, Hours: []int64{rd.hour}, Pack: new(rd.packs[d.Pack]), Block: d.Block, Offset: d.Offset,
			Defect: d.Reason, Err: d.Err,
		}); err != nil {
			return err
		}
	}

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
	if err := l.grow(ctx, gapCost(&g)); err != nil {
		return err
	}
	l.res.Gaps = append(l.res.Gaps, g)

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

// settlePrimary settles the primary once scope key.Hour is read (the tracepack semantics specification §7.2).
//
// With no version, the primary is missing: a TxGapNoKey gap when the read has a defect other than a coverage entry,
// a coverage entry meets the primary's seq and hour, or the scope is not indexed; ErrNotPrimary otherwise.
// The gap carries a copy of the first coverage entry that meets the primary's seq and hour, if any.
// A version of the primary after a record above it, in the same read, is a TxGapIndex gap: the order was broken.
// Copies of the primary that the read yielded uncompared, from clusters that a block disagreeing with its F-2 entry split,
// are compared here, byte for byte: identical copies are one version, kept once.
// Several versions are a conflict at the primary: a TxGapConflict and a TxGapNoKey gap,
// every version kept and marked as a conflict;
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
		if at < 0 && !explainsMissing(rd) {
			return false, fmt.Errorf("%w: hour %d holds no seq %d of capture %s", ErrNotPrimary, l.key.Hour, p, l.key.Capture)
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
			l.res.Gaps[len(l.res.Gaps)-1].Coverage = cloneCoverage(g.Coverage)
		}

		return false, nil
	}
	if l.early > 0 {
		if err := l.addGap(ctx, l.keyGap(TxGapIndex, fmt.Sprintf("%d records above the primary arrived before it", l.early))); err != nil {
			return false, err
		}
	}

	listed := slices.ContainsFunc(l.primary, func(v TxRecord) bool { return v.Conflict })
	distinct, err := l.distinctVersions(ctx, l.primary)
	if err != nil {
		return false, err
	}
	l.primary = distinct
	first := &l.primary[0].Record
	if len(l.primary) > 1 {
		if !listed {
			if err := l.reserveConflict(); err != nil {
				return false, fmt.Errorf("the conflict at seq %d: %w", p, err)
			}
		}
		l.deriveKey(first)
		for i := range l.primary {
			l.primary[i].Conflict = true
		}
		l.res.Records = append(l.res.Records, l.primary...)
		if err := l.addGap(ctx, l.keyGap(TxGapConflict, fmt.Sprintf("the primary has %d versions", len(l.primary)))); err != nil {
			return false, err
		}

		return false, l.addGap(ctx, l.keyGap(TxGapNoKey, "the primary conflicts"))
	}
	h := first.HSMSHeader()
	if first.Kind != KindData || h.Available&FieldValidityFunction == 0 || h.Function%2 == 0 {
		return false, fmt.Errorf("%w: seq %d of capture %s in hour %d is a %v record with function %d, available %t",
			ErrNotPrimary, p, l.key.Capture, l.key.Hour, first.Kind, h.Function, h.Available&FieldValidityFunction != 0)
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

// distinctVersions returns the versions of vs that differ from every earlier one, in order:
// two versions are one when their records, header fields as stored, payload and header extension, are equal.
// The charge of each version left out is released once every version is compared.
//
// Returns:
//   - error: ctx's error, as is, nothing released.
func (l *txLookup) distinctVersions(ctx context.Context, vs []TxRecord) ([]TxRecord, error) {
	out := vs[:0:0]
	var dropped int64
	for i := range vs {
		same := false
		for j := range out {
			if err := l.tick(ctx); err != nil {
				return nil, err
			}
			if sameStoredRecord(&out[j], &vs[i]) {
				same = true
				break
			}
		}
		if same {
			dropped += versionCost(&vs[i].Record, vs[i].HeaderExtra)
		} else {
			out = append(out, vs[i])
		}
	}
	l.state.release(dropped)

	return out, nil
}

// sameStoredRecord reports whether a and b hold the same record: every record header field, the payload and the header extension.
func sameStoredRecord(a, b *TxRecord) bool {
	x, y := &a.Record, &b.Record

	return x.Seq == y.Seq && x.TSUTCNs == y.TSUTCNs && x.MonoNs == y.MonoNs && x.MonoPresent == y.MonoPresent &&
		x.Epoch == y.Epoch && x.Kind == y.Kind && x.Dir == y.Dir && x.Fidelity == y.Fidelity &&
		x.DecodeStatus == y.DecodeStatus && x.TrailingBytes == y.TrailingBytes && x.Quality == y.Quality &&
		x.FieldValidity == y.FieldValidity && bytes.Equal(x.Payload, y.Payload) && bytes.Equal(a.HeaderExtra, b.HeaderExtra)
}

// explainsMissing reports whether the read rd has a defect other than a coverage entry, or reads a scope that is not indexed,
// either of which explains why it yielded no version of the primary.
func explainsMissing(rd *txScopeRead) bool {
	return !rd.indexed || rd.defect
}

// coverageAtPrimary finds the first coverage entry read in scope key.Hour that meets the primary's seq and hour,
// which explains why the read yielded no version of the primary.
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
		if c.hour == l.key.Hour && coverageMeets(c.cov, l.key.Capture, l.key.Seq, l.key.Seq, from, to) {
			return i, nil
		}
	}

	return -1, nil
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
