package tracepack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txFrame returns an edit that changes a clone of the record's payload with edit.
func txFrame(edit func(p []byte)) func(r *Record) {
	return func(r *Record) {
		r.Payload = slices.Clone(r.Payload)
		edit(r.Payload)
	}
}

// txReply returns the reply to txRecord's primary at seq: an S1F4 equipment-to-host data record, changed by edit when set.
func txReply(seq uint64, edit func(r *Record)) Record {
	return txRecord(seq, func(r *Record) {
		r.Dir = DirEquipmentToHost
		txFrame(func(p []byte) {
			p[fieldByte6Off] = 0x01
			p[fieldFunctionOff] = 4
		})(r)
		if edit != nil {
			edit(r)
		}
	})
}

// txControl returns an equipment-to-host Reject.req control record at 13 over txRecord's frame, SType 7,
// with the field_validity of a raw capture, changed by edit when set.
func txControl(edit func(r *Record)) Record {
	return txRecord(13, func(r *Record) {
		r.Kind, r.Dir = KindControl, DirEquipmentToHost
		txFrame(func(p []byte) { p[fieldSTypeOff] = sTypeRejectReq })(r)
		r.SetCapturedFieldValidity()
		if edit != nil {
			edit(r)
		}
	})
}

// txT3 returns a T3 timer-expiry event naming txRecord's primary: SessionID, System Bytes, stream 1 and function 3,
// changed by edit when set.
func txT3(edit func(ev *TransportEvent)) *TransportEvent {
	ev := &TransportEvent{
		Event: EventTimerExpiry, Timer: new(TimerT3),
		PrimarySessionID: new(uint64(0x1234)), PrimarySystemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, PrimarySystemBytesPresent: true,
		PrimaryStream: new(uint64(1)), PrimaryFunction: new(uint64(3)),
	}
	if edit != nil {
		edit(ev)
	}

	return ev
}

// inHour returns r moved by hours whole hours.
func inHour(hours int64, r Record) Record {
	r.TSUTCNs += hours * hourNs

	return r
}

// inHours returns the records of steps moved by hours whole hours.
func inHours(hours int64, steps []footerTestStep) []footerTestStep {
	out := slices.Clone(steps)
	for i := range out {
		out[i].rec = inHour(hours, out[i].rec)
	}

	return out
}

// txPackIn writes steps, moved by hours whole hours, into a pack id as txPack does,
// its period the hour memTestHour + hours, its pack metadata then changed by meta when set.
func txPackIn(t testing.TB, id UUID, hours int64, meta func(m *PackMeta), steps []footerTestStep) []byte {
	t.Helper()

	return txPack(t, id, func(m *PackMeta) {
		m.PeriodStart += hours * hourNs
		m.PeriodEnd += hours * hourNs
		if meta != nil {
			meta(m)
		}
	}, inHours(hours, steps))
}

// txHours returns a memSource holding files[i] in captureLow's scope memTestHour + i, each of those scopes indexed.
func txHours(t testing.TB, files ...[][]byte) *memSource {
	t.Helper()

	s := newMemSource()
	for i, fs := range files {
		hour := memTestHour + int64(i)
		for _, f := range fs {
			s.addPack(t, hour, f)
		}
		s.setIndexed(captureLow, hour, true)
	}

	return s
}

// runTxLookup runs a lookup of key with opts over one observation of s and closes it,
// returning the lookup so that a test reads its state, whose charge it requires to equal its recount.
func runTxLookup(t testing.TB, s *memSource, key TxKey, opts TxOptions) (*txLookup, error) {
	t.Helper()

	opts, err := checkFindTransaction(s, key, opts)
	require.NoError(t, err)
	obs, err := s.Observe(t.Context(), key.Capture, key.Hour, key.Hour+int64(opts.MaxScopes))
	require.NoError(t, err)
	defer func() { require.NoError(t, obs.Close()) }()

	l := newTxLookup(key, opts, obs)
	err = l.run(t.Context())
	requireStateRecount(t, l)

	return l, err
}

// recordSeqs describes each record as "<seq>@<hour - memTestHour> <class>", with "!" after a conflicting one, in order.
func recordSeqs(recs []TxRecord) []string {
	out := make([]string, len(recs))
	for i := range recs {
		r := &recs[i]
		out[i] = fmt.Sprintf("%d@%d %v", r.Record.Seq, r.Hour-memTestHour, r.Class)
		if r.Conflict {
			out[i] += "!"
		}
	}

	return out
}

// TestTxPrimaryKeyClassify classifies records above txRecord's primary at 12 (S1F3 W host-to-equipment, epoch 1,
// SessionID 0x1234, System Bytes DE AD BE EF) by the roles of the tracepack semantics specification §7.2,
// one case for each availability that decides a role.
func TestTxPrimaryKeyClassify(t *testing.T) {
	t.Parallel()

	noStream := func(r *Record) { r.FieldValidity &^= FieldValidityStreamAndW }
	event := func(epoch uint32, ev *TransportEvent) Record {
		return testEventRecord(t, 13, blockTestHour+13, epoch, ev)
	}
	boundary := func(epoch uint32, kind *BoundaryKind) Record {
		return event(epoch, &TransportEvent{Event: EventCaptureBoundary, BoundaryKind: kind})
	}
	otherSB := txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 })
	tests := []struct {
		name string
		// primary changes the primary when set.
		primary func(r *Record)
		rec     Record
		want    TxClass
	}{
		{name: "a reply", rec: txReply(13, nil), want: TxCandidate},
		{name: "a reply without stream", rec: txReply(13, noStream), want: TxCandidate},
		{name: "a reply to another System Bytes", rec: txReply(13, otherSB)},
		{name: "a reply to another SessionID", rec: txReply(13, txFrame(func(p []byte) { p[fieldSessionIDOff] = 0 }))},
		{name: "a reply with System Bytes unavailable", rec: txReply(13, func(r *Record) { r.FieldValidity &^= FieldValiditySystemBytes }),
			want: TxPossibleReply},
		{name: "a reply with SessionID unavailable", rec: txReply(13, func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }),
			want: TxPossibleReply},
		{name: "a reply with System Bytes unavailable and another SessionID", rec: txReply(13, func(r *Record) {
			r.FieldValidity &^= FieldValiditySystemBytes
			txFrame(func(p []byte) { p[fieldSessionIDOff] = 0 })(r)
		})},
		{name: "a reply whose bytes stop before its SessionID", rec: txReply(13, func(r *Record) {
			r.Payload = r.Payload[:fieldSessionIDEnd-1]
		}), want: TxPossibleReply},
		{name: "a reply in another epoch", rec: txReply(13, func(r *Record) { r.Epoch = 2 })},
		{name: "a reply in local direction", rec: txReply(13, func(r *Record) { r.Dir = DirLocal })},
		{name: "an even function in unknown direction", rec: txReply(13, func(r *Record) { r.Dir = DirUnknown }), want: TxPossibleReply},
		{name: "an odd function in unknown direction", rec: txRecord(13, func(r *Record) { r.Dir = DirUnknown }),
			want: TxPossibleReply | TxPossiblePrimary},
		{name: "an unavailable function in unknown direction", rec: txRecord(13, func(r *Record) {
			r.Dir = DirUnknown
			r.FieldValidity &^= FieldValidityFunction
		}), want: TxPossibleReply | TxPossiblePrimary},
		{name: "unknown direction and another System Bytes", rec: txRecord(13, func(r *Record) {
			r.Dir = DirUnknown
			otherSB(r)
		})},
		{name: "a same-key primary", rec: txRecord(13, nil), want: TxSameKeyPrimary},
		{name: "a same-key primary without stream", rec: txRecord(13, noStream), want: TxSameKeyPrimary},
		{name: "the primary's direction, function unavailable", rec: txRecord(13, func(r *Record) { r.FieldValidity &^= FieldValidityFunction }),
			want: TxPossiblePrimary},
		{name: "the primary's direction, SessionID unavailable", rec: txRecord(13, func(r *Record) { r.FieldValidity &^= FieldValiditySessionID }),
			want: TxPossiblePrimary},
		{name: "the primary's direction, System Bytes unavailable", rec: txRecord(13, func(r *Record) {
			r.FieldValidity &^= FieldValiditySystemBytes
		}), want: TxPossiblePrimary},
		{name: "the primary's direction, an even function", rec: txRecord(13, txFrame(func(p []byte) { p[fieldFunctionOff] = 4 }))},
		{name: "the primary's direction, an even function, System Bytes unavailable", rec: txRecord(13, func(r *Record) {
			txFrame(func(p []byte) { p[fieldFunctionOff] = 4 })(r)
			r.FieldValidity &^= FieldValiditySystemBytes
		})},
		{name: "the primary's direction, another System Bytes", rec: txRecord(13, otherSB)},
		{name: "a primary in another epoch", rec: txRecord(13, func(r *Record) { r.Epoch = 2 })},
		{name: "a primary in local direction", rec: txRecord(13, func(r *Record) { r.Dir = DirLocal })},
		{name: "a Reject.req", rec: txControl(nil), want: TxOutcomeRecord},
		{name: "a Reject.req with SType unavailable", rec: txControl(func(r *Record) { r.FieldValidity &^= FieldValiditySType })},
		{name: "a Reject.req whose bytes stop before its SType", rec: txControl(func(r *Record) {
			r.Payload = r.Payload[:fieldSTypeEnd-1]
			r.SetCapturedFieldValidity()
		})},
		{name: "a Reject.req with System Bytes unavailable", rec: txControl(func(r *Record) {
			r.FieldValidity &^= FieldValiditySystemBytes
		})},
		{name: "a Reject.req of another System Bytes", rec: txControl(otherSB)},
		{name: "a Reject.req in the primary's direction", rec: txControl(func(r *Record) { r.Dir = DirHostToEquipment })},
		{name: "a Reject.req in another epoch", rec: txControl(func(r *Record) { r.Epoch = 2 })},
		{name: "a control record whose bytes 6-7 read S1F4", rec: txControl(txFrame(func(p []byte) {
			p[fieldByte6Off], p[fieldFunctionOff], p[fieldSTypeOff] = 0x01, 4, 0
		}))},
		{name: "a control record whose bytes 6-7 read S1F0", rec: txControl(txFrame(func(p []byte) {
			p[fieldByte6Off], p[fieldFunctionOff], p[fieldSTypeOff] = 0x01, 0, 0
		}))},
		{name: "a control record in unknown direction whose bytes 6-7 read S1F3", rec: txControl(func(r *Record) {
			r.Dir = DirUnknown
			txFrame(func(p []byte) { p[fieldByte6Off], p[fieldFunctionOff], p[fieldSTypeOff] = 0x81, 3, 0 })(r)
		})},
		{name: "a socket-close of the epoch", rec: event(txTestEpoch, &TransportEvent{Event: EventSocketClose}), want: TxClosing},
		{name: "a socket-close of another epoch", rec: event(2, &TransportEvent{Event: EventSocketClose})},
		{name: "a stop of the epoch", rec: boundary(txTestEpoch, new(BoundaryKindStop)), want: TxClosing},
		{name: "a stop of another epoch", rec: boundary(2, new(BoundaryKindStop)), want: TxClosing},
		{name: "a stop-unclean of the epoch", rec: boundary(txTestEpoch, new(BoundaryKindStopUnclean))},
		{name: "a gap boundary of the epoch", rec: boundary(txTestEpoch, new(BoundaryKindGap))},
		{name: "a boundary without kind", rec: boundary(txTestEpoch, nil)},
		{name: "a socket-close that does not decode", rec: func() Record {
			r := event(txTestEpoch, &TransportEvent{Event: EventSocketClose})
			r.Payload = append(slices.Clone(r.Payload), 0xFF)

			return r
		}()},
		{name: "a T3", rec: event(txTestEpoch, txT3(nil)), want: TxOutcomeRecord},
		{name: "a T3 without stream and function", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimaryStream, ev.PrimaryFunction = nil, nil
		})), want: TxOutcomeRecord},
		{name: "a T3 of another stream", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) { ev.PrimaryStream = new(uint64(2)) }))},
		{name: "a T3 of another stream, the primary's unavailable", primary: noStream,
			rec: event(txTestEpoch, txT3(func(ev *TransportEvent) { ev.PrimaryStream = new(uint64(2)) })), want: TxOutcomeRecord},
		{name: "a T3 of another function", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) { ev.PrimaryFunction = new(uint64(5)) }))},
		{name: "a T3 of another System Bytes", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimarySystemBytes[3] = 0
		}))},
		{name: "a T3 whose SessionID exceeds 16 bits", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimarySessionID = new(uint64(0x1_1234))
		}))},
		{name: "a T3 whose stream exceeds 8 bits", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimaryStream = new(uint64(0x101))
		}))},
		{name: "a T3 whose function exceeds 8 bits", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimaryFunction = new(uint64(0x103))
		}))},
		{name: "a T3 without SessionID", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) { ev.PrimarySessionID = nil }))},
		{name: "a T3 without System Bytes", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
			ev.PrimarySystemBytesPresent = false
		}))},
		{name: "a T3 of another epoch", rec: event(2, txT3(nil))},
		{name: "a T3 without System Bytes, the primary's zero",
			primary: txFrame(func(p []byte) { copy(p[fieldSystemBytesOff:fieldSystemBytesEnd], []byte{0, 0, 0, 0}) }),
			rec: event(txTestEpoch, txT3(func(ev *TransportEvent) {
				ev.PrimarySystemBytes, ev.PrimarySystemBytesPresent = [4]byte{}, false
			}))},
		{name: "a T6", rec: event(txTestEpoch, txT3(func(ev *TransportEvent) { ev.Timer = new(TimerT6) }))},
		{name: "an annotation", rec: Record{Seq: 13, TSUTCNs: blockTestHour + 13, Epoch: txTestEpoch, Kind: KindAnnotation}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary := txRecord(12, tt.primary)
			k := primaryKeyOf(&primary)
			require.True(t, k.keyed)
			got, ev := k.classify(&tt.rec)
			assert.Equal(t, tt.want, got, "%v", got)
			_, derr := UnmarshalTransportEvent(tt.rec.Payload)
			assert.Equal(t, tt.rec.Kind == KindTransportEvent && derr == nil, ev != nil, "the decoded event")
		})
	}
}

// TestTxRunsInsert inserts seqs in and out of order, at the ends of the uint64 range included:
// the runs stay sorted, disjoint and not adjacent, contains finds exactly the seqs inserted,
// and the budget holds the charge of each run left, the runs joined released.
func TestTxRunsInsert(t *testing.T) {
	t.Parallel()

	const top = ^uint64(0)
	var s txRuns
	b := txBudget{max: math.MaxInt64}
	for _, seq := range []uint64{5, 6, 6, 8, 12, 7, 3, 1, 0, 10, top, top - 2, 4, 11, top - 1} {
		require.NoError(t, s.insert(seq, &b))
	}
	assert.Equal(t, []txRun{{first: 0, last: 1}, {first: 3, last: 8}, {first: 10, last: 12}, {first: top - 2, last: top}}, s.runs)
	assert.Equal(t, 4*txRunCharge, b.used)
	for _, seq := range []uint64{0, 1, 3, 8, 10, 12, top - 2, top} {
		assert.True(t, s.contains(seq), "%d", seq)
	}
	for _, seq := range []uint64{2, 9, 13, top - 3} {
		assert.False(t, s.contains(seq), "%d", seq)
	}
	assert.False(t, (&txRuns{}).contains(0))
}

// TestFindTransactionKeepsSeqGroups reads seq groups of several versions in the primary's scope:
// a group is kept with every version, each with its own roles and marked as a conflict,
// when one version plays a role, whether it comes first, last or between;
// a group in which none does is dropped, its conflict still a gap,
// and so is a single version that plays no role.
func TestFindTransactionKeepsSeqGroups(t *testing.T) {
	t.Parallel()

	// 12 is the primary, 13 its reply, 14 a primary of other System Bytes, which plays no role.
	base := txBlock(txRecord(12, nil), txReply(13, nil), txRecord(14, txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 })))
	tests := []struct {
		name  string
		steps [][]footerTestStep
		want  []string
		// packs holds the pack of each record.
		packs     []UUID
		conflicts []uint64
	}{
		{name: "an irrelevant version first", steps: [][]footerTestStep{changed(base, 1, 13), base},
			want:  []string{"12@0 primary", "13@0 none!", "13@0 candidate!"},
			packs: []UUID{seg0, seg0, seg1}, conflicts: []uint64{13}},
		{name: "an irrelevant version last", steps: [][]footerTestStep{base, changed(base, 1, 13)},
			want:  []string{"12@0 primary", "13@0 candidate!", "13@0 none!"},
			packs: []UUID{seg0, seg0, seg1}, conflicts: []uint64{13}},
		{name: "three versions", steps: [][]footerTestStep{changed(base, 1, 13), base, changed(base, 2, 13)},
			want:  []string{"12@0 primary", "13@0 none!", "13@0 candidate!", "13@0 none!"},
			packs: []UUID{seg0, seg0, seg1, seg2}, conflicts: []uint64{13}},
		{name: "no version qualifies", steps: [][]footerTestStep{base, changed(base, 1, 14)},
			want:  []string{"12@0 primary", "13@0 candidate"},
			packs: []UUID{seg0, seg0}, conflicts: []uint64{14}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var files [][]byte
			for i, steps := range tt.steps {
				files = append(files, txPack(t, []UUID{seg0, seg1, seg2}[i], nil, steps))
			}
			res, err := findTx(t, t.Context(), txSource(t, true, files...), txKeyAt(12), TxOptions{MaxScopes: 1})
			require.ErrorIs(t, err, errTxNotImplemented)
			assert.Equal(t, tt.want, recordSeqs(res.Records))
			packs := make([]UUID, len(res.Records))
			for i := range res.Records {
				packs[i] = res.Records[i].Pack
			}
			assert.Equal(t, tt.packs, packs)
			assert.Equal(t, tt.conflicts, conflictSeqs(res.Conflicts))
			require.Equal(t, []TxGapReason{TxGapConflict}, gapReasons(res.Gaps))
			assert.Equal(t, TxGap{
				Reason: TxGapConflict, Hours: []int64{memTestHour}, Block: -1, Offset: -1, Seq: new(tt.conflicts[0]), Err: res.Gaps[0].Err,
			}, res.Gaps[0])
		})
	}
}

// TestFindTransactionReadsEveryScope reads three scopes, the last empty:
// each is read once and searched, and each record above the primary that plays a role is kept once,
// those of the primary's scope included, which its one read classified.
func TestFindTransactionReadsEveryScope(t *testing.T) {
	t.Parallel()

	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, txBlock(txRecord(11, nil), txRecord(12, nil), txReply(14, nil)))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(20, nil), txRecord(21, func(r *Record) { r.Dir = DirUnknown })))},
		nil)
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3})
	require.ErrorIs(t, err, errTxNotImplemented)
	observes, scopes, closes := s.counts()
	assert.Equal(t, [2]int{1, 1}, [2]int{observes, closes})
	assert.Equal(t, map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1}, scopes)
	assert.Equal(t, []TxScope{
		{Hour: memTestHour, Indexed: true, Packs: []UUID{seg0}},
		{Hour: memTestHour + 1, Indexed: true, Packs: []UUID{seg1}},
		{Hour: memTestHour + 2, Indexed: true},
	}, res.Searched)
	assert.Equal(t, []string{"12@0 primary", "14@0 candidate", "20@1 candidate", "21@1 possible-reply|possible-primary"},
		recordSeqs(res.Records))
	assert.Empty(t, res.Gaps)
}

// TestFindTransactionCrossScopeConflicts reads seqs that the reads of several scopes yield:
// each is a conflict across scope reads, one TxGapConflict gap naming the seq and the hours of every read that yielded it,
// reserved once however many reads yield it, with every kept version marked as a conflict.
// A version kept nowhere else is kept where it qualifies, and one that plays no role is not kept.
// Runs of adjacent seqs from different hours stay apart.
func TestFindTransactionCrossScopeConflicts(t *testing.T) {
	t.Parallel()

	irrelevant := txFrame(func(p []byte) { p[fieldSystemBytesOff] = 0 })
	primary := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, irrelevant), txReply(14, nil)))
	tests := []struct {
		name      string
		hours     [][][]byte
		want      []string
		gaps      []TxGap
		runs      []txRuns
		conflicts int
	}{
		{name: "adjacent runs", hours: [][][]byte{
			{primary},
			{txPackIn(t, seg1, 1, nil, txBlock(txRecord(15, irrelevant), txRecord(16, irrelevant)))},
		}, want: []string{"12@0 primary", "14@0 candidate"}, runs: []txRuns{
			{hour: memTestHour, runs: []txRun{{first: 12, last: 14}}},
			{hour: memTestHour + 1, runs: []txRun{{first: 15, last: 16}}},
		}},
		{name: "a version irrelevant where first read", hours: [][][]byte{
			{primary},
			{txPackIn(t, seg1, 1, nil, txBlock(txReply(13, nil), txRecord(16, irrelevant)))},
		}, want: []string{"12@0 primary", "14@0 candidate", "13@1 candidate!"}, gaps: []TxGap{
			{Reason: TxGapConflict, Hours: []int64{memTestHour, memTestHour + 1}, Block: -1, Offset: -1, Seq: new(uint64(13))},
		}, runs: []txRuns{
			{hour: memTestHour, runs: []txRun{{first: 12, last: 14}}},
			{hour: memTestHour + 1, runs: []txRun{{first: 13, last: 13}, {first: 16, last: 16}}},
		}, conflicts: 1},
		{name: "three scopes", hours: [][][]byte{
			{primary},
			{txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil)))},
			{txPackIn(t, seg2, 2, nil, txBlock(txReply(14, nil)))},
		}, want: []string{"12@0 primary", "14@0 candidate!", "14@1 candidate!", "14@2 candidate!"}, gaps: []TxGap{
			{Reason: TxGapConflict, Hours: []int64{memTestHour, memTestHour + 1, memTestHour + 2}, Block: -1, Offset: -1, Seq: new(uint64(14))},
		}, conflicts: 1},
		{name: "two scopes apart", hours: [][][]byte{
			{primary},
			{txPackIn(t, seg1, 1, nil, txBlock(txRecord(20, irrelevant)))},
			{txPackIn(t, seg2, 2, nil, txBlock(txReply(14, irrelevant)))},
		}, want: []string{"12@0 primary", "14@0 candidate!"}, gaps: []TxGap{
			{Reason: TxGapConflict, Hours: []int64{memTestHour, memTestHour + 2}, Block: -1, Offset: -1, Seq: new(uint64(14))},
		}, conflicts: 1},
		{name: "within a scope and across scopes", hours: [][][]byte{
			{primary},
			{
				txPackIn(t, seg1, 1, nil, txBlock(txReply(14, nil))),
				txPackIn(t, seg2, 1, nil, changed(txBlock(txReply(14, nil)), 1, 14)),
			},
		}, want: []string{"12@0 primary", "14@0 candidate!", "14@1 candidate!", "14@1 none!"}, gaps: []TxGap{
			{Reason: TxGapConflict, Hours: []int64{memTestHour + 1}, Block: -1, Offset: -1, Seq: new(uint64(14))},
			{Reason: TxGapConflict, Hours: []int64{memTestHour, memTestHour + 1}, Block: -1, Offset: -1, Seq: new(uint64(14))},
		}, conflicts: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.hours...)
			l, err := runTxLookup(t, s, txKeyAt(12), TxOptions{MaxScopes: len(tt.hours)})
			require.ErrorIs(t, err, errTxNotImplemented)
			assert.Equal(t, tt.want, recordSeqs(l.res.Records))
			for i := range l.res.Gaps {
				require.Error(t, l.res.Gaps[i].Err)
				l.res.Gaps[i].Err = nil
			}
			assert.Equal(t, tt.gaps, nilIfEmpty(l.res.Gaps))
			assert.Equal(t, tt.conflicts, l.conflicts)
			if tt.runs != nil {
				runs := make([]txRuns, len(l.reads))
				for i, rd := range l.reads {
					runs[i] = *rd.runs
				}
				assert.Equal(t, tt.runs, runs)
			}
			_, scopes, _ := s.counts()
			for h := range tt.hours {
				assert.Equal(t, 1, scopes[memTestHour+int64(h)])
			}
		})
	}
}

// TestFindTransactionLaterPrimaryInvalidation reads another version of the primary in a later scope:
// a TxGapConflict gap at the primary naming both hours and a TxGapNoKey gap,
// the key derived from the first version left in place, every later scope still read,
// and every version of the primary kept and marked as a conflict.
// A third scope that yields the primary again adds its hour, and no other gap;
// two versions in the later scope are also a conflict within that read, one more gap and one more conflict counted.
func TestFindTransactionLaterPrimaryInvalidation(t *testing.T) {
	t.Parallel()

	first := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))
	later := txPackIn(t, seg1, 1, nil, txBlock(txRecord(12, nil), txReply(14, nil)))
	tests := []struct {
		name  string
		third []byte
		want  []string
		hours []int64
	}{
		{name: "two scopes", third: txPackIn(t, seg2, 2, nil, txBlock(txReply(15, nil))),
			want:  []string{"12@0 primary!", "13@0 candidate", "12@1 primary!", "14@1 candidate", "15@2 candidate"},
			hours: []int64{memTestHour, memTestHour + 1}},
		{name: "three scopes", third: txPackIn(t, seg2, 2, nil, txBlock(txRecord(12, nil), txReply(15, nil))),
			want:  []string{"12@0 primary!", "13@0 candidate", "12@1 primary!", "14@1 candidate", "12@2 primary!", "15@2 candidate"},
			hours: []int64{memTestHour, memTestHour + 1, memTestHour + 2}},
	}
	t.Run("two versions in the later scope", func(t *testing.T) {
		t.Parallel()

		other := txPackIn(t, seg3, 1, nil, changed(txBlock(txRecord(12, nil)), 1, 12))
		l, err := runTxLookup(t, txHours(t, [][]byte{first}, [][]byte{later, other}), txKeyAt(12), TxOptions{MaxScopes: 2})
		require.ErrorIs(t, err, errTxNotImplemented)
		require.Equal(t, []TxGapReason{TxGapConflict, TxGapConflict, TxGapNoKey}, gapReasons(l.res.Gaps))
		assert.Equal(t, []int64{memTestHour + 1}, l.res.Gaps[0].Hours, "the conflict within the later read")
		assert.Equal(t, []int64{memTestHour, memTestHour + 1}, l.res.Gaps[1].Hours, "the conflict across reads")
		assert.Equal(t, 2, l.conflicts)
		assert.Equal(t, []string{"12@0 primary!", "13@0 candidate", "12@1 primary!", "12@1 primary!", "14@1 candidate"},
			recordSeqs(l.res.Records))
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, [][]byte{first}, [][]byte{later}, [][]byte{tt.third})
			l, err := runTxLookup(t, s, txKeyAt(12), TxOptions{MaxScopes: 3})
			require.ErrorIs(t, err, errTxNotImplemented)
			_, scopes, _ := s.counts()
			assert.Equal(t, map[int64]int{memTestHour: 1, memTestHour + 1: 1, memTestHour + 2: 1}, scopes)
			assert.Len(t, l.res.Searched, 3)
			assert.Equal(t, tt.want, recordSeqs(l.res.Records))
			require.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(l.res.Gaps))
			assert.Equal(t, tt.hours, l.res.Gaps[0].Hours)
			assert.Equal(t, new(uint64(12)), l.res.Gaps[0].Seq)
			assert.Equal(t, []int64{memTestHour}, l.res.Gaps[1].Hours)
			assert.Equal(t, new(uint64(12)), l.res.Gaps[1].Seq)
			assert.Equal(t, 1, l.conflicts)
			assert.Equal(t, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, l.res.SystemBytes, "the key of the first version stays")
			assert.Equal(t, DirHostToEquipment, l.res.Dir)
		})
	}
}

// TestFindTransactionConflictBelowPrimary reads a conflict below a keyed primary:
// MergeIterate lists it and it counts against MaxConflicts, but it is no gap of the lookup.
func TestFindTransactionConflictBelowPrimary(t *testing.T) {
	t.Parallel()

	steps := txBlock(txSeqs(10, 12)...)
	s := txSource(t, true, txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 10)))
	l, err := runTxLookup(t, s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.ErrorIs(t, err, errTxNotImplemented)
	assert.Equal(t, []uint64{10}, conflictSeqs(l.res.Conflicts))
	assert.Equal(t, 1, l.conflicts)
	assert.Empty(t, l.res.Gaps)
	assert.Equal(t, []string{"12@0 primary"}, recordSeqs(l.res.Records))
}

// TestFindTransactionCrossScopeConflictBeforeError finds a conflict across scope reads in the second scope,
// then fails: at the third scope's Scope call, or within the second read at a conflict past MaxConflicts.
// The result returned with the error still lists the conflict across reads with both hours,
// and every kept version of its seq is marked as a conflict.
func TestFindTransactionCrossScopeConflictBeforeError(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	first := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txReply(13, nil)))
	tests := []struct {
		name   string
		second [][]byte
		max    int
		setup  func(s *memSource)
		err    error
	}{
		{name: "the next Scope call fails", second: [][]byte{txPackIn(t, seg1, 1, nil, txBlock(txReply(13, nil)))},
			setup: func(s *memSource) {
				s.beforeScope = func(_ context.Context, hour int64) error {
					if hour == memTestHour+2 {
						return boom
					}

					return nil
				}
			}, err: boom},
		{name: "the read fails after it", second: [][]byte{
			txPackIn(t, seg1, 1, nil, slices.Concat(txBlock(txReply(13, nil)), txBlock(txSeqs(20)...))),
			txPackIn(t, seg2, 1, nil, changed(txBlock(txSeqs(20)...), 1, 20)),
		}, max: 1, err: ErrReadLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, [][]byte{first}, tt.second, nil)
			if tt.setup != nil {
				tt.setup(s)
			}
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 3, MaxConflicts: tt.max})
			require.ErrorIs(t, err, tt.err)
			require.Equal(t, []TxGapReason{TxGapConflict}, gapReasons(res.Gaps))
			assert.Equal(t, new(uint64(13)), res.Gaps[0].Seq)
			assert.Equal(t, []int64{memTestHour, memTestHour + 1}, res.Gaps[0].Hours)
			assert.Equal(t, []string{"12@0 primary", "13@0 candidate!", "13@1 candidate!"}, recordSeqs(res.Records))
		})
	}
}

// TestFindTransactionConflictBudget counts the conflicts of a lookup against MaxConflicts:
// each conflict a scope read lists and each seq that first becomes a conflict across scope reads is one;
// the allowance used exactly lets a later conflict-free scope be read,
// and one more, within a scope or across scopes, in either order within one scope, fails with ErrReadLimit
// at the discovery that passes it.
func TestFindTransactionConflictBudget(t *testing.T) {
	t.Parallel()

	in := func(id UUID, hours int64, steps ...[]footerTestStep) []byte {
		return txPackIn(t, id, hours, nil, slices.Concat(steps...))
	}
	other := UUID{0x60}
	// The primary's scope with a conflict at 13 and a record at 14.
	local := [][]byte{
		in(seg0, 0, txBlock(txSeqs(12, 13, 14)...)),
		in(seg1, 0, changed(txBlock(txSeqs(12, 13, 14)...), 1, 13)),
	}
	tests := []struct {
		name  string
		hours [][][]byte
		max   int
		// err is part of the error's text; empty for a lookup that reads every scope.
		err      string
		searched int
	}{
		{name: "the allowance used exactly", max: 2, hours: [][][]byte{
			local,
			{in(seg2, 1, txBlock(txSeqs(14)...))},
			{in(seg3, 2, txBlock(txSeqs(20)...))},
		}, searched: 3},
		{name: "one more within a scope", max: 1, hours: [][][]byte{
			local,
			{in(seg2, 1, txBlock(txSeqs(20)...)), in(seg3, 1, changed(txBlock(txSeqs(20)...), 1, 20))},
		}, err: "merge iterate: the conflict at seq 20", searched: 1},
		{name: "one more across scopes", max: 1, hours: [][][]byte{
			local,
			{in(seg2, 1, txBlock(txSeqs(14)...))},
		}, err: "the conflict at seq 14 between hours", searched: 1},
		{name: "across scopes, then within the scope", max: 1, hours: [][][]byte{
			{in(seg0, 0, txBlock(txSeqs(12, 14)...))},
			{in(seg2, 1, txBlock(txSeqs(14)...), txBlock(txSeqs(20)...)), in(seg3, 1, changed(txBlock(txSeqs(20)...), 1, 20))},
		}, err: "merge iterate: the conflict at seq 20", searched: 1},
		{name: "within the scope, then across scopes", max: 1, hours: [][][]byte{
			{in(seg0, 0, txBlock(txSeqs(12, 20)...))},
			{in(seg2, 1, txBlock(txSeqs(14)...), txBlock(txSeqs(20)...)), in(other, 1, changed(txBlock(txSeqs(14)...), 1, 14))},
		}, err: "the conflict at seq 20 between hours", searched: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txHours(t, tt.hours...)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: len(tt.hours), MaxConflicts: tt.max})
			assert.Len(t, res.Searched, tt.searched)
			if tt.err == "" {
				require.ErrorIs(t, err, errTxNotImplemented)
				return
			}
			require.ErrorIs(t, err, ErrReadLimit)
			require.ErrorContains(t, err, tt.err)
			assert.Equal(t, TxOutcome(0), res.Outcome)
		})
	}

	t.Run("a conflict at the primary that the read listed", func(t *testing.T) {
		t.Parallel()

		// The read lists the conflict at 12 and reserves the one conflict allowed; settling the primary reserves no other.
		steps := txBlock(txSeqs(11, 12, 13)...)
		s := txSource(t, true, txPack(t, seg0, nil, steps), txPack(t, seg1, nil, changed(steps, 1, 12)))
		res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2, MaxConflicts: 1})
		require.NoError(t, err)
		assert.Equal(t, TxIncomplete, res.Outcome)
		assert.Equal(t, []TxGapReason{TxGapConflict, TxGapNoKey}, gapReasons(res.Gaps))
		assert.Equal(t, []uint64{12}, conflictSeqs(res.Conflicts))
	})

	t.Run("split copies of the primary", func(t *testing.T) {
		t.Parallel()

		// The read yields the copy of 15 in seg0's misindexed block and seg1's other copy uncompared,
		// after the conflict at 20 between seg1 and seg2 used the allowance.
		s := txSource(t, true, txMisindexed(t, 15),
			txPack(t, seg1, nil, changed(txBlock(txSeqs(15, 20)...), 1, 15)),
			txPack(t, seg2, nil, changed(txBlock(txSeqs(20)...), 2, 20)))
		res, err := findTx(t, t.Context(), s, txKeyAt(15), TxOptions{MaxScopes: 2, MaxConflicts: 1})
		require.ErrorIs(t, err, ErrReadLimit)
		require.ErrorContains(t, err, "hour 497222: the conflict at seq 15")
		assert.Equal(t, []uint64{20}, conflictSeqs(res.Conflicts))
		assert.Len(t, res.Searched, 1, "the scope was read to its end")
		_, scopes, _ := s.counts()
		assert.Equal(t, map[int64]int{memTestHour: 1}, scopes)
	})
}

// TestFindTransactionGapOrder reads a local conflict in the primary's scope and, in the next,
// a conflict across scopes and a local one, each scope with an unevaluated pack:
// each read's conflict gaps follow its other gaps, in the order the read found them.
func TestFindTransactionGapOrder(t *testing.T) {
	t.Parallel()

	unevaluated := func(m *PackMeta) { m.QualityEvaluated = false }
	steps := txBlock(txSeqs(12, 13)...)
	s := txHours(t,
		[][]byte{txPackIn(t, seg0, 0, unevaluated, steps), txPackIn(t, seg1, 0, nil, changed(steps, 1, 13))},
		[][]byte{
			txPackIn(t, seg2, 1, unevaluated, txBlock(txSeqs(13, 20)...)),
			txPackIn(t, seg3, 1, nil, changed(txBlock(txSeqs(20)...), 1, 20)),
		})
	res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxScopes: 2})
	require.ErrorIs(t, err, errTxNotImplemented)
	assert.Equal(t, []TxGapReason{TxGapUnevaluated, TxGapConflict, TxGapUnevaluated, TxGapConflict, TxGapConflict}, gapReasons(res.Gaps))
	var seqs []uint64
	var hours [][]int64
	for _, g := range res.Gaps {
		if g.Reason == TxGapConflict {
			seqs, hours = append(seqs, *g.Seq), append(hours, g.Hours)
		}
	}
	assert.Equal(t, []uint64{13, 13, 20}, seqs)
	assert.Equal(t, [][]int64{{memTestHour}, {memTestHour, memTestHour + 1}, {memTestHour + 1}}, hours)
	assert.Equal(t, []string{"12@0 primary", "13@0 same-key-primary!", "13@0 none!", "13@1 same-key-primary!",
		"20@1 same-key-primary!", "20@1 none!"}, recordSeqs(res.Records))
}

// TestFindTransactionOrderingUncertainEpochs reads records with ordering-uncertain below the primary, at it, above it,
// and below it in a later scope: each epoch is noted once, with where its first such record was read.
func TestFindTransactionOrderingUncertainEpochs(t *testing.T) {
	t.Parallel()

	uncertain := func(epoch uint32) func(r *Record) {
		return func(r *Record) {
			r.Epoch = epoch
			r.Quality |= QualityOrderingUncertain
		}
	}
	s := txHours(t,
		[][]byte{txPack(t, seg0, nil, txBlock(txRecord(10, uncertain(1)), txRecord(11, uncertain(2)), txRecord(12, nil), txRecord(13, uncertain(2))))},
		[][]byte{txPackIn(t, seg1, 1, nil, txBlock(txRecord(5, uncertain(3)), txRecord(20, uncertain(3))))})
	l, err := runTxLookup(t, s, txKeyAt(12), TxOptions{MaxScopes: 2})
	require.ErrorIs(t, err, errTxNotImplemented)
	assert.Equal(t, map[uint32]txSeen{
		1: {hour: memTestHour, seq: 10, pack: seg0},
		2: {hour: memTestHour, seq: 11, pack: seg0},
		3: {hour: memTestHour + 1, seq: 5, pack: seg1},
	}, l.uncertain)
}

// TestFindTransactionNotesBoundaryRecords reads capture-boundary records:
// those of the primary's epoch above it are noted with their kind, time and gap bounds, whatever their kind,
// a stop also kept as a closing record;
// one below the primary or of another epoch is not noted, and a stop of another epoch is kept as a closing record.
func TestFindTransactionNotesBoundaryRecords(t *testing.T) {
	t.Parallel()

	at := func(v int64) *int64 { return &v }
	boundary := func(seq uint64, epoch uint32, kind BoundaryKind, from, to *int64) Record {
		return testEventRecord(t, seq, blockTestHour+int64(seq), epoch,
			&TransportEvent{Event: EventCaptureBoundary, BoundaryKind: new(kind), GapStart: from, GapEnd: to})
	}
	s := txHours(t, [][]byte{txPack(t, seg0, nil, txBlock(
		boundary(11, txTestEpoch, BoundaryKindGap, nil, nil),
		txRecord(12, nil),
		boundary(13, txTestEpoch, BoundaryKindGap, at(5), at(9)),
		boundary(14, 2, BoundaryKindStop, nil, nil),
		boundary(15, txTestEpoch, BoundaryKindStart, nil, nil),
		boundary(16, txTestEpoch, BoundaryKindStop, nil, nil),
	))})
	l, err := runTxLookup(t, s, txKeyAt(12), TxOptions{MaxScopes: 1})
	require.ErrorIs(t, err, errTxNotImplemented)
	assert.Equal(t, []txBoundaryRecord{
		{at: txSeen{hour: memTestHour, seq: 13, pack: seg0}, boundary: Boundary{
			Capture: captureLow, Seq: 13, Kind: BoundaryKindGap, TS: blockTestHour + 13, Epoch: txTestEpoch, GapStart: at(5), GapEnd: at(9),
		}},
		{at: txSeen{hour: memTestHour, seq: 15, pack: seg0}, boundary: Boundary{
			Capture: captureLow, Seq: 15, Kind: BoundaryKindStart, TS: blockTestHour + 15, Epoch: txTestEpoch,
		}},
		{at: txSeen{hour: memTestHour, seq: 16, pack: seg0}, boundary: Boundary{
			Capture: captureLow, Seq: 16, Kind: BoundaryKindStop, TS: blockTestHour + 16, Epoch: txTestEpoch,
		}},
	}, l.boundaries)
	assert.Equal(t, []string{"12@0 primary", "14@0 closing", "16@0 closing"}, recordSeqs(l.res.Records))
}
