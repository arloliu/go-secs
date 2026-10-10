package tracepack

import (
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

// txSource returns a memSource holding files in captureLow's scope memTestHour, indexed or not.
func txSource(t testing.TB, indexed bool, files ...[]byte) *memSource {
	t.Helper()

	s := newMemSource()
	for _, f := range files {
		s.addPack(t, memTestHour, f)
	}
	s.setIndexed(captureLow, memTestHour, indexed)

	return s
}

// txKeyAt returns the key of seq of captureLow in memTestHour.
func txKeyAt(seq uint64) TxKey {
	return TxKey{Capture: captureLow, Seq: seq, Hour: memTestHour}
}

// requireOneScopeRead requires that s was observed and closed once and that only scope memTestHour was read, once.
func requireOneScopeRead(t testing.TB, s *memSource) {
	t.Helper()

	observes, scopes, closes := s.counts()
	require.Equal(t, 1, observes)
	require.Equal(t, map[int64]int{memTestHour: 1}, scopes)
	require.Equal(t, 1, closes)
}

// TestFindTransactionMissingPrimary looks up a seq that an indexed scope read without a defect does not hold,
// below its first seq, between two seqs and above its last, and in an empty hour:
// each fails with ErrNotPrimary after one scope read, the result carrying the read's gaps and its searched scope.
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true, tt.file)
			res, err := findTx(t, t.Context(), s, tt.key, TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, ErrNotPrimary)
			assert.Equal(t, TxOutcome(0), res.Outcome)
			assert.Equal(t, []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}}}, res.Searched)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Gaps)))
			assert.Empty(t, res.Records)
			requireOneScopeRead(t, s)
		})
	}

	t.Run("an empty hour", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, true, file)
		s.setIndexed(captureLow, memTestHour+1, true)
		key := txKeyAt(11)
		key.Hour++
		res, err := findTx(t, t.Context(), s, key, TxOptions{})
		require.ErrorIs(t, err, ErrNotPrimary)
		assert.Equal(t, []TxScope{{Hour: memTestHour + 1, Indexed: true}}, res.Searched)
		assert.Empty(t, res.Gaps)
		_, scopes, closes := s.counts()
		assert.Equal(t, map[int64]int{memTestHour + 1: 1}, scopes)
		assert.Equal(t, 1, closes)
	})
}

// TestFindTransactionMissingPrimaryExplained looks up a seq that its scope's read does not yield,
// beside a gap that explains it:
// a failed block, a coverage entry that meets the seq and the hour, and a scope not indexed;
// the primary has no key, the outcome is TxIncomplete, and no further scope is read.
// A coverage entry that misses the seq, the hour or the capture explains nothing: ErrNotPrimary.
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

	tests := []struct {
		name     string
		file     []byte
		cold     bool
		gaps     []TxGapReason
		notFound bool
		// covered reports that the pack's one coverage entry explains the missing primary, so the no-key gap carries it.
		covered bool
	}{
		{name: "a failed block", file: corrupt, gaps: []TxGapReason{TxGapRead, TxGapNoKey}},
		{name: "a scope not indexed", file: withCoverage(Coverage{SeqFirst: seqOf(1), SeqLast: seqOf(2)}), cold: true,
			gaps: []TxGapReason{TxGapCold, TxGapNoKey}},
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

			s := txSource(t, !tt.cold, tt.file)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			requireOneScopeRead(t, s)
			if tt.notFound {
				require.ErrorIs(t, err, ErrNotPrimary)
				assert.Empty(t, res.Gaps)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, tt.gaps, gapReasons(res.Gaps))
			noKey := res.Gaps[len(res.Gaps)-1]
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
			assert.Equal(t, []TxScope{{Hour: memTestHour, Indexed: !tt.cold, Packs: []UUID{seg0}}}, res.Searched)
		})
	}

	t.Run("the failed block's gap", func(t *testing.T) {
		t.Parallel()

		res, err := findTx(t, t.Context(), txSource(t, true, corrupt), txKeyAt(12), TxOptions{})
		require.NoError(t, err)
		g := res.Gaps[0]
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
// its validity bit clear or its bytes missing; each fails with ErrNotPrimary after one scope read,
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

			s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(11, nil), tt.rec(t), txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, ErrNotPrimary)
			require.ErrorContains(t, err, tt.why)
			assert.Equal(t, TxResult{Searched: []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}}}}, res)
			requireOneScopeRead(t, s)
		})
	}
}

// TestFindTransactionNoKey looks up primaries without a key:
// in unknown or local direction, with SessionID or System Bytes unavailable;
// the outcome is TxIncomplete with a TxGapNoKey gap after one scope read,
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
			s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(11, nil), primary, txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.NoError(t, err)
			requireOneScopeRead(t, s)

			want := tt.want
			want.Outcome, want.Epoch = TxIncomplete, txTestEpoch
			want.Stream, want.Function, want.StreamAvailable, want.W, want.WAvailable = 1, 3, true, true, true
			want.Records = []TxRecord{{Record: primary, Hour: memTestHour, Pack: seg0, Class: TxPrimary}}
			want.Searched = []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}}}
			require.Len(t, res.Gaps, 1)
			assert.Equal(t, TxGapNoKey, res.Gaps[0].Reason)
			assert.Equal(t, new(uint64(12)), res.Gaps[0].Seq)
			assert.Equal(t, []int64{memTestHour}, res.Gaps[0].Hours)
			assert.Nil(t, res.Gaps[0].Pack)
			assert.Equal(t, [2]int64{-1, -1}, [2]int64{int64(res.Gaps[0].Block), res.Gaps[0].Offset})
			res.Gaps = nil
			assert.Equal(t, want, res)
		})
	}
}

// TestFindTransactionConflictingPrimary looks up a primary that two packs of its scope hold in different versions:
// TxGapConflict and TxGapNoKey gaps, the outcome TxIncomplete after one scope read,
// both versions kept as primaries, marked conflicting, with their packs, and the key derived from the first.
func TestFindTransactionConflictingPrimary(t *testing.T) {
	t.Parallel()

	steps := txBlock(txSeqs(11, 12, 13)...)
	s := txSource(t, true, txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 12)))
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
	require.NoError(t, err)
	requireOneScopeRead(t, s)

	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(res.Gaps))
	assert.Equal(t, new(uint64(12)), res.Gaps[0].Seq)
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
// and the outcome is TxIncomplete after one scope read, both versions kept and marked,
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
			s := txSource(t, true, txPack(t, seg0, nil, steps), txFlagged(t, txPack(t, seg1, nil, steps), 12))
			// One scope, so that no other scope's gap makes the outcome incomplete.
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, TxIncomplete, res.Outcome)
			assert.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(res.Gaps))
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
// The same-key primary at 13 bounds an empty window, so the outcome is TxUnmatched;
// in epoch 0 it lies in another epoch, so the window stays open and the outcome is TxIncomplete.
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
		{name: "every field available", stream: true, epoch: txTestEpoch, outcome: TxUnmatched},
		{name: "the stream unavailable", edit: func(r *Record) { r.FieldValidity &^= FieldValidityStreamAndW }, epoch: txTestEpoch,
			outcome: TxUnmatched},
		{name: "correlation-incomplete", edit: func(r *Record) { r.Epoch = 0 }, stream: true,
			gaps: []TxGapReason{TxGapCorrelation, TxGapOpenWindow}, outcome: TxIncomplete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary := txRecord(12, tt.edit)
			s := txSource(t, true, txPack(t, seg0, nil, txBlock(txRecord(11, nil), primary, txRecord(13, nil))))
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 1})
			require.NoError(t, err)
			assert.Equal(t, tt.outcome, res.Outcome)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Gaps)))
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
			_, _, closes := s.counts()
			assert.Equal(t, 1, closes)
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
// each of recs lying above that range.
// The footer states no closure or capture-boundary in the block, so it stays valid when one of recs is a closing record.
func txMisindexedAt(t testing.TB, recs ...Record) []byte {
	t.Helper()

	last := 10 + uint64(len(recs))
	require.Greater(t, recs[0].Seq, last)
	file := reindexedWith(t, txPack(t, seg0, nil, txBlock(append([]Record{txRecord(10, nil)}, recs...)...)), func(_ int, s *blockSummary) {
		s.lastSeq = last
		s.seqRanges = []seqRange{{first: 10, last: last}}
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

// TestFindTransactionPrimaryAfterHigherSeqs reads a scope where a block whose F-2 entry under-reports its last seq
// yields a seq above the primary before the primary arrives from another pack:
// the order was broken, a TxGapIndex gap at the primary's seq beside the block's ReasonIndexMismatch defect.
// The record above the primary, a same-key primary at 20, is classified once the primary is established
// (the tracepack semantics specification §7.2): it is kept and bounds the window, whose seqs 16 to 19 no read yields.
// Without the primary, the records above it are no gap of their own: the defect explains the missing primary.
func TestFindTransactionPrimaryAfterHigherSeqs(t *testing.T) {
	t.Parallel()

	// The one block of seg0 holds seqs 10 and 20, while its index states 10 to 11.
	misindexed := txMisindexed(t, 20)
	primary := txPack(t, seg1, nil, txBlock(txSeqs(15)...))

	t.Run("the primary arrives", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, true, misindexed, primary)
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		require.Empty(t, res.FooterErrs, "the footer must stay valid")
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, new(uint64(20)), res.WindowEnd)
		assert.Equal(t, []string{"15@0 primary", "20@0 same-key-primary bound"}, recordFlags(res.Records))
		require.Len(t, res.Records, 2)
		assert.Equal(t, seg1, res.Records[0].Pack)
		assert.Equal(t, seg0, res.Records[1].Pack)
		assert.Equal(t, 0, res.Records[1].Block)
		assert.Equal(t, []string{"index", "index@15", "seq-gap@16"}, gapSeqs(res.Gaps))
		assert.Equal(t, ReasonIndexMismatch, res.Gaps[0].Defect)
		assert.Equal(t, new(seg0), res.Gaps[0].Pack)
		assert.Nil(t, res.Gaps[1].Pack)
		assert.Equal(t, []TxScope{{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0, seg1}}}, res.Searched)
	})

	t.Run("the primary is missing", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, true, misindexed)
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapNoKey}, gapReasons(res.Gaps))
		assert.Equal(t, ReasonIndexMismatch, res.Gaps[0].Defect)
	})
}

// txEarlyCase is a lookup of the primary at 15 in a scope where seg0's block, whose F-2 entry states a range from 10,
// also holds records above the primary, which arrive before it, while seg1 holds the primary and the records after it.
type txEarlyCase struct {
	name string
	// early is held by seg0's misindexed block beside 10, and later by seg1 after the primary.
	early, later func(t testing.TB) []Record
	// primary, when set, changes the primary at 15, txRecord's host-to-equipment S1F3 otherwise.
	primary func(r *Record)
	// closure, when not zero, is the close_seq of the primary's epoch in the per-capture evidence.
	closure uint64
	// noEvidence gives the capture empty per-capture evidence, so that every boundary is reported as a record read.
	noEvidence bool
	window     *uint64
	records    []string
	// packs, when set, is the pack of each record; otherwise every record of early's first seq is required to be seg0's.
	packs []UUID
	gaps  []string
}

// check runs the lookup of c and requires its outcome TxIncomplete, its window, records and gaps,
// the footer of seg0 valid, and a capture-boundary gap at 17 placed in seg0's block 0.
func (c *txEarlyCase) check(t *testing.T) {
	t.Helper()

	later := append([]Record{txRecord(15, c.primary)}, c.later(t)...)
	s := txSource(t, true, txMisindexedAt(t, c.early(t)...), txPack(t, seg1, nil, txBlock(later...)))
	if c.closure != 0 {
		s.addClosure(captureLow, txTestEpoch, c.closure)
	}
	if c.noEvidence {
		s.giveEvidence(captureLow, CaptureEvidence{})
	}
	res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.Empty(t, res.FooterErrs, "the footer must stay valid")
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, later[0].Dir, res.Dir, "the primary's direction")
	assert.Equal(t, c.window, res.WindowEnd)
	assert.Equal(t, c.records, recordFlags(res.Records))
	assert.Equal(t, c.gaps, gapSeqs(res.Gaps))
	if c.packs != nil {
		packs := make([]UUID, 0, len(res.Records))
		for _, r := range res.Records {
			packs = append(packs, r.Pack)
		}
		assert.Equal(t, c.packs, packs)
	}
	for _, r := range res.Records[1:] {
		if c.packs == nil && r.Record.Seq == c.early(t)[0].Seq {
			assert.Equal(t, seg0, r.Pack, "seq %d is seg0's", r.Record.Seq)
			assert.Equal(t, 0, r.Block)
			assert.Equal(t, memTestHour, r.Hour)
		}
	}
	for _, g := range res.Gaps {
		if g.Reason == TxGapCaptureBoundary && *g.Seq == 17 {
			assert.Equal(t, []int64{memTestHour}, g.Hours)
			assert.Equal(t, new(seg0), g.Pack)
			assert.Equal(t, 0, g.Block)
			require.NotNil(t, g.Barrier)
			assert.Equal(t, BoundaryKindGap, g.Barrier.Kind)
		}
	}
}

// TestFindTransactionEarlyVersions reads, in the primary's scope, seg0's block whose F-2 entry states seqs 10 to 11
// while it also holds records above the primary at 15, so that they arrive before the primary, from seg1:
// each is classified once the primary is established, as every other version read
// (the tracepack semantics specification §7.2), and supplies its roles, flags, window bound and facts;
// the disagreeing block stays a TxGapIndex gap, beside the one at the primary, and the outcome stays TxIncomplete.
func TestFindTransactionEarlyVersions(t *testing.T) {
	t.Parallel()

	note := func(seq uint64) Record { return txNote(seq, nil) }
	tests := []txEarlyCase{
		{
			name:    "an early candidate",
			early:   func(testing.TB) []Record { return []Record{txReply(16, nil)} },
			later:   func(testing.TB) []Record { return []Record{note(17), txRecord(18, nil)} },
			window:  new(uint64(18)),
			records: []string{"15@0 primary", "16@0 candidate in eligible decidable valid", "18@0 same-key-primary bound"},
			gaps:    []string{"index", "index@15"},
		},
		{
			name:    "an early same-key primary bounds the window",
			early:   func(testing.TB) []Record { return []Record{txRecord(17, nil)} },
			later:   func(testing.TB) []Record { return []Record{note(16), txReply(19, nil)} },
			window:  new(uint64(17)),
			records: []string{"15@0 primary", "17@0 same-key-primary bound", "19@0 candidate decidable"},
			gaps:    []string{"index", "index@15"},
		},
		{
			name:    "an early socket-close bounds the window and meets the close_seq",
			early:   func(t testing.TB) []Record { return []Record{txSocketClose(t, 17)} },
			later:   func(testing.TB) []Record { return []Record{note(16), txReply(19, nil)} },
			closure: 17,
			window:  new(uint64(17)),
			records: []string{"15@0 primary", "17@0 closing bound", "19@0 candidate decidable"},
			gaps:    []string{"index", "index@15"},
		},
		{
			name: "an early gap boundary of the epoch",
			early: func(t testing.TB) []Record {
				return []Record{txBoundaryAt(t, 17, txTestEpoch, BoundaryKindGap, nil, nil)}
			},
			later:   func(testing.TB) []Record { return []Record{note(16), txRecord(18, nil)} },
			window:  new(uint64(18)),
			records: []string{"15@0 primary", "18@0 same-key-primary bound"},
			gaps:    []string{"index", "index@15", "capture-boundary@17"},
		},
		{
			name: "an early gap boundary before a later one",
			early: func(t testing.TB) []Record {
				return []Record{txBoundaryAt(t, 17, txTestEpoch, BoundaryKindGap, nil, nil)}
			},
			later: func(t testing.TB) []Record {
				return []Record{txBoundaryAt(t, 16, txTestEpoch, BoundaryKindGap, nil, nil), txRecord(18, nil)}
			},
			noEvidence: true,
			window:     new(uint64(18)),
			records:    []string{"15@0 primary", "18@0 same-key-primary bound"},
			// The read yields 17 first, so its fact is found first.
			gaps: []string{"index", "index@15", "capture-boundary@17", "capture-boundary@16"},
		},
		{
			name:    "a close_seq at an early annotation",
			early:   func(testing.TB) []Record { return []Record{note(17)} },
			later:   func(testing.TB) []Record { return []Record{note(16), txRecord(18, nil)} },
			closure: 17,
			window:  new(uint64(18)),
			records: []string{"15@0 primary", "18@0 same-key-primary bound"},
			gaps:    []string{"index", "index@15", "contradiction@17"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.check(t)
		})
	}
}

// TestFindTransactionEarlyAndLaterVersions reads a seq whose version in seg0's misindexed block arrives before the primary at 15,
// while seg1, a pack of the same scope, yields another version of it after the primary, uncompared:
// the tracepack semantics specification §7.2 keeps every version of a seq that the same scope's read yielded
// once one of them plays a role, so both versions are kept, in the order the read yielded them,
// whichever of them plays the role, the early one or the later one,
// for a host-to-equipment primary and, with every direction swapped, an equipment-to-host one.
func TestFindTransactionEarlyAndLaterVersions(t *testing.T) {
	t.Parallel()

	base := []txEarlyCase{
		{
			name:    "an early annotation, a later same-key primary",
			early:   func(testing.TB) []Record { return []Record{txNote(18, nil)} },
			later:   func(testing.TB) []Record { return []Record{txNote(16, nil), txReply(17, nil), txRecord(18, nil)} },
			window:  new(uint64(18)),
			records: []string{"15@0 primary", "17@0 candidate in eligible decidable valid", "18@0 none", "18@0 same-key-primary bound"},
			packs:   []UUID{seg1, seg1, seg0, seg1},
			gaps:    []string{"index", "index@15"},
		},
		{
			name:    "an early same-key primary, a later annotation",
			early:   func(testing.TB) []Record { return []Record{txRecord(18, nil)} },
			later:   func(testing.TB) []Record { return []Record{txNote(16, nil), txReply(17, nil), txNote(18, nil)} },
			window:  new(uint64(18)),
			records: []string{"15@0 primary", "17@0 candidate in eligible decidable valid", "18@0 same-key-primary bound", "18@0 none"},
			packs:   []UUID{seg1, seg1, seg0, seg1},
			gaps:    []string{"index", "index@15"},
		},
		{
			name: "early versions of two seqs, later ones of both",
			early: func(testing.TB) []Record {
				return []Record{txNote(17, []byte{1}), txRecord(18, nil)}
			},
			later: func(testing.TB) []Record {
				return []Record{txNote(16, nil), txReply(17, nil), txNote(18, []byte{2})}
			},
			window: new(uint64(18)),
			records: []string{
				"15@0 primary", "17@0 none in", "17@0 candidate in eligible decidable valid", "18@0 same-key-primary bound", "18@0 none",
			},
			packs: []UUID{seg1, seg0, seg1, seg0, seg1},
			gaps:  []string{"index", "index@15"},
		},
	}
	// The same cases with the directions swapped: an equipment-to-host primary, a host-to-equipment candidate.
	toEquipment := func(r *Record) { r.Dir = DirEquipmentToHost }
	toHost := func(r *Record) { r.Dir = DirHostToEquipment }
	tests := make([]txEarlyCase, 0, 2*len(base))
	tests = append(tests, base...)
	for _, c := range base {
		early, later := c.early, c.later
		c.name += ", the directions swapped"
		c.primary = toEquipment
		c.early = func(t testing.TB) []Record { return swapDirs(early(t), toEquipment, toHost) }
		c.later = func(t testing.TB) []Record { return swapDirs(later(t), toEquipment, toHost) }
		tests = append(tests, c)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.check(t)
		})
	}
}

// swapDirs returns recs with every host-to-equipment data record changed by toEquipment
// and every equipment-to-host one by toHost; other records are unchanged.
func swapDirs(recs []Record, toEquipment, toHost func(r *Record)) []Record {
	out := slices.Clone(recs)
	for i := range out {
		switch {
		case out[i].Kind != KindData:
		case out[i].Dir == DirHostToEquipment:
			toEquipment(&out[i])
		case out[i].Dir == DirEquipmentToHost:
			toHost(&out[i])
		default:
			// Local or unknown: no direction to swap.
		}
	}

	return out
}

// TestFindTransactionEarlyStopOfAnotherEpoch reads, in the primary's scope, seg0's block whose F-2 entry states seqs 10 to 11
// while it holds 10 and a clean stop of epoch 2 at 17, both of epoch 2, so that the stop arrives before the epoch-1 primary at 15, from seg1,
// beside per-capture evidence claiming a stop of epoch 1 at 17.
// The early stop is classified once the primary is established: a stop of any epoch is a closing record, so it bounds the window at 17.
// It is not the boundary of the primary's epoch that the evidence names at 17, so that entry is a capture-boundary,
// and the claim, which no version at 17 meets, a contradiction.
func TestFindTransactionEarlyStopOfAnotherEpoch(t *testing.T) {
	t.Parallel()

	const other = txTestEpoch + 1
	inOther := func(r *Record) { r.Epoch = other }
	file := reindexedWith(t, txPack(t, seg0, nil, txBlock(txRecord(10, inOther), txBoundaryAt(t, 17, other, BoundaryKindStop, nil, nil))),
		func(_ int, s *blockSummary) {
			s.lastSeq = 11
			s.seqRanges = []seqRange{{first: 10, last: 11}}
			require.Len(t, s.epochs, 1)
			s.epochs[0].seqLast = 11
			// The closure and boundary entries of the stop would name a seq outside the stated range.
			s.epochs[0].closeSeq, s.epochs[0].hasCloseSeq = 0, false
			s.boundaries = nil
		})
	tr := layoutOf(t, file).tr
	tr.LastSeq = 11
	misindexed := format.AppendTrailer(file[:len(file)-format.TrailerLen], &tr)

	s := txSource(t, true, misindexed, txPack(t, seg1, nil, txBlock(txRecord(15, nil), txNote(16, nil))))
	s.addBoundary(Boundary{Capture: captureLow, Seq: 17, Kind: BoundaryKindStop, TS: blockTestHour + 17, Epoch: txTestEpoch})
	res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	require.Empty(t, res.FooterErrs, "the footer must stay valid")
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, new(uint64(17)), res.WindowEnd)
	assert.Equal(t, []string{"15@0 primary", "17@0 closing bound"}, recordFlags(res.Records))
	require.Len(t, res.Records, 2)
	assert.Equal(t, seg0, res.Records[1].Pack)
	assert.Equal(t, uint32(other), res.Records[1].Record.Epoch)
	assert.Equal(t, []string{"index", "index@15", "capture-boundary@17", "contradiction@17"}, gapSeqs(res.Gaps))
	b := res.Gaps[2].Barrier
	require.NotNil(t, b)
	assert.Equal(t, BoundaryKindStop, b.Kind)
	assert.Equal(t, uint32(txTestEpoch), b.Epoch)
	assert.Nil(t, res.Gaps[2].Pack, "the evidence entry, not a record read")
}

// TestFindTransactionEarlyCopyOfConflict reads a same-key primary at 20
// whose copy in seg0's misindexed block arrives before the primary at 15,
// while seg1 and seg2 hold versions of 20 that the read compares and finds in conflict:
// the early copy is kept before them, in the order the read yielded the versions, and marked as a conflict with them.
// A same-key primary bounds the window whether or not it conflicts, so the window ends at 20, every same-key version bound.
func TestFindTransactionEarlyCopyOfConflict(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txMisindexed(t, 20),
		txPack(t, seg1, nil, txBlock(txSeqs(15, 20)...)), txPack(t, seg2, nil, changed(txBlock(txSeqs(20)...), 1, 20)))
	res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 1})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, new(uint64(20)), res.WindowEnd)
	assert.Equal(t, []uint64{20}, conflictSeqs(res.Conflicts))
	assert.Equal(t, []string{"15@0 primary", "20@0 same-key-primary! bound", "20@0 same-key-primary! bound", "20@0 none!"},
		recordFlags(res.Records))
	packs := make([]UUID, 0, len(res.Records))
	for _, r := range res.Records {
		packs = append(packs, r.Pack)
	}
	assert.Equal(t, []UUID{seg1, seg0, seg1, seg2}, packs)
	assert.Equal(t, []string{"index", "index@15", "conflict@20", "seq-gap@16"}, gapSeqs(res.Gaps))
}

// TestFindTransactionClosesOnce ends lookups on each error of the source and of the observation,
// on ctx done before the evidence and during the scope read, and on a Close error:
// the observation is closed exactly once whenever Observe returned one,
// the error is returned wrapped with the Outcome zero,
// and a Close error is returned only when nothing else failed.
func TestFindTransactionClosesOnce(t *testing.T) {
	t.Parallel()

	file := txPack(t, seg0, nil, txBlock(txSeqs(11, 13)...))
	boom := errors.New("boom")
	tests := []struct {
		name   string
		setup  func(s *memSource, cancel context.CancelFunc)
		err    error
		closes int
		scopes map[int64]int
		gaps   []TxGapReason
	}{
		{name: "Observe fails", setup: func(s *memSource, _ context.CancelFunc) { s.observeErr = boom }, err: boom},
		{name: "Evidence fails", setup: func(s *memSource, _ context.CancelFunc) {
			s.beforeEvidence = func(context.Context) error { return boom }
		}, err: boom, closes: 1},
		{name: "Scope fails", setup: func(s *memSource, _ context.CancelFunc) {
			s.beforeScope = func(context.Context, int64) error { return boom }
		}, err: boom, closes: 1, scopes: map[int64]int{memTestHour: 1}},
		{name: "ctx done before the evidence", setup: func(s *memSource, cancel context.CancelFunc) {
			s.beforeEvidence = func(context.Context) error { cancel(); return nil }
		}, err: context.Canceled, closes: 1},
		{name: "ctx done during the scope read", setup: func(s *memSource, cancel context.CancelFunc) {
			s.beforeScope = func(context.Context, int64) error { cancel(); return nil }
		}, err: context.Canceled, closes: 1, scopes: map[int64]int{memTestHour: 1}},
		{name: "Close fails after ErrNotPrimary", setup: func(s *memSource, _ context.CancelFunc) {
			s.closeErr = boom
		}, err: ErrNotPrimary, closes: 1, scopes: map[int64]int{memTestHour: 1}},
		{name: "Close fails after a primary without a key", setup: func(s *memSource, _ context.CancelFunc) {
			s.closeErr = boom
			s.setIndexed(captureLow, memTestHour, false)
		}, err: boom, closes: 1, scopes: map[int64]int{memTestHour: 1}, gaps: []TxGapReason{TxGapCold, TxGapNoKey}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, true, file)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tt.setup(s, cancel)
			res, err := findTx(t, ctx, s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, TxOutcome(0), res.Outcome)
			assert.Equal(t, tt.gaps, nilIfEmpty(gapReasons(res.Gaps)))
			observes, scopes, closes := s.counts()
			assert.Equal(t, 1, observes)
			assert.Equal(t, tt.closes, closes)
			assert.Equal(t, nilIfEmptyMap(tt.scopes), nilIfEmptyMap(scopes))
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

// TestFindTransactionConflictLimit reads a scope with two conflicts, beside a primary without a key:
// MaxConflicts 2 lets the read finish and lists both, with no conflict gap since the lookup ends at the primary;
// MaxConflicts 1 ends the read at the second with ErrReadLimit, the first listed,
// the scope not searched and the observation closed once.
func TestFindTransactionConflictLimit(t *testing.T) {
	t.Parallel()

	unknown := func(r *Record) { r.Dir = DirUnknown }
	steps := txBlock(txRecord(10, nil), txRecord(11, unknown), txRecord(12, nil), txRecord(13, nil))
	files := [][]byte{txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 10, 13))}

	s := txSource(t, true, files...)
	res, err := findTx(t, t.Context(), s, txKeyAt(11), TxOptions{MaxConflicts: 2})
	require.NoError(t, err)
	assert.Equal(t, TxIncomplete, res.Outcome)
	assert.Equal(t, []uint64{10, 13}, conflictSeqs(res.Conflicts))
	assert.Equal(t, []TxGapReason{TxGapNoKey}, gapReasons(res.Gaps), "no conflict gap beside a primary without a key")

	s = txSource(t, true, files...)
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
	s := txSource(t, true, file)
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

// TestFindTransactionNamesPacks reads a scope of two packs whose second, in view order, has a corrupt block,
// or a footer that fails validation over truthful blocks, read by walking:
// the read gap, or the footer error, names the second pack's pack_id and the hour,
// and the walked pack, which the walk accounts for in full, adds no gap of its read;
// its registration marked the per-capture evidence partial, which keeps the outcome from TxUnmatched.
func TestFindTransactionNamesPacks(t *testing.T) {
	t.Parallel()

	first := txPack(t, seg0, nil, txBlock(txSeqs(10, 11)...))
	second := txPack(t, seg1, nil, slices.Concat(txBlock(txSeqs(12)...), txBlock(txSeqs(13)...)))

	t.Run("a corrupt block", func(t *testing.T) {
		t.Parallel()

		corrupt := flipByte(second, mustOpen(t, second, ReaderOptions{}).Blocks()[1].Offset+format.EnvelopeLen+3)
		res, err := findTx(t, t.Context(), txSource(t, true, first, corrupt), txKeyAt(11), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		require.Len(t, res.Gaps, 1, "the same-key primary at 12 bounds an empty window")
		assert.Equal(t, TxGapRead, res.Gaps[0].Reason)
		assert.Equal(t, new(seg1), res.Gaps[0].Pack)
		assert.Equal(t, 1, res.Gaps[0].Block)
		assert.Equal(t, []UUID{seg0, seg1}, res.Searched[0].Packs)
	})

	t.Run("a footer that fails validation", func(t *testing.T) {
		t.Parallel()

		walked := invalidFooterFile(t, second)
		res, err := findTx(t, t.Context(), txSource(t, true, first, walked), txKeyAt(11), TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapEvidence}, gapReasons(res.Gaps))
		require.Len(t, res.FooterErrs, 1)
		fe := res.FooterErrs[0]
		assert.Equal(t, memTestHour, fe.Hour)
		assert.Equal(t, seg1, fe.Pack)
		require.ErrorIs(t, fe.Err, ErrInvalidFooter)
		assert.Equal(t, []UUID{seg0, seg1}, res.Searched[0].Packs)
	})
}

// TestFindTransactionSplitPrimaryCopies reads a scope where a block whose F-2 entry under-reports its last seq
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

		res, err := findTx(t, t.Context(), txSource(t, true, misindexed, txPack(t, seg1, nil, other)), txKeyAt(15),
			TxOptions{MaxScopes: 1})
		require.NoError(t, err)
		require.Empty(t, res.FooterErrs, "the footer must stay valid")
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapSeqGap}, gapReasons(res.Gaps))
		assert.Equal(t, ReasonIndexMismatch, res.Gaps[0].Defect)
		assert.Equal(t, new(uint64(16)), res.Gaps[1].Seq, "the window (15, 20) holds no seq read")
		assert.Empty(t, res.Conflicts)
		assert.Equal(t, []TxRecord{
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg0, Class: TxPrimary},
			{Record: txRecord(20, nil), Hour: memTestHour, Pack: seg1, Class: TxSameKeyPrimary, Bound: true},
		}, res.Records)
		assert.Equal(t, DirHostToEquipment, res.Dir)
	})

	t.Run("different copies", func(t *testing.T) {
		t.Parallel()

		s := txSource(t, true, misindexed, txPack(t, seg1, nil, changed(other, 1, 15)))
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 3})
		require.NoError(t, err)
		requireOneScopeRead(t, s)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapConflict, TxGapNoKey}, gapReasons(res.Gaps))
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
		s := txSource(t, true, misindexed, txFlagged(t, txPack(t, seg1, nil, other), 15))
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 3})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapIndex, TxGapConflict, TxGapNoKey}, gapReasons(res.Gaps))
		assert.Empty(t, res.Conflicts, "the read compared neither copy with the other")
		assert.Equal(t, []TxRecord{
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg0, Conflict: true, Class: TxPrimary},
			{Record: txRecord(15, nil), Hour: memTestHour, Pack: seg1, Conflict: true, Class: TxPrimary},
		}, res.Records)
		requireOneScopeRead(t, s)
	})
}

// TestFindTransactionClosesOnPanic panics inside the observation's Scope:
// the panic goes on to the caller, and the observation is closed once on its way.
func TestFindTransactionClosesOnPanic(t *testing.T) {
	t.Parallel()

	s := txSource(t, true, txPack(t, seg0, nil, txBlock(txSeqs(11)...)))
	s.beforeScope = func(context.Context, int64) error { panic("scope") }
	assert.PanicsWithValue(t, "scope", func() { _, _ = FindTransaction(t.Context(), s, txKeyAt(11), TxOptions{}) })
	_, _, closes := s.counts()
	assert.Equal(t, 1, closes)
}

// TestCoverageMeets matches coverage entries against a seq range and a time range by the tracepack format specification §5:
// both sides inclusive on the entry, the time range half-open, an absent bound unbounded,
// an inverted side meeting every query of the capture, and an entry of another capture meeting none.
func TestCoverageMeets(t *testing.T) {
	t.Parallel()

	at := func(v int64) *int64 { return &v }
	seqOf := func(v uint64) *uint64 { return &v }
	own, other := captureLow, captureHigh
	tests := []struct {
		name string
		c    Coverage
		want bool
	}{
		{name: "no bounds", c: Coverage{}, want: true},
		{name: "this capture named", c: Coverage{CaptureID: &own}, want: true},
		{name: "another capture", c: Coverage{CaptureID: &other}},
		{name: "another capture, inverted", c: Coverage{CaptureID: &other, SeqFirst: seqOf(9), SeqLast: seqOf(1)}},
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
		assert.Equal(t, tt.want, coverageMeets(&tt.c, own, 10, 20, 100, 200), tt.name)
	}
}
