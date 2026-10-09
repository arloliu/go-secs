package corpus

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// Pack ids of the read tests: the packs numbered 0, 1 and 2.
var (
	testPack0 = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x01, 0x80, 0, 0, 0, 0, 0, 0, 0x10}
	testPack1 = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x01, 0x80, 0, 0, 0, 0, 0, 0, 0x11}
	testPack2 = tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x01, 0x80, 0, 0, 0, 0, 0, 0, 0x12}
)

// testReadsJSON is reads.json written by hand from the tracepack corpus specification §5.10, every key present:
// a read in the error form and one in the success form, its filter and every key of an incomplete reason included.
const testReadsJSON = `[
{"id": "conflict-limit", "cites": ["SEM §7.4"], "packs": [0, 1], "order": "time", "max_conflicts": 1, "filter": {},
 "expect": {"error": "conflict-limit", "conflicts": [
   {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "4", "versions": [[0], [1]]}]}},
{"id": "two-packs", "cites": ["SEM §7.4", "CORPUS §5.10"], "packs": [2, 0], "order": "capture", "payloads": true,
 "max_conflicts": 3,
 "filter": {"time_from": "-5", "kinds": ["data"], "system_bytes": "AAAACQ=="},
 "expect": {
   "items": [{"seq": "9", "pack": 2, "block": 0, "conflict": false}, {"seq": "10", "pack": 0, "block": 1, "conflict": true}],
   "incomplete": [
     {"pack": 2, "reason": "coverage", "coverage": {"capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
       "seq_first": "9", "seq_last": "10", "time_start": "-1", "time_end": "7",
       "unknown": [{"tag": 9, "value_type": 2, "value": "AQI="}]}},
     {"pack": 2, "reason": "truncated", "offset": "500"},
     {"pack": 0, "reason": "corrupt-block", "block": 1, "offset": "300"}],
   "conflicts": [{"capture_id": "019a2b3c-4d5e-7002-8000-000000000002", "seq": "10", "versions": [[2], [0]]}],
   "footer_errors": [0]}}
]`

// canonicalText returns the canonical form of the hand-written JSON text s, its key order kept.
func canonicalText(t *testing.T, s string) []byte {
	t.Helper()

	b, err := Canonical([]byte(s))
	require.NoError(t, err)

	return b
}

// testCoverage returns the coverage entry of testReadsJSON.
func testCoverage() tracepack.Coverage {
	return tracepack.Coverage{
		CaptureID: new(testCaptureID), SeqFirst: new(uint64(9)), SeqLast: new(uint64(10)),
		TimeStart: new(int64(-1)), TimeEnd: new(int64(7)),
		Unknown: []tracepack.RawEntry{{Tag: 9, Type: 2, Value: []byte{1, 2}}},
	}
}

func TestReadsJSON(t *testing.T) {
	t.Parallel()

	data := canonicalText(t, testReadsJSON)
	var rs Reads
	require.NoError(t, Unmarshal(data, &rs))
	require.Len(t, rs, 2)

	limit, two := rs[0], rs[1]
	require.Equal(t, ErrorConflictLimit, limit.Expect.Error)
	require.Nil(t, limit.Expect.Items)
	require.Equal(t, [][]int{{0}, {1}}, limit.Expect.Conflicts[0].Versions)
	require.Equal(t, []int{2, 0}, two.Packs)
	require.True(t, two.Payloads)
	require.Equal(t, 3, two.MaxConflicts)
	require.Equal(t, []int{0}, *two.Expect.FooterErrors)
	require.Equal(t, "coverage", (*two.Expect.Incomplete)[0].Reason)
	require.Equal(t, 2, (*two.Expect.Incomplete)[0].Pack)

	q, opts, err := two.Query()
	require.NoError(t, err)
	require.True(t, q.Payloads)
	require.Equal(t, int64(-5), *q.Filter.TimeFrom)
	require.Equal(t, tracepack.MergeIterateOptions{Order: tracepack.OrderCapture, MaxConflicts: 3}, opts)

	// Marshal sorts by id.
	again, err := Reads{two, limit}.Marshal()
	require.NoError(t, err)
	require.Equal(t, string(data), string(again))
}

func TestReadExpectFrom(t *testing.T) {
	t.Parallel()

	var rs Reads
	require.NoError(t, Unmarshal(canonicalText(t, testReadsJSON), &rs))
	cov := testCoverage()
	run := ReadRun{
		Packs: []int{2, 0}, IDs: []tracepack.UUID{testPack2, testPack0},
		Coverage: [][]tracepack.Coverage{{cov}, nil}, MaxConflicts: 3,
		Items: []tracepack.Item{
			{Record: tracepack.Record{Seq: 9}, Pack: 0, Block: 0},
			{Record: tracepack.Record{Seq: 10}, Pack: 1, Block: 1, Conflict: true},
		},
		Result: tracepack.Result{
			// In the order found, which is not the corpus's.
			Incomplete: []tracepack.Defect{
				{Reason: tracepack.ReasonCorruptBlock, Pack: 1, Block: 1, Offset: 300},
				{Reason: tracepack.ReasonTruncated, Pack: 0, Block: -1, Offset: 500},
				{Reason: tracepack.ReasonCoverage, Pack: 0, Block: -1, Offset: -1, Coverage: &cov},
			},
			Conflicts:  []tracepack.Conflict{{CaptureID: testCaptureID, Seq: 10, Versions: [][]tracepack.UUID{{testPack2}, {testPack0}}}},
			FooterErrs: []tracepack.PackError{{Pack: 1, Err: errors.New("footer")}},
		},
	}
	got, err := ReadExpectFrom(&run)
	require.NoError(t, err)
	require.Equal(t, marshalJSON(t, rs[1].Expect), marshalJSON(t, got))

	limit := ReadRun{
		Packs: []int{0, 1}, IDs: []tracepack.UUID{testPack0, testPack1}, Coverage: make([][]tracepack.Coverage, 2), MaxConflicts: 1,
		Items: []tracepack.Item{{Record: tracepack.Record{Seq: 1}}},
		Result: tracepack.Result{
			Incomplete: []tracepack.Defect{{Reason: tracepack.ReasonTruncated, Block: -1, Offset: -1}},
			Conflicts:  []tracepack.Conflict{{CaptureID: testCaptureID, Seq: 4, Versions: [][]tracepack.UUID{{testPack0}, {testPack1}}}},
		},
		Err: fmt.Errorf("read: %w", tracepack.ErrReadLimit),
	}
	got, err = ReadExpectFrom(&limit)
	require.NoError(t, err)
	require.Equal(t, marshalJSON(t, rs[0].Expect), marshalJSON(t, got), "the items and reasons before the error are no part of it")

	empty := ReadRun{Packs: []int{1}, IDs: []tracepack.UUID{testPack1}, Coverage: make([][]tracepack.Coverage, 1)}
	got, err = ReadExpectFrom(&empty)
	require.NoError(t, err)
	require.JSONEq(t, `{"items": [], "incomplete": [], "conflicts": [], "footer_errors": []}`, marshalJSON(t, got))
}

func TestReadExpectFromRefuses(t *testing.T) {
	t.Parallel()

	base := func() ReadRun {
		return ReadRun{
			Packs: []int{2, 0}, IDs: []tracepack.UUID{testPack2, testPack0}, Coverage: make([][]tracepack.Coverage, 2), MaxConflicts: 1,
			Items: []tracepack.Item{{Record: tracepack.Record{Seq: 9}, Pack: 1}},
			Result: tracepack.Result{
				Conflicts:  []tracepack.Conflict{{CaptureID: testCaptureID, Seq: 4, Versions: [][]tracepack.UUID{{testPack2}, {testPack0}}}},
				FooterErrs: []tracepack.PackError{{Pack: 0}, {Pack: 1}},
			},
		}
	}
	ok := base()
	_, err := ReadExpectFrom(&ok)
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*ReadRun)
	}{
		{"another error", func(r *ReadRun) { r.Err = errors.New("i/o") }},
		{"a read limit without a bound", func(r *ReadRun) { r.MaxConflicts, r.Err = 0, tracepack.ErrReadLimit }},
		{"a read limit short of the bound", func(r *ReadRun) { r.MaxConflicts, r.Err = 2, tracepack.ErrReadLimit }},
		{"a removed hour", func(r *ReadRun) { r.Result.Removed = []int64{5} }},
		{"a reader limit", func(r *ReadRun) {
			r.Result.Incomplete = []tracepack.Defect{{Reason: tracepack.ReasonLimit, Block: -1, Offset: -1}}
		}},
		{"an index-mismatch without a block", func(r *ReadRun) {
			r.Result.Incomplete = []tracepack.Defect{{Reason: tracepack.ReasonIndexMismatch, Block: -1, Offset: -1}}
		}},
		{"a coverage defect the metadata lacks", func(r *ReadRun) {
			cov := testCoverage()
			r.Result.Incomplete = []tracepack.Defect{{Reason: tracepack.ReasonCoverage, Block: -1, Offset: -1, Coverage: &cov}}
		}},
		{"a defect of no reader", func(r *ReadRun) {
			r.Result.Incomplete = []tracepack.Defect{{Reason: tracepack.ReasonTruncated, Pack: 2, Block: -1, Offset: -1}}
		}},
		{"an item of no reader", func(r *ReadRun) { r.Items[0].Pack = 2 }},
		{"a version out of reader order", func(r *ReadRun) { r.Result.Conflicts[0].Versions = [][]tracepack.UUID{{testPack0, testPack2}} }},
		{"a version naming a pack twice", func(r *ReadRun) { r.Result.Conflicts[0].Versions = [][]tracepack.UUID{{testPack2, testPack2}} }},
		{"a version naming a pack not read", func(r *ReadRun) { r.Result.Conflicts[0].Versions = [][]tracepack.UUID{{testPack1}} }},
		{"footer errors out of reader order", func(r *ReadRun) { r.Result.FooterErrs = []tracepack.PackError{{Pack: 1}, {Pack: 0}} }},
		{"pack_ids missing", func(r *ReadRun) { r.IDs = r.IDs[:1] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			run := base()
			tt.edit(&run)
			_, err := ReadExpectFrom(&run)
			require.Error(t, err)
		})
	}
}

func TestReadsMarshalRefuses(t *testing.T) {
	t.Parallel()

	ok := func() ReadVector {
		return ReadVector{
			ID: "read", Cites: []string{"SEM §7.4"}, Packs: []int{1, 0}, Order: ReadOrderTime, MaxConflicts: 2,
			Expect: ReadExpect{
				Items:      &[]ReadItem{{Seq: 1, Pack: 0}},
				Incomplete: &[]PackIncomplete{{Pack: 1, Incomplete: Incomplete{Reason: "truncated"}}, {Pack: 0, Incomplete: Incomplete{Reason: "truncated"}}},
				Conflicts: []ConflictEntry{
					{CaptureID: testCaptureID.String(), Seq: 1, Versions: [][]int{{1}, {0}}},
				},
				FooterErrors: &[]int{1, 0},
			},
		}
	}
	errorForm := func(r *ReadVector) {
		r.Expect = ReadExpect{Error: ErrorConflictLimit, Conflicts: r.Expect.Conflicts}
		r.MaxConflicts = 1
	}
	r := ok()
	_, err := Reads{r}.Marshal()
	require.NoError(t, err)
	r = ok()
	errorForm(&r)
	_, err = Reads{r}.Marshal()
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*ReadVector)
	}{
		{"no cites", func(r *ReadVector) { r.Cites = nil }},
		{"no packs", func(r *ReadVector) { r.Packs = []int{} }},
		{"a pack twice", func(r *ReadVector) { r.Packs = []int{1, 1} }},
		{"an order", func(r *ReadVector) { r.Order = "seq" }},
		{"a negative bound", func(r *ReadVector) { r.MaxConflicts = -1 }},
		{"a filter", func(r *ReadVector) { r.Filter.Kinds = []string{} }},
		{"no conflicts", func(r *ReadVector) { r.Expect.Conflicts = nil }},
		{"no items", func(r *ReadVector) { r.Expect.Items = nil }},
		{"no footer errors", func(r *ReadVector) { r.Expect.FooterErrors = nil }},
		{"conflicts over the bound", func(r *ReadVector) {
			r.MaxConflicts, r.Expect.Conflicts = 1, append(r.Expect.Conflicts, r.Expect.Conflicts[0])
		}},
		{"an item of a pack not read", func(r *ReadVector) { (*r.Expect.Items)[0].Pack = 3 }},
		{"incomplete out of pack order", func(r *ReadVector) { (*r.Expect.Incomplete)[0].Pack = 0; (*r.Expect.Incomplete)[1].Pack = 1 }},
		{"footer errors out of pack order", func(r *ReadVector) { r.Expect.FooterErrors = &[]int{0, 1} }},
		{"a version out of pack order", func(r *ReadVector) { r.Expect.Conflicts[0].Versions = [][]int{{0, 1}} }},
		{"a capture_id", func(r *ReadVector) { r.Expect.Conflicts[0].CaptureID = "X" }},
		{"the error form without a bound", func(r *ReadVector) { errorForm(r); r.MaxConflicts = 0 }},
		{"the error form with items", func(r *ReadVector) { errorForm(r); r.Expect.Items = &[]ReadItem{} }},
		{"the error form short of the bound", func(r *ReadVector) { errorForm(r); r.MaxConflicts = 2 }},
		{"another error", func(r *ReadVector) { errorForm(r); r.Expect.Error = "limit" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := ok()
			tt.edit(&r)
			_, err := Reads{r}.Marshal()
			require.Error(t, err)
		})
	}

	_, err = Reads{ok(), ok()}.Marshal()
	require.Error(t, err, "an id twice")
}
