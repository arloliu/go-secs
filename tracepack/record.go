package tracepack

import "encoding/binary"

// Positions of the copy fields inside a data or control record payload:
// the 4-byte length prefix, then the 10-byte HSMS message header (SEMI E37 §8.2).
// Each end offset is the captured length at which the field becomes available.
const (
	copySessionIDEnd   = 6
	copyByte2End       = 7
	copyFunctionEnd    = 8
	copyPTypeEnd       = 9
	copySTypeEnd       = 10
	copySystemBytesEnd = 14

	copySessionIDOff   = 4
	copyByte2Off       = 6
	copyFunctionOff    = 7
	copyPTypeOff       = 8
	copySTypeOff       = 9
	copySystemBytesOff = 10

	// streamMask keeps bits 0-6 of HSMS header byte 2; bit 7 is the W bit.
	streamMask = 0x7F
	wBit       = 0x80
)

// Record is one record of a tracepack file: its record header fields and its payload
// (the tracepack format specification §7).
//
// Mono is carried as MonoNs and MonoPresent rather than a pointer,
// so building or reading a record never allocates for it.
// When MonoPresent is false the Writer stores mono_ns as 0 and sets quality.no-mono,
// so a record read back has MonoNs 0.
//
// The copy fields (SessionID, Stream, W, Function, PType, SType and SystemBytes)
// are positional copies of the HSMS header bytes of Payload for data and control records;
// SetHeaderCopies fills them and FieldValidity from Payload.
// The Writer zeroes every copy field whose FieldValidity bit is clear,
// and every copy field and FieldValidity of a record that is neither data nor control,
// because the tracepack format specification §7.2 requires them to be zero on write.
// It derives record_flags from W and MonoPresent,
// and owns three quality bits whatever Quality holds:
// QualityNoMono is set iff MonoPresent is false,
// QualityDecodeFailed is set iff DecodeStatus.Malformed(),
// and QualityCaptureBoundary is set iff the record is a transport event whose payload decodes to a capture-boundary event.
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
	// SystemBytes copies HSMS header bytes 6-9.
	SystemBytes [4]byte
	// SessionID copies HSMS header bytes 0-1, big-endian on the wire.
	SessionID uint16
	// Stream copies bits 0-6 of HSMS header byte 2.
	Stream uint8
	// W copies bit 7 of HSMS header byte 2 (record_flags.W).
	W bool
	// Function copies HSMS header byte 3.
	Function uint8
	// PType copies HSMS header byte 4.
	PType uint8
	// SType copies HSMS header byte 5.
	SType uint8
	// Quality is the stored quality bit set.
	Quality Quality
	// FieldValidity marks which copy fields were present in the captured bytes,
	// or, for a log conversion, which the source's metadata established (the tracepack format specification §7.2);
	// a copy field whose bit is clear is unavailable to a query even where Payload holds its bytes.
	FieldValidity FieldValidity
	// Payload is the record payload: the captured HSMS frame for data and control records,
	// a TLV body for transport-event and annotation records (the tracepack format specification §8).
	Payload []byte
}

// SetHeaderCopies fills r's copy fields and FieldValidity from r.Payload,
// following the positional copy rule of the tracepack format specification §7.2.
//
// For a data or control record, each copy field whose bytes lie inside Payload is copied
// and its FieldValidity bit set;
// a field of a short capture whose bytes are missing is zeroed and its bit cleared.
// The rule is identical for data and control records:
// for a control message Stream, W and Function hold the raw bits of HSMS header bytes 2 and 3.
// For any other kind every copy field and FieldValidity are zeroed.
func (r *Record) SetHeaderCopies() {
	r.SessionID, r.Stream, r.W, r.Function, r.PType, r.SType = 0, 0, false, 0, 0, 0
	r.SystemBytes = [4]byte{}
	r.FieldValidity = 0

	if !r.hasCopies() {
		return
	}

	p := r.Payload
	if len(p) >= copySessionIDEnd {
		r.SessionID = binary.BigEndian.Uint16(p[copySessionIDOff:copySessionIDEnd])
		r.FieldValidity |= FieldValiditySessionID
	}
	if len(p) >= copyByte2End {
		r.Stream = p[copyByte2Off] & streamMask
		r.W = p[copyByte2Off]&wBit != 0
		r.FieldValidity |= FieldValidityStreamAndW
	}
	if len(p) >= copyFunctionEnd {
		r.Function = p[copyFunctionOff]
		r.FieldValidity |= FieldValidityFunction
	}
	if len(p) >= copyPTypeEnd {
		r.PType = p[copyPTypeOff]
		r.FieldValidity |= FieldValidityPType
	}
	if len(p) >= copySTypeEnd {
		r.SType = p[copySTypeOff]
		r.FieldValidity |= FieldValiditySType
	}
	if len(p) >= copySystemBytesEnd {
		r.SystemBytes = [4]byte(p[copySystemBytesOff:copySystemBytesEnd])
		r.FieldValidity |= FieldValiditySystemBytes
	}
}

// hasCopies reports whether r's kind carries copy fields: only data and control records do.
func (r *Record) hasCopies() bool {
	return r.Kind == KindData || r.Kind == KindControl
}

// zeroUnavailableCopies zeroes every copy field whose FieldValidity bit is clear,
// as the tracepack format specification §7.2 requires on write.
func (r *Record) zeroUnavailableCopies() {
	v := r.FieldValidity
	if !v.Has(FieldValiditySessionID) {
		r.SessionID = 0
	}
	if !v.Has(FieldValidityStreamAndW) {
		r.Stream, r.W = 0, false
	}
	if !v.Has(FieldValidityFunction) {
		r.Function = 0
	}
	if !v.Has(FieldValidityPType) {
		r.PType = 0
	}
	if !v.Has(FieldValiditySType) {
		r.SType = 0
	}
	if !v.Has(FieldValiditySystemBytes) {
		r.SystemBytes = [4]byte{}
	}
}
