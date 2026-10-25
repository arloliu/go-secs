package tracepack

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txAlign rounds n up to the 16-byte multiple the nominal charges of fixed-size parts take.
func txAlign(n uintptr) int64 {
	return (int64(n) + 15) &^ 15
}

// txRedactionRecount recounts the charge of a copy of rr from its contents:
// the RecordRedaction, each defect, and each entry with its masked ranges and its bytes.
func txRedactionRecount(rr *RecordRedaction) int64 {
	if rr == nil {
		return 0
	}
	n := txAlign(unsafe.Sizeof(RecordRedaction{})) + int64(len(rr.Defects))*txAlign(unsafe.Sizeof(RedactionDefect{}))
	for i := range rr.Entries {
		e := &rr.Entries[i]
		n += txAlign(unsafe.Sizeof(RedactionEntry{})) + int64(len(e.MaskedRanges))*int64(unsafe.Sizeof(MaskedRange{})) +
			int64(len(e.RawMaskedRanges)) + int64(len(e.Domain)) + int64(len(e.Digest)) + int64(len(e.SIV))
		if e.ItemPath != nil {
			n += int64(len(*e.ItemPath))
		}
	}

	return n
}

// txChargeEvents recounts, from rec's contents, the charges a fill of rec made against MaxCachedBytes, in the order it made them:
// the record and its observation; each reservation and each item in yield order,
// an item with the key of its seq when b pairs it (txRangePrimary) and the item before it did not key the same seq;
// then each defect, each footer error and each conflict of the read's Result.
// It also returns the number of items keyed, each charged a key, before the fill releases the keys of repeated seqs.
func txChargeEvents(b *txBatch, rec *txHourRecord) ([]txChargeEvent, int) {
	events := []txChargeEvent{{n: txAlign(unsafe.Sizeof(txHourRecord{})) + int64(len(rec.readers))*int64(unsafe.Sizeof(uintptr(0)))}}
	enumerate := rec.hour >= b.first && rec.hour <= b.last
	keyed, k := 0, 0
	var last uint64
	for i := 0; ; i++ {
		for ; k < len(rec.reservations) && rec.reservations[k] == i; k++ {
			events = append(events, txChargeEvent{n: int64(unsafe.Sizeof(int(0))), items: i})
		}
		if i == len(rec.items) {
			break
		}
		it := &rec.items[i]
		n := txAlign(unsafe.Sizeof(Item{})+unsafe.Sizeof(uint8(0))) + int64(len(it.Record.Payload)) + int64(len(it.HeaderExtra)) +
			txRedactionRecount(it.Redaction)
		capture := UUID(rec.readers[it.Pack].hdr.CaptureID)
		if enumerate && txRangePrimary(b.r.Capture, capture, &it.Record, rec.hour, b.fromNs, b.toNs) &&
			(keyed == 0 || last != it.Record.Seq) {
			n += int64(unsafe.Sizeof(uint64(0)))
			keyed, last = keyed+1, it.Record.Seq
		}
		events = append(events, txChargeEvent{n: n, items: i + 1})
	}
	for i := range rec.res.Incomplete {
		d := &rec.res.Incomplete[i]
		n := txAlign(unsafe.Sizeof(Defect{}))
		if c := d.Coverage; c != nil {
			n += txCoverageCharge
			for _, u := range c.Unknown {
				n += txRawEntryCharge + int64(len(u.Value))
			}
		}
		if d.Boundary != nil {
			n += txBoundaryCharge
		}
		events = append(events, txChargeEvent{n: n, items: len(rec.items)})
	}
	for range rec.res.FooterErrs {
		events = append(events, txChargeEvent{n: txAlign(unsafe.Sizeof(PackError{})), items: len(rec.items)})
	}
	for i := range rec.res.Conflicts {
		n := txConflictCharge
		for _, v := range rec.res.Conflicts[i].Versions {
			n += txVersionCharge + int64(len(v))*txPackCharge
		}
		events = append(events, txChargeEvent{n: n, items: len(rec.items)})
	}

	return events, keyed
}

// txChargeEvent is one charge of a fill: its size, and the items the record holds once it is admitted.
type txChargeEvent struct {
	n     int64
	items int
}

// txCacheRecount recounts, from its contents, what rec, an hour record b filled, must have charged against MaxCachedBytes:
// every charge of the fill (txChargeEvents), less the key of each item that keyed a seq another item keyed too.
func txCacheRecount(b *txBatch, rec *txHourRecord) int64 {
	events, keyed := txChargeEvents(b, rec)
	var n int64
	for _, e := range events {
		n += e.n
	}

	return n - int64(keyed-len(rec.keys))*int64(unsafe.Sizeof(uint64(0)))
}

// txFill fills the hour memTestHour of src, with MaxScopes 1 and limit as MaxCachedBytes (0 for the default),
// and returns the batch, the hour's record, as filled or as it stood when the fill failed (nil when the batch never held it),
// and the error.
func txFill(t *testing.T, src PackSource, limit int64) (*txBatch, *txHourRecord, error) {
	t.Helper()

	var batch *txBatch
	var rec *txHourRecord
	_, err := findTransactionsWith(t.Context(), src, txBatchHour(0),
		TxRangeOptions{Lookup: TxOptions{MaxScopes: 1}, MaxCachedBytes: limit}, func(*TxPairing) error { return nil },
		func(b *txBatch) {
			batch = b
			b.filled = func(r *txHourRecord) {
				rec = r
				require.Equal(t, r.charge, b.cache.held, "the batch holds the hour's charge once it is filled")
			}
		})
	if r, ok := batch.hours[memTestHour]; ok {
		rec = r
	}

	return batch, rec, err
}

// txFillCharge returns the charge of the hour memTestHour of src once filled, requiring it to equal its recount.
func txFillCharge(t *testing.T, src PackSource) int64 {
	t.Helper()

	b, rec, err := txFill(t, src, 0)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, txCacheRecount(b, rec), rec.charge, "the charge against its recount")

	return rec.charge
}

// requireExactAdmission fills the hour memTestHour of src under a MaxCachedBytes at each edge of each charge the fill makes,
// as recounted from the hour's contents (txChargeEvents):
// one byte under a charge, the fill fails with ErrReadLimit, the batch holding exactly the charges before it
// and the record exactly the items they admitted;
// at exactly the sum through a charge, the fill admits it and fails at the next, or succeeds after the last.
func requireExactAdmission(t *testing.T, src PackSource) {
	t.Helper()

	b, rec, err := txFill(t, src, 0)
	require.NoError(t, err)
	events, keyed := txChargeEvents(b, rec)
	require.Len(t, rec.keys, keyed, "no seq keyed twice, so no key is released after its charge")
	refused := func(limit, held int64, items int, msg string) {
		t.Helper()

		fb, frec, err := txFill(t, src, limit)
		require.ErrorIs(t, err, ErrReadLimit, msg)
		assert.Equal(t, held, fb.cache.held, "%s: the charges admitted", msg)
		if held == 0 {
			assert.Nil(t, frec, "%s: no record held", msg)
		} else {
			require.NotNil(t, frec, msg)
			assert.Equal(t, held, frec.charge, "%s: the partial record's charge", msg)
			assert.Len(t, frec.items, items, "%s: the items kept", msg)
		}
	}
	var sum int64
	items := 0
	for k, e := range events {
		refused(sum+e.n-1, sum, items, fmt.Sprintf("one byte under charge %d of %d", k, len(events)))
		sum, items = sum+e.n, e.items
		if k < len(events)-1 {
			refused(sum, sum, items, fmt.Sprintf("exactly through charge %d of %d", k, len(events)))
		}
	}
	_, frec, err := txFill(t, src, sum)
	require.NoError(t, err, "a MaxCachedBytes of exactly every charge")
	assert.Equal(t, sum, frec.charge)
}

// Each variable-sized part of an hour record is charged by its size:
// the value bytes of a coverage entry's unknown nested entries, the holders of a conflict's versions,
// and a record's redaction entries, their masked ranges, their bytes and the defects found in them,
// each measured as the difference between two hours that differ only in it, against the size of the part;
// and each hour, one of no record among them, is admitted and refused at each edge of each charge its fill makes.
func TestFindTransactionsVariableCharges(t *testing.T) {
	t.Parallel()

	ptr, uuid := int64(unsafe.Sizeof(uintptr(0))), int64(unsafe.Sizeof(UUID{}))

	t.Run("coverage unknown entries", func(t *testing.T) {
		t.Parallel()

		withUnknown := func(n int) PackSource {
			return txSource(t, txPack(t, seg0, func(m *PackMeta) {
				m.Coverage = []Coverage{{SeqFirst: new(uint64(0)), SeqLast: new(uint64(5)),
					Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: bytes.Repeat([]byte{1}, n)}}}}
			}, txBlock(txRecord(10, nil))))
		}
		assert.Equal(t, int64(4096), txFillCharge(t, withUnknown(4097))-txFillCharge(t, withUnknown(1)), "4096 more value bytes")
		requireExactAdmission(t, withUnknown(4097))
	})

	t.Run("an hour of no record", func(t *testing.T) {
		t.Parallel()

		src := txSource(t, txPack(t, seg0, func(m *PackMeta) {
			m.Coverage = []Coverage{{SeqFirst: new(uint64(0)), SeqLast: new(uint64(5)),
				Unknown: []RawEntry{{Tag: 0x0050, Type: 7, Value: bytes.Repeat([]byte{1}, 512)}}}}
		}, nil))
		b, rec, err := txFill(t, src, 0)
		require.NoError(t, err)
		require.Zero(t, b.res.Primaries)
		require.Empty(t, rec.items)
		assert.Greater(t, rec.charge, int64(512), "the coverage entry's diagnostics are charged without any item")
		requireExactAdmission(t, src)
	})

	t.Run("conflict holders", func(t *testing.T) {
		t.Parallel()

		other := func(r *Record) {
			r.Payload = bytes.Clone(r.Payload)
			r.Payload[len(r.Payload)-1]++
		}
		hour := func(holders []UUID) PackSource {
			files := make([][]byte, 0, 1+len(holders))
			files = append(files, txPack(t, seg4, nil, txBlock(txRecord(10, other))))
			for _, id := range holders {
				files = append(files, txPack(t, id, nil, txBlock(txRecord(10, nil))))
			}

			return txSource(t, files...)
		}
		// Two more readers in the observation and two more holders of the first version.
		assert.Equal(t, 2*ptr+2*uuid, txFillCharge(t, hour([]UUID{seg0, seg1, seg2, seg3}))-txFillCharge(t, hour([]UUID{seg0, seg1})))
		requireExactAdmission(t, hour([]UUID{seg0, seg1, seg2, seg3}))
	})

	t.Run("redaction entries", func(t *testing.T) {
		t.Parallel()

		// note masks the text of maskedNote(3) under the entry e, which edit changes; more adds entries after it.
		note := func(edit func(e *RedactionEntry), more ...RedactionEntry) PackSource {
			m := padMeta(extractMinorMeta(0))
			e := maskedNoteEntry(3, false)
			if edit != nil {
				edit(&e)
			}
			m.Redaction = append(append(m.Redaction, e), more...)

			return txSource(t, writeMinorPack(t, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}},
				redactedRecord(0), maskedNote(3)))
		}
		base := txFillCharge(t, note(nil))
		long := note(func(e *RedactionEntry) { e.Domain += strings.Repeat("x", 1000) })
		assert.Equal(t, int64(1000), txFillCharge(t, long)-base, "1000 more domain bytes in the record's redaction entry")

		// A second entry naming the record, its domain, digest and one masked range.
		second := maskedNoteEntry(3, false)
		entry := txAlign(unsafe.Sizeof(RedactionEntry{})) + int64(len(second.Domain)+len(second.Digest)) + int64(unsafe.Sizeof(MaskedRange{}))
		assert.Equal(t, entry, txFillCharge(t, note(nil, second))-base, "one more redaction entry")

		// Two disjoint masked ranges in place of one, then the same two adjacent, which the record's read finds a defect of.
		split := func(gap uint64) func(e *RedactionEntry) {
			return func(e *RedactionEntry) {
				e.MaskedRanges = []MaskedRange{{Offset: maskedNoteTextOffset, Length: 2}, {Offset: maskedNoteTextOffset + 2 + gap, Length: 2}}
			}
		}
		disjoint, adjacent := txFillCharge(t, note(split(2))), txFillCharge(t, note(split(0)))
		assert.Equal(t, int64(unsafe.Sizeof(MaskedRange{})), disjoint-base, "one more masked range")
		assert.Equal(t, txAlign(unsafe.Sizeof(RedactionDefect{})), adjacent-disjoint, "one redaction defect")

		requireExactAdmission(t, long)
		requireExactAdmission(t, note(split(0), second))
	})
}
