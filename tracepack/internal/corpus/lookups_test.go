package corpus

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// testOtherCapture is a capture of the lookup tests other than the primary's.
var testOtherCapture = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x02, 0x80, 0, 0, 0, 0, 0, 0, 0x03}

// testLookupsJSON is lookups.json written by hand from the tracepack corpus specification §5.11, every key present:
// a lookup in the error form; one whose result holds every key, every fact reason with its keys,
// a barrier boundary with and without gap bounds and a coverage fact with an unknown list;
// and an early return whose key holds no optional field: a primary in an undefined direction.
const testLookupsJSON = `[
{"id": "every-key", "cites": ["SEM §7.2", "CORPUS §5.11"],
 "source": {"view": [0, 1, 3], "evidence": [2], "complete": true},
 "key": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "5", "hour": "10"},
 "max_scopes": 3,
 "expect": {
   "outcome": "incomplete",
   "key": {"epoch": 1, "dir": "host-to-equipment", "session_id": 7, "system_bytes": "AAAABQ==", "stream": 1, "function": 1, "w": true},
   "window_end": "20",
   "records": [
     {"seq": "5", "hour": "10", "pack": 0, "block": 0, "conflict": true, "roles": ["primary"], "in_window": false, "bound": false},
     {"seq": "6", "hour": "10", "pack": 0, "block": 0, "conflict": false, "roles": ["candidate"], "in_window": true, "bound": false,
      "decidable": true, "eligible": true, "valid": false},
     {"seq": "7", "hour": "10", "pack": 1, "block": 0, "conflict": true, "roles": [], "in_window": true, "bound": false},
     {"seq": "20", "hour": "11", "pack": 3, "block": 0, "conflict": false,
      "roles": ["same-key-primary", "possible-primary", "closing", "outcome"], "in_window": false, "bound": true}],
   "gaps": [
     {"reason": "no-key"},
     {"reason": "conflict", "hours": ["10"], "seq": "5"},
     {"reason": "conflict", "hours": ["10", "11"], "seq": "5"},
     {"reason": "index", "hours": ["10"], "pack": 0, "block": 1, "offset": "300"},
     {"reason": "scope-breach", "hours": ["10"], "pack": 0, "block": 1, "seq": "7"},
     {"reason": "cold", "hours": ["11"]},
     {"reason": "conflicted", "hours": ["12"]},
     {"reason": "read", "hours": ["10"], "pack": 0, "defect": "truncated"},
     {"reason": "read", "hours": ["10"], "pack": 0, "offset": "500", "defect": "truncated"},
     {"reason": "read", "hours": ["10"], "pack": 0, "block": 2, "offset": "600", "defect": "corrupt-block"},
     {"reason": "coverage", "hours": ["10"], "pack": 1, "coverage": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
       "seq_first": "9", "seq_last": "10", "time_start": "-1", "time_end": "7",
       "unknown": [{"tag": 9, "value_type": 2, "value": "AQI="}]}},
     {"reason": "unevaluated", "hours": ["10"], "pack": 1},
     {"reason": "evidence"},
     {"reason": "barrier", "boundary": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "3",
       "boundary_kind": "stop-unclean", "ts": "-9", "epoch": 1, "gap_start": "-20", "gap_end": "100"}, "basis": ["epoch"]},
     {"reason": "barrier", "boundary": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000003", "seq": "3",
       "boundary_kind": "stop-unclean", "ts": "-9", "epoch": 1}, "basis": ["time"]},
     {"reason": "capture-boundary", "boundary": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "8",
       "boundary_kind": "start", "epoch": 1}},
     {"reason": "ordering-uncertain"},
     {"reason": "correlation"},
     {"reason": "seq-gap"},
     {"reason": "open-window"},
     {"reason": "unavailable", "hours": ["11"], "pack": 3, "block": 0, "seq": "12"},
     {"reason": "contradiction", "seq": "8"}],
   "searched": [{"hour": "10", "indexed": true, "packs": [0, 1]}, {"hour": "11", "indexed": false, "packs": [3]}],
   "conflicts": [{"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "5", "versions": [[0], [1]]}],
   "footer_errors": [{"hour": "10", "pack": 0}, {"hour": "10", "pack": 1}]}},
{"id": "key-without-fields", "cites": ["SEM §7.2"],
 "source": {"view": [1], "evidence": [], "complete": false},
 "key": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "5", "hour": "-1"},
 "max_scopes": 1,
 "expect": {
   "outcome": "incomplete",
   "key": {"epoch": 0, "dir": "unknown(9)"},
   "records": [{"seq": "5", "hour": "-1", "pack": 1, "block": 0, "conflict": false, "roles": ["primary"], "in_window": false, "bound": false}],
   "gaps": [{"reason": "no-key"}, {"reason": "cold", "hours": ["-1"]}],
   "searched": [{"hour": "-1", "indexed": false, "packs": [1]}], "conflicts": [], "footer_errors": []}},
{"id": "not-primary", "cites": ["SEM §7.2"],
 "source": {"view": [0], "evidence": [], "complete": true},
 "key": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "4", "hour": "10"},
 "max_scopes": 1,
 "expect": {"error": "not-primary"}}
]`

// testPack3 is the pack_id of pack 3 of the lookup tests, the view pack of hour 11.
var testPack3 = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x01, 0x80, 0, 0, 0, 0, 0, 0, 0x13}

// testNumbers names the packs of the lookup tests.
var testNumbers = map[tracepack.UUID]int{testPack0: 0, testPack1: 1, testPack2: 2, testPack3: 3}

// testBoundary returns a stop-unclean boundary of capture at seq 3, its ts -9, in epoch 1.
func testBoundary(capture tracepack.UUID, gapStart, gapEnd *int64) *tracepack.Boundary {
	return &tracepack.Boundary{
		Capture: capture, Seq: 3, Kind: tracepack.BoundaryKindStopUnclean, TS: -9, Epoch: 1, GapStart: gapStart, GapEnd: gapEnd,
	}
}

// everyKeyRun returns the lookup run whose projection is the lookup every-key of testLookupsJSON:
// its gaps in an order of discovery, with gaps that normalize away or collapse.
func everyKeyRun() LookupRun {
	cov := testCoverage()
	at := func(seq uint64) *uint64 { return &seq }
	primary := dataRecord(5, 1, nil)
	closing := tracepack.Record{Seq: 20, Epoch: 2}
	gaps := []tracepack.TxGap{
		{Reason: tracepack.TxGapUnavailable, Hours: []int64{11}, Pack: new(testPack3), Block: 0, Offset: -1, Seq: at(12)},
		{Reason: tracepack.TxGapNoKey, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5), Coverage: &cov},
		{Reason: tracepack.TxGapConflict, Hours: []int64{10, 11}, Block: -1, Offset: -1, Seq: at(5)},
		{Reason: tracepack.TxGapConflict, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5)},
		{Reason: tracepack.TxGapIndex, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5), Err: errors.New("early")},
		{Reason: tracepack.TxGapIndex, Hours: []int64{10}, Pack: new(testPack0), Block: 1, Offset: 300, Defect: tracepack.ReasonIndexMismatch},
		{Reason: tracepack.TxGapScopeBreach, Hours: []int64{10}, Pack: new(testPack0), Block: 1, Offset: -1, Seq: at(7)},
		{Reason: tracepack.TxGapRead, Hours: []int64{10}, Pack: new(testPack0), Block: 2, Offset: 600, Defect: tracepack.ReasonCorruptBlock},
		{Reason: tracepack.TxGapRead, Hours: []int64{10}, Pack: new(testPack0), Block: -1, Offset: 500, Defect: tracepack.ReasonTruncated},
		{Reason: tracepack.TxGapRead, Hours: []int64{10}, Pack: new(testPack0), Block: -1, Offset: -1, Defect: tracepack.ReasonTruncated},
		{Reason: tracepack.TxGapUnevaluated, Hours: []int64{10}, Pack: new(testPack1), Block: -1, Offset: -1},
		{Reason: tracepack.TxGapCold, Hours: []int64{11}, Block: -1, Offset: -1},
		{Reason: tracepack.TxGapConflicted, Hours: []int64{12}, Block: -1, Offset: -1},
		{Reason: tracepack.TxGapCoverage, Hours: []int64{10}, Pack: new(testPack1), Block: -1, Offset: -1, Coverage: &cov},
		{Reason: tracepack.TxGapBarrier, Block: -1, Offset: -1, Barrier: testBoundary(testCaptureID, new(int64(-20)), new(int64(100)))},
		{Reason: tracepack.TxGapBarrier, Block: -1, Offset: -1, Barrier: testBoundary(testOtherCapture, nil, nil)},
		{Reason: tracepack.TxGapCaptureBoundary, Block: -1, Offset: -1, Seq: at(8), Barrier: &tracepack.Boundary{
			Capture: testCaptureID, Seq: 8, Kind: tracepack.BoundaryKindStart, TS: 4, Epoch: 1,
		}},
		{Reason: tracepack.TxGapCaptureBoundary, Hours: []int64{10}, Pack: new(testPack1), Block: 0, Offset: -1, Seq: at(8), Barrier: &tracepack.Boundary{
			Capture: testCaptureID, Seq: 8, Kind: tracepack.BoundaryKindStart, TS: 5, Epoch: 1, GapStart: new(int64(1)),
		}},
		{Reason: tracepack.TxGapOrderingUncertain, Hours: []int64{10}, Pack: new(testPack0), Block: 0, Offset: -1, Seq: at(6)},
		{Reason: tracepack.TxGapEvidence, Block: -1, Offset: -1},
		{Reason: tracepack.TxGapCorrelation, Hours: []int64{10}, Block: -1, Offset: -1, Seq: at(5)},
		{Reason: tracepack.TxGapSeqGap, Block: -1, Offset: -1, Seq: at(13)},
		{Reason: tracepack.TxGapOpenWindow, Block: -1, Offset: -1},
		{Reason: tracepack.TxGapContradiction, Block: -1, Offset: -1, Seq: at(8)},
		{Reason: tracepack.TxGapContradiction, Block: -1, Offset: -1, Seq: at(8)},
	}

	return LookupRun{
		Key: tracepack.TxKey{Capture: testCaptureID, Seq: 5, Hour: 10}, MaxScopes: 3, Numbers: testNumbers,
		Result: tracepack.TxResult{
			Outcome: tracepack.TxIncomplete, Epoch: 1, Dir: tracepack.DirHostToEquipment, SessionID: 7, SystemBytes: [4]byte{0, 0, 0, 5},
			Stream: 1, Function: 1, StreamAvailable: true, W: true, WAvailable: true, WindowEnd: at(20),
			Records: []tracepack.TxRecord{
				{Record: primary, Hour: 10, Pack: testPack0, Block: 0, Conflict: true, Class: tracepack.TxPrimary},
				{
					Record: tracepack.Record{Seq: 6}, Hour: 10, Pack: testPack0, Class: tracepack.TxCandidate, InWindow: true,
					Decidable: true, Eligible: true,
				},
				{Record: tracepack.Record{Seq: 7}, Hour: 10, Pack: testPack1, Conflict: true, InWindow: true},
				{
					Record: closing, Hour: 11, Pack: testPack3, Bound: true,
					Class: tracepack.TxOutcomeRecord | tracepack.TxClosing | tracepack.TxPossiblePrimary | tracepack.TxSameKeyPrimary,
				},
			},
			Gaps: gaps,
			Searched: []tracepack.TxScope{
				{Hour: 10, Indexed: true, Packs: []tracepack.UUID{testPack0, testPack1}},
				{Hour: 11, Packs: []tracepack.UUID{testPack3}},
			},
			Conflicts:  []tracepack.Conflict{{CaptureID: testCaptureID, Seq: 5, Versions: [][]tracepack.UUID{{testPack0}, {testPack1}}}},
			FooterErrs: []tracepack.TxPackError{{Hour: 10, Pack: testPack0}, {Hour: 10, Pack: testPack1}},
		},
	}
}

func TestLookupsJSON(t *testing.T) {
	t.Parallel()

	data := canonicalText(t, testLookupsJSON)
	var ls Lookups
	require.NoError(t, Unmarshal(data, &ls))
	require.Len(t, ls, 3)

	every, minimal, notPrimary := ls[0], ls[1], ls[2]
	require.Len(t, *every.Expect.Gaps, 22)
	require.Equal(t, []string{RoleSameKeyPrimary, RolePossiblePrimary, RoleClosing, RoleOutcome}, (*every.Expect.Records)[3].Roles)
	require.False(t, *(*every.Expect.Records)[1].Valid)
	require.Equal(t, uint8(1), *every.Expect.Key.Function)
	require.Equal(t, &PrimaryKey{Dir: "unknown(9)"}, minimal.Expect.Key)
	require.Equal(t, []string{}, (*every.Expect.Records)[2].Roles)
	require.Equal(t, LookupExpect{Error: ErrorNotPrimary}, notPrimary.Expect)

	key, opts, err := every.Lookup()
	require.NoError(t, err)
	require.Equal(t, tracepack.TxKey{Capture: testCaptureID, Seq: 5, Hour: 10}, key)
	require.Equal(t, tracepack.TxOptions{MaxScopes: 3}, opts)

	// Marshal sorts the lookups by id, each version's roles by the role table, and the gaps by the comparator,
	// each distinct fact once.
	shuffled := every
	gaps := append([]Fact{}, *every.Expect.Gaps...)
	gaps = append(gaps, gaps[3], gaps[0])
	for i, j := 0, len(gaps)-1; i < j; i, j = i+1, j-1 {
		gaps[i], gaps[j] = gaps[j], gaps[i]
	}
	records := append([]LookupRecord{}, *every.Expect.Records...)
	records[3].Roles = []string{RoleOutcome, RoleClosing, RoleSameKeyPrimary, RolePossiblePrimary}
	shuffled.Expect.Gaps, shuffled.Expect.Records = &gaps, &records
	again, err := Lookups{notPrimary, shuffled, minimal}.Marshal()
	require.NoError(t, err)
	require.Equal(t, string(data), string(again))
	require.Equal(t, RoleOutcome, records[3].Roles[0], "Marshal does not reorder its input")
}

func TestLookupExpectFrom(t *testing.T) {
	t.Parallel()

	var ls Lookups
	require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &ls))

	run := everyKeyRun()
	got, err := LookupExpectFrom(&run)
	require.NoError(t, err)
	require.Equal(t, marshalJSON(t, ls[0].Expect), marshalJSON(t, got))

	run = LookupRun{Key: tracepack.TxKey{Capture: testCaptureID, Seq: 4, Hour: 10}, MaxScopes: 1, Err: fmt.Errorf("x: %w", tracepack.ErrNotPrimary)}
	got, err = LookupExpectFrom(&run)
	require.NoError(t, err)
	require.Equal(t, marshalJSON(t, ls[2].Expect), marshalJSON(t, got))

	// A primary in an undefined direction, with no HSMS field available, in a scope that is not indexed: an early return.
	primary := tracepack.Record{Seq: 5, Kind: tracepack.KindData, Dir: tracepack.Dir(9)}
	run = LookupRun{
		Key: tracepack.TxKey{Capture: testCaptureID, Seq: 5, Hour: -1}, MaxScopes: 1, Numbers: testNumbers,
		Result: tracepack.TxResult{
			Outcome: tracepack.TxIncomplete, Dir: tracepack.Dir(9),
			Records: []tracepack.TxRecord{{Record: primary, Hour: -1, Pack: testPack1, Class: tracepack.TxPrimary}},
			Gaps: []tracepack.TxGap{
				{Reason: tracepack.TxGapCold, Hours: []int64{-1}, Block: -1, Offset: -1},
				{Reason: tracepack.TxGapNoKey, Hours: []int64{-1}, Block: -1, Offset: -1, Seq: new(uint64(5))},
			},
			Searched: []tracepack.TxScope{{Hour: -1, Packs: []tracepack.UUID{testPack1}}},
		},
	}
	got, err = LookupExpectFrom(&run)
	require.NoError(t, err)
	require.Equal(t, marshalJSON(t, ls[1].Expect), marshalJSON(t, got))
}

func TestLookupExpectFromRefuses(t *testing.T) {
	t.Parallel()

	run := everyKeyRun()
	_, err := LookupExpectFrom(&run)
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*LookupRun)
	}{
		{"another error", func(r *LookupRun) { r.Err = tracepack.ErrReadLimit }},
		{"no outcome", func(r *LookupRun) { r.Result.Outcome = 0 }},
		{"a pack no source pack is", func(r *LookupRun) { r.Result.Records[1].Pack = testPackID }},
		{"an undefined role bit", func(r *LookupRun) { r.Result.Records[2].Class = 1 << 12 }},
		{"candidate flags without the role", func(r *LookupRun) { r.Result.Records[2].Eligible = true }},
		{"a first version not the primary's", func(r *LookupRun) { r.Result.Records = r.Result.Records[1:] }},
		{"a key field the primary does not have", func(r *LookupRun) { r.Result.SessionID = 8 }},
		{"a key field available without the bit", func(r *LookupRun) { r.Result.WAvailable = false }},
		{"versions out of order", func(r *LookupRun) {
			r.Result.Records[1], r.Result.Records[2] = r.Result.Records[2], r.Result.Records[1]
		}},
		{"versions of one seq out of hour order", func(r *LookupRun) { r.Result.Records[2].Record.Seq, r.Result.Records[2].Hour = 6, 9 }},
		{"scopes out of order", func(r *LookupRun) {
			r.Result.Searched[0], r.Result.Searched[1] = r.Result.Searched[1], r.Result.Searched[0]
		}},
		{"a conflict of no scope searched", func(r *LookupRun) {
			r.Result.Conflicts[0].Versions = [][]tracepack.UUID{{testPack2}, {testPack0}}
		}},
		{"a version out of view order", func(r *LookupRun) {
			r.Result.Conflicts[0].Versions = [][]tracepack.UUID{{testPack1, testPack0}}
		}},
		{"a footer error of an hour not searched", func(r *LookupRun) {
			r.Result.FooterErrs = []tracepack.TxPackError{{Hour: 12, Pack: testPack0}}
		}},
		{"a footer error of a pack not in its scope", func(r *LookupRun) {
			r.Result.FooterErrs = []tracepack.TxPackError{{Hour: 11, Pack: testPack0}}
		}},
		{"footer errors out of view order", func(r *LookupRun) {
			r.Result.FooterErrs = []tracepack.TxPackError{{Hour: 10, Pack: testPack1}, {Hour: 10, Pack: testPack0}}
		}},
		{"an outcome its facts contradict", func(r *LookupRun) { r.Result.Outcome = tracepack.TxMatched }},
		{"unmatched beside facts", func(r *LookupRun) {
			r.Result.Outcome, r.Result.Gaps = tracepack.TxUnmatched, r.Result.Gaps[:1]
		}},
		{"incomplete without facts", func(r *LookupRun) { r.Result.Gaps = nil }},
		{"matched without a valid match", func(r *LookupRun) {
			r.Result.Outcome, r.Result.Gaps = tracepack.TxMatched, r.Result.Gaps[:1]
		}},
		{"incomplete beside a valid match and no fact that makes it incomplete", func(r *LookupRun) {
			r.Result.Gaps, r.Result.Records[1].Valid = r.Result.Gaps[:1], true
		}},
		{"matched with two valid matches", func(r *LookupRun) { twoValidRun(r, tracepack.TxMatched) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			run := everyKeyRun()
			tt.edit(&run)
			_, err := LookupExpectFrom(&run)
			require.Error(t, err)
		})
	}

	// The same two valid matches without a fact give ambiguous.
	run = everyKeyRun()
	twoValidRun(&run, tracepack.TxAmbiguous)
	got, err := LookupExpectFrom(&run)
	require.NoError(t, err)
	require.Equal(t, "ambiguous", got.Outcome)
}

// twoValidRun edits the run of everyKeyRun into a result without facts whose versions at seqs 6 and 7 are valid matches,
// its outcome outcome.
func twoValidRun(r *LookupRun, outcome tracepack.TxOutcome) {
	r.Result.Outcome, r.Result.Gaps = outcome, nil
	r.Result.Records[1].Valid = true
	c := &r.Result.Records[2]
	c.Class, c.Decidable, c.Eligible, c.Valid = tracepack.TxCandidate, true, true, true
}

// scheduleFacts are the facts of the lookup every-key of testLookupsJSON that its schedule needs, and none other:
// its third hour scheduled, 12, conflicted.
func scheduleFacts() []Fact {
	return []Fact{{Reason: FactConflicted, Hours: hoursOf(12)}}
}

// twoValidLookup edits the lookup every-key of testLookupsJSON into a result without facts but scheduleFacts
// whose versions at seqs 6 and 7 are valid matches, its outcome outcome.
func twoValidLookup(l *LookupVector, outcome string) {
	l.Expect.Outcome, *l.Expect.Gaps = outcome, scheduleFacts()
	(*l.Expect.Records)[1].Valid = new(true)
	c := &(*l.Expect.Records)[2]
	c.Roles, c.Decidable, c.Eligible, c.Valid = []string{RoleCandidate}, new(true), new(true), new(true)
}

// moveHour11 edits the lookup every-key of testLookupsJSON so that what its read of hour 11 searched and kept,
// the scope, the version at seq 20 and its unavailable fact, are of hour instead.
func moveHour11(l *LookupVector, hour I64) {
	(*l.Expect.Searched)[1].Hour, (*l.Expect.Records)[3].Hour, (*l.Expect.Gaps)[20].Hours = hour, hour, hoursOf(int64(hour))
}

// closingConflictLookup edits the lookup every-key of testLookupsJSON so that its window end, seq 20,
// has a second version: a closing version in conflict, which cannot bound the window, its bound flag bound.
func closingConflictLookup(l *LookupVector, bound bool) {
	rs := *l.Expect.Records
	rs[3].Conflict = true
	closing := LookupRecord{Seq: 20, Hour: 11, Pack: 3, Block: 0, Conflict: true, Roles: []string{RoleClosing}, Bound: bound}
	*l.Expect.Records = slices.Concat(rs[:3], []LookupRecord{closing}, rs[3:])
}

func TestLookupHoursInRange(t *testing.T) {
	t.Parallel()

	var ls Lookups
	require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &ls))
	tests := []struct {
		name  string
		hour  int64
		count int
		ok    bool
	}{
		{"the last hour, one scope", tracepack.MaxTxHour, 1, true},
		{"the last hour, two scopes", tracepack.MaxTxHour, 2, false},
		{"the first hour", tracepack.MinTxHour, 1, true},
		{"before the first hour", tracepack.MinTxHour - 1, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := ls[2]
			l.Key.Hour, l.MaxScopes = I64(tt.hour), tt.count
			key, opts, err := l.Lookup()
			if !tt.ok {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.hour, key.Hour)
			require.Equal(t, tt.count, opts.MaxScopes)
		})
	}
}

func TestLookupsMarshalRefuses(t *testing.T) {
	t.Parallel()

	var ls Lookups
	require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &ls))
	base := func() LookupVector {
		var copied Lookups
		require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &copied))

		return copied[0]
	}

	tests := []struct {
		name string
		edit func(*LookupVector)
	}{
		{"no cites", func(l *LookupVector) { l.Cites = nil }},
		{"an empty view", func(l *LookupVector) { l.Source.View = []int{} }},
		{"no evidence", func(l *LookupVector) { l.Source.Evidence = nil }},
		{"a pack in view and evidence", func(l *LookupVector) { l.Source.Evidence = []int{1} }},
		{"a capture_id", func(l *LookupVector) { l.Key.CaptureID = "019A2B3C-4D5E-7002-8000-000000000002" }},
		{"no max_scopes", func(l *LookupVector) { l.MaxScopes = 0 }},
		{"another error", func(l *LookupVector) { l.Expect = LookupExpect{Error: "missing"} }},
		{"the error form with an outcome", func(l *LookupVector) { l.Expect = LookupExpect{Error: ErrorNotPrimary, Outcome: "unmatched"} }},
		{"an outcome", func(l *LookupVector) { l.Expect.Outcome = "found" }},
		{"no searched", func(l *LookupVector) { l.Expect.Searched = nil }},
		{"a dir", func(l *LookupVector) { l.Expect.Key.Dir = "up" }},
		{"stream without w", func(l *LookupVector) { l.Expect.Key.W = nil }},
		{"a role", func(l *LookupVector) { (*l.Expect.Records)[0].Roles = []string{"reply"} }},
		{"a role twice", func(l *LookupVector) { (*l.Expect.Records)[0].Roles = []string{RolePrimary, RolePrimary} }},
		{"no roles", func(l *LookupVector) { (*l.Expect.Records)[0].Roles = nil }},
		{"candidate flags without the role", func(l *LookupVector) { (*l.Expect.Records)[0].Valid = new(true) }},
		{"a candidate without flags", func(l *LookupVector) { (*l.Expect.Records)[1].Valid = nil }},
		{"a record of a pack not in view", func(l *LookupVector) { (*l.Expect.Records)[0].Pack = 2 }},
		{"records out of order", func(l *LookupVector) { (*l.Expect.Records)[1].Seq = 4 }},
		{"a fact of a pack not in view", func(l *LookupVector) { (*l.Expect.Gaps)[3].Pack = new(2) }},
		{"a fact the table refuses", func(l *LookupVector) { (*l.Expect.Gaps)[0].Seq = new(U64(5)) }},
		{"an outcome its facts contradict", func(l *LookupVector) { l.Expect.Outcome = "matched" }},
		{"searched out of order", func(l *LookupVector) { (*l.Expect.Searched)[1].Hour = 9 }},
		{"a conflict of a pack not in view", func(l *LookupVector) { (*l.Expect.Conflicts)[0].Versions = [][]int{{2}} }},
		{"a footer error out of order", func(l *LookupVector) {
			*l.Expect.FooterErrors = []ScopeFooterError{{Hour: 10, Pack: 1}, {Hour: 10, Pack: 0}}
		}},
		{"a footer error of an hour not searched", func(l *LookupVector) { (*l.Expect.FooterErrors)[1].Hour = 9 }},
		{"a footer error of a pack not in its scope", func(l *LookupVector) { (*l.Expect.FooterErrors)[1].Hour = 11 }},
		{"no primary role at the primary's seq", func(l *LookupVector) { (*l.Expect.Records)[0].Roles = []string{} }},
		{"a primary role at another seq", func(l *LookupVector) { (*l.Expect.Records)[2].Roles = []string{RolePrimary} }},
		{"no window end", func(l *LookupVector) { l.Expect.WindowEnd = nil }},
		{"another window end", func(l *LookupVector) { l.Expect.WindowEnd = new(U64(7)) }},
		{"a version in the window marked outside it", func(l *LookupVector) { (*l.Expect.Records)[1].InWindow = false }},
		{"the end marked in the window", func(l *LookupVector) { (*l.Expect.Records)[3].InWindow = true }},
		{"the end not bounding", func(l *LookupVector) { (*l.Expect.Records)[3].Bound = false }},
		{"a bound below the end", func(l *LookupVector) { (*l.Expect.Records)[2].Bound = true }},
		{"matched without a valid match", func(l *LookupVector) { l.Expect.Outcome, *l.Expect.Gaps = "matched", scheduleFacts() }},
		{"ambiguous with one valid match", func(l *LookupVector) {
			l.Expect.Outcome, *l.Expect.Gaps, (*l.Expect.Records)[1].Valid = "ambiguous", scheduleFacts(), new(true)
		}},
		{"unmatched beside a valid match", func(l *LookupVector) {
			l.Expect.Outcome, *l.Expect.Gaps, (*l.Expect.Records)[1].Valid = "unmatched", scheduleFacts(), new(true)
		}},
		{"matched with two valid matches", func(l *LookupVector) { twoValidLookup(l, "matched") }},
		{"bound on a version that cannot bound the window", func(l *LookupVector) { closingConflictLookup(l, true) }},
		{"a scheduled hour neither searched nor conflicted", func(l *LookupVector) { l.MaxScopes = 4 }},
		{"a searched hour not scheduled", func(l *LookupVector) { moveHour11(l, 13) }},
		{"a conflicted hour also searched", func(l *LookupVector) { moveHour11(l, 12) }},
		{"a conflicted hour not scheduled", func(l *LookupVector) { (*l.Expect.Gaps)[6].Hours = hoursOf(13) }},
		{"hours scheduled out of the range of a lookup", func(l *LookupVector) { l.Key.Hour, l.MaxScopes = I64(tracepack.MaxTxHour), 2 }},
		{"a record of an hour not searched", func(l *LookupVector) { (*l.Expect.Records)[3].Hour = 12 }},
		{"a record of a pack not in its scope", func(l *LookupVector) { (*l.Expect.Records)[3].Pack = 0 }},
		{"a fact of an hour not searched", func(l *LookupVector) { (*l.Expect.Gaps)[11].Hours = hoursOf(12) }},
		{"a fact of a pack not in its scope", func(l *LookupVector) { (*l.Expect.Gaps)[20].Pack = new(0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := base()
			tt.edit(&l)
			_, err := Lookups{l}.Marshal()
			require.Error(t, err)
		})
	}

	_, err := Lookups{ls[0], ls[0]}.Marshal()
	require.Error(t, err, "an id twice")

	// The controls of the refusals above: two valid matches beside a conflicted hour are ambiguous,
	// and a closing version in conflict at the window's end does not bound it.
	l := base()
	twoValidLookup(&l, "ambiguous")
	_, err = Lookups{l}.Marshal()
	require.NoError(t, err)
	l = base()
	closingConflictLookup(&l, false)
	_, err = Lookups{l}.Marshal()
	require.NoError(t, err)

	// The early return of key-without-fields: its shape is checked too.
	early := func() LookupVector {
		var copied Lookups
		require.NoError(t, Unmarshal(canonicalText(t, testLookupsJSON), &copied))

		return copied[1]
	}
	_, err = Lookups{early()}.Marshal()
	require.NoError(t, err)
	earlyTests := []struct {
		name string
		edit func(*LookupVector)
	}{
		{"a window end", func(l *LookupVector) { l.Expect.WindowEnd = new(U64(6)) }},
		{"a version of another seq", func(l *LookupVector) {
			*l.Expect.Records = append(*l.Expect.Records,
				LookupRecord{Seq: 6, Hour: -1, Pack: 1, Roles: []string{}, InWindow: true})
		}},
		{"the primary in the window", func(l *LookupVector) { (*l.Expect.Records)[0].InWindow = true }},
		{"a later scope searched", func(l *LookupVector) {
			*l.Expect.Searched = append(*l.Expect.Searched, SearchedScope{Hour: 0, Packs: []int{}})
		}},
		{"a fact it does not report", func(l *LookupVector) { *l.Expect.Gaps = append(*l.Expect.Gaps, Fact{Reason: FactOpenWindow}) }},
		{"a conflict above the primary", func(l *LookupVector) {
			*l.Expect.Gaps = append(*l.Expect.Gaps, Fact{Reason: FactConflict, Hours: hoursOf(-1), Seq: new(U64(6))})
		}},
	}
	for _, tt := range earlyTests {
		t.Run("early return "+tt.name, func(t *testing.T) {
			t.Parallel()

			l := early()
			tt.edit(&l)
			_, err := Lookups{l}.Marshal()
			require.Error(t, err)
		})
	}
}
