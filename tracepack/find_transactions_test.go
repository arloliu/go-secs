package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txStrictForm is a TxResult in the form the batch-versus-single comparison compares:
// every field as it is, every slice in its order, nil and empty apart,
// with each error replaced by its text, since a lookup builds its errors afresh.
type txStrictForm struct {
	res       TxResult
	gapErrs   []string
	packErrs  []string
	recordLen int
}

// strictForm returns the strict form of res, sharing no storage with it.
func strictForm(res *TxResult) txStrictForm {
	c := cloneTxResult(res)
	f := txStrictForm{recordLen: len(c.Records)}
	for i := range c.Incomplete {
		f.gapErrs = append(f.gapErrs, errText(c.Incomplete[i].Err))
		c.Incomplete[i].Err = nil
	}
	for i := range c.FooterErrs {
		f.packErrs = append(f.packErrs, errText(c.FooterErrs[i].Err))
		c.FooterErrs[i].Err = nil
	}
	f.res = c

	return f
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}

	return err.Error()
}

// cloneTxResult returns a deep copy of res, its errors shared.
func cloneTxResult(res *TxResult) TxResult {
	c := *res
	c.WindowEnd = clonePtr(res.WindowEnd)
	if res.Records != nil {
		c.Records = make([]TxRecord, len(res.Records))
		for i, r := range res.Records {
			r.Record.Payload = slices.Clone(r.Record.Payload)
			r.HeaderExtra = slices.Clone(r.HeaderExtra)
			c.Records[i] = r
		}
	}
	if res.Incomplete != nil {
		c.Incomplete = make([]TxGap, len(res.Incomplete))
		for i, g := range res.Incomplete {
			g.Hours = slices.Clone(g.Hours)
			g.Pack, g.Seq = clonePtr(g.Pack), clonePtr(g.Seq)
			if g.Coverage != nil {
				g.Coverage = cloneCoverageStrict(g.Coverage)
			}
			if g.Boundary != nil {
				g.Boundary = new(cloneBoundary(*g.Boundary))
			}
			c.Incomplete[i] = g
		}
	}
	if res.Searched != nil {
		c.Searched = make([]TxScope, len(res.Searched))
		for i, s := range res.Searched {
			c.Searched[i] = TxScope{Hour: s.Hour, Packs: slices.Clone(s.Packs)}
		}
	}
	if res.Conflicts != nil {
		c.Conflicts = make([]Conflict, len(res.Conflicts))
		for i := range res.Conflicts {
			c.Conflicts[i] = cloneConflict(&res.Conflicts[i])
		}
	}
	c.FooterErrs = slices.Clone(res.FooterErrs)

	return c
}

// cloneCoverageStrict returns a copy of c that keeps a nil Unknown nil and an empty one empty, as cloneCoverage does not.
func cloneCoverageStrict(c *Coverage) *Coverage {
	out := &Coverage{
		CaptureID: clonePtr(c.CaptureID), SeqFirst: clonePtr(c.SeqFirst), SeqLast: clonePtr(c.SeqLast),
		TimeStart: clonePtr(c.TimeStart), TimeEnd: clonePtr(c.TimeEnd),
	}
	if c.Unknown != nil {
		out.Unknown = make([]RawEntry, len(c.Unknown))
		for i, e := range c.Unknown {
			out.Unknown[i] = RawEntry{Tag: e.Tag, Type: e.Type, Value: slices.Clone(e.Value)}
		}
	}

	return out
}

// requireStrictEqual requires got to equal want in the strict form;
// require.Equal compares by reflect.DeepEqual, so nil and empty slices stay apart, and the errors are already texts.
func requireStrictEqual(t testing.TB, want, got txStrictForm, msgAndArgs ...any) {
	t.Helper()

	require.Equal(t, want, got, msgAndArgs...)
}

// requireBatchAgrees runs FindTransactions over the hour of key with opts, over src, a source whose answers do not change
// (requireBatchOverHour), requiring the pairing of key, when the hour pairs it, to equal want, the result of its lookup alone.
func requireBatchAgrees(t testing.TB, src PackSource, key TxKey, opts TxOptions, want *TxResult) {
	t.Helper()

	requireBatchOverHour(t, src, key, opts, want)
}

// requireBatchAgreesOnError runs FindTransactions over the hour of key, a key whose lookup alone failed (requireBatchOverHour).
func requireBatchAgreesOnError(t testing.TB, src PackSource, key TxKey, opts TxOptions) {
	t.Helper()

	requireBatchOverHour(t, src, key, opts, nil)
}

// txLookupRun is a lookup's result alone, in the strict form, its error, and the events of the hours it read (txEventLog).
type txLookupRun struct {
	form   txStrictForm
	err    error
	events []string
}

// requireBatchOverHour runs FindTransactions over the whole hour of key with opts, over src, a source whose answers do not change,
// its lookups one after another and on several workers,
// and requires it to pair the keys the hour holds, derived apart from FindTransactions (txHourKeys), in order,
// each with the result its lookup alone returns, compared strictly, the lookup reading its hours through the same events;
// and the pairing of key, when want is not nil and the hour pairs key, to equal want.
// When a lookup fails alone, the batch fails at the first that does, after pairing exactly the keys before it,
// with its error wrapped with its key, its replay stopping at the event where the lookup alone stops.
// A batch may also fail in reading an hour into its cache, under MaxHeldBytes, before it pairs any key:
// a read of that hour alone then fails too.
func requireBatchOverHour(t testing.TB, src PackSource, key TxKey, opts TxOptions, want *TxResult) {
	t.Helper()

	if key.Hour < MinTxHour || key.Hour > MaxTxHour || key.CaptureID.IsZero() {
		return
	}
	from := time.Unix(0, key.Hour*hourNs)
	r := TxRange{CaptureID: key.CaptureID, From: from, To: from.Add(time.Hour)}
	if _, err := newTxBatch(src, r, TxRangeOptions{Lookup: opts}, func(*TxPairing) error { return nil }); err != nil {
		require.ErrorIs(t, err, ErrInvalidQuery)
		return // a tail past MaxTxHour, which the single lookup refuses too
	}

	keys := txHourKeys(t, src, key.CaptureID, key.Hour)
	singles := make([]txLookupRun, len(keys))
	first := len(keys)
	for i, k := range keys {
		var log *txEventLog
		res, err := findTransactionWith(t.Context(), src, k, opts, func(l *txLookup) {
			log = &txEventLog{inner: l.hours}
			l.hours = log
		})
		singles[i] = txLookupRun{form: strictForm(&res), err: err, events: log.events}
		if err != nil && first == len(keys) {
			first = i
		}
		if k == key && want != nil {
			requireStrictEqual(t, strictForm(want), singles[i].form, "the lookup of the test's key %v", key)
		}
	}

	var gotKeys []TxKey
	var got []txStrictForm
	var replays []*txEventLog
	res, err := findTransactionsWith(t.Context(), src, r, TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
		gotKeys, got = append(gotKeys, p.Key), append(got, strictForm(&p.Result))
		mutateTxResult(&p.Result)

		return nil
	}, func(b *txBatch) {
		b.lookupPrep = func(l *txLookup) {
			log := &txEventLog{inner: l.hours}
			l.hours = log
			replays = append(replays, log)
		}
	})
	assert.Equal(t, len(gotKeys), res.Primaries, "the pairings counted")
	// The lookups run on several workers pair the same keys with the same results, and end as the batch above does.
	var parKeys []TxKey
	var par []txStrictForm
	pres, perr := findTransactionsWith(t.Context(), src, r, TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
		parKeys, par = append(parKeys, p.Key), append(par, strictForm(&p.Result))
		mutateTxResult(&p.Result)

		return nil
	}, txWorkers(4))
	assert.Equal(t, res, pres, "the summary on several workers")
	assert.Equal(t, errText(err), errText(perr), "the error on several workers")
	require.Equal(t, gotKeys, parKeys, "the keys paired on several workers")
	for i := range par {
		requireStrictEqual(t, got[i], par[i], "pairing of %v on several workers", parKeys[i])
	}
	pairedBefore := func(n int) {
		t.Helper()

		if n == 0 {
			require.Empty(t, gotKeys, "no key paired")
		} else {
			require.Equal(t, keys[:n], gotKeys, "the keys paired, in order")
		}
		for i := range n {
			requireStrictEqual(t, singles[i].form, got[i], "batch pairing of %v", keys[i])
		}
	}

	var seq uint64
	var hour int64
	switch {
	case err == nil:
		require.Equal(t, len(keys), first, "no lookup fails alone")
		pairedBefore(len(keys))
	case scanErr("tracepack: find transactions: primary seq %d of hour %d:", err, &seq, &hour):
		require.Less(t, first, len(keys), "a lookup fails alone: %v", err)
		k, serr := keys[first], singles[first].err
		require.Equal(t, TxKey{CaptureID: key.CaptureID, Seq: seq, Hour: hour}, k, "the batch fails at the first key failing alone")
		assert.Equal(t, fmt.Sprintf("tracepack: find transactions: primary seq %d of hour %d: %v", k.Seq, k.Hour, serr), err.Error())
		for _, target := range []error{ErrReadLimit, ErrNotPrimary, ErrInvalidQuery} {
			assert.Equal(t, errors.Is(serr, target), errors.Is(err, target), "errors.Is %v", target)
		}
		pairedBefore(first)
	case scanErr("tracepack: find transactions: hour %d:", err, &hour):
		require.ErrorIs(t, err, ErrReadLimit, "the batch fails in reading an hour only under MaxHeldBytes here")
		require.Empty(t, gotKeys, "no key of the hour is paired before its hours are read")
		requireHourReadFails(t, src, hour, opts.MaxHeldBytes)

		return
	default:
		require.Failf(t, "an unexpected error", "%v", err)
	}
	require.Len(t, replays, len(gotKeys)+min(1, len(keys)-first), "a replayed lookup for each key paired and for the one failing")
	for i, log := range replays {
		assert.Equal(t, singles[i].events, log.events, "the events the lookup of %v reads, through its failure", keys[i])
	}
}

// scanErr reports whether err's text starts with format, scanning its values into args.
func scanErr(format string, err error, args ...any) bool {
	_, serr := fmt.Sscanf(err.Error(), format, args...)

	return serr == nil
}

// requireHourReadFails requires a read of hour over src, as one MergeIterate in capture order with payloads
// and maxHeld as MaxHeldBytes, to fail with an error wrapping ErrReadLimit.
func requireHourReadFails(t testing.TB, src PackSource, hour, maxHeld int64) {
	t.Helper()

	readers, err := src.Readers(t.Context(), hour)
	require.NoError(t, err)
	_, err = MergeIterate(t.Context(), readers, Query{Payloads: true},
		MergeIterateOptions{Order: OrderCapture, MaxHeldBytes: maxHeld}, func(*Item) error { return nil })
	require.ErrorIs(t, err, ErrReadLimit, "the read of hour %d alone", hour)
}

// txEventLog is a txHourSource that records, in order, the events a lookup reads its hours through:
// each readers call, each read, each reservation of a conflict and each item passed to the lookup, with their errors,
// and the end of each read.
type txEventLog struct {
	inner  txHourSource
	events []string
}

// addf records one event, formatted as fmt.Sprintf formats it.
func (l *txEventLog) addf(format string, a ...any) {
	l.events = append(l.events, fmt.Sprintf(format, a...))
}

func (l *txEventLog) readers(ctx context.Context, hour int64) ([]*Reader, error) {
	readers, err := l.inner.readers(ctx, hour)
	l.addf("readers %d: %d, %s", hour, len(readers), errText(err))

	return readers, err
}

func (l *txEventLog) read(ctx context.Context, hour int64, readers []*Reader, maxHeld int64,
	reserveConflict func() error, flags *uint8, header *txCachedHeader, fn func(*Item) error,
) (Result, error) {
	l.addf("read %d", hour)
	res, err := l.inner.read(ctx, hour, readers, maxHeld,
		func() error {
			err := reserveConflict()
			l.addf("reserve: %s", errText(err))

			return err
		},
		flags, header,
		func(it *Item) error {
			err := fn(it)
			l.addf("item %d/%d seq %d flags %d conflict %t: %s", it.Pack, it.Block, it.Record.Seq, *flags, it.Conflict, errText(err))

			return err
		})
	l.addf("end %d: %s", hour, errText(err))

	return res, err
}

// txHourKeys returns the keys FindTransactions pairs over src for capture and the whole of hour, in ascending seq,
// derived apart from FindTransactions:
// from the reference of a read over several packs (newMergeIterateReference),
// or, when a block of the hour disagrees with its F-2 entry, which the reference does not model,
// from each pack's own read (txPerPackKeys).
func txHourKeys(t testing.TB, src PackSource, capture UUID, hour int64) []TxKey {
	t.Helper()

	readers, err := src.Readers(t.Context(), hour)
	require.NoError(t, err)
	fromNs, toNs := hour*hourNs, (hour+1)*hourNs
	if !txReferenceHolds(readers) {
		return txPerPackKeys(t, readers, capture, hour, fromNs, toNs)
	}
	sc := &txOracleScope{hour: hour}
	for _, r := range readers {
		sc.captures = append(sc.captures, r.Header().CaptureID)
	}
	sc.items, _ = newMergeIterateReference(t, readers, Query{Payloads: true}).read(OrderCapture)

	return txRangeKeys(&txSnapshot{capture: capture, scopes: map[int64]*txOracleScope{hour: sc}}, capture, fromNs, toNs)
}

// txReferenceHolds reports whether the reference of a read over several packs holds for readers:
// every block reads without a ReadAt error, and none disagrees with its F-2 entry.
func txReferenceHolds(readers []*Reader) bool {
	for _, r := range readers {
		for j := range r.blocks {
			var buf blockBuf
			_, def, err := r.readBlock(j, &buf)
			if err != nil || def != nil && def.Reason == IncompleteReasonIndexMismatch {
				return false
			}
		}
	}

	return true
}

// txPerPackKeys returns the keys FindTransactions pairs for capture and the range [fromNs, toNs) of hour
// from the read of each pack of readers on its own:
// the seqs of every version a pack yields that is a primary (txRangePrimary), ascending, each once.
func txPerPackKeys(t testing.TB, readers []*Reader, capture UUID, hour, fromNs, toNs int64) []TxKey {
	t.Helper()

	var seqs []uint64
	for _, r := range readers {
		packCapture := r.Header().CaptureID
		_, err := r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
			if txRangePrimary(capture, packCapture, &it.Record, hour, fromNs, toNs) {
				seqs = append(seqs, it.Record.Seq)
			}

			return nil
		})
		require.NoError(t, err)
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	if len(seqs) == 0 {
		return nil
	}
	keys := make([]TxKey, 0, len(seqs))
	for _, s := range seqs {
		keys = append(keys, TxKey{CaptureID: capture, Seq: s, Hour: hour})
	}

	return keys
}

// mutateTxResult changes every mutable part of res that a caller can reach.
func mutateTxResult(res *TxResult) {
	res.Outcome = 99
	if res.WindowEnd != nil {
		*res.WindowEnd ^= 1
	}
	for i := range res.Records {
		r := &res.Records[i]
		for j := range r.Record.Payload {
			r.Record.Payload[j] ^= 0xFF
		}
		for j := range r.HeaderExtra {
			r.HeaderExtra[j] ^= 0xFF
		}
		r.Record.Seq ^= 1
	}
	for i := range res.Incomplete {
		mutateTxGap(&res.Incomplete[i])
	}
	for i := range res.Searched {
		for j := range res.Searched[i].Packs {
			res.Searched[i].Packs[j][0] ^= 0xFF
		}
	}
	for i := range res.Conflicts {
		for j := range res.Conflicts[i].Versions {
			for k := range res.Conflicts[i].Versions[j] {
				res.Conflicts[i].Versions[j][k][0] ^= 0xFF
			}
		}
	}
	for i := range res.FooterErrs {
		res.FooterErrs[i].Pack[0] ^= 0xFF
	}
}

// mutateTxGap changes every mutable part of g.
func mutateTxGap(g *TxGap) {
	for j := range g.Hours {
		g.Hours[j]++
	}
	if g.Pack != nil {
		g.Pack[0] ^= 0xFF
	}
	if g.Seq != nil {
		*g.Seq ^= 1
	}
	if c := g.Coverage; c != nil {
		mutateCoverage(c)
	}
	if b := g.Boundary; b != nil {
		b.Seq ^= 1
		b.CaptureID[0] ^= 0xFF
		for _, p := range []*int64{b.GapStart, b.GapEnd} {
			if p != nil {
				*p ^= 1
			}
		}
	}
}

// mutateCoverage changes every mutable part of c.
func mutateCoverage(c *Coverage) {
	for _, p := range []*uint64{c.SeqFirst, c.SeqLast} {
		if p != nil {
			*p ^= 1
		}
	}
	for _, p := range []*int64{c.TimeStart, c.TimeEnd} {
		if p != nil {
			*p ^= 1
		}
	}
	if c.CaptureID != nil {
		c.CaptureID[0] ^= 0xFF
	}
	for j := range c.Unknown {
		for k := range c.Unknown[j].Value {
			c.Unknown[j].Value[k] ^= 0xFF
		}
	}
}

// txRangeKeys returns the keys FindTransactions pairs over snap for capture and the range [fromNs, toNs),
// by the rule of FindTransactions, from the reference read items of each hour.
func txRangeKeys(snap *txSnapshot, capture UUID, fromNs, toNs int64) []TxKey {
	var keys []TxKey
	for h := hourOf(fromNs); h <= hourOf(toNs-1); h++ {
		sc := snap.scopes[h]
		if sc == nil {
			continue
		}
		var seqs []uint64
		for _, it := range sc.items {
			if txRangePrimary(capture, sc.captures[it.pack], &it.rec, h, fromNs, toNs) {
				seqs = append(seqs, it.rec.Seq)
			}
		}
		slices.Sort(seqs)
		for _, s := range slices.Compact(seqs) {
			keys = append(keys, TxKey{CaptureID: capture, Seq: s, Hour: h})
		}
	}

	return keys
}

// txRangePrimary reports whether r, a version the read of hour yields from a pack of packCapture,
// is a primary FindTransactions pairs for capture and the range [fromNs, toNs):
// a data record of capture whose ts_utc_ns lies in hour and in the range, whose function is available and odd.
func txRangePrimary(capture, packCapture UUID, r *Record, hour, fromNs, toNs int64) bool {
	if packCapture != capture || r.Kind != KindData || r.TSUTCNs < fromNs || r.TSUTCNs >= toNs || hourOf(r.TSUTCNs) != hour {
		return false
	}
	hh := r.HSMSHeader()

	return hh.Available&FieldValidityFunction != 0 && hh.Function%2 == 1
}

// txWorkerCounts are the worker counts the batch tests run FindTransactions with:
// its lookups one after another, and on several goroutines whatever GOMAXPROCS the test runs under.
var txWorkerCounts = []int{1, 4}

// txWorkers returns the prep of a batch that runs an hour's lookups on n workers.
func txWorkers(n int) func(b *txBatch) {
	return func(b *txBatch) { b.workers = n }
}

// checkTxRange runs FindTransactions over g for [fromNs, toNs) with opts, mutating every pairing once it is recorded,
// with each of txWorkerCounts,
// and requires it to pair exactly the keys of the reference, in order, each with the result FindTransaction gives,
// or to fail as the first lookup that fails.
func checkTxRange(t *testing.T, g *txGen, snap *txSnapshot, fromNs, toNs int64, opts TxOptions) {
	t.Helper()

	want := txRangeKeys(snap, captureLow, fromNs, toNs)
	// Every expectation is taken before the batch runs and its callback mutates what it is given.
	singles := make([]txStrictForm, len(want))
	serrs := make([]error, len(want))
	for i, key := range want {
		single, serr := FindTransaction(t.Context(), g.src.view(), key, opts)
		singles[i], serrs[i] = strictForm(&single), serr
	}
	exp := txRangeWant{keys: want, singles: singles, serrs: serrs}
	for _, workers := range txWorkerCounts {
		checkTxRangeWith(t, g, &exp, fromNs, toNs, opts, workers)
	}
}

// txRangeWant is what checkTxRange expects of a FindTransactions call, taken before any call:
// the keys of the range, and each key's result alone, in the strict form, and error.
type txRangeWant struct {
	keys    []TxKey
	singles []txStrictForm
	serrs   []error
}

// checkTxRangeWith is the check of checkTxRange with workers, against exp.
func checkTxRangeWith(t *testing.T, g *txGen, exp *txRangeWant, fromNs, toNs int64, opts TxOptions, workers int) {
	t.Helper()

	want, singles, serrs := exp.keys, exp.singles, exp.serrs
	var gotKeys []TxKey
	var got []txStrictForm
	before := g.src.counts()
	res, err := findTransactionsWith(t.Context(), g.src, TxRange{CaptureID: captureLow, From: time.Unix(0, fromNs), To: time.Unix(0, toNs)},
		TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
			gotKeys = append(gotKeys, p.Key)
			got = append(got, strictForm(&p.Result))
			mutateTxResult(&p.Result)

			return nil
		}, txWorkers(workers))

	m := opts.MaxScopes
	if m <= 0 {
		m = DefaultTxMaxScopes
	}
	for h, n := range g.src.counts() {
		require.LessOrEqual(t, n-before[h], 1, "workers %d: hour %d read once", workers, h)
	}

	for i, key := range want {
		if serr := serrs[i]; serr != nil {
			require.Error(t, err, "workers %d: key %v fails alone", workers, key)
			assert.Equal(t, fmt.Sprintf("tracepack: find transactions: primary seq %d of hour %d: %v", key.Seq, key.Hour, serr), err.Error())
			assert.Equal(t, errors.Is(serr, ErrReadLimit), errors.Is(err, ErrReadLimit))
			require.Len(t, gotKeys, i, "the keys before the failing one")
			assert.Equal(t, i, res.Primaries)

			return
		}
		require.Greater(t, len(gotKeys), i, "workers %d: key %v paired", workers, key)
		require.Equal(t, key, gotKeys[i], "workers %d: key %d", workers, i)
		requireStrictEqual(t, singles[i], got[i], "workers %d: key %v", workers, key)
	}
	require.NoError(t, err, "workers %d", workers)
	require.Len(t, gotKeys, len(want), "no other key")
	assert.Equal(t, len(want), res.Primaries)
	assert.Equal(t, int(hourOf(toNs-1)-hourOf(fromNs))+m, res.HoursRead)
}

// FindTransactions over every generated capture pairs exactly the primaries of its range,
// each with the result FindTransaction gives its key over the same source, compared strictly;
// over whole hours, ranges that cut hours, and with lookup budgets small enough to fail some lookups.
func TestFindTransactionsGenerated(t *testing.T) {
	t.Parallel()

	n := 64
	if testing.Short() {
		n = 16
	}
	for seed := range uint64(n) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()

			g := genTxCapture(t, seed)
			snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
			rng := rand.New(rand.NewPCG(seed, 0x6261746368))
			from, to := g.first*hourNs, (g.first+int64(g.hours))*hourNs

			t.Run("whole hours", func(t *testing.T) {
				checkTxRange(t, g, snap, from, to, TxOptions{MaxScopes: g.maxScopes})
			})
			t.Run("cut hours", func(t *testing.T) {
				a := from + rng.Int64N(to-from)
				b := a + 1 + rng.Int64N(to-a)
				checkTxRange(t, g, snap, a, b, TxOptions{MaxScopes: g.maxScopes})
			})
			t.Run("small budgets", func(t *testing.T) {
				checkTxRange(t, g, snap, from, to, TxOptions{MaxScopes: g.maxScopes, MaxConflicts: 1, MaxStateBytes: 4 << 10})
			})
			t.Run("one scope", func(t *testing.T) {
				checkTxRange(t, g, snap, from, to, TxOptions{MaxScopes: 1})
			})
		})
	}
}

// The keys each pack's own read gives (txPerPackKeys), the fallback of txHourKeys for blocks the reference does not model,
// equal the keys the reference read gives, over every hour of every generated capture.
func TestTxPerPackKeys(t *testing.T) {
	t.Parallel()

	for seed := range uint64(32) {
		g := genTxCapture(t, seed)
		snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
		paired := 0
		for h := range snap.scopes {
			from, to := h*hourNs, (h+1)*hourNs
			readers, err := g.src.view().Readers(t.Context(), h)
			require.NoError(t, err)
			require.True(t, txReferenceHolds(readers), "seed %d: hour %d", seed, h)
			want := txRangeKeys(snap, captureLow, from, to)
			assert.Equal(t, want, txPerPackKeys(t, readers, captureLow, h, from, to), "seed %d: hour %d", seed, h)
			paired += len(want)
		}
		require.Positive(t, paired, "seed %d", seed)
	}
}

// Over an hour whose read passes MaxHeldBytes, the batch fails in reading the hour, before it pairs anything,
// as the read of the hour alone fails.
func TestFindTransactionsHourOverMaxHeld(t *testing.T) {
	t.Parallel()

	s := txSource(t, txPack(t, seg0, nil, txBlock(txRecord(10, nil), txReply(11, nil), txRecord(12, nil))))
	key := TxKey{CaptureID: captureLow, Seq: 10, Hour: memTestHour}
	_, err := FindTransaction(t.Context(), s.view(), key, TxOptions{MaxHeldBytes: 1})
	require.ErrorIs(t, err, ErrReadLimit)
	requireBatchAgreesOnError(t, s.view(), key, TxOptions{MaxHeldBytes: 1})
}

// FindTransactions refuses invalid arguments before it calls the source.
func TestFindTransactionsArguments(t *testing.T) {
	t.Parallel()

	src := newMemSource()
	at := time.Unix(0, memTestHour*hourNs)
	ok := TxRange{CaptureID: captureLow, From: at, To: at.Add(time.Hour)}
	fn := func(*TxPairing) error { return nil }
	maxStart := time.Unix(0, MaxTxHour*hourNs)
	for name, tt := range map[string]struct {
		src  PackSource
		r    TxRange
		opts TxRangeOptions
		fn   func(*TxPairing) error
	}{
		"a nil source":         {nil, ok, TxRangeOptions{}, fn},
		"a nil fn":             {src, ok, TxRangeOptions{}, nil},
		"the nil capture":      {src, TxRange{From: ok.From, To: ok.To}, TxRangeOptions{}, fn},
		"an empty range":       {src, TxRange{CaptureID: captureLow, From: at, To: at}, TxRangeOptions{}, fn},
		"an inverted range":    {src, TxRange{CaptureID: captureLow, From: at.Add(time.Hour), To: at}, TxRangeOptions{}, fn},
		"a time outside int64": {src, TxRange{CaptureID: captureLow, From: time.Time{}, To: at}, TxRangeOptions{}, fn},
		"a tail past MaxTxHour": {src, TxRange{CaptureID: captureLow, From: maxStart, To: maxStart.Add(time.Minute)},
			TxRangeOptions{Lookup: TxOptions{MaxScopes: 2}}, fn},
		"an hour below MinTxHour": {src, TxRange{CaptureID: captureLow, From: time.Unix(0, math64Min()), To: time.Unix(0, math64Min()+1)},
			TxRangeOptions{}, fn},
	} {
		before := src.counts()
		_, err := FindTransactions(t.Context(), tt.src, tt.r, tt.opts, tt.fn)
		require.ErrorIs(t, err, ErrInvalidQuery, name)
		assert.Equal(t, before, src.counts(), "%s: no source call", name)
	}

	// The last hour with a tail of one hour is valid.
	_, err := FindTransactions(t.Context(), src, TxRange{CaptureID: captureLow, From: maxStart, To: maxStart.Add(time.Minute)},
		TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}}, fn)
	require.NoError(t, err)
}

func math64Min() int64 { return -1 << 63 }

// FuzzFindTransactionsGenerated runs the check of TestFindTransactionsGenerated on the capture genTxCapture generates from a seed,
// over the range ctl chooses: whole hours, or cut at points drawn from the seed,
// with MaxScopes 1 to 3 or the capture's own, and small lookup budgets when ctl asks for them.
func FuzzFindTransactionsGenerated(f *testing.F) {
	for i := range uint64(8) {
		f.Add(i, uint8(i))
	}

	f.Fuzz(func(t *testing.T, seed uint64, ctl uint8) {
		g := genTxCapture(t, seed)
		snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
		opts := TxOptions{MaxScopes: g.maxScopes}
		if n := int(ctl % 4); n > 0 {
			opts.MaxScopes = n
		}
		if ctl&0x10 != 0 {
			opts.MaxConflicts, opts.MaxStateBytes = 1, 4<<10
		}
		from, to := g.first*hourNs, (g.first+int64(g.hours))*hourNs
		if ctl&0x20 != 0 {
			rng := rand.New(rand.NewPCG(seed, uint64(ctl)))
			from += rng.Int64N(to - from)
			to = from + 1 + rng.Int64N(to-from)
		}
		checkTxRange(t, g, snap, from, to, opts)
	})
}

// The strict form tells apart results that differ only in the order of their gaps, in a gap's text,
// or in a nil slice against an empty one, all of which txComparable lets pass.
func TestStrictFormRejects(t *testing.T) {
	t.Parallel()

	base := TxResult{Outcome: TxNotFound, Incomplete: []TxGap{
		{Reason: TxGapSeqGap, Block: -1, Offset: -1, Err: errors.New("a")},
		{Reason: TxGapOpenWindow, Block: -1, Offset: -1, Err: errors.New("b")},
	}}
	same := cloneTxResult(&base)
	assert.Equal(t, strictForm(&base), strictForm(&same))

	swapped := cloneTxResult(&base)
	swapped.Incomplete[0], swapped.Incomplete[1] = swapped.Incomplete[1], swapped.Incomplete[0]
	text := cloneTxResult(&base)
	text.Incomplete[1].Err = errors.New("c")
	empty := cloneTxResult(&base)
	empty.Records = []TxRecord{}
	for name, r := range map[string]TxResult{"reordered gaps": swapped, "a gap's text": text, "an empty slice for nil": empty} {
		assert.NotEqual(t, strictForm(&base), strictForm(&r), name)
	}
}
