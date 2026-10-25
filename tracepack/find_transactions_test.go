package tracepack

import (
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
				g.Coverage = cloneCoverage(g.Coverage)
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

// requireStrictEqual requires got to equal want in the strict form;
// require.Equal compares by reflect.DeepEqual, so nil and empty slices stay apart, and the errors are already texts.
func requireStrictEqual(t testing.TB, want, got txStrictForm, msgAndArgs ...any) {
	t.Helper()

	require.Equal(t, want, got, msgAndArgs...)
}

// requireBatchAgrees runs FindTransactions over the hour of key with opts, over src, a source whose answers do not change,
// and requires every pairing to equal FindTransaction's result for its key, compared strictly,
// and the pairing of key, when the hour pairs it, to equal want too.
func requireBatchAgrees(t testing.TB, src PackSource, key TxKey, opts TxOptions, want *TxResult) {
	t.Helper()

	if key.Hour < MinTxHour || key.Hour > MaxTxHour || key.Capture.IsZero() {
		return
	}
	from := time.Unix(0, key.Hour*hourNs)
	_, err := FindTransactions(t.Context(), src, TxRange{Capture: key.Capture, From: from, To: from.Add(time.Hour)},
		TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
			single, serr := FindTransaction(t.Context(), src, p.Key, opts)
			require.NoError(t, serr, "key %v alone", p.Key)
			requireStrictEqual(t, strictForm(&single), strictForm(&p.Result), "batch pairing of %v", p.Key)
			if p.Key == key {
				requireStrictEqual(t, strictForm(want), strictForm(&p.Result), "batch pairing of the test's key %v", key)
			}

			return nil
		})
	if errors.Is(err, ErrInvalidQuery) {
		return // a tail past MaxTxHour, which the single lookup refuses too
	}
	require.NoError(t, err, "the batch over the hour of %v", key)
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
		g := &res.Incomplete[i]
		for j := range g.Hours {
			g.Hours[j]++
		}
		if g.Pack != nil {
			g.Pack[0] ^= 0xFF
		}
		if g.Seq != nil {
			*g.Seq ^= 1
		}
		if g.Coverage != nil && g.Coverage.SeqFirst != nil {
			*g.Coverage.SeqFirst ^= 1
		}
		if g.Coverage != nil {
			for j := range g.Coverage.Unknown {
				for k := range g.Coverage.Unknown[j].Value {
					g.Coverage.Unknown[j].Value[k] ^= 0xFF
				}
			}
		}
		if g.Boundary != nil {
			g.Boundary.Seq ^= 1
			if g.Boundary.GapStart != nil {
				*g.Boundary.GapStart ^= 1
			}
		}
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
			r := &it.rec
			if sc.captures[it.pack] != capture || r.Kind != KindData || r.TSUTCNs < fromNs || r.TSUTCNs >= toNs || hourOf(r.TSUTCNs) != h {
				continue
			}
			if hh := r.HSMSHeader(); hh.Available&FieldValidityFunction == 0 || hh.Function%2 == 0 {
				continue
			}
			seqs = append(seqs, r.Seq)
		}
		slices.Sort(seqs)
		for _, s := range slices.Compact(seqs) {
			keys = append(keys, TxKey{Capture: capture, Seq: s, Hour: h})
		}
	}

	return keys
}

// checkTxRange runs FindTransactions over g for [fromNs, toNs) with opts, mutating every pairing once it is recorded,
// and requires it to pair exactly the keys of the reference, in order, each with the result FindTransaction gives,
// or to fail as the first lookup that fails.
func checkTxRange(t *testing.T, g *txGen, snap *txSnapshot, fromNs, toNs int64, opts TxOptions) {
	t.Helper()

	want := txRangeKeys(snap, captureLow, fromNs, toNs)
	var gotKeys []TxKey
	var got []txStrictForm
	before := g.src.counts()
	res, err := FindTransactions(t.Context(), g.src, TxRange{Capture: captureLow, From: time.Unix(0, fromNs), To: time.Unix(0, toNs)},
		TxRangeOptions{Lookup: opts}, func(p *TxPairing) error {
			gotKeys = append(gotKeys, p.Key)
			got = append(got, strictForm(&p.Result))
			mutateTxResult(&p.Result)

			return nil
		})

	m := opts.MaxScopes
	if m <= 0 {
		m = DefaultTxMaxScopes
	}
	for h, n := range g.src.counts() {
		require.LessOrEqual(t, n-before[h], 1, "hour %d read once", h)
	}

	for i, key := range want {
		single, serr := FindTransaction(t.Context(), g.src, key, opts)
		if serr != nil {
			require.Error(t, err, "key %v fails alone", key)
			assert.Equal(t, fmt.Sprintf("tracepack: find transactions: primary seq %d of hour %d: %v", key.Seq, key.Hour, serr), err.Error())
			assert.Equal(t, errors.Is(serr, ErrReadLimit), errors.Is(err, ErrReadLimit))
			require.Len(t, gotKeys, i, "the keys before the failing one")
			assert.Equal(t, i, res.Primaries)

			return
		}
		require.Greater(t, len(gotKeys), i, "key %v paired", key)
		require.Equal(t, key, gotKeys[i], "key %d", i)
		requireStrictEqual(t, strictForm(&single), got[i], "key %v", key)
	}
	require.NoError(t, err)
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

// FindTransactions refuses invalid arguments before it calls the source.
func TestFindTransactionsArguments(t *testing.T) {
	t.Parallel()

	src := newMemSource()
	at := time.Unix(0, memTestHour*hourNs)
	ok := TxRange{Capture: captureLow, From: at, To: at.Add(time.Hour)}
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
		"an empty range":       {src, TxRange{Capture: captureLow, From: at, To: at}, TxRangeOptions{}, fn},
		"an inverted range":    {src, TxRange{Capture: captureLow, From: at.Add(time.Hour), To: at}, TxRangeOptions{}, fn},
		"a time outside int64": {src, TxRange{Capture: captureLow, From: time.Time{}, To: at}, TxRangeOptions{}, fn},
		"a tail past MaxTxHour": {src, TxRange{Capture: captureLow, From: maxStart, To: maxStart.Add(time.Minute)},
			TxRangeOptions{Lookup: TxOptions{MaxScopes: 2}}, fn},
		"an hour below MinTxHour": {src, TxRange{Capture: captureLow, From: time.Unix(0, math64Min()), To: time.Unix(0, math64Min()+1)},
			TxRangeOptions{}, fn},
	} {
		before := src.counts()
		_, err := FindTransactions(t.Context(), tt.src, tt.r, tt.opts, tt.fn)
		require.ErrorIs(t, err, ErrInvalidQuery, name)
		assert.Equal(t, before, src.counts(), "%s: no source call", name)
	}

	// The last hour with a tail of one hour is valid.
	_, err := FindTransactions(t.Context(), src, TxRange{Capture: captureLow, From: maxStart, To: maxStart.Add(time.Minute)},
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
