package tracepack_test

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// The forward-walk reader below is test-only:
// it reads a pack the way the tracepack format specification I-1 allows a reader to without a footer,
// file header, then pack metadata, then block after block (envelope, body, header section, payload section),
// and checks I-2 on every block, so every writer test asserts through bytes a reader independent of the writer accepts.
// A pack that ends in a valid trailer is finalized:
// its blocks end at footer_offset, and its footer is decoded into walkedFooter,
// which verifyFooter checks against the footer validation list of §10.

// errWalk reports a forward-walk failure that is not already an error of an internal decoder.
var errWalk = errors.New("forward walk")

// walkedBlock is one block of a walked pack.
type walkedBlock struct {
	// Offset is the file offset of the block envelope.
	Offset int
	// Env is the decoded block envelope.
	Env format.BlockEnvelope
	// Body is the on-disk block body.
	Body []byte
	// Records are the block's records, their payloads copied out of the decoded body.
	Records []tracepack.Record
}

// walkedPack is a pack read by walkPack.
type walkedPack struct {
	Header format.FileHeader
	// MetaBytes are the raw pack metadata bytes.
	MetaBytes []byte
	Meta      *tracepack.PackMeta
	Blocks    []walkedBlock
	// Footer is the decoded footer and trailer of a finalized pack; nil for an unfinalized one.
	Footer *walkedFooter
	// Size is the length of the walked file.
	Size int
}

// records returns every record of p in file order.
func (p *walkedPack) records() []tracepack.Record {
	var out []tracepack.Record
	for _, b := range p.Blocks {
		out = append(out, b.Records...)
	}

	return out
}

// mustWalkPack walks b and fails the test on any error;
// a finalized pack's footer must also pass verifyFooter.
func mustWalkPack(t *testing.T, b []byte) *walkedPack {
	t.Helper()

	p, err := walkPack(b)
	require.NoError(t, err)
	if p.Footer != nil {
		verifyFooter(t, p)
	}

	return p
}

// walkPack reads the file header, the pack metadata and every block of b, in file order, until b ends.
func walkPack(b []byte) (*walkedPack, error) {
	h, err := format.UnmarshalFileHeader(b)
	if err != nil {
		return nil, err
	}

	metaEnd := format.FileHeaderLen + int(h.PackMetadataLen)
	if metaEnd > len(b) {
		return nil, fmt.Errorf("%w: pack metadata ends at %d, file is %d bytes", errWalk, metaEnd, len(b))
	}

	metaBytes := b[format.FileHeaderLen:metaEnd]
	if crc := format.CRC(metaBytes); crc != h.PackMetadataCRC {
		return nil, fmt.Errorf("%w: pack metadata CRC 0x%08X, header says 0x%08X", errWalk, crc, h.PackMetadataCRC)
	}

	meta, err := tracepack.UnmarshalPackMeta(metaBytes)
	if err != nil {
		return nil, err
	}

	p := &walkedPack{Header: h, MetaBytes: metaBytes, Meta: meta, Size: len(b)}

	blocksEnd := len(b)
	if tr, err := format.UnmarshalTrailer(b[max(len(b)-format.TrailerLen, 0):]); err == nil && len(b)-format.TrailerLen >= metaEnd {
		f, err := walkFooter(b, &tr)
		if err != nil {
			return nil, fmt.Errorf("footer: %w", err)
		}
		p.Footer = f
		blocksEnd = int(min(tr.FooterOffset, uint64(len(b))))
	}

	for off := metaEnd; off < blocksEnd; {
		blk, next, err := walkBlock(b[:blocksEnd], off)
		if err != nil {
			return nil, fmt.Errorf("block at %d: %w", off, err)
		}

		p.Blocks = append(p.Blocks, blk)
		off = next
	}

	return p, nil
}

// walkBlock reads the block whose envelope starts at b[off:] and returns it with the offset after its body.
func walkBlock(b []byte, off int) (walkedBlock, int, error) {
	env, err := format.UnmarshalBlockEnvelope(b[off:])
	if err != nil {
		return walkedBlock{}, 0, err
	}

	bodyStart := off + format.EnvelopeLen
	bodyEnd := bodyStart + int(env.BodyLen)
	if bodyEnd > len(b) {
		return walkedBlock{}, 0, fmt.Errorf("%w: body ends at %d, file is %d bytes", errWalk, bodyEnd, len(b))
	}

	body := b[bodyStart:bodyEnd]
	if crc := format.CRC(body); crc != env.BodyCRC {
		return walkedBlock{}, 0, fmt.Errorf("%w: body CRC 0x%08X, envelope says 0x%08X", errWalk, crc, env.BodyCRC)
	}

	decoded, err := codec.Decode(env.Codec, nil, body, int(env.UncompressedLen))
	if err != nil {
		return walkedBlock{}, 0, err
	}

	records, err := walkRecords(&env, decoded)
	if err != nil {
		return walkedBlock{}, 0, err
	}

	return walkedBlock{Offset: off, Env: env, Body: body, Records: records}, bodyEnd, nil
}

// walkRecords splits a decoded body into its records and checks the I-2 equations on them.
func walkRecords(env *format.BlockEnvelope, decoded []byte) ([]tracepack.Record, error) {
	hsLen, err := format.HeaderSectionLen(env.RecordCount, env.RecordHeaderLen)
	if err != nil {
		return nil, err
	}
	if hsLen > len(decoded) {
		return nil, fmt.Errorf("%w: header section %d bytes, body %d", errWalk, hsLen, len(decoded))
	}

	hlen := int(env.RecordHeaderLen)
	records := make([]tracepack.Record, 0, env.RecordCount)
	payloadOff := hsLen

	for i := range int(env.RecordCount) {
		h, err := format.UnmarshalRecordHeader(decoded[i*hlen:], hlen)
		if err != nil {
			return nil, err
		}

		end := payloadOff + int(h.PayloadLen)
		if end > len(decoded) {
			return nil, fmt.Errorf("%w: record %d payload ends at %d, body %d", errWalk, i, end, len(decoded))
		}

		if err := checkSeqOrder(env, records, i, h.Seq); err != nil {
			return nil, err
		}

		records = append(records, recordOf(&h, bytes.Clone(decoded[payloadOff:end])))
		payloadOff = end
	}

	if payloadOff != len(decoded) {
		return nil, fmt.Errorf("%w: headers and payloads cover %d bytes, uncompressed_len %d", errWalk, payloadOff, len(decoded))
	}

	return records, nil
}

// checkSeqOrder checks the seq rules of I-2 for record i: the first equals first_seq, later ones strictly increase.
func checkSeqOrder(env *format.BlockEnvelope, prev []tracepack.Record, i int, seq uint64) error {
	if i == 0 && seq != env.FirstSeq {
		return fmt.Errorf("%w: first seq %d, envelope first_seq %d", errWalk, seq, env.FirstSeq)
	}
	if i > 0 && seq <= prev[i-1].Seq {
		return fmt.Errorf("%w: record %d seq %d after %d", errWalk, i, seq, prev[i-1].Seq)
	}

	return nil
}

// recordOf converts a decoded record header and its payload into a Record.
func recordOf(h *format.RecordHeader, payload []byte) tracepack.Record {
	flags := tracepack.RecordFlags(h.RecordFlags)

	return tracepack.Record{
		Seq:           h.Seq,
		TSUTCNs:       h.TSUTCNs,
		MonoNs:        h.MonoNs,
		MonoPresent:   flags.Has(tracepack.RecordFlagsMonoPresent),
		Epoch:         h.Epoch,
		Kind:          tracepack.Kind(h.Kind),
		Dir:           tracepack.Dir(h.Dir),
		Fidelity:      tracepack.Fidelity(h.Fidelity),
		DecodeStatus:  tracepack.DecodeStatus(h.DecodeStatus),
		TrailingBytes: h.TrailingBytes,
		SystemBytes:   h.SystemBytes,
		SessionID:     h.SessionID,
		Stream:        h.Stream,
		W:             flags.Has(tracepack.RecordFlagsW),
		Function:      h.Function,
		PType:         h.PType,
		SType:         h.SType,
		Quality:       tracepack.Quality(h.Quality),
		FieldValidity: tracepack.FieldValidity(h.FieldValidity),
		Payload:       payload,
	}
}

// walkedFooter is the decoded footer and trailer of a finalized pack.
type walkedFooter struct {
	Trailer format.Trailer
	// Decoded is the decoded footer.
	Decoded  []byte
	Prologue format.FooterPrologue
	F2       []format.F2Entry
	// F3 holds each block's summary, in F-2 order.
	F3 []packStat
	F5 packStat
}

// epochStat is a nested epoch entry of F-3 or F-5.
type epochStat struct {
	Epoch       uint64
	RecordCount uint64
	SeqFirst    uint64
	SeqLast     uint64
	TSMin       int64
	TSMax       int64
	CloseSeq    *uint64
}

// boundaryStat is a nested boundary entry of F-3 or F-5.
type boundaryStat struct {
	Seq      uint64
	Kind     uint8
	TS       int64
	Epoch    uint64
	GapStart *int64
	GapEnd   *int64
}

// packStat is the typed content of an F-3 block summary or of the F-5 pack statistics.
// A field only one of the two lists carries is nil in the other,
// and every count array is widened to u64.
type packStat struct {
	RecordCount        *uint64 // F-5 only
	KindCounts         []uint64
	DirCounts          []uint64
	DecodeStatusCounts []uint64
	MaxPayloadLen      *uint64 // F-3 only
	QualityUnion       uint64
	ContentBytes       uint64
	TSMin              *int64 // F-5 only
	TSMax              *int64 // F-5 only
	SeqRanges          []tlv.SeqRange
	Epochs             []epochStat
	Boundaries         []boundaryStat
}

// statTags are the tags of the F-3 or the F-5 entry list; tag 0, which no entry carries, marks a tag the list lacks.
type statTags struct {
	recordCount, kind, dir, decodeStatus, maxPayloadLen, qualityUnion uint16
	contentBytes, tsMin, tsMax, seqRange, epoch, boundary             uint16
	// countWidth is the element width of the count arrays: 4 in F-3, 8 in F-5.
	countWidth int
}

// f3Tags and f5Tags follow the tag tables of the tracepack format specification §10.
var (
	f3Tags = statTags{
		kind: 0x01, dir: 0x02, decodeStatus: 0x03, maxPayloadLen: 0x04, qualityUnion: 0x05,
		contentBytes: 0x06, epoch: 0x07, boundary: 0x08, seqRange: 0x09, countWidth: 4,
	}
	f5Tags = statTags{
		recordCount: 0x01, kind: 0x02, dir: 0x03, decodeStatus: 0x04, tsMin: 0x05, tsMax: 0x06,
		contentBytes: 0x07, qualityUnion: 0x08, seqRange: 0x09, epoch: 0x0A, boundary: 0x0B, countWidth: 8,
	}
)

// walkFooter reads the footer the trailer tr of b locates:
// it checks footer_crc and that the footer ends where the trailer starts,
// decodes it with footer_codec, and parses F-1, every F-2 entry, every F-3 list and F-5,
// validating each TLV list against its registry.
// Section order and every other item of the footer validation list are verifyFooter's.
func walkFooter(b []byte, tr *format.Trailer) (*walkedFooter, error) {
	end := uint64(len(b) - format.TrailerLen)
	if tr.FooterOffset > end || tr.FooterLen != end-tr.FooterOffset {
		return nil, fmt.Errorf("%w: footer at %d of %d bytes does not end at the trailer, %d", errWalk, tr.FooterOffset, tr.FooterLen, end)
	}

	onDisk := b[tr.FooterOffset:end]
	if crc := format.CRC(onDisk); crc != tr.FooterCRC {
		return nil, fmt.Errorf("%w: footer CRC 0x%08X, trailer says 0x%08X", errWalk, crc, tr.FooterCRC)
	}

	decoded, err := codec.Decode(tr.FooterCodec, nil, onDisk, int(tr.FooterUncompressedLen))
	if err != nil {
		return nil, err
	}

	pro, err := format.UnmarshalFooterPrologue(decoded)
	if err != nil {
		return nil, err
	}

	f := &walkedFooter{Trailer: *tr, Decoded: decoded, Prologue: pro}
	if err := f.parseSections(); err != nil {
		return nil, err
	}

	return f, nil
}

// section returns b[off:off+n], or an error when it does not lie inside b.
func section(b []byte, off, n uint64) ([]byte, error) {
	if off > uint64(len(b)) || n > uint64(len(b))-off {
		return nil, fmt.Errorf("%w: section at %d of %d bytes outside %d bytes", errWalk, off, n, len(b))
	}

	return b[off : off+n], nil
}

// parseSections parses F-2, F-3 and F-5 of the decoded footer.
func (f *walkedFooter) parseSections() error {
	pro := &f.Prologue

	f2, err := section(f.Decoded, pro.F2Offset, uint64(pro.BlockCount)*uint64(pro.F2EntryLen))
	if err != nil {
		return fmt.Errorf("F-2: %w", err)
	}
	f3, err := section(f.Decoded, pro.F3Offset, pro.F3Len)
	if err != nil {
		return fmt.Errorf("F-3: %w", err)
	}

	for i := range int(pro.BlockCount) {
		e, err := format.UnmarshalF2Entry(f2[i*int(pro.F2EntryLen):], int(pro.F2EntryLen))
		if err != nil {
			return err
		}
		f.F2 = append(f.F2, e)

		list, err := section(f3, e.SummaryOffset, uint64(e.SummaryLen))
		if err != nil {
			return fmt.Errorf("F-3 list of block %d: %w", i, err)
		}
		s, err := parseStat(list, tlv.F3, &f3Tags)
		if err != nil {
			return fmt.Errorf("F-3 list of block %d: %w", i, err)
		}
		f.F3 = append(f.F3, s)
	}

	f5, err := section(f.Decoded, pro.F5Offset, pro.F5Len)
	if err != nil {
		return fmt.Errorf("F-5: %w", err)
	}
	f.F5, err = parseStat(f5, tlv.F5, &f5Tags)
	if err != nil {
		return fmt.Errorf("F-5: %w", err)
	}

	return nil
}

// parseStat decodes an F-3 or F-5 entry list, validates it against reg, and types it by tags.
func parseStat(b []byte, reg tlv.Registry, tags *statTags) (packStat, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return packStat{}, err
	}
	if err := tlv.Validate(entries, reg); err != nil {
		return packStat{}, err
	}

	var s packStat
	for _, e := range entries {
		if err := s.set(e, tags); err != nil {
			return packStat{}, fmt.Errorf("tag 0x%04X: %w", e.Tag, err)
		}
	}

	return s, nil
}

// set stores one validated entry of an F-3 or F-5 list in s.
func (s *packStat) set(e tlv.Entry, tags *statTags) error {
	var err error

	switch e.Tag {
	case tags.recordCount:
		s.RecordCount = new(entryU64(e))
	case tags.kind:
		s.KindCounts, err = decodeCounts(e.Value, tags.countWidth)
	case tags.dir:
		s.DirCounts, err = decodeCounts(e.Value, tags.countWidth)
	case tags.decodeStatus:
		s.DecodeStatusCounts, err = decodeCounts(e.Value, tags.countWidth)
	case tags.maxPayloadLen:
		s.MaxPayloadLen = new(entryU64(e))
	case tags.qualityUnion:
		s.QualityUnion = entryU64(e)
	case tags.contentBytes:
		s.ContentBytes = entryU64(e)
	case tags.tsMin:
		s.TSMin = new(entryI64(e))
	case tags.tsMax:
		s.TSMax = new(entryI64(e))
	case tags.seqRange:
		var r tlv.SeqRange
		r, err = tlv.DecodeSeqRange(e.Value)
		s.SeqRanges = append(s.SeqRanges, r)
	case tags.epoch:
		var ep epochStat
		ep, err = parseEpoch(e)
		s.Epochs = append(s.Epochs, ep)
	case tags.boundary:
		var bd boundaryStat
		bd, err = parseBoundary(e)
		s.Boundaries = append(s.Boundaries, bd)
	default:
		// An unknown tag carries nothing this reader checks.
	}

	return err
}

// entryU64 returns the value of a u64 entry that tlv.Validate accepted.
func entryU64(e tlv.Entry) uint64 {
	v, err := e.U64()
	if err != nil {
		panic(err)
	}

	return v
}

// entryI64 returns the value of an i64 entry that tlv.Validate accepted.
func entryI64(e tlv.Entry) int64 {
	v, err := e.I64()
	if err != nil {
		panic(err)
	}

	return v
}

// decodeCounts decodes a u32 (width 4) or u64 (width 8) count array, widened to u64.
func decodeCounts(b []byte, width int) ([]uint64, error) {
	if width == 8 {
		return tlv.DecodeU64Array(b)
	}

	u32s, err := tlv.DecodeU32Array(b)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, len(u32s))
	for i, v := range u32s {
		out[i] = uint64(v)
	}

	return out, nil
}

// parseEpoch types a nested epoch entry that tlv.Validate accepted against the Epoch registry.
func parseEpoch(e tlv.Entry) (epochStat, error) {
	nested, err := e.Nested()
	if err != nil {
		return epochStat{}, err
	}

	var ep epochStat
	for _, n := range nested {
		switch n.Tag {
		case 0x01:
			ep.Epoch = entryU64(n)
		case 0x02:
			ep.RecordCount = entryU64(n)
		case 0x03:
			ep.SeqFirst = entryU64(n)
		case 0x04:
			ep.SeqLast = entryU64(n)
		case 0x05:
			ep.TSMin = entryI64(n)
		case 0x06:
			ep.TSMax = entryI64(n)
		case 0x07:
			ep.CloseSeq = new(entryU64(n))
		default:
			// An unknown nested tag carries nothing this reader checks.
		}
	}

	return ep, nil
}

// parseBoundary types a nested boundary entry that tlv.Validate accepted against the Boundary registry.
func parseBoundary(e tlv.Entry) (boundaryStat, error) {
	nested, err := e.Nested()
	if err != nil {
		return boundaryStat{}, err
	}

	var bd boundaryStat
	for _, n := range nested {
		switch n.Tag {
		case 0x01:
			bd.Seq = entryU64(n)
		case 0x02:
			bd.Kind = n.Value[0]
		case 0x03:
			bd.TS = entryI64(n)
		case 0x04:
			bd.Epoch = entryU64(n)
		case 0x05:
			bd.GapStart = new(entryI64(n))
		case 0x06:
			bd.GapEnd = new(entryI64(n))
		default:
			// An unknown nested tag carries nothing this reader checks.
		}
	}

	return bd, nil
}

// verifyFooter asserts every item of the footer validation list of the tracepack format specification §10 on p,
// which must be finalized,
// plus the agreement of each F-2 entry with the envelope the forward walk read at its offset,
// and the trailer fields that locate and describe the footer (§11).
// It recomputes F-5 from the parsed F-2 and F-3 values with its own aggregation, aggregateStats.
func verifyFooter(t *testing.T, p *walkedPack) {
	t.Helper()

	require.NotNil(t, p.Footer, "the pack ends in a valid trailer")
	f := p.Footer

	verifyPrologue(t, f)
	verifyTrailer(t, p)
	verifyBlockIndex(t, p)
	for i := range f.F2 {
		verifyBlockSummary(t, &f.F2[i], &f.F3[i], i)
	}

	assert.Equal(t, aggregateStats(t, f.F2, f.F3), f.F5, "F-5 equals the aggregate of F-2 and F-3")

	var sum uint64
	for _, e := range f.F2 {
		sum = checkedAdd(t, sum, uint64(e.RecordCount))
	}
	require.NotNil(t, f.F5.RecordCount)
	assert.Equal(t, sum, *f.F5.RecordCount, "Σ F-2 record_count = F-5 record_count")
	assert.Equal(t, sum, f.Trailer.RecordCount, "Σ F-2 record_count = trailer record_count")

	if len(f.F2) == 0 {
		assert.Equal(t, uint64(format.FileHeaderLen)+uint64(p.Header.PackMetadataLen), f.Trailer.FooterOffset,
			"a pack without blocks has footer_offset = 80 + pack_metadata_len")
		assert.Nil(t, f.F5.TSMin, "no F-5 ts_min without blocks")
		assert.Nil(t, f.F5.TSMax, "no F-5 ts_max without blocks")
		assert.Empty(t, f.F5.SeqRanges, "no F-5 seq_range without blocks")
		assert.Empty(t, f.F5.Epochs, "no F-5 epoch without blocks")
		assert.Empty(t, f.F5.Boundaries, "no F-5 boundary without blocks")

		return
	}

	assert.Equal(t, f.F2[len(f.F2)-1].LastSeq, f.Trailer.LastSeq, "trailer last_seq = the last F-2 entry's last_seq")
}

// verifyPrologue asserts the F-1 items of the validation list and the section layout of the decoded footer.
func verifyPrologue(t *testing.T, f *walkedFooter) {
	t.Helper()

	pro := &f.Prologue
	assert.Equal(t, format.FooterLayoutVersion, pro.FooterLayoutVersion)
	assert.Equal(t, uint16(1), pro.Flags, "F-3 present, F-4 absent")
	assert.Equal(t, uint64(format.FooterPrologueLen), pro.F2Offset, "f2_offset = 72")
	assert.GreaterOrEqual(t, pro.F2EntryLen, uint32(format.F2EntryLen), "f2_entry_len ≥ 80")
	assert.Equal(t, uint32(0), pro.ExtractionVersion, "extraction_version is 0 without F-4")
	assert.Zero(t, pro.F4Len, "F-4 absent")

	// F-2, F-3, F-4 and F-5 lie inside the decoded footer, in that order, without overlap.
	f2End := pro.F2Offset + uint64(pro.BlockCount)*uint64(pro.F2EntryLen)
	assert.LessOrEqual(t, f2End, pro.F3Offset, "F-2 ends before F-3")
	assert.LessOrEqual(t, pro.F3Offset+pro.F3Len, pro.F4Offset, "F-3 ends before F-4")
	assert.LessOrEqual(t, pro.F4Offset+pro.F4Len, pro.F5Offset, "F-4 ends before F-5")
	assert.LessOrEqual(t, pro.F5Offset+pro.F5Len, uint64(len(f.Decoded)), "F-5 ends inside the footer")

	// This writer leaves no gap between sections.
	assert.Equal(t, f2End, pro.F3Offset)
	assert.Equal(t, pro.F3Offset+pro.F3Len, pro.F4Offset)
	assert.Equal(t, pro.F4Offset, pro.F5Offset)
	assert.Equal(t, uint64(len(f.Decoded)), pro.F5Offset+pro.F5Len)
}

// verifyTrailer asserts the trailer fields walkFooter did not already check.
// UnmarshalTrailer checked the magic and trailer_crc, and walkFooter checked footer_crc over the on-disk footer.
func verifyTrailer(t *testing.T, p *walkedPack) {
	t.Helper()

	tr := &p.Footer.Trailer
	assert.Equal(t, format.TrailerVersion, tr.TrailerVersion)
	assert.Equal(t, uint64(p.Size-format.TrailerLen), tr.FooterOffset+tr.FooterLen, "footer_offset + footer_len = size - 64")
	assert.Equal(t, uint64(len(p.Footer.Decoded)), tr.FooterUncompressedLen)
	assert.Equal(t, p.Footer.Prologue.BlockCount, tr.BlockCount, "block_count agrees in F-1 and the trailer")
	assert.Len(t, p.Blocks, int(tr.BlockCount), "the forward walk finds block_count blocks")
	assert.Zero(t, tr.Flags)
}

// verifyBlockIndex asserts the F-2 items of the validation list,
// and that each F-2 entry agrees with the envelope at its offset.
func verifyBlockIndex(t *testing.T, p *walkedPack) {
	t.Helper()

	f := p.Footer
	next := uint64(format.FileHeaderLen) + uint64(p.Header.PackMetadataLen)
	for i, e := range f.F2 {
		assert.Equal(t, next, e.Offset, "block %d starts where the previous one ends", i)
		next = checkedAdd(t, next, uint64(e.OnDiskLen))

		assert.LessOrEqual(t, e.FirstSeq, e.LastSeq, "block %d first_seq ≤ last_seq", i)
		if i > 0 {
			assert.Greater(t, e.FirstSeq, f.F2[i-1].LastSeq, "block %d seqs ascend across entries", i)
		}
		assert.Positive(t, e.SummaryLen, "block %d summary_len", i)
		assert.LessOrEqual(t, e.SummaryOffset+uint64(e.SummaryLen), f.Prologue.F3Len, "block %d summary inside F-3", i)
		assert.GreaterOrEqual(t, e.RecordHeaderLen, uint16(format.RecordHeaderLen), "block %d record_header_len ≥ 56", i)
		assert.LessOrEqual(t, e.TSMin, e.TSMax, "block %d ts_min ≤ ts_max", i)
		assert.Equal(t, floorDiv(e.TSMin, hourNs), floorDiv(e.TSMax, hourNs), "block %d ts_min and ts_max in one UTC hour", i)
		assert.LessOrEqual(t, e.EpochMin, e.EpochMax, "block %d epoch_min ≤ epoch_max", i)

		if i >= len(p.Blocks) {
			continue
		}
		env, blk := &p.Blocks[i].Env, &p.Blocks[i]
		assert.Equal(t, uint64(blk.Offset), e.Offset, "block %d offset", i)
		assert.Equal(t, uint32(format.EnvelopeLen)+env.BodyLen, e.OnDiskLen, "block %d on_disk_len", i)
		assert.Equal(t, env.UncompressedLen, e.UncompressedLen, "block %d uncompressed_len", i)
		assert.Equal(t, env.RecordCount, e.RecordCount, "block %d record_count", i)
		assert.Equal(t, env.BodyCRC, e.BodyCRC, "block %d body_crc", i)
		assert.Equal(t, env.FirstSeq, e.FirstSeq, "block %d first_seq", i)
		assert.Equal(t, env.RecordHeaderLen, e.RecordHeaderLen, "block %d record_header_len", i)
	}
	assert.Equal(t, next, f.Trailer.FooterOffset, "the last block ends at footer_offset")
}

// verifyBlockSummary asserts the per-block F-3 items of the validation list for block i.
func verifyBlockSummary(t *testing.T, e *format.F2Entry, s *packStat, i int) {
	t.Helper()

	count := uint64(e.RecordCount)
	if len(s.SeqRanges) > 0 {
		var members uint64
		for j, r := range s.SeqRanges {
			assert.LessOrEqual(t, r.First, r.Last, "block %d seq_range %d", i, j)
			if j > 0 {
				assert.Greater(t, r.First, s.SeqRanges[j-1].Last+1, "block %d seq_range %d sorted, non-overlapping, maximal", i, j)
			}
			members = checkedAdd(t, members, r.Last-r.First+1)
		}
		assert.Equal(t, e.FirstSeq, s.SeqRanges[0].First, "block %d seq_range starts at first_seq", i)
		assert.Equal(t, e.LastSeq, s.SeqRanges[len(s.SeqRanges)-1].Last, "block %d seq_range ends at last_seq", i)
		assert.Equal(t, count, members, "block %d seq_range members = record_count", i)
	} else {
		assert.Equal(t, count, e.LastSeq-e.FirstSeq+1, "block %d without seq_range: last - first + 1 = record_count", i)
	}

	for name, counts := range map[string][]uint64{"kind": s.KindCounts, "dir": s.DirCounts, "decode_status": s.DecodeStatusCounts} {
		assert.Equal(t, count, sumChecked(t, counts), "block %d %s_counts sum to record_count", i, name)
	}
	assert.Equal(t, uint64(e.UncompressedLen), s.ContentBytes, "block %d content_bytes = uncompressed_len", i)

	var epochRecords uint64
	for _, ep := range s.Epochs {
		epochRecords = checkedAdd(t, epochRecords, ep.RecordCount)
		assert.True(t, uint64(e.EpochMin) <= ep.Epoch && ep.Epoch <= uint64(e.EpochMax), "block %d epoch %d inside epoch bounds", i, ep.Epoch)
		assert.True(t, e.FirstSeq <= ep.SeqFirst && ep.SeqFirst <= ep.SeqLast && ep.SeqLast <= e.LastSeq, "block %d epoch %d inside seq bounds", i, ep.Epoch)
		assert.True(t, e.TSMin <= ep.TSMin && ep.TSMin <= ep.TSMax && ep.TSMax <= e.TSMax, "block %d epoch %d inside time bounds", i, ep.Epoch)
		if ep.CloseSeq != nil {
			assert.True(t, inSeqSet(*ep.CloseSeq, e, s), "block %d epoch %d close_seq in the block's seq set", i, ep.Epoch)
		}
	}
	assert.Equal(t, count, epochRecords, "block %d epoch record counts sum to record_count", i)

	for _, b := range s.Boundaries {
		assert.True(t, inSeqSet(b.Seq, e, s), "block %d boundary seq %d in the block's seq set", i, b.Seq)
	}
}

// inSeqSet reports whether seq is one of the block's seqs: its seq_range list, or first_seq..last_seq without one.
func inSeqSet(seq uint64, e *format.F2Entry, s *packStat) bool {
	if len(s.SeqRanges) == 0 {
		return e.FirstSeq <= seq && seq <= e.LastSeq
	}

	return slices.ContainsFunc(s.SeqRanges, func(r tlv.SeqRange) bool { return r.First <= seq && seq <= r.Last })
}

// checkedAdd returns a + b, failing the test when the sum exceeds format.MaxU64:
// the validation list requires overflow-checked sums.
func checkedAdd(t *testing.T, a, b uint64) uint64 {
	t.Helper()

	sum, carry := bits.Add64(a, b, 0)
	require.Zero(t, carry, "sum overflows")
	require.LessOrEqual(t, sum, format.MaxU64, "sum above 2^63-1")

	return sum
}

// sumChecked returns the overflow-checked sum of v.
func sumChecked(t *testing.T, v []uint64) uint64 {
	t.Helper()

	var sum uint64
	for _, x := range v {
		sum = checkedAdd(t, sum, x)
	}

	return sum
}

// floorDiv divides rounding toward negative infinity.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}

	return q
}

// aggregateStats recomputes F-5 from F-2 and F-3 by the aggregation rule of the tracepack format specification §10,
// independently of the writer:
// counts and content_bytes summed with overflow checks, ts extremes from F-2, seq ranges unioned and coalesced,
// epoch entries combined per epoch, in ascending epoch order and keeping the smallest close_seq,
// and boundary entries concatenated in block order.
func aggregateStats(t *testing.T, f2 []format.F2Entry, f3 []packStat) packStat {
	t.Helper()

	var count uint64
	out := packStat{RecordCount: &count, KindCounts: []uint64{}, DirCounts: []uint64{}, DecodeStatusCounts: []uint64{}}
	for i := range f2 {
		e, s := &f2[i], &f3[i]
		count = checkedAdd(t, count, uint64(e.RecordCount))
		out.KindCounts = addCounts(t, out.KindCounts, s.KindCounts)
		out.DirCounts = addCounts(t, out.DirCounts, s.DirCounts)
		out.DecodeStatusCounts = addCounts(t, out.DecodeStatusCounts, s.DecodeStatusCounts)
		out.ContentBytes = checkedAdd(t, out.ContentBytes, s.ContentBytes)
		out.QualityUnion |= s.QualityUnion
		if i == 0 {
			out.TSMin, out.TSMax = new(e.TSMin), new(e.TSMax)
		}
		out.TSMin, out.TSMax = new(min(*out.TSMin, e.TSMin)), new(max(*out.TSMax, e.TSMax))

		ranges := s.SeqRanges
		if len(ranges) == 0 {
			ranges = []tlv.SeqRange{{First: e.FirstSeq, Last: e.LastSeq}}
		}
		for _, r := range ranges {
			out.SeqRanges = appendCoalesced(out.SeqRanges, r)
		}

		for _, ep := range s.Epochs {
			out.Epochs = mergeEpoch(t, out.Epochs, ep)
		}
		out.Boundaries = append(out.Boundaries, s.Boundaries...)
	}
	slices.SortFunc(out.Epochs, func(a, b epochStat) int { return cmp.Compare(a.Epoch, b.Epoch) })

	return out
}

// addCounts adds the count array add to sum element by element, extending sum as needed.
func addCounts(t *testing.T, sum, add []uint64) []uint64 {
	t.Helper()

	for len(sum) < len(add) {
		sum = append(sum, 0)
	}
	for i, v := range add {
		sum[i] = checkedAdd(t, sum[i], v)
	}

	return sum
}

// appendCoalesced appends r to ranges, which end below r.First, merging it into the last range when they touch.
func appendCoalesced(ranges []tlv.SeqRange, r tlv.SeqRange) []tlv.SeqRange {
	if n := len(ranges); n > 0 && ranges[n-1].Last+1 == r.First {
		ranges[n-1].Last = r.Last
		return ranges
	}

	return append(ranges, r)
}

// mergeEpoch combines ep into the entry of the same epoch in epochs, or appends it.
func mergeEpoch(t *testing.T, epochs []epochStat, ep epochStat) []epochStat {
	t.Helper()

	i := slices.IndexFunc(epochs, func(e epochStat) bool { return e.Epoch == ep.Epoch })
	if i < 0 {
		return append(epochs, ep)
	}

	m := &epochs[i]
	m.RecordCount = checkedAdd(t, m.RecordCount, ep.RecordCount)
	m.SeqFirst, m.SeqLast = min(m.SeqFirst, ep.SeqFirst), max(m.SeqLast, ep.SeqLast)
	m.TSMin, m.TSMax = min(m.TSMin, ep.TSMin), max(m.TSMax, ep.TSMax)
	if ep.CloseSeq != nil && (m.CloseSeq == nil || *ep.CloseSeq < *m.CloseSeq) {
		m.CloseSeq = ep.CloseSeq
	}

	return epochs
}
