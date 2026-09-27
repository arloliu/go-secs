package tracepack

import (
	"encoding"
	"fmt"

	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Transport-event record payload tag registry (the tracepack format specification §8).
const (
	tagEvent              uint16 = 0x0001
	tagPrevState          uint16 = 0x0002
	tagCurState           uint16 = 0x0003
	tagCause              uint16 = 0x0004
	tagCauseRaw           uint16 = 0x0005
	tagTimer              uint16 = 0x0006
	tagSocketRole         uint16 = 0x0007
	tagInferred           uint16 = 0x0008
	tagPrimarySessionID   uint16 = 0x0009
	tagPrimaryStream      uint16 = 0x000A
	tagPrimaryFunction    uint16 = 0x000B
	tagPrimarySystemBytes uint16 = 0x000C
	tagBoundaryKind       uint16 = 0x000D
	tagBoundarySeqFirst   uint16 = 0x000E
	tagBoundarySeqLast    uint16 = 0x000F
	tagGapStart           uint16 = 0x0010
	tagGapEnd             uint16 = 0x0011
	tagDetail             uint16 = 0x0012
	tagClockStepNs        uint16 = 0x0013
)

// Annotation record payload tag registry (the tracepack format specification §8).
const (
	tagAnnotationKind uint16 = 0x0001
	tagRefCaptureID   uint16 = 0x0002
	tagRefSeqFirst    uint16 = 0x0003
	tagRefSeqLast     uint16 = 0x0004
	tagText           uint16 = 0x0005
	tagRaw            uint16 = 0x0006
	tagSourceIndex    uint16 = 0x0007
	tagSourceOffset   uint16 = 0x0008
	tagSourceLen      uint16 = 0x0009
)

// primarySystemBytesLen is the byte length of a transport-event primary_system_bytes value.
const primarySystemBytesLen = 4

// TransportEvent is the payload of a transport-event record: one lifecycle event a writer observed or derived,
// per the tag registry of the tracepack format specification §8.
//
// A pointer field is present on the wire only when non-nil.
// PrimarySystemBytes is a fixed-size array rather than a pointer,
// so its presence is carried separately in PrimarySystemBytesPresent.
// Unknown preserves every entry whose tag TransportEvent does not recognize, private tags included,
// so MarshalBinary reproduces them on a round trip.
type TransportEvent struct {
	Event                     Event
	PrevState                 *State
	CurState                  *State
	Cause                     *Cause
	CauseRaw                  *string
	Timer                     *Timer
	SocketRole                *SocketRole
	Inferred                  *bool
	PrimarySessionID          *uint64
	PrimaryStream             *uint64
	PrimaryFunction           *uint64
	PrimarySystemBytes        [4]byte
	PrimarySystemBytesPresent bool
	BoundaryKind              *BoundaryKind
	BoundarySeqFirst          *uint64
	BoundarySeqLast           *uint64
	GapStart                  *int64
	GapEnd                    *int64
	Detail                    *string
	ClockStepNs               *int64
	Unknown                   []RawEntry
}

var _ encoding.BinaryMarshaler = (*TransportEvent)(nil)

// Annotation is the payload of an annotation record:
// free text, or a note about bytes the writer could not attribute to a record,
// per the tag registry of the tracepack format specification §8.
//
// Exactly one of Text and Raw is present, on both MarshalBinary and UnmarshalAnnotation.
// Raw's presence is nil versus non-nil, not length: a zero-length Raw is present,
// so UnmarshalAnnotation gives it the non-nil empty slice rather than nil.
// Unknown preserves every entry whose tag Annotation does not recognize, private tags included,
// so MarshalBinary reproduces them on a round trip.
type Annotation struct {
	AnnotationKind AnnotationKind
	RefCaptureID   *UUID
	RefSeqFirst    *uint64
	RefSeqLast     *uint64
	Text           *string
	Raw            []byte
	SourceIndex    *uint64
	SourceOffset   *uint64
	SourceLen      *uint64
	Unknown        []RawEntry
}

var _ encoding.BinaryMarshaler = (*Annotation)(nil)

// transportEventSetters decodes one known transport-event tag into t, keyed by tag.
// A tag absent from this map is unknown or private, and is preserved in TransportEvent.Unknown instead.
var transportEventSetters = map[uint16]func(t *TransportEvent, e tlv.Entry) error{
	tagPrevState:        setField(u8As[State], func(t *TransportEvent, v State) { t.PrevState = &v }),
	tagCurState:         setField(u8As[State], func(t *TransportEvent, v State) { t.CurState = &v }),
	tagCause:            setField(u8As[Cause], func(t *TransportEvent, v Cause) { t.Cause = &v }),
	tagCauseRaw:         setField(tlv.Entry.UTF8, func(t *TransportEvent, v string) { t.CauseRaw = &v }),
	tagTimer:            setField(u8As[Timer], func(t *TransportEvent, v Timer) { t.Timer = &v }),
	tagSocketRole:       setField(u8As[SocketRole], func(t *TransportEvent, v SocketRole) { t.SocketRole = &v }),
	tagInferred:         setField(tlv.Entry.Bool, func(t *TransportEvent, v bool) { t.Inferred = &v }),
	tagPrimarySessionID: setField(tlv.Entry.U64, func(t *TransportEvent, v uint64) { t.PrimarySessionID = &v }),
	tagPrimaryStream:    setField(tlv.Entry.U64, func(t *TransportEvent, v uint64) { t.PrimaryStream = &v }),
	tagPrimaryFunction:  setField(tlv.Entry.U64, func(t *TransportEvent, v uint64) { t.PrimaryFunction = &v }),
	tagPrimarySystemBytes: func(t *TransportEvent, e tlv.Entry) error {
		b, err := e.Bytes()
		if err != nil {
			return err
		}
		if len(b) != primarySystemBytesLen {
			return fmt.Errorf("tracepack: primary_system_bytes holds %d bytes: %w", len(b), ErrFieldLength)
		}
		copy(t.PrimarySystemBytes[:], b)
		t.PrimarySystemBytesPresent = true

		return nil
	},
	tagBoundaryKind:     setField(u8As[BoundaryKind], func(t *TransportEvent, v BoundaryKind) { t.BoundaryKind = &v }),
	tagBoundarySeqFirst: setField(tlv.Entry.U64, func(t *TransportEvent, v uint64) { t.BoundarySeqFirst = &v }),
	tagBoundarySeqLast:  setField(tlv.Entry.U64, func(t *TransportEvent, v uint64) { t.BoundarySeqLast = &v }),
	tagGapStart:         setField(tlv.Entry.I64, func(t *TransportEvent, v int64) { t.GapStart = &v }),
	tagGapEnd:           setField(tlv.Entry.I64, func(t *TransportEvent, v int64) { t.GapEnd = &v }),
	tagDetail:           setField(tlv.Entry.UTF8, func(t *TransportEvent, v string) { t.Detail = &v }),
	tagClockStepNs:      setField(tlv.Entry.I64, func(t *TransportEvent, v int64) { t.ClockStepNs = &v }),
}

// annotationSetters decodes one known annotation tag into a, keyed by tag.
// A tag absent from this map is unknown or private, and is preserved in Annotation.Unknown instead.
var annotationSetters = map[uint16]func(a *Annotation, e tlv.Entry) error{
	tagRefCaptureID: setField(uuidOf, func(a *Annotation, v UUID) { a.RefCaptureID = &v }),
	tagRefSeqFirst:  setField(tlv.Entry.U64, func(a *Annotation, v uint64) { a.RefSeqFirst = &v }),
	tagRefSeqLast:   setField(tlv.Entry.U64, func(a *Annotation, v uint64) { a.RefSeqLast = &v }),
	tagText:         setField(tlv.Entry.UTF8, func(a *Annotation, v string) { a.Text = &v }),
	tagRaw: setField(tlv.Entry.Bytes, func(a *Annotation, v []byte) {
		// append([]byte{}, v...) rather than append([]byte(nil), v...):
		// a present-but-empty raw tag must decode to a non-nil Raw, so validateTextRaw sees it as present.
		a.Raw = append([]byte{}, v...)
	}),
	tagSourceIndex:  setField(tlv.Entry.U64, func(a *Annotation, v uint64) { a.SourceIndex = &v }),
	tagSourceOffset: setField(tlv.Entry.U64, func(a *Annotation, v uint64) { a.SourceOffset = &v }),
	tagSourceLen:    setField(tlv.Entry.U64, func(a *Annotation, v uint64) { a.SourceLen = &v }),
}

// MarshalBinary encodes t as the TLV bytes of a transport-event record payload (the tracepack format specification §8).
// Every RawEntry in t.Unknown is re-encoded after the typed fields.
// It rejects any value UnmarshalTransportEvent would reject: a string that is not valid UTF-8, or a u64 above 2^63-1.
//
// Returns:
//   - []byte: the encoded payload.
//   - error: a *FieldError wrapping ErrReservedTag for an Unknown entry that names a tag this package already encodes,
//     or an error naming the first value the decoder would reject.
func (t *TransportEvent) MarshalBinary() ([]byte, error) {
	entries := []tlv.Entry{tlv.U8Entry(tagEvent, uint8(t.Event))}
	entries = t.appendOptionalEntries(entries)

	entries, err := appendUnknownEntries(entries, t.Unknown, tlv.TransportEvent)
	if err != nil {
		return nil, err
	}

	return encodeEntries(entries, tlv.TransportEvent, "transport-event")
}

// MarshalBinary encodes a as the TLV bytes of an annotation record payload (the tracepack format specification §8).
// Every RawEntry in a.Unknown is re-encoded after the typed fields.
// It rejects any value UnmarshalAnnotation would reject: a string that is not valid UTF-8, or a u64 above 2^63-1.
//
// Returns:
//   - []byte: the encoded payload.
//   - error: ErrAnnotationText if Text and Raw are not exactly one of the two,
//     a *FieldError wrapping ErrReservedTag for an Unknown entry that names a tag this package already encodes,
//     or an error naming the first value the decoder would reject.
func (a *Annotation) MarshalBinary() ([]byte, error) {
	if err := a.validateTextRaw(); err != nil {
		return nil, err
	}

	entries := []tlv.Entry{tlv.U8Entry(tagAnnotationKind, uint8(a.AnnotationKind))}
	entries = a.appendOptionalEntries(entries)

	entries, err := appendUnknownEntries(entries, a.Unknown, tlv.Annotation)
	if err != nil {
		return nil, err
	}

	return encodeEntries(entries, tlv.Annotation, "annotation")
}

// UnmarshalTransportEvent decodes a transport-event record payload (the tracepack format specification §8) from b.
//
// It decodes b and checks it against the transport-event tag registry with tlv.Decode and tlv.Validate,
// which enforce the required event tag and every value type.
// An entry whose tag is not in the registry is preserved in the returned TransportEvent's Unknown field.
//
// Returns:
//   - *TransportEvent: the decoded payload; nil on error.
//   - error: non-nil if b is not valid TLV or fails the registry check.
func UnmarshalTransportEvent(b []byte) (*TransportEvent, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("tracepack: decode transport-event: %w", err)
	}
	if err := tlv.Validate(entries, tlv.TransportEvent); err != nil {
		return nil, fmt.Errorf("tracepack: validate transport-event: %w", err)
	}

	t := &TransportEvent{}
	for _, e := range entries {
		if e.Tag == tagEvent {
			v, err := u8As[Event](e)
			if err != nil {
				return nil, fmt.Errorf("tracepack: event: %w", err)
			}
			t.Event = v

			continue
		}

		if set, known := transportEventSetters[e.Tag]; known {
			if err := set(t, e); err != nil {
				return nil, err
			}

			continue
		}

		t.Unknown = append(t.Unknown, newRawEntry(e))
	}

	return t, nil
}

// UnmarshalAnnotation decodes an annotation record payload (the tracepack format specification §8) from b.
//
// It decodes b and checks it against the annotation tag registry with tlv.Decode and tlv.Validate,
// which enforce the required annotation_kind tag and every value type,
// then rejects a payload whose text and raw tags are not exactly one of the two.
// An entry whose tag is not in the registry is preserved in the returned Annotation's Unknown field.
//
// Returns:
//   - *Annotation: the decoded payload; nil on error.
//   - error: non-nil if b is not valid TLV, fails the registry check, or violates the exactly-one-of rule.
func UnmarshalAnnotation(b []byte) (*Annotation, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, fmt.Errorf("tracepack: decode annotation: %w", err)
	}
	if err := tlv.Validate(entries, tlv.Annotation); err != nil {
		return nil, fmt.Errorf("tracepack: validate annotation: %w", err)
	}

	a := &Annotation{}
	for _, e := range entries {
		if e.Tag == tagAnnotationKind {
			v, err := u8As[AnnotationKind](e)
			if err != nil {
				return nil, fmt.Errorf("tracepack: annotation_kind: %w", err)
			}
			a.AnnotationKind = v

			continue
		}

		if set, known := annotationSetters[e.Tag]; known {
			if err := set(a, e); err != nil {
				return nil, err
			}

			continue
		}

		a.Unknown = append(a.Unknown, newRawEntry(e))
	}

	if err := a.validateTextRaw(); err != nil {
		return nil, err
	}

	return a, nil
}

// validateTextRaw checks that exactly one of a.Text and a.Raw is present (the tracepack format specification §8).
func (a *Annotation) validateTextRaw() error {
	hasText := a.Text != nil
	hasRaw := a.Raw != nil
	if hasText == hasRaw {
		return ErrAnnotationText
	}

	return nil
}

// appendOptionalEntries appends the entries of t's optional tags that are present.
func (t *TransportEvent) appendOptionalEntries(dst []tlv.Entry) []tlv.Entry {
	return appendPresent(dst, []optionalEntry{
		{t.PrevState != nil, func() tlv.Entry { return tlv.U8Entry(tagPrevState, uint8(*t.PrevState)) }},
		{t.CurState != nil, func() tlv.Entry { return tlv.U8Entry(tagCurState, uint8(*t.CurState)) }},
		{t.Cause != nil, func() tlv.Entry { return tlv.U8Entry(tagCause, uint8(*t.Cause)) }},
		{t.CauseRaw != nil, func() tlv.Entry { return tlv.UTF8Entry(tagCauseRaw, *t.CauseRaw) }},
		{t.Timer != nil, func() tlv.Entry { return tlv.U8Entry(tagTimer, uint8(*t.Timer)) }},
		{t.SocketRole != nil, func() tlv.Entry { return tlv.U8Entry(tagSocketRole, uint8(*t.SocketRole)) }},
		{t.Inferred != nil, func() tlv.Entry { return tlv.BoolEntry(tagInferred, *t.Inferred) }},
		{t.PrimarySessionID != nil, func() tlv.Entry { return tlv.U64Entry(tagPrimarySessionID, *t.PrimarySessionID) }},
		{t.PrimaryStream != nil, func() tlv.Entry { return tlv.U64Entry(tagPrimaryStream, *t.PrimaryStream) }},
		{t.PrimaryFunction != nil, func() tlv.Entry { return tlv.U64Entry(tagPrimaryFunction, *t.PrimaryFunction) }},
		{t.PrimarySystemBytesPresent, func() tlv.Entry { return tlv.BytesEntry(tagPrimarySystemBytes, t.PrimarySystemBytes[:]) }},
		{t.BoundaryKind != nil, func() tlv.Entry { return tlv.U8Entry(tagBoundaryKind, uint8(*t.BoundaryKind)) }},
		{t.BoundarySeqFirst != nil, func() tlv.Entry { return tlv.U64Entry(tagBoundarySeqFirst, *t.BoundarySeqFirst) }},
		{t.BoundarySeqLast != nil, func() tlv.Entry { return tlv.U64Entry(tagBoundarySeqLast, *t.BoundarySeqLast) }},
		{t.GapStart != nil, func() tlv.Entry { return tlv.I64Entry(tagGapStart, *t.GapStart) }},
		{t.GapEnd != nil, func() tlv.Entry { return tlv.I64Entry(tagGapEnd, *t.GapEnd) }},
		{t.Detail != nil, func() tlv.Entry { return tlv.UTF8Entry(tagDetail, *t.Detail) }},
		{t.ClockStepNs != nil, func() tlv.Entry { return tlv.I64Entry(tagClockStepNs, *t.ClockStepNs) }},
	})
}

// appendOptionalEntries appends the entries of a's optional tags that are present.
func (a *Annotation) appendOptionalEntries(dst []tlv.Entry) []tlv.Entry {
	return appendPresent(dst, []optionalEntry{
		{a.RefCaptureID != nil, func() tlv.Entry { return tlv.UUIDEntry(tagRefCaptureID, [16]byte(*a.RefCaptureID)) }},
		{a.RefSeqFirst != nil, func() tlv.Entry { return tlv.U64Entry(tagRefSeqFirst, *a.RefSeqFirst) }},
		{a.RefSeqLast != nil, func() tlv.Entry { return tlv.U64Entry(tagRefSeqLast, *a.RefSeqLast) }},
		{a.Text != nil, func() tlv.Entry { return tlv.UTF8Entry(tagText, *a.Text) }},
		{a.Raw != nil, func() tlv.Entry { return tlv.BytesEntry(tagRaw, a.Raw) }},
		{a.SourceIndex != nil, func() tlv.Entry { return tlv.U64Entry(tagSourceIndex, *a.SourceIndex) }},
		{a.SourceOffset != nil, func() tlv.Entry { return tlv.U64Entry(tagSourceOffset, *a.SourceOffset) }},
		{a.SourceLen != nil, func() tlv.Entry { return tlv.U64Entry(tagSourceLen, *a.SourceLen) }},
	})
}
