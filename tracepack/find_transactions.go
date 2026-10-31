package tracepack

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// DefaultTxMaxCachedBytes is the default TxRangeOptions.MaxCachedBytes: 256 MiB.
const DefaultTxMaxCachedBytes = 256 << 20

// TxRange selects the primaries FindTransactions pairs:
// the primaries of one capture whose ts_utc_ns lies in [From, To).
type TxRange struct {
	// CaptureID is the capture whose primaries are paired; not the nil UUID.
	CaptureID UUID
	// From and To bound the primaries' ts_utc_ns, From included and To excluded;
	// From is before To, and both are representable as int64 nanoseconds.
	From, To time.Time
}

// TxRangeOptions configures FindTransactions.
type TxRangeOptions struct {
	// Lookup applies to the lookup of each primary as it applies to one FindTransaction:
	// MaxHours, and MaxHeldBytes, MaxConflicts and MaxStateBytes, each per lookup;
	// as many lookups as FindTransactions runs at once, currently up to GOMAXPROCS, are charged MaxStateBytes each at once (see FindTransactions).
	// MaxHeldBytes also bounds the read of each hour into the cache, as it bounds a lookup's read of that hour.
	Lookup TxOptions
	// MaxCachedBytes bounds the hours FindTransactions holds at once, the records and diagnostics of each hour read,
	// charged nominally, as TxOptions.MaxStateBytes charges a lookup's state;
	// zero means DefaultTxMaxCachedBytes; a negative value is refused.
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
// FindTransactions pairs every seq of r.CaptureID whose versions, as the read of hour H yields them,
// include a primary whose ts_utc_ns lies in hour H and in [r.From, r.To);
// its key is (r.CaptureID, seq, H).
// A version whose ts_utc_ns lies outside the hour it is read from is a scope breach and is not paired:
// the lookups whose hours hold it report the breach.
// Records of other captures are never paired, and take part in every lookup as they take part in FindTransaction's.
//
// FindTransactions calls src.Readers once for each hour it reads, in ascending order,
// from the hour of r.From to the last hour a primary's lookup schedules
// (the hour of r.To − 1 ns plus MaxHours − 1);
// the readers it gets for an hour, in that order, are the hour's observation for the whole call.
// It reads an hour's packs once, during that hour's read,
// and afterwards uses only the Readers' decoded header and metadata and the records it kept of the read,
// also while the lookups of earlier hours run on several goroutines.
// So, as PackSource describes, once it enters its next src.Readers call, or once it returns,
// it issues no read of the hour's packs and has none in flight,
// and the caller may close the objects backing that hour's Readers,
// while the Reader values stay valid until FindTransactions returns.
// The result of every primary it reports equals FindTransaction's for its key over a PackSource that returns each hour's observation,
// with the same TxOptions, field by field and in every order;
// over a source whose Readers returns the same readers in the same order on every call, as NewReaderSource's does,
// that is FindTransaction over src itself.
//
// It passes the pairings to fn in ascending hour, then ascending seq;
// a primary of hour H is passed once hour H + MaxHours − 1 is read,
// also when its lookup ended after hour H, as the lookup of a missing, conflicting or keyless primary does.
// A primary near the end of the hours read can report TxGapOpenWindow
// because a later hour is empty or not yet written: FindTransactions cannot tell a settled empty hour from one not yet settled.
// A caller that needs settled results keeps the hour of r.To − 1 ns at or before the last settled hour minus MaxHours − 1,
// or calls again once later hours are settled.
//
// Each hour is held, read once, while a primary's lookup still needs it, so at most MaxHours hours at once,
// bounded by TxRangeOptions.MaxCachedBytes.
//
// The lookups of an hour's primaries may run on several goroutines at once:
// currently up to runtime.GOMAXPROCS(0), read when the call starts, and on the calling goroutine alone when that is 1 or less,
// a count that may change.
// fn is called from the calling goroutine, one pairing at a time, in the order above;
// without cancellation, the pairings, the error and the result are those of the lookups run one after another,
// and a panic in a lookup is raised again on the calling goroutine.
// Every goroutine FindTransactions starts has ended when it returns.
//
// Parameters:
//   - ctx: cancels the call; checked before each hour, during each hour's read and enumeration,
//     before and inside each lookup, before each pairing is passed to fn, and before FindTransactions returns;
//     the checks before each lookup's result and each pairing are the calling goroutine's, and no pairing is passed after one of them sees ctx done.
//     Which check sees it first, and so the error's wrapping and the counts, can depend on timing.
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
	txCacheItemCharge        = (int64(unsafe.Sizeof(Item{})+unsafe.Sizeof(txItemMeta{})) + txAlignMask) &^ txAlignMask
	txCacheReservationCharge = int64(unsafe.Sizeof(int(0)))
	txCacheKeyCharge         = int64(unsafe.Sizeof(uint64(0)))
	txCacheDefectCharge      = (int64(unsafe.Sizeof(Defect{})) + txAlignMask) &^ txAlignMask
	txCacheFooterErrCharge   = (int64(unsafe.Sizeof(PackError{})) + txAlignMask) &^ txAlignMask
	txCacheRedactionCharge   = (int64(unsafe.Sizeof(RecordRedaction{})) + txAlignMask) &^ txAlignMask
	txCacheEntryCharge       = (int64(unsafe.Sizeof(RedactionEntry{})) + txAlignMask) &^ txAlignMask
	txCacheRangeCharge       = int64(unsafe.Sizeof(MaskedRange{}))
	txCacheRedDefectCharge   = (int64(unsafe.Sizeof(RedactionDefect{})) + txAlignMask) &^ txAlignMask
)

// txHourRecord is one hour a batch read once: its observation, the read's items with their stored flags and HSMS headers,
// where the read reserved each conflict, and the read's Result.
// It is never changed once filled: every lookup replaying it reads it only (txReplay).
type txHourRecord struct {
	hour    int64
	readers []*Reader
	// items are the read's items, in yield order, each the record's own copy (Item.Clone),
	// whose HSMSHeader every lookup replaying it reads instead of decoding the payload again;
	// meta[i] holds item i's record_flags as stored.
	items []Item
	meta  []txItemMeta
	// reservations holds, for the k-th conflict the read reserved, the number of items yielded before it;
	// the conflict is res.Conflicts[k], since a read lists every conflict it reserves, in order.
	reservations []int
	res          Result
	// keys holds the seqs of the hour's primaries the batch pairs (FindTransactions), ascending, each once.
	keys []uint64
	// charge is what the record charged against MaxCachedBytes.
	charge int64
}

// txItemMeta is what an hour record holds beside one of its items: the record_flags of its record header, as stored.
type txItemMeta struct {
	flags uint8
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
	// filled and evicting, set only by tests, run on each hour record once it is filled and before it is dropped;
	// lookupPrep, set only by tests, runs on the state of each primary's lookup before it starts.
	filled, evicting func(rec *txHourRecord)
	lookupPrep       func(l *txLookup)
	// onTick, set only by tests, runs at each check of ctx that tick makes, before it.
	onTick func()
	// workers is the number of goroutines that run an hour's lookups, runtime.GOMAXPROCS(0) when the call starts;
	// 1 or less runs them one after another on the calling goroutine (pairParallel).
	workers int
	// parallelLookup, set only by tests, runs on a worker in place of the lookup of the i-th key of an hour; it must be safe for concurrent use.
	// liveWorkers counts the workers running, for tests.
	parallelLookup func(ctx context.Context, i int, key TxKey) (TxResult, error)
	liveWorkers    atomic.Int32
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
	case r.CaptureID.IsZero():
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

	if msg := negativeOption("TxRangeOptions.Lookup", opts.Lookup.budgets()...); msg != "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidQuery, msg)
	}
	// checkFindTransaction applies the defaults and checks the scheduled tail of the last hour's primaries.
	lookup, err := checkFindTransaction(src, TxKey{CaptureID: r.CaptureID, Hour: last}, opts.Lookup)
	if err != nil {
		return nil, err
	}
	if msg := negativeOption("TxRangeOptions", optionField{"MaxCachedBytes", opts.MaxCachedBytes}); msg != "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidQuery, msg)
	}
	limit := opts.MaxCachedBytes
	if limit <= 0 {
		limit = DefaultTxMaxCachedBytes
	}

	return &txBatch{
		src: src, r: r, opts: lookup, fn: fn, fromNs: fromNs, toNs: toNs, first: first, last: last,
		// The map grows with the hours held, at most MaxHours, each charged when filled; no capacity is reserved ahead of them.
		hours: map[int64]*txHourRecord{}, cache: txCacheBudget{limit: limit},
		ticks: ctxCounter{step: txCtxStep}, workers: runtime.GOMAXPROCS(0),
	}, nil
}

// representableNs reports whether t is representable as int64 nanoseconds since the Unix epoch.
func representableNs(t time.Time) bool {
	return time.Unix(0, t.UnixNano()).Equal(t)
}

// run reads the hours in ascending order and pairs the primaries of each hour once its lookups have every hour they read.
func (b *txBatch) run(ctx context.Context) error {
	m := int64(b.opts.MaxHours)
	// newTxBatch checked that last + MaxHours - 1 does not pass MaxTxHour.
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
	// The hours after last were read only as the tails of earlier hours' lookups.
	for h := b.last + 1; h <= b.last+m-1; h++ {
		b.evict(h)
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
	if b.onTick != nil {
		b.onTick()
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
			if err := b.tick(ctx); err != nil {
				return err
			}
			if err := b.charge(rec, txCacheReservationCharge); err != nil {
				return err
			}
			rec.reservations = append(rec.reservations, len(rec.items))

			return nil
		},
		&flags, nil,
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
	// A read yields the versions of a seq together, so a seq repeating the last key is charged once;
	// the repeats a block disagreeing with its F-2 entry brings apart are released when the keys are sorted.
	key := enumerate && b.pairs(rec, it) && (len(rec.keys) == 0 || rec.keys[len(rec.keys)-1] != it.Record.Seq)
	if key {
		n += txCacheKeyCharge
	}
	if err := b.charge(rec, n); err != nil {
		return err
	}

	rec.items = append(rec.items, it.Clone())
	rec.meta = append(rec.meta, txItemMeta{flags: flags})
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
	if UUID(rec.readers[it.Input].hdr.CaptureID) != b.r.CaptureID || r.Kind != KindData {
		return false
	}
	if r.TSUTCNs < b.fromNs || r.TSUTCNs >= b.toNs || hourOf(r.TSUTCNs) != rec.hour {
		return false
	}
	h := r.HSMSHeader()

	return h.Available&FieldValidityFunction != 0 && h.Function%2 == 1
}

// keepResult keeps res, the Result of rec's read, which the read built for the batch alone,
// charging each of its parts before the record holds it;
// every defect, coverage Unknown entry, footer error and conflict version counts once toward the checks of ctx,
// in the same traversal that charges it.
func (b *txBatch) keepResult(ctx context.Context, rec *txHourRecord, res *Result) error {
	for i := range res.Incomplete {
		d := &res.Incomplete[i]
		if err := b.tick(ctx); err != nil {
			return err
		}
		n := txCacheDefectCharge
		if c := d.Coverage; c != nil {
			n += txCoverageCharge
			for k := range c.Unknown {
				if err := b.tick(ctx); err != nil {
					return err
				}
				n += rawEntryCost(&c.Unknown[k])
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
		// The reservation event charged a fixed size; the conflict and its versions are charged here.
		n := txConflictCharge
		for _, v := range res.Conflicts[i].Versions {
			if err := b.tick(ctx); err != nil {
				return err
			}
			n += conflictVersionCost(v)
		}
		if err := b.charge(rec, n); err != nil {
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
	n := len(rec.keys)
	rec.keys = slices.Compact(rec.keys)
	// A seq kept twice, apart, charged twice; the repeats are released.
	dropped := int64(n-len(rec.keys)) * txCacheKeyCharge
	b.cache.release(dropped)
	rec.charge -= dropped

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
// over the batch's hour records, and passes each pairing to fn:
// on the batch's workers (pairParallel), or one after another when it has one worker, rec has one key, or a test hooks every lookup.
func (b *txBatch) pair(ctx context.Context, rec *txHourRecord) error {
	if b.workers > 1 && len(rec.keys) > 1 && b.lookupPrep == nil {
		return b.pairParallel(ctx, rec)
	}
	hours := txReplay{b: b}
	for _, seq := range rec.keys {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		key := TxKey{CaptureID: b.r.CaptureID, Seq: seq, Hour: rec.hour}
		res, err := lookupHours(ctx, hours, key, b.opts, b.lookupPrep)
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

// txSlot is where a worker of pairParallel leaves the result of one lookup for the calling goroutine.
// Its worker writes the result between taking free and sending ready; the calling goroutine reads and clears it
// between receiving ready and sending free, so the two channels order every access.
type txSlot struct {
	// free holds a token while the worker may run its next lookup; ready, once the result is in the slot.
	free, ready chan struct{}
	res         TxResult
	err         error
	// panicked reports that the lookup panicked with panicVal.
	panicked bool
	panicVal any
}

// pairParallel is pair over min(workers, len(rec.keys)) worker goroutines, w:
// worker s runs the lookups of keys s, s+w, s+2w, …, each once the calling goroutine has passed the pairing before it in its slot,
// so at most w lookups are running or waiting to be passed at once;
// the calling goroutine takes the results in key order, checks ctx before each lookup's result and before each pairing as pair does,
// and passes each pairing to fn, so the pairings, their order, the error and the counts are pair's when ctx is not cancelled.
// A lookup's error or panic is seen only when its key's turn comes; a panic is raised again on the calling goroutine.
// Every worker has ended when it returns, or panics.
func (b *txBatch) pairParallel(ctx context.Context, rec *txHourRecord) error {
	n := len(rec.keys)
	w := min(b.workers, n)
	slots := make([]txSlot, w)
	lctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
	}()

	lookup := func(ctx context.Context, _ int, key TxKey) (TxResult, error) {
		return lookupHours(ctx, txReplay{b: b}, key, b.opts, nil)
	}
	if b.parallelLookup != nil {
		lookup = b.parallelLookup
	}
	wg.Add(w)
	for s := range slots {
		sl := &slots[s]
		sl.free, sl.ready = make(chan struct{}, 1), make(chan struct{}, 1)
		sl.free <- struct{}{}
		go func() {
			defer wg.Done()
			b.liveWorkers.Add(1)
			defer b.liveWorkers.Add(-1)
			for i := s; i < n; i += w {
				select {
				case <-sl.free:
				case <-lctx.Done():
					return
				}
				key := TxKey{CaptureID: b.r.CaptureID, Seq: rec.keys[i], Hour: rec.hour}
				func() {
					defer func() {
						if v := recover(); v != nil {
							sl.panicked, sl.panicVal = true, v
						}
					}()
					sl.res, sl.err = lookup(lctx, i, key)
				}()
				panicked := sl.panicked
				// ready is empty: the calling goroutine takes it before it gives free back.
				sl.ready <- struct{}{}
				if panicked {
					return
				}
			}
		}()
	}

	for i, seq := range rec.keys {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		sl := &slots[i%w]
		select {
		case <-sl.ready:
		case <-ctx.Done():
			return fmt.Errorf("tracepack: find transactions: %w", ctx.Err())
		}
		if sl.panicked {
			panic(sl.panicVal)
		}
		if sl.err != nil {
			return fmt.Errorf("tracepack: find transactions: primary seq %d of hour %d: %w", seq, rec.hour, sl.err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("tracepack: find transactions: %w", err)
		}
		b.res.Primaries++
		if err := b.fn(&TxPairing{Key: TxKey{CaptureID: b.r.CaptureID, Seq: seq, Hour: rec.hour}, Result: sl.res}); err != nil {
			return err
		}
		sl.res, sl.err = TxResult{}, nil
		sl.free <- struct{}{}
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
// each item through fn after its stored flags and its record's HSMS header, the item the record's own, then the read's Result, shared and read-only.
// A refused reservation is wrapped as the live read wraps it; an error of fn is returned as is.
func (h txReplay) read(_ context.Context, hour int64, _ []*Reader, _ int64,
	reserveConflict func() error, flags *uint8, header *txCachedHeader, fn func(*Item) error,
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
	// fn gets the record's own item, which it reads only (txHourSource); the lookup copies what it keeps.
	for i := range rec.items {
		if k < len(rec.reservations) && rec.reservations[k] == i {
			if err := reserve(i); err != nil {
				return Result{}, err
			}
		}
		*flags = rec.meta[i].flags
		*header = txCachedHeader{h: rec.items[i].HSMSHeader, ok: true}
		if err := fn(&rec.items[i]); err != nil {
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
