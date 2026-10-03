package tracepack

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordFlags describes each record as recordSeqs does, followed by the flags set among
// "in" (InWindow), "bound", "eligible", "decidable" and "valid", in order.
func recordFlags(recs []TxRecord) []string {
	out := recordSeqs(recs)
	for i := range recs {
		r := &recs[i]
		for _, f := range []struct {
			set  bool
			name string
		}{{r.InWindow, "in"}, {r.Bound, "bound"}, {r.Eligible, "eligible"}, {r.Decidable, "decidable"}, {r.Valid, "valid"}} {
			if f.set {
				out[i] += " " + f.name
			}
		}
	}

	return out
}

// gapSeqs describes each gap as "<reason>" or "<reason>@<seq>", in order.
func gapSeqs(gaps []TxGap) []string {
	out := make([]string, len(gaps))
	for i := range gaps {
		out[i] = gaps[i].Reason.String()
		if gaps[i].Seq != nil {
			out[i] += fmt.Sprintf("@%d", *gaps[i].Seq)
		}
	}

	return out
}

// txAt returns r moved to seq, its time blockTestHour + seq.
func txAt(seq uint64, r Record) Record {
	r.Seq, r.TSUTCNs = seq, blockTestHour+int64(seq)

	return r
}

// txSocketClose returns a socket-close transport event of epoch txTestEpoch at seq: a closing record.
func txSocketClose(t testing.TB, seq uint64) Record {
	t.Helper()

	return testEventRecord(t, seq, blockTestHour+int64(seq), txTestEpoch, &TransportEvent{Event: EventSocketClose})
}

// TestFindTransactionOutcomes looks up txRecord's primary at 12 (S1F3, stream available) in one scope
// whose records leave no condition but the window's own: indexed, every pack evaluated, no coverage entry,
// no capture-boundary, ordering-uncertain or correlation-incomplete record.
// Each case checks the outcome, the window's end, every kept version's class and flags, and the gaps:
// valid matches (F+1 and F0 with the primary's stream), mismatches (another stream, a wrong-stream F0),
// control records whose bytes read like a reply, candidates after the window, two valid matches,
// matches beside a seq gap or a possible reply, undecidable candidates,
// possible same-key primaries before and after a reply, and outcome records inside and after the window.
func TestFindTransactionOutcomes(t *testing.T) {
	t.Parallel()

	stream := func(s byte) func(p []byte) { return func(p []byte) { p[fieldByte6Off] = s } }
	function := func(f byte) func(p []byte) { return func(p []byte) { p[fieldFunctionOff] = f } }
	noStream := func(r *Record) { r.FieldValidity &^= FieldValidityStreamAndW }
	// lacks returns a primary-direction S1F3 at seq without the fields of v: a possible same-key primary.
	lacks := func(seq uint64, v FieldValidity) Record {
		return txRecord(seq, func(r *Record) { r.FieldValidity &^= v })
	}
	// lookalike returns a control record at seq whose bytes 6-7 read S1F<f> and whose SType is not Reject.req.
	lookalike := func(seq uint64, f byte) Record {
		return txAt(seq, txControl(txFrame(func(p []byte) { p[fieldByte6Off], p[fieldFunctionOff], p[fieldSTypeOff] = 0x01, f, 0 })))
	}
	t3 := testEventRecord(t, 15, blockTestHour+15, txTestEpoch, txT3(nil))
	type outcomeCase struct {
		name    string
		primary func(r *Record)
		recs    []Record
		outcome TxOutcome
		end     uint64
		records []string
		gaps    []string
	}
	tests := []outcomeCase{
		{name: "a valid match", recs: []Record{txReply(13, nil), txRecord(14, nil)}, outcome: TxMatched, end: 14,
			records: []string{"13@0 candidate in eligible decidable valid", "14@0 same-key-primary bound"}},
		{name: "another stream", recs: []Record{txReply(13, txFrame(stream(2))), txRecord(14, nil)}, outcome: TxUnmatched, end: 14,
			records: []string{"13@0 candidate in eligible decidable", "14@0 same-key-primary bound"}},
		{name: "an F0 abort", recs: []Record{txReply(13, txFrame(function(0))), txRecord(14, nil)}, outcome: TxMatched, end: 14,
			records: []string{"13@0 candidate in eligible decidable valid", "14@0 same-key-primary bound"}},
		{name: "a wrong-stream F0", recs: []Record{txReply(13, txFrame(func(p []byte) { stream(2)(p); function(0)(p) })), txRecord(14, nil)},
			outcome: TxUnmatched, end: 14,
			records: []string{"13@0 candidate in eligible decidable", "14@0 same-key-primary bound"}},
		{name: "control records that read like F+1 and F0", recs: []Record{lookalike(13, 4), lookalike(14, 0), txRecord(15, nil)},
			outcome: TxUnmatched, end: 15, records: []string{"15@0 same-key-primary bound"}},
		{name: "a candidate after the window's end", recs: []Record{txRecord(13, nil), txReply(14, nil)}, outcome: TxUnmatched, end: 13,
			records: []string{"13@0 same-key-primary bound", "14@0 candidate decidable"}},
		{name: "two valid matches", recs: []Record{txReply(13, nil), txReply(14, txFrame(function(0))), txRecord(15, nil)},
			outcome: TxAmbiguous, end: 15, records: []string{
				"13@0 candidate in eligible decidable valid", "14@0 candidate in eligible decidable valid", "15@0 same-key-primary bound",
			}},
		{name: "a match beside a seq gap", recs: []Record{txReply(13, nil), txRecord(15, nil)}, outcome: TxMatched, end: 15,
			records: []string{"13@0 candidate in eligible decidable valid", "15@0 same-key-primary bound"},
			gaps:    []string{"seq-gap@14"}},
		{name: "a match beside a possible reply",
			recs:    []Record{txReply(13, nil), txReply(14, func(r *Record) { r.FieldValidity &^= FieldValiditySystemBytes }), txRecord(15, nil)},
			outcome: TxMatched, end: 15,
			records: []string{"13@0 candidate in eligible decidable valid", "14@0 possible-reply in", "15@0 same-key-primary bound"},
			gaps:    []string{"unavailable@14"}},
		{name: "a candidate whose function is unavailable", recs: []Record{
			txReply(13, func(r *Record) { r.FieldValidity &^= FieldValidityFunction }), txRecord(14, nil),
		}, outcome: TxIncomplete, end: 14,
			records: []string{"13@0 candidate in eligible", "14@0 same-key-primary bound"}, gaps: []string{"unavailable@13"}},
		{name: "a one-seq window whose seq is missing", recs: []Record{txRecord(14, nil)}, outcome: TxIncomplete, end: 14,
			records: []string{"14@0 same-key-primary bound"}, gaps: []string{"seq-gap@13"}},
		{name: "an undecidable candidate", recs: []Record{txReply(13, noStream), txRecord(14, nil)}, outcome: TxIncomplete, end: 14,
			records: []string{"13@0 candidate in eligible", "14@0 same-key-primary bound"}, gaps: []string{"unavailable@13"}},
		{name: "the primary's stream unavailable", primary: noStream, recs: []Record{txReply(13, nil), txRecord(14, nil)},
			outcome: TxIncomplete, end: 14,
			records: []string{"13@0 candidate in eligible", "14@0 same-key-primary bound"}, gaps: []string{"unavailable@13"}},
		{name: "a socket-close bounds the window", recs: []Record{txReply(13, nil), txSocketClose(t, 14), txReply(15, nil)},
			outcome: TxMatched, end: 14,
			records: []string{"13@0 candidate in eligible decidable valid", "14@0 closing bound", "15@0 candidate decidable"}},
		{name: "outcome records inside and after the window", recs: []Record{txControl(nil), txRecord(14, nil), t3},
			outcome: TxUnmatched, end: 14,
			records: []string{"13@0 outcome in", "14@0 same-key-primary bound", "15@0 outcome"}},
		{name: "a reply before a possible primary", recs: []Record{txReply(13, nil), lacks(14, FieldValiditySessionID), txRecord(15, nil)},
			outcome: TxMatched, end: 15, records: []string{
				"13@0 candidate in eligible decidable valid", "14@0 possible-primary in", "15@0 same-key-primary bound",
			}},
		{name: "a known mismatch rules out a possible primary",
			recs: []Record{txRecord(13, func(r *Record) {
				r.FieldValidity &^= FieldValiditySessionID
				txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 })(r)
			}), txReply(14, nil), txRecord(15, nil)},
			outcome: TxMatched, end: 15,
			records: []string{"14@0 candidate in eligible decidable valid", "15@0 same-key-primary bound"}},
	}
	// A reply after a possible same-key primary, which lacks one field each time.
	lacking := []struct {
		name string
		rec  Record
		gaps []string
	}{
		{name: "SessionID", rec: lacks(13, FieldValiditySessionID), gaps: []string{"unavailable@14"}},
		{name: "System Bytes", rec: lacks(13, FieldValiditySystemBytes), gaps: []string{"unavailable@14"}},
		{name: "function", rec: lacks(13, FieldValidityFunction), gaps: []string{"unavailable@14"}},
		{name: "direction", rec: txRecord(13, func(r *Record) { r.Dir = DirUnknown }), gaps: []string{"unavailable@13", "unavailable@14"}},
	}
	after := make([]outcomeCase, len(lacking))
	for i, c := range lacking {
		class := "possible-primary in"
		if c.rec.Dir == DirUnknown {
			class = "possible-reply|possible-primary in"
		}
		after[i] = outcomeCase{
			name: "a reply after a possible primary without " + c.name, recs: []Record{c.rec, txReply(14, nil), txRecord(15, nil)},
			outcome: TxIncomplete, end: 15,
			records: []string{"13@0 " + class, "14@0 candidate in decidable", "15@0 same-key-primary bound"}, gaps: c.gaps,
		}
	}
	for _, tt := range slices.Concat(tests, after) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recs := append([]Record{txRecord(11, nil), txRecord(12, tt.primary)}, tt.recs...)
			s := txSource(t, true, txPack(t, seg0, nil, txBlock(recs...)))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, append([]string{"12@0 primary"}, tt.records...), recordFlags(res.Records))
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)))
			for _, g := range res.Gaps {
				if g.Reason != TxGapUnavailable {
					continue
				}
				i := slices.IndexFunc(res.Records, func(r TxRecord) bool { return r.Record.Seq == *g.Seq })
				require.GreaterOrEqual(t, i, 0)
				assert.Equal(t, TxGap{
					Reason: TxGapUnavailable, Hours: []int64{memTestHour}, Pack: new(seg0), Block: res.Records[i].Block, Offset: -1,
					Seq: g.Seq, Err: g.Err,
				}, g)
				require.Error(t, g.Err)
			}
		})
	}
}

// TestFindTransactionWindowShrinks reads, in the primary's scope, a window bounded at 20 with a missing seq 15,
// a T3 at 13, a valid reply at 14, a possible same-key primary at 16, a reply after it at 17, a possible reply at 18
// and a Reject.req at 19;
// the next scope holds a same-key primary at 15, its time after 16 to 20 by a backward clock step.
// Reading one scope, the window is (12, 20): the seq gap at 15, and the reply after the possible primary
// and the possible reply each an unavailable gap, beside the match.
// Reading both, the window shrinks to (12, 15) and is covered:
// the records above 15 are classified again, outside the window, and the match has no gap beside it.
func TestFindTransactionWindowShrinks(t *testing.T) {
	t.Parallel()

	first := txPack(t, seg0, nil, txBlock(
		txRecord(12, nil),
		testEventRecord(t, 13, blockTestHour+13, txTestEpoch, txT3(nil)),
		txReply(14, nil),
		txRecord(16, func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }),
		txReply(17, nil),
		txReply(18, func(r *Record) { r.Dir = DirUnknown }),
		txAt(19, txControl(nil)),
		txRecord(20, nil),
	))
	later := txPackIn(t, seg1, 1, nil, txBlock(txRecord(15, nil)))
	tests := []struct {
		name    string
		scopes  int
		end     uint64
		records []string
		gaps    []string
	}{
		{name: "one scope", scopes: 1, end: 20, records: []string{
			"12@0 primary", "13@0 outcome in", "14@0 candidate in eligible decidable valid", "16@0 possible-primary in",
			"17@0 candidate in decidable", "18@0 possible-reply in", "19@0 outcome in", "20@0 same-key-primary bound",
		}, gaps: []string{"seq-gap@15", "unavailable@17", "unavailable@18"}},
		{name: "two scopes", scopes: 2, end: 15, records: []string{
			"12@0 primary", "13@0 outcome in", "14@0 candidate in eligible decidable valid", "15@1 same-key-primary bound",
			"16@0 possible-primary", "17@0 candidate decidable", "18@0 possible-reply", "19@0 outcome", "20@0 same-key-primary",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := findTx(t, t.Context(), txHours(t, [][]byte{first}, [][]byte{later}), txKeyAt(12), TxOptions{MaxScopes: tt.scopes})
			require.NoError(t, err)
			assert.Equal(t, TxMatched, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, tt.records, recordFlags(res.Records))
			assert.Equal(t, tt.gaps, nilIfEmpty(gapSeqs(res.Gaps)))
		})
	}
}

// TestFindTransactionOpenWindow reads three scopes, the two after the primary's empty:
// MaxScopes counts them, each is read and searched, and no record bounds the window,
// so the match is reported beside a TxGapOpenWindow gap, its record inside the unbounded window.
func TestFindTransactionOpenWindow(t *testing.T) {
	t.Parallel()

	s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))}, nil, nil)
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
	require.NoError(t, err)
	assert.Equal(t, TxMatched, res.Outcome)
	assert.Nil(t, res.WindowEnd)
	assert.Equal(t, []string{"12@0 primary", "13@0 candidate in eligible decidable valid"}, recordFlags(res.Records))
	require.Equal(t, []string{"open-window"}, gapSeqs(res.Gaps))
	assert.Equal(t, TxGap{Reason: TxGapOpenWindow, Block: -1, Offset: -1, Err: res.Gaps[0].Err}, res.Gaps[0])
	require.ErrorContains(t, res.Gaps[0].Err, "hours [497222, 497225)")
	assert.Len(t, res.Searched, 3)
	_, scopes, _ := s.counts()
	assert.Equal(t, map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1}, scopes)
}

// TestFindTransactionIncompleteBesideMatch finds a valid match beside a conflict after the window's end,
// within the scope's read or across two scope reads, and beside a block whose records disagree with its F-2 entry:
// each makes the outcome TxIncomplete, the match still flagged valid.
func TestFindTransactionIncompleteBesideMatch(t *testing.T) {
	t.Parallel()

	match := txBlock(txRecord(12, nil), txReply(13, nil), txRecord(14, nil), txSeqs(20)[0])
	tests := []struct {
		name  string
		hours [][][]byte
		gaps  []string
	}{
		{name: "a conflict within the read", hours: [][][]byte{{
			txPack(t, seg0, nil, match), txPack(t, seg1, nil, changed(txBlock(txSeqs(20)...), 1, 20)),
		}}, gaps: []string{"conflict@20"}},
		{name: "a conflict across reads", hours: [][][]byte{
			{txPack(t, seg0, nil, match)},
			{txPackIn(t, seg1, 1, nil, txBlock(txSeqs(20)...))},
		}, gaps: []string{"conflict@20"}},
		{name: "a block that disagrees with its F-2 entry", hours: [][][]byte{{
			txMisindexed(t, 30), txPack(t, seg1, nil, match),
		}}, gaps: []string{"index", "index@12"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := findTx(t, t.Context(), txHours(t, tt.hours...), txKeyAt(12), TxOptions{MaxScopes: len(tt.hours)})
			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, new(uint64(14)), res.WindowEnd)
			assert.Equal(t, tt.gaps, gapSeqs(res.Gaps))
			i := slices.IndexFunc(res.Records, func(r TxRecord) bool { return r.Record.Seq == 13 })
			require.GreaterOrEqual(t, i, 0)
			assert.True(t, res.Records[i].Valid)
		})
	}
}

// TestFindTransactionRecordsOrder reads, in the primary's scope, replies at 14 and 16, the latter in two versions,
// and, in the next scope, a reply at 13 and 15, its time later by a backward clock step, and another version of 14:
// the records are in seq order, then hour of the read, then the order in which the read yielded them.
// The conflicts at 14 and 16 make the outcome TxIncomplete.
func TestFindTransactionRecordsOrder(t *testing.T) {
	t.Parallel()

	first := txBlock(txRecord(12, nil), txReply(14, nil), txReply(16, nil))
	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, first), txPack(t, seg1, nil, changed(txBlock(txReply(16, nil)), 1, 16))},
		[][]byte{txPackIn(t, seg2, 1, nil, txBlock(txReply(13, nil), txReply(14, nil), txReply(15, nil)))})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
	require.NoError(t, err)
	// Each conflicting version carries the flags its own bytes give.
	assert.Equal(t, []string{
		"12@0 primary", "13@1 candidate in eligible decidable valid",
		"14@0 candidate! in eligible decidable valid", "14@1 candidate! in eligible decidable valid",
		"15@1 candidate in eligible decidable valid", "16@0 candidate! in eligible decidable valid", "16@0 none! in",
	}, recordFlags(res.Records))
	packs := make([]string, len(res.Records))
	for i, r := range res.Records {
		packs[i] = fmt.Sprintf("%d:%x", r.Record.Seq, r.Pack[0])
	}
	assert.Equal(t, "12:50 13:52 14:50 14:52 15:52 16:50 16:51", strings.Join(packs, " "))
	assert.Equal(t, TxIncomplete, res.Outcome)
}

// TestFindTransactionDisputedBound reads a window bound in conflict across two scope reads:
// a socket-close at 14 bounds nothing, so the window ends at the same-key primary at 16,
// while a same-key primary at 14 bounds the window whether or not it conflicts.
// Either conflict makes the outcome TxIncomplete.
func TestFindTransactionDisputedBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bound   Record
		end     uint64
		records []string
	}{
		{name: "a socket-close", bound: txSocketClose(t, 14), end: 16, records: []string{
			"12@0 primary", "13@0 candidate in eligible decidable valid", "14@0 closing! in", "16@0 same-key-primary bound",
		}},
		{name: "a same-key primary", bound: txRecord(14, nil), end: 14, records: []string{
			"12@0 primary", "13@0 candidate in eligible decidable valid", "14@0 same-key-primary! bound", "16@0 same-key-primary",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t,
				[][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil), tt.bound, txNote(15, nil), txRecord(16, nil)))},
				[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txNote(14, nil)))})
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, new(tt.end), res.WindowEnd)
			assert.Equal(t, tt.records, recordFlags(res.Records))
			assert.Equal(t, []string{"conflict@14"}, gapSeqs(res.Gaps))
		})
	}
}

// TestFindTransactionCoverageAcrossReads bounds the window (12, 16) with seqs 13 and 15 read in the primary's scope
// and 14 in the next: the reads together cover the window, so no seq gap is listed and the outcome is TxUnmatched.
func TestFindTransactionCoverageAcrossReads(t *testing.T) {
	t.Parallel()

	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txNote(15, nil), txRecord(16, nil)))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txNote(14, nil)))})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
	require.NoError(t, err)
	assert.Equal(t, TxUnmatched, res.Outcome)
	assert.Equal(t, new(uint64(16)), res.WindowEnd)
	assert.Empty(t, res.Gaps)
}

// TestFindTransactionBoundVersion reads two versions of the window's end, 14, in one scope read:
// a same-key primary and an annotation, in either order.
// Only the same-key primary's version is the bound; the conflict makes the outcome TxIncomplete.
func TestFindTransactionBoundVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		first   Record
		second  Record
		records []string
	}{
		{name: "the same-key primary first", first: txRecord(14, nil), second: txNote(14, nil),
			records: []string{"14@0 same-key-primary! bound", "14@0 none!"}},
		{name: "the annotation first", first: txNote(14, nil), second: txRecord(14, nil),
			records: []string{"14@0 none!", "14@0 same-key-primary! bound"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true,
				txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil), tt.first)),
				txPack(t, seg1, nil, txBlock(tt.second)))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, new(uint64(14)), res.WindowEnd)
			assert.Equal(t, append([]string{"12@0 primary", "13@0 candidate in eligible decidable valid"}, tt.records...),
				recordFlags(res.Records))
			assert.Equal(t, []string{"conflict@14"}, gapSeqs(res.Gaps))
		})
	}
}
