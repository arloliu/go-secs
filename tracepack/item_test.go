package tracepack

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// itemTestRead is one way of reading the records of the readers given:
// Reader.Iterate of the first, or MergeIterate in an order.
type itemTestRead struct {
	name string
	read func(t *testing.T, readers []*Reader, q Query, fn func(*Item)) Result
}

// itemTestReads returns Reader.Iterate and MergeIterate in both orders.
func itemTestReads() []itemTestRead {
	merge := func(order Order) func(t *testing.T, readers []*Reader, q Query, fn func(*Item)) Result {
		return func(t *testing.T, readers []*Reader, q Query, fn func(*Item)) Result {
			t.Helper()
			res, err := MergeIterate(t.Context(), readers, q, MergeIterateOptions{Order: order}, func(it *Item) error {
				fn(it)
				return nil
			})
			require.NoError(t, err)

			return res
		}
	}

	return []itemTestRead{
		{name: "iterate", read: func(t *testing.T, readers []*Reader, q Query, fn func(*Item)) Result {
			t.Helper()
			res, err := readers[0].Iterate(t.Context(), q, func(it *Item) error {
				fn(it)
				return nil
			})
			require.NoError(t, err)

			return res
		}},
		{name: "merge capture order", read: merge(OrderCapture)},
		{name: "merge time order", read: merge(OrderTime)},
	}
}

// itemTestPack returns a pack of format minor 1, whose thinning declares S1F3 and S1F4, over two blocks:
// an S1F3 data record at 1, an unobserved S1F4 at 2, an S6F11 at 3, a control record at 4 and a transport event at 5.
func itemTestPack(t *testing.T) []byte {
	t.Helper()

	control := frameRecord(4, hsmsFrame(0, 0, nil))
	control.Kind, control.DecodeStatus = KindControl, DecodeStatusNotApplicable
	control.Payload[fieldSTypeOff] = 5
	control.SetCapturedFieldValidity()
	steps := []footerTestStep{
		{rec: frameRecord(1, hsmsFrame(1, 3, nil))},
		{rec: unobservedRecord(2), flush: true},
		{rec: frameRecord(3, hsmsFrame(6, 11, []byte{0x01, 0x00}))},
		{rec: control},
		{rec: testEventRecord(t, 5, blockTestHour+5, 1, &TransportEvent{Event: EventSocketClose}), flush: true},
	}
	file := writeRepairPack(t, CodecZstd, func(m *PackMeta) { m.Thinning = thinnedMeta().Thinning }, false, steps).file
	require.GreaterOrEqual(t, mustOpen(t, file, ReaderOptions{}).Header().FormatMinor, uint16(1))

	return file
}

// Item.HSMSHeader is Record.HSMSHeader of the record as stored, whether or not the read carries payloads,
// for Reader.Iterate and MergeIterate, with or without a filter on an HSMS header field.
func TestItemHSMSHeader(t *testing.T) {
	t.Parallel()

	readers := []*Reader{mustOpen(t, itemTestPack(t), ReaderOptions{})}
	want := map[uint64]HSMSHeader{}
	_, err := readers[0].Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
		want[it.Record.Seq] = it.Record.HSMSHeader()
		return nil
	})
	require.NoError(t, err)
	require.Len(t, want, 5)
	assert.Equal(t, SF{Stream: 6, Function: 11}, SF{Stream: want[3].Stream, Function: want[3].Function})
	assert.Equal(t, uint8(5), want[4].SType, "a control record's header")
	assert.Equal(t, HSMSHeader{}, want[5], "a transport event has no HSMS header")
	for seq := range uint64(4) {
		assert.Equal(t, capturedFields(14), want[seq+1].Available, "seq %d: every field available", seq+1)
	}

	filters := []struct {
		name string
		f    Filter
		seqs []uint64
	}{
		{name: "no filter", seqs: []uint64{1, 2, 3, 4, 5}},
		{name: "a stream and function", f: Filter{SF: []SF{{Stream: 6, Function: 11}}}, seqs: []uint64{3}},
		{name: "a session id", f: Filter{SessionIDs: []uint16{1}}, seqs: []uint64{1, 2, 3, 4}},
	}
	for _, rd := range itemTestReads() {
		for _, fc := range filters {
			for _, payloads := range []bool{false, true} {
				name := fmt.Sprintf("%s, %s, payloads %v", rd.name, fc.name, payloads)
				var seqs []uint64
				rd.read(t, readers, Query{Filter: fc.f, Payloads: payloads}, func(it *Item) {
					seqs = append(seqs, it.Record.Seq)
					assert.Equal(t, payloads, it.Record.Payload != nil, "%s: seq %d", name, it.Record.Seq)
					assert.Equal(t, want[it.Record.Seq], it.HSMSHeader, "%s: seq %d", name, it.Record.Seq)
				})
				assert.Equal(t, fc.seqs, seqs, name)
			}
		}
	}
}

// bit7Pack returns file with quality bit 7 set on the record at index k of its first block, its footer rebuilt.
func bit7Pack(t *testing.T, file []byte, k int) []byte {
	t.Helper()

	return reindexed(t, editBlock(t, file, 0, func(b *testBlock) { b.headers[k].Quality |= uint16(QualityUnobserved) }))
}

// Item.Unobserved is Reader.Unobserved of the record in the pack of the Item, the representative's for MergeIterate:
// set for a thinned record of a minor-1 pack, never for bit 7 of a minor-0 pack,
// and following the representative across identical copies in packs of both minors, whichever is given first,
// while each version of a conflict takes its own pack's.
func TestItemUnobserved(t *testing.T) {
	t.Parallel()

	collect := func(t *testing.T, rd itemTestRead, readers []*Reader, q Query) []iterItem {
		t.Helper()
		var got []iterItem
		rd.read(t, readers, q, func(it *Item) { got = append(got, copyItem(it)) })
		return got
	}

	t.Run("a thinned record of a minor-1 pack", func(t *testing.T) {
		t.Parallel()

		readers := []*Reader{mustOpen(t, itemTestPack(t), ReaderOptions{})}
		for _, rd := range itemTestReads() {
			for _, payloads := range []bool{false, true} {
				for _, it := range collect(t, rd, readers, Query{Payloads: payloads}) {
					assert.Equal(t, it.rec.Seq == 2, it.unobserved, "%s, payloads %v: seq %d", rd.name, payloads, it.rec.Seq)
				}
			}
		}
	})

	// Seqs 10 to 13, two to a block; bit 7 set on seq 10, in a pack of minor 0 and in a copy of minor 1.
	minor0 := bit7Pack(t, mergePack(t, seg0, nil, hourSteps(10, 13, 2)), 0)
	minor1 := withMinor(t, withIDs(t, minor0, seg1, viewCapture), 1)

	t.Run("bit 7 of a minor-0 pack", func(t *testing.T) {
		t.Parallel()

		readers := []*Reader{mustOpen(t, minor0, ReaderOptions{})}
		for _, rd := range itemTestReads() {
			got := collect(t, rd, readers, Query{})
			require.Len(t, got, 4, rd.name)
			assert.True(t, got[0].rec.Quality.HasAny(QualityUnobserved), "%s: the stored bit is kept", rd.name)
			for _, it := range got {
				assert.False(t, it.unobserved, "%s: seq %d", rd.name, it.rec.Seq)
			}
		}
	})

	t.Run("identical copies of both minors", func(t *testing.T) {
		t.Parallel()

		for _, first := range []int{0, 1} {
			files := [][]byte{minor0, minor1}
			if first == 1 {
				files = [][]byte{minor1, minor0}
			}
			readers := []*Reader{mustOpen(t, files[0], ReaderOptions{}), mustOpen(t, files[1], ReaderOptions{})}
			for _, rd := range itemTestReads()[1:] {
				got := collect(t, rd, readers, Query{})
				require.Len(t, got, 4, "%s, minor %d first", rd.name, first)
				for _, it := range got {
					assert.Equal(t, 0, it.pack, "%s, minor %d first: the representative is the first reader's copy", rd.name, first)
					assert.False(t, it.conflict)
					assert.Equal(t, first == 1 && it.rec.Seq == 10, it.unobserved, "%s, minor %d first: seq %d", rd.name, first, it.rec.Seq)
				}
			}
		}
	})

	t.Run("conflicting versions of both minors", func(t *testing.T) {
		t.Parallel()

		// The minor-1 pack's seq 10 differs in its payload: two versions, each its own pack's.
		other := withMinor(t, bit7Pack(t, mergePack(t, seg1, nil, changed(hourSteps(10, 13, 2), 1, 10)), 0), 1)
		for _, files := range [][][]byte{{minor0, other}, {other, minor0}} {
			readers := []*Reader{mustOpen(t, files[0], ReaderOptions{}), mustOpen(t, files[1], ReaderOptions{})}
			for _, rd := range itemTestReads()[1:] {
				var versions int
				for _, it := range collect(t, rd, readers, Query{}) {
					if it.rec.Seq != 10 {
						assert.False(t, it.unobserved)
						continue
					}
					versions++
					assert.True(t, it.conflict)
					assert.Equal(t, readers[it.pack].Header().FormatMinor == 1, it.unobserved, "%s: the version of pack %d", rd.name, it.pack)
				}
				assert.Equal(t, 2, versions, rd.name)
			}
		}
	})
}

// txUnobservedPacks returns a pack seg0 of minor 0 holding an S1F3 primary at 12 and its S1F4 reply at 13, carrying bit 7,
// and a copy seg1 of minor 1, in which the reply is unobserved; with conflict the copy's reply differs in its payload.
func txUnobservedPacks(t *testing.T, conflict bool) (minor0, minor1 []byte) {
	t.Helper()

	steps := txBlock(txRecord(12, nil), txReply(13, nil))
	minor0 = bit7Pack(t, txPack(t, seg0, nil, steps), 1)
	if conflict {
		steps = changed(steps, 1, 13)
	}
	minor1 = withMinor(t, bit7Pack(t, txPack(t, seg1, nil, steps), 1), 1)

	return minor0, minor1
}

// txBatchRecords returns the records of the pairing of key that FindTransactions passes over key's hour of src.
func txBatchRecords(t *testing.T, src PackSource, key TxKey) []TxRecord {
	t.Helper()

	from := time.Unix(0, key.Hour*hourNs)
	var got []TxRecord
	_, err := FindTransactions(t.Context(), src, TxRange{CaptureID: key.CaptureID, From: from, To: from.Add(time.Hour)}, TxRangeOptions{},
		func(p *TxPairing) error {
			if p.Key == key {
				got = cloneTxResult(&p.Result).Records
			}
			return nil
		})
	require.NoError(t, err)
	require.NotNil(t, got, "the batch pairs the key")

	return got
}

// TxRecord.Unobserved is Item.Unobserved of the version's representative, in a lookup alone and in a batch:
// the reply of a minor-1 pack is unobserved, its copy in a minor-0 pack not,
// identical copies follow the pack given first, and each version of a conflict takes its own pack's.
func TestTxRecordUnobserved(t *testing.T) {
	t.Parallel()

	minor0, minor1 := txUnobservedPacks(t, false)
	_, conflicting := txUnobservedPacks(t, true)
	tests := []struct {
		name  string
		files [][]byte
		// want is the Unobserved of each record, by its pack.
		want map[UUID][]bool
	}{
		{name: "minor 0", files: [][]byte{minor0}, want: map[UUID][]bool{seg0: {false, false}}},
		{name: "minor 1", files: [][]byte{minor1}, want: map[UUID][]bool{seg1: {false, true}}},
		{name: "minor 0 first", files: [][]byte{minor0, minor1}, want: map[UUID][]bool{seg0: {false, false}}},
		{name: "minor 1 first", files: [][]byte{minor1, minor0}, want: map[UUID][]bool{seg1: {false, true}}},
		{name: "conflicting replies", files: [][]byte{minor0, conflicting}, want: map[UUID][]bool{seg0: {false, false}, seg1: {true}}},
		{name: "conflicting replies, minor 1 first", files: [][]byte{conflicting, minor0}, want: map[UUID][]bool{seg1: {false, true}, seg0: {false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := txSource(t, tt.files...)
			res, err := findTx(t, t.Context(), s, txKeyAt(12), TxOptions{MaxHours: 1})
			require.NoError(t, err)
			for _, records := range [][]TxRecord{res.Records, txBatchRecords(t, s.view(), txKeyAt(12))} {
				got := map[UUID][]bool{}
				for _, r := range records {
					got[r.Pack] = append(got[r.Pack], r.Unobserved)
				}
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// Versions compared as stored are the same whatever their derived fields: the pack, block, hour, class and Unobserved.
func TestSameStoredRecordIgnoresDerivedFields(t *testing.T) {
	t.Parallel()

	a := TxRecord{Record: txRecord(12, nil), HeaderExtra: seqExtra(12), Pack: seg0, Class: TxPrimary}
	b := a
	b.Record = a.Record.Clone()
	b.Pack, b.Block, b.Hour, b.Class, b.Unobserved, b.Conflict = seg1, 3, 1, TxCandidate, true, true
	assert.True(t, sameStoredRecord(&a, &b, 1, 1))
	b.Record.Payload[len(b.Record.Payload)-1]++
	assert.False(t, sameStoredRecord(&a, &b, 1, 1), "a payload byte differs")
}

// cloneTestItem returns an Item whose every slice and pointer is set, built afresh on every call.
func cloneTestItem(err error) Item {
	path := "/1/2"

	return Item{
		Record:      Record{Seq: 7, Kind: KindData, Payload: []byte{1, 2, 3}},
		HeaderExtra: []byte{4, 5},
		Input:       1, Block: 2, Conflict: true, Unobserved: true,
		HSMSHeader: HSMSHeader{SessionID: 1, Stream: 6, Function: 11, Available: FieldValiditySessionID},
		Redaction: &RecordRedaction{
			Entries: []RedactionEntry{{
				Seq: 7, ItemPath: &path, MaskedRanges: []MaskedRange{{Offset: 14, Length: 2}}, RawMaskedRanges: []byte{6},
				Domain: "d", Digest: []byte{7, 8}, SIV: []byte{9}, HasSIV: true,
			}},
			WhollyMasked: true,
			Defects:      []RedactionDefect{{Seq: 7, Entry: 0, Err: err}},
		},
	}
}

// scribbleBacking changes every byte, element and pointee an Item's slices and pointers reach, without reassigning them.
func scribbleBacking(it *Item) {
	it.Record.Payload[0] ^= 0xff
	it.HeaderExtra[0] ^= 0xff
	rr := it.Redaction
	rr.WhollyMasked = false
	e := &rr.Entries[0]
	e.Seq++
	*e.ItemPath = "/9"
	e.MaskedRanges[0].Offset++
	e.RawMaskedRanges[0] ^= 0xff
	e.Digest[0] ^= 0xff
	e.SIV[0] ^= 0xff
	rr.Defects[0].Seq++
	rr.Defects[0].Err = errors.New("other")
}

// Item.Clone shares no mutable storage with its source, in either direction, the defects' errors excepted,
// and keeps a nil field nil; Record.Clone copies the payload.
func TestItemClone(t *testing.T) {
	t.Parallel()

	defect := fmt.Errorf("a defect: %w", ErrRedactionDefect)

	src := cloneTestItem(defect)
	c := src.Clone()
	assert.Equal(t, cloneTestItem(defect), c)
	assert.Same(t, src.Redaction.Defects[0].Err, c.Redaction.Defects[0].Err, "the defect's error is shared")
	scribbleBacking(&src)
	assert.Equal(t, cloneTestItem(defect), c, "the source changed after Clone")

	src = cloneTestItem(defect)
	c = src.Clone()
	scribbleBacking(&c)
	assert.Equal(t, cloneTestItem(defect), src, "the clone changed after Clone")

	empty := (&Item{Redaction: &RecordRedaction{}}).Clone()
	assert.Nil(t, empty.Record.Payload)
	assert.Nil(t, empty.HeaderExtra)
	assert.Equal(t, &RecordRedaction{}, empty.Redaction)
	assert.Nil(t, empty.Redaction.Entries)
	assert.Nil(t, empty.Redaction.Defects)
	assert.Nil(t, (&Item{}).Clone().Redaction)

	rec := Record{Seq: 3, Payload: []byte{1, 2}}
	rc := rec.Clone()
	rc.Payload[0] = 9
	assert.Equal(t, Record{Seq: 3, Payload: []byte{1, 2}}, rec)
	assert.Nil(t, (&Record{}).Clone().Payload)
}

// An Item a read yields, cloned in the callback, stays as the read yielded it after the read reuses its buffers.
func TestItemCloneOutlivesCallback(t *testing.T) {
	t.Parallel()

	readers := []*Reader{mustOpen(t, itemTestPack(t), ReaderOptions{})}
	for _, rd := range itemTestReads() {
		var kept []Item
		var want []iterItem
		rd.read(t, readers, Query{Payloads: true}, func(it *Item) {
			kept = append(kept, it.Clone())
			want = append(want, copyItem(it))
		})
		require.Len(t, kept, 5)
		for i := range kept {
			assert.Equal(t, want[i], copyItem(&kept[i]), "%s: item %d", rd.name, i)
		}
	}
}
