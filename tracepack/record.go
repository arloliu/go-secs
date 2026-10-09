package tracepack

import "encoding/binary"

// Positions of the HSMS header fields inside a data or control record payload:
// the 4-byte length prefix, then the 10-byte HSMS message header (SEMI E37 §8.2).
// Each end offset is the captured length at which the field's bytes are all present
// (the tracepack format specification §7.2).
const (
	fieldSessionIDEnd   = 6
	fieldByte6End       = 7
	fieldFunctionEnd    = 8
	fieldPTypeEnd       = 9
	fieldSTypeEnd       = 10
	fieldSystemBytesEnd = 14

	fieldSessionIDOff   = 4
	fieldByte6Off       = 6
	fieldFunctionOff    = 7
	fieldPTypeOff       = 8
	fieldSTypeOff       = 9
	fieldSystemBytesOff = 10

	// streamMask keeps bits 0-6 of payload byte 6; bit 7 is the W bit.
	streamMask = 0x7F
	wBit       = 0x80
)

// Record is one record of a tracepack file: its record header fields and its payload
// (the tracepack format specification §7).
//
// The record header holds no field of the HSMS message header:
// the payload of a data or control record holds the whole captured frame,
// and HSMSHeader reads SessionID, S/F, W, PType, SType and System Bytes from it.
// FieldValidity says which of those fields are real (the tracepack format specification §7.2).
//
// Mono is carried as MonoNs and MonoPresent rather than a pointer,
// so building or reading a record never allocates for it.
// When MonoPresent is false the Writer stores mono_ns as 0, so a record read back has MonoNs 0.
//
// The Writer stores FieldValidity as 0 for a record that is neither data nor control,
// derives record_flags from MonoPresent,
// and owns the quality bit QualityCaptureBoundary, set iff the record is a transport event whose payload decodes to a capture-boundary event.
// It adds QualityCorrelationIncomplete to a record of epoch 0 (the tracepack format specification I-7),
// and keeps the bit the caller set on any other epoch.
// QualityRedacted is accepted only in an extract whose file header sets redaction-present.
type Record struct {
	// Seq is the capture-scoped record number;
	// a Writer with WriterOptions.AssignSeq assigns it.
	Seq uint64
	// TSUTCNs is the wall clock at the observation point, in nanoseconds since the Unix epoch.
	TSUTCNs int64
	// MonoNs is the elapsed monotonic time since capture_origin_mono_ns; valid iff MonoPresent.
	MonoNs int64
	// MonoPresent reports that MonoNs is valid (record_flags.mono_present).
	MonoPresent bool
	// Epoch is the transport connection number within the capture; 0 means unknown.
	Epoch uint32
	// Kind is the record kind.
	Kind Kind
	// Dir is the message direction.
	Dir Dir
	// Fidelity says how faithfully Payload reproduces the transmitted bytes.
	Fidelity Fidelity
	// DecodeStatus is the classifier's judgement over Payload.
	DecodeStatus DecodeStatus
	// TrailingBytes counts the message-text bytes after the first complete SECS-II item.
	TrailingBytes uint32
	// Quality is the stored quality bit set.
	Quality Quality
	// FieldValidity marks which HSMS header fields of Payload are real: present in the captured bytes,
	// or, for a log conversion, established from the source's metadata (the tracepack format specification §7.2).
	// A field whose bit is clear is unavailable to a query even where Payload holds its bytes.
	// A Writer rejects a data or control record with a set bit whose bytes Payload lacks.
	FieldValidity FieldValidity
	// Payload is the record payload: the captured HSMS frame for data and control records,
	// a TLV body for transport-event and annotation records (the tracepack format specification §8).
	Payload []byte
}

// HSMSHeader is the HSMS message header of a data or control record, read from its payload
// (the tracepack format specification §7.2).
//
// Each field whose bytes the payload holds carries the payload's value, whatever field_validity says;
// a field whose bytes are missing reads as zero.
// Available says which fields a predicate may use: a field is available only when its field_validity bit is set
// and the payload holds all of its bytes, so a log conversion's placeholder bytes are visible but never available.
type HSMSHeader struct {
	// SessionID is payload bytes 4-5, big-endian: SessionID, or DeviceID for normalised SECS-I, as observed.
	SessionID uint16
	// Stream is bits 0-6 of payload byte 6.
	Stream uint8
	// W is bit 7 of payload byte 6.
	W bool
	// Function is payload byte 7.
	Function uint8
	// PType is payload byte 8, the full byte value.
	PType uint8
	// SType is payload byte 9, the full byte value.
	SType uint8
	// SystemBytes is payload bytes 10-13, as on the wire.
	SystemBytes [4]byte
	// Available holds the FieldValidity bit of every available field; Stream and W share one bit.
	Available FieldValidity
}

// HSMSHeader returns the HSMS message header fields of r's payload and which of them are available
// (the tracepack format specification §7.2).
//
// A record that is neither data nor control has no HSMS frame: every field reads as zero and none is available.
// Every field is read at its position, for data and control records alike (the tracepack format specification §7.2):
// for a control message, Stream, W and Function hold the raw bits of payload bytes 6 and 7,
// which carry its status or reason codes (SEMI E37 §8.3),
// and a predicate on Stream, W or Function tests those bits as they are stored
// (the tracepack semantics specification §7.4).
// Interpreting them according to SType is the consumer's, not the predicate's.
func (r *Record) HSMSHeader() HSMSHeader {
	var h HSMSHeader
	if !r.hasHSMSFrame() {
		return h
	}

	p := r.Payload
	if len(p) >= fieldSessionIDEnd {
		h.SessionID = binary.BigEndian.Uint16(p[fieldSessionIDOff:fieldSessionIDEnd])
	}
	if len(p) >= fieldByte6End {
		h.Stream = p[fieldByte6Off] & streamMask
		h.W = p[fieldByte6Off]&wBit != 0
	}
	if len(p) >= fieldFunctionEnd {
		h.Function = p[fieldFunctionOff]
	}
	if len(p) >= fieldPTypeEnd {
		h.PType = p[fieldPTypeOff]
	}
	if len(p) >= fieldSTypeEnd {
		h.SType = p[fieldSTypeOff]
	}
	if len(p) >= fieldSystemBytesEnd {
		h.SystemBytes = [4]byte(p[fieldSystemBytesOff:fieldSystemBytesEnd])
	}
	h.Available = r.FieldValidity & capturedFields(len(p))

	return h
}

// SetCapturedFieldValidity sets r.FieldValidity to exactly the HSMS header fields whose bytes r.Payload holds,
// for a data or control record, and to 0 for any other kind:
// the field_validity a writer of raw captures records (the tracepack format specification §7.2).
// A log converter clears the bits of identities its source did not carry instead (the tracepack storage specification §7).
func (r *Record) SetCapturedFieldValidity() {
	r.FieldValidity = 0
	if r.hasHSMSFrame() {
		r.FieldValidity = capturedFields(len(r.Payload))
	}
}

// hasHSMSFrame reports whether r's kind carries an HSMS frame in its payload: only data and control records do.
func (r *Record) hasHSMSFrame() bool {
	return r.Kind == KindData || r.Kind == KindControl
}

// capturedFields returns the field_validity bits of the HSMS header fields whose bytes a payload of n bytes holds.
func capturedFields(n int) FieldValidity {
	var v FieldValidity
	for _, f := range [...]struct {
		bit FieldValidity
		end int
	}{
		{FieldValiditySessionID, fieldSessionIDEnd},
		{FieldValidityStreamAndW, fieldByte6End},
		{FieldValidityFunction, fieldFunctionEnd},
		{FieldValidityPType, fieldPTypeEnd},
		{FieldValiditySType, fieldSTypeEnd},
		{FieldValiditySystemBytes, fieldSystemBytesEnd},
	} {
		if n >= f.end {
			v |= f.bit
		}
	}

	return v
}
