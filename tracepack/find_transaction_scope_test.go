package tracepack

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gapHours describes each gap as "<reason>@<hours - memTestHour>", followed by "/<seq>" when it has a seq, in order.
func gapHours(gaps []TxGap) []string {
	out := make([]string, len(gaps))
	for i := range gaps {
		g := &gaps[i]
		hours := make([]string, len(g.Hours))
		for j, h := range g.Hours {
			hours[j] = fmt.Sprint(h - memTestHour)
		}
		out[i] = fmt.Sprintf("%v@%s", g.Reason, strings.Join(hours, ","))
		if g.Seq != nil {
			out[i] += fmt.Sprintf("/%d", *g.Seq)
		}
	}

	return out
}

// searchedHours returns the hour of each scope searched, as an offset from memTestHour, in order.
func searchedHours(scopes []TxScope) []int64 {
	out := make([]int64, len(scopes))
	for i := range scopes {
		out[i] = scopes[i].Hour - memTestHour
	}

	return out
}

// readHours returns the hour of each scope the lookup l read, as an offset from memTestHour, in order.
func readHours(l *txLookup) []int64 {
	out := make([]int64, len(l.reads))
	for i, rd := range l.reads {
		out[i] = rd.hour - memTestHour
	}

	return out
}

// TestFindTransactionConflictedScope looks up txRecord's primary at 12 in memTestHour,
// with a scope whose view is conflicted (the tracepack semantics specification §7.2, the tracepack storage specification §5):
// the scope is not read, so it adds no read, run set, identity comparison or searched scope, only its gaps,
// cold first when it is not indexed;
// in the primary's hour it explains the missing primary and ends the lookup without a key, no later scope asked for;
// in a later hour the next hour is still read, and a match found elsewhere stays a match,
// while a conflict in another hour still makes the outcome incomplete.
// The conflicted generations hold a version of each seq the hours read hold too, never compared with them.
// Each lookup returns what the observation-bounded check computes from the observation, independently of the lookup.
func TestFindTransactionConflictedScope(t *testing.T) {
	t.Parallel()

	// conflicted makes hour conflicted, its generations holding recs, admitted and indexed or staged and not indexed.
	conflicted := func(t testing.TB, s *memSource, hour int64, indexed bool, recs ...Record) {
		t.Helper()

		s.conflict(t, memTestHour+hour, !indexed, txBlock(recs...))
		s.setIndexed(captureLow, memTestHour+hour, indexed)
	}
	reply := func(seq uint64) Record { return txReply(seq, nil) }
	tests := []struct {
		name string
		// source returns the hours' packs, every hour indexed; edit then changes it.
		source    [][][]byte
		edit      func(t testing.TB, s *memSource)
		maxScopes int
		outcome   TxOutcome
		gaps      []string
		records   []string
		searched  []int64
		scopes    map[int64]int
	}{
		{
			name:   "a conflicted primary scope, indexed, with a valid reply later",
			source: [][][]byte{nil, {txPackIn(t, seg1, 1, nil, txBlock(reply(14)))}},
			edit: func(t testing.TB, s *memSource) {
				conflicted(t, s, 0, true, txRecord(12, nil), reply(13))
			},
			maxScopes: 2, outcome: TxIncomplete, gaps: []string{"conflicted@0", "no-key@0/12"},
			searched: []int64{}, scopes: map[int64]int{memTestHour: 1},
		},
		{
			name:   "a conflicted primary scope, not indexed, with a valid reply later",
			source: [][][]byte{nil, {txPackIn(t, seg1, 1, nil, txBlock(reply(14)))}},
			edit: func(t testing.TB, s *memSource) {
				conflicted(t, s, 0, false, txRecord(12, nil), reply(13))
			},
			maxScopes: 2, outcome: TxIncomplete, gaps: []string{"cold@0", "conflicted@0", "no-key@0/12"},
			searched: []int64{}, scopes: map[int64]int{memTestHour: 1},
		},
		{
			name: "a conflicted middle hour",
			source: [][][]byte{
				{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil)))}, nil, {txPackIn(t, seg2, 2, nil, txBlock(reply(14)))},
			},
			edit:      func(t testing.TB, s *memSource) { conflicted(t, s, 1, true, inHour(1, reply(14))) },
			maxScopes: 3, outcome: TxMatched, gaps: []string{"conflicted@1", "open-window@"},
			records: []string{"12@0 primary", "14@2 candidate"}, searched: []int64{0, 2},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1},
		},
		{
			name: "a conflicted middle hour, not indexed",
			source: [][][]byte{
				{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil)))}, nil, {txPackIn(t, seg2, 2, nil, txBlock(reply(14)))},
			},
			edit:      func(t testing.TB, s *memSource) { conflicted(t, s, 1, false, inHour(1, reply(14))) },
			maxScopes: 3, outcome: TxMatched, gaps: []string{"cold@1", "conflicted@1", "open-window@"},
			records: []string{"12@0 primary", "14@2 candidate"}, searched: []int64{0, 2},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1},
		},
		{
			name:      "a conflicted last hour",
			source:    [][][]byte{{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), reply(13)))}},
			edit:      func(t testing.TB, s *memSource) { conflicted(t, s, 1, true, inHour(1, reply(13))) },
			maxScopes: 2, outcome: TxMatched, gaps: []string{"conflicted@1", "open-window@"},
			records: []string{"12@0 primary", "13@0 candidate"}, searched: []int64{0},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
		},
		{
			name: "a record conflict in a later hour overrides the match",
			source: [][][]byte{
				{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), reply(13)))}, nil,
				{txPackIn(t, seg2, 2, nil, txBlock(txNote(20, []byte{1}))), txPackIn(t, seg3, 2, nil, txBlock(txNote(20, []byte{2})))},
			},
			edit:      func(t testing.TB, s *memSource) { conflicted(t, s, 1, true, inHour(1, reply(13))) },
			maxScopes: 3, outcome: TxIncomplete, gaps: []string{"conflicted@1", "conflict@2/20", "open-window@"},
			records: []string{"12@0 primary", "13@0 candidate"}, searched: []int64{0, 2},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1},
		},
		{
			name:      "a window the primary's hour closes",
			source:    [][][]byte{{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txRecord(13, nil)))}},
			edit:      func(t testing.TB, s *memSource) { conflicted(t, s, 1, true, inHour(1, txRecord(13, nil))) },
			maxScopes: 2, outcome: TxIncomplete, gaps: []string{"conflicted@1"},
			records: []string{"12@0 primary", "13@0 same-key-primary"}, searched: []int64{0},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
		},
		{
			name: "control: the window the primary's hour closes, the next hour empty",
			source: [][][]byte{
				{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), txRecord(13, nil)))}, nil,
			},
			maxScopes: 2, outcome: TxUnmatched, gaps: []string{},
			records: []string{"12@0 primary", "13@0 same-key-primary"}, searched: []int64{0, 1},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
		},
		{
			name: "closure evidence naming a seq only the conflicted hour holds",
			source: [][][]byte{
				{txPackIn(t, seg0, 0, nil, txBlock(txRecord(12, nil), reply(13)))},
			},
			edit: func(t testing.TB, s *memSource) {
				conflicted(t, s, 1, true, inHour(1, testEventRecord(t, 14, blockTestHour+14, txTestEpoch,
					&TransportEvent{Event: EventSocketClose})))
				s.addClosure(captureLow, txTestEpoch, 14)
			},
			maxScopes: 2, outcome: TxMatched, gaps: []string{"conflicted@1", "open-window@"},
			records: []string{"12@0 primary", "13@0 candidate"}, searched: []int64{0},
			scopes: map[int64]int{memTestHour: 1, memTestHour + 1: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.source...)
			if tt.edit != nil {
				tt.edit(t, s)
			}
			var l *txLookup
			key := txKeyAt(12)
			opts := TxOptions{MaxScopes: tt.maxScopes}
			res, err := findTransactionWith(t.Context(), s, key, opts, func(lk *txLookup) { l = lk })
			require.NoError(t, err)
			requireStateRecount(t, l)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, tt.gaps, gapHours(res.Gaps))
			assert.Equal(t, tt.records, nilIfEmpty(recordSeqs(res.Records)))
			assert.Equal(t, tt.searched, searchedHours(res.Searched))
			// The lookup's reads, each with its run set, are the searched scopes, the conflicted hour never among them.
			assert.Equal(t, tt.searched, readHours(l), "the scopes read")
			_, scopes, _ := s.counts()
			assert.Equal(t, tt.scopes, scopes)

			snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+int64(tt.maxScopes))
			want, notPrimary := snap.expect(key, tt.maxScopes)
			requireTxExpected(t, want, notPrimary, res, err)
		})
	}
}

// TestFindTransactionConflictedScheduled looks up txRecord's primary at 12 in memTestHour,
// its window (12, 15) read whole in that hour, with the next hour conflicted,
// beside a coverage entry of the pack read whose seqs meet the window and whose time interval lies in the next hour only,
// or beside a stop-unclean of another capture whose gap lies in the next hour only:
// with MaxScopes 2, the conflicted hour is scheduled though not read, so the entry or the barrier is a gap;
// with MaxScopes 1 neither is, and the lookup is TxUnmatched.
// The observation-bounded check computes the same, from the hours scheduled.
func TestFindTransactionConflictedScheduled(t *testing.T) {
	t.Parallel()

	inNext := func(v int64) *int64 { return new(blockTestHour + hourNs + v) }
	recs := txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil), txRecord(15, nil))
	tests := []struct {
		name   string
		meta   func(m *PackMeta)
		edit   func(s *memSource)
		reason string
	}{
		{
			name: "a coverage entry", reason: "coverage@0",
			meta: func(m *PackMeta) {
				m.Coverage = []Coverage{{SeqFirst: new(uint64(13)), SeqLast: new(uint64(14)), TimeStart: inNext(10), TimeEnd: inNext(20)}}
			},
		},
		{
			name: "a barrier", reason: "barrier@",
			edit: func(s *memSource) { s.addBoundary(txUnclean(captureHigh, 5, 1, inNext(10), inNext(20))) },
		},
	}
	for _, tt := range tests {
		for _, maxScopes := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, MaxScopes %d", tt.name, maxScopes), func(t *testing.T) {
				t.Parallel()

				s := txHours(t, [][]byte{txPackIn(t, seg0, 0, tt.meta, recs)})
				s.conflict(t, memTestHour+1, false, txBlock(inHour(1, txNote(16, nil))))
				s.setIndexed(captureLow, memTestHour+1, true)
				if tt.edit != nil {
					tt.edit(s)
				}
				key, opts := txKeyAt(12), TxOptions{MaxScopes: maxScopes}
				res, err := findTx(t, t.Context(), s, key, opts)
				require.NoError(t, err)
				want := []string{"unmatched"}
				if maxScopes == 2 {
					want = []string{"incomplete", "conflicted@1", tt.reason}
				}
				assert.Equal(t, want, append([]string{res.Outcome.String()}, gapHours(res.Gaps)...))

				snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+int64(maxScopes))
				exp, notPrimary := snap.expect(key, maxScopes)
				requireTxExpected(t, exp, notPrimary, res, err)
			})
		}
	}
}

// TestFindTransactionConflictedStateLimit requires the charge of a conflicted scope to be its gaps only:
// a conflicted primary scope, indexed or not, charges its TxGapConflicted gap, its TxGapCold gap when not indexed,
// and the TxGapNoKey gap; a conflicted later hour charges its TxGapConflicted gap beside the primary's scope read
// and the open window.
// Each lookup fits that limit exactly and fails one byte below it.
func TestFindTransactionConflictedStateLimit(t *testing.T) {
	t.Parallel()

	primary := txRecord(12, nil)
	hourGap := gapCost(&TxGap{Hours: []int64{memTestHour}})
	tests := []struct {
		name   string
		source func(t testing.TB) *memSource
		limit  int64
	}{
		{
			name: "the primary's scope, indexed",
			source: func(t testing.TB) *memSource {
				s := txHours(t, nil)
				s.conflict(t, memTestHour, false, txBlock(primary))

				return s
			},
			limit: 2 * hourGap,
		},
		{
			name: "the primary's scope, not indexed",
			source: func(t testing.TB) *memSource {
				s := newMemSource()
				s.conflict(t, memTestHour, true, txBlock(primary))

				return s
			},
			limit: 3 * hourGap,
		},
		{
			name: "a later hour",
			source: func(t testing.TB) *memSource {
				s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(primary))}, nil)
				s.conflict(t, memTestHour+1, false, txBlock(inHour(1, txNote(13, nil))))

				return s
			},
			limit: scopeCost(1) + versionCost(&primary, nil) + txRunCharge + hourGap + gapCost(&TxGap{}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requireStateLimit(t, tt.source, TxOptions{MaxScopes: 2}, tt.limit)
		})
	}
}

// TestFindTransactionConflictedReaders requires a conflicted scope that comes with readers to fail the lookup,
// which reads none of them, Outcome zero and the observation closed once.
func TestFindTransactionConflictedReaders(t *testing.T) {
	t.Parallel()

	file := txPack(t, seg0, nil, txBlock(txRecord(12, nil)))
	s := &txReadersSource{memSource: txSource(t, true), readers: []*Reader{mustOpen(t, file, ReaderOptions{})}, conflicted: true}
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.ErrorContains(t, err, "a conflicted scope comes with readers (1)")
	assert.Zero(t, res.Outcome)
	assert.Empty(t, res.Gaps)
	_, _, closes := s.counts()
	assert.Equal(t, 1, closes)
}

// TestTxTruthConflicted requires the safety check to leave a conflicted hour's packs out of the comparison domain
// and to reject a TxUnmatched whose hours scheduled hold a conflicted one.
func TestTxTruthConflicted(t *testing.T) {
	t.Parallel()

	tr := newTxTruth(txSeqRange(12, 14), 0, 1)
	txTruthPack(t, tr, seg0, 0, nil, nil, txRecord(12, nil), txNote(13, nil), txRecord(14, nil))
	txTruthPack(t, tr, seg1, 1, nil, nil, inHour(1, txNote(13, []byte("x"))))
	key := txKeyAt(12)
	assert.Len(t, tr.domain(key, 2).versions[13], 2)

	tr.conflicted = map[int64]bool{memTestHour + 1: true}
	d := tr.domain(key, 2)
	assert.Len(t, d.versions[13], 1, "the conflicted hour's version is outside the domain")
	assert.Len(t, d.packs, 1)
	res := TxResult{Outcome: TxUnmatched, WindowEnd: new(uint64(14)), Epoch: txTestEpoch}
	res.Dir, res.SessionID, res.SystemBytes, res.Function = DirHostToEquipment, 0x1234, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, 3
	res.Stream, res.StreamAvailable = 1, true
	out := tr.violations(key, 2, &res, [][]txRun{{{first: 12, last: 14}}})
	assert.Equal(t, []string{fmt.Sprintf("hour %d is conflicted", memTestHour+1)}, slices.DeleteFunc(out, func(v string) bool { return !strings.Contains(v, "conflicted") }))
	assert.NotContains(t, out, "seq 13 has 2 versions in the scopes read")
}

// txBreachPack writes steps, whose records keep their times, into a pack id whose period is the hour memTestHour + hours,
// as txPack does, unfinalized when walked is set.
func txBreachPack(t testing.TB, id UUID, hours int64, walked bool, steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.QualityEvaluated = true
		m.PeriodStart += hours * hourNs
		m.PeriodEnd += hours * hourNs
	}, walked, steps).file, id, captureLow)
}

// TestFindTransactionScopeBreach looks up txRecord's primary at 12 in memTestHour
// beside versions whose ts_utc_ns lies outside the hour of the scope that holds them
// (the tracepack semantics specification §7.2, the tracepack format specification I-13):
// each is a TxGapScopeBreach gap naming the scope's hour and the version's seq,
// and the outcome is TxIncomplete, also beside a match or a closed window;
// the version is still classified, and kept as any other version is.
// The packs' footers are truthful, used or not (walked), and the scopes indexed or read through their listing views.
// A segment whose period is narrower than its hour, its records in the hour but outside the period, is no breach.
// Each lookup returns what the observation-bounded check computes from the observation.
func TestFindTransactionScopeBreach(t *testing.T) {
	t.Parallel()

	socketClose := func(seq uint64) Record {
		return testEventRecord(t, seq, blockTestHour+int64(seq), txTestEpoch, &TransportEvent{Event: EventSocketClose})
	}
	tests := []struct {
		name string
		// steps are the records of a pack in memTestHour, and later those of a pack in the next hour.
		steps, later []footerTestStep
		walked       bool
		indexed      bool
		meta         func(m *PackMeta)
		outcome      TxOutcome
		breaches     []string
		records      []string
	}{
		{
			name:  "the primary and its reply in the next hour",
			steps: inHours(1, txBlock(txRecord(12, nil), txReply(13, nil))), indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/12", "scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "the primary and its reply in the next hour, walked",
			steps: inHours(1, txBlock(txRecord(12, nil), txReply(13, nil))), walked: true, indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/12", "scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:    "the primary and its reply in the next hour, not indexed",
			steps:   inHours(1, txBlock(txRecord(12, nil), txReply(13, nil))),
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/12", "scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a reply in the next hour",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txReply(13, nil)))...), indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a reply in the next hour, walked",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txReply(13, nil)))...), walked: true, indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a closing record in the next hour, walked",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(socketClose(13)))...), walked: true, indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 closing bound"},
		},
		{
			name:  "a closing record in the next hour",
			steps: append(txBlock(txRecord(12, nil)), inHours(1, txBlock(socketClose(13)))...), indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/13"},
			records: []string{"12@0 primary", "13@0 closing bound"},
		},
		{
			name:  "a record below the primary in the hour before",
			steps: append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txRecord(12, nil), txRecord(13, nil))...), indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@0/10"},
			records: []string{"12@0 primary", "13@0 same-key-primary bound"},
		},
		{
			name:  "a record of a later scope in the hour after it",
			steps: txBlock(txRecord(12, nil), txReply(13, nil)), later: inHours(2, txBlock(txNote(20, nil))), indexed: true,
			outcome: TxIncomplete, breaches: []string{"scope-breach@1/20"},
			records: []string{"12@0 primary", "13@0 candidate in eligible decidable valid"},
		},
		{
			name:  "a segment whose period starts in the middle of the hour",
			steps: txBlock(txRecord(12, nil), txRecord(13, nil)), indexed: true,
			meta:    func(m *PackMeta) { m.PeriodStart += hourNs / 2 },
			outcome: TxUnmatched, breaches: []string{},
			records: []string{"12@0 primary", "13@0 same-key-primary bound"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newMemSource()
			file := txBreachPack(t, seg0, 0, tt.walked, tt.steps)
			if tt.meta != nil {
				file = txPack(t, seg0, tt.meta, tt.steps)
			}
			s.addPack(t, memTestHour, file)
			if tt.later != nil {
				s.addPack(t, memTestHour+1, txBreachPack(t, seg1, 1, false, tt.later))
			}
			s.setIndexed(captureLow, memTestHour, tt.indexed)
			s.setIndexed(captureLow, memTestHour+1, true)
			key, opts := txKeyAt(12), TxOptions{MaxScopes: 2}
			res, err := findTx(t, t.Context(), s, key, opts)
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			breaches := slices.DeleteFunc(gapHours(res.Gaps), func(g string) bool { return !strings.HasPrefix(g, "scope-breach") })
			assert.Equal(t, tt.breaches, breaches)
			assert.Equal(t, tt.records, recordFlags(res.Records))
			for _, g := range res.Gaps {
				if g.Reason == TxGapScopeBreach {
					// The observation-bounded check compares the pack and block.
					assert.True(t, g.Pack != nil && g.Block >= 0 && g.Offset == -1, "the gap names the pack and block")
				}
			}

			snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+int64(opts.MaxScopes))
			want, notPrimary := snap.expect(key, opts.MaxScopes)
			requireTxExpected(t, want, notPrimary, res, err)
		})
	}
}

// TestFindTransactionScopeBreachSettlement looks up a primary that an indexed scope's read does not yield,
// beside a version of another hour below or above the primary's seq in the scope:
// the breach explains the missing primary, as a read defect does,
// so the lookup is TxIncomplete with the scope-breach and no-key gaps and reads no later scope;
// without the breach, the missing primary is ErrNotPrimary.
// A record at the primary's seq that is not a primary is ErrNotPrimary beside a breach, as it is beside an index mismatch.
// The state's charge equals its recount on each path.
func TestFindTransactionScopeBreachSettlement(t *testing.T) {
	t.Parallel()

	later := [][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil)))}
	tests := []struct {
		name   string
		files  [][]byte
		seq    uint64
		gaps   []string
		notPri bool
	}{
		{
			name:  "a breach below the primary",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txNote(11, nil), txNote(13, nil))...))},
			seq:   12, gaps: []string{"scope-breach@0/10", "no-key@0/12"},
		},
		{
			name:  "a breach above the primary",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(txBlock(txNote(11, nil)), inHours(1, txBlock(txNote(13, nil)))...))},
			seq:   12, gaps: []string{"scope-breach@0/13", "no-key@0/12"},
		},
		{
			name:  "no breach",
			files: [][]byte{txBreachPack(t, seg0, 0, false, txBlock(txNote(11, nil), txNote(13, nil)))},
			seq:   12, gaps: []string{}, notPri: true,
		},
		{
			name:  "a reply at the primary's seq beside a breach",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(inHours(-1, txBlock(txNote(10, nil))), txBlock(txReply(12, nil))...))},
			seq:   12, gaps: []string{"scope-breach@0/10"}, notPri: true,
		},
		{
			name:  "a reply at the primary's seq beside an index mismatch",
			files: [][]byte{txMisindexed(t, 20), txPack(t, seg1, nil, txBlock(txReply(15, nil)))},
			seq:   15, gaps: []string{"index@0", "index@0/15"}, notPri: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.files, later)
			key := txKeyAt(tt.seq)
			res, err := findTx(t, t.Context(), s, key, TxOptions{MaxScopes: 2})
			if tt.notPri {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Zero(t, res.Outcome)
			} else {
				require.NoError(t, err)
				assert.Equal(t, TxIncomplete, res.Outcome)
			}
			assert.Equal(t, tt.gaps, gapHours(res.Gaps))
			assert.Empty(t, res.Records)
			_, scopes, _ := s.counts()
			assert.Equal(t, map[int64]int{memTestHour: 1}, scopes, "no later scope read")

			if tt.name != "a reply at the primary's seq beside an index mismatch" {
				snap := newTxSnapshot(t, s, captureLow, key.Hour, key.Hour+2)
				want, notPrimary := snap.expect(key, 2)
				requireTxExpected(t, want, notPrimary, res, err)
			}
		})
	}
}

// TestFindTransactionScopeBreachGapOrder pins where a read lists its scope-breach gaps:
// as the read yields their versions, before the gaps its mapping adds, the index mismatch first among them,
// and before the settlement's gaps.
// The misindexed block's same-key primary at 20, yielded before the primary, bounds the window, whose seq 15 no read yields.
func TestFindTransactionScopeBreachGapOrder(t *testing.T) {
	t.Parallel()

	breach := txBreachPack(t, seg1, 0, false, append(txBlock(txRecord(12, nil)), inHours(1, txBlock(txNote(13, nil), txNote(14, nil)))...))
	s := txHours(t, [][]byte{txMisindexed(t, 20), breach})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []string{"scope-breach@0/13", "scope-breach@0/14", "index@0", "index@0/12", "seq-gap@/15"}, gapHours(res.Gaps))
}

// TestTxTruthBreach requires the safety check to reject a TxUnmatched, and a TxMatched without a gap,
// whose comparison domain holds a version outside its pack's hour.
func TestTxTruthBreach(t *testing.T) {
	t.Parallel()

	tr := newTxTruth(txSeqRange(11, 14), 0)
	txTruthPack(t, tr, seg0, 0, nil, nil, txNote(11, nil), txRecord(12, nil), txNote(13, nil), inHour(1, txRecord(14, nil)))
	key := txKeyAt(12)
	want := fmt.Sprintf("pack %v of hour %d holds seq 14 of hour %d", seg0, memTestHour, memTestHour+1)
	for _, outcome := range []TxOutcome{TxUnmatched, TxMatched} {
		res := TxResult{Outcome: outcome}
		assert.Contains(t, tr.violations(key, 1, &res, nil), want, "%v", outcome)
	}
	res := TxResult{Outcome: TxIncomplete}
	assert.Empty(t, tr.violations(key, 1, &res, nil))
}
