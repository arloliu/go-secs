package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retentionTxSource is a PackSource over a memSource whose Observe and scopes a test changes.
type retentionTxSource struct {
	*memSource
	// observe, when set, runs before each Observe; an error it returns is returned instead of observing.
	observe func(ctx context.Context) error
	// scope, when set, changes each scope the observation answers without an error.
	scope func(hour int64, sc SourceScope) SourceScope
}

var _ PackSource = (*retentionTxSource)(nil)

// retentionTxObservation is an observation of a retentionTxSource.
type retentionTxObservation struct {
	Observation
	scope func(hour int64, sc SourceScope) SourceScope
}

// Observe runs the source's observe hook, then observes the memSource, wrapping the observation when scopes are changed.
func (s *retentionTxSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	if s.observe != nil {
		if err := s.observe(ctx); err != nil {
			return nil, err
		}
	}
	o, err := s.memSource.Observe(ctx, capture, from, to)
	if err != nil || s.scope == nil {
		return o, err
	}

	return &retentionTxObservation{Observation: o, scope: s.scope}, nil
}

// Scope answers the memSource's scope, changed by the source's scope hook.
func (o *retentionTxObservation) Scope(ctx context.Context, hour int64) (SourceScope, error) {
	sc, err := o.Observation.Scope(ctx, hour)
	if err != nil {
		return sc, err
	}

	return o.scope(hour, sc), nil
}

// hookedReaderAt is an io.ReaderAt that calls fn before each read.
type hookedReaderAt struct {
	io.ReaderAt
	fn func()
}

// ReadAt calls fn, then reads.
func (a hookedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	a.fn()

	return a.ReaderAt.ReadAt(p, off)
}

// beforeReads makes every read of the readers of sc, opened already, so a block read, call fn first.
func beforeReads(sc SourceScope, fn func()) SourceScope {
	for _, r := range sc.Readers {
		r.ra = hookedReaderAt{ReaderAt: r.ra, fn: fn}
	}

	return sc
}

// retentionTxHours returns a memSource of three indexed hours of captureLow from memTestHour:
// seqs 11, the primary 12 and its reply 13 in the first, the same-key primaries 14 and 15 in the second, 16 in the third.
func retentionTxHours(t testing.TB) *memSource {
	t.Helper()

	return txHours(t,
		[][]byte{txPackIn(t, seg0, 0, nil, txBlock(txRecord(11, nil), txRecord(12, nil), txReply(13, nil)))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(14, 15)...))},
		[][]byte{txPackIn(t, seg2, 2, nil, txBlock(txSeqs(16)...))},
	)
}

// retentionTxWant returns the result of the lookup of seq 12 over retentionTxHours with maxScopes and no boundary,
// its outcome cleared: what a lookup that ends with an error after its evaluation holds.
func retentionTxWant(t testing.TB, maxScopes int) TxResult {
	t.Helper()

	want, err := findTx(t, t.Context(), retentionTxHours(t), txKeyAt(12), TxOptions{MaxScopes: maxScopes})
	require.NoError(t, err)
	require.NotZero(t, want.Outcome)
	want.Outcome = 0

	return want
}

// requireScopesAsked requires that s was asked for the scope of each of hours, as offsets from memTestHour, once,
// and for no other.
func requireScopesAsked(t testing.TB, s *memSource, hours ...int64) {
	t.Helper()

	want := map[int64]int{}
	for _, h := range hours {
		want[memTestHour+h] = 1
	}
	_, scopes, closes := s.counts()
	require.Equal(t, want, scopes)
	require.Equal(t, 1, closes, "the observation is closed once")
}

// TestFindTransactionRetentionBeforeObserve looks up a primary whose hour is removed,
// or whose provider fails, before Observe:
// the lookup fails without observing, its result zero.
// A boundary at the primary's hour removes nothing, and the lookup returns what it returns without a boundary.
func TestFindTransactionRetentionBeforeObserve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    *testRetention
		err  error
	}{
		{name: "the primary's hour removed", p: newTestRetention(memTestHour + 1), err: ErrRemoved},
		{name: "every hour removed", p: newTestRetention(MaxTxHour + 1), err: ErrRemoved},
		{name: "the provider fails", p: func() *testRetention { p := newTestRetention(0); p.fail(errRetention); return p }(), err: errRetention},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := retentionTxHours(t)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{Retention: tt.p})
			require.ErrorIs(t, err, tt.err)
			if !errors.Is(tt.err, ErrRemoved) {
				require.NotErrorIs(t, err, ErrRemoved)
			}
			assert.Equal(t, TxResult{}, res)
			assert.Equal(t, 1, tt.p.callCount())
			observes, _, _ := s.counts()
			assert.Zero(t, observes, "the source is not observed")
		})
	}

	t.Run("the primary's hour retained", func(t *testing.T) {
		t.Parallel()

		want, err := FindTransaction(t.Context(), retentionTxHours(t), txKeyAt(12), TxOptions{})
		require.NoError(t, err)
		res, err := findTx(t, t.Context(), retentionTxHours(t), txKeyAt(12), TxOptions{Retention: newTestRetention(memTestHour)})
		require.NoError(t, err)
		assert.Equal(t, want, res)
	})
}

// TestFindTransactionRetentionObserveFails fails Observe after the boundary advanced past the primary's hour:
// an I/O failure becomes ErrRemoved, joined after it, or, while the hour is retained, stands;
// a provider failing at that check joins its error after the failure;
// a ctx error and ErrReadLimit are returned as they are, with no check after them.
func TestFindTransactionRetentionObserveFails(t *testing.T) {
	t.Parallel()

	gone := fmt.Errorf("pack gone: %w", ErrObjectNotFound)
	tests := []struct {
		name string
		// fail returns Observe's error once the boundary is advanced by advance; cancel cancels the lookup's ctx.
		fail    func(cancel context.CancelFunc) error
		advance func(p *testRetention)
		errs    []error
		notErrs []error
		calls   int
	}{
		{
			name: "an object gone, the hour removed", fail: func(context.CancelFunc) error { return gone },
			advance: func(p *testRetention) { p.set(memTestHour + 1) }, errs: []error{ErrObjectNotFound, ErrRemoved}, calls: 2,
		},
		{
			name: "an object gone, the hour retained", fail: func(context.CancelFunc) error { return gone },
			advance: func(*testRetention) {}, errs: []error{ErrObjectNotFound}, notErrs: []error{ErrRemoved}, calls: 2,
		},
		{
			name: "an object gone, the provider failing", fail: func(context.CancelFunc) error { return gone },
			advance: func(p *testRetention) { p.fail(errRetention) }, errs: []error{ErrObjectNotFound, errRetention},
			notErrs: []error{ErrRemoved}, calls: 2,
		},
		{
			name: "a budget refusal", fail: func(context.CancelFunc) error { return fmt.Errorf("listing: %w", ErrReadLimit) },
			advance: func(p *testRetention) { p.set(memTestHour + 1) }, errs: []error{ErrReadLimit}, notErrs: []error{ErrRemoved}, calls: 1,
		},
		{
			name: "a cancellation", fail: func(cancel context.CancelFunc) error { cancel(); return context.Canceled },
			advance: func(p *testRetention) { p.set(memTestHour + 1) }, errs: []error{context.Canceled}, notErrs: []error{ErrRemoved}, calls: 1,
		},
		{
			name: "a deadline", fail: func(context.CancelFunc) error { return fmt.Errorf("listing: %w", context.DeadlineExceeded) },
			advance: func(p *testRetention) { p.set(memTestHour + 1) }, errs: []error{context.DeadlineExceeded},
			notErrs: []error{ErrRemoved}, calls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p := newTestRetention(memTestHour)
			s := &retentionTxSource{memSource: retentionTxHours(t), observe: func(context.Context) error {
				tt.advance(p)

				return tt.fail(cancel)
			}}
			res, err := findTx(t, ctx, s, txKeyAt(12), TxOptions{Retention: p})
			for _, e := range tt.errs {
				require.ErrorIs(t, err, e)
			}
			for _, e := range tt.notErrs {
				require.NotErrorIs(t, err, e)
			}
			assert.True(t, strings.HasPrefix(err.Error(), "tracepack: find transaction: observe hours"), "Observe's error comes first: %v", err)
			assert.Equal(t, TxResult{}, res)
			assert.Equal(t, tt.calls, p.callCount())
		})
	}
}

// TestFindTransactionRetentionPrimaryRead advances the boundary past the primary's hour
// at the first block read of its scope:
// the scope read ends with ErrRemoved before it yields a record, the next scope is not asked for,
// and the result holds no record and no searched scope.
func TestFindTransactionRetentionPrimaryRead(t *testing.T) {
	t.Parallel()

	p := newTestRetention(memTestHour)
	s := &retentionTxSource{memSource: retentionTxHours(t), scope: func(hour int64, sc SourceScope) SourceScope {
		if hour != memTestHour {
			return sc
		}

		return beforeReads(sc, func() { p.set(memTestHour + 1) })
	}}
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2, Retention: p})
	require.ErrorIs(t, err, ErrRemoved)
	require.ErrorContains(t, err, fmt.Sprintf("hour %d:", memTestHour))
	assert.Zero(t, res.Outcome)
	assert.Empty(t, res.Records, "no record is yielded after the boundary passed its hour")
	assert.Empty(t, res.Searched)
	requireScopesAsked(t, s.memSource, 0)
}

// TestFindTransactionRetentionLaterRead advances the boundary to the hour after the primary's
// at the first block read of that hour:
// the read covers a retained hour and completes, but the primary's hour is removed.
// With that hour the last scheduled, the check after evaluation ends the lookup with ErrRemoved,
// its result the one found without a boundary, outcome cleared;
// with another hour scheduled, the check before it does, and that hour is not asked for.
func TestFindTransactionRetentionLaterRead(t *testing.T) {
	t.Parallel()

	for _, maxScopes := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d scopes", maxScopes), func(t *testing.T) {
			t.Parallel()

			p := newTestRetention(memTestHour)
			s := &retentionTxSource{memSource: retentionTxHours(t), scope: func(hour int64, sc SourceScope) SourceScope {
				if hour != memTestHour+1 {
					return sc
				}

				return beforeReads(sc, func() { p.set(memTestHour + 1) })
			}}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: maxScopes, Retention: p})
			require.ErrorIs(t, err, ErrRemoved)
			require.ErrorContains(t, err, fmt.Sprintf("hour %d is removed", memTestHour))
			requireScopesAsked(t, s.memSource, 0, 1)
			if maxScopes == 2 {
				assert.Equal(t, retentionTxWant(t, maxScopes), res, "the result keeps what the lookup found, its outcome zero")

				return
			}
			assert.Zero(t, res.Outcome)
			assert.Equal(t, []int64{0, 1}, searchedHours(res.Searched))
			assert.Equal(t, []string{"12@0 primary", "13@0 candidate", "14@1 same-key-primary", "15@1 same-key-primary"},
				recordSeqs(res.Records))
		})
	}
}

// TestFindTransactionRetentionEvaluation advances the boundary past the primary's hour once every scope is read:
// while the observation answers the barriers, and, for a primary without a key, which ends the lookup after its scope,
// while the primary is settled.
// The check before the outcome is returned ends the lookup with ErrRemoved,
// the result the one found without a boundary, outcome cleared.
// For a record that is not a primary, settled while the boundary passes its hour, ErrNotPrimary stands.
func TestFindTransactionRetentionEvaluation(t *testing.T) {
	t.Parallel()

	for _, maxScopes := range []int{1, 2} {
		t.Run(fmt.Sprintf("at the barriers, %d scopes", maxScopes), func(t *testing.T) {
			t.Parallel()

			p := newTestRetention(memTestHour)
			s := retentionTxHours(t)
			s.beforeBarriers = func(context.Context, int64, int64) error {
				p.set(memTestHour + 1)

				return nil
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: maxScopes, Retention: p})
			require.ErrorIs(t, err, ErrRemoved)
			assert.Equal(t, retentionTxWant(t, maxScopes), res)
			_, _, closes := s.counts()
			assert.Equal(t, 1, closes)
		})
	}

	t.Run("a primary without a key", func(t *testing.T) {
		t.Parallel()

		noKey := func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }
		build := func(t testing.TB) *memSource {
			t.Helper()

			return txHours(t, [][]byte{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, noKey), txReply(13, nil)))}, nil)
		}
		want, err := findTx(t, t.Context(), build(t), txKeyAt(12), TxOptions{})
		require.NoError(t, err)
		require.Equal(t, TxIncomplete, want.Outcome)
		want.Outcome = 0

		p := newTestRetention(memTestHour)
		s := build(t)
		res, err := findTransactionWith(t.Context(), s, txKeyAt(12), TxOptions{Retention: p}, func(l *txLookup) {
			l.ticks.step = 1
			l.onTick = func() {
				// The primary's scope is read: the primary is being settled.
				if len(l.reads) == 1 && l.reads[0].readers == nil {
					p.set(memTestHour + 1)
				}
			}
		})
		require.ErrorIs(t, err, ErrRemoved)
		assert.Equal(t, want, res)
		requireScopesAsked(t, s, 0)
	})

	// The lookup's last check comes only after an outcome: ErrNotPrimary, found while the primary is settled, stands.
	t.Run("a record that is not a primary", func(t *testing.T) {
		t.Parallel()

		even := func(r *Record) {
			r.Payload = slices.Clone(r.Payload)
			r.Payload[fieldFunctionOff] = 4
		}
		p := newTestRetention(memTestHour)
		s := txHours(t, [][]byte{txPackIn(t, seg0, 0, nil, txBlock(txRecord(11, nil), txRecord(12, even), txRecord(13, nil)))}, nil)
		advanced := false
		res, err := findTransactionWith(t.Context(), s, txKeyAt(12), TxOptions{Retention: p}, func(l *txLookup) {
			l.ticks.step = 1
			l.onTick = func() {
				// The primary's scope is read: the primary is being settled.
				if len(l.reads) == 1 && l.reads[0].readers == nil {
					p.set(memTestHour + 1)
					advanced = true
				}
			}
		})
		require.True(t, advanced, "the boundary passed the primary's hour while the primary was settled")
		require.ErrorIs(t, err, ErrNotPrimary)
		require.NotErrorIs(t, err, ErrRemoved)
		assert.Equal(t, TxResult{Searched: []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}}}}, res)
		requireScopesAsked(t, s, 0)
	})
}

// TestFindTransactionRetentionPassedScopes advances the boundary past the primary's hour
// while the observation answers the next hour's scope, empty or conflicted,
// which the lookup passes without a check of its own:
// the check before the hour after it, empty, conflicted or holding packs, ends the lookup with ErrRemoved,
// and that hour is not asked for.
func TestFindTransactionRetentionPassedScopes(t *testing.T) {
	t.Parallel()

	for _, mid := range []string{"empty", "conflicted"} {
		for _, last := range []string{"empty", "conflicted", "packs"} {
			t.Run(mid+" then "+last, func(t *testing.T) {
				t.Parallel()

				s := txHours(t, [][]byte{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))}, nil, nil)
				if mid == "conflicted" {
					s.setConflicted(captureLow, memTestHour+1, true)
				}
				switch last {
				case "conflicted":
					s.setConflicted(captureLow, memTestHour+2, true)
				case "packs":
					s.addPack(t, memTestHour+2, txPackIn(t, seg2, 2, nil, txBlock(txSeqs(16)...)))
				default: // an empty hour
				}
				p := newTestRetention(memTestHour)
				s.beforeScope = func(_ context.Context, hour int64) error {
					if hour == memTestHour+1 {
						p.set(memTestHour + 1)
					}

					return nil
				}
				res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3, Retention: p})
				require.ErrorIs(t, err, ErrRemoved)
				require.ErrorContains(t, err, fmt.Sprintf("hour %d:", memTestHour+2))
				assert.Zero(t, res.Outcome)
				requireScopesAsked(t, s, 0, 1)
				searched := []int64{0}
				if mid == "empty" {
					searched = append(searched, 1)
				} else {
					assert.Contains(t, gapReasons(res.Gaps), TxGapConflicted)
				}
				assert.Equal(t, searched, searchedHours(res.Searched))
			})
		}
	}
}

// TestFindTransactionRetentionRemovedScope has the observation report a scheduled hour removed,
// with no boundary given to the lookup:
// the primary's hour, whatever else the scope reports, or the next hour:
// the lookup ends with ErrRemoved, keeping what it found before, and asks for no further scope.
// A removed scope with readers is a source error.
func TestFindTransactionRetentionRemovedScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// hour is the hour reported removed, as an offset from memTestHour.
		hour    int64
		removed func(sc SourceScope) SourceScope
		asked   []int64
		err     error
		text    string
		records []string
	}{
		{name: "the primary's hour", removed: func(SourceScope) SourceScope { return SourceScope{Removed: true} },
			asked: []int64{0}, err: ErrRemoved, text: "the source reports the hour removed"},
		{name: "the primary's hour, indexed and conflicted",
			removed: func(SourceScope) SourceScope { return SourceScope{Removed: true, Indexed: true, Conflicted: true} },
			asked:   []int64{0}, err: ErrRemoved},
		{name: "the next hour", hour: 1, removed: func(SourceScope) SourceScope { return SourceScope{Removed: true, Indexed: true} },
			asked: []int64{0, 1}, err: ErrRemoved, records: []string{"12@0 primary", "13@0 candidate"}},
		{name: "the primary's hour with readers", removed: func(sc SourceScope) SourceScope { sc.Removed = true; return sc },
			asked: []int64{0}, text: "a removed scope comes with readers (1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &retentionTxSource{memSource: retentionTxHours(t), scope: func(hour int64, sc SourceScope) SourceScope {
				if hour != memTestHour+tt.hour {
					return sc
				}

				return tt.removed(sc)
			}}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.Error(t, err)
			require.ErrorContains(t, err, fmt.Sprintf("hour %d: scope: %s", memTestHour+tt.hour, tt.text))
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			} else {
				require.NotErrorIs(t, err, ErrRemoved)
			}
			assert.Zero(t, res.Outcome)
			assert.Equal(t, tt.records, nilIfEmpty(recordSeqs(res.Records)))
			requireScopesAsked(t, s.memSource, tt.asked...)
		})
	}
}

// TestFindTransactionRetentionProviderFails makes the provider fail before the primary's scope,
// during a later scope's read, and while the barriers are answered:
// the lookup ends with the provider's error, wrapped, keeping what it found, and asks for no further scope.
func TestFindTransactionRetentionProviderFails(t *testing.T) {
	t.Parallel()

	t.Run("before the primary's scope", func(t *testing.T) {
		t.Parallel()

		p := newTestRetention(memTestHour)
		s := retentionTxHours(t)
		s.beforeEvidence = func(context.Context) error {
			p.fail(errRetention)

			return nil
		}
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{Retention: p})
		require.ErrorIs(t, err, errRetention)
		require.NotErrorIs(t, err, ErrRemoved)
		assert.Equal(t, TxResult{}, res)
		requireScopesAsked(t, s)
	})
	t.Run("during a later scope's read", func(t *testing.T) {
		t.Parallel()

		p := newTestRetention(memTestHour)
		s := &retentionTxSource{memSource: retentionTxHours(t), scope: func(hour int64, sc SourceScope) SourceScope {
			if hour != memTestHour+1 {
				return sc
			}

			return beforeReads(sc, func() { p.fail(errRetention) })
		}}
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3, Retention: p})
		require.ErrorIs(t, err, errRetention)
		require.NotErrorIs(t, err, ErrRemoved)
		assert.Zero(t, res.Outcome)
		assert.Equal(t, []int64{0}, searchedHours(res.Searched))
		requireScopesAsked(t, s.memSource, 0, 1)
	})
	t.Run("at the barriers", func(t *testing.T) {
		t.Parallel()

		p := newTestRetention(memTestHour)
		s := retentionTxHours(t)
		s.beforeBarriers = func(context.Context, int64, int64) error {
			p.fail(errRetention)

			return nil
		}
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2, Retention: p})
		require.ErrorIs(t, err, errRetention)
		require.NotErrorIs(t, err, ErrRemoved)
		assert.Equal(t, retentionTxWant(t, 2), res)
	})
}

// TestFindTransactionRetentionConfigurations looks up a primary through a store source,
// with the boundary given to the source only, to the lookup only, to both, or to neither,
// while the boundary advances past the primary's hour and its pack is deleted after it:
// never, at the pack's open during Observe, or at its block read during the lookup.
// The source alone reports the hour removed when the deletion meets Observe, and leaves the lookup's read unprotected;
// the lookup alone turns the failure, of Observe or of its read, into ErrRemoved, joined after it;
// with neither, the failure stands.
// Every object opened is closed once.
func TestFindTransactionRetentionConfigurations(t *testing.T) {
	t.Parallel()

	type config struct{ source, lookup bool }
	configs := map[string]config{
		"source only": {source: true}, "lookup only": {lookup: true}, "both": {source: true, lookup: true}, "neither": {},
	}
	type want struct{ removed, gone bool }
	tests := []struct {
		name  string
		wants map[string]want
	}{
		{name: "never", wants: map[string]want{}},
		{name: "during Observe", wants: map[string]want{
			"source only": {removed: true}, "lookup only": {removed: true, gone: true}, "both": {removed: true}, "neither": {gone: true},
		}},
		{name: "during the read", wants: map[string]want{
			"source only": {gone: true}, "lookup only": {removed: true, gone: true}, "both": {removed: true, gone: true}, "neither": {gone: true},
		}},
	}
	build := func(t testing.TB) (*memStore, *fakeCatalog, string) {
		t.Helper()

		store, cat := newMemStore(), newFakeCatalog()
		pack := store.putPack(t, memTestHour, txPack(t, seg0, nil, txBlock(txRecord(11, nil), txRecord(12, nil), txReply(13, nil))))
		cat.setView(captureLow, memTestHour, pack)

		return store, cat, pack.Key
	}
	store, cat, _ := build(t)
	free, err := FindTransaction(t.Context(), newTestStoreSource(t, store, cat, nil), txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.NotZero(t, free.Outcome)

	for _, tt := range tests {
		for name, c := range configs {
			t.Run(tt.name+", "+name, func(t *testing.T) {
				t.Parallel()

				store, cat, key := build(t)
				p := newTestRetention(memTestHour)
				var srcRet, lookupRet Retention
				if c.source {
					srcRet = p
				}
				if c.lookup {
					lookupRet = p
				}
				gone := func() {
					p.set(memTestHour + 1)
					store.remove(key)
				}
				var src PackSource = newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) { o.Retention = srcRet })
				switch tt.name {
				case "during Observe":
					store.openHook = func(_ context.Context, k string) (bool, Object, error) {
						if k == key {
							gone()
						}

						return false, nil, nil
					}
				case "during the read":
					src = afterObserveSource{PackSource: src, after: func() {
						store.readHook = func(k string, _ int64, _ int) error {
							if k == key {
								gone()
							}

							return nil
						}
					}}
				default: // nothing is deleted
				}
				res, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: 1, Retention: lookupRet})
				requireClosedOnce(t, store)
				w, ok := tt.wants[name]
				if !ok {
					require.NoError(t, err)
					assert.Equal(t, free, res)

					return
				}
				require.Error(t, err)
				assert.Zero(t, res.Outcome)
				assert.Equal(t, w.removed, errors.Is(err, ErrRemoved), "ErrRemoved: %v", err)
				assert.Equal(t, w.gone, errors.Is(err, ErrObjectNotFound), "ErrObjectNotFound: %v", err)
				if w.removed && w.gone {
					assert.True(t, strings.Index(err.Error(), key) < strings.Index(err.Error(), "is removed"),
						"the failure comes before the removal: %v", err)
				}
			})
		}
	}
}
