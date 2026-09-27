package format

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// EnvelopeLen is the length of the block envelope that precedes every block body,
// per the tracepack format specification §6.
const EnvelopeLen = 40

// EnvelopeMagic is the block envelope magic: 54 50 4B 42.
const EnvelopeMagic = "TPKB"

// envelopeStructure names the block envelope in errors.
const envelopeStructure = "block envelope"

// Block envelope field offsets and widths.
const (
	envelopeRecordHeaderLenOff = 6
	envelopeBodyLenOff         = 8
	envelopeUncompressedLenOff = 12
	envelopeRecordCountOff     = 16
	envelopeFirstSeqOff        = 24
	envelopeTailReservedLen    = 4
	envelopeCRCOff             = 36
)

// BlockEnvelope is the decoded block envelope of the tracepack format specification §6.
//
// Fields hold the raw values as stored; the reserved bytes 5 and 32-35 are not represented.
// Codec keeps any value, including one the reader does not support:
// an unknown codec is not an envelope error, because the block is reported unreadable when its body is decoded.
type BlockEnvelope struct {
	Codec           uint8
	RecordHeaderLen uint16
	BodyLen         uint32
	UncompressedLen uint32
	RecordCount     uint32
	BodyCRC         uint32
	FirstSeq        uint64
	// EnvelopeCRC is the CRC over bytes 0-35 as read.
	// AppendBlockEnvelope ignores it and writes the computed CRC.
	EnvelopeCRC uint32
}

// AppendBlockEnvelope appends the 40-byte encoding of e to dst and returns the extended slice.
//
// Named fields are written verbatim; the reserved bytes 5 and 32-35 are written as zero,
// and envelope_crc is computed over bytes 0-35, ignoring e.EnvelopeCRC and leaving e unchanged.
// AppendBlockEnvelope does not validate e.
// It does not allocate when dst has room for EnvelopeLen more bytes.
func AppendBlockEnvelope(dst []byte, e *BlockEnvelope) []byte {
	dst = slices.Grow(dst, EnvelopeLen)
	start := len(dst)

	dst = append(dst, EnvelopeMagic...)
	dst = append(dst, e.Codec, 0)
	dst = binary.LittleEndian.AppendUint16(dst, e.RecordHeaderLen)
	dst = binary.LittleEndian.AppendUint32(dst, e.BodyLen)
	dst = binary.LittleEndian.AppendUint32(dst, e.UncompressedLen)
	dst = binary.LittleEndian.AppendUint32(dst, e.RecordCount)
	dst = binary.LittleEndian.AppendUint32(dst, e.BodyCRC)
	dst = binary.LittleEndian.AppendUint64(dst, e.FirstSeq)
	dst = append(dst, make([]byte, envelopeTailReservedLen)...)

	return binary.LittleEndian.AppendUint32(dst, CRC(dst[start:]))
}

// UnmarshalBlockEnvelope decodes the block envelope from the first EnvelopeLen bytes of b.
//
// It checks the magic, then envelope_crc over bytes 0-35;
// then that record_header_len is at least RecordHeaderLen and record_count at least 1 (ErrCorrupt);
// then that body_len and uncompressed_len are at most MaxLen32 and first_seq at most MaxU64 (ErrLimit).
// Reserved bytes are ignored, and bytes after the first EnvelopeLen are not read.
// The returned envelope copies every field out of b.
//
// Returns:
//   - BlockEnvelope: the decoded envelope; the zero value on error.
//   - error: an *OffsetError wrapping ErrShort, ErrBadMagic, ErrCRC, ErrCorrupt or ErrLimit.
func UnmarshalBlockEnvelope(b []byte) (BlockEnvelope, error) {
	if err := checkShort(envelopeStructure, b, EnvelopeLen); err != nil {
		return BlockEnvelope{}, err
	}

	if err := checkMagic(envelopeStructure, b, 0, EnvelopeMagic); err != nil {
		return BlockEnvelope{}, err
	}

	crc, err := checkCRC(envelopeStructure, "envelope_crc", b, envelopeCRCOff)
	if err != nil {
		return BlockEnvelope{}, err
	}

	e := BlockEnvelope{
		Codec:           b[4],
		RecordHeaderLen: binary.LittleEndian.Uint16(b[6:8]),
		BodyLen:         binary.LittleEndian.Uint32(b[8:12]),
		UncompressedLen: binary.LittleEndian.Uint32(b[12:16]),
		RecordCount:     binary.LittleEndian.Uint32(b[16:20]),
		BodyCRC:         binary.LittleEndian.Uint32(b[20:24]),
		FirstSeq:        binary.LittleEndian.Uint64(b[24:32]),
		EnvelopeCRC:     crc,
	}

	if err := e.validate(); err != nil {
		return BlockEnvelope{}, err
	}

	return e, nil
}

// validate runs the structural and limit checks of UnmarshalBlockEnvelope on the decoded fields.
func (e *BlockEnvelope) validate() error {
	if err := checkRecordHeaderLen(envelopeStructure, envelopeRecordHeaderLenOff, int(e.RecordHeaderLen)); err != nil {
		return err
	}

	if e.RecordCount == 0 {
		return &OffsetError{
			Structure: envelopeStructure,
			Field:     "record_count",
			Offset:    envelopeRecordCountOff,
			Err:       fmt.Errorf("record_count 0, want at least 1: %w", ErrCorrupt),
		}
	}

	lens := [...]len32Field{
		{name: "body_len", off: envelopeBodyLenOff, value: e.BodyLen},
		{name: "uncompressed_len", off: envelopeUncompressedLenOff, value: e.UncompressedLen},
	}
	if err := checkLen32Fields(envelopeStructure, lens[:]); err != nil {
		return err
	}

	u64s := [...]u64Field{{name: "first_seq", off: envelopeFirstSeqOff, value: e.FirstSeq}}

	return checkU64Fields(envelopeStructure, u64s[:])
}
