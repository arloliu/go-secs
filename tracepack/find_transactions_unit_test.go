package tracepack

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txBatchHour returns the range of the hour memTestHour + h.
func txBatchHour(h int64) TxRange {
	from := time.Unix(0, (memTestHour+h)*hourNs)

	return TxRange{Capture: captureLow, From: from, To: from.Add(time.Hour)}
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
	res, err := FindTransactions(t.Context(), s, txBatchHour(0), TxRangeOptions{Lookup: TxOptions{MaxScopes: 2}}, func(p *TxPairing) error {
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
		res, err := FindTransactions(t.Context(), s.view(), txBatchHour(0), TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}},
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

	t.Run("ctx cancelled during an hour's read", func(t *testing.T) {
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
	opts := TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}}
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

	t.Run("evicted hours release their charge", func(t *testing.T) {
		t.Parallel()

		var held []int64
		_, err := findTransactionsWith(t.Context(), s.view(), TxRange{Capture: captureLow, From: txBatchHour(0).From, To: txBatchHour(3).To},
			TxRangeOptions{Lookup: TxOptions{MaxScopes: 2}}, func(*TxPairing) error { return nil },
			func(b *txBatch) {
				b.evicting = func(rec *txHourRecord) { held = append(held, b.cache.held) }
				b.filled = func(rec *txHourRecord) {
					require.LessOrEqual(t, len(b.hours), 2, "at most MaxScopes hours held")
				}
			})
		require.NoError(t, err)
		assert.Len(t, held, 4)
	})
}

// fingerprint hashes every part of rec a lookup could change, through the pointers it holds.
func fingerprint(rec *txHourRecord) [32]byte {
	h := sha256.New()
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(h, format, a...) }
	p("%d %p %v %v %v|", rec.hour, rec.readers, rec.flags, rec.reservations, rec.keys)
	for _, r := range rec.readers {
		p("%p ", r)
	}
	for i := range rec.items {
		it := &rec.items[i]
		p("%+v %x %x %d %d %t|", it.Record, it.Record.Payload, it.HeaderExtra, it.Pack, it.Block, it.Conflict)
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
			p("%v %v %v %v %v %v|", c.CaptureID, c.SeqFirst, c.SeqLast, c.TimeStart, c.TimeEnd, c.Unknown)
			if c.SeqFirst != nil {
				p("%d", *c.SeqFirst)
			}
			for _, u := range c.Unknown {
				p("%x", u.Value)
			}
		}
		if b := d.Boundary; b != nil {
			p("%+v", *b)
			if b.GapStart != nil {
				p("%d", *b.GapStart)
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

// Every hour record a batch holds is unchanged from its fill to its eviction,
// though every pairing passed to fn is mutated in full: the lookups and the caller share no mutable storage with it.
func TestFindTransactionsRecordsUnchanged(t *testing.T) {
	t.Parallel()

	for seed := range uint64(24) {
		g := genTxCapture(t, seed)
		filled := map[int64][32]byte{}
		evicted := 0
		r := TxRange{Capture: captureLow, From: time.Unix(0, g.first*hourNs), To: time.Unix(0, (g.first+int64(g.hours))*hourNs)}
		_, err := findTransactionsWith(t.Context(), g.src.view(), r, TxRangeOptions{Lookup: TxOptions{MaxScopes: g.maxScopes}},
			func(p *TxPairing) error {
				mutateTxResult(&p.Result)
				return nil
			},
			func(b *txBatch) {
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
		require.Positive(t, evicted, "seed %d", seed)
	}
}
