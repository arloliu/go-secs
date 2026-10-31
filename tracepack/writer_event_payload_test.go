package tracepack

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unknownPayloadTag is a tag outside both the transport-event and the annotation tag registry.
const unknownPayloadTag uint16 = 0x0F00

// eventPayloadRecord returns a record of kind, a transport event or an annotation, of seq at blockTestHour + 1 + seq,
// carrying payload.
func eventPayloadRecord(kind Kind, seq uint64, payload []byte) Record {
	return Record{
		Seq: seq, TSUTCNs: blockTestHour + 1 + int64(seq), Epoch: 1, Kind: kind, Dir: DirLocal,
		Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
	}
}

// tlvBody returns the TLV encoding of entries.
func tlvBody(entries ...tlv.Entry) []byte {
	var b []byte
	for _, e := range entries {
		b = tlv.AppendEntry(b, e)
	}

	return b
}

// undecodableEventPayloads returns transport-event and annotation payloads that do not decode,
// with the sentinel their decode error wraps, when it wraps one.
func undecodableEventPayloads(t testing.TB) []struct {
	name    string
	kind    Kind
	payload []byte
	want    error
} {
	t.Helper()

	event := mustMarshalEvent(t, &TransportEvent{Event: EventSocketClose, SocketRole: new(SocketRoleActive)})
	note := mustMarshalNote(t)
	// A detail entry, of type utf-8, whose value is not valid UTF-8.
	badUTF8 := tlv.Entry{Tag: tagDetail, Type: tlv.TypeUTF8, Value: []byte{0xFF, 0xFE}}
	// An entry header declaring 4 bytes of value, followed by 1.
	overrun := binary.LittleEndian.AppendUint16(tlvBody(tlv.U8Entry(tagEvent, uint8(EventStateTransition))), tagDetail)
	overrun = append(overrun, byte(tlv.TypeUTF8), 0)
	overrun = binary.LittleEndian.AppendUint32(overrun, 4)
	overrun = append(overrun, 'x')

	return []struct {
		name    string
		kind    Kind
		payload []byte
		want    error
	}{
		{name: "transport event, not TLV framing", kind: KindTransportEvent, payload: []byte{0xFF}},
		{name: "transport event, truncated", kind: KindTransportEvent, payload: event[:len(event)-1]},
		{name: "transport event, an entry running past the body", kind: KindTransportEvent, payload: overrun},
		{name: "transport event, without event", kind: KindTransportEvent,
			payload: tlvBody(tlv.U8Entry(tagSocketRole, uint8(SocketRoleActive)))},
		{name: "transport event, a u8 of two bytes", kind: KindTransportEvent,
			payload: tlvBody(tlv.Entry{Tag: tagEvent, Type: tlv.TypeU8, Value: []byte{1, 2}})},
		{name: "transport event, invalid UTF-8", kind: KindTransportEvent,
			payload: tlvBody(tlv.U8Entry(tagEvent, uint8(EventStateTransition)), badUTF8)},
		{name: "annotation, not TLV framing", kind: KindAnnotation, payload: []byte{0x01}},
		{name: "annotation, truncated", kind: KindAnnotation, payload: note[:len(note)-1]},
		{name: "annotation, without annotation_kind", kind: KindAnnotation,
			payload: tlvBody(tlv.UTF8Entry(tagText, "note"))},
		{name: "annotation, invalid UTF-8 text", kind: KindAnnotation,
			payload: tlvBody(tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote)),
				tlv.Entry{Tag: tagText, Type: tlv.TypeUTF8, Value: []byte{0xFF, 0xFE}})},
		{name: "annotation, neither text nor raw", kind: KindAnnotation,
			payload: tlvBody(tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote))), want: ErrAnnotationText},
		{name: "annotation, both text and raw", kind: KindAnnotation,
			payload: tlvBody(tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote)),
				tlv.UTF8Entry(tagText, "note"), tlv.BytesEntry(tagRaw, []byte{1})), want: ErrAnnotationText},
	}
}

// newerEventPayloads returns valid transport-event and annotation payloads that a newer writer might write:
// a tag outside the registry, and an enum value this package does not name.
func newerEventPayloads() []struct {
	name    string
	kind    Kind
	payload []byte
} {
	unknown := tlv.BytesEntry(unknownPayloadTag, []byte{1, 2, 3})

	return []struct {
		name    string
		kind    Kind
		payload []byte
	}{
		{name: "transport event, unknown tag", kind: KindTransportEvent,
			payload: tlvBody(tlv.U8Entry(tagEvent, uint8(EventStateTransition)), unknown)},
		{name: "transport event, unknown event", kind: KindTransportEvent, payload: tlvBody(tlv.U8Entry(tagEvent, 0xEE))},
		{name: "transport event, unknown socket_role", kind: KindTransportEvent,
			payload: tlvBody(tlv.U8Entry(tagEvent, uint8(EventSocketConnect)), tlv.U8Entry(tagSocketRole, 0xEE))},
		{name: "annotation, unknown tag", kind: KindAnnotation,
			payload: tlvBody(tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote)), tlv.UTF8Entry(tagText, "note"), unknown)},
		{name: "annotation, unknown annotation_kind", kind: KindAnnotation,
			payload: tlvBody(tlv.U8Entry(tagAnnotationKind, 0xEE), tlv.UTF8Entry(tagText, "note"))},
	}
}

// TestWriterRefusesAnUndecodableEventPayload appends a transport-event or annotation record whose payload does not decode:
// Append refuses it with ErrInvalidRecord, marked ErrRefused and with the SegmentWriter's error text,
// takes no seq, and the Writer takes the next record and closes a pack holding only the records it took.
func TestWriterRefusesAnUndecodableEventPayload(t *testing.T) {
	t.Parallel()

	for _, tt := range undecodableEventPayloads(t) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			w := refusalWriter(t, &out, nil)
			bad := eventPayloadRecord(tt.kind, 0, tt.payload)
			err := w.Append(&bad)
			requireRefused(t, err, ErrInvalidRecord)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
			}

			seg, _ := newSeg(t, nil)
			segRec := bad
			segRec.TSUTCNs = segAt(time.Second).UnixNano()
			require.EqualError(t, seg.Append(t.Context(), &segRec), err.Error(), "the SegmentWriter's refusal")

			// No seq was taken: the pack's first record still has seq_start.
			good := testDataRecord(0, blockTestHour+1, 1)
			require.NoError(t, w.Append(&good))
			_, err = w.Close()
			require.NoError(t, err)

			_, recs := readMergedRecords(t, out.Bytes())
			require.Len(t, recs, 1)
			assert.Equal(t, KindData, recs[0].Kind)
		})
	}
}

// TestWriterAcceptsANewerEventPayload appends transport-event and annotation records whose payloads only a newer reader knows:
// a tag outside the registry or an enum value this package does not name is preserved
// (the tracepack format specification §14), so Append stores the payload as given.
func TestWriterAcceptsANewerEventPayload(t *testing.T) {
	t.Parallel()

	for _, tt := range newerEventPayloads() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			w := refusalWriter(t, &out, nil)
			r := eventPayloadRecord(tt.kind, 0, tt.payload)
			require.NoError(t, w.Append(&r))
			_, err := w.Close()
			require.NoError(t, err)

			_, recs := readMergedRecords(t, out.Bytes())
			require.Len(t, recs, 1)
			assert.Equal(t, tt.payload, recs[0].Payload)
			rep := mustVerify(t, out.Bytes())
			assert.Equal(t, VerifyOutcomeFinalizedConsistent, rep.Outcome)
			assert.Empty(t, rep.WriterDefects)
		})
	}
}

// TestMergeCopiesAnUndecodableEventPayload merges a pack another writer wrote holding a transport-event record
// and an annotation record whose payloads do not decode, a writer defect:
// Merge does not refuse the records but copies them as they are, whether it copies their block or resolves it,
// and Verify reports them in the archive.
func TestMergeCopiesAnUndecodableEventPayload(t *testing.T) {
	t.Parallel()

	steps := hourSteps(10, 13, 4)
	steps[1].rec = eventPayloadRecord(KindTransportEvent, 11, []byte{0xFF})
	steps[1].rec.TSUTCNs = blockTestHour + 11
	steps[2].rec = eventPayloadRecord(KindAnnotation, 12, []byte{0x01})
	steps[2].rec.TSUTCNs = blockTestHour + 12
	seg := withIDs(t, writeUncheckedPack(t, steps), seg0, viewCapture)
	// Seqs 12 to 15 in another segment, overlapping seg at seqs 12 and 13 with the same records,
	// so the merge resolves the blocks record by record instead of copying them.
	more := hourSteps(12, 15, 4)
	more[0].rec = steps[2].rec
	overlap := withIDs(t, writeUncheckedPack(t, more), seg1, viewCapture)

	tests := []struct {
		name     string
		files    [][]byte
		n        int
		resolved bool
	}{
		{name: "copied", files: [][]byte{seg}, n: 4},
		{name: "resolved", files: [][]byte{seg, overlap}, n: 6, resolved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, rep := mustMerge(t, inputsOf(tt.files...), mergeOpts())
			if tt.resolved {
				require.Positive(t, rep.Resolved)
			} else {
				require.Equal(t, 1, rep.Copied)
			}
			_, recs := readMergedRecords(t, out)
			require.Len(t, recs, tt.n)
			assert.Equal(t, []byte{0xFF}, recs[1].Payload)
			assert.Equal(t, []byte{0x01}, recs[2].Payload)

			vr := mustVerify(t, out)
			assert.Equal(t, VerifyOutcomeFinalizedConsistent, vr.Outcome)
			require.Len(t, vr.WriterDefects, 2)
			for i, d := range vr.WriterDefects {
				assert.Equal(t, WriterDefectEventPayload, d.Kind)
				assert.Equal(t, uint64(11+i), d.Seq)
			}
		})
	}
}

// writeUncheckedPack writes steps into a finalized pack of repairTestMeta,
// storing a transport-event or annotation payload that does not decode as another writer might.
func writeUncheckedPack(t testing.TB, steps []footerTestStep) []byte {
	t.Helper()

	m := repairTestMeta()
	m.SeqStart = steps[0].rec.Seq
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m})
	require.NoError(t, err)
	for i := range steps {
		require.NoError(t, w.addRecord(&steps[i].rec, false), "step %d", i)
		if steps[i].flush {
			require.NoError(t, w.Flush())
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}
