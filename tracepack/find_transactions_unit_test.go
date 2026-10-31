package tracepack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txBatchHour returns the range of the hour memTestHour + h.
func txBatchHour(h int64) TxRange {
	from := time.Unix(0, (memTestHour+h)*hourNs)

	return TxRange{CaptureID: captureLow, From: from, To: from.Add(time.Hour)}
}

// txKeyless edits a primary so that its System Bytes are unavailable: its lookup ends after its own hour, without a key.
func txKeyless(r *Record) {
	r.FieldValidity &^= FieldValiditySystemBytes
}

// FindTransactions calls the source once per hour, in ascending order, and passes every primary of an hour,
// a keyless one included, once the last hour its lookups read is read, in ascending seq.
func TestFindTransactionsScheduling(t *testing.T) {
	t.Parallel()

	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, txBlock(txRecord(10, nil), txReply(11, nil), txRecord(12, txKeyless), txRecord(13, nil)))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil)))},
	)
	var calls []int64
	s.beforeReaders = func(_ context.Context, hour int64) error {
		calls = append(calls, hour)
		return nil
	}

	var keys []uint64
	res, err := FindTransactions(t.Context(), s, txBatchHour(0), TxRangeOptions{Lookup: TxOptions{MaxHours: 2}}, func(p *TxPairing) error {
		assert.Len(t, calls, 2, "seq %d passed once both hours are read", p.Key.Seq)
		keys = append(keys, p.Key.Seq)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{memTestHour, memTestHour + 1}, calls)
	assert.Equal(t, []uint64{10, 12, 13}, keys)
	assert.Equal(t, TxRangeResult{HoursRead: 2, Primaries: 3}, res)

	t.Run("one scope", func(t *testing.T) {
		t.Parallel()

		var got []uint64
		res, err := FindTransactions(t.Context(), s.view(), txBatchHour(0), TxRangeOptions{Lookup: TxOptions{MaxHours: 1}},
			func(p *TxPairing) error {
				got = append(got, p.Key.Seq)
				return nil
			})
		require.NoError(t, err)
		assert.Equal(t, []uint64{10, 12, 13}, got)
		assert.Equal(t, TxRangeResult{HoursRead: 1, Primaries: 3}, res)
	})

	t.Run("no primary in range", func(t *testing.T) {
		t.Parallel()

		res, err := FindTransactions(t.Context(), s.view(), txBatchHour(1), TxRangeOptions{}, func(*TxPairing) error {
			t.Fatal("no primary in hour memTestHour + 1")
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, TxRangeResult{HoursRead: 2}, res)
	})
}

// An error of the source or of a later hour's read stops the batch at once:
// no primary still pending is passed, a keyless one included, though its own result needs no later hour.
func TestFindTransactionsErrors(t *testing.T) {
	t.Parallel()

	files := func(t *testing.T) *memSource {
		return txHours(t,
			[][]byte{txPack(t, seg0, nil, txBlock(txRecord(10, txKeyless), txRecord(12, nil)))},
			[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(13, nil)))},
		)
	}
	boom := errors.New("boom")

	t.Run("the source fails at a later hour", func(t *testing.T) {
		t.Parallel()

		s := files(t)
		s.beforeReaders = func(_ context.Context, hour int64) error {
			if hour == memTestHour+1 {
				return boom
			}
			return nil
		}
		res, err := FindTransactions(t.Context(), s, txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error {
			t.Fatal("no pairing after an error")
			return nil
		})
		require.ErrorIs(t, err, boom)
		assert.Contains(t, err.Error(), fmt.Sprintf("hour %d", memTestHour+1))
		assert.Equal(t, TxRangeResult{HoursRead: 1}, res)
	})

	t.Run("fn fails", func(t *testing.T) {
		t.Parallel()

		calls := 0
		res, err := FindTransactions(t.Context(), files(t), txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error {
			calls++
			return boom
		})
		require.Equal(t, boom, err, "fn's error as is")
		assert.Equal(t, 1, calls)
		assert.Equal(t, TxRangeResult{HoursRead: 2, Primaries: 1}, res)
	})

	t.Run("ctx cancelled by fn", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res, err := FindTransactions(ctx, files(t), txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error {
			cancel()
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, TxRangeResult{HoursRead: 2, Primaries: 1}, res, "no pairing after the cancellation")
	})

	t.Run("fn's error before ctx's", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		_, err := FindTransactions(ctx, files(t), txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error {
			cancel()
			return boom
		})
		require.Equal(t, boom, err)
	})

	t.Run("ctx cancelled by the last fn", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		n := 0
		res, err := FindTransactions(ctx, files(t), txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error {
			if n++; n == 2 {
				cancel()
			}
			return nil
		})
		require.ErrorIs(t, err, context.Canceled, "the last check before success")
		assert.Equal(t, 2, res.Primaries)
	})

	t.Run("ctx cancelled before", func(t *testing.T) {
		t.Parallel()

		s := files(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		res, err := FindTransactions(ctx, s, txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error { return nil })
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, res)
		assert.Empty(t, s.counts(), "no source call")
	})

	t.Run("ctx cancelled once an hour is read", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		res, err := findTransactionsWith(ctx, files(t), txBatchHour(0), TxRangeOptions{}, func(*TxPairing) error { return nil },
			func(b *txBatch) {
				b.ticks.step = 1
				b.filled = func(*txHourRecord) { cancel() }
			})
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, TxRangeResult{HoursRead: 1}, res)
	})
}

// MaxCachedBytes admits an hour whose record charges exactly the limit and refuses one byte less, before it reads on.
func TestFindTransactionsCacheBudget(t *testing.T) {
	t.Parallel()

	s := txSource(t, txPack(t, seg0, nil, txBlock(txRecord(10, nil), txReply(11, nil), txRecord(12, nil))))
	opts := TxRangeOptions{Lookup: TxOptions{MaxHours: 1}}
	var charge int64
	_, err := findTransactionsWith(t.Context(), s, txBatchHour(0), opts, func(*TxPairing) error { return nil },
		func(b *txBatch) { b.filled = func(rec *txHourRecord) { charge = rec.charge } })
	require.NoError(t, err)
	require.Positive(t, charge)

	opts.MaxCachedBytes = charge
	res, err := FindTransactions(t.Context(), s, txBatchHour(0), opts, func(*TxPairing) error { return nil })
	require.NoError(t, err, "a charge of exactly the limit")
	assert.Equal(t, 2, res.Primaries)

	opts.MaxCachedBytes = charge - 1
	res, err = FindTransactions(t.Context(), s, txBatchHour(0), opts, func(*TxPairing) error {
		t.Fatal("no pairing over the limit")
		return nil
	})
	require.ErrorIs(t, err, ErrReadLimit)
	assert.Contains(t, err.Error(), fmt.Sprintf("hour %d", memTestHour))
	assert.Equal(t, TxRangeResult{}, res)
}

// Over every generated capture, each hour record holds the HSMS header of each item's record, as Record.HSMSHeader decodes it,
// and charges exactly what its contents recount to,
// the cache holds the sum of the records held, at most MaxHours of them, and nothing once the batch returns.
func TestFindTransactionsCacheRecount(t *testing.T) {
	t.Parallel()

	for seed := range uint64(32) {
		g := genTxCapture(t, seed)
		r := TxRange{CaptureID: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
		var batch *txBatch
		filled, evicted := 0, 0
		res, err := findTransactionsWith(t.Context(), g.src.view(), r, TxRangeOptions{Lookup: TxOptions{MaxHours: g.maxScopes}},
			func(*TxPairing) error { return nil },
			func(b *txBatch) {
				batch = b
				held := func() int64 {
					var n int64
					for _, rec := range b.hours {
						n += rec.charge
					}
					return n
				}
				b.filled = func(rec *txHourRecord) {
					filled++
					require.Equal(t, txCacheRecount(b, rec), rec.charge, "seed %d: hour %d recount", seed, rec.hour)
					require.Len(t, rec.meta, len(rec.items), "seed %d: hour %d: the flags of each item", seed, rec.hour)
					for i := range rec.items {
						require.Equal(t, rec.items[i].Record.HSMSHeader(), rec.items[i].HSMSHeader, "seed %d: hour %d: the header of item %d", seed, rec.hour, i)
					}
					require.Equal(t, held(), b.cache.held, "seed %d: held after filling hour %d", seed, rec.hour)
					require.LessOrEqual(t, len(b.hours), max(g.maxScopes, 1), "seed %d: hours held", seed)
				}
				b.evicting = func(rec *txHourRecord) {
					evicted++
					require.Equal(t, held(), b.cache.held, "seed %d: held before dropping hour %d", seed, rec.hour)
				}
			})
		require.NoError(t, err, "seed %d", seed)
		assert.Equal(t, res.HoursRead, filled, "seed %d", seed)
		assert.Equal(t, filled, evicted, "seed %d: every hour dropped", seed)
		assert.Zero(t, batch.cache.held, "seed %d: every charge released", seed)
		assert.Empty(t, batch.hours, "seed %d", seed)
	}
}

// A MaxHours far beyond the hours read reserves nothing ahead: with a cancelled context or a one-byte cache,
// FindTransactions returns without allocating for the hours it would schedule.
func TestFindTransactionsNoAllocationAhead(t *testing.T) {
	src := newMemSource()
	r := TxRange{CaptureID: captureLow, From: time.Unix(0, 0), To: time.Unix(0, int64(time.Hour))}
	opts := TxRangeOptions{Lookup: TxOptions{MaxHours: 2_000_000}, MaxCachedBytes: 1}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, ctx := range map[string]context.Context{"a cancelled ctx": cancelled, "a one-byte cache": t.Context()} {
		allocs := testing.AllocsPerRun(5, func() {
			_, err := FindTransactions(ctx, src, r, opts, func(*TxPairing) error { return nil })
			require.Error(t, err)
		})
		var ms0, ms1 runtime.MemStats
		runtime.ReadMemStats(&ms0)
		_, _ = FindTransactions(ctx, src, r, opts, func(*TxPairing) error { return nil })
		runtime.ReadMemStats(&ms1)
		assert.Less(t, ms1.TotalAlloc-ms0.TotalAlloc, uint64(64<<10), "%s: bytes allocated", name)
		assert.Less(t, allocs, 50.0, "%s: allocations", name)
	}
}

// fingerprint hashes every part of rec a lookup could change, through the pointers it holds.
func fingerprint(rec *txHourRecord) [32]byte {
	h := sha256.New()
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(h, format, a...) }
	p("%d %p %+v %v %v|", rec.hour, rec.readers, rec.meta, rec.reservations, rec.keys)
	for _, r := range rec.readers {
		p("%p ", r)
	}
	for i := range rec.items {
		it := &rec.items[i]
		p("%+v %x %x %d %d %t|", it.Record, it.Record.Payload, it.HeaderExtra, it.Input, it.Block, it.Conflict)
		if rr := it.Redaction; rr != nil {
			p("%t %+v|", rr.WhollyMasked, rr.Defects)
			for _, e := range rr.Entries {
				p("%d %v %v %x %s %x %x %t|", e.Seq, e.ItemPath != nil && *e.ItemPath != "", e.MaskedRanges, e.RawMaskedRanges, e.Domain, e.Digest, e.SIV, e.HasSIV)
			}
		}
	}
	for _, d := range rec.res.Incomplete {
		p("%d %d %d %d %v|", d.Reason, d.Pack, d.Block, d.Offset, d.Err)
		if c := d.Coverage; c != nil {
			p("%v %t|", c.Unknown, c.Unknown == nil)
			if c.CaptureID != nil {
				p("c%x", *c.CaptureID)
			}
			for _, v := range []*uint64{c.SeqFirst, c.SeqLast} {
				if v != nil {
					p("s%d", *v)
				}
			}
			for _, v := range []*int64{c.TimeStart, c.TimeEnd} {
				if v != nil {
					p("t%d", *v)
				}
			}
			for _, u := range c.Unknown {
				p("%x", u.Value)
			}
		}
		if b := d.Boundary; b != nil {
			p("%+v", *b)
			for _, v := range []*int64{b.GapStart, b.GapEnd} {
				if v != nil {
					p("g%d", *v)
				}
			}
		}
	}
	for _, c := range rec.res.Conflicts {
		p("%v %d %v|", c.CaptureID, c.Seq, c.Versions)
	}
	for _, fe := range rec.res.FooterErrs {
		p("%d %v|", fe.Pack, fe.Err)
	}

	return [32]byte(h.Sum(nil))
}

// Every hour record a batch holds is unchanged from its fill to its eviction, read by lookups on several workers,
// though every pairing passed to fn is mutated in full: the lookups and the caller share no mutable storage with it.
func TestFindTransactionsRecordsUnchanged(t *testing.T) {
	t.Parallel()

	for seed := range uint64(24) {
		g := genTxCapture(t, seed)
		filled := map[int64][32]byte{}
		evicted := 0
		var batch *txBatch
		r := TxRange{CaptureID: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
		// On several workers, the records are read by lookups running beside the callback's checks.
		_, err := findTransactionsWith(t.Context(), g.src.view(), r, TxRangeOptions{Lookup: TxOptions{MaxHours: g.maxScopes}},
			func(p *TxPairing) error {
				mutateTxResult(&p.Result)
				for _, rec := range batch.hours {
					require.Equal(t, filled[rec.hour], fingerprint(rec), "seed %d: hour %d unchanged after %v is mutated", seed, rec.hour, p.Key)
				}
				return nil
			},
			func(b *txBatch) {
				batch = b
				b.workers = 4
				b.filled = func(rec *txHourRecord) { filled[rec.hour] = fingerprint(rec) }
				b.evicting = func(rec *txHourRecord) {
					evicted++
					require.Equal(t, filled[rec.hour], fingerprint(rec), "seed %d: hour %d unchanged", seed, rec.hour)
				}
			})
		if err != nil {
			require.ErrorIs(t, err, ErrReadLimit, "seed %d", seed)
			continue
		}
		require.Equal(t, len(filled), evicted, "seed %d: every hour checked when it is dropped, the tail hours included", seed)
	}
}

// Each primary's lookup over the replay checks ctx as many times as its lookup over the source:
// the batch's own checks stay out of the lookup's count.
func TestFindTransactionsLookupTicks(t *testing.T) {
	t.Parallel()

	for seed := range uint64(48) {
		g := genTxCapture(t, seed)
		snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
		from, to := g.first*hourNs, (g.first+int64(g.hours))*hourNs
		opts := TxOptions{MaxHours: g.maxScopes}
		if seed%2 == 1 {
			opts.MaxConflicts, opts.MaxStateBytes = 1, 4<<10 // budgets small enough to fail some lookups
		}
		live, failed := map[TxKey]int{}, map[TxKey]int{}
		for _, key := range txRangeKeys(snap, captureLow, from, to) {
			n := 0
			_, err := findTransactionWith(t.Context(), g.src.view(), key, opts, func(l *txLookup) {
				l.ticks.step = 1
				l.onTick = func() { n++ }
			})
			if err == nil {
				live[key] = n
			} else {
				failed[key] = n
			}
		}

		n, compared := 0, 0
		_, err := findTransactionsWith(t.Context(), g.src.view(), TxRange{CaptureID: captureLow, From: time.Unix(0, from), To: time.Unix(0, to)},
			TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
				want, ok := live[p.Key]
				require.True(t, ok, "seed %d: %v succeeded alone", seed, p.Key)
				require.Equal(t, want, n, "seed %d: checks of ctx in the lookup of %v", seed, p.Key)
				compared++
				return nil
			},
			func(b *txBatch) {
				b.lookupPrep = func(l *txLookup) {
					n = 0
					l.ticks.step = 1
					l.onTick = func() { n++ }
				}
			})
		if err == nil {
			assert.Len(t, live, compared, "seed %d", seed)
			assert.Empty(t, failed, "seed %d: no lookup fails alone when the batch succeeds", seed)
			continue
		}
		// The batch stops in the lookup it names, at the event where that lookup fails alone.
		// A live read that fails returns its partial Result, which the lookup still maps before it returns;
		// a replay returns none, by design, since a failed lookup's result is never reported,
		// so the replayed lookup checks ctx no more often than the live one, and fails with the same error.
		var seq uint64
		var hour int64
		_, serr := fmt.Sscanf(err.Error(), "tracepack: find transactions: primary seq %d of hour %d:", &seq, &hour)
		require.NoError(t, serr, "seed %d: %v", seed, err)
		key := TxKey{CaptureID: captureLow, Seq: seq, Hour: hour}
		want, ok := failed[key]
		require.True(t, ok, "seed %d: %v fails alone too", seed, key)
		assert.LessOrEqual(t, n, want, "seed %d: checks of ctx in the failing lookup of %v", seed, key)
		assert.Positive(t, n, "seed %d: the failing lookup of %v ran", seed, key)
	}
}

// txCancelAfter is a context whose Err reports cancellation from its call number at on,
// and that records whether it has reported it.
type txCancelAfter struct {
	context.Context
	calls, at int
	reported  bool
}

func (c *txCancelAfter) Err() error {
	c.calls++
	if c.calls > c.at {
		c.reported = true
		return context.Canceled
	}

	return nil
}

// Cancelled at every check of ctx in turn, a batch returns ctx's error without passing a pairing after it,
// whether the check falls in a fill, an enumeration, a sort, a lookup, before a delivery or before success.
func TestFindTransactionsCancelEverywhere(t *testing.T) {
	t.Parallel()

	g := genTxCapture(t, 3)
	r := TxRange{CaptureID: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
	opts := TxRangeOptions{Lookup: TxOptions{MaxHours: g.maxScopes}}
	prep := func(b *txBatch) { b.ticks.step = 1 }

	total := &txCancelAfter{Context: t.Context(), at: math.MaxInt}
	full, err := findTransactionsWith(total, g.src.view(), r, opts, func(*TxPairing) error { return nil }, prep)
	require.NoError(t, err)
	require.Positive(t, full.Primaries)
	// With a step of 1 the batch's own counter checks ctx once per item of every hour it reads and once per conflict version,
	// apart from the lookups' checks and from the checks between hours:
	// a check removed from the fill shrinks its count below the bound.
	snap := newTxSnapshot(t, g.src, captureLow, g.first, g.first+int64(g.hours+txGenHours))
	own := 0
	_, err = findTransactionsWith(t.Context(), g.src.view(), r, opts, func(*TxPairing) error { return nil }, func(b *txBatch) {
		b.ticks.step = 1
		b.onTick = func() { own++ }
	})
	require.NoError(t, err)
	bound := 0
	for h := g.first; h < g.first+int64(full.HoursRead); h++ {
		sc := snap.scopes[h]
		bound += len(sc.items)
		for _, c := range sc.conflicts {
			bound += len(c.Versions)
		}
	}
	require.Positive(t, bound)
	require.GreaterOrEqual(t, own, bound, "the batch's own checks of ctx")

	for at := 0; at <= total.calls; at++ {
		ctx := &txCancelAfter{Context: t.Context(), at: at}
		src := &txGuardedSource{src: g.src.view(), cancelled: func() bool { return ctx.reported }, t: t}
		paired := 0
		res, err := findTransactionsWith(ctx, src, r, opts, func(*TxPairing) error {
			require.False(t, ctx.reported, "at %d: no pairing after ctx reported its cancellation", at)
			paired++
			return nil
		}, prep)
		if at == total.calls {
			require.NoError(t, err, "at %d: past the last check", at)
			continue
		}
		require.ErrorIs(t, err, context.Canceled, "at %d", at)
		require.Equal(t, paired, res.Primaries, "at %d: the pairings passed", at)
		// An hour counts as read once its fill completes: an error wrapped with an hour cut that hour's fill short,
		// after the source was called for it; any other error came after the last hour the source was called for was read.
		read := src.calls
		if strings.HasPrefix(err.Error(), "tracepack: find transactions: hour ") {
			read--
		}
		require.Equal(t, read, res.HoursRead, "at %d: hours read against source calls (%v)", at, err)
	}
}

// txCancelOn is a context whose Err reports cancellation once on is set,
// or, when countdown is set, from the countdown-th call of Err on, counting this one.
type txCancelOn struct {
	context.Context
	on        bool
	countdown int
}

func (c *txCancelOn) Err() error {
	if c.countdown > 0 {
		if c.countdown--; c.countdown == 0 {
			c.on = true
		}
	}
	if c.on {
		return context.Canceled
	}

	return nil
}

// Cancelled at each check of ctx the batch makes for its own work while it reads an hour into its cache,
// a batch returns ctx's error wrapped with the hour, having read no hour and passed no pairing,
// and calls the source for no later hour;
// those checks are exactly one for each item, conflict reservation, defect, coverage Unknown entry and conflict version of the hour's read
// and one for each footer error and each key of the hour, counted from the fixture:
// an hour of diagnostics only, packs whose footers are not valid, a conflict whose versions have many holders,
// keys to enumerate and sort, and none selected by the range.
func TestFindTransactionsCancelInFill(t *testing.T) {
	t.Parallel()

	later := [][]byte{txPackIn(t, seg0, 1, nil, txBlock(txReply(14, nil)))}
	other := func(r *Record) {
		r.Payload = bytes.Clone(r.Payload)
		r.Payload[len(r.Payload)-1]++
	}
	unknown := []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{1}}, {Tag: 0x0051, Type: 7, Value: []byte{2}},
		{Tag: 0x0052, Type: 7, Value: []byte{3}}, {Tag: 0x0053, Type: 7, Value: []byte{4}}}
	coverage := func(m *PackMeta) {
		for i := range uint64(3) {
			m.Coverage = append(m.Coverage, Coverage{SeqFirst: new(100 * i), SeqLast: new(100*i + 5), Unknown: unknown})
		}
	}
	conflict := make([][]byte, 0, 5)
	conflict = append(conflict, txPack(t, seg4, nil, txBlock(txRecord(10, other))))
	for _, id := range []UUID{seg0, seg1, seg2, seg3} {
		conflict = append(conflict, txPack(t, id, nil, txBlock(txRecord(10, nil))))
	}
	footers := make([][]byte, 0, 3)
	for i, id := range []UUID{seg0, seg1, seg2} {
		footers = append(footers, invalidFooterFile(t, txPack(t, id, nil, txBlock(txReply(11+2*uint64(i), nil)))))
	}
	keys := [][]byte{txPack(t, seg0, nil, txBlock(txRecord(10, nil), txReply(11, nil), txRecord(12, nil), txReply(13, nil),
		txRecord(14, nil), txRecord(15, nil), txRecord(16, nil), txReply(17, nil)))}

	for name, tt := range map[string]struct {
		hour [][]byte
		// from is the start of the range after the start of the hour.
		from time.Duration
		// ticks counts the items, reservations, defects, Unknown entries, footer errors and conflict versions of the read, and the keys.
		ticks int
		// sorts is set when the hour has keys to sort.
		sorts bool
	}{
		"diagnostics only":           {hour: [][]byte{txPack(t, seg0, coverage, nil)}, ticks: 3 + 3*len(unknown)},
		"footer errors":              {hour: footers, ticks: 3 + 3},
		"a conflict of many holders": {hour: conflict, ticks: 2 + 1 + 2 + 1},
		"keys to enumerate and sort": {hour: keys, ticks: 8 + 5, sorts: true},
		"no key selected":            {hour: keys, from: time.Microsecond, ticks: 8},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.hour, later)
			r := TxRange{CaptureID: captureLow, From: time.Unix(0, memTestHour*hourNs).Add(tt.from), To: time.Unix(0, (memTestHour+2)*hourNs)}
			opts := TxRangeOptions{Lookup: TxOptions{MaxHours: 1}}
			ticks, n := -1, 0
			_, err := findTransactionsWith(t.Context(), s.view(), r, opts, func(*TxPairing) error { return nil }, func(b *txBatch) {
				b.ticks.step = 1
				b.onTick = func() { n++ }
				b.filled = func(rec *txHourRecord) {
					if rec.hour == memTestHour {
						ticks = n
					}
				}
			})
			require.NoError(t, err)
			require.Equal(t, tt.ticks, ticks, "the batch's checks of ctx in reading the hour")

			for at := range ticks {
				ctx := &txCancelOn{Context: t.Context()}
				src := &txGuardedSource{src: s.view(), cancelled: func() bool { return ctx.on }, t: t}
				paired, n := 0, 0
				res, err := findTransactionsWith(ctx, src, r, opts, func(*TxPairing) error {
					paired++
					return nil
				}, func(b *txBatch) {
					b.ticks.step = 1
					b.onTick = func() {
						if n == at {
							ctx.on = true
						}
						n++
					}
				})
				require.ErrorIs(t, err, context.Canceled, "at %d", at)
				assert.True(t, strings.HasPrefix(err.Error(), fmt.Sprintf("tracepack: find transactions: hour %d: ", memTestHour)), "at %d: %v", at, err)
				assert.Equal(t, TxRangeResult{}, res, "at %d", at)
				assert.Zero(t, paired, "at %d: no pairing", at)
				assert.Equal(t, 1, src.calls, "at %d: the source called for the hour cancelled only", at)
			}
			if !tt.sorts {
				return
			}
			// The sort of the keys cannot check ctx, so ctx is checked before and after it, still within the hour's read:
			// cancelled at the second or third check from the last key's, the fill fails as cancelled at that key.
			for _, d := range []int{2, 3} {
				ctx := &txCancelOn{Context: t.Context()}
				src := &txGuardedSource{src: s.view(), cancelled: func() bool { return ctx.on }, t: t}
				n := 0
				res, err := findTransactionsWith(ctx, src, r, opts, func(*TxPairing) error {
					t.Fatal("no pairing")
					return nil
				}, func(b *txBatch) {
					b.ticks.step = 1
					b.onTick = func() {
						if n++; n == ticks {
							ctx.countdown = d
						}
					}
				})
				require.EqualError(t, err, fmt.Sprintf("tracepack: find transactions: hour %d: context canceled", memTestHour), "check %d", d)
				assert.Equal(t, TxRangeResult{}, res, "check %d", d)
			}
		})
	}
}

// Cancelled at the last check of ctx before each pairing is passed, a batch returns ctx's error, wrapped by no key,
// having passed exactly the pairings before it, and calls the source for no later hour;
// cancelled at the first check after each pairing is passed, before the next lookup or the next hour,
// it returns that error too, having passed that pairing.
func TestFindTransactionsCancelBeforeDelivery(t *testing.T) {
	t.Parallel()

	for _, workers := range txWorkerCounts {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			t.Parallel()

			txCancelAroundDelivery(t, workers)
		})
	}
}

// txCancelAroundDelivery is TestFindTransactionsCancelBeforeDelivery with workers.
func txCancelAroundDelivery(t *testing.T, workers int) {
	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, txBlock(txRecord(10, nil), txReply(11, nil), txRecord(12, txKeyless), txRecord(13, nil)))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil)))},
	)
	r := TxRange{CaptureID: captureLow, From: time.Unix(0, memTestHour*hourNs), To: time.Unix(0, (memTestHour+2)*hourNs)}
	opts := TxRangeOptions{Lookup: TxOptions{MaxHours: 1}}
	dry := &txCancelAfter{Context: t.Context(), at: math.MaxInt}
	var before []int
	_, err := findTransactionsWith(dry, s.view(), r, opts, func(*TxPairing) error {
		before = append(before, dry.calls)
		return nil
	}, txWorkers(workers))
	require.NoError(t, err)
	require.Len(t, before, 3)

	for k, c := range before {
		ctx := &txCancelAfter{Context: t.Context(), at: c - 1}
		src := &txGuardedSource{src: s.view(), cancelled: func() bool { return ctx.reported }, t: t}
		paired := 0
		res, err := findTransactionsWith(ctx, src, r, opts, func(*TxPairing) error {
			paired++
			return nil
		}, txWorkers(workers))
		require.EqualError(t, err, "tracepack: find transactions: context canceled", "pairing %d", k)
		assert.Equal(t, k, paired, "pairing %d", k)
		assert.Equal(t, TxRangeResult{HoursRead: 1, Primaries: k}, res, "pairing %d", k)
		assert.Equal(t, 1, src.calls, "pairing %d: no later hour", k)

		// fn makes no check, so the first check after it is the one after the c-th.
		ctx = &txCancelAfter{Context: t.Context(), at: c}
		src = &txGuardedSource{src: s.view(), cancelled: func() bool { return ctx.reported }, t: t}
		paired = 0
		res, err = findTransactionsWith(ctx, src, r, opts, func(*TxPairing) error {
			paired++
			return nil
		}, txWorkers(workers))
		require.EqualError(t, err, "tracepack: find transactions: context canceled", "after pairing %d", k)
		assert.Equal(t, TxRangeResult{HoursRead: 1, Primaries: k + 1}, res, "after pairing %d", k)
		assert.Equal(t, k+1, paired, "after pairing %d", k)
		assert.Equal(t, 1, src.calls, "after pairing %d: no later hour", k)
	}
}

// txGuardedSource fails its test when called once cancelled reports that its ctx reported cancellation, and counts its calls.
type txGuardedSource struct {
	src       PackSource
	cancelled func() bool
	t         *testing.T
	calls     int
}

func (s *txGuardedSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	require.False(s.t, s.cancelled(), "no source call after ctx reported its cancellation")
	s.calls++

	return s.src.Readers(ctx, hour)
}

// Every pairing is the caller's own, its lookup run on one of several workers:
// kept after fn returns, each still holds what it held when it was passed,
// after later pairings, the eviction of its hours and the end of the call, and no two share a TxPairing.
func TestFindTransactionsPairingsOwned(t *testing.T) {
	t.Parallel()

	for seed := range uint64(16) {
		g := genTxCapture(t, seed)
		r := TxRange{CaptureID: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
		var kept []*TxPairing
		var forms []txStrictForm
		_, err := findTransactionsWith(t.Context(), g.src.view(), r, TxRangeOptions{Lookup: TxOptions{MaxHours: g.maxScopes}},
			func(p *TxPairing) error {
				for _, q := range kept {
					require.NotSame(t, q, p, "seed %d: a fresh TxPairing", seed)
				}
				kept, forms = append(kept, p), append(forms, strictForm(&p.Result))
				return nil
			}, txWorkers(4))
		require.NoError(t, err, "seed %d", seed)
		for i, p := range kept {
			requireStrictEqual(t, forms[i], strictForm(&p.Result), "seed %d: pairing %v kept", seed, p.Key)
			single, err := FindTransaction(t.Context(), g.src.view(), p.Key, TxOptions{MaxHours: g.maxScopes})
			require.NoError(t, err)
			requireStrictEqual(t, strictForm(&single), strictForm(&p.Result), "seed %d: pairing %v kept", seed, p.Key)
		}
	}
}
