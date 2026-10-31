package tracepack

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// thinnedMeta returns repairTestMeta declaring S1F3 and S1F4 in thinning,
// the FDC service's interval polls and their replies.
func thinnedMeta() *PackMeta {
	m := repairTestMeta()
	m.Thinning = []Thinning{
		{Stream: 1, Function: 3, Source: "fdc interval poll"},
		{Stream: 1, Function: 4, Source: "fdc interval poll"},
	}

	return m
}

// hsmsFrame returns an HSMS data frame of stream s and function f carrying text as its message text.
func hsmsFrame(s, f byte, text []byte) []byte {
	b := make([]byte, 14, 14+len(text))
	binary.BigEndian.PutUint32(b, uint32(10+len(text)))
	b[5], b[6], b[7], b[13] = 1, s, f, 7

	return append(b, text...)
}

// frameRecord returns a classified data record of seq over frame, at seq nanoseconds into blockTestHour.
func frameRecord(seq uint64, frame []byte) Record {
	r := Record{
		Seq: seq, TSUTCNs: blockTestHour + int64(seq), Epoch: 1,
		Kind: KindData, Dir: DirEquipmentToHost, Fidelity: FidelityWireExact, DecodeStatus: DecodeStatusOK, Payload: frame,
	}
	r.SetCapturedFieldValidity()

	return r
}

// unobservedRecord returns an S1F4 record of seq unobserved by design (the tracepack semantics specification §2):
// its 6-byte message text zero-filled, not classified, carrying QualityUnobserved.
func unobservedRecord(seq uint64) Record {
	r := frameRecord(seq, hsmsFrame(1, 4, make([]byte, 6)))
	r.DecodeStatus = DecodeStatusNotAttempted
	r.Quality = QualityUnobserved

	return r
}

// The Writer accepts a record carrying quality.unobserved
// only as an unobserved data record of a declared stream and function
// (the tracepack semantics specification §2, the tracepack storage specification §4), whatever SkipValidation says;
// it refuses any other before the record takes a seq, leaving the Writer usable.
func TestWriterUnobserved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		meta   func() *PackMeta
		edit   func(r *Record)
		field  string // the commitment's field for a *FieldError wrapping ErrMetadataCommitment
		record bool   // the record's own fields break the rule: ErrInvalidRecord
	}{
		{name: "declared S/F with a zero-filled text", meta: thinnedMeta, edit: func(*Record) {}},
		{name: "undeclared S/F", meta: thinnedMeta, edit: func(r *Record) { r.Payload[7] = 6 }, field: "thinning"},
		{name: "no thinning", meta: repairTestMeta, edit: func(*Record) {}, field: "thinning"},
		{name: "a control record", meta: thinnedMeta, edit: func(r *Record) { r.Kind = KindControl }, record: true},
		{name: "payload_len 14", meta: thinnedMeta, edit: func(r *Record) { r.Payload = r.Payload[:14] }, record: true},
		{name: "a non-zero text byte", meta: thinnedMeta, edit: func(r *Record) { r.Payload[19] = 1 }, record: true},
		{name: "classified", meta: thinnedMeta, edit: func(r *Record) { r.DecodeStatus = DecodeStatusOK }, record: true},
		{name: "trailing bytes", meta: thinnedMeta, edit: func(r *Record) { r.TrailingBytes = 1 }, record: true},
		{name: "redacted", meta: thinnedMeta, edit: func(r *Record) { r.Quality |= QualityRedacted }, record: true},
		{
			name: "function unavailable", meta: thinnedMeta, record: true,
			edit: func(r *Record) { r.FieldValidity &^= FieldValidityFunction },
		},
	}
	for _, skip := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				var buf bytes.Buffer
				w, err := NewWriter(&buf, WriterOptions{Meta: tt.meta(), SkipValidation: skip})
				require.NoError(t, err)

				r := unobservedRecord(0)
				tt.edit(&r)
				err = w.Append(&r)
				switch {
				case tt.field != "":
					require.ErrorIs(t, err, ErrMetadataCommitment)
					var fe *FieldError
					require.ErrorAs(t, err, &fe)
					assert.Equal(t, tt.field, fe.Field)
				case tt.record:
					require.ErrorIs(t, err, ErrInvalidRecord)
				default:
					require.NoError(t, err)
					_, err = w.Close()
					require.NoError(t, err)

					return
				}

				plain := frameRecord(0, hsmsFrame(1, 4, nil))
				require.NoError(t, w.Append(&plain), "no seq taken, the Writer still usable")
				_, err = w.Close()
				require.NoError(t, err)
			})
		}
	}
}

// NewWriter refuses a thinning entry no record can match: a stream above 127 or a function above 255.
func TestWriterThinningRange(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		th Thinning
		ok bool
	}{
		{Thinning{Stream: 127, Function: 255, Source: "s"}, true},
		{Thinning{Stream: 128, Function: 1, Source: "s"}, false},
		{Thinning{Stream: 1, Function: 256, Source: "s"}, false},
	} {
		m := repairTestMeta()
		m.Thinning = []Thinning{{Stream: 1, Function: 3, Source: "s"}, tt.th}
		var buf bytes.Buffer
		_, err := NewWriter(&buf, WriterOptions{Meta: m})
		if tt.ok {
			require.NoError(t, err, "%+v", tt.th)
			continue
		}
		require.ErrorIs(t, err, ErrFieldValue, "%+v", tt.th)
		var fe *FieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "thinning", fe.Field)
		assert.Zero(t, buf.Len())
	}
}

// An unobserved record written through the Writer keeps bit 7
// in its encoded record header, its block's F-3 and the pack's F-5,
// and reads back unobserved, with and without block validation.
func TestUnobservedRecordBytes(t *testing.T) {
	t.Parallel()

	for _, skip := range []bool{false, true} {
		var buf bytes.Buffer
		w, err := NewWriter(&buf, WriterOptions{Meta: thinnedMeta(), SkipValidation: skip})
		require.NoError(t, err)
		recs := []Record{frameRecord(0, hsmsFrame(1, 4, []byte{0x21, 0x01, 0x00})), unobservedRecord(1)}
		for i := range recs {
			require.NoError(t, w.Append(&recs[i]))
		}
		_, err = w.Close()
		require.NoError(t, err)
		file := buf.Bytes()

		r, blocks := readSourceBlocks(t, file)
		require.Len(t, blocks, 1)
		assert.True(t, blocks[0].sum.qualityUnion.HasAny(QualityUnobserved), "the block's records, decoded from its bytes")
		assert.Equal(t, uint16(1), r.Header().FormatMinor)

		tr, decoded, blocksStart := splitPack(t, file)
		idx, err := parseFooter(decoded, &tr, blocksStart, DefaultMaxFooterLen, false)
		require.NoError(t, err)
		assert.True(t, idx.blocks[0].qualityUnion.HasAny(QualityUnobserved), "F-3 quality_union")
		assert.True(t, footerStats(t, decoded).qualityUnion.HasAny(QualityUnobserved), "F-5 quality_union")

		var got []bool
		_, err = r.Iterate(t.Context(), Query{}, func(it *Item) error {
			assert.Equal(t, it.Record.Quality.HasAny(QualityUnobserved), r.Unobserved(&it.Record))
			got = append(got, r.Unobserved(&it.Record))

			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, []bool{false, true}, got)
	}
}

// An unobserved record, a record whose message text is genuinely empty and an abnormal reply hold no text to analyse;
// no filter selects by the bit, and Reader.Unobserved, the payload's length and decode_status tell them apart,
// through Iterate and MergeIterate (the tracepack semantics specification §2).
func TestUnobservedDistinguishable(t *testing.T) {
	t.Parallel()

	empty := frameRecord(1, hsmsFrame(1, 4, nil))
	abnormal := frameRecord(2, hsmsFrame(9, 7, []byte{0xB1, 0x0A, 0, 1, 7, 1, 4, 0, 0, 0, 0, 7}))
	recs := []Record{unobservedRecord(0), empty, abnormal}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: thinnedMeta()})
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]))
	}
	_, err = w.Close()
	require.NoError(t, err)
	r := mustOpen(t, buf.Bytes(), ReaderOptions{})

	type seen struct {
		seq        uint64
		unobserved bool
		payloadLen int
		status     DecodeStatus
	}
	collect := func(sf SF) (fromIterate, fromMerge []seen) {
		q := Query{Filter: Filter{SF: []SF{sf}}, Payloads: true}
		_, err := r.Iterate(t.Context(), q, func(it *Item) error {
			fromIterate = append(fromIterate, seen{it.Record.Seq, r.Unobserved(&it.Record), len(it.Record.Payload), it.Record.DecodeStatus})
			return nil
		})
		require.NoError(t, err)
		readers := []*Reader{r}
		_, err = MergeIterate(t.Context(), readers, q, MergeIterateOptions{Order: OrderCapture}, func(it *Item) error {
			rd := readers[it.Input]
			fromMerge = append(fromMerge, seen{it.Record.Seq, rd.Unobserved(&it.Record), len(it.Record.Payload), it.Record.DecodeStatus})
			return nil
		})
		require.NoError(t, err)

		return fromIterate, fromMerge
	}

	want := map[SF][]seen{
		{Stream: 1, Function: 4}: {{0, true, 20, DecodeStatusNotAttempted}, {1, false, 14, DecodeStatusOK}},
		{Stream: 9, Function: 7}: {{2, false, 26, DecodeStatusOK}},
	}
	for sf, w := range want {
		fromIterate, fromMerge := collect(sf)
		assert.Equal(t, w, fromIterate, "Iterate %v", sf)
		assert.Equal(t, w, fromMerge, "MergeIterate %v", sf)
	}
}
