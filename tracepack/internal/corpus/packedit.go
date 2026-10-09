package corpus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Positions the pack surgery reads and writes, beside those of the structural footer reader
// (the tracepack format specification §4, §6, §10 and §11).
const (
	headerPackMetadataCRCOff = 20

	envelopeCodecOff           = 4
	envelopeRecordHeaderLenOff = 6
	envelopeBodyLenOff         = 8
	envelopeUncompressedLenOff = 12
	envelopeRecordCountOff     = 16
	envelopeBodyCRCOff         = 20
	envelopeFirstSeqOff        = 24

	f2OffsetOff          = 0
	f2OnDiskLenOff       = 8
	f2UncompressedLenOff = 12
	f2RecordCountOff     = 16
	f2BodyCRCOff         = 20
	f2FirstSeqOff        = 24
	f2RecordHeaderLenOff = 76

	prologueFlagsOff    = 2
	prologueF4OffsetOff = 40
	prologueF4LenOff    = 48

	// footerFlagF4Present is F-1 flag bit 1, F-4 present.
	footerFlagF4Present uint16 = 0x0002

	// f5ContentBytesTag is the F-5 tag of content_bytes, the sum of the blocks' F-2 uncompressed_len.
	f5ContentBytesTag uint16 = 0x0007
	// f5RecordCountTag is the F-5 tag of record_count, the pack's record count.
	f5RecordCountTag uint16 = 0x0001
)

// ErrPackEdit reports an edit the pack surgery cannot make:
// a pack too short for the structure the edit names, an index or offset out of range,
// a structure the surgery cannot locate, or a value that does not fit its field.
var ErrPackEdit = errors.New("corpus: pack edit")

// Layout locates the stored structures of a pack as the pack surgery finds them.
type Layout struct {
	// MetaEnd is 80 + pack_metadata_len: where the first block starts.
	MetaEnd uint64
	// Blocks holds each block the forward walk finds, in file order:
	// an envelope with the block magic whose body_len keeps the block inside the walked range.
	// Neither CRC is checked, so a block whose CRCs an earlier edit broke is still found.
	Blocks []Span
	// End is where the walk stopped: the footer_offset of a finalized pack,
	// else the end of the last block found, or MetaEnd when there is none.
	End uint64
	// Finalized reports a trailer whose magic and trailer_crc hold.
	Finalized bool
}

// Span is a byte range of a pack.
type Span struct {
	// Offset is the range's first byte.
	Offset uint64
	// Len is the range's length.
	Len uint64
}

// Body is a block's decoded body as PatchBody hands it to an edit, split at the end of the header section.
type Body struct {
	// Codec is the block's codec; PatchBody encodes the edited body with it and writes it into the envelope.
	Codec uint8
	// RecordHeaderLen is the length of each record header in Rows.
	RecordHeaderLen int
	// Rows holds the record headers in record order, RecordHeaderLen bytes each:
	// the header section untransposed, every bit as stored, reserved and retired bits and extension areas included.
	Rows []byte
	// Payloads holds every decoded byte after the header section, as stored.
	Payloads []byte
}

// Section is a byte range of a decoded footer or of a TLV entry list.
type Section struct {
	// Off is the range's first byte.
	Off int
	// Len is the range's length.
	Len int
}

// FooterParts is a decoded footer split into its stored sections for raw surgery.
// Every part holds the stored bytes: F-2 entries with their bytes past offset 80, TLV entries with their reserved bytes, F-4 as found.
type FooterParts struct {
	// Prologue is the 72-byte F-1 prologue.
	Prologue []byte
	// Entries holds the F-2 entries in order, f2_entry_len bytes each.
	Entries [][]byte
	// F3 is the F-3 section; each entry's summary_offset and summary_len locate its list in it.
	F3 []byte
	// F4 is the F-4 section, empty when f4_len is 0.
	F4 []byte
	// F5 is the F-5 entry list.
	F5 []byte
}

// mirror names the F-2 entry fields a replaced block's entry takes from its new envelope.
type mirror uint8

const (
	// mirrorOnDisk copies on_disk_len and body_crc.
	mirrorOnDisk mirror = 1 << iota
	// mirrorDecoded copies uncompressed_len, record_count and record_header_len.
	mirrorDecoded
	// mirrorFirstSeq copies first_seq.
	mirrorFirstSeq
)

// Locate finds the stored structures of pack: the end of the pack metadata, the blocks and, when finalized, the footer offset.
//
// Every edit that names a block locates it this way, so a recipe makes its block edits before an edit that breaks the walk,
// the trailer or the footer:
// after a block magic or body_len that stops the walk before footer_offset, Locate refuses a finalized pack;
// after an edit that breaks the trailer, the pack is not finalized, and the footer and trailer are bytes after the last block, which a block edit moves without changing;
// and PatchBody, DamageZstd and ReencodeBlocks on a finalized pack need its footer readable, as PatchFooter does.
//
// Parameters:
//   - pack: the pack's bytes.
//
// Returns:
//   - the layout;
//   - an error wrapping ErrPackEdit when pack holds no file header, its pack metadata runs past its end,
//     or its valid trailer places the footer where the walk cannot end.
func Locate(pack []byte) (Layout, error) {
	if err := needLen(pack, format.FileHeaderLen, "file header"); err != nil {
		return Layout{}, err
	}

	size := uint64(len(pack))
	l := Layout{MetaEnd: format.FileHeaderLen + uint64(binary.LittleEndian.Uint32(pack[headerPackMetadataLenOff:]))}
	if l.MetaEnd > size {
		return Layout{}, fmt.Errorf("%w: pack metadata ends at %d, past the %d-byte pack", ErrPackEdit, l.MetaEnd, size)
	}

	end := size
	if size-l.MetaEnd >= format.TrailerLen && validTrailer(pack[size-format.TrailerLen:]) {
		l.Finalized = true
		end = binary.LittleEndian.Uint64(pack[size-format.TrailerLen+trailerFooterOffsetOff:])
		if end < l.MetaEnd || end > size-format.TrailerLen {
			return Layout{}, fmt.Errorf("%w: footer_offset %d lies outside %d..%d", ErrPackEdit, end, l.MetaEnd, size-format.TrailerLen)
		}
	}

	off := l.MetaEnd
	for end-off >= format.EnvelopeLen && string(pack[off:off+4]) == format.EnvelopeMagic {
		n := format.EnvelopeLen + uint64(binary.LittleEndian.Uint32(pack[off+envelopeBodyLenOff:]))
		if n > end-off {
			break
		}
		l.Blocks = append(l.Blocks, Span{Offset: off, Len: n})
		off += n
	}
	l.End = off
	if l.Finalized && off != end {
		return Layout{}, fmt.Errorf("%w: the block walk stops at %d, before footer_offset %d", ErrPackEdit, off, end)
	}

	return l, nil
}

// PackWithMeta returns a pack of a file header and meta, and nothing after them:
// the header holds h's fields, with pack_metadata_len, pack_metadata_crc and header_crc computed for meta.
// The header is encoded from h, so its reserved bytes and bits are zero; PatchHeader sets them.
func PackWithMeta(h format.FileHeader, meta []byte) ([]byte, error) {
	if uint64(len(meta)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: %d bytes of pack metadata do not fit pack_metadata_len", ErrPackEdit, len(meta))
	}
	h.PackMetadataLen, h.PackMetadataCRC = uint32(len(meta)), format.CRC(meta)

	return append(format.AppendFileHeader(nil, &h), meta...), nil
}

// FlipByte returns a copy of pack with the byte at off inverted; it recomputes nothing.
func FlipByte(pack []byte, off int) ([]byte, error) {
	if off < 0 || off >= len(pack) {
		return nil, fmt.Errorf("%w: offset %d outside the %d-byte pack", ErrPackEdit, off, len(pack))
	}
	out := bytes.Clone(pack)
	out[off] ^= 0xFF

	return out, nil
}

// PatchHeader returns a copy of pack whose 80-byte file header edit changes in place, with header_crc recomputed.
// Every other byte, reserved bytes and bits included, is kept as stored unless edit changes it.
func PatchHeader(pack []byte, edit func(h []byte)) ([]byte, error) {
	if err := needLen(pack, format.FileHeaderLen, "file header"); err != nil {
		return nil, err
	}

	out := bytes.Clone(pack)
	h := out[:format.FileHeaderLen:format.FileHeaderLen]
	edit(h)
	putCRC(h, fileHeaderCRCOff)

	return out, nil
}

// PatchMeta returns a copy of pack whose pack metadata is the entry list edit returns for a copy of the stored one,
// with pack_metadata_len, pack_metadata_crc and header_crc recomputed and every other header byte kept.
// The metadata may change length only in a pack that holds nothing after it,
// since the surgery does not move the offsets of later structures.
func PatchMeta(pack []byte, edit func(meta []byte) []byte) ([]byte, error) {
	if err := needLen(pack, format.FileHeaderLen, "file header"); err != nil {
		return nil, err
	}
	metaEnd := format.FileHeaderLen + uint64(binary.LittleEndian.Uint32(pack[headerPackMetadataLenOff:]))
	if metaEnd > uint64(len(pack)) {
		return nil, fmt.Errorf("%w: pack metadata ends at %d, past the %d-byte pack", ErrPackEdit, metaEnd, len(pack))
	}

	old := pack[format.FileHeaderLen:metaEnd]
	meta := edit(bytes.Clone(old))
	switch {
	case len(meta) != len(old) && metaEnd != uint64(len(pack)):
		return nil, fmt.Errorf("%w: pack metadata of %d bytes would move what follows it", ErrPackEdit, len(meta))
	case uint64(len(meta)) > math.MaxUint32:
		return nil, fmt.Errorf("%w: %d bytes of pack metadata do not fit pack_metadata_len", ErrPackEdit, len(meta))
	}

	out := slices.Concat(pack[:format.FileHeaderLen], meta, pack[metaEnd:])
	binary.LittleEndian.PutUint32(out[headerPackMetadataLenOff:], uint32(len(meta)))
	binary.LittleEndian.PutUint32(out[headerPackMetadataCRCOff:], format.CRC(meta))
	putCRC(out[:format.FileHeaderLen], fileHeaderCRCOff)

	return out, nil
}

// PatchEnvelope returns a copy of pack whose block envelope of block i (in walk order, see Locate) edit changes in place,
// with envelope_crc recomputed.
// Nothing else is recomputed: the block's F-2 entry and body keep their stored bytes.
func PatchEnvelope(pack []byte, i int, edit func(env []byte)) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	s, err := l.block(i)
	if err != nil {
		return nil, err
	}

	out := bytes.Clone(pack)
	env := out[s.Offset : s.Offset+format.EnvelopeLen : s.Offset+format.EnvelopeLen]
	edit(env)
	putCRC(env, blockEnvelopeCRCOff)

	return out, nil
}

// PatchBody returns a copy of pack whose block i (in walk order, see Locate) holds the body edit makes of its decoded body.
//
// The edited body is the header section transposed from Rows, then Payloads, encoded with Codec.
// The block's envelope is edited in place: codec, record_header_len, body_len, uncompressed_len,
// record_count (len(Rows) / RecordHeaderLen), body_crc and envelope_crc are recomputed, its other bytes kept.
// In a finalized pack the footer follows the block (see the relocation rule of replaceBlock):
// the block's F-2 entry takes on_disk_len, uncompressed_len, record_count, body_crc and record_header_len from the envelope,
// later entries' offset and the trailer's footer_offset move with the block's length,
// and F-5 content_bytes moves with its uncompressed_len.
// first_seq and every value a record determines (seqs, times, epochs, F-3 and F-5 statistics) keep their stored bytes,
// so an edit of those makes the records disagree with the footer.
//
// The block's body must decode with its stored codec.
func PatchBody(pack []byte, i int, edit func(b *Body) error) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	s, err := l.block(i)
	if err != nil {
		return nil, err
	}

	stored := pack[s.Offset : s.Offset+s.Len]
	decoded, hs, err := decodeBlock(stored)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", i, err)
	}
	count := int(binary.LittleEndian.Uint32(stored[envelopeRecordCountOff:]))
	rhl := int(binary.LittleEndian.Uint16(stored[envelopeRecordHeaderLenOff:]))
	b := &Body{
		Codec: stored[envelopeCodecOff], RecordHeaderLen: rhl,
		Rows: format.UntransposeHeaders(nil, decoded[:hs], count, rhl), Payloads: bytes.Clone(decoded[hs:]),
	}
	if err := edit(b); err != nil {
		return nil, err
	}

	blk, err := b.encode(stored[:format.EnvelopeLen])
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", i, err)
	}

	return replaceBlock(pack, &l, i, blk, mirrorOnDisk|mirrorDecoded)
}

// PatchFooter returns a copy of pack whose decoded footer is the one edit returns for a copy of the stored one.
// The footer is encoded with the trailer's footer_codec;
// the trailer's footer_len, footer_uncompressed_len, footer_crc and trailer_crc are recomputed, its other bytes kept.
// The stored footer must be readable: a valid trailer locating it, footer_crc matching, and decodable.
func PatchFooter(pack []byte, edit func(d []byte) ([]byte, error)) ([]byte, error) {
	if err := needLen(pack, format.FileHeaderLen+format.TrailerLen, "file header and trailer"); err != nil {
		return nil, err
	}
	decoded, err := footerBytes(pack)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	d, err := edit(decoded)
	if err != nil {
		return nil, err
	}

	tr := pack[len(pack)-format.TrailerLen:]
	enc, err := codec.Encode(tr[trailerFooterCodecOff], nil, d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	at := binary.LittleEndian.Uint64(tr[trailerFooterOffsetOff:])

	return appendTrailer(slices.Concat(pack[:at], enc), tr, enc, len(d)), nil
}

// PatchTrailer returns a copy of pack whose 64-byte trailer edit changes in place, with trailer_crc recomputed.
func PatchTrailer(pack []byte, edit func(tr []byte)) ([]byte, error) {
	if err := needLen(pack, format.FileHeaderLen+format.TrailerLen, "file header and trailer"); err != nil {
		return nil, err
	}

	out := bytes.Clone(pack)
	tr := out[len(out)-format.TrailerLen:]
	edit(tr)
	putCRC(tr, trailerCRCOff)

	return out, nil
}

// DecodedFooter returns the decoded footer of pack, which must be readable as PatchFooter requires.
func DecodedFooter(pack []byte) ([]byte, error) {
	if err := needLen(pack, format.FileHeaderLen+format.TrailerLen, "file header and trailer"); err != nil {
		return nil, err
	}
	d, err := footerBytes(pack)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}

	return d, nil
}

// F2Entry returns the section of F-2 entry i in the decoded footer d, f2_entry_len bytes from f2_offset + i × f2_entry_len.
func F2Entry(d []byte, i int) (Section, error) {
	if err := needLen(d, format.FooterPrologueLen, "F-1 prologue"); err != nil {
		return Section{}, err
	}
	n := uint64(binary.LittleEndian.Uint32(d[prologueF2EntryLenOff:]))
	if i < 0 || uint64(i) >= uint64(binary.LittleEndian.Uint32(d[prologueBlockCountOff:])) || n < format.F2EntryLen {
		return Section{}, fmt.Errorf("%w: no F-2 entry %d of f2_entry_len %d", ErrPackEdit, i, n)
	}
	off := binary.LittleEndian.Uint64(d[prologueF2OffsetOff:])
	// i and n are below 2^32, so their product cannot overflow; the sum is checked by inside.
	at := uint64(i) * n
	if off > math.MaxUint64-at || !inside(off+at, n, uint64(len(d))) {
		return Section{}, fmt.Errorf("%w: F-2 entry %d lies outside the %d-byte footer", ErrPackEdit, i, len(d))
	}

	return Section{Off: int(off + at), Len: int(n)}, nil
}

// F3List returns the section of block i's F-3 entry list in the decoded footer d,
// located by f3_offset and the block's summary_offset and summary_len.
func F3List(d []byte, i int) (Section, error) {
	e, err := F2Entry(d, i)
	if err != nil {
		return Section{}, err
	}
	f3 := binary.LittleEndian.Uint64(d[prologueF3OffsetOff:])
	so := binary.LittleEndian.Uint64(d[e.Off+f2SummaryOffsetOff:])
	sl := uint64(binary.LittleEndian.Uint32(d[e.Off+f2SummaryLenOff:]))
	if f3 > math.MaxUint64-so || !inside(f3+so, sl, uint64(len(d))) {
		return Section{}, fmt.Errorf("%w: block %d's F-3 list lies outside the %d-byte footer", ErrPackEdit, i, len(d))
	}

	return Section{Off: int(f3 + so), Len: int(sl)}, nil
}

// F5List returns the section of the F-5 entry list in the decoded footer d, located by f5_offset and f5_len.
func F5List(d []byte) (Section, error) {
	if err := needLen(d, format.FooterPrologueLen, "F-1 prologue"); err != nil {
		return Section{}, err
	}
	off, n := binary.LittleEndian.Uint64(d[prologueF5OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF5LenOff:])
	if !inside(off, n, uint64(len(d))) {
		return Section{}, fmt.Errorf("%w: F-5 lies outside the %d-byte footer", ErrPackEdit, len(d))
	}

	return Section{Off: int(off), Len: int(n)}, nil
}

// EntryOffset returns the offset in b of the header of the k-th entry (from 0) of tag in the TLV entry list s of b,
// which must pass the framing rules of the tracepack format specification §5.
func EntryOffset(b []byte, s Section, tag uint16, k int) (int, error) {
	list, err := s.of(b)
	if err != nil {
		return 0, err
	}
	entries, err := tlv.Decode(list)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	for _, e := range entries {
		if e.Tag != tag {
			continue
		}
		if k == 0 {
			return s.Off + e.Offset, nil
		}
		k--
	}

	return 0, fmt.Errorf("%w: no such entry of tag 0x%04X", ErrPackEdit, tag)
}

// NestedValueOffset returns the offset in b of the value of the first nested entry of tag inner
// inside the k-th entry of tag in the TLV entry list s of b.
func NestedValueOffset(b []byte, s Section, tag uint16, k int, inner uint16) (int, error) {
	outer, err := EntryOffset(b, s, tag, k)
	if err != nil {
		return 0, err
	}
	n := int(binary.LittleEndian.Uint32(b[outer+4:]))
	at, err := EntryOffset(b, Section{Off: outer + tlv.HeaderLen, Len: n}, inner, 0)
	if err != nil {
		return 0, err
	}

	return at + tlv.HeaderLen, nil
}

// SetU64 sets, in place, the value of the first entry of tag in the TLV entry list s of b to v;
// the entry's value must be 8 bytes long.
func SetU64(b []byte, s Section, tag uint16, v uint64) error {
	at, err := EntryOffset(b, s, tag, 0)
	if err != nil {
		return err
	}
	if n := binary.LittleEndian.Uint32(b[at+4:]); n != 8 {
		return fmt.Errorf("%w: entry of tag 0x%04X holds %d bytes, not 8", ErrPackEdit, tag, n)
	}
	binary.LittleEndian.PutUint64(b[at+tlv.HeaderLen:], v)

	return nil
}

// DropEntries returns the TLV entry list list without its entries of tag, every other entry's bytes as stored;
// list must pass the framing rules of the tracepack format specification §5 and hold at least one entry of tag.
func DropEntries(list []byte, tag uint16) ([]byte, error) {
	entries, err := tlv.Decode(list)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}

	out := make([]byte, 0, len(list))
	for _, e := range entries {
		if e.Tag != tag {
			out = append(out, list[e.Offset:e.Offset+tlv.HeaderLen+len(e.Value)]...)
		}
	}
	if len(out) == len(list) {
		return nil, fmt.Errorf("%w: no entry of tag 0x%04X", ErrPackEdit, tag)
	}

	return out, nil
}

// SplitFooter splits the decoded footer d into its sections, as F-1 places them; each part is a copy.
func SplitFooter(d []byte) (*FooterParts, error) {
	if err := needLen(d, format.FooterPrologueLen, "F-1 prologue"); err != nil {
		return nil, err
	}

	n := uint64(len(d))
	count := uint64(binary.LittleEndian.Uint32(d[prologueBlockCountOff:]))
	entryLen := uint64(binary.LittleEndian.Uint32(d[prologueF2EntryLenOff:]))
	f2 := binary.LittleEndian.Uint64(d[prologueF2OffsetOff:])
	f3, f3Len := binary.LittleEndian.Uint64(d[prologueF3OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF3LenOff:])
	f4, f4Len := binary.LittleEndian.Uint64(d[prologueF4OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF4LenOff:])
	f5, f5Len := binary.LittleEndian.Uint64(d[prologueF5OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF5LenOff:])
	// count and entryLen are below 2^32, so their product cannot overflow.
	if entryLen < format.F2EntryLen || !inside(f2, count*entryLen, n) || !inside(f3, f3Len, n) || !inside(f4, f4Len, n) || !inside(f5, f5Len, n) {
		return nil, fmt.Errorf("%w: F-1 places a section outside the %d-byte footer", ErrPackEdit, n)
	}

	p := &FooterParts{
		Prologue: bytes.Clone(d[:format.FooterPrologueLen]),
		F3:       bytes.Clone(d[f3 : f3+f3Len]), F4: bytes.Clone(d[f4 : f4+f4Len]), F5: bytes.Clone(d[f5 : f5+f5Len]),
	}
	for i := range count {
		at := f2 + i*entryLen
		p.Entries = append(p.Entries, bytes.Clone(d[at:at+entryLen]))
	}

	return p, nil
}

// putCRC stores, at off in b, the CRC of b's bytes before off.
func putCRC(b []byte, off int) {
	binary.LittleEndian.PutUint32(b[off:], format.CRC(b[:off]))
}

// needLen reports an error wrapping ErrPackEdit when b is shorter than n bytes.
func needLen(b []byte, n int, what string) error {
	if len(b) < n {
		return fmt.Errorf("%w: %d bytes hold no %s of %d bytes", ErrPackEdit, len(b), what, n)
	}

	return nil
}

// validTrailer reports whether the 64 bytes tr hold the trailer magic and a matching trailer_crc.
func validTrailer(tr []byte) bool {
	return string(tr[trailerMagicOff:]) == format.TrailerMagic &&
		binary.LittleEndian.Uint32(tr[trailerCRCOff:]) == format.CRC(tr[:trailerCRCOff])
}

// appendTrailer appends to dst a copy of the stored trailer tr for the on-disk footer enc of n decoded bytes:
// footer_len, footer_uncompressed_len, footer_crc and trailer_crc recomputed, its other bytes kept.
func appendTrailer(dst, tr, enc []byte, n int) []byte {
	at := len(dst)
	dst = append(dst, tr...)
	t := dst[at:]
	binary.LittleEndian.PutUint64(t[trailerFooterLenOff:], uint64(len(enc)))
	binary.LittleEndian.PutUint64(t[trailerUncompressedOff:], uint64(n))
	binary.LittleEndian.PutUint32(t[trailerFooterCRCOff:], format.CRC(enc))
	putCRC(t, trailerCRCOff)

	return dst
}

// decodeBlock decodes the body of the stored block blk (envelope and body) with its stored codec
// and returns it with the length of its header section, record_count × record_header_len.
func decodeBlock(blk []byte) ([]byte, int, error) {
	// A decoder sizes its output by uncompressed_len before it decodes anything, so the value is bounded first,
	// as a reader bounds it by its block budget.
	n := uint64(binary.LittleEndian.Uint32(blk[envelopeUncompressedLenOff:]))
	if n > tracepack.DefaultMaxBlockLen {
		return nil, 0, fmt.Errorf("%w: uncompressed_len %d is over %d", ErrPackEdit, n, tracepack.DefaultMaxBlockLen)
	}
	decoded, err := codec.Decode(blk[envelopeCodecOff], nil, blk[format.EnvelopeLen:], int(n))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: the body does not decode: %w", ErrPackEdit, err)
	}

	hs := uint64(binary.LittleEndian.Uint32(blk[envelopeRecordCountOff:])) * uint64(binary.LittleEndian.Uint16(blk[envelopeRecordHeaderLenOff:]))
	if hs > uint64(len(decoded)) {
		return nil, 0, fmt.Errorf("%w: a header section of %d bytes exceeds the %d-byte body", ErrPackEdit, hs, len(decoded))
	}

	return decoded, int(hs), nil
}

// replaceBlock returns pack with the stored bytes of block i of its layout l replaced by blk, an envelope and its body.
//
// Relocation rule: in a finalized pack the footer follows the block.
// The block's F-2 entry takes the fields m names from blk's envelope (on_disk_len from blk's length);
// every later F-2 entry's offset, and the trailer's footer_offset, move by the change of the block's length;
// when m copies uncompressed_len, an F-5 content_bytes entry moves by its change.
// A footer whose decoded bytes changed is encoded again with the trailer's footer_codec,
// and the trailer's footer_len, footer_uncompressed_len and footer_crc follow it;
// trailer_crc is recomputed when the trailer changed.
// Nothing else changes: the bytes before and after the block, F-2 offsets before it, F-3 and the other F-5 entries are kept as stored.
// A footer encoded again with codec zstd is the running encoder's output.
func replaceBlock(pack []byte, l *Layout, i int, blk []byte, m mirror) ([]byte, error) {
	s := l.Blocks[i]
	out := slices.Concat(pack[:s.Offset], blk, pack[s.Offset+s.Len:l.End])
	if !l.Finalized {
		return append(out, pack[l.End:]...), nil
	}
	if uint64(len(blk)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: a block of %d bytes does not fit on_disk_len", ErrPackEdit, len(blk))
	}

	decoded, err := footerBytes(pack)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	d := bytes.Clone(decoded)
	delta := uint64(len(blk)) - s.Len // two's complement: adding it moves an offset back when the block shrank
	if err := relocateEntries(d, i, blk, delta, m); err != nil {
		return nil, err
	}

	tr := bytes.Clone(pack[len(pack)-format.TrailerLen:])
	binary.LittleEndian.PutUint64(tr[trailerFooterOffsetOff:], l.End+delta)
	onDisk := pack[l.End : len(pack)-format.TrailerLen]
	if bytes.Equal(d, decoded) {
		if delta != 0 {
			putCRC(tr, trailerCRCOff)
		}

		return slices.Concat(out, onDisk, tr), nil
	}

	enc, err := codec.Encode(tr[trailerFooterCodecOff], nil, d)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}

	return appendTrailer(append(out, enc...), tr, enc, len(d)), nil
}

// relocateEntries applies the relocation rule of replaceBlock to the decoded footer d, in place:
// block i's F-2 entry takes the fields m names from the envelope of blk,
// later entries' offset moves by delta, and F-5 content_bytes by the change of uncompressed_len.
func relocateEntries(d []byte, i int, blk []byte, delta uint64, m mirror) error {
	e, err := F2Entry(d, i)
	if err != nil {
		return err
	}
	entry, env := d[e.Off:e.Off+e.Len], blk[:format.EnvelopeLen]
	oldLen := binary.LittleEndian.Uint32(entry[f2UncompressedLenOff:])
	if m&mirrorOnDisk != 0 {
		binary.LittleEndian.PutUint32(entry[f2OnDiskLenOff:], uint32(len(blk)))
		copy(entry[f2BodyCRCOff:f2BodyCRCOff+4], env[envelopeBodyCRCOff:])
	}
	if m&mirrorDecoded != 0 {
		copy(entry[f2UncompressedLenOff:f2UncompressedLenOff+4], env[envelopeUncompressedLenOff:])
		copy(entry[f2RecordCountOff:f2RecordCountOff+4], env[envelopeRecordCountOff:])
		copy(entry[f2RecordHeaderLenOff:f2RecordHeaderLenOff+2], env[envelopeRecordHeaderLenOff:])
	}
	if m&mirrorFirstSeq != 0 {
		copy(entry[f2FirstSeqOff:f2FirstSeqOff+8], env[envelopeFirstSeqOff:])
	}

	for j := i + 1; ; j++ {
		next, err := F2Entry(d, j)
		if err != nil {
			break
		}
		at := d[next.Off+f2OffsetOff:]
		binary.LittleEndian.PutUint64(at, binary.LittleEndian.Uint64(at)+delta)
	}

	newLen := binary.LittleEndian.Uint32(entry[f2UncompressedLenOff:])
	if newLen == oldLen {
		return nil
	}
	f5, err := F5List(d)
	if err != nil {
		return err
	}
	entries, err := tlv.Decode(d[f5.Off : f5.Off+f5.Len])
	if err != nil {
		return fmt.Errorf("%w: F-5: %w", ErrPackEdit, err)
	}
	for _, c := range entries {
		if c.Tag != f5ContentBytesTag {
			continue
		}
		if len(c.Value) != 8 {
			return fmt.Errorf("%w: F-5 content_bytes holds %d bytes, not 8", ErrPackEdit, len(c.Value))
		}
		binary.LittleEndian.PutUint64(c.Value, binary.LittleEndian.Uint64(c.Value)+uint64(newLen)-uint64(oldLen))

		break
	}

	return nil
}

// block returns the span of block i of l.
func (l *Layout) block(i int) (Span, error) {
	if i < 0 || i >= len(l.Blocks) {
		return Span{}, fmt.Errorf("%w: no block %d among %d", ErrPackEdit, i, len(l.Blocks))
	}

	return l.Blocks[i], nil
}

// encode returns the block of b: a copy of the stored envelope env with the fields PatchBody names recomputed,
// followed by the encoded body.
func (b *Body) encode(env []byte) ([]byte, error) {
	rhl := b.RecordHeaderLen
	if rhl <= 0 || rhl > math.MaxUint16 || len(b.Rows)%rhl != 0 {
		return nil, fmt.Errorf("%w: %d bytes of rows of record_header_len %d", ErrPackEdit, len(b.Rows), rhl)
	}
	count := len(b.Rows) / rhl
	body := append(format.TransposeHeaders(nil, b.Rows, count, rhl), b.Payloads...)
	if uint64(len(body)) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: a body of %d bytes does not fit uncompressed_len", ErrPackEdit, len(body))
	}
	enc, err := codec.Encode(b.Codec, nil, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	if uint64(len(enc)) > math.MaxUint32-format.EnvelopeLen {
		return nil, fmt.Errorf("%w: an encoded body of %d bytes does not fit body_len", ErrPackEdit, len(enc))
	}

	blk := append(bytes.Clone(env), enc...)
	blk[envelopeCodecOff] = b.Codec
	binary.LittleEndian.PutUint16(blk[envelopeRecordHeaderLenOff:], uint16(rhl))
	binary.LittleEndian.PutUint32(blk[envelopeBodyLenOff:], uint32(len(enc)))
	binary.LittleEndian.PutUint32(blk[envelopeUncompressedLenOff:], uint32(len(body)))
	binary.LittleEndian.PutUint32(blk[envelopeRecordCountOff:], uint32(count))
	binary.LittleEndian.PutUint32(blk[envelopeBodyCRCOff:], format.CRC(enc))
	putCRC(blk[:format.EnvelopeLen], blockEnvelopeCRCOff)

	return blk, nil
}

// Join returns the decoded footer of p:
// F-1, F-2 at offset 72, then F-3, F-4 and F-5 in that order without gaps.
// In the prologue only f2_entry_len (when p has entries), f2_offset and the offsets and lengths of F-3, F-4 and F-5 are rewritten;
// block_count and flags keep their stored bytes, and so does every byte of the entries and sections.
// The entries must all have the same length, at least 80 bytes.
func (p *FooterParts) Join() ([]byte, error) {
	if len(p.Prologue) != format.FooterPrologueLen {
		return nil, fmt.Errorf("%w: a prologue of %d bytes", ErrPackEdit, len(p.Prologue))
	}
	pro := bytes.Clone(p.Prologue)
	if len(p.Entries) > 0 {
		n := len(p.Entries[0])
		if n < format.F2EntryLen || uint64(n) > math.MaxUint32 {
			return nil, fmt.Errorf("%w: F-2 entries of %d bytes", ErrPackEdit, n)
		}
		binary.LittleEndian.PutUint32(pro[prologueF2EntryLenOff:], uint32(n))
	}

	out := pro
	for _, e := range p.Entries {
		if len(e) != len(p.Entries[0]) {
			return nil, fmt.Errorf("%w: F-2 entries of different lengths", ErrPackEdit)
		}
		out = append(out, e...)
	}
	f3 := uint64(len(out))
	f4 := f3 + uint64(len(p.F3))
	f5 := f4 + uint64(len(p.F4))
	out = slices.Concat(out, p.F3, p.F4, p.F5)

	binary.LittleEndian.PutUint64(out[prologueF2OffsetOff:], format.FooterPrologueLen)
	binary.LittleEndian.PutUint64(out[prologueF3OffsetOff:], f3)
	binary.LittleEndian.PutUint64(out[prologueF3LenOff:], uint64(len(p.F3)))
	binary.LittleEndian.PutUint64(out[prologueF4OffsetOff:], f4)
	binary.LittleEndian.PutUint64(out[prologueF4LenOff:], uint64(len(p.F4)))
	binary.LittleEndian.PutUint64(out[prologueF5OffsetOff:], f5)
	binary.LittleEndian.PutUint64(out[prologueF5LenOff:], uint64(len(p.F5)))

	return out, nil
}

// SetF3List replaces block i's F-3 entry list in p.F3 with list:
// the entry's summary_len becomes len(list), and every entry whose summary_offset lies at or after the end of the replaced list
// moves by the change of its length; every other byte keeps its stored value.
func (p *FooterParts) SetF3List(i int, list []byte) error {
	if i < 0 || i >= len(p.Entries) || len(p.Entries[i]) < format.F2EntryLen {
		return fmt.Errorf("%w: no F-2 entry %d", ErrPackEdit, i)
	}
	if uint64(len(list)) > math.MaxUint32 {
		return fmt.Errorf("%w: an F-3 list of %d bytes does not fit summary_len", ErrPackEdit, len(list))
	}
	e := p.Entries[i]
	so := binary.LittleEndian.Uint64(e[f2SummaryOffsetOff:])
	sl := uint64(binary.LittleEndian.Uint32(e[f2SummaryLenOff:]))
	if !inside(so, sl, uint64(len(p.F3))) {
		return fmt.Errorf("%w: block %d's F-3 list lies outside F-3", ErrPackEdit, i)
	}

	end := so + sl
	delta := uint64(len(list)) - sl
	p.F3 = slices.Concat(p.F3[:so], list, p.F3[end:])
	binary.LittleEndian.PutUint32(e[f2SummaryLenOff:], uint32(len(list)))
	for j, other := range p.Entries {
		if j == i || len(other) < format.F2EntryLen {
			continue
		}
		if at := binary.LittleEndian.Uint64(other[f2SummaryOffsetOff:]); at >= end {
			binary.LittleEndian.PutUint64(other[f2SummaryOffsetOff:], at+delta)
		}
	}

	return nil
}

// SetF4 sets p's F-4 section to f4 and F-1 flag bit 1, F-4 present, to whether f4 is non-empty;
// extraction_version and the other flag bits keep their stored values.
func (p *FooterParts) SetF4(f4 []byte) {
	p.F4 = bytes.Clone(f4)
	if len(p.Prologue) < prologueFlagsOff+2 {
		return
	}
	flags := binary.LittleEndian.Uint16(p.Prologue[prologueFlagsOff:]) &^ footerFlagF4Present
	if len(f4) > 0 {
		flags |= footerFlagF4Present
	}
	binary.LittleEndian.PutUint16(p.Prologue[prologueFlagsOff:], flags)
}

// of returns the bytes of s in b.
func (s Section) of(b []byte) ([]byte, error) {
	if s.Off < 0 || s.Len < 0 || s.Off > len(b) || s.Len > len(b)-s.Off {
		return nil, fmt.Errorf("%w: section %d+%d outside %d bytes", ErrPackEdit, s.Off, s.Len, len(b))
	}

	return b[s.Off : s.Off+s.Len], nil
}
