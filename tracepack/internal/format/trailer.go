package format

import (
	"encoding/binary"
	"slices"
)

// TrailerLen is the length of the trailer at the end of every finalized pack,
// per the tracepack format specification §11.
const TrailerLen = 64

// TrailerMagic is the trailer magic at offset 56: 54 50 4B 45 4E 44 0D 0A.
const TrailerMagic = "TPKEND\r\n"

// TrailerVersion is the only trailer_version this package reads and writes.
const TrailerVersion uint16 = 1

// trailerStructure names the trailer in errors.
const trailerStructure = "trailer"

// Trailer field offsets.
const (
	trailerFooterOffsetOff          = 0
	trailerFooterLenOff             = 8
	trailerFooterUncompressedLenOff = 16
	trailerRecordCountOff           = 32
	trailerLastSeqOff               = 40
	trailerVersionOff               = 48
	trailerCRCOff                   = 52
	trailerMagicOff                 = 56
)

// Trailer is the decoded trailer of the tracepack format specification §11.
//
// Fields hold the raw values as stored.
// FooterCodec keeps any value, including one the reader does not support,
// and Flags keeps its reserved bits as read.
type Trailer struct {
	FooterOffset          uint64
	FooterLen             uint64
	FooterUncompressedLen uint64
	BlockCount            uint32
	FooterCRC             uint32
	RecordCount           uint64
	LastSeq               uint64
	TrailerVersion        uint16
	FooterCodec           uint8
	Flags                 uint8
	// TrailerCRC is the CRC over bytes 0-51 as read.
	// AppendTrailer ignores it and writes the computed CRC.
	TrailerCRC uint32
}

// AppendTrailer appends the 64-byte encoding of t to dst and returns the extended slice.
//
// Named fields are written verbatim, including Flags, whose bits are all reserved;
// trailer_crc is computed over bytes 0-51, ignoring t.TrailerCRC and leaving t unchanged,
// and the magic follows it.
// AppendTrailer does not validate t: a writer sets t.TrailerVersion to the TrailerVersion constant itself.
// It does not allocate when dst has room for TrailerLen more bytes.
func AppendTrailer(dst []byte, t *Trailer) []byte {
	dst = slices.Grow(dst, TrailerLen)
	start := len(dst)

	dst = binary.LittleEndian.AppendUint64(dst, t.FooterOffset)
	dst = binary.LittleEndian.AppendUint64(dst, t.FooterLen)
	dst = binary.LittleEndian.AppendUint64(dst, t.FooterUncompressedLen)
	dst = binary.LittleEndian.AppendUint32(dst, t.BlockCount)
	dst = binary.LittleEndian.AppendUint32(dst, t.FooterCRC)
	dst = binary.LittleEndian.AppendUint64(dst, t.RecordCount)
	dst = binary.LittleEndian.AppendUint64(dst, t.LastSeq)
	dst = binary.LittleEndian.AppendUint16(dst, t.TrailerVersion)
	dst = append(dst, t.FooterCodec, t.Flags)
	dst = binary.LittleEndian.AppendUint32(dst, CRC(dst[start:]))

	return append(dst, TrailerMagic...)
}

// UnmarshalTrailer decodes the trailer from the first TrailerLen bytes of b.
//
// It checks the magic at offset 56, then trailer_crc over bytes 0-51,
// then trailer_version, which must equal TrailerVersion (ErrSchema),
// then that footer_offset, footer_len, footer_uncompressed_len, record_count and last_seq are at most MaxU64 (ErrLimit).
// last_seq is checked even when record_count is 0 and the value carries no meaning.
// Relations between the trailer, the footer and the file size are the caller's to check.
// The returned trailer copies every field out of b.
//
// Returns:
//   - Trailer: the decoded trailer; the zero value on error.
//   - error: an *OffsetError wrapping ErrShort, ErrBadMagic, ErrCRC, ErrSchema or ErrLimit.
func UnmarshalTrailer(b []byte) (Trailer, error) {
	if err := checkShort(trailerStructure, b, TrailerLen); err != nil {
		return Trailer{}, err
	}

	if err := checkMagic(trailerStructure, b, trailerMagicOff, TrailerMagic); err != nil {
		return Trailer{}, err
	}

	crc, err := checkCRC(trailerStructure, "trailer_crc", b, trailerCRCOff)
	if err != nil {
		return Trailer{}, err
	}

	t := Trailer{
		FooterOffset:          binary.LittleEndian.Uint64(b[0:8]),
		FooterLen:             binary.LittleEndian.Uint64(b[8:16]),
		FooterUncompressedLen: binary.LittleEndian.Uint64(b[16:24]),
		BlockCount:            binary.LittleEndian.Uint32(b[24:28]),
		FooterCRC:             binary.LittleEndian.Uint32(b[28:32]),
		RecordCount:           binary.LittleEndian.Uint64(b[32:40]),
		LastSeq:               binary.LittleEndian.Uint64(b[40:48]),
		TrailerVersion:        binary.LittleEndian.Uint16(b[48:50]),
		FooterCodec:           b[50],
		Flags:                 b[51],
		TrailerCRC:            crc,
	}

	if err := checkVersion(trailerStructure, "trailer_version", trailerVersionOff, t.TrailerVersion, TrailerVersion); err != nil {
		return Trailer{}, err
	}

	u64s := [...]u64Field{
		{name: "footer_offset", off: trailerFooterOffsetOff, value: t.FooterOffset},
		{name: "footer_len", off: trailerFooterLenOff, value: t.FooterLen},
		{name: "footer_uncompressed_len", off: trailerFooterUncompressedLenOff, value: t.FooterUncompressedLen},
		{name: "record_count", off: trailerRecordCountOff, value: t.RecordCount},
		{name: "last_seq", off: trailerLastSeqOff, value: t.LastSeq},
	}
	if err := checkU64Fields(trailerStructure, u64s[:]); err != nil {
		return Trailer{}, err
	}

	return t, nil
}
