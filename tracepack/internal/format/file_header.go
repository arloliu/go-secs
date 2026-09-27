package format

import (
	"encoding/binary"
	"slices"
)

// FileHeaderLen is the length of the file header at offset 0 of every pack,
// per the tracepack format specification §4.
const FileHeaderLen = 80

// FileHeaderMagic is the file header magic: 89 54 50 4B 0D 0A 1A 0A.
//
// The high-bit first byte and the CR LF, SUB and LF bytes detect non-tracepack input and text-mode transfer damage.
const FileHeaderMagic = "\x89TPK\r\n\x1a\n"

// FormatMajor is the only format_major a format 1.x reader accepts, per the tracepack format specification §14.
const FormatMajor uint16 = 1

// fileHeaderStructure names the file header in errors.
const fileHeaderStructure = "file header"

// File header field offsets and widths.
const (
	fileHeaderMajorOff    = 8
	fileHeaderReservedLen = 12
	fileHeaderCRCOff      = 76
)

// FileHeader is the decoded file header of the tracepack format specification §4.
//
// Fields hold the raw values as stored;
// Flags keeps its reserved bits 1-31 as read, and the reserved bytes 64-75 are not represented.
type FileHeader struct {
	FormatMajor      uint16
	FormatMinor      uint16
	Flags            uint32
	PackMetadataLen  uint32
	PackMetadataCRC  uint32
	WriterStartUTCNs int64
	PackID           UUID
	CaptureID        UUID
	// HeaderCRC is the CRC over bytes 0-75 as read.
	// AppendFileHeader ignores it and writes the computed CRC.
	HeaderCRC uint32
}

// AppendFileHeader appends the 80-byte encoding of h to dst and returns the extended slice.
//
// Named fields are written verbatim, including any reserved bits of Flags;
// the reserved bytes 64-75 are written as zero,
// and header_crc is computed over bytes 0-75, ignoring h.HeaderCRC and leaving h unchanged.
// AppendFileHeader does not validate h: a writer sets h.FormatMajor to the FormatMajor constant itself.
// It does not allocate when dst has room for FileHeaderLen more bytes.
func AppendFileHeader(dst []byte, h *FileHeader) []byte {
	dst = slices.Grow(dst, FileHeaderLen)
	start := len(dst)

	dst = append(dst, FileHeaderMagic...)
	dst = binary.LittleEndian.AppendUint16(dst, h.FormatMajor)
	dst = binary.LittleEndian.AppendUint16(dst, h.FormatMinor)
	dst = binary.LittleEndian.AppendUint32(dst, h.Flags)
	dst = binary.LittleEndian.AppendUint32(dst, h.PackMetadataLen)
	dst = binary.LittleEndian.AppendUint32(dst, h.PackMetadataCRC)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(h.WriterStartUTCNs))
	dst = append(dst, h.PackID[:]...)
	dst = append(dst, h.CaptureID[:]...)
	dst = append(dst, make([]byte, fileHeaderReservedLen)...)

	return binary.LittleEndian.AppendUint32(dst, CRC(dst[start:]))
}

// UnmarshalFileHeader decodes the file header from the first FileHeaderLen bytes of b.
//
// It checks, in the order the tracepack format specification §4 requires,
// the magic, then header_crc over bytes 0-75, then format_major, which must equal FormatMajor;
// any format_minor is accepted (§14).
// Reserved bytes are ignored, and pack_metadata_len is not range-checked beyond fitting its u32 field.
// Bytes after the first FileHeaderLen are not read.
// The returned header copies every field out of b.
//
// Returns:
//   - FileHeader: the decoded header; the zero value on error.
//   - error: an *OffsetError wrapping ErrShort, ErrBadMagic, ErrCRC or ErrSchema.
func UnmarshalFileHeader(b []byte) (FileHeader, error) {
	if err := checkShort(fileHeaderStructure, b, FileHeaderLen); err != nil {
		return FileHeader{}, err
	}

	if err := checkMagic(fileHeaderStructure, b, 0, FileHeaderMagic); err != nil {
		return FileHeader{}, err
	}

	crc, err := checkCRC(fileHeaderStructure, "header_crc", b, fileHeaderCRCOff)
	if err != nil {
		return FileHeader{}, err
	}

	h := FileHeader{
		FormatMajor:      binary.LittleEndian.Uint16(b[8:10]),
		FormatMinor:      binary.LittleEndian.Uint16(b[10:12]),
		Flags:            binary.LittleEndian.Uint32(b[12:16]),
		PackMetadataLen:  binary.LittleEndian.Uint32(b[16:20]),
		PackMetadataCRC:  binary.LittleEndian.Uint32(b[20:24]),
		WriterStartUTCNs: int64(binary.LittleEndian.Uint64(b[24:32])),
		PackID:           UUID(b[32:48]),
		CaptureID:        UUID(b[48:64]),
		HeaderCRC:        crc,
	}

	if err := checkVersion(fileHeaderStructure, "format_major", fileHeaderMajorOff, h.FormatMajor, FormatMajor); err != nil {
		return FileHeader{}, err
	}

	return h, nil
}
