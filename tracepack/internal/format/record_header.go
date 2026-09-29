package format

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// RecordHeaderLen is the number of record header bytes format 1.0 defines, per the tracepack format specification §7.1.
//
// It is also the smallest record_header_len a block may declare:
// a larger value appends bytes a format 1.0 reader preserves without interpreting, the extension area.
const RecordHeaderLen = 44

// recordHeaderStructure names the record header in errors.
const recordHeaderStructure = "record header"

// Record header field offsets.
const (
	recordHeaderSeqOff        = 0
	recordHeaderPayloadLenOff = 28
)

// Record header bit-set masks, per the tracepack format specification §9.
const (
	// recordHeaderQualityMask keeps bits 0-2, 4 and 5; bits 3 and 6 are retired, bits 7-15 reserved.
	recordHeaderQualityMask uint16 = 0x0037
	// recordHeaderFieldValidityMask keeps bits 0-5; bits 6-7 are reserved.
	recordHeaderFieldValidityMask uint8 = 0x3F
	// recordHeaderRecordFlagsMask keeps bit 1, mono_present; bit 0 is retired, bits 2-7 reserved.
	recordHeaderRecordFlagsMask uint8 = 0x02
)

// RecordHeader is the decoded record header of the tracepack format specification §7.1:
// the bytes of one record gathered from its block's header section.
//
// Fields hold the raw values as stored: enums and bit sets are plain integers.
type RecordHeader struct {
	Seq           uint64
	TSUTCNs       int64
	MonoNs        int64
	Epoch         uint32
	PayloadLen    uint32
	TrailingBytes uint32
	Quality       uint16
	Kind          uint8
	Dir           uint8
	Fidelity      uint8
	DecodeStatus  uint8
	FieldValidity uint8
	RecordFlags   uint8
	// Extra holds the extension area, the record header bytes behind offset 44 that a later minor version appended,
	// so a block with a longer record_header_len keeps them for byte identity.
	// It is nil when the header is exactly RecordHeaderLen bytes long.
	Extra []byte
}

// AppendRecordHeader appends the encoding of h to dst and returns the extended slice:
// the 44 known bytes followed by h.Extra verbatim.
//
// It is a writer primitive, not the inverse of UnmarshalRecordHeader:
// a header read with nonzero reserved or retired bits is written back with them zeroed,
// so byte identity of stored records is compared on their raw bytes, never through a decode and re-encode.
// Quality, FieldValidity and RecordFlags are each masked to their defined bits (§9) before being written.
// The caller keeps RecordHeaderLen + len(h.Extra) within the u16 record_header_len of the block.
// AppendRecordHeader does not allocate when dst has room for the whole header.
func AppendRecordHeader(dst []byte, h *RecordHeader) []byte {
	dst = slices.Grow(dst, RecordHeaderLen+len(h.Extra))

	dst = binary.LittleEndian.AppendUint64(dst, h.Seq)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.TSUTCNs))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.MonoNs))
	dst = binary.LittleEndian.AppendUint32(dst, h.Epoch)
	dst = binary.LittleEndian.AppendUint32(dst, h.PayloadLen)
	dst = binary.LittleEndian.AppendUint32(dst, h.TrailingBytes)
	dst = binary.LittleEndian.AppendUint16(dst, h.Quality&recordHeaderQualityMask)
	dst = append(dst,
		h.Kind, h.Dir, h.Fidelity, h.DecodeStatus,
		h.FieldValidity&recordHeaderFieldValidityMask, h.RecordFlags&recordHeaderRecordFlagsMask,
	)

	return append(dst, h.Extra...)
}

// UnmarshalRecordHeader decodes one record header of recordHeaderLen bytes from the start of b,
// a record header already gathered from its block's header section (§6).
//
// recordHeaderLen is the record_header_len of the enclosing block, from its envelope;
// a value below RecordHeaderLen is reported as ErrCorrupt (tracepack format specification §6), with the pseudo-offset 0,
// because the length is a property of the block, not a record header field.
// UnmarshalRecordHeader reads the 44 known bytes and copies bytes 44 to recordHeaderLen into Extra (§7.1).
// Bytes of b after recordHeaderLen are not read.
// It checks that seq is at most MaxU64 and payload_len at most MaxLen32 (ErrLimit).
//
// The returned header never aliases b: Extra is a fresh copy, or nil when recordHeaderLen is RecordHeaderLen,
// and UnmarshalRecordHeader allocates only that copy.
//
// Returns:
//   - RecordHeader: the decoded header; the zero value on error.
//   - error: an *OffsetError wrapping ErrCorrupt, ErrShort or ErrLimit.
func UnmarshalRecordHeader(b []byte, recordHeaderLen int) (RecordHeader, error) {
	if err := checkRecordHeaderLen(recordHeaderStructure, 0, recordHeaderLen); err != nil {
		return RecordHeader{}, err
	}

	if err := checkShort(recordHeaderStructure, b, recordHeaderLen); err != nil {
		return RecordHeader{}, err
	}

	h := RecordHeader{
		Seq:           binary.LittleEndian.Uint64(b[0:8]),
		TSUTCNs:       int64(binary.LittleEndian.Uint64(b[8:16])),
		MonoNs:        int64(binary.LittleEndian.Uint64(b[16:24])),
		Epoch:         binary.LittleEndian.Uint32(b[24:28]),
		PayloadLen:    binary.LittleEndian.Uint32(b[28:32]),
		TrailingBytes: binary.LittleEndian.Uint32(b[32:36]),
		Quality:       binary.LittleEndian.Uint16(b[36:38]),
		Kind:          b[38],
		Dir:           b[39],
		Fidelity:      b[40],
		DecodeStatus:  b[41],
		FieldValidity: b[42],
		RecordFlags:   b[43],
	}

	u64s := [...]u64Field{{name: "seq", off: recordHeaderSeqOff, value: h.Seq}}
	if err := checkU64Fields(recordHeaderStructure, u64s[:]); err != nil {
		return RecordHeader{}, err
	}

	lens := [...]len32Field{{name: "payload_len", off: recordHeaderPayloadLenOff, value: h.PayloadLen}}
	if err := checkLen32Fields(recordHeaderStructure, lens[:]); err != nil {
		return RecordHeader{}, err
	}

	if recordHeaderLen > RecordHeaderLen {
		h.Extra = bytes.Clone(b[RecordHeaderLen:recordHeaderLen])
	}

	return h, nil
}
