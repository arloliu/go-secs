package format

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// FooterPrologueLen is the length of the F-1 prologue at the start of the decoded footer,
// per the tracepack format specification §10.
const FooterPrologueLen = 72

// F2EntryLen is the length of an F-2 block index entry in footer layout 1, per the tracepack format specification §10.
//
// It is also the smallest f2_entry_len a footer may declare:
// a larger value appends bytes a reader of layout 1 skips.
const F2EntryLen = 80

// FooterLayoutVersion is the only footer_layout_version this package reads and writes.
const FooterLayoutVersion uint16 = 1

// Structure names used in errors.
const (
	footerPrologueStructure = "footer prologue"
	f2EntryStructure        = "footer block index entry"
)

// F-1 prologue field offsets.
const (
	prologueF2EntryLenOff = 8
	prologueF2OffsetOff   = 16
	prologueF3OffsetOff   = 24
	prologueF3LenOff      = 32
	prologueF4OffsetOff   = 40
	prologueF4LenOff      = 48
	prologueF5OffsetOff   = 56
	prologueF5LenOff      = 64
)

// F-2 entry field offsets and widths.
const (
	f2OffsetOff          = 0
	f2OnDiskLenOff       = 8
	f2UncompressedLenOff = 12
	f2FirstSeqOff        = 24
	f2LastSeqOff         = 32
	f2SummaryOffsetOff   = 64
	f2RecordHeaderLenOff = 76
	f2ReservedLen        = 2
)

// F-1 flags bits of the tracepack format specification §10;
// bits 2-15 are reserved.
const (
	// FooterFlagF3Present is F-1 flags bit 0, F-3 present, which footer layout 1 always sets.
	FooterFlagF3Present uint16 = 1 << 0
	// FooterFlagF4Present is F-1 flags bit 1, F-4 present.
	FooterFlagF4Present uint16 = 1 << 1
)

// footerPrologueFlagsMask keeps the defined F-1 flags bits, F-3 present and F-4 present.
const footerPrologueFlagsMask = FooterFlagF3Present | FooterFlagF4Present

// FooterPrologue is the decoded F-1 footer prologue of the tracepack format specification §10.
//
// Fields hold the raw values as stored, and Flags keeps its reserved bits as read.
// The relations between the sections, such as f2_offset being 72 and the sections not overlapping,
// are footer validation the caller performs.
type FooterPrologue struct {
	FooterLayoutVersion uint16
	Flags               uint16
	BlockCount          uint32
	F2EntryLen          uint32
	ExtractionVersion   uint32
	F2Offset            uint64
	F3Offset            uint64
	F3Len               uint64
	F4Offset            uint64
	F4Len               uint64
	F5Offset            uint64
	F5Len               uint64
}

// F2Entry is a decoded F-2 block index entry of the tracepack format specification §10:
// the 80 bytes footer layout 1 defines.
//
// Fields hold the raw values as stored; the reserved bytes 78-79 and any bytes behind offset 80 are not represented.
type F2Entry struct {
	Offset          uint64
	OnDiskLen       uint32
	UncompressedLen uint32
	RecordCount     uint32
	BodyCRC         uint32
	FirstSeq        uint64
	LastSeq         uint64
	TSMin           int64
	TSMax           int64
	EpochMin        uint32
	EpochMax        uint32
	SummaryOffset   uint64
	SummaryLen      uint32
	RecordHeaderLen uint16
}

// AppendFooterPrologue appends the 72-byte encoding of p to dst and returns the extended slice.
//
// Every field is written verbatim, except that Flags is masked to its defined bits 0-1 first;
// the prologue has no reserved bytes and no CRC.
// AppendFooterPrologue does not validate p.
// It does not allocate when dst has room for FooterPrologueLen more bytes.
func AppendFooterPrologue(dst []byte, p *FooterPrologue) []byte {
	dst = slices.Grow(dst, FooterPrologueLen)

	dst = binary.LittleEndian.AppendUint16(dst, p.FooterLayoutVersion)
	dst = binary.LittleEndian.AppendUint16(dst, p.Flags&footerPrologueFlagsMask)
	dst = binary.LittleEndian.AppendUint32(dst, p.BlockCount)
	dst = binary.LittleEndian.AppendUint32(dst, p.F2EntryLen)
	dst = binary.LittleEndian.AppendUint32(dst, p.ExtractionVersion)
	dst = binary.LittleEndian.AppendUint64(dst, p.F2Offset)
	dst = binary.LittleEndian.AppendUint64(dst, p.F3Offset)
	dst = binary.LittleEndian.AppendUint64(dst, p.F3Len)
	dst = binary.LittleEndian.AppendUint64(dst, p.F4Offset)
	dst = binary.LittleEndian.AppendUint64(dst, p.F4Len)
	dst = binary.LittleEndian.AppendUint64(dst, p.F5Offset)

	return binary.LittleEndian.AppendUint64(dst, p.F5Len)
}

// UnmarshalFooterPrologue decodes the F-1 prologue from the first FooterPrologueLen bytes of b,
// which hold the start of the decoded footer.
//
// It checks that footer_layout_version equals FooterLayoutVersion (ErrSchema),
// then that f2_entry_len is at least F2EntryLen (ErrCorrupt),
// then that every u64 section offset and length is at most MaxU64 (ErrLimit).
// The rest of the footer validation of the tracepack format specification §10 is the caller's.
// The returned prologue copies every field out of b.
//
// Returns:
//   - FooterPrologue: the decoded prologue; the zero value on error.
//   - error: an *OffsetError wrapping ErrShort, ErrSchema, ErrCorrupt or ErrLimit.
func UnmarshalFooterPrologue(b []byte) (FooterPrologue, error) {
	if err := checkShort(footerPrologueStructure, b, FooterPrologueLen); err != nil {
		return FooterPrologue{}, err
	}

	p := FooterPrologue{
		FooterLayoutVersion: binary.LittleEndian.Uint16(b[0:2]),
		Flags:               binary.LittleEndian.Uint16(b[2:4]),
		BlockCount:          binary.LittleEndian.Uint32(b[4:8]),
		F2EntryLen:          binary.LittleEndian.Uint32(b[8:12]),
		ExtractionVersion:   binary.LittleEndian.Uint32(b[12:16]),
		F2Offset:            binary.LittleEndian.Uint64(b[16:24]),
		F3Offset:            binary.LittleEndian.Uint64(b[24:32]),
		F3Len:               binary.LittleEndian.Uint64(b[32:40]),
		F4Offset:            binary.LittleEndian.Uint64(b[40:48]),
		F4Len:               binary.LittleEndian.Uint64(b[48:56]),
		F5Offset:            binary.LittleEndian.Uint64(b[56:64]),
		F5Len:               binary.LittleEndian.Uint64(b[64:72]),
	}

	if err := p.validate(); err != nil {
		return FooterPrologue{}, err
	}

	return p, nil
}

// AppendF2Entry appends the 80-byte layout 1 encoding of e to dst and returns the extended slice.
//
// Named fields are written verbatim, and the reserved bytes 78-79 are written as zero.
// AppendF2Entry does not validate e.
// It does not allocate when dst has room for F2EntryLen more bytes.
func AppendF2Entry(dst []byte, e *F2Entry) []byte {
	dst = slices.Grow(dst, F2EntryLen)

	dst = binary.LittleEndian.AppendUint64(dst, e.Offset)
	dst = binary.LittleEndian.AppendUint32(dst, e.OnDiskLen)
	dst = binary.LittleEndian.AppendUint32(dst, e.UncompressedLen)
	dst = binary.LittleEndian.AppendUint32(dst, e.RecordCount)
	dst = binary.LittleEndian.AppendUint32(dst, e.BodyCRC)
	dst = binary.LittleEndian.AppendUint64(dst, e.FirstSeq)
	dst = binary.LittleEndian.AppendUint64(dst, e.LastSeq)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.TSMin))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(e.TSMax))
	dst = binary.LittleEndian.AppendUint32(dst, e.EpochMin)
	dst = binary.LittleEndian.AppendUint32(dst, e.EpochMax)
	dst = binary.LittleEndian.AppendUint64(dst, e.SummaryOffset)
	dst = binary.LittleEndian.AppendUint32(dst, e.SummaryLen)
	dst = binary.LittleEndian.AppendUint16(dst, e.RecordHeaderLen)

	return append(dst, make([]byte, f2ReservedLen)...)
}

// UnmarshalF2Entry decodes one F-2 block index entry of f2EntryLen bytes from the start of b.
//
// f2EntryLen is the f2_entry_len of the F-1 prologue;
// a value below F2EntryLen is reported as ErrCorrupt, with the pseudo-offset 0,
// because the length is a property of the footer, not an entry field.
// b must hold the whole entry, f2EntryLen bytes, so a caller can decode entry i from section[i*f2EntryLen:].
// UnmarshalF2Entry reads the 80 bytes layout 1 defines and skips the rest, ignoring the reserved bytes 78-79.
// It checks that record_header_len is at least RecordHeaderLen (ErrCorrupt),
// then that offset, first_seq, last_seq and summary_offset are at most MaxU64
// uncompressed_len at most MaxLen32, and on_disk_len at most EnvelopeLen + MaxLen32,
// the envelope plus the largest body_len (ErrLimit).
// The rest of the footer validation of the tracepack format specification §10 is the caller's.
// The returned entry copies every field out of b.
//
// Returns:
//   - F2Entry: the decoded entry; the zero value on error.
//   - error: an *OffsetError wrapping ErrCorrupt, ErrShort or ErrLimit.
func UnmarshalF2Entry(b []byte, f2EntryLen int) (F2Entry, error) {
	if f2EntryLen < F2EntryLen {
		return F2Entry{}, errF2EntryLen(f2EntryStructure, 0, f2EntryLen)
	}

	if err := checkShort(f2EntryStructure, b, f2EntryLen); err != nil {
		return F2Entry{}, err
	}

	e := F2Entry{
		Offset:          binary.LittleEndian.Uint64(b[0:8]),
		OnDiskLen:       binary.LittleEndian.Uint32(b[8:12]),
		UncompressedLen: binary.LittleEndian.Uint32(b[12:16]),
		RecordCount:     binary.LittleEndian.Uint32(b[16:20]),
		BodyCRC:         binary.LittleEndian.Uint32(b[20:24]),
		FirstSeq:        binary.LittleEndian.Uint64(b[24:32]),
		LastSeq:         binary.LittleEndian.Uint64(b[32:40]),
		TSMin:           int64(binary.LittleEndian.Uint64(b[40:48])),
		TSMax:           int64(binary.LittleEndian.Uint64(b[48:56])),
		EpochMin:        binary.LittleEndian.Uint32(b[56:60]),
		EpochMax:        binary.LittleEndian.Uint32(b[60:64]),
		SummaryOffset:   binary.LittleEndian.Uint64(b[64:72]),
		SummaryLen:      binary.LittleEndian.Uint32(b[72:76]),
		RecordHeaderLen: binary.LittleEndian.Uint16(b[76:78]),
	}

	if err := e.validate(); err != nil {
		return F2Entry{}, err
	}

	return e, nil
}

// HeaderSectionLen returns the length of a decoded block body's header section:
// recordCount × recordHeaderLen, per the tracepack format specification §6.
//
// A decoded body is at most MaxLen32 bytes long,
// so a product above MaxLen32 cannot describe a valid block and is reported before any allocation or slicing.
// The product of a u32 and a u16 cannot overflow 64 bits, so the check itself is exact.
//
// Returns:
//   - int: the header section length in bytes; 0 on error.
//   - error: an *OffsetError for the envelope's record_count wrapping ErrLimit when the product exceeds MaxLen32.
func HeaderSectionLen(recordCount uint32, recordHeaderLen uint16) (int, error) {
	n := uint64(recordCount) * uint64(recordHeaderLen)
	if n > uint64(MaxLen32) {
		return 0, &OffsetError{
			Structure: envelopeStructure,
			Field:     "record_count",
			Offset:    envelopeRecordCountOff,
			Err: fmt.Errorf("header section of %d records of %d bytes is %d bytes, above the %d limit: %w",
				recordCount, recordHeaderLen, n, MaxLen32, ErrLimit),
		}
	}

	return int(n), nil
}

// errF2EntryLen reports an f2_entry_len below F2EntryLen as ErrCorrupt.
func errF2EntryLen(structure string, off, n int) error {
	return &OffsetError{
		Structure: structure,
		Field:     "f2_entry_len",
		Offset:    off,
		Err:       fmt.Errorf("f2_entry_len %d below %d: %w", n, F2EntryLen, ErrCorrupt),
	}
}

// validate runs the version, structural and limit checks of UnmarshalFooterPrologue on the decoded fields.
func (p *FooterPrologue) validate() error {
	if err := checkVersion(footerPrologueStructure, "footer_layout_version", 0, p.FooterLayoutVersion, FooterLayoutVersion); err != nil {
		return err
	}

	if p.F2EntryLen < F2EntryLen {
		return errF2EntryLen(footerPrologueStructure, prologueF2EntryLenOff, int(p.F2EntryLen))
	}

	u64s := [...]u64Field{
		{name: "f2_offset", off: prologueF2OffsetOff, value: p.F2Offset},
		{name: "f3_offset", off: prologueF3OffsetOff, value: p.F3Offset},
		{name: "f3_len", off: prologueF3LenOff, value: p.F3Len},
		{name: "f4_offset", off: prologueF4OffsetOff, value: p.F4Offset},
		{name: "f4_len", off: prologueF4LenOff, value: p.F4Len},
		{name: "f5_offset", off: prologueF5OffsetOff, value: p.F5Offset},
		{name: "f5_len", off: prologueF5LenOff, value: p.F5Len},
	}

	return checkU64Fields(footerPrologueStructure, u64s[:])
}

// validate runs the structural and limit checks of UnmarshalF2Entry on the decoded fields.
func (e *F2Entry) validate() error {
	if err := checkRecordHeaderLen(f2EntryStructure, f2RecordHeaderLenOff, int(e.RecordHeaderLen)); err != nil {
		return err
	}

	u64s := [...]u64Field{
		{name: "offset", off: f2OffsetOff, value: e.Offset},
		{name: "first_seq", off: f2FirstSeqOff, value: e.FirstSeq},
		{name: "last_seq", off: f2LastSeqOff, value: e.LastSeq},
		{name: "summary_offset", off: f2SummaryOffsetOff, value: e.SummaryOffset},
	}
	if err := checkU64Fields(f2EntryStructure, u64s[:]); err != nil {
		return err
	}

	// on_disk_len is the envelope plus a body_len of at most MaxLen32,
	// so it may exceed MaxLen32 by the envelope's length.
	if e.OnDiskLen > EnvelopeLen+MaxLen32 {
		return &OffsetError{
			Structure: f2EntryStructure,
			Field:     "on_disk_len",
			Offset:    f2OnDiskLenOff,
			Err:       fmt.Errorf("on_disk_len: value %d exceeds the %d-byte envelope plus %d: %w", e.OnDiskLen, EnvelopeLen, MaxLen32, ErrLimit),
		}
	}

	lens := [...]len32Field{{name: "uncompressed_len", off: f2UncompressedLenOff, value: e.UncompressedLen}}

	return checkLen32Fields(f2EntryStructure, lens[:])
}
