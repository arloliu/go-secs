package tracepack

import (
	"bytes"
	"context"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txNote returns an annotation record of seq at blockTestHour + seq in epoch txTestEpoch, carrying payload:
// a record that plays no role in a lookup.
func txNote(seq uint64, payload []byte) Record {
	return Record{
		Seq: seq, TSUTCNs: blockTestHour + int64(seq), Epoch: txTestEpoch, Kind: KindAnnotation, Dir: DirLocal,
		DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
	}
}

// txTinyReply returns an equipment-to-host data record of seq at blockTestHour + seq + dt without payload:
// none of its HSMS header fields is available, so it is a possible reply to txRecord's primary.
func txTinyReply(seq uint64, dt int64) Record {
	r := txReply(seq, nil)
	r.TSUTCNs += dt
	r.Payload = nil
	r.SetCapturedFieldValidity()

	return r
}

// txStateRecount returns the charge of the state of the lookup l, recounted from that state alone:
// each scope read (its view's packs, its runs, the versions it buffers or kept and the conflict gaps it holds,
// its repeated seqs' runs and the versions its second pass buffers),
// the primary's versions and every other record in the result, the conflicts across scope reads with their hours,
// every gap (a gap of a conflict across scope reads without its Hours, which are the conflict's),
// the coverage entries, the footer errors, the listed conflicts, the ordering-uncertain epochs, the capture-boundary records,
// and the versions the primary's scope yielded before the primary, with their seqs' runs and later versions, while they are buffered.
// It holds after any lookup, also one that failed, since nothing is added to the state before it is charged.
func txStateRecount(l *txLookup) int64 {
	var n int64
	versions := func(vs []TxRecord) {
		for i := range vs {
			n += versionCost(&vs[i].Record, vs[i].HeaderExtra)
		}
	}
	gaps := func(gs []TxGap, cross map[int]bool) {
		for i := range gs {
			g := gs[i]
			if cross[i] {
				g.Hours = nil
			}
			n += gapCost(&g)
		}
	}
	for _, rd := range l.reads {
		n += scopeCost(len(rd.packs)) + int64(len(rd.runs.runs))*txRunCharge
		versions(rd.group.versions)
		versions(rd.kept)
		versions(rd.reread)
		n += int64(len(rd.repeats.runs)) * txRunCharge
		for i := range rd.gaps {
			n += gapCost(&rd.gaps[i].gap)
		}
	}
	// The versions of the primary's seq in its own scope are in Records once settled; before, or on ErrNotPrimary, only in primary.
	versions(l.primary)
	for i := range l.res.Records {
		r := &l.res.Records[i]
		if r.Hour != l.key.Hour || r.Record.Seq != l.key.Seq {
			versions(l.res.Records[i : i+1])
		}
	}
	cross := map[int]bool{}
	for _, c := range l.cross {
		n += txCrossCharge + int64(len(c.hours))*txHourCharge
		if c.gap >= 0 {
			cross[c.gap] = true
		}
	}
	gaps(l.res.Gaps, cross)
	for _, c := range l.coverage {
		n += txPendingCoverageCharge + coverageCost(c.cov)
	}
	n += int64(len(l.res.FooterErrs)) * txPackErrorCharge
	for i := range l.res.Conflicts {
		n += conflictCost(&l.res.Conflicts[i])
	}
	n += int64(len(l.uncertain))*txUncertainCharge + int64(len(l.boundaries))*txBoundaryRecordCharge
	n += int64(len(l.conflicted)) * txConflictedCharge
	versions(l.earlyVersions)
	versions(l.laterVersions)
	n += int64(len(l.earlySeqs.runs)) * txRunCharge

	return n
}

// requireStateRecount requires that the charge of the lookup l's state equals its recount (txStateRecount).
func requireStateRecount(t testing.TB, l *txLookup) {
	t.Helper()

	require.Equal(t, txStateRecount(l), l.state.used, "the charge of the lookup's state")
}

// findTx runs FindTransaction with ctx, src, key and opts,
// then, when the lookup started, requires that the charge of its state equals its recount.
func findTx(t testing.TB, ctx context.Context, src PackSource, key TxKey, opts TxOptions) (TxResult, error) {
	t.Helper()

	var l *txLookup
	res, err := findTransactionWith(ctx, src, key, opts, func(lk *txLookup) { l = lk })
	if l != nil {
		requireStateRecount(t, l)
	}

	return res, err
}

// txReadersSource is a memSource whose observations answer scope key.Hour of txKeyAt with readers, as given, nil ones included.
type txReadersSource struct {
	*memSource
	readers []*Reader
	// conflicted marks that scope conflicted, beside its readers.
	conflicted bool
}

// txReadersObservation is an observation of a txReadersSource.
type txReadersObservation struct {
	Observation
	readers    []*Reader
	conflicted bool
}

// Observe observes the memSource, wrapping the observation.
func (s *txReadersSource) Observe(ctx context.Context, capture UUID, from, to int64) (Observation, error) {
	o, err := s.memSource.Observe(ctx, capture, from, to)
	if err != nil {
		return nil, err
	}

	return &txReadersObservation{Observation: o, readers: s.readers, conflicted: s.conflicted}, nil
}

// Scope answers scope memTestHour with the source's readers, as an indexed scope, conflicted when the source says so,
// and every other hour as the memSource does.
func (o *txReadersObservation) Scope(ctx context.Context, hour int64) (SourceScope, error) {
	if hour != memTestHour {
		return o.Observation.Scope(ctx, hour)
	}

	return SourceScope{Readers: o.readers, Indexed: true, Conflicted: o.conflicted}, nil
}

// TestFindTransactionScopeChargedFirst reads a scope whose view has more packs than MaxStateBytes admits:
// the scope's charge fails before any of its readers is looked at, so a nil reader among them is not reported,
// and with room for the scope, a nil reader fails the read with the scope's charge released.
func TestFindTransactionScopeChargedFirst(t *testing.T) {
	t.Parallel()

	r := mustOpen(t, txPack(t, seg0, nil, txBlock(txSeqs(12)...)), ReaderOptions{})
	tests := []struct {
		name    string
		readers []*Reader
		max     int64
		err     error
		text    string
	}{
		{name: "more packs than the state admits", readers: make([]*Reader, 10000), max: scopeCost(10000) - 1, err: ErrReadLimit,
			text: "hour 497222: the lookup's state of 0 bytes and"},
		{name: "a nil reader", readers: []*Reader{r, nil}, max: scopeCost(2), text: "hour 497222: scope: reader 1 is nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &txReadersSource{memSource: txSource(t, true), readers: tt.readers}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1, MaxStateBytes: tt.max})
			require.ErrorContains(t, err, tt.text)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
			}
			assert.Empty(t, res.Searched)
			_, _, closes := s.counts()
			assert.Equal(t, 1, closes)
		})
	}
}

// findTxState runs FindTransaction with opts over s for the primary at seq 12, keeping the lookup's state.
func findTxState(t testing.TB, s *memSource, opts TxOptions) (TxResult, *txLookup, error) {
	t.Helper()

	var l *txLookup
	res, err := findTransactionWith(t.Context(), s, txKeyAt(12), opts, func(lk *txLookup) { l = lk })
	requireStateRecount(t, l)

	return res, l, err
}

// requireStateLimit requires that a lookup over the source that source returns
// fits in MaxStateBytes limit, reading every scope,
// and fails with ErrReadLimit with one byte less, its result so far without an outcome,
// the observation closed once each time.
func requireStateLimit(t *testing.T, source func(t testing.TB) *memSource, opts TxOptions, limit int64) {
	t.Helper()

	s := source(t)
	opts.MaxStateBytes = limit
	_, err := findTx(t, t.Context(), s, txKeyAt(12), opts)
	require.NoError(t, err, "MaxStateBytes %d fits", limit)
	_, _, closes := s.counts()
	require.Equal(t, 1, closes)

	s = source(t)
	opts.MaxStateBytes = limit - 1
	res, err := findTx(t, t.Context(), s, txKeyAt(12), opts)
	require.ErrorIs(t, err, ErrReadLimit, "MaxStateBytes %d", limit-1)
	require.ErrorContains(t, err, "exceeds MaxStateBytes")
	assert.Equal(t, TxOutcome(0), res.Outcome)
	_, _, closes = s.counts()
	require.Equal(t, 1, closes)
}

// TestTxRunsChargeMillions inserts millions of one-seq runs into a run set:
// each run is charged txRunCharge, the budget is used exactly, and one more run fails with ErrReadLimit, adding nothing.
func TestTxRunsChargeMillions(t *testing.T) {
	t.Parallel()

	const n = 2_000_000
	b := txBudget{max: n * txRunCharge}
	s := txRuns{runs: make([]txRun, 0, n)}
	for i := range uint64(n) {
		if err := s.insert(2*i, &b); err != nil {
			require.NoError(t, err, "seq %d", 2*i)
		}
	}
	require.Len(t, s.runs, n)
	require.Equal(t, b.max, b.used)

	// A seq that joins a run is free; one that starts a run is not.
	require.NoError(t, s.insert(1, &b))
	require.Len(t, s.runs, n-1, "1 joins 0 and 2")
	require.Equal(t, b.max-txRunCharge, b.used)
	require.NoError(t, s.insert(2*n+1, &b))
	err := s.insert(2*n+3, &b)
	require.ErrorIs(t, err, ErrReadLimit)
	require.Len(t, s.runs, n)
	require.Equal(t, b.max, b.used)
}

// TestFindTransactionStateBudget charges a lookup's state against MaxStateBytes:
// the charge of each lookup is computed from the charges of what it holds,
// the lookup succeeds with exactly that budget, and fails with ErrReadLimit with one byte less.
// No record bounds the window of any of these lookups, so each holds a TxGapOpenWindow gap,
// each version of a possible reply a TxGapUnavailable gap, and a coverage entry that meets the window a TxGapCoverage gap.
func TestFindTransactionStateBudget(t *testing.T) {
	t.Parallel()

	primary := txRecord(12, nil)
	primaryCost := versionCost(&primary, nil)
	openWindow := gapCost(&TxGap{})
	unavailable := gapCost(&TxGap{Hours: []int64{memTestHour}})
	const runs = 30_000
	const replies = 5_000
	const versions = 64
	unknown := []RawEntry{
		{Tag: 0x0050, Type: 7, Value: bytes.Repeat([]byte{0xAA}, 20_000)},
		{Tag: 0x0051, Type: 7, Value: bytes.Repeat([]byte{0xBB}, 30_000)},
	}
	cov := Coverage{SeqFirst: new(uint64(100)), SeqLast: new(uint64(200)), Unknown: unknown}

	tests := []struct {
		name   string
		source func(t testing.TB) *memSource
		// want is the charge of the lookup's state.
		want int64
	}{
		{
			name: "one-seq runs without payload",
			source: func(t testing.TB) *memSource {
				steps := make([]footerTestStep, 0, runs+1)
				steps = append(steps, footerTestStep{rec: primary})
				for i := range uint64(runs) {
					steps = append(steps, footerTestStep{rec: txNote(14+2*i, nil)})
				}

				return txSource(t, true, txPack(t, seg0, nil, steps))
			},
			want: scopeCost(1) + primaryCost + (runs+1)*txRunCharge + openWindow,
		},
		{
			name: "many tiny possible replies",
			source: func(t testing.TB) *memSource {
				recs := make([]Record, 0, replies+1)
				recs = append(recs, primary)
				for i := range uint64(replies) {
					recs = append(recs, txTinyReply(13+i, 0))
				}

				return txSource(t, true, txPack(t, seg0, nil, txBlock(recs...)))
			},
			want: scopeCost(1) + primaryCost + txRunCharge + replies*txRecordCharge + openWindow + replies*unavailable,
		},
		{
			name: "one conflict of many versions",
			source: func(t testing.TB) *memSource {
				// Each pack holds its own version of 13, a possible reply, so that every version is kept.
				files := [][]byte{txPack(t, UUID{0x70}, nil, txBlock(primary, txTinyReply(13, 0)))}
				for i := 1; i < versions; i++ {
					files = append(files, txPack(t, UUID{0x70, byte(i)}, nil, txBlock(txTinyReply(13, int64(i)))))
				}

				return txSource(t, true, files...)
			},
			want: scopeCost(versions) + primaryCost + txRunCharge + versions*txRecordCharge +
				gapCost(&TxGap{Hours: []int64{memTestHour}}) + txConflictedCharge + txConflictCharge +
				versions*(txVersionCharge+txPackCharge) + openWindow + versions*unavailable,
		},
		{
			name: "a coverage entry with large unknown values",
			source: func(t testing.TB) *memSource {
				return txSource(t, true, txPack(t, seg0, func(m *PackMeta) { m.Coverage = []Coverage{cov} },
					txBlock(primary)))
			},
			// The entry meets the unbounded window (12, ∞): a coverage gap holds a second copy.
			want: scopeCost(1) + primaryCost + txRunCharge + txPendingCoverageCharge +
				txCoverageCharge + 2*txRawEntryCharge + 50_000 + openWindow +
				gapCost(&TxGap{Hours: []int64{memTestHour}, Coverage: &cov}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, l, err := findTxState(t, tt.source(t), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			require.Equal(t, tt.want, l.state.used, "the charge of %d records, %d gaps, %d conflicts",
				len(res.Records), len(res.Gaps), len(res.Conflicts))
			requireStateLimit(t, tt.source, TxOptions{MaxScopes: 1}, tt.want)
		})
	}
}

// TestFindTransactionStateBudgetReleases reads a seq group of two large versions that play no role, cloned as a conflict,
// then a large reply: the group's charge is released when it is dropped,
// so a budget that holds the group, but not the group and the reply, lets the reply be kept.
func TestFindTransactionStateBudgetReleases(t *testing.T) {
	t.Parallel()

	primary := txRecord(12, nil)
	note := txNote(13, bytes.Repeat([]byte{1}, 40_000))
	other := txNote(13, bytes.Repeat([]byte{2}, 40_000))
	reply := txReply(14, txFrame(func([]byte) {}))
	reply.Payload = append(reply.Payload, bytes.Repeat([]byte{3}, 60_000)...)
	source := func(t testing.TB) *memSource {
		return txSource(t, true,
			txPack(t, seg0, nil, txBlock(primary, note, reply)),
			txPack(t, seg1, nil, txBlock(other)))
	}

	// The conflict at 13 adds its gap and notes its seq.
	base := scopeCost(2) + versionCost(&primary, nil) + txRunCharge + gapCost(&TxGap{Hours: []int64{memTestHour}}) +
		txConflictedCharge
	group := versionCost(&note, nil) + versionCost(&other, nil)
	kept := versionCost(&reply, nil)
	listed := txConflictCharge + 2*(txVersionCharge+txPackCharge)
	// No record bounds the window.
	evaluated := gapCost(&TxGap{})
	require.LessOrEqual(t, base+kept+listed+evaluated, base+group, "the reply, the conflict and the open window fit once the group is released")

	res, l, err := findTxState(t, source(t), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"12@0 primary", "14@0 candidate"}, recordSeqs(res.Records))
	require.Equal(t, base+kept+listed+evaluated, l.state.used)

	// The peak is reached when the group's second version is cloned.
	requireStateLimit(t, source, TxOptions{MaxScopes: 1}, base+group)
}

// TestFindTransactionCancelSteps cancels a lookup at each step:
// before Observe, before Evidence, before the first Scope, while Scope opens the readers, during a scope read,
// and, through the checks every 4096 iterations of the lookup's loops, during the identity insertions of a read,
// the mapping of its status, the search for coverage at a missing primary, the comparison of the primary's copies,
// the probes of the earlier reads for a seq, the commit of a read's conflict gaps, the marking of the kept records,
// and each loop of the evaluation: the sort, the search for the window's end,
// the search for a missing seq and its probes of the reads, the flags of the kept records,
// the coverage entries, the epoch barrier and the epoch's closing record, the call of Barriers and its boundaries,
// whether a stop bounds the window, the capture-boundaries and their look-up in the evidence,
// the evidence closures and the versions at a closure's seq,
// and the outcome's look at the gaps:
// each returns ctx's error with the result so far, no outcome, and the observation closed once when it was opened.
func TestFindTransactionCancelSteps(t *testing.T) {
	t.Parallel()

	twoHours := func(t testing.TB) *memSource {
		return txHours(t,
			[][]byte{txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12, 13)...))},
			[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...))})
	}
	// evaluated holds a window (12, 17) that misses 13 and holds a possible reply at 15:
	// its evaluation adds a seq gap and an unavailable gap.
	evaluated := func(t testing.TB, _ context.CancelFunc) *memSource {
		return txHours(t,
			[][]byte{txPackIn(t, seg0, 0, nil, txBlock(
				txRecord(12, nil), txReply(14, nil), txReply(15, func(r *Record) { r.FieldValidity &^= FieldValiditySystemBytes }),
				txRecord(17, nil)))},
			[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...))})
	}
	// complete holds a window (12, 14) bounded by a clean stop of the epoch, a gap boundary of the epoch at 13,
	// a coverage entry that meets the window, and a stop-unclean boundary in another epoch whose gap meets the hours read:
	// every loop of the completeness gaps runs, and the evaluation adds a coverage, a barrier and a capture-boundary gap.
	complete := func(t testing.TB, _ context.CancelFunc) *memSource {
		s := txHours(t,
			[][]byte{txPackIn(t, seg0, 0, func(m *PackMeta) { m.Coverage = []Coverage{{}} }, txBlock(
				txRecord(12, nil), txBoundaryAt(t, 13, txTestEpoch, BoundaryKindGap, nil, nil),
				txBoundaryAt(t, 14, txTestEpoch, BoundaryKindStop, nil, nil)))},
			[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...))})
		s.addBoundary(txUnclean(captureLow, 100, 2, new(blockTestHour), nil))

		return s
	}
	// noteConflict holds two versions of an annotation at 13, a seq group that plays no role, dropped once 14 is read.
	noteConflict := func(t testing.TB, _ context.CancelFunc) *memSource {
		return txHours(t, [][]byte{
			txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txNote(13, []byte{1}), txRecord(14, nil))),
			txPackIn(t, seg1, 0, nil, txBlock(txNote(13, []byte{2}))),
		})
	}
	tests := []struct {
		name string
		// source returns the source, its hooks set with cancel.
		source func(t testing.TB, cancel context.CancelFunc) *memSource
		// prep, when set, prepares the lookup's state with cancel; t is the subtest's.
		prep func(t *testing.T, l *txLookup, cancel context.CancelFunc)
		// cancelled cancels ctx before the lookup.
		cancelled bool
		observes  int
		scopes    map[int64]int
		searched  int
		// records is the number of records in the result.
		records int
		// gaps is the number of gaps in the result.
		gaps int
		// err is part of the error's text.
		err string
		// seq is the primary's seq; zero means 12.
		seq uint64
		// in, when set, names the method whose check of ctx is cancelled: every iteration is checked,
		// and ctx is cancelled at the first check that method makes.
		in string
		// unmarked requires that no record is marked as a conflict.
		unmarked bool
	}{
		{name: "before Observe", cancelled: true, err: "find transaction: context canceled", source: func(t testing.TB, _ context.CancelFunc) *memSource {
			return twoHours(t)
		}},
		{name: "before Evidence", observes: 1, err: "find transaction: context canceled", source: func(t testing.TB, cancel context.CancelFunc) *memSource {
			s := twoHours(t)
			s.duringObserve = cancel

			return s
		}},
		{name: "before the first Scope", observes: 1, err: "find transaction: context canceled", source: func(t testing.TB, cancel context.CancelFunc) *memSource {
			s := twoHours(t)
			s.beforeEvidence = func(context.Context) error {
				cancel()
				return nil
			}

			return s
		}},
		{name: "while Scope opens the readers", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 1, records: 2, err: "hour 497223: scope:",
			source: func(t testing.TB, cancel context.CancelFunc) *memSource {
				s := twoHours(t)
				s.beforeScope = func(_ context.Context, hour int64) error {
					if hour == memTestHour+1 {
						cancel()
					}

					return nil
				}

				return s
			}},
		{name: "during a scope read", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 1, records: 2, err: "hour 497223: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource { return twoHours(t) },
			prep: func(t *testing.T, l *txLookup, cancel context.CancelFunc) {
				l.ticks.step = 1
				l.onTick = func() {
					// The read of the second scope has begun once its run set is added.
					if len(l.reads) == 2 {
						cancel()
					}
				}
			}},
		{name: "during the identity insertions of a read", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				steps := make([]footerTestStep, 0, 2*txCtxStep+1)
				steps = append(steps, footerTestStep{rec: txRecord(12, nil)})
				for i := range uint64(2 * txCtxStep) {
					steps = append(steps, footerTestStep{rec: txNote(14+2*i, nil)})
				}

				return txSource(t, true, txPack(t, seg0, nil, steps))
			},
			prep: func(t *testing.T, l *txLookup, cancel context.CancelFunc) {
				l.onTick = func() {
					// The first check comes after txCtxStep iterations:
					// the one reader's pack_id, the primary and txCtxStep-2 records above it.
					require.Len(t, l.reads, 1)
					require.Len(t, l.reads[0].runs.runs, txCtxStep-2)
					cancel()
				}
			}},
		{name: "during the marking of the kept records", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 3, gaps: 1, unmarked: true, err: "find transaction: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				// 13 is a conflict across the two scope reads.
				return txHours(t,
					[][]byte{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))},
					[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(13, nil)))})
			},
			prep: func(t *testing.T, l *txLookup, cancel context.CancelFunc) {
				l.ticks.step = 1
				l.onTick = func() {
					// Once the conflict's gap is added, the next loop marks the kept records.
					if c := l.cross[13]; c != nil && c.gap >= 0 {
						cancel()
					}
				}
			}},
		{name: "while mapping a read's status", in: "grow", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				return txHours(t, [][]byte{txPackIn(t, seg0, 0, func(m *PackMeta) { m.Coverage = []Coverage{{}} },
					txBlock(txSeqs(12, 13)...))})
			}},
		{name: "while looking for coverage at a missing primary", in: "coverageAtPrimary", observes: 1,
			scopes: map[int64]int{memTestHour: 1}, searched: 1, err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				return txHours(t, [][]byte{txPackIn(t, seg0, 0, func(m *PackMeta) {
					m.Coverage = []Coverage{{SeqFirst: new(uint64(100)), SeqLast: new(uint64(200))}}
				}, txBlock(txSeqs(11, 13)...))})
			}},
		{name: "while comparing copies of the primary", in: "distinctVersions", seq: 15, observes: 1,
			scopes: map[int64]int{memTestHour: 1}, searched: 1, gaps: 1, err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				// seg0's misindexed block and seg1 yield two copies of 15 uncompared.
				return txSource(t, true, txMisindexed(t, 15), txPack(t, seg1, nil, changed(txBlock(txSeqs(15, 20)...), 1, 15)))
			}},
		{name: "while probing the earlier reads for a seq", in: "crossConflict", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 1, records: 2, err: "hour 497223: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource { return twoHours(t) }},
		{name: "while sorting the kept records", in: "sortRecords", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 5, err: "find transaction: context canceled", source: evaluated},
		{name: "while finding the window", in: "finalWindow", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 5, err: "find transaction: context canceled", source: evaluated},
		{name: "while finding a missing seq", in: "firstMissing", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 5, err: "find transaction: context canceled", source: evaluated},
		{name: "while probing the reads for a missing seq", in: "visitedFrom", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 5, err: "find transaction: context canceled",
			source: evaluated},
		{name: "while flagging the kept records", in: "flagRecords", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 5, gaps: 1, err: "find transaction: context canceled", source: evaluated},
		{name: "while deciding the outcome", in: "decide", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 5, gaps: 2, err: "find transaction: context canceled", source: evaluated},
		{name: "while matching the coverage entries", in: "coverageGaps", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 3, err: "find transaction: context canceled", source: complete},
		{name: "while looking for an epoch barrier", in: "epochBarrierGaps", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 1,
			err: "find transaction: context canceled", source: complete},
		{name: "while looking for the epoch's closing record", in: "epochClosed", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 1,
			err: "find transaction: context canceled", source: complete},
		{name: "while Barriers answers", observes: 1, scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
			searched: 2, records: 3, gaps: 1, err: "find transaction: barriers: context canceled",
			source: func(t testing.TB, cancel context.CancelFunc) *memSource {
				s := complete(t, cancel)
				s.beforeBarriers = func(ctx context.Context, _, _ int64) error {
					cancel()
					return ctx.Err()
				}

				return s
			}},
		{name: "while checking the barriers", in: "timeBarrierGaps", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 1,
			err: "find transaction: context canceled", source: complete},
		{name: "while finding whether a stop of the epoch bounds the window", in: "epochStopBounds", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 2,
			err: "find transaction: context canceled", source: complete},
		{name: "while looking for capture-boundaries", in: "captureBoundaryGaps", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 2,
			err: "find transaction: context canceled", source: complete},
		{name: "while looking up a boundary read in the evidence", in: "evidenceHasBoundary", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 3,
			err: "find transaction: context canceled", source: complete},
		{name: "while checking the evidence closures", in: "contradictionGaps", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 3,
			err: "find transaction: context canceled", source: complete},
		{name: "while checking a closure's seq", in: "checkClaim", observes: 1,
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1}, searched: 2, records: 3, gaps: 3,
			err: "find transaction: context canceled", source: complete},
		{name: "while taking the readers' pack_ids", in: "readScope", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled", source: func(t testing.TB, _ context.CancelFunc) *memSource { return twoHours(t) }},
		{name: "while looking for packs not evaluated", in: "mapScope", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled", source: func(t testing.TB, _ context.CancelFunc) *memSource { return twoHours(t) }},
		{name: "while charging a coverage entry", in: "coverageCharge", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				unknown := []RawEntry{{Tag: 0x0050, Type: 7, Value: []byte{0xAA}}}
				return txHours(t, [][]byte{txPackIn(t, seg0, 0, func(m *PackMeta) { m.Coverage = []Coverage{{Unknown: unknown}} },
					txBlock(txSeqs(12, 13)...))})
			}},
		{name: "while charging a listed conflict", in: "conflictCharge", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				return txHours(t, [][]byte{
					txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12, 13)...)),
					txPackIn(t, seg1, 0, nil, changed(txBlock(txSeqs(13)...), 1, 13)),
				})
			}},
		{name: "while looking for a conflict at the primary", in: "distinctVersions", observes: 1, scopes: map[int64]int{memTestHour: 1},
			searched: 1, err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				// One version of the primary: nothing to compare, yet the versions are scanned for a listed conflict.
				return txHours(t, [][]byte{txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12, 13)...))})
			}},
		{name: "while marking the versions of a conflicting primary", in: "settleConflict", observes: 1,
			scopes: map[int64]int{memTestHour: 1}, searched: 1, err: "hour 497222: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				steps := txBlock(txSeqs(12, 13)...)
				return txHours(t, [][]byte{txPackIn(t, seg0, 0, nil, steps), txPackIn(t, seg1, 0, nil, changed(steps, 1, 12))})
			}},
		{name: "while dropping a seq group", in: "endGroup", observes: 1, scopes: map[int64]int{memTestHour: 1},
			err: "hour 497222: context canceled", source: noteConflict},
		{name: "while adding a read's conflict gaps", in: "commitRead", observes: 1, scopes: map[int64]int{memTestHour: 1},
			searched: 1, records: 3, err: "find transaction: context canceled",
			source: func(t testing.TB, _ context.CancelFunc) *memSource {
				// Two versions of 13 in the primary's scope: a conflict gap waits for the commit.
				return txHours(t, [][]byte{
					txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12, 13)...)),
					txPackIn(t, seg1, 0, nil, changed(txBlock(txSeqs(13)...), 1, 13)),
				})
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := tt.source(t, cancel)
			if tt.cancelled {
				cancel()
			}
			key := txKeyAt(12)
			if tt.seq != 0 {
				key = txKeyAt(tt.seq)
			}
			var lk *txLookup
			res, err := findTransactionWith(ctx, s, key, TxOptions{MaxScopes: 2}, func(l *txLookup) {
				lk = l
				if tt.prep != nil {
					tt.prep(t, l, cancel)
				}
				if tt.in != "" {
					l.ticks.step = 1
					l.onTick = func() {
						if tickCaller() == tt.in {
							cancel()
						}
					}
				}
			})
			if lk != nil {
				requireStateRecount(t, lk)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, tt.err)
			assert.Equal(t, TxOutcome(0), res.Outcome)
			assert.Len(t, res.Searched, tt.searched)
			assert.Len(t, res.Records, tt.records)
			assert.Len(t, res.Gaps, tt.gaps)
			for _, r := range res.Records {
				assert.False(t, tt.unmarked && r.Conflict, "seq %d is not marked once the marking is cancelled", r.Record.Seq)
			}
			observes, scopes, closes := s.counts()
			assert.Equal(t, tt.observes, observes)
			assert.Equal(t, tt.scopes, nilIfEmptyMap(scopes))
			assert.Equal(t, tt.observes, closes, "closed once when opened")
		})
	}
}

// TestFindTransactionCancelDroppingGroup cancels the lookup while a dropped seq group of three versions is released,
// after its first version: the versions released leave the state, the others stay buffered and charged,
// so the state is still as recounted.
func TestFindTransactionCancelDroppingGroup(t *testing.T) {
	t.Parallel()

	s := txHours(t, [][]byte{
		txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txNote(13, []byte{1}), txRecord(14, nil))),
		txPackIn(t, seg1, 0, nil, txBlock(txNote(13, []byte{2}))),
		txPackIn(t, seg2, 0, nil, txBlock(txNote(13, []byte{3}))),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var lk *txLookup
	ticks := 0
	res, err := findTransactionWith(ctx, s, txKeyAt(12), TxOptions{MaxScopes: 1}, func(l *txLookup) {
		lk = l
		l.ticks.step = 1
		l.onTick = func() {
			if tickCaller() == "endGroup" {
				if ticks++; ticks == 2 {
					cancel()
				}
			}
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, res.Outcome)
	require.NotNil(t, lk)
	require.Len(t, lk.reads, 1)
	assert.Len(t, lk.reads[0].group.versions, 2, "the versions not released stay buffered")
	requireStateRecount(t, lk)
}

// TestFindTransactionStateBudgetEarly charges each version a scope read yields before the primary as it arrives:
// a large annotation that seg0's misindexed block yields before the primary at 15 plays no role,
// yet it is cloned and charged, since it cannot be classified before the primary is established, and released once it is.
// So the MaxStateBytes that a lookup of the same records read in seq order fits in, the annotation dropped there without a copy,
// fails with ErrReadLimit while it is buffered; with room for it, the lookup succeeds and holds no charge of it.
// A primary without a key ends the lookup before any classification: the versions buffered are released, not kept.
func TestFindTransactionStateBudgetEarly(t *testing.T) {
	t.Parallel()

	big := txNote(16, bytes.Repeat([]byte{1}, 40_000))
	run := func(t *testing.T, opts TxOptions, files ...[]byte) (TxResult, *txLookup, error) {
		t.Helper()

		var l *txLookup
		res, err := findTransactionWith(t.Context(), txSource(t, true, files...), txKeyAt(15), opts, func(lk *txLookup) { l = lk })
		require.NotNil(t, l)
		requireStateRecount(t, l)

		return res, l, err
	}
	later := txPack(t, seg1, nil, txBlock(txRecord(15, nil), txRecord(17, nil)))

	res, l, err := run(t, TxOptions{MaxScopes: 1}, txMisindexedAt(t, big), later)
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []string{"15@0 primary", "17@0 same-key-primary"}, recordSeqs(res.Records))
	assert.Equal(t, []string{"index", "index@15"}, gapSeqs(res.Gaps))
	assert.Empty(t, l.earlyVersions)
	limit := l.state.used
	require.Less(t, limit, versionCost(&big, nil), "the annotation's charge is released")

	inOrder, l, err := run(t, TxOptions{MaxScopes: 1, MaxStateBytes: limit},
		txPack(t, seg0, nil, txBlock(txRecord(10, nil), big)), later)
	require.NoError(t, err, "read in seq order, the annotation is dropped without a copy")
	assert.Equal(t, TxUnmatched, inOrder.Outcome)
	assert.LessOrEqual(t, l.state.used, limit)

	_, _, err = run(t, TxOptions{MaxScopes: 1, MaxStateBytes: limit}, txMisindexedAt(t, big), later)
	require.ErrorIs(t, err, ErrReadLimit, "the annotation is charged while it is buffered")

	t.Run("a primary without a key", func(t *testing.T) {
		t.Parallel()

		unkeyed := txPack(t, seg1, nil, txBlock(txRecord(15, func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }), txRecord(17, nil)))
		res, l, err := run(t, TxOptions{MaxScopes: 1}, txMisindexedAt(t, big, txReply(18, nil)), unkeyed)
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []string{"15@0 primary"}, recordSeqs(res.Records))
		assert.Equal(t, []string{"index", "index@15", "no-key@15"}, gapSeqs(res.Gaps))
		assert.Empty(t, l.earlyVersions, "released, not kept")
		require.Less(t, l.state.used, versionCost(&big, nil))
	})
}

// TestFindTransactionCancelEarlyVersions cancels a lookup
// while the versions that seg0's misindexed block yields before the primary at 15,
// a reply at 16 and an annotation at 17, are buffered, beside seg1's later version of 17:
// during the read, once both early versions are buffered; while they are classified;
// and while they are kept or released, after the first of them.
// What is not yet classified, kept or released keeps its charge, so the state is still as recounted,
// and the result has no outcome.
func TestFindTransactionCancelEarlyVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// at reports whether to cancel ctx at a check of the lookup l, the n-th check made by the same method.
		at func(l *txLookup, n int) bool
		// buffered and later are the versions left in the early buffer and among the later versions of early seqs.
		buffered, later int
		// records are the records of the result: the primary once it is settled, none of the read's others.
		records []string
	}{
		{name: "during the read", at: func(l *txLookup, _ int) bool { return len(l.earlyVersions) == 2 && len(l.primary) == 0 },
			buffered: 2, records: []string{}},
		{name: "while classifying them", at: func(_ *txLookup, _ int) bool { return tickCaller() == "classifyEarly" },
			buffered: 2, later: 1, records: []string{"15@0 primary"}},
		// keepEarly checks ctx once for each of the three versions, then before keeping or releasing each.
		{name: "while keeping or releasing them", at: func(_ *txLookup, n int) bool { return tickCaller() == "keepEarly" && n == 5 },
			buffered: 1, later: 1, records: []string{"15@0 primary"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true, txMisindexedAt(t, txReply(16, nil), txNote(17, []byte{1})),
				txPack(t, seg1, nil, txBlock(txRecord(15, nil), txNote(17, []byte{2}), txRecord(18, nil))))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var lk *txLookup
			calls := map[string]int{}
			res, err := findTransactionWith(ctx, s, txKeyAt(15), TxOptions{MaxScopes: 1}, func(l *txLookup) {
				lk = l
				l.ticks.step = 1
				l.onTick = func() {
					caller := tickCaller()
					calls[caller]++
					if tt.at(l, calls[caller]) {
						cancel()
					}
				}
			})
			require.ErrorIs(t, err, context.Canceled)
			assert.Zero(t, res.Outcome)
			assert.Equal(t, tt.records, recordSeqs(res.Records), "the read is not committed")
			require.NotNil(t, lk)
			assert.Len(t, lk.earlyVersions, tt.buffered)
			assert.Len(t, lk.laterVersions, tt.later)
			requireStateRecount(t, lk)
		})
	}
}

// TestFindTransactionStateBudgetReread charges each version the second pass over a scope's view recovers:
// seg0's misindexed block yields a large annotation at 20 after the primary at 10, and seg1 a same-key primary at 20 later,
// so the read yields 20 in two places and reads the view again, keeping the annotation beside the same-key primary.
// The lookup fits in MaxStateBytes equal to its final charge, which holds the annotation, and fails with ErrReadLimit with one byte less.
func TestFindTransactionStateBudgetReread(t *testing.T) {
	t.Parallel()

	big := txNote(20, bytes.Repeat([]byte{1}, 40_000))
	run := func(t *testing.T, limit int64) (TxResult, *txLookup, error) {
		t.Helper()

		var l *txLookup
		s := txSource(t, true, txMisindexedAt(t, big), txPack(t, seg1, nil, txBlock(txNote(15, nil), txRecord(20, nil))))
		res, err := findTransactionWith(t.Context(), s, txKeyAt(10), TxOptions{MaxScopes: 1, MaxStateBytes: limit},
			func(lk *txLookup) { l = lk })
		require.NotNil(t, l)
		requireStateRecount(t, l)

		return res, l, err
	}

	res, l, err := run(t, DefaultTxMaxStateBytes)
	require.NoError(t, err)
	assert.Equal(t, []string{"10@0 primary", "20@0 none", "20@0 same-key-primary"}, recordSeqs(res.Records))
	assert.Equal(t, 2, l.passes)
	limit := l.state.used
	require.Greater(t, limit, versionCost(&big, nil), "the annotation is held")

	_, _, err = run(t, limit)
	require.NoError(t, err, "MaxStateBytes %d fits", limit)
	_, _, err = run(t, limit-1)
	require.ErrorIs(t, err, ErrReadLimit, "MaxStateBytes %d", limit-1)
}

// TestFindTransactionCancelReread cancels a lookup during the second pass over a scope's view that yields 20 in two places,
// an annotation in seg0's misindexed block and a same-key primary in seg1:
// while the versions of 20 the first pass kept are dropped, while the second pass reads, and while its versions are kept or released.
// What is not yet dropped, kept or released keeps its charge, so the state is still as recounted, and the result has no outcome.
func TestFindTransactionCancelReread(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// caller and nth name the check of ctx that cancels it: the nth that caller makes.
		caller string
		nth    int
		// kept and reread are the versions left kept in the read and buffered by its second pass.
		kept, reread int
	}{
		{name: "while dropping the versions kept", caller: "dropRepeated", nth: 1, kept: 1},
		// The second pass yields 10, 20, 15 and 20: the first 20 is buffered before the check of the third item.
		{name: "during the second pass", caller: "collectRepeat", nth: 3, reread: 1},
		// keepRepeated checks ctx once for each of the two versions, then before keeping or releasing each.
		{name: "while keeping or releasing", caller: "keepRepeated", nth: 4, kept: 1, reread: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true, txMisindexedAt(t, txNote(20, nil)), txPack(t, seg1, nil, txBlock(txNote(15, nil), txRecord(20, nil))))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var lk *txLookup
			calls := map[string]int{}
			res, err := findTransactionWith(ctx, s, txKeyAt(10), TxOptions{MaxScopes: 1}, func(l *txLookup) {
				lk = l
				l.ticks.step = 1
				l.onTick = func() {
					caller := tickCaller()
					if calls[caller]++; caller == tt.caller && calls[caller] == tt.nth {
						cancel()
					}
				}
			})
			require.ErrorIs(t, err, context.Canceled)
			assert.Zero(t, res.Outcome)
			assert.Empty(t, res.Records, "the read is not committed")
			assert.Empty(t, res.Searched, "a read that ends early is not searched")
			require.NotNil(t, lk)
			require.Len(t, lk.reads, 1)
			rd := lk.reads[0]
			assert.Len(t, rd.kept, tt.kept)
			assert.Len(t, rd.reread, tt.reread)
			assert.NotEmpty(t, rd.repeats.runs, "the repeated seq stays charged")
			requireStateRecount(t, lk)
		})
	}
}

// TestFindTransactionTicksEmptyScopes counts the checks of ctx in the probes of the scope reads for a seq:
// a read whose run set is empty counts toward them as any other,
// both when a later scope's seq is looked up in the earlier reads and when a window seq is looked up in every read.
func TestFindTransactionTicksEmptyScopes(t *testing.T) {
	t.Parallel()

	// The window (12, 14) misses 13; the two hours between the primary's and 14's are read and empty.
	s := txHours(t,
		[][]byte{txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12)...))},
		nil, nil,
		[][]byte{txPackIn(t, seg1, 3, nil, txBlock(txSeqs(14)...))})
	ticks := map[string]int{}
	res, err := findTransactionWith(t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 4}, func(l *txLookup) {
		l.ticks.step = 1
		l.onTick = func() { ticks[tickCaller()]++ }
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"seq-gap@13"}, gapSeqs(res.Gaps))
	assert.Equal(t, 3, ticks["crossConflict"], "14 probes the three earlier reads")
	assert.Equal(t, 4, ticks["visitedFrom"], "13 probes the four reads")
}

// armedCtx is a context that is done only once armed: after arm, Err returns nil once, then context.Canceled.
type armedCtx struct {
	context.Context
	armed bool
	// passes counts the calls of Err that still return nil once armed.
	passes int
}

// arm makes c done after the next call of Err; a later arm changes nothing.
func (c *armedCtx) arm() {
	if !c.armed {
		c.armed, c.passes = true, 1
	}
}

// Err returns the parent's error until c is armed, then nil once, then context.Canceled.
func (c *armedCtx) Err() error {
	if !c.armed {
		return c.Context.Err()
	}
	if c.passes > 0 {
		c.passes--
		return nil
	}

	return context.Canceled
}

// TestFindTransactionCancelBeforeBarriers cancels a lookup right after the last check of ctx that the epoch barrier's loop makes,
// whose one evidence boundary, a gap of another epoch, adds no gap:
// the lookup checks ctx before it calls the observation's Barriers, and fails with ctx's error before the call.
func TestFindTransactionCancelBeforeBarriers(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil))))
	s.addBoundary(Boundary{Capture: captureLow, Seq: 500, Kind: BoundaryKindGap, Epoch: 2})
	calls := 0
	s.beforeBarriers = func(context.Context, int64, int64) error {
		calls++
		return nil
	}
	ctx := &armedCtx{Context: t.Context()}
	var lk *txLookup
	res, err := findTransactionWith(ctx, s, txKeyAt(12), TxOptions{MaxScopes: 1}, func(l *txLookup) {
		lk = l
		l.ticks.step = 1
		l.onTick = func() {
			if tickCaller() == "epochBarrierGaps" {
				ctx.arm()
			}
		}
	})
	requireStateRecount(t, lk)
	require.ErrorIs(t, err, context.Canceled)
	require.EqualError(t, err, "tracepack: find transaction: context canceled")
	assert.Zero(t, calls, "Barriers is not called")
	assert.Equal(t, TxOutcome(0), res.Outcome)
	_, _, closes := s.counts()
	assert.Equal(t, 1, closes)
}

// tickCaller returns the name of the method or function that called the lookup's tick, called from its onTick.
func tickCaller() string {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	for f, more := frames.Next(); more; f, more = frames.Next() {
		if !strings.HasSuffix(f.Function, ".tick") {
			continue
		}
		caller, _ := frames.Next()

		return caller.Function[strings.LastIndex(caller.Function, ".")+1:]
	}

	return ""
}

// TestTxBudget reserves and releases charges: a reservation that would pass the maximum fails with ErrReadLimit,
// charging nothing, also for a charge near the int64 range's end.
func TestTxBudget(t *testing.T) {
	t.Parallel()

	b := txBudget{max: 100}
	require.NoError(t, b.reserve(60))
	require.ErrorIs(t, b.reserve(41), ErrReadLimit)
	require.Equal(t, int64(60), b.used)
	require.NoError(t, b.reserve(40))
	b.release(30)
	require.Equal(t, int64(70), b.used)
	require.ErrorIs(t, b.reserve(math.MaxInt64), ErrReadLimit)
	require.Equal(t, int64(70), b.used)
}

// TestTxCosts pins each cost function to the charge constants, by arithmetic written out here.
func TestTxCosts(t *testing.T) {
	t.Parallel()

	unknown := []RawEntry{{Tag: 0x0050, Type: 7, Value: make([]byte, 5)}, {Tag: 0x0051, Type: 7}, {Tag: 0x0052, Type: 7, Value: make([]byte, 100)}}
	cov := &Coverage{SeqFirst: new(uint64(1)), Unknown: unknown}
	rec := Record{Payload: make([]byte, 30)}
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{name: "a version", got: versionCost(&rec, make([]byte, 7)), want: txRecordCharge + 30 + 7},
		{name: "a version without bytes", got: versionCost(&Record{}, nil), want: txRecordCharge},
		{name: "a coverage entry", got: coverageCost(cov), want: txCoverageCharge + 3*txRawEntryCharge + 105},
		{name: "a coverage entry without unknown entries", got: coverageCost(&Coverage{}), want: txCoverageCharge},
		{name: "a gap", got: gapCost(&TxGap{}), want: txGapCharge},
		{name: "a gap with two hours, coverage and a barrier", got: gapCost(&TxGap{
			Hours: []int64{1, 2}, Coverage: cov, Barrier: &Boundary{GapStart: new(int64(1))},
		}), want: txGapCharge + 2*txHourCharge + txCoverageCharge + 3*txRawEntryCharge + 105 + txBoundaryCharge},
		{name: "three packs", got: scopeCost(3), want: txScopeCharge + 3*txPackCharge},
		{name: "no pack", got: scopeCost(0), want: txScopeCharge},
		{name: "a conflict", got: conflictCost(&Conflict{Versions: [][]UUID{{seg0}, {seg1, seg2, seg3}, {}}}),
			want: txConflictCharge + 3*txVersionCharge + 4*txPackCharge},
		{name: "a conflict without versions", got: conflictCost(&Conflict{}), want: txConflictCharge},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.got, tt.name)
	}

	// The charges that count toward the checks of ctx sum as the cost functions do.
	l := newTxLookup(TxKey{}, TxOptions{}, nil)
	conflict := &Conflict{Versions: [][]UUID{{seg0}, {seg1, seg2, seg3}, {}}}
	gap := &TxGap{Hours: []int64{1, 2}, Coverage: cov, Barrier: &Boundary{}}
	for _, c := range []struct {
		name  string
		got   func() (int64, error)
		want  int64
		ticks int
	}{
		{name: "a coverage entry", got: func() (int64, error) { return l.coverageCharge(t.Context(), cov) }, want: coverageCost(cov), ticks: 3},
		{name: "a gap", got: func() (int64, error) { return l.gapCharge(t.Context(), gap) }, want: gapCost(gap), ticks: 3},
		{name: "a gap without coverage", got: func() (int64, error) { return l.gapCharge(t.Context(), &TxGap{Hours: []int64{1}}) },
			want: gapCost(&TxGap{Hours: []int64{1}})},
		{name: "a conflict", got: func() (int64, error) { return l.conflictCharge(t.Context(), conflict) }, want: conflictCost(conflict),
			ticks: 3},
	} {
		l.ticks = ctxCounter{step: 1}
		n := 0
		l.onTick = func() { n++ }
		got, err := c.got()
		require.NoError(t, err)
		assert.Equal(t, c.want, got, c.name)
		assert.Equal(t, c.ticks, n, "%s: one check per entry", c.name)
	}

	// The rounded fixed charges are positive multiples of 16; a pack_id is 16 bytes and an hour 8.
	for _, c := range []int64{
		txRecordCharge, txRunCharge, txCrossCharge, txGapCharge, txScopeCharge, txPackErrorCharge, txUncertainCharge, txConflictedCharge,
		txPendingCoverageCharge, txCoverageCharge, txRawEntryCharge, txConflictCharge,
	} {
		assert.Positive(t, c)
		assert.Zero(t, c%16, "%d", c)
	}
	assert.Equal(t, (int64(unsafe.Sizeof(Boundary{}))+15)&^15+16, txBoundaryCharge, "a Boundary's two pointers add 16 bytes")
	assert.Equal(t, (int64(unsafe.Sizeof(txSeen{}))+15)&^15+txBoundaryCharge, txBoundaryRecordCharge)
	assert.Equal(t, int64(16), txPackCharge)
	assert.Equal(t, int64(8), txHourCharge)
}

// TestFindTransactionStateBudgetCrossConflict charges a conflict across scope reads, its entry, hours and gap, at once:
// a budget one byte short of that charge leaves neither the entry nor its gap,
// and a budget that holds it, but not the version cloned after it, leaves both.
func TestFindTransactionStateBudgetCrossConflict(t *testing.T) {
	t.Parallel()

	source := func(t testing.TB) *memSource {
		return txHours(t,
			[][]byte{txPackIn(t, seg0, 0, nil, txBlock(txSeqs(12, 13)...))},
			[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(13)...))})
	}
	res, l, err := findTxState(t, source(t), TxOptions{MaxScopes: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"12@0 primary", "13@0 same-key-primary!", "13@1 same-key-primary!"}, recordSeqs(res.Records))
	// The version of 13 in the second hour is the last charge, after the conflict's.
	last := &res.Records[2]
	atCross := l.state.used - versionCost(&last.Record, last.HeaderExtra)

	for _, tt := range []struct {
		limit int64
		cross bool
	}{{limit: atCross - 1}, {limit: atCross, cross: true}} {
		var lk *txLookup
		_, err := findTransactionWith(t.Context(), source(t), txKeyAt(12), TxOptions{MaxScopes: 2, MaxStateBytes: tt.limit},
			func(l *txLookup) { lk = l })
		require.ErrorIs(t, err, ErrReadLimit, "limit %d", tt.limit)
		requireStateRecount(t, lk)
		require.Len(t, lk.reads, 2)
		assert.Equal(t, tt.cross, lk.cross[13] != nil, "limit %d", tt.limit)
		// The failed read's gaps are committed to the result.
		assert.Empty(t, lk.reads[1].gaps)
		assert.Equal(t, tt.cross, slices.ContainsFunc(lk.res.Gaps, func(g TxGap) bool {
			return g.Reason == TxGapConflict && g.Seq != nil && *g.Seq == 13
		}), "limit %d", tt.limit)
	}
}
