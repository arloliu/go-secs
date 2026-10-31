package tracepack

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// requireFieldError requires err to hold a *FieldError naming field whose Err wraps want, the chain wrapping cause too.
func requireFieldError(t *testing.T, err error, field string, want, cause error) {
	t.Helper()

	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, field, fe.Field)
	require.ErrorIs(t, fe, want)
	require.ErrorIs(t, err, want)
	if cause != nil {
		require.ErrorIs(t, err, cause, "the encoding's reason stays in the chain")
	}
}

// TestFieldErrorOf maps each fault of an entry to the field sentinel and names the field through the nested registries.
func TestFieldErrorOf(t *testing.T) {
	t.Parallel()

	nested := func(tag uint16, inner *tlv.EntryError) *tlv.EntryError {
		return &tlv.EntryError{Tag: tag, Offset: 8, Err: inner}
	}
	tests := []struct {
		name  string
		err   *tlv.EntryError
		field string
		want  error
	}{
		{"missing top-level tag", &tlv.EntryError{Tag: tagToolID, Offset: -1, Err: tlv.ErrMissing}, "tool_id", ErrRequiredTag},
		{"missing nested tag", nested(tagRedaction, &tlv.EntryError{Tag: tagRedactionEntrySeq, Offset: -1, Err: tlv.ErrMissing}), "redaction.seq", ErrRequiredTag},
		{"length", &tlv.EntryError{Tag: tagRecorderInstanceID, Err: tlv.ErrLength}, "recorder_instance_id", ErrFieldLength},
		{"nested length", nested(tagCoverage, &tlv.EntryError{Tag: tagCoverageCaptureID, Err: tlv.ErrLength}), "coverage.capture_id", ErrFieldLength},
		{"UTF-8", &tlv.EntryError{Tag: tagToolID, Err: tlv.ErrUTF8}, "tool_id", ErrFieldValue},
		{"limit", &tlv.EntryError{Tag: tagDeviceID, Err: tlv.ErrLimit}, "device_id", ErrFieldValue},
		{"bool", &tlv.EntryError{Tag: tagQualityEvaluated, Err: tlv.ErrBool}, "quality_evaluated", ErrFieldValue},
		{"type", &tlv.EntryError{Tag: tagTransport, Err: tlv.ErrType}, "transport", ErrFieldValue},
		{"duplicate", &tlv.EntryError{Tag: tagToolID, Err: tlv.ErrDuplicate}, "tool_id", ErrFieldValue},
		{"zero tag", &tlv.EntryError{Tag: 0, Err: tlv.ErrZeroTag}, "0x0000", ErrFieldValue},
		{"depth", &tlv.EntryError{Tag: tagRedactionPolicy, Err: tlv.ErrDepth}, "redaction_policy", ErrFieldValue},
		{"unknown tag", &tlv.EntryError{Tag: 0x7F00, Err: tlv.ErrLength}, "0x7F00", ErrFieldLength},
		{"unknown nested tag", nested(tagHSMSTimers, &tlv.EntryError{Tag: 9, Err: tlv.ErrLength}), "hsms_timers.0x0009", ErrFieldLength},
		{"truncated nested list", nested(tagRedactionPolicy, &tlv.EntryError{Tag: 1, Err: tlv.ErrTruncated}), "redaction_policy", ErrFieldValue},
		{"truncated twice nested", nested(tagRedaction, nested(tagRedactionEntryDigest, &tlv.EntryError{Err: tlv.ErrTruncated})), "redaction.digest", ErrFieldValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := fieldErrorOf(tt.err, tlv.PackMetadata)
			requireFieldError(t, err, tt.field, tt.want, tt.err)
			var ee *tlv.EntryError
			require.ErrorAs(t, err, &ee)
			assert.Same(t, tt.err, ee, "the outermost entry error is kept")
		})
	}

	t.Run("unchanged", func(t *testing.T) {
		t.Parallel()

		truncated := &tlv.EntryError{Tag: tagToolID, Offset: 12, Err: tlv.ErrTruncated}
		assert.Same(t, error(truncated), fieldErrorOf(truncated, tlv.PackMetadata), "a list that ends inside an entry has no field")
		fe := &FieldError{Field: "pack_role", Err: ErrFieldValue}
		assert.Same(t, error(fe), fieldErrorOf(fe, tlv.PackMetadata))
		assert.Same(t, ErrAnnotationText, fieldErrorOf(ErrAnnotationText, tlv.Annotation))
		require.NoError(t, fieldErrorOf(nil, tlv.Annotation))
	})
}

// TestCodecErrorsWritingCalls requires a value a writing call's encoding refuses to be a refusal
// wrapping a *FieldError that names the field and its field sentinel, never ErrCorrupt.
func TestCodecErrorsWritingCalls(t *testing.T) {
	t.Parallel()

	bad := "\xff"
	tests := []struct {
		name    string
		provoke func(t *testing.T) error
		field   string
		want    error
		cause   error
	}{
		{"NewWriter tool_id not valid UTF-8", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) { m.ToolID = bad }, nil)
		}, "tool_id", ErrFieldValue, tlv.ErrUTF8},
		{"NewWriter device_id above 2^63-1", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) { m.DeviceID = new(uint64(1 << 63)) }, nil)
		}, "device_id", ErrFieldValue, tlv.ErrLimit},
		{"NewWriter nested hsms_timers above 2^63-1", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) { m.HSMSTimers = &HSMSTimers{T3Ms: new(uint64(1 << 63))} }, nil)
		}, "hsms_timers.T3", ErrFieldValue, tlv.ErrLimit},
		{"NewWriter Unknown entry of tag zero", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) { m.Unknown = []RawEntry{{Tag: 0, Type: uint8(tlv.TypeUTF8), Value: []byte("x")}} }, nil)
		}, "0x0000", ErrFieldValue, tlv.ErrZeroTag},
		{"NewWriter Unknown entry of the wrong length", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) { m.Unknown = []RawEntry{{Tag: 0x7F00, Type: uint8(tlv.TypeU64), Value: []byte("x")}} }, nil)
		}, "0x7F00", ErrFieldLength, tlv.ErrLength},
		{"NewWriter coverage Unknown entry of tag zero", func(*testing.T) error {
			return newWriterErr(func(m *PackMeta) {
				m.Coverage = []Coverage{{Unknown: []RawEntry{{Tag: 0, Type: uint8(tlv.TypeU64), Value: make([]byte, 8)}}}}
			}, nil)
		}, "coverage.0x0000", ErrFieldValue, tlv.ErrZeroTag},
		{"AppendEvent detail not valid UTF-8", func(t *testing.T) error {
			w, _ := newSeg(t, nil)
			return w.AppendEvent(t.Context(), segAt(time.Second), 1, &TransportEvent{Event: EventStateTransition, Detail: &bad})
		}, "detail", ErrFieldValue, tlv.ErrUTF8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.provoke(t)
			requireRefused(t, err, tt.want)
			requireFieldError(t, err, tt.field, tt.want, tt.cause)
			require.NotErrorIs(t, err, ErrCorrupt)
		})
	}

	t.Run("NewSegmentWriter classifier name not valid UTF-8", func(t *testing.T) {
		t.Parallel()

		sink := newMemSink()
		opts := segOptions(sink)
		opts.Classifier = &fakeClassifier{name: bad}
		w, err := NewSegmentWriter(t.Context(), opts)
		require.Nil(t, w)
		requireRefused(t, err, ErrFieldValue)
		requireFieldError(t, err, "classifier", ErrFieldValue, tlv.ErrUTF8)
		require.NotErrorIs(t, err, ErrCorrupt)
		assert.Zero(t, sink.calls[sinkCreate], "refused before the sink is called")
	})
}

// TestCodecErrorsEventRecord requires a writing call to refuse a transport-event or annotation record
// whose payload does not decode with ErrInvalidRecord, marked ErrRefused, and never with ErrCorrupt:
// the record is the caller's, not stored bytes.
func TestCodecErrorsEventRecord(t *testing.T) {
	t.Parallel()

	detail := tlv.Entry{Tag: tagDetail, Type: tlv.TypeUTF8, Value: []byte{0xFF}}
	payloads := []struct {
		name    string
		kind    Kind
		payload []byte
	}{
		{"transport event, not TLV framing", KindTransportEvent, []byte{0xFF}},
		{"transport event, invalid UTF-8", KindTransportEvent, encodeTLV([]tlv.Entry{tlv.U8Entry(tagEvent, uint8(EventStateTransition)), detail})},
		{"annotation, without annotation_kind", KindAnnotation, encodeTLV([]tlv.Entry{tlv.UTF8Entry(tagText, "note")})},
	}
	record := func(kind Kind, payload []byte) Record {
		return Record{
			Seq: 0, TSUTCNs: blockTestHour + 1, Kind: kind, Dir: DirLocal,
			Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable, Payload: payload,
		}
	}
	for _, p := range payloads {
		t.Run("Writer.Append "+p.name, func(t *testing.T) {
			t.Parallel()

			w := refusalWriter(t, io.Discard, nil)
			r := record(p.kind, p.payload)
			err := w.Append(&r)
			requireRefused(t, err, ErrInvalidRecord)
			require.NotErrorIs(t, err, ErrCorrupt)
		})
		t.Run("SegmentWriter.Append "+p.name, func(t *testing.T) {
			t.Parallel()

			w, _ := newSeg(t, nil)
			r := record(p.kind, p.payload)
			r.TSUTCNs = segAt(time.Second).UnixNano()
			err := w.Append(t.Context(), &r)
			requireRefused(t, err, ErrInvalidRecord)
			require.NotErrorIs(t, err, ErrCorrupt)
		})
	}
}

// TestCodecErrorsStandalone requires the standalone codecs to report an entry's fault as a *FieldError,
// a decode also wrapping ErrCorrupt and an encode never, and a nil or unparsable argument as ErrInvalidArgument.
func TestCodecErrorsStandalone(t *testing.T) {
	t.Parallel()

	t.Run("EncodeDescriptor nil", func(t *testing.T) {
		t.Parallel()

		_, err := EncodeDescriptor(nil)
		require.ErrorIs(t, err, ErrInvalidArgument)
		require.NotErrorIs(t, err, ErrRefused)
		require.NotErrorIs(t, err, ErrCorrupt)
	})
	t.Run("ParseUUID", func(t *testing.T) {
		t.Parallel()

		for _, s := range []string{"", "not-a-uuid", "f47ac10b-58cc-4372-a567-0e02b2c3d47g"} {
			u, err := ParseUUID(s)
			require.ErrorIs(t, err, ErrInvalidArgument, "%q", s)
			require.NotErrorIs(t, err, ErrRefused)
			assert.True(t, u.IsZero())
		}
	})

	encodes := []struct {
		name   string
		encode func() error
		field  string
		want   error
		cause  error
	}{
		{"EncodeDescriptor missing capture_origin_utc_ns", func() error {
			m := descriptorMeta()
			m.CaptureOriginUTCNs = nil
			_, err := EncodeDescriptor(m)

			return err
		}, "capture_origin_utc_ns", ErrRequiredTag, tlv.ErrMissing},
		{"EncodeDescriptor site_id not valid UTF-8", func() error {
			m := descriptorMeta()
			m.SiteID = new("\xff")
			_, err := EncodeDescriptor(m)

			return err
		}, "site_id", ErrFieldValue, tlv.ErrUTF8},
		{"PackMeta.MarshalBinary tool_id not valid UTF-8", func() error {
			m := repairTestMeta()
			m.ToolID = "\xff"
			_, err := m.MarshalBinary()

			return err
		}, "tool_id", ErrFieldValue, tlv.ErrUTF8},
		{"TransportEvent.MarshalBinary primary_function above 2^63-1", func() error {
			_, err := (&TransportEvent{Event: EventStateTransition, PrimaryFunction: new(uint64(1 << 63))}).MarshalBinary()
			return err
		}, "primary_function", ErrFieldValue, tlv.ErrLimit},
		{"Annotation.MarshalBinary text not valid UTF-8", func() error {
			_, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: new("\xff")}).MarshalBinary()
			return err
		}, "text", ErrFieldValue, tlv.ErrUTF8},
	}
	for _, tt := range encodes {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.encode()
			requireFieldError(t, err, tt.field, tt.want, tt.cause)
			require.NotErrorIs(t, err, ErrCorrupt, "an encode never reports corrupt data")
			require.NotErrorIs(t, err, ErrRefused, "not a writing call")
		})
	}

	event := func(entries ...tlv.Entry) []byte {
		return encodeTLV(append([]tlv.Entry{tlv.U8Entry(tagEvent, uint8(EventStateTransition))}, entries...))
	}
	thinning := tlv.NestedEntry(tagThinning, []tlv.Entry{
		tlv.U64Entry(tagThinningStream, 1), tlv.U64Entry(tagThinningFunction, 3), tlv.UTF8Entry(tagThinningSource, "s"), tlv.U64Entry(4, 1),
	})
	decodes := []struct {
		name   string
		decode func(t *testing.T) error
		field  string
		want   error
		cause  error
	}{
		{"UnmarshalPackMeta nil", func(*testing.T) error {
			_, err := UnmarshalPackMeta(nil)
			return err
		}, "tool_id", ErrRequiredTag, tlv.ErrMissing},
		{"UnmarshalPackMeta redaction without seq", func(t *testing.T) error {
			b, err := repairTestMeta().MarshalBinary()
			require.NoError(t, err)
			b = tlv.AppendEntry(b, tlv.NestedEntry(tagRedaction, []tlv.Entry{
				tlv.BytesEntry(tagRedactionEntryMaskedRanges, nil), tlv.UTF8Entry(tagRedactionEntryDomain, "d"), tlv.BytesEntry(tagRedactionEntryDigest, nil),
			}))
			_, err = UnmarshalPackMetaMinor(b, 1)

			return err
		}, "redaction.seq", ErrRequiredTag, tlv.ErrMissing},
		{"UnmarshalTransportEvent detail not valid UTF-8", func(*testing.T) error {
			_, err := UnmarshalTransportEvent(event(tlv.Entry{Tag: tagDetail, Type: tlv.TypeUTF8, Value: []byte{0xFF}}))
			return err
		}, "detail", ErrFieldValue, tlv.ErrUTF8},
		{"UnmarshalTransportEvent primary_system_bytes of 3 bytes", func(*testing.T) error {
			_, err := UnmarshalTransportEvent(event(tlv.BytesEntry(tagPrimarySystemBytes, []byte{1, 2, 3})))
			return err
		}, "primary_system_bytes", ErrFieldLength, nil},
		{"UnmarshalAnnotation without annotation_kind", func(*testing.T) error {
			_, err := UnmarshalAnnotation(encodeTLV([]tlv.Entry{tlv.UTF8Entry(tagText, "note")}))
			return err
		}, "annotation_kind", ErrRequiredTag, tlv.ErrMissing},
		{"DecodeDescriptor unknown tag", func(t *testing.T) error {
			_, err := DecodeDescriptor(encodeTLV(append(descriptorEntries(t), tlv.U64Entry(0x7F00, 1))))
			return err
		}, "0x7F00", ErrFieldValue, nil},
		{"DecodeDescriptor consumer-owned tag", func(t *testing.T) error {
			_, err := DecodeDescriptor(encodeTLV(append(descriptorEntries(t), tlv.UTF8Entry(tagWriter, "w"))))
			return err
		}, "writer", ErrFieldValue, nil},
		{"DecodeDescriptor unknown nested thinning tag", func(t *testing.T) error {
			_, err := DecodeDescriptor(encodeTLV(append(descriptorEntries(t), thinning)))
			return err
		}, "thinning.0x0004", ErrFieldValue, nil},
		{"DecodeDescriptor missing tool_id", func(*testing.T) error {
			_, err := DecodeDescriptor(nil)
			return err
		}, "tool_id", ErrRequiredTag, tlv.ErrMissing},
	}
	for _, tt := range decodes {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.decode(t)
			requireFieldError(t, err, tt.field, tt.want, tt.cause)
			require.ErrorIs(t, err, ErrCorrupt)
			require.NotErrorIs(t, err, ErrRefused)
		})
	}

	t.Run("UnmarshalAnnotation neither text nor raw", func(t *testing.T) {
		t.Parallel()

		_, err := UnmarshalAnnotation(encodeTLV([]tlv.Entry{tlv.U8Entry(tagAnnotationKind, uint8(AnnotationKindNote))}))
		require.ErrorIs(t, err, ErrAnnotationText)
		require.ErrorIs(t, err, ErrCorrupt)
	})
	t.Run("truncated", func(t *testing.T) {
		t.Parallel()

		b := event(tlv.UTF8Entry(tagDetail, "detail"))
		for _, cut := range [][]byte{b[:len(b)-1], b[:1]} {
			_, err := UnmarshalTransportEvent(cut)
			require.ErrorIs(t, err, ErrCorrupt)
			require.ErrorIs(t, err, tlv.ErrTruncated)
			var fe *FieldError
			require.NotErrorAs(t, err, &fe, "a list that ends inside an entry names no field")
		}
	})
}
