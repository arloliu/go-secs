package tracepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// txTestEpoch is the epoch of the lookup tests' records: not 0, which the Writer marks correlation-incomplete.
const txTestEpoch = 1

// gapReasons returns the reason of each gap, in order.
func gapReasons(gaps []TxGap) []TxGapReason {
	out := make([]TxGapReason, len(gaps))
	for i := range gaps {
		out[i] = gaps[i].Reason
	}

	return out
}

// txRecord returns the data record of seq at blockTestHour + seq over blockTestFrame, an S1F3 W host-to-equipment primary,
// in epoch txTestEpoch, changed by edit when set.
func txRecord(seq uint64, edit func(r *Record)) Record {
	r := testDataRecord(seq, blockTestHour+int64(seq), txTestEpoch)
	if edit != nil {
		edit(&r)
	}

	return r
}

// txBlock returns recs as the steps of one block.
func txBlock(recs ...Record) []footerTestStep {
	steps := make([]footerTestStep, len(recs))
	for i := range recs {
		steps[i] = footerTestStep{rec: recs[i], flush: i == len(recs)-1}
	}

	return steps
}

// txSeqs returns the records txRecord gives seqs, unchanged.
func txSeqs(seqs ...uint64) []Record {
	out := make([]Record, len(seqs))
	for i, seq := range seqs {
		out[i] = txRecord(seq, nil)
	}

	return out
}

// txPack writes steps into a finalized pack id of captureLow whose pack metadata has quality_evaluated true,
// changed by meta when set.
func txPack(t testing.TB, id UUID, meta func(m *PackMeta), steps []footerTestStep) []byte {
	t.Helper()

	return withIDs(t, writeRepairPack(t, CodecZstd, func(m *PackMeta) {
		m.QualityEvaluated = true
		if meta != nil {
			meta(m)
		}
	}, false, steps).file, id, captureLow)
}

// txSource returns a memSource giving files for memTestHour.
func txSource(t testing.TB, files ...[]byte) *memSource {
	t.Helper()

	s := newMemSource()
	for _, f := range files {
		s.addPack(t, memTestHour, f)
	}

	return s
}

// txKeyAt returns the key of seq of captureLow in memTestHour.
func txKeyAt(seq uint64) TxKey {
	return TxKey{Capture: captureLow, Seq: seq, Hour: memTestHour}
}

// requireOneScopeRead requires that the lookup asked s only for the readers of memTestHour, once.
func requireOneScopeRead(t testing.TB, s *memSource) {
	t.Helper()

	require.Equal(t, map[int64]int{memTestHour: 1}, s.counts())
}

// TestFindTransactionMissingPrimary looks up a seq that an hour's read without a defect does not hold,
// below its first seq, between two seqs and above its last, beside a pack of another capture that holds it,
// and in an empty hour:
// each fails with ErrNotPrimary after one hour's read, the result carrying the read's gaps and its searched hour.
func TestFindTransactionMissingPrimary(t *testing.T) {
	t.Parallel()

	file := txPack(t, seg0, nil, txBlock(txSeqs(10, 11, 13)...))
	unevaluated := withIDs(t, writeRepairPack(t, CodecZstd, nil, false, txBlock(txSeqs(10, 11, 13)...)).file, seg0, captureLow)
	tests := []struct {
		name string
		file []byte
		key  TxKey
		gaps []TxGapReason
	}{
		{name: "below the first seq", file: file, key: txKeyAt(5)},
		{name: "between seqs", file: file, key: txKeyAt(12)},
		{name: "above the last seq", file: file, key: txKeyAt(14)},
		{name: "an unevaluated pack", file: unevaluated, key: txKeyAt(12), gaps: []TxGapReason{TxGapUnevaluated}},
	}
	other := withIDs(t, txPack(t, seg1, nil, txBlock(txSeqs(11, 12, 13)...)), seg1, captureHigh)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, tt.file, other)
			res, err := findTx(t, t.Context(), s, tt.key, TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, ErrNotPrimary)
			assert.Equal(t, TxOutcome(0), res.Outcome)
			assert.Equal(t, []TxScope{{Hour: memTestHour, Packs: []UUID{seg0, seg1}}}, res.Searched)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Incomplete)))
			assert.Empty(t, res.Records)
			requireOneScopeRead(t, s)
		})
	}

	t.Run("an empty hour", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, file)
		key := txKeyAt(11)
		key.Hour++
		res, err := findTx(t, t.Context(), s, key, TxOptions{})
		require.ErrorIs(t, err, ErrNotPrimary)
		assert.Equal(t, []TxScope{{Hour: memTestHour + 1}}, res.Searched)
		assert.Empty(t, res.Incomplete)
		assert.Equal(t, map[int64]int{memTestHour + 1: 1}, s.counts())
	})
}

// TestFindTransactionMissingPrimaryExplained looks up a seq that its hour's read does not yield,
// beside a defect that explains it:
// a failed block, in the primary's capture or another, and a coverage entry that meets the seq and the hour;
// the primary has no key, the outcome is TxNotFound and incomplete, and no further hour is read.
// A coverage entry that misses the seq, the hour or the capture explains nothing: ErrNotPrimary;
// so does an entry without capture_id in a pack of another capture, which belongs to that capture.
func TestFindTransactionMissingPrimaryExplained(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	at := func(v int64) *int64 { return &v }
	seqOf := func(v uint64) *uint64 { return &v }
	other := captureHigh
	withCoverage := func(c Coverage) []byte {
		return txPack(t, seg0, func(m *PackMeta) { m.Coverage = []Coverage{c} }, txBlock(txSeqs(10, 11, 13)...))
	}
	corrupt := txPack(t, seg0, nil, slices.Concat(txBlock(txSeqs(10, 11)...), txBlock(txSeqs(13)...)))
	corrupt = flipByte(corrupt, mustOpen(t, corrupt, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+3)

	otherCorrupt := withIDs(t, corrupt, seg1, other)
	otherCovered := withIDs(t, withCoverage(Coverage{}), seg1, other)
	clean := txPack(t, seg0, nil, txBlock(txSeqs(10, 11, 13)...))
	tests := []struct {
		name     string
		files    [][]byte
		file     []byte
		gaps     []TxGapReason
		notFound bool
		// covered reports that the pack's one coverage entry explains the missing primary, so the no-key gap carries it.
		covered bool
	}{
		{name: "a failed block", file: corrupt, gaps: []TxGapReason{TxGapRead, TxGapNoKey}},
		{name: "a failed block of another capture", files: [][]byte{clean, otherCorrupt}, gaps: []TxGapReason{TxGapRead, TxGapNoKey}},
		{name: "coverage of another capture's pack", files: [][]byte{clean, otherCovered}, notFound: true},
		{name: "coverage over the seq and the hour", file: withCoverage(Coverage{
			SeqFirst: seqOf(12), SeqLast: seqOf(12), TimeStart: at(h + 5), TimeEnd: at(h + 6),
		}), gaps: []TxGapReason{TxGapNoKey}, covered: true},
		{name: "coverage without bounds", file: withCoverage(Coverage{}), gaps: []TxGapReason{TxGapNoKey}, covered: true},
		{name: "coverage up to the hour's start", file: withCoverage(Coverage{SeqFirst: seqOf(11), TimeEnd: at(h)}),
			gaps: []TxGapReason{TxGapNoKey}, covered: true},
		{name: "coverage with inverted seqs", file: withCoverage(Coverage{
			SeqFirst: seqOf(30), SeqLast: seqOf(20), TimeStart: at(h - 10), TimeEnd: at(h - 5),
		}), gaps: []TxGapReason{TxGapNoKey}, covered: true},
		{name: "coverage with an inverted time interval", file: withCoverage(Coverage{
			SeqFirst: seqOf(20), SeqLast: seqOf(30), TimeStart: at(h + 10), TimeEnd: at(h + 5),
		}), gaps: []TxGapReason{TxGapNoKey}, covered: true},
		{name: "coverage of other seqs", file: withCoverage(Coverage{SeqFirst: seqOf(13), SeqLast: seqOf(20)}), notFound: true},
		{name: "coverage of another hour", file: withCoverage(Coverage{TimeStart: at(h + hourNs)}), notFound: true},
		{name: "coverage before the hour", file: withCoverage(Coverage{TimeEnd: at(h - 1)}), notFound: true},
		{name: "coverage of another capture", file: withCoverage(Coverage{CaptureID: &other}), notFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := tt.files
			if files == nil {
				files = [][]byte{tt.file}
			}
			s := txSource(t, files...)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			requireOneScopeRead(t, s)
			if tt.notFound {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Empty(t, res.Incomplete)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, tt.gaps, gapReasons(res.Incomplete))
			noKey := res.Incomplete[len(res.Incomplete)-1]
			assert.Equal(t, []int64{memTestHour}, noKey.Hours)
			assert.Equal(t, new(uint64(12)), noKey.Seq)
			if tt.covered {
				want := mustOpen(t, tt.file, ReaderOptions{}).Header().Meta.Coverage
				require.Len(t, want, 1)
				assert.Equal(t, &want[0], noKey.Coverage)
			} else {
				assert.Nil(t, noKey.Coverage)
			}
			assert.Empty(t, res.Records)
			assert.Equal(t, memTestHour, res.Searched[0].Hour)
			assert.Len(t, res.Searched, 1)
		})
	}

	t.Run("the failed block's gap", func(t *testing.T) {
		t.Parallel()

		res, err := findTx(t, t.Context(), txSource(t, corrupt), txKeyAt(12), TxOptions{})
		require.NoError(t, err)
		g := res.Incomplete[0]
		assert.Equal(t, TxGapRead, g.Reason)
		assert.Equal(t, ReasonCorruptBlock, g.Defect)
		assert.Equal(t, []int64{memTestHour}, g.Hours)
		assert.Equal(t, new(seg0), g.Pack)
		assert.Equal(t, 1, g.Block)
		assert.Equal(t, int64(mustOpen(t, corrupt, ReaderOptions{}).Blocks()[1].Offset), g.Offset)
		require.Error(t, g.Err)
	})
}

// TestFindTransactionNotPrimary looks up a record that is not a primary:
// a transport event, a data record with an even function, and one whose function is unavailable,
// its validity bit clear or its bytes missing; each fails with ErrNotPrimary after one hour's read,
// with no record kept and no key derived.
func TestFindTransactionNotPrimary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rec  func(t *testing.T) Record
		// why is part of the error's text, naming what the record is.
		why string
	}{
		{name: "a transport event", why: "is a transport-event record", rec: func(t *testing.T) Record {
			return testEventRecord(t, 12, blockTestHour+12, txTestEpoch, &TransportEvent{Event: EventSocketClose})
		}},
		{name: "an even function", why: "data record with function 4, available true", rec: func(*testing.T) Record {
			return txRecord(12, func(r *Record) {
				r.Payload = slices.Clone(r.Payload)
				r.Payload[fieldFunctionOff] = 4
			})
		}},
		{name: "a function bit clear", why: "data record with function 3, available false", rec: func(*testing.T) Record {
			return txRecord(12, func(r *Record) { r.FieldValidity &^= FieldValidityFunction })
		}},
		{name: "a control record", why: "control record with function 3, available true", rec: func(*testing.T) Record {
			return txRecord(12, func(r *Record) {
				r.Kind = KindControl
				r.SetCapturedFieldValidity()
			})
		}},
		{name: "a function byte missing", why: "data record with function 0, available false", rec: func(*testing.T) Record {
			return txRecord(12, func(r *Record) {
				r.Payload = r.Payload[:fieldFunctionOff]
				r.SetCapturedFieldValidity()
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, txPack(t, seg0, nil, txBlock(txRecord(11, nil), tt.rec(t), txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, ErrNotPrimary)
			require.ErrorContains(t, err, tt.why)
			assert.Equal(t, TxResult{Searched: []TxScope{{Hour: memTestHour, Packs: []UUID{seg0}}}}, res)
			requireOneScopeRead(t, s)
		})
	}
}

// TestFindTransactionNoKey looks up primaries without a key:
// in unknown or local direction, with SessionID or System Bytes unavailable;
// the outcome is TxNotFound with a TxGapNoKey gap after one hour's read,
// the primary kept and the fields that are available derived from it.
func TestFindTransactionNoKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(r *Record)
		want TxResult
	}{
		{name: "unknown direction", edit: func(r *Record) { r.Dir = DirUnknown }, want: TxResult{
			Dir: DirUnknown, SessionID: 0x1234, SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
		}},
		{name: "local direction", edit: func(r *Record) { r.Dir = DirLocal }, want: TxResult{
			Dir: DirLocal, SessionID: 0x1234, SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
		}},
		{name: "SessionID bit clear", edit: func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }, want: TxResult{
			Dir: DirHostToEquipment, SystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
		}},
		{name: "System Bytes bit clear", edit: func(r *Record) { r.FieldValidity &^= FieldValiditySystemBytes }, want: TxResult{
			Dir: DirHostToEquipment, SessionID: 0x1234,
		}},
		{name: "System Bytes missing", edit: func(r *Record) {
			r.Payload = r.Payload[:fieldSystemBytesEnd-1]
			r.SetCapturedFieldValidity()
		}, want: TxResult{Dir: DirHostToEquipment, SessionID: 0x1234}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary := txRecord(12, tt.edit)
			s := txSource(t, txPack(t, seg0, nil, txBlock(txRecord(11, nil), primary, txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.NoError(t, err)
			requireOneScopeRead(t, s)

			want := tt.want
			want.Outcome, want.Epoch = TxNotFound, txTestEpoch
			want.Stream, want.Function, want.StreamAvailable, want.W, want.WAvailable = 1, 3, true, true, true
			want.Records = []TxRecord{{Record: primary, Hour: memTestHour, Pack: seg0, Class: TxPrimary}}
			want.Searched = []TxScope{{Hour: memTestHour, Packs: []UUID{seg0}}}
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, TxGapNoKey, res.Incomplete[0].Reason)
			assert.Equal(t, new(uint64(12)), res.Incomplete[0].Seq)
			assert.Equal(t, []int64{memTestHour}, res.Incomplete[0].Hours)
			assert.Nil(t, res.Incomplete[0].Pack)
			assert.Equal(t, [2]int64{-1, -1}, [2]int64{int64(res.Incomplete[0].Block), res.Incomplete[0].Offset})
			res.Incomplete = nil
			assert.Equal(t, want, res)
		})
	}
}

// TestFindTransactionConflictingPrimary looks up a primary that two packs of its scope hold in different versions:
// TxGapConflict and TxGapNoKey gaps, the outcome TxNotFound after one hour's read,
// both versions kept as primaries, marked conflicting, with their packs, and the key derived from the first.
func TestFindTransactionConflictingPrimary(t *testing.T) {
	t.Parallel()

	steps := txBlock(txSeqs(11, 12, 13)...)
	s := txSource(t, txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 12)))
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
	require.NoError(t, err)
	requireOneScopeRead(t, s)

	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(res.Incomplete))
	assert.Equal(t, new(uint64(12)), res.Incomplete[0].Seq)
	assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
	second := txRecord(12, nil)
	second.Payload = slices.Clone(second.Payload)
	second.Payload[len(second.Payload)-1]++
	assert.Equal(t, []TxRecord{
		{Record: txRecord(12, nil), Hour: memTestHour, Pack: seg0, Block: 0, Conflict: true, Class: TxPrimary},
		{Record: second, Hour: memTestHour, Pack: seg1, Block: 0, Conflict: true, Class: TxPrimary},
	}, res.Records)
	assert.Equal(t, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, res.SystemBytes, "the key of the first version")
	assert.Equal(t, DirHostToEquipment, res.Dir)
}

// txFlagged returns file with reserved bit 5 of record_flags set in the record header of seq:
// a stored difference that the decoded Record does not show.
func txFlagged(t testing.TB, file []byte, seq uint64) []byte {
	t.Helper()

	return reindexed(t, rebuildPack(t, file, func(_ int, b *testBlock) {
		b.patchRows = func(rows []byte, rhl int) {
			for off := 0; off < len(rows); off += rhl {
				if binary.LittleEndian.Uint64(rows[off:]) == seq {
					rows[off+43] |= 1 << 5
				}
			}
		}
	}))
}

// TestFindTransactionPrimaryFlagsApart looks up a primary that two packs of its scope hold in versions
// whose record headers differ only in a reserved record_flags bit:
// the read lists the conflict at the primary, so the primary has no key whatever its decoded fields show,
// and the outcome is TxNotFound after one hour's read, both versions kept and marked,
// whether the window would have been empty or held a valid reply.
func TestFindTransactionPrimaryFlagsApart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		after []Record
	}{
		{name: "a same-key primary next", after: txSeqs(13)},
		{name: "a valid reply, then a same-key primary", after: []Record{txReply(13, nil), txRecord(14, nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			steps := txBlock(slices.Concat(txSeqs(11, 12), tt.after)...)
			s := txSource(t, txPack(t, seg0, nil, steps), txFlagged(t, txPack(t, seg1, nil, steps), 12))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(res.Incomplete))
			assert.Equal(t, []Conflict{{CaptureID: captureLow, Seq: 12, Versions: [][]UUID{{seg0}, {seg1}}}}, res.Conflicts)
			assert.Equal(t, []TxRecord{
				{Record: txRecord(12, nil), Hour: memTestHour, Pack: seg0, Conflict: true, Class: TxPrimary},
				{Record: txRecord(12, nil), Hour: memTestHour, Pack: seg1, Conflict: true, Class: TxPrimary},
			}, res.Records)
			assert.Nil(t, res.WindowEnd)
			requireOneScopeRead(t, s)
		})
	}
}

// TestFindTransactionKeyed looks up primaries that have a key, all fields available, the stream unavailable,
// and in epoch 0, which the Writer marks correlation-incomplete:
// the key and match fields are derived, StreamAvailable and WAvailable false without the stream,
// and correlation-incomplete adds a TxGapCorrelation gap.
// The same-key primary at 13 bounds an empty window, so the outcome is TxNotFound and complete;
// in epoch 0 it lies in another epoch, so the window stays open and the lookup is incomplete.
func TestFindTransactionKeyed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		edit    func(r *Record)
		stream  bool
		epoch   uint32
		gaps    []TxGapReason
		outcome TxOutcome
	}{
		{name: "every field available", stream: true, epoch: txTestEpoch, outcome: TxNotFound},
		{name: "the stream unavailable", edit: func(r *Record) { r.FieldValidity &^= FieldValidityStreamAndW }, epoch: txTestEpoch,
			outcome: TxNotFound},
		{name: "correlation-incomplete", edit: func(r *Record) { r.Epoch = 0 }, stream: true,
			gaps: []TxGapReason{TxGapCorrelation, TxGapOpenWindow}, outcome: TxNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary := txRecord(12, tt.edit)
			s := txSource(t, txPack(t, seg0, nil, txBlock(txRecord(11, nil), primary, txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Incomplete)))
			assert.Equal(t, tt.epoch, res.Epoch)
			assert.Equal(t, DirHostToEquipment, res.Dir)
			assert.Equal(t, uint16(0x1234), res.SessionID)
			assert.Equal(t, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, res.SystemBytes)
			assert.Equal(t, uint8(3), res.Function)
			assert.Equal(t, tt.stream, res.StreamAvailable)
			assert.Equal(t, tt.stream, res.WAvailable)
			assert.Equal(t, tt.stream, res.W)
			if tt.stream {
				assert.Equal(t, uint8(1), res.Stream)
			} else {
				assert.Zero(t, res.Stream)
			}
			if tt.epoch == txTestEpoch {
				require.Len(t, res.Records, 2, "the primary and the same-key primary at 13")
				assert.Equal(t, TxSameKeyPrimary, res.Records[1].Class)
				assert.True(t, res.Records[1].Bound)
				assert.Equal(t, new(uint64(13)), res.WindowEnd)
			} else {
				require.Len(t, res.Records, 1, "13 lies in another epoch")
				assert.Nil(t, res.WindowEnd)
			}
			assert.Equal(t, TxPrimary, res.Records[0].Class)
			assert.Equal(t, tt.epoch != 0, res.Records[0].Record.Quality&QualityCorrelationIncomplete == 0)
		})
	}
}

// txMisindexed returns a pack seg0 of one block that holds seqs 10 and seq, above 11,
// while the block's F-2 entry and the trailer state seqs 10 to 11; its footer stays valid.
func txMisindexed(t testing.TB, seq uint64) []byte {
	t.Helper()

	return txMisindexedAt(t, txRecord(seq, nil))
}

// txMisindexedAt is txMisindexed with recs, in ascending seq, in place of the data record of seq:
// the block holds 10 and recs, while its F-2 entry and the trailer state seqs 10 to 10 + len(recs), one seq for each record,
// each of recs lying above that range (txUnderstated).
func txMisindexedAt(t testing.TB, recs ...Record) []byte {
	t.Helper()

	last := 10 + uint64(len(recs))
	require.Greater(t, recs[0].Seq, last)

	return txUnderstated(t, txPack(t, seg0, nil, txBlock(append([]Record{txRecord(10, nil)}, recs...)...)), last)
}

// txUnderstated returns file, a pack of one block of one epoch,
// with the block's F-2 entry, its epoch summary and the trailer stating its seqs to run from its first to last, below its last record;
// the caller keeps as many seqs as records in that range.
// The footer states no closure or capture-boundary in the block, so it stays valid when a record above last is a closing record.
func txUnderstated(t testing.TB, file []byte, last uint64) []byte {
	t.Helper()

	file = reindexedWith(t, file, func(_ int, s *blockSummary) {
		s.lastSeq = last
		s.seqRanges = []seqRange{{first: s.firstSeq, last: last}}
		require.Len(t, s.epochs, 1)
		s.epochs[0].seqLast = last
		// The closure and boundary entries of a closing record above the stated range would name a seq outside it.
		s.epochs[0].closeSeq, s.epochs[0].hasCloseSeq = 0, false
		s.boundaries = nil
	})
	tr := layoutOf(t, file).tr
	tr.LastSeq = last

	return format.AppendTrailer(file[:len(file)-format.TrailerLen], &tr)
}

// TestFindTransactionPrimaryAfterHigherSeqs reads an hour where a block whose F-2 entry under-reports its last seq
// yields a seq above the primary before the primary arrives from another pack:
// the read detects the disagreement, and the order was broken,
// so the lookup is incomplete with the block's TxGapIndex gap, beside one at the primary's seq;
// nothing else of the result is guaranteed (the tracepack semantics specification §7.2).
// Without the primary, the records above it are no gap of their own: the defect explains the missing primary.
func TestFindTransactionPrimaryAfterHigherSeqs(t *testing.T) {
	t.Parallel()

	// The one block of seg0 holds seqs 10 and 20, while its index states 10 to 11.
	misindexed := txMisindexed(t, 20)
	primary := txPack(t, seg1, nil, txBlock(txSeqs(15)...))

	t.Run("the primary arrives", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, misindexed, primary)
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		require.Empty(t, res.FooterErrs, "the footer must stay valid")
		require.GreaterOrEqual(t, len(res.Incomplete), 2)
		assert.Equal(t, []string{"index", "index@15"}, gapSeqs(res.Incomplete[:2]))
		assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Defect)
		assert.Equal(t, new(seg0), res.Incomplete[0].Pack)
		assert.Nil(t, res.Incomplete[1].Pack)
	})

	t.Run("the primary is missing", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, misindexed)
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapNoKey}, gapReasons(res.Incomplete))
		assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Defect)
	})
}

// TestFindTransactionSourceErrors ends lookups on an error of the source and on ctx done during the hour's read:
// the error is returned wrapped with the Outcome zero, and an hour whose read ends early is not searched.
func TestFindTransactionSourceErrors(t *testing.T) {
	t.Parallel()

	file := txPack(t, seg0, nil, txBlock(txSeqs(11, 12, 13)...))
	boom := errors.New("boom")
	tests := []struct {
		name  string
		setup func(s *memSource, cancel context.CancelFunc)
		err   error
		calls map[int64]int
	}{
		{name: "Readers fails", setup: func(s *memSource, _ context.CancelFunc) {
			s.beforeReaders = func(context.Context, int64) error { return boom }
		}, err: boom, calls: map[int64]int{memTestHour: 1}},
		{name: "Readers fails in a later hour", setup: func(s *memSource, _ context.CancelFunc) {
			s.beforeReaders = func(_ context.Context, h int64) error {
				if h > memTestHour {
					return boom
				}

				return nil
			}
		}, err: boom, calls: map[int64]int{memTestHour: 1, memTestHour + 1: 1}},
		{name: "ctx done during the hour's read", setup: func(s *memSource, cancel context.CancelFunc) {
			s.beforeReaders = func(context.Context, int64) error { cancel(); return nil }
		}, err: context.Canceled, calls: map[int64]int{memTestHour: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, file)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tt.setup(s, cancel)
			res, err := findTx(t, ctx, s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, TxOutcome(0), res.Outcome)
			assert.Equal(t, tt.calls, s.counts())
			if errors.Is(tt.err, context.Canceled) {
				assert.Empty(t, res.Searched, "a read that ends early is not searched")
			}
		})
	}
}

// nilIfEmptyMap returns nil for an empty m, m otherwise.
func nilIfEmptyMap[K comparable, V any](m map[K]V) map[K]V {
	if len(m) == 0 {
		return nil
	}

	return m
}

// TestFindTransactionConflictLimit reads an hour with two conflicts, beside a primary without a key:
// MaxConflicts 2 lets the read finish and lists both, with no conflict gap since the lookup ends at the primary;
// MaxConflicts 1 ends the read at the second with ErrReadLimit, the first listed, and the hour not searched.
func TestFindTransactionConflictLimit(t *testing.T) {
	t.Parallel()

	unknown := func(r *Record) { r.Dir = DirUnknown }
	steps := txBlock(txRecord(10, nil), txRecord(11, unknown), txRecord(12, nil), txRecord(13, nil))
	files := [][]byte{txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 10, 13))}

	s := txSource(t, files...)
	res, err := findTx(t, t.Context(), s, txKeyAt(11), TxOptions{MaxConflicts: 2})
	require.NoError(t, err)
	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, []uint64{10, 13}, conflictSeqs(res.Conflicts))
	assert.Equal(t, []TxGapReason{TxGapNoKey}, gapReasons(res.Incomplete), "no conflict gap beside a primary without a key")

	s = txSource(t, files...)
	res, err = findTx(t, t.Context(), s, txKeyAt(11), TxOptions{MaxConflicts: 1})
	require.ErrorIs(t, err, ErrReadLimit)
	require.ErrorContains(t, err, "seq 13")
	assert.Equal(t, TxOutcome(0), res.Outcome)
	assert.Equal(t, []uint64{10}, conflictSeqs(res.Conflicts))
	assert.Empty(t, res.Searched)
	requireOneScopeRead(t, s)
}

// conflictSeqs returns the seq of each conflict, in order.
func conflictSeqs(cs []Conflict) []uint64 {
	out := make([]uint64, len(cs))
	for i := range cs {
		out[i] = cs[i].Seq
	}

	return out
}

// TestFindTransactionOwnsRecords looks up a primary whose record header has an extension area,
// in the first of several blocks:
// the kept version's payload and header extension are copies,
// unchanged after the read went on through the other blocks,
// and after another lookup over the same packs whose records the caller changed.
func TestFindTransactionOwnsRecords(t *testing.T) {
	t.Parallel()

	// Blocks of the same size follow the primary's, each with other bytes,
	// so a buffer reused for them would change the primary's.
	steps := txBlock(txSeqs(11, 12)...)
	for seq := uint64(13); seq < 19; seq += 2 {
		steps = append(steps, changed(txBlock(txSeqs(seq, seq+1)...), byte(seq), seq, seq+1)...)
	}
	file := widenedFrom(t, txPack(t, seg0, nil, steps), 11)
	s := txSource(t, file)
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.Len(t, res.Records, 1)

	got := res.Records[0]
	assert.Equal(t, blockTestFrame, got.Record.Payload)
	assert.Equal(t, seqExtra(12), got.HeaderExtra)

	// Another lookup over the same packs and the caller's own changes leave the first result as it was.
	again, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	again.Records[0].Record.Payload[0] ^= 0xFF
	again.Records[0].HeaderExtra[0] ^= 0xFF
	assert.Equal(t, blockTestFrame, got.Record.Payload)
	assert.Equal(t, seqExtra(12), got.HeaderExtra)
}

// TestKeepVersionCopies keeps a version from an Item whose payload and header extension the test then overwrites,
// as a read reuses an Item and its buffers: the kept version is unchanged, its pack named by the read's pack_ids.
func TestKeepVersionCopies(t *testing.T) {
	t.Parallel()

	payload, extra := slices.Clone(blockTestFrame), seqExtra(12)
	it := &Item{Record: txRecord(12, nil), HeaderExtra: extra, Pack: 1, Block: 2, Conflict: true}
	it.Record.Payload = payload
	rd := &txScopeRead{hour: memTestHour, packs: []UUID{seg0, seg1}}
	got := keepVersion(rd, it, TxPrimary)
	clear(payload)
	clear(extra)

	assert.Equal(t, TxRecord{
		Record: txRecord(12, nil), HeaderExtra: seqExtra(12), Hour: memTestHour, Pack: seg1, Block: 2, Conflict: true, Class: TxPrimary,
	}, got)
}

// TestFindTransactionNamesPacks reads an hour of two packs whose second, in the order given, has a corrupt block,
// or a footer that fails validation over truthful blocks, read by walking:
// the read gap, or the footer error, names the second pack's pack_id and the hour,
// and the walked pack, which the walk accounts for in full, adds no gap of its read, so the lookup is complete.
func TestFindTransactionNamesPacks(t *testing.T) {
	t.Parallel()

	first := txPack(t, seg0, nil, txBlock(txSeqs(10, 11)...))
	second := txPack(t, seg1, nil, slices.Concat(txBlock(txSeqs(12)...), txBlock(txSeqs(13)...)))

	t.Run("a corrupt block", func(t *testing.T) {
		t.Parallel()

		corrupt := flipByte(second, mustOpen(t, second, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+3)
		res, err := findTx(t, t.Context(), txSource(t, first, corrupt), txKeyAt(11), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		require.Len(t, res.Incomplete, 1, "the same-key primary at 12 bounds an empty window")
		assert.Equal(t, TxGapRead, res.Incomplete[0].Reason)
		assert.Equal(t, new(seg1), res.Incomplete[0].Pack)
		assert.Equal(t, 1, res.Incomplete[0].Block)
		assert.Equal(t, []UUID{seg0, seg1}, res.Searched[0].Packs)
	})

	t.Run("a footer that fails validation", func(t *testing.T) {
		t.Parallel()

		walked := invalidFooterFile(t, second)
		res, err := findTx(t, t.Context(), txSource(t, first, walked), txKeyAt(11), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Empty(t, res.Incomplete)
		require.Len(t, res.FooterErrs, 1)
		fe := res.FooterErrs[0]
		assert.Equal(t, memTestHour, fe.Hour)
		assert.Equal(t, seg1, fe.Pack)
		require.ErrorIs(t, fe.Err, ErrInvalidFooter)
		assert.Equal(t, []UUID{seg0, seg1}, res.Searched[0].Packs)
	})
}

// TestFindTransactionSplitPrimaryCopies reads an hour where a block whose F-2 entry under-reports its last seq
// holds a copy of the primary in a cluster of its own, so the read yields that copy and another pack's uncompared:
// identical copies are one primary, kept once, beside the block's index gap and no conflict;
// different copies are a conflict at the primary, both kept and marked, without a key.
func TestFindTransactionSplitPrimaryCopies(t *testing.T) {
	t.Parallel()

	// The one block of seg0 holds seqs 10 and 15, while its index states 10 to 11; seg1 holds 15 and 20.
	misindexed := txMisindexed(t, 15)
	other := txBlock(txSeqs(15, 20)...)

	t.Run("identical copies", func(t *testing.T) {
		t.Parallel()

		res, err := findTx(t, t.Context(), txSource(t, misindexed, txPack(t, seg1, nil, other)), txKeyAt(15),
			TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		require.Empty(t, res.FooterErrs, "the footer must stay valid")
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapSeqGap}, gapReasons(res.Incomplete))
		assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Defect)
		assert.Equal(t, new(uint64(16)), res.Incomplete[1].Seq, "the window (15, 20) holds no seq read")
		assert.Empty(t, res.Conflicts)
		assert.Equal(t, []TxRecord{
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg0, Class: TxPrimary},
			{Record: txRecord(20, nil), Hour: memTestHour, Pack: seg1, Class: TxSameKeyPrimary, Bound: true},
		}, res.Records)
		assert.Equal(t, DirHostToEquipment, res.Dir)
	})

	t.Run("different copies", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, misindexed, txPack(t, seg1, nil, changed(other, 1, 15)))
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 3})
		require.NoError(t, err)
		requireOneScopeRead(t, s)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapConflict, TxGapNoKey}, gapReasons(res.Incomplete))
		assert.Empty(t, res.Conflicts, "the read compared neither copy with the other")
		second := txRecord(15, nil)
		second.Payload = slices.Clone(second.Payload)
		second.Payload[len(second.Payload)-1]++
		assert.Equal(t, []TxRecord{
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg0, Conflict: true, Class: TxPrimary},
			{Record: second, Hour: memTestHour, Pack: seg1, Conflict: true, Class: TxPrimary},
		}, res.Records)
	})

	t.Run("copies a reserved record_flags bit apart", func(t *testing.T) {
		t.Parallel()

		// The decoded copies are equal; their stored headers are not.
		s := txSource(t, misindexed, txFlagged(t, txPack(t, seg1, nil, other), 15))
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 3})
		require.NoError(t, err)
		assert.Equal(t, TxNotFound, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapConflict, TxGapNoKey}, gapReasons(res.Incomplete))
		assert.Empty(t, res.Conflicts, "the read compared neither copy with the other")
		assert.Equal(t, []TxRecord{
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg0, Conflict: true, Class: TxPrimary},
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg1, Conflict: true, Class: TxPrimary},
		}, res.Records)
		requireOneScopeRead(t, s)
	})
}

// TestCoverageMeets matches coverage entries against a seq range and a time range by the tracepack format specification §5:
// both sides inclusive on the entry, the time range half-open, an absent bound unbounded,
// and an inverted side meeting every query.
func TestCoverageMeets(t *testing.T) {
	t.Parallel()

	at := func(v int64) *int64 { return &v }
	seqOf := func(v uint64) *uint64 { return &v }
	tests := []struct {
		name string
		c    Coverage
		want bool
	}{
		{name: "no bounds", c: Coverage{}, want: true},
		{name: "seqs at the query's first", c: Coverage{SeqFirst: seqOf(1), SeqLast: seqOf(10)}, want: true},
		{name: "seqs at the query's last", c: Coverage{SeqFirst: seqOf(20), SeqLast: seqOf(30)}, want: true},
		{name: "seqs below", c: Coverage{SeqLast: seqOf(9)}},
		{name: "seqs above", c: Coverage{SeqFirst: seqOf(21)}},
		{name: "time ending at the range's start", c: Coverage{TimeEnd: at(100)}, want: true},
		{name: "time starting at the range's end", c: Coverage{TimeStart: at(200)}},
		{name: "time before", c: Coverage{TimeStart: at(0), TimeEnd: at(99)}},
		{name: "seqs inverted outside", c: Coverage{SeqFirst: seqOf(50), SeqLast: seqOf(40), TimeEnd: at(0)}, want: true},
		{name: "time inverted outside", c: Coverage{SeqFirst: seqOf(50), TimeStart: at(500), TimeEnd: at(400)}, want: true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, coverageMeets(&tt.c, 10, 20, 100, 200), tt.name)
	}
}

// TestFindTransactionEarlyReturns looks up txRecord's primary at 12 over two hours scheduled,
// in each case of an early return of the tracepack semantics specification §7.2,
// which ends the lookup after the primary's hour, TxNotFound and incomplete, with no window:
// a missing primary explained by a read defect, by a crash gap of its capture meeting its hour, or by a scope breach;
// a conflicting primary; and a primary without a key.
// A missing primary nothing explains is ErrNotPrimary;
// a primary whose versions conflict only across hours is no early return: every hour is read.
func TestFindTransactionEarlyReturns(t *testing.T) {
	t.Parallel()

	h := blockTestHour
	corrupt := txPack(t, seg0, nil, slices.Concat(txBlock(txSeqs(11)...), txBlock(txSeqs(13)...)))
	corrupt = flipByte(corrupt, mustOpen(t, corrupt, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+3)
	missing := txPack(t, seg0, nil, txBlock(txSeqs(11, 13)...))
	steps := txBlock(txSeqs(12, 13)...)
	tests := []struct {
		name  string
		files [][]byte
		// gaps are the gaps of the early return; nil with notPrimary.
		gaps       []string
		notPrimary bool
	}{
		{name: "a missing primary explained by a read defect", files: [][]byte{corrupt}, gaps: []string{"read", "no-key@12"}},
		{name: "a missing primary explained by a crash gap", files: [][]byte{missing, txPack(t, seg1, nil,
			txBlock(txBoundaryAt(t, 20, txTestEpoch+1, BoundaryKindStopUnclean, new(h+100), new(h+200))))},
			gaps: []string{"no-key@12", "crash-gap"}},
		{name: "a missing primary explained by a scope breach",
			files: [][]byte{txBreachPack(t, seg0, 0, false, append(txBlock(txNote(11, nil)), inHours(1, txBlock(txNote(13, nil)))...))},
			gaps:  []string{"scope-breach@13", "no-key@12"}},
		{name: "a missing primary nothing explains", files: [][]byte{missing}, notPrimary: true},
		{name: "a conflicting primary", files: [][]byte{txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 12))},
			gaps: []string{"conflict@12", "no-key@12"}},
		{name: "a primary without a key", files: [][]byte{txPack(t, seg0, nil, txBlock(txRecord(12, func(r *Record) { r.Dir = DirUnknown })))},
			gaps: []string{"no-key@12"}},
	}
	later := txPackIn(t, seg2, 1, nil, txBlock(txReply(13, nil)))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.files, [][]byte{later})
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
			requireOneScopeRead(t, s)
			if tt.notPrimary {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Zero(t, res.Outcome)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, TxNotFound, res.Outcome)
			assert.Equal(t, tt.gaps, gapSeqs(res.Incomplete))
			assert.Nil(t, res.WindowEnd)
			for _, r := range res.Records {
				assert.Equal(t, uint64(12), r.Record.Seq, "only the primary's versions are kept")
			}
		})
	}

	t.Run("a primary whose versions conflict only across hours", func(t *testing.T) {
		t.Parallel()

		s := txHours(t, [][]byte{txPack(t, seg0, nil, steps)}, [][]byte{txPackIn(t, seg1, 1, nil, changed(txBlock(txSeqs(12)...), 1, 12))})
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
		require.NoError(t, err)
		assert.Equal(t, map[int64]int{memTestHour: 1, memTestHour + 1: 1}, s.counts(), "every hour read")
		assert.Equal(t, []string{"conflict@12", "no-key@12"}, gapSeqs(res.Incomplete))
		assert.Equal(t, []int64{memTestHour, memTestHour + 1}, res.Incomplete[0].Hours)
		assert.Equal(t, new(uint64(13)), res.WindowEnd, "the window is evaluated")
	})
}

// TestFindTransactionDetectedDisagreement reads, in an hour the lookup reads, a block whose records disagree with its F-2 entry:
// the lookup is incomplete with a TxGapIndex gap naming that block's pack, block and offset;
// nothing else of the result is guaranteed, so nothing else is asserted (the tracepack semantics specification §7.2).
func TestFindTransactionDetectedDisagreement(t *testing.T) {
	t.Parallel()

	answered := txPack(t, seg1, nil, txBlock(txRecord(15, nil), txReply(16, nil), txRecord(17, nil)))
	// seg2 of the next hour holds 30 and 40 in one block whose F-2 entry states 30 to 31.
	later := txUnderstated(t, txPackIn(t, seg2, 1, nil, txBlock(txRecord(30, nil), txRecord(40, nil))), 31)
	tests := []struct {
		name  string
		hours [][][]byte
		pack  UUID
	}{
		{name: "in the primary's hour", hours: [][][]byte{{txMisindexed(t, 20), answered}}, pack: seg0},
		{name: "in a later hour", hours: [][][]byte{{answered}, {later}}, pack: seg2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := findTx(t, t.Context(), txHours(t, tt.hours...), txKeyAt(15), TxOptions{MaxScopes: len(tt.hours)})
			require.NoError(t, err)
			i := slices.IndexFunc(res.Incomplete, func(g TxGap) bool { return g.Reason == TxGapIndex && g.Pack != nil })
			require.GreaterOrEqual(t, i, 0, "a TxGapIndex gap naming the block")
			g := res.Incomplete[i]
			assert.Equal(t, ReasonIndexMismatch, g.Defect)
			assert.Equal(t, tt.pack, *g.Pack)
			assert.Equal(t, 0, g.Block)
			assert.Positive(t, g.Offset)
		})
	}
}

// TestFindTransactionReadGuarantees reads, beside txRecord's primary at 12 and a socket-close at 14,
// a pack holding the reply at 13 in a valid block and a defect in another:
// a block over the reader's MaxBlockLen, a failed block, an unknown codec and a truncated tail.
// Each defect keeps the guarantees of the tracepack format specification §13:
// it is a TxGapRead gap with its reason, and the validated block's reply is still read and matched.
func TestFindTransactionReadGuarantees(t *testing.T) {
	t.Parallel()

	primary := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txSocketClose(t, 14)))
	big := txNote(20, bytes.Repeat([]byte{1}, 4096))
	twoBlocks := txPack(t, seg1, nil, slices.Concat(txBlock(txReply(13, nil)), txBlock(big)))
	second := mustOpen(t, twoBlocks, ReaderOptions{}).Blocks()[1]
	tests := []struct {
		name   string
		file   []byte
		opts   ReaderOptions
		reason IncompleteReason
	}{
		{name: "a reader limit", file: twoBlocks, opts: ReaderOptions{MaxBlockLen: 2048}, reason: ReasonLimit},
		{name: "a failed block", file: flipByte(twoBlocks, second.Offset+format.EnvelopeLen+3), reason: ReasonCorruptBlock},
		{name: "an unknown codec", file: patchEnvelope(t, twoBlocks, second.Offset, func(e *format.BlockEnvelope) { e.Codec = 7 }),
			reason: ReasonUnknownCodec},
		{name: "a truncated pack", file: twoBlocks[:second.Offset+format.EnvelopeLen+5], reason: ReasonTruncated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, err := NewReaderSource([]*Reader{mustOpen(t, primary, ReaderOptions{}), mustOpen(t, tt.file, tt.opts)})
			require.NoError(t, err)
			res, err := FindTransaction(t.Context(), src, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, TxMatched, res.Outcome, "the validated block's reply")
			assert.Equal(t, []string{"12@0 primary", "13@0 candidate in eligible decidable valid", "14@0 closing bound"}, recordFlags(res.Records))
			reads := slices.DeleteFunc(slices.Clone(res.Incomplete), func(g TxGap) bool { return g.Reason != TxGapRead })
			require.NotEmpty(t, reads)
			assert.Equal(t, tt.reason, reads[0].Defect)
			assert.Equal(t, new(seg1), reads[0].Pack)
		})
	}
}

// TestFindTransactionOtherCaptures reads an hour holding a pack of the primary's capture, captureLow,
// and packs of captureHigh holding the same seqs as other records: 12 in other bytes, 13, and two versions of 20,
// beside a pack of captureHigh holding a record of the next hour and one whose quality_evaluated is false.
// The other capture's records take no part in the lookup's decisions (the tracepack semantics specification §7.2):
// its 13 fills no seq gap of the primary's window, its 12 is no conflict at the primary,
// and its conflict at 20 is listed in Conflicts without making the lookup incomplete;
// but its scope breach and its unevaluated pack are reported, as for every pack read.
func TestFindTransactionOtherCaptures(t *testing.T) {
	t.Parallel()

	own := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txRecord(14, nil)))
	otherSteps := txBlock(txReply(12, nil), txReply(13, nil), txNote(20, []byte{1}))
	other := withIDs(t, txPack(t, seg1, nil, otherSteps), seg1, captureHigh)
	otherVersion := withIDs(t, txPack(t, seg2, nil, changed(txBlock(txNote(20, []byte{1})), 1, 20)), seg2, captureHigh)
	breach := withIDs(t, txBreachPack(t, seg3, 0, false, inHours(1, txBlock(txNote(30, nil)))), seg3, captureHigh)
	unevaluated := withIDs(t, writeRepairPack(t, CodecZstd, nil, false, txBlock(txNote(40, nil))).file, memA, captureHigh)
	res, err := findTx(t, t.Context(), txSource(t, own, other, otherVersion, breach, unevaluated), txKeyAt(12), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxNotFound, res.Outcome)
	assert.Equal(t, []string{"scope-breach@30", "unevaluated", "seq-gap@13"}, gapSeqs(res.Incomplete),
		"another capture's 13 fills no gap; its breach and unevaluated pack are reported")
	assert.Equal(t, new(seg3), res.Incomplete[0].Pack)
	assert.Equal(t, new(memA), res.Incomplete[1].Pack)
	assert.Equal(t, []string{"12@0 primary", "14@0 same-key-primary bound"}, recordFlags(res.Records))
	assert.Equal(t, []Conflict{{CaptureID: captureHigh, Seq: 20, Versions: [][]UUID{{seg1}, {seg2}}}}, res.Conflicts)
	assert.Equal(t, []TxScope{{Hour: memTestHour, Packs: []UUID{seg0, seg1, seg2, seg3, memA}}}, res.Searched)
	assert.Equal(t, DirHostToEquipment, res.Dir, "the key of the primary's own capture")
}
