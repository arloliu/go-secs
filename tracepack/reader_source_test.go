package tracepack

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewReaderSource groups readers of two captures by the UTC hour of their period:
// Readers returns an hour's readers in the order given, whatever their capture, in a fresh slice, none for an hour without readers,
// and ctx's error once ctx is done.
// Packs of role repair, of the retired role 5 and of a role this package does not define are read like any pack given.
// NewReaderSource rejects, wrapping ErrInvalidQuery, a nil reader, a Reader given twice, a repeated pack_id,
// readers of two tool_ids, a period empty or spanning two hours, and an extract.
func TestNewReaderSource(t *testing.T) {
	t.Parallel()

	steps := txBlock(txSeqs(12, 13)...)
	low := txPack(t, seg0, nil, steps)
	high := withIDs(t, txPack(t, seg1, nil, steps), seg1, captureHigh)
	next := txPackIn(t, seg2, 1, nil, steps)
	retired := setMetaRole(t, txPack(t, seg3, nil, steps), packRoleRetired)
	unknown := setMetaRole(t, txPack(t, memA, nil, steps), 9)
	repair := setMetaRole(t, txPack(t, memB, func(m *PackMeta) { m.Supersedes = []UUID{{0xD0}} }, steps), uint8(PackRoleRepair))

	readers := openAll(t, low, high, next, retired, unknown, repair)
	src, err := NewReaderSource(readers)
	require.NoError(t, err)
	got, err := src.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg0, seg1, seg3, memA, memB}, packIDs(got), "every capture and role, in the order given")
	assert.Same(t, readers[0], got[0])
	got[0] = nil
	again, err := src.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Same(t, readers[0], again[0], "a fresh slice each call")
	got, err = src.Readers(t.Context(), memTestHour+1)
	require.NoError(t, err)
	assert.Equal(t, []UUID{seg2}, packIDs(got))
	got, err = src.Readers(t.Context(), memTestHour+2)
	require.NoError(t, err)
	assert.Empty(t, got)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = src.Readers(ctx, memTestHour)
	require.ErrorIs(t, err, context.Canceled)

	empty, err := NewReaderSource(nil)
	require.NoError(t, err)
	got, err = empty.Readers(t.Context(), memTestHour)
	require.NoError(t, err)
	assert.Empty(t, got)

	r := mustOpen(t, low, ReaderOptions{})
	tests := []struct {
		name    string
		readers []*Reader
		msg     string
	}{
		{name: "a nil reader", readers: []*Reader{r, nil}, msg: "reader 1 is nil"},
		{name: "a Reader twice", readers: []*Reader{r, r}, msg: "readers 0 and 1 are the same Reader"},
		{name: "a repeated pack_id", readers: openAll(t, low, low), msg: "readers 0 and 1 hold the same pack_id"},
		{name: "two tool_ids", readers: openAll(t, low, txPack(t, seg1, func(m *PackMeta) { m.ToolID = "EQP-2" }, steps)),
			msg: `reader 0 holds tool_id "tool", reader 1 tool_id "EQP-2"`},
		{name: "a period spanning two hours", readers: openAll(t, low, txPack(t, seg1, func(m *PackMeta) { m.PeriodEnd += hourNs }, steps)),
			msg: "reader 1 has the period"},
		{name: "an extract", readers: openAll(t, txPack(t, seg1, func(m *PackMeta) {
			m.PackRole, m.ScopeGeneration, m.ExtractFilter = PackRoleExtract, nil, new("all")
		}, steps)), msg: "reader 0 is an extract"},
	}
	for _, tt := range tests {
		src, err := NewReaderSource(tt.readers)
		require.ErrorIs(t, err, ErrInvalidQuery, tt.name)
		require.ErrorContains(t, err, tt.msg, tt.name)
		assert.Nil(t, src, tt.name)
	}
}

// TestNewReaderSourceEmptyPeriod rejects a reader whose period is empty, whose scope is then unknown.
func TestNewReaderSourceEmptyPeriod(t *testing.T) {
	t.Parallel()

	r := mustOpen(t, txPack(t, seg0, nil, txBlock(txSeqs(12)...)), ReaderOptions{})
	r.meta.PeriodEnd = r.meta.PeriodStart
	_, err := NewReaderSource([]*Reader{r})
	require.ErrorIs(t, err, ErrInvalidQuery)
	require.ErrorContains(t, err, "empty or not inside one UTC hour")
}

// TestFindTransactionAnyRole looks up txRecord's primary at 12 in a segment,
// its reply at 13 and a socket-close at 14 in a pack of the retired role 5,
// beside a pack of role 9 holding a stop-unclean boundary of the capture whose gap meets the hour,
// each with a period inside the hour, over NewReaderSource:
// both are read like any pack given (the tracepack storage specification §2),
// so the reply matches and the crash gap is reported.
func TestFindTransactionAnyRole(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	segment := txPack(t, seg0, nil, txBlock(txRecord(12, nil)))
	retired := setMetaRole(t, txPack(t, seg1, nil, txBlock(txReply(13, nil), txSocketClose(t, 14))), packRoleRetired)
	unknown := setMetaRole(t, txPack(t, seg2, nil,
		txBlock(txBoundaryAt(t, 20, txTestEpoch+1, BoundaryKindStopUnclean, new(h+100), new(h+200)))), 9)
	src, err := NewReaderSource(openAll(t, segment, retired, unknown))
	require.NoError(t, err)
	res, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxMatched, res.Outcome)
	assert.Equal(t, []string{"12@0 primary", "13@0 candidate in eligible decidable valid", "14@0 closing bound"}, recordFlags(res.Records))
	assert.Equal(t, seg1, res.Records[1].Pack)
	require.Equal(t, []string{"crash-gap"}, gapSeqs(res.Incomplete))
	assert.Equal(t, new(seg2), res.Incomplete[0].Pack)
	assert.Equal(t, []TxScope{{Hour: memTestHour, Packs: []UUID{seg0, seg1, seg2}}}, res.Searched)
}

// TestTxKeyOf builds the key of a primary from its record's time: its hour is HourOf's, before 1970 too.
func TestTxKeyOf(t *testing.T) {
	t.Parallel()

	for _, ns := range []int64{blockTestHour, blockTestHour + hourNs - 1, 0, -1, -hourNs, -hourNs - 1} {
		ts := time.Unix(0, ns)
		assert.Equal(t, TxKey{CaptureID: captureLow, Seq: 7, Hour: hourOf(ns)}, TxKeyOf(captureLow, 7, ts), "ts %d", ns)
	}
	assert.Equal(t, memTestHour, TxKeyOf(captureLow, 0, time.Unix(0, blockTestHour).In(time.FixedZone("x", 5*3600))).Hour,
		"the hour is UTC's, whatever the time's location")
}

// e2eFrame returns blockTestFrame as an S1F<function> frame, its W bit set by w,
// with blockTestFrame's SessionID and System Bytes.
func e2eFrame(function byte, w bool) []byte {
	p := slices.Clone(blockTestFrame)
	p[fieldByte6Off] = 0x01
	if w {
		p[fieldByte6Off] |= 0x80
	}
	p[fieldFunctionOff] = function

	return p
}

// TestReaderSourceEndToEnd records a capture with a SegmentWriter into a NewDirSink directory:
// an S1F1 W primary late in one hour and its S1F2 reply early in the next.
// It opens every committed segment, and a lookup over NewReaderSource finds the reply across the hour, complete.
// The segments of each hour then merge without a refused input,
// and the lookup over the segments and the archives together, which hold identical copies of each record, still finds it.
func TestReaderSourceEndToEnd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sink, err := NewDirSink(root, dirTestPrefix)
	require.NoError(t, err)
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	opts.Now = nowAt(segAt(time.Hour + time.Minute))
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	primaryAt, replyAt := segAt(time.Hour-20*time.Second), segAt(time.Hour)
	require.Equal(t, HourOf(primaryAt)+1, HourOf(replyAt), "the reply lies in the next hour")
	require.NoError(t, w.AppendFrame(t.Context(), primaryAt, DirHostToEquipment, 1, e2eFrame(1, true)))
	require.NoError(t, w.AppendFrame(t.Context(), replyAt, DirEquipmentToHost, 1, e2eFrame(2, false)))
	require.NoError(t, w.Close(t.Context()))

	keys, partial := dirFiles(t, root)
	assert.Empty(t, partial)
	require.Len(t, keys, 3, "the start's period, the primary's, and the reply's in the next hour")
	byHour := make(map[int64][][]byte)
	readers := make([]*Reader, 0, len(keys))
	primarySeq := uint64(math.MaxUint64)
	for _, key := range keys {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		require.NoError(t, err)
		r := mustOpen(t, data, ReaderOptions{})
		readers = append(readers, r)
		byHour[hourOf(r.Header().Meta.PeriodStart)] = append(byHour[hourOf(r.Header().Meta.PeriodStart)], data)
		for _, it := range iterate(t, r, Query{}).items {
			if it.rec.Kind == KindData && it.rec.Dir == DirHostToEquipment {
				primarySeq = it.rec.Seq
			}
		}
	}
	require.NotEqual(t, uint64(math.MaxUint64), primarySeq, "the primary was recorded")

	key := TxKeyOf(w.CaptureID(), primarySeq, primaryAt)
	lookup := func(readers []*Reader) TxResult {
		t.Helper()

		src, err := NewReaderSource(readers)
		require.NoError(t, err)
		res, err := FindTransaction(t.Context(), src, key, TxOptions{MaxScopes: 2})
		require.NoError(t, err)

		return res
	}
	res := lookup(readers)
	require.Equal(t, TxMatched, res.Outcome, "gaps %v", res.Incomplete)
	assert.Empty(t, res.Incomplete)
	var valid []TxRecord
	for _, v := range res.Records {
		if v.Valid {
			valid = append(valid, v)
		}
	}
	require.Len(t, valid, 1)
	assert.Equal(t, replyAt.UnixNano(), valid[0].Record.TSUTCNs, "the reply")
	assert.Equal(t, HourOf(replyAt), valid[0].Hour, "read in the next hour's scope")
	assert.Len(t, res.Searched, 2)

	archives := slices.Clone(readers)
	for hour, files := range byHour {
		var buf bytes.Buffer
		_, err := Merge(t.Context(), &buf, inputsOf(files...), mergeOpts())
		require.NoError(t, err, "hour %d", hour)
		archive := mustOpen(t, buf.Bytes(), ReaderOptions{})
		require.Equal(t, hour, hourOf(archive.Header().Meta.PeriodStart), "the archive of the hour's segments")
		archives = append(archives, archive)
	}
	res = lookup(archives)
	require.Equal(t, TxMatched, res.Outcome, "gaps %v", res.Incomplete)
	assert.Empty(t, res.Incomplete)
	assert.Empty(t, res.Conflicts)
	require.Len(t, res.Searched, 2)
	segments := packIDs(readers)
	for _, sc := range res.Searched {
		require.Len(t, sc.Packs, len(byHour[sc.Hour])+1, "hour %d: the segments and the archive", sc.Hour)
		assert.NotContains(t, segments, sc.Packs[len(sc.Packs)-1], "hour %d", sc.Hour)
	}
}
