package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unsafe"
)

// DefaultTxMaxCachedBytes is the default TxRangeOptions.MaxCachedBytes: 256 MiB.
const DefaultTxMaxCachedBytes = 256 << 20

// TxRange selects the primaries FindTransactions pairs:
// the primaries of one capture whose ts_utc_ns lies in [From, To).
type TxRange struct {
	// Capture is the capture whose primaries are paired; not the nil UUID.
	Capture UUID
	// From and To bound the primaries' ts_utc_ns, From included and To excluded;
	// From is before To, and both are representable as int64 nanoseconds.
	From, To time.Time
}

// TxRangeOptions configures FindTransactions.
type TxRangeOptions struct {
	// Lookup applies to the lookup of each primary as it applies to one FindTransaction:
	// MaxScopes, and MaxHeldBytes, MaxConflicts and MaxStateBytes, each per lookup.
	// MaxHeldBytes also bounds the read of each hour into the cache, as it bounds a lookup's read of that hour.
	Lookup TxOptions
	// MaxCachedBytes bounds the hours FindTransactions holds at once, the records and diagnostics of each hour read,
	// charged nominally, as TxOptions.MaxStateBytes charges a lookup's state;
	// zero or negative means DefaultTxMaxCachedBytes.
	MaxCachedBytes int64
}

// TxPairing is one primary FindTransactions pairs, with the result of its lookup.
//
// Every mutable part is the caller's own copy, valid after fn returns,
// sharing no storage with FindTransactions or with another pairing;
// only error values (TxGap.Err, TxPackError.Err) and their chains may be shared, as immutable values.
type TxPairing struct {
	// Key names the primary.
	Key TxKey
	// Result is the result FindTransaction returns for Key over the same observation (see FindTransactions).
	Result TxResult
}

// TxRangeResult is the status of a FindTransactions call.
type TxRangeResult struct {
	// HoursRead counts the hours whose read into the cache completed.
	HoursRead int
	// Primaries counts the pairings passed to fn, the one whose call returned an error included.
	Primaries int
}

// FindTransactions pairs every primary of one capture in a time range with its reply,
// reading each hour once (the tracepack semantics specification §7.2, Pairing a time range).
//
// A primary is a data record whose function is available and odd.
// For each UTC hour H from the hour of r.From to the hour of r.To − 1 ns,
// FindTransactions pairs every seq of r.Capture whose versions, as the read of hour H yields them,
// include a primary whose ts_utc_ns lies in hour H and in [r.From, r.To);
// its key is (r.Capture, seq, H).
// A version whose ts_utc_ns lies outside the hour it is read from is a scope breach and is not paired:
// the lookups whose hours hold it report the breach.
// Records of other captures are never paired, and take part in every lookup as they take part in FindTransaction's.
//
// FindTransactions calls src.Readers once for each hour it reads, in ascending order,
// from the hour of r.From to the last hour a primary's lookup schedules
// (the hour of r.To − 1 ns plus MaxScopes − 1);
// the readers it gets for an hour, in that order, are the hour's observation for the whole call,
// and they must stay valid until FindTransactions returns.
// The result of every primary it reports equals the TxResult that FindTransaction returns for its key
// over a PackSource that returns each hour's observation,
// with the same TxOptions, field by field and in every order;
// over a source whose Readers returns the same readers in the same order on every call, as NewReaderSource's does,
// that is FindTransaction over src itself.
//
// It passes the pairings to fn in ascending hour, then ascending seq;
// a primary of hour H is passed once hour H + MaxScopes − 1 is read,
// also when its lookup ended after hour H, as the lookup of a missing, conflicting or keyless primary does.
// A primary near the end of the hours read can report TxGapOpenWindow
// because a later hour is empty or not yet written: FindTransactions cannot tell a settled empty hour from one not yet settled.
// A caller that needs settled results keeps the hour of r.To − 1 ns at or before the last settled hour minus MaxScopes − 1,
// or calls again once later hours are settled.
//
// Each hour is held, read once, while a primary's lookup still needs it, so at most MaxScopes hours at once,
// bounded by TxRangeOptions.MaxCachedBytes.
//
// Parameters:
//   - ctx: cancels the call; checked before each hour, during each hour's read and enumeration,
//     before and inside each lookup, before each pairing is passed to fn, and before FindTransactions returns.
//   - src: the packs of each hour, as for FindTransaction.
//   - r: the capture and time range of the primaries.
//   - opts: see TxRangeOptions.
//   - fn: receives each pairing; an error it returns ends the call.
//
// Returns:
//   - TxRangeResult: the hours read and the pairings passed to fn, also beside an error.
//   - error: an error wrapping ErrInvalidQuery, before src is called;
//     the error of src or of an hour's read, wrapped with the hour;
//     an error wrapping ErrReadLimit from MaxCachedBytes or MaxHeldBytes, wrapped with the hour,
//     or from a lookup's MaxConflicts or MaxStateBytes, wrapped with its key;
//     ctx's error, wrapped;
//     or the error fn returned, as is.
//     After an error no further pairing is passed to fn.
func FindTransactions(ctx context.Context, src PackSource, r TxRange, opts TxRangeOptions, fn func(*TxPairing) error) (TxRangeResult, error) {
	return findTransactionsWith(ctx, src, r, opts, fn, nil)
}

// findTransactionsWith is FindTransactions; prep, set only by tests, runs on the batch before it reads anything.
func findTransactionsWith(ctx context.Context, src PackSource, r TxRange, opts TxRangeOptions, fn func(*TxPairing) error,
	prep func(b *txBatch),
) (TxRangeResult, error) {
	b, err := newTxBatch(src, r, opts, fn)
	if err != nil {
		return TxRangeResult{}, err
	}
	if prep != nil {
		prep(b)
	}
	err = b.run(ctx)

	return b.res, err
}

// Nominal charges of the hour records a batch holds, against TxRangeOptions.MaxCachedBytes.
const (
	txCacheHourCharge        = (int64(unsafe.Sizeof(txHourRecord{})) + txAlignMask) &^ txAlignMask
	txCacheReaderCharge      = int64(unsafe.Sizeof(uintptr(0)))
	txCacheItemCharge        = (int64(unsafe.Sizeof(Item{})+unsafe.Sizeof(uint8(0))) + txAlignMask) &^ txAlignMask
	txCacheReservationCharge = int64(unsafe.Sizeof(int(0)))
	txCacheKeyCharge         = int64(unsafe.Sizeof(uint64(0)))
	txCacheDefectCharge      = (int64(unsafe.Sizeof(Defect{})) + txAlignMask) &^ txAlignMask
	txCacheFooterErrCharge   = (int64(unsafe.Sizeof(PackError{})) + txAlignMask) &^ txAlignMask
	txCacheRedactionCharge   = (int64(unsafe.Sizeof(RecordRedaction{})) + txAlignMask) &^ txAlignMask
	txCacheEntryCharge       = (int64(unsafe.Sizeof(RedactionEntry{})) + txAlignMask) &^ txAlignMask
	txCacheRangeCharge       = int64(unsafe.Sizeof(MaskedRange{}))
	txCacheRedDefectCharge   = (int64(unsafe.Sizeof(RedactionDefect{})) + txAlignMask) &^ txAlignMask
)

// txHourRecord is one hour a batch read once: its observation, the read's items with their stored flags,
// where the read reserved each conflict, and the read's Result.
// It is never changed once filled: every lookup replaying it reads it only (txReplay).
type txHourRecord struct {
	hour    int64
	readers []*Reader
	// items are the read's items, in yield order, each the record's own copy; flags[i] is item i's record_flags as stored.
	items []Item
	flags []uint8
	// reservations holds, for the k-th conflict the read reserved, the number of items yielded before it;
	// the conflict is res.Conflicts[k], since a read lists every conflict it reserves, in order.
	reservations []int
	res          Result
	// keys holds the seqs of the hour's primaries the batch pairs (FindTransactions), ascending, each once.
	keys []uint64
	// charge is what the record charged against MaxCachedBytes.
	charge int64
}

// txBatch is the state of one FindTransactions call.
type txBatch struct {
	src  PackSource
	r    TxRange
	opts TxOptions
	fn   func(*TxPairing) error
	// fromNs and toNs are r's bounds in nanoseconds; first and last the hours whose primaries are paired.
	fromNs, toNs int64
	first, last  int64
	// hours holds the hour records held, by hour.
	hours map[int64]*txHourRecord
	cache txCacheBudget
	ticks ctxCounter
	res   TxRangeResult
	// filled and evicting, set only by tests, run on each hour record once it is filled and before it is dropped.
	filled, evicting func(rec *txHourRecord)
}

// txCacheBudget charges the hour records a batch holds against MaxCachedBytes.
type txCacheBudget struct {
	limit, held int64
}

// reserve charges n bytes more, n ≥ 0.
//
// Returns:
//   - error: nil, or an error wrapping ErrReadLimit, charging nothing, when the charge would pass the limit.
func (c *txCacheBudget) reserve(n int64) error {
	// held ≤ limit, so limit - held cannot overflow.
	if n > c.limit-c.held {
		return fmt.Errorf("tracepack: find transactions: caching %d bytes more beside the %d held exceeds MaxCachedBytes %d: %w",
			n, c.held, c.limit, ErrReadLimit)
	}
	c.held += n

	return nil
}

func (c *txCacheBudget) release(n int64) {
	c.held -= n
}

// newTxBatch checks the arguments of FindTransactions, before src is called, and returns the batch.
func newTxBatch(src PackSource, r TxRange, opts TxRangeOptions, fn func(*TxPairing) error) (*txBatch, error) {
	switch {
	case src == nil:
		return nil, fmt.Errorf("%w: src is nil", ErrInvalidQuery)
	case fn == nil:
		return nil, fmt.Errorf("%w: fn is nil", ErrInvalidQuery)
	case r.Capture.IsZero():
		return nil, fmt.Errorf("%w: the capture_id is zero", ErrInvalidQuery)
	case !representableNs(r.From) || !representableNs(r.To):
		return nil, fmt.Errorf("%w: the range [%v, %v) is not representable as int64 nanoseconds", ErrInvalidQuery, r.From, r.To)
	}
	fromNs, toNs := r.From.UnixNano(), r.To.UnixNano()
	if fromNs >= toNs {
		return nil, fmt.Errorf("%w: the range [%v, %v) is empty", ErrInvalidQuery, r.From, r.To)
	}
	first, last := hourOf(fromNs), hourOf(toNs-1)
	if first < MinTxHour || last > MaxTxHour {
		return nil, fmt.Errorf("%w: the hours [%d, %d] of the range are outside [%d, %d]", ErrInvalidQuery, first, last, MinTxHour, MaxTxHour)
	}

	// checkFindTransaction applies the defaults and checks the scheduled tail of the last hour's primaries.
	lookup, err := checkFindTransaction(src, TxKey{Capture: r.Capture, Hour: last}, opts.Lookup)
	if err != nil {
		return nil, err
	}
	limit := opts.MaxCachedBytes
	if limit <= 0 {
		limit = DefaultTxMaxCachedBytes
	}

	return &txBatch{
		src: src, r: r, opts: lookup, fn: fn, fromNs: fromNs, toNs: toNs, first: first, last: last,
		hours: make(map[int64]*txHourRecord, lookup.MaxScopes), cache: txCacheBudget{limit: limit},
		ticks: ctxCounter{step: txCtxStep},
	}, nil
}

// representableNs reports whether t is representable as int64 nanoseconds since the Unix epoch.
func representableNs(t time.Time) bool {
	return time.Unix(0, t.UnixNano()).Equal(t)
}

// run reads the hours in ascending order and pairs the primaries of each hour once its lookups have every hour they read.
func (b *txBatch) run(ctx context.Context) error {
	m := int64(b.opts.MaxScopes)
	// newTxBatch checked that last + MaxScopes - 1 does not pass MaxTxHour.
	for h := b.first; h <= b.last+m-1; h++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		if err := b.fill(ctx, h); err != nil {
			return err
		}
		b.res.HoursRead++

		q := h - m + 1
		if q < b.first {
			continue
		}
		if q <= b.last {
			if err := b.pair(ctx, b.hours[q]); err != nil {
				return err
			}
		}
		b.evict(q)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: find transactions: %w", err)
	}

	return nil
}

// tick counts one unit of the batch's own work toward the checks of ctx, apart from any lookup's.
func (b *txBatch) tick(ctx context.Context) error {
	if !b.ticks.add(1) {
		return nil
	}

	return ctx.Err()
}

// fill reads hour h once into a new hour record: its observation, then one read of it as a lookup reads an hour,
// each part charged against MaxCachedBytes before it is kept.
func (b *txBatch) fill(ctx context.Context, h int64) error {
	readers, err := b.src.Readers(ctx, h)
	if err != nil {
		return fmt.Errorf("tracepack: find transactions: hour %d: readers: %w", h, err)
	}
	rec := &txHourRecord{hour: h}
	if err := b.charge(rec, txCacheHourCharge+int64(len(readers))*txCacheReaderCharge); err != nil {
		return fmt.Errorf("tracepack: find transactions: hour %d: %w", h, err)
	}
	b.hours[h] = rec
	for i, r := range readers {
		if r == nil {
			return fmt.Errorf("tracepack: find transactions: hour %d: readers: reader %d is nil", h, i)
		}
	}
	rec.readers = readers

	enumerate := h >= b.first && h <= b.last
	var flags uint8
	res, err := liveHours{src: b.src}.read(ctx, h, readers, b.opts.MaxHeldBytes,
		func() error {
			if err := b.charge(rec, txCacheReservationCharge); err != nil {
				return err
			}
			rec.reservations = append(rec.reservations, len(rec.items))

			return nil
		},
		&flags,
		func(it *Item) error { return b.keep(ctx, rec, it, flags, enumerate) })
	if err != nil {
		return fmt.Errorf("tracepack: find transactions: hour %d: %w", h, err)
	}
	if err := b.keepResult(ctx, rec, &res); err != nil {
		return fmt.Errorf("tracepack: find transactions: hour %d: %w", h, err)
	}
	if err := b.sortKeys(ctx, rec); err != nil {
		return fmt.Errorf("tracepack: find transactions: hour %d: %w", h, err)
	}
	if b.filled != nil {
		b.filled(rec)
	}

	return nil
}

// charge charges n bytes to rec against MaxCachedBytes.
func (b *txBatch) charge(rec *txHourRecord, n int64) error {
	if err := b.cache.reserve(n); err != nil {
		return err
	}
	rec.charge += n

	return nil
}

// keep keeps a copy of it, an item of rec's read whose stored record_flags are flags,
// and, when enumerate is set, the seq of its record when it is a primary the batch pairs.
func (b *txBatch) keep(ctx context.Context, rec *txHourRecord, it *Item, flags uint8, enumerate bool) error {
	if err := b.tick(ctx); err != nil {
		return err
	}
	n := txCacheItemCharge + int64(len(it.Record.Payload)) + int64(len(it.HeaderExtra)) + redactionCost(it.Redaction)
	key := enumerate && b.pairs(rec, it)
	if key {
		n += txCacheKeyCharge
	}
	if err := b.charge(rec, n); err != nil {
		return err
	}

	c := *it
	c.Record.Payload = bytes.Clone(it.Record.Payload)
	c.HeaderExtra = bytes.Clone(it.HeaderExtra)
	c.Redaction = cloneRecordRedaction(it.Redaction)
	rec.items = append(rec.items, c)
	rec.flags = append(rec.flags, flags)
	if key {
		rec.keys = append(rec.keys, it.Record.Seq)
	}

	return nil
}

// pairs reports whether it, an item of the read of rec's hour, is a version the batch pairs as a primary:
// a record of the batch's capture, a data record whose function is available and odd,
// whose ts_utc_ns lies in the hour read and in the batch's range.
func (b *txBatch) pairs(rec *txHourRecord, it *Item) bool {
	r := &it.Record
	if UUID(rec.readers[it.Pack].hdr.CaptureID) != b.r.Capture || r.Kind != KindData {
		return false
	}
	if r.TSUTCNs < b.fromNs || r.TSUTCNs >= b.toNs || hourOf(r.TSUTCNs) != rec.hour {
		return false
	}
	h := r.HSMSHeader()

	return h.Available&FieldValidityFunction != 0 && h.Function%2 == 1
}

// keepResult keeps res, the Result of rec's read, which the read built for the batch alone,
// charging each of its parts before the record holds it.
func (b *txBatch) keepResult(ctx context.Context, rec *txHourRecord, res *Result) error {
	for i := range res.Incomplete {
		d := &res.Incomplete[i]
		if err := b.tick(ctx); err != nil {
			return err
		}
		n := txCacheDefectCharge
		if d.Coverage != nil {
			n += coverageCost(d.Coverage)
			for range d.Coverage.Unknown {
				if err := b.tick(ctx); err != nil {
					return err
				}
			}
		}
		if d.Boundary != nil {
			n += txBoundaryCharge
		}
		if err := b.charge(rec, n); err != nil {
			return err
		}
	}
	for range res.FooterErrs {
		if err := b.tick(ctx); err != nil {
			return err
		}
		if err := b.charge(rec, txCacheFooterErrCharge); err != nil {
			return err
		}
	}
	for i := range res.Conflicts {
		c := &res.Conflicts[i]
		for range c.Versions {
			if err := b.tick(ctx); err != nil {
				return err
			}
		}
		// The reservation event charged a fixed size; the conflict's versions are charged here.
		if err := b.charge(rec, conflictCost(c)); err != nil {
			return err
		}
	}
	rec.res = *res

	return nil
}

// sortKeys sorts rec's keys and drops repeated seqs: a read yields the versions of a seq together,
// unless a block disagreeing with its F-2 entry yields them in two stretches.
// The sort cannot check ctx, so every key counts once toward the checks of ctx before it, and ctx is checked after it.
func (b *txBatch) sortKeys(ctx context.Context, rec *txHourRecord) error {
	if len(rec.keys) == 0 {
		return nil
	}
	for range rec.keys {
		if err := b.tick(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	slices.Sort(rec.keys)
	rec.keys = slices.Compact(rec.keys)

	return ctx.Err()
}

// evict drops the hour record of hour h and releases its charge.
func (b *txBatch) evict(h int64) {
	if rec, ok := b.hours[h]; ok {
		if b.evicting != nil {
			b.evicting(rec)
		}
		b.cache.release(rec.charge)
		delete(b.hours, h)
	}
}

// pair runs the lookup of every primary of rec, the record of an hour whose lookups have every hour they read,
// over the batch's hour records, and passes each pairing to fn.
func (b *txBatch) pair(ctx context.Context, rec *txHourRecord) error {
	hours := txReplay{b: b}
	for _, seq := range rec.keys {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		key := TxKey{Capture: b.r.Capture, Seq: seq, Hour: rec.hour}
		res, err := lookupHours(ctx, hours, key, b.opts, nil)
		if err != nil {
			return fmt.Errorf("tracepack: find transactions: primary seq %d of hour %d: %w", seq, rec.hour, err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		b.res.Primaries++
		if err := b.fn(&TxPairing{Key: key, Result: res}); err != nil {
			return err
		}
	}

	return nil
}

// txReplay serves a lookup the hours a batch read once, as liveHours serves the source's.
type txReplay struct {
	b *txBatch
}

var _ txHourSource = txReplay{}

// errTxHourNotHeld reports a replay of an hour the batch does not hold, which its scheduling never asks for.
var errTxHourNotHeld = errors.New("tracepack: find transactions: replay of an hour not held")

func (h txReplay) readers(ctx context.Context, hour int64) ([]*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rec, ok := h.b.hours[hour]
	if !ok {
		return nil, fmt.Errorf("%w: hour %d", errTxHourNotHeld, hour)
	}

	return slices.Clone(rec.readers), nil
}

// read replays the read of hour: each reservation at its place between the items, through reserveConflict,
// each item through fn after its stored flags, then the read's Result, shared and read-only.
// A refused reservation is wrapped as the live read wraps it; an error of fn is returned as is.
func (h txReplay) read(_ context.Context, hour int64, _ []*Reader, _ int64,
	reserveConflict func() error, flags *uint8, fn func(*Item) error,
) (Result, error) {
	rec, ok := h.b.hours[hour]
	if !ok {
		return Result{}, fmt.Errorf("%w: hour %d", errTxHourNotHeld, hour)
	}
	k := 0
	reserve := func(before int) error {
		for ; k < len(rec.reservations) && rec.reservations[k] == before; k++ {
			if err := reserveConflict(); err != nil {
				c := &rec.res.Conflicts[k]
				return refusedConflict(c.CaptureID, c.Seq, err)
			}
		}

		return nil
	}
	// One Item serves every call of fn, as a live read reuses its Item: the lookup copies what it keeps.
	var it Item
	for i := range rec.items {
		if err := reserve(i); err != nil {
			return Result{}, err
		}
		*flags = rec.flags[i]
		it = rec.items[i]
		if err := fn(&it); err != nil {
			return Result{}, err
		}
	}
	if err := reserve(len(rec.items)); err != nil {
		return Result{}, err
	}

	return rec.res, nil
}

// redactionCost returns the nominal charge of a copy of rr.
func redactionCost(rr *RecordRedaction) int64 {
	if rr == nil {
		return 0
	}
	n := txCacheRedactionCharge + int64(len(rr.Defects))*txCacheRedDefectCharge
	for i := range rr.Entries {
		e := &rr.Entries[i]
		n += txCacheEntryCharge + int64(len(e.MaskedRanges))*txCacheRangeCharge +
			int64(len(e.RawMaskedRanges)) + int64(len(e.Domain)) + int64(len(e.Digest)) + int64(len(e.SIV))
		if e.ItemPath != nil {
			n += int64(len(*e.ItemPath))
		}
	}

	return n
}

// cloneRecordRedaction returns a copy of rr that shares no storage with it, the defects' errors excepted; nil for nil.
func cloneRecordRedaction(rr *RecordRedaction) *RecordRedaction {
	if rr == nil {
		return nil
	}
	out := &RecordRedaction{WhollyMasked: rr.WhollyMasked, Defects: slices.Clone(rr.Defects)}
	if rr.Entries != nil {
		out.Entries = make([]RedactionEntry, len(rr.Entries))
		for i := range rr.Entries {
			out.Entries[i] = cloneRedactionEntry(&rr.Entries[i])
		}
	}

	return out
}
