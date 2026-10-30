package corpus

import (
	"bytes"
	"cmp"
	"context"
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

// Positions the structural footer reader reads, from the file header, the trailer, F-1 and an F-2 entry
// (the tracepack format specification §4, §10 and §11).
const (
	headerPackMetadataLenOff = 16
	headerCaptureIDOff       = 48

	trailerFooterOffsetOff = 0
	trailerFooterLenOff    = 8
	trailerUncompressedOff = 16
	trailerFooterCRCOff    = 28
	trailerFooterCodecOff  = 50
	trailerCRCOff          = 52
	trailerMagicOff        = 56

	prologueBlockCountOff = 4
	prologueF2EntryLenOff = 8
	prologueF2OffsetOff   = 16
	prologueF3OffsetOff   = 24
	prologueF3LenOff      = 32
	prologueF5OffsetOff   = 56
	prologueF5LenOff      = 64

	f2SummaryOffsetOff = 64
	f2SummaryLenOff    = 72
)

// Tags the projection reads: the epoch and boundary entries of an F-3 list and of F-5, F-5's seq ranges, and their nested tags
// (the tracepack format specification §10).
const (
	f3EpochTag    uint16 = 0x0007
	f3BoundaryTag uint16 = 0x0008
	// seqRangeTag is the tag of seq_range in F-3 and in F-5.
	seqRangeTag   uint16 = 0x0009
	f5EpochTag    uint16 = 0x000A
	f5BoundaryTag uint16 = 0x000B

	epochEpochTag    uint16 = 0x0001
	epochCloseSeqTag uint16 = 0x0007

	boundarySeqTag      uint16 = 0x0001
	boundaryKindTag     uint16 = 0x0002
	boundaryGapStartTag uint16 = 0x0005
	boundaryGapEndTag   uint16 = 0x0006
)

// seqRangeLen is the length of an F-5 seq_range value: first and last, two u64 values.
const seqRangeLen = 16

// ErrFooterUnreadable reports a pack whose footer is not structurally readable (the tracepack corpus specification §5.8),
// so footer.json has no stored part.
var ErrFooterUnreadable = errors.New("corpus: footer not structurally readable")

// Footer is footer.json (the tracepack corpus specification §5.8).
type Footer struct {
	// Stored is present iff the footer is structurally readable.
	Stored     *FooterProjection `json:"stored,omitempty"`
	Accepted   bool              `json:"accepted"`
	Recomputed FooterProjection  `json:"recomputed"`
}

// FooterProjection is the projection of a footer's F-3 and F-5 values that footer.json states.
type FooterProjection struct {
	Blocks []FooterBlock `json:"blocks"`
	F5     FooterF5      `json:"f5"`
}

// FooterBlock is the projection of one block's F-3 list:
// the close_seq of its epoch entries that carry one, and its boundary entries.
type FooterBlock struct {
	Block      int        `json:"block"`
	CloseSeqs  []CloseSeq `json:"close_seqs"`
	Boundaries []Boundary `json:"boundaries"`
}

// CloseSeq is the close_seq of one epoch entry.
type CloseSeq struct {
	Epoch uint32 `json:"epoch"`
	Seq   U64    `json:"seq"`
}

// Boundary is one boundary entry: its seq, its boundary_kind name, and its gap_start and gap_end, each present iff the entry has it.
type Boundary struct {
	Seq      U64    `json:"seq"`
	Kind     string `json:"kind"`
	GapStart *I64   `json:"gap_start,omitempty"`
	GapEnd   *I64   `json:"gap_end,omitempty"`
}

// FooterF5 is the projection of F-5: one object per epoch entry, the boundary entries and the seq_range entries.
type FooterF5 struct {
	Epochs     []F5Epoch  `json:"epochs"`
	Boundaries []Boundary `json:"boundaries"`
	SeqRanges  []SeqRange `json:"seq_ranges"`
}

// SeqRange is one F-5 seq_range entry: the first and last seq of a range.
type SeqRange struct {
	First U64 `json:"first"`
	Last  U64 `json:"last"`
}

// F5Epoch is one F-5 epoch entry: its epoch and its close_seq, if any.
type F5Epoch struct {
	Epoch    uint32 `json:"epoch"`
	CloseSeq *U64   `json:"close_seq,omitempty"`
}

// projection is a footer projection before it is sorted and named.
type projection struct {
	blocks      []blockProjection
	f5Epochs    []epochEntry
	f5Boundary  []boundaryEntry
	f5SeqRanges []tlv.SeqRange
}

// blockProjection is the projection of one block before it is sorted and named.
type blockProjection struct {
	block      int
	epochs     []epochEntry
	boundaries []boundaryEntry
}

// epochEntry is an epoch entry: its epoch, and its close_seq when hasClose.
type epochEntry struct {
	epoch    uint32
	closeSeq uint64
	hasClose bool
}

// boundaryEntry is a boundary entry: its seq, its numeric boundary_kind, and its gap_start and gap_end, nil when absent.
type boundaryEntry struct {
	seq              uint64
	kind             uint8
	gapStart, gapEnd *int64
}

// FooterOf returns the footer.json of pack:
// the stored projection when its footer is structurally readable,
// whether the tracepack package accepts the footer, and the projection the records of its validated blocks give.
//
// Returns:
//   - Footer: the expectation.
//   - error: the error of Open, a footer the reader's budget kept out, which is never a golden,
//     or the error of ReadStoredFooter or RecomputeFooter other than ErrFooterUnreadable.
func FooterOf(ctx context.Context, pack []byte) (Footer, error) {
	r, err := tracepack.Open(ctx, bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	if err != nil {
		return Footer{}, err
	}
	h := r.Header()
	if errors.Is(h.FooterErr, tracepack.ErrReadLimit) {
		return Footer{}, fmt.Errorf("corpus: the footer is over a reader budget: %w", h.FooterErr)
	}

	var out Footer
	stored, err := ReadStoredFooter(pack)
	switch {
	case err == nil:
		out.Stored = &stored
	case errors.Is(err, ErrFooterUnreadable):
		// No stored part: footer.json omits it.
	default:
		return Footer{}, err
	}
	out.Accepted = h.Finalized && h.FooterErr == nil
	if out.Recomputed, err = RecomputeFooter(ctx, r); err != nil {
		return Footer{}, err
	}

	return out, nil
}

// ReadStoredFooter reads the F-3 and F-5 values pack's footer states, structurally:
// from the stored bytes, by the checks of the tracepack corpus specification §5.8 and no other,
// whatever the footer validation of the tracepack format specification §10 decides,
// so it shows what an invalid footer states.
// It shares with the tracepack package only the CRC, the codecs and the TLV entry walk.
//
// Returns:
//   - FooterProjection: the stored projection, sorted as footer.json orders it.
//   - error: an error wrapping ErrFooterUnreadable that names the first check that fails;
//     an error for a decoded footer over tracepack.DefaultMaxFooterLen, a reader limit that is never a golden.
func ReadStoredFooter(pack []byte) (FooterProjection, error) {
	decoded, err := footerBytes(pack)
	if err != nil {
		return FooterProjection{}, err
	}

	p, err := readSections(decoded)
	if err != nil {
		return FooterProjection{}, err
	}

	return p.sorted(), nil
}

// RecomputeFooter computes the projection from the records of r's validated blocks
// by the rules of the tracepack format specification §10:
// a block's close_seq per epoch is the lowest seq among its records that end that epoch,
// a socket-close event or a clean stop, each a transport-event record whose payload is a valid TLV body;
// a boundary per capture-boundary record, its boundary_kind 0 when the payload carries none,
// with the gap_start and gap_end the payload carries;
// F-5 holds one epoch per epoch of the records, with the lowest close_seq of the blocks, every boundary,
// and the seqs of the records as sorted, maximal ranges.
//
// Returns:
//   - FooterProjection: the projection, sorted as footer.json orders it.
//   - error: the error of the read, or a block the reader's budget kept out, which is never a golden.
func RecomputeFooter(ctx context.Context, r *tracepack.Reader) (FooterProjection, error) {
	var (
		p      projection
		epochs []uint32
		seqs   []uint64
	)
	res, err := r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		if n := len(p.blocks); n == 0 || p.blocks[n-1].block != it.Block {
			p.blocks = append(p.blocks, blockProjection{block: it.Block})
		}
		b := &p.blocks[len(p.blocks)-1]
		rec := &it.Record
		epochs, seqs = append(epochs, rec.Epoch), append(seqs, rec.Seq)
		boundary, ends := eventFacts(rec)
		if boundary != nil {
			b.boundaries = append(b.boundaries, *boundary)
		}
		if ends {
			b.epochs = append(b.epochs, epochEntry{epoch: rec.Epoch, closeSeq: rec.Seq, hasClose: true})
		}

		return nil
	})
	if err != nil {
		return FooterProjection{}, err
	}
	if slices.ContainsFunc(res.Incomplete, func(d tracepack.Defect) bool { return d.Reason == tracepack.IncompleteReasonLimit }) {
		return FooterProjection{}, fmt.Errorf("corpus: a block is over a reader budget")
	}

	p.aggregate(epochs)
	p.f5SeqRanges = seqRanges(seqs)

	return p.sorted(), nil
}

// eventFacts returns the boundary entry of rec when it is a capture-boundary record:
// its seq, its numeric boundary_kind, 0 when its payload carries none, and the gap_start and gap_end its payload carries;
// and whether it ends its epoch: a socket-close event or a clean stop.
// Only a transport-event record whose payload is a valid TLV body is either (the tracepack format specification §10).
func eventFacts(rec *tracepack.Record) (*boundaryEntry, bool) {
	if rec.Kind != tracepack.KindTransportEvent {
		return nil, false
	}
	ev, err := tracepack.UnmarshalTransportEvent(rec.Payload)
	if err != nil {
		return nil, false
	}
	if ev.Event == tracepack.EventSocketClose {
		return nil, true
	}
	if ev.Event != tracepack.EventCaptureBoundary {
		return nil, false
	}
	b := &boundaryEntry{seq: rec.Seq, gapStart: ev.GapStart, gapEnd: ev.GapEnd}
	if ev.BoundaryKind != nil {
		b.kind = uint8(*ev.BoundaryKind)
	}

	return b, b.kind == uint8(tracepack.BoundaryKindStop)
}

// seqRanges returns the seqs of seqs as sorted, non-overlapping, maximal ranges, a seq given twice counted once.
func seqRanges(seqs []uint64) []tlv.SeqRange {
	var out []tlv.SeqRange
	for _, s := range slices.Compact(slices.Sorted(slices.Values(seqs))) {
		if n := len(out); n > 0 && out[n-1].Last != math.MaxUint64 && out[n-1].Last+1 == s {
			out[n-1].Last = s
			continue
		}
		out = append(out, tlv.SeqRange{First: s, Last: s})
	}

	return out
}

// footerBytes returns pack's decoded footer, after the checks that make the pack finalized (I-5)
// and the footer decodable under footer_codec to footer_uncompressed_len bytes.
// The file header is not validated: only its pack_metadata_len is read, to place the footer after the pack metadata.
func footerBytes(pack []byte) ([]byte, error) {
	if len(pack) < format.FileHeaderLen {
		return nil, fmt.Errorf("%w: the pack has no file header", ErrFooterUnreadable)
	}
	size := uint64(len(pack))
	metaEnd := format.FileHeaderLen + uint64(binary.LittleEndian.Uint32(pack[headerPackMetadataLenOff:]))
	trailerAt := size - format.TrailerLen
	t := pack[trailerAt:]
	if string(t[trailerMagicOff:]) != format.TrailerMagic || binary.LittleEndian.Uint32(t[trailerCRCOff:]) != format.CRC(t[:trailerCRCOff]) {
		return nil, fmt.Errorf("%w: no trailer: its magic or CRC does not match", ErrFooterUnreadable)
	}

	footerOffset := binary.LittleEndian.Uint64(t[trailerFooterOffsetOff:])
	footerLen := binary.LittleEndian.Uint64(t[trailerFooterLenOff:])
	if footerOffset < metaEnd || footerOffset > trailerAt || footerLen != trailerAt-footerOffset {
		return nil, fmt.Errorf("%w: the trailer places the footer at %d, %d bytes, outside %d..%d", ErrFooterUnreadable, footerOffset, footerLen, metaEnd, trailerAt)
	}
	onDisk := pack[footerOffset:trailerAt]
	if crc := format.CRC(onDisk); crc != binary.LittleEndian.Uint32(t[trailerFooterCRCOff:]) {
		return nil, fmt.Errorf("%w: footer CRC 0x%08X does not match footer_crc", ErrFooterUnreadable, crc)
	}

	n := binary.LittleEndian.Uint64(t[trailerUncompressedOff:])
	if n > tracepack.DefaultMaxFooterLen {
		return nil, fmt.Errorf("corpus: footer_uncompressed_len %d is over %d", n, tracepack.DefaultMaxFooterLen)
	}
	decoded, err := codec.Decode(t[trailerFooterCodecOff], nil, onDisk, int(n))
	if err != nil {
		return nil, fmt.Errorf("%w: the footer does not decode: %w", ErrFooterUnreadable, err)
	}

	return decoded, nil
}

// readSections reads the projection from the decoded footer d: the F-1 placement of F-2, F-3 and F-5,
// each F-2 entry's F-3 list, and F-5.
func readSections(d []byte) (projection, error) {
	if len(d) < format.FooterPrologueLen {
		return projection{}, fmt.Errorf("%w: the footer holds no F-1 prologue", ErrFooterUnreadable)
	}

	n := uint64(len(d))
	blockCount := binary.LittleEndian.Uint32(d[prologueBlockCountOff:])
	entryLen := uint64(binary.LittleEndian.Uint32(d[prologueF2EntryLenOff:]))
	f2Off := binary.LittleEndian.Uint64(d[prologueF2OffsetOff:])
	f3Off, f3Len := binary.LittleEndian.Uint64(d[prologueF3OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF3LenOff:])
	f5Off, f5Len := binary.LittleEndian.Uint64(d[prologueF5OffsetOff:]), binary.LittleEndian.Uint64(d[prologueF5LenOff:])
	switch {
	case entryLen < format.F2EntryLen:
		return projection{}, fmt.Errorf("%w: f2_entry_len %d is below %d", ErrFooterUnreadable, entryLen, format.F2EntryLen)
	// Both factors are below 2^32, so the product cannot overflow.
	case !inside(f2Off, uint64(blockCount)*entryLen, n), !inside(f3Off, f3Len, n), !inside(f5Off, f5Len, n):
		return projection{}, fmt.Errorf("%w: F-2, F-3 or F-5 lies outside the %d-byte footer", ErrFooterUnreadable, n)
	}

	var p projection
	// The capacity ends with F-3 too, so no slice of a list can reach past it.
	f3 := d[f3Off : f3Off+f3Len : f3Off+f3Len]
	for i := range blockCount {
		e := d[f2Off+uint64(i)*entryLen:]
		so := binary.LittleEndian.Uint64(e[f2SummaryOffsetOff:])
		sl := uint64(binary.LittleEndian.Uint32(e[f2SummaryLenOff:]))
		if !inside(so, sl, f3Len) {
			return projection{}, fmt.Errorf("%w: block %d's F-3 list lies outside F-3", ErrFooterUnreadable, i)
		}
		l, err := readList(f3[so:so+sl], f3EpochTag, f3BoundaryTag, 0)
		if err != nil {
			return projection{}, fmt.Errorf("block %d's F-3 list: %w", i, err)
		}
		p.blocks = append(p.blocks, blockProjection{block: int(i), epochs: l.epochs, boundaries: l.boundaries})
	}

	l, err := readList(d[f5Off:f5Off+f5Len], f5EpochTag, f5BoundaryTag, seqRangeTag)
	if err != nil {
		return projection{}, fmt.Errorf("F-5: %w", err)
	}
	p.f5Epochs, p.f5Boundary, p.f5SeqRanges = l.epochs, l.boundaries, l.seqRanges

	return p, nil
}

// inside reports whether the n bytes from off lie inside a section of size bytes.
func inside(off, n, size uint64) bool {
	return off <= size && n <= size-off
}

// listEntries are the entries of an F-3 list or of F-5 that the projection reads.
type listEntries struct {
	epochs     []epochEntry
	boundaries []boundaryEntry
	seqRanges  []tlv.SeqRange
}

// readList reads the epoch entries (tag epochTag), boundary entries (tag boundaryTag)
// and, unless rangeTag is 0, seq_range entries (tag rangeTag) of the entry list b,
// which must pass the framing rules of the tracepack format specification §5:
// each epoch or boundary entry of value type tlv holding a framed entry list
// with exactly one epoch, a u64 at most 2^32-1, and at most one close_seq, a u64,
// or exactly one seq, a u64, exactly one boundary_kind, a u8, and at most one gap_start and one gap_end, each an i64;
// each seq_range entry holding 16 bytes.
// Every other entry, and every other nested tag, is left unread.
func readList(b []byte, epochTag, boundaryTag, rangeTag uint16) (listEntries, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return listEntries{}, fmt.Errorf("%w: %w", ErrFooterUnreadable, err)
	}

	var out listEntries
	for _, e := range entries {
		if rangeTag != 0 && e.Tag == rangeTag {
			// Only the length is checked: an endpoint above 2^63-1 is a value footer validation judges.
			if len(e.Value) != seqRangeLen {
				return listEntries{}, fmt.Errorf("%w: a seq_range entry of %d bytes", ErrFooterUnreadable, len(e.Value))
			}
			out.seqRanges = append(out.seqRanges, tlv.SeqRange{
				First: binary.LittleEndian.Uint64(e.Value), Last: binary.LittleEndian.Uint64(e.Value[8:]),
			})

			continue
		}
		if e.Tag != epochTag && e.Tag != boundaryTag {
			continue
		}
		if err := e.CheckValue(tlv.TypeTLV); err != nil {
			return listEntries{}, fmt.Errorf("%w: %w", ErrFooterUnreadable, err)
		}
		nested, err := tlv.Decode(e.Value)
		if err != nil {
			return listEntries{}, fmt.Errorf("%w: entry 0x%04X: %w", ErrFooterUnreadable, e.Tag, err)
		}
		if e.Tag == epochTag {
			ep, err := readEpoch(nested)
			if err != nil {
				return listEntries{}, err
			}
			out.epochs = append(out.epochs, ep)
		} else {
			bd, err := readBoundary(nested)
			if err != nil {
				return listEntries{}, err
			}
			out.boundaries = append(out.boundaries, bd)
		}
	}

	return out, nil
}

// readEpoch reads an epoch entry's nested entries:
// exactly one epoch, a u64 at most 2^32-1, and at most one close_seq, a u64.
func readEpoch(nested []tlv.Entry) (epochEntry, error) {
	var (
		out            epochEntry
		epochs, closes int
	)
	for _, e := range nested {
		switch e.Tag {
		case epochEpochTag:
			v, err := e.U64()
			if err != nil || v > math.MaxUint32 {
				return epochEntry{}, fmt.Errorf("%w: epoch entry: epoch is not a u64 below 2^32", ErrFooterUnreadable)
			}
			out.epoch = uint32(v)
			epochs++
		case epochCloseSeqTag:
			v, err := e.U64()
			if err != nil {
				return epochEntry{}, fmt.Errorf("%w: epoch entry: close_seq: %w", ErrFooterUnreadable, err)
			}
			out.closeSeq, out.hasClose = v, true
			closes++
		default:
			// A nested tag the projection does not read.
		}
	}
	if epochs != 1 || closes > 1 {
		return epochEntry{}, fmt.Errorf("%w: epoch entry holds %d epoch and %d close_seq tags", ErrFooterUnreadable, epochs, closes)
	}

	return out, nil
}

// readBoundary reads a boundary entry's nested entries:
// exactly one seq, a u64, exactly one boundary_kind, a u8, and at most one gap_start and one gap_end, each an i64.
func readBoundary(nested []tlv.Entry) (boundaryEntry, error) {
	var (
		out                       boundaryEntry
		seqs, kinds, starts, ends int
	)
	for _, e := range nested {
		var err error
		switch e.Tag {
		case boundarySeqTag:
			out.seq, err = e.U64()
			seqs++
		case boundaryKindTag:
			out.kind, err = e.U8()
			kinds++
		case boundaryGapStartTag:
			out.gapStart, err = i64Of(e)
			starts++
		case boundaryGapEndTag:
			out.gapEnd, err = i64Of(e)
			ends++
		default:
			// A nested tag the projection does not read.
		}
		if err != nil {
			return boundaryEntry{}, fmt.Errorf("%w: boundary entry: %w", ErrFooterUnreadable, err)
		}
	}
	if seqs != 1 || kinds != 1 || starts > 1 || ends > 1 {
		return boundaryEntry{}, fmt.Errorf("%w: boundary entry holds %d seq, %d boundary_kind, %d gap_start and %d gap_end tags",
			ErrFooterUnreadable, seqs, kinds, starts, ends)
	}

	return out, nil
}

// i64Of returns the value of the i64 entry e.
func i64Of(e tlv.Entry) (*int64, error) {
	v, err := e.I64()
	if err != nil {
		return nil, err
	}

	return &v, nil
}

// aggregate reduces each block's epoch entries, one per record that ends an epoch,
// to the block-local lowest close_seq per epoch,
// and builds F-5: one epoch entry per epoch of epochs, the epochs of every record read,
// with the lowest close_seq of the blocks,
// and every block's boundaries.
func (p *projection) aggregate(epochs []uint32) {
	var closes []epochEntry
	for i := range p.blocks {
		b := &p.blocks[i]
		slices.SortFunc(b.epochs, compareEpochEntries)
		b.epochs = slices.CompactFunc(b.epochs, func(x, y epochEntry) bool { return x.epoch == y.epoch })
		closes = append(closes, b.epochs...)
		p.f5Boundary = append(p.f5Boundary, b.boundaries...)
	}
	slices.SortFunc(closes, compareEpochEntries)

	slices.Sort(epochs)
	for _, e := range slices.Compact(epochs) {
		entry := epochEntry{epoch: e}
		if k := slices.IndexFunc(closes, func(c epochEntry) bool { return c.epoch == e }); k >= 0 {
			entry.closeSeq, entry.hasClose = closes[k].closeSeq, true
		}
		p.f5Epochs = append(p.f5Epochs, entry)
	}
}

// sorted returns p in the orders and forms of footer.json:
// blocks ascending by block; close_seqs ascending by epoch, then seq, epoch entries without close_seq left out;
// F-5 epochs ascending by epoch, one without close_seq first, then by close_seq;
// boundaries ascending by seq, then by numeric boundary_kind, then by gap_start, then by gap_end, each named;
// F-5 seq ranges ascending by first, then by last.
func (p *projection) sorted() FooterProjection {
	out := FooterProjection{Blocks: []FooterBlock{}, F5: FooterF5{
		Epochs: []F5Epoch{}, Boundaries: namedBoundaries(p.f5Boundary), SeqRanges: []SeqRange{},
	}}
	for _, r := range slices.SortedFunc(slices.Values(p.f5SeqRanges), func(a, b tlv.SeqRange) int {
		return cmp.Or(cmp.Compare(a.First, b.First), cmp.Compare(a.Last, b.Last))
	}) {
		out.F5.SeqRanges = append(out.F5.SeqRanges, SeqRange{First: U64(r.First), Last: U64(r.Last)})
	}

	blocks := slices.Clone(p.blocks)
	slices.SortStableFunc(blocks, func(a, b blockProjection) int { return cmp.Compare(a.block, b.block) })
	for _, b := range blocks {
		fb := FooterBlock{Block: b.block, CloseSeqs: []CloseSeq{}, Boundaries: namedBoundaries(b.boundaries)}
		for _, e := range slices.SortedFunc(slices.Values(b.epochs), compareEpochEntries) {
			if e.hasClose {
				fb.CloseSeqs = append(fb.CloseSeqs, CloseSeq{Epoch: e.epoch, Seq: U64(e.closeSeq)})
			}
		}
		out.Blocks = append(out.Blocks, fb)
	}

	for _, e := range slices.SortedFunc(slices.Values(p.f5Epochs), compareEpochEntries) {
		fe := F5Epoch{Epoch: e.epoch}
		if e.hasClose {
			fe.CloseSeq = new(U64(e.closeSeq))
		}
		out.F5.Epochs = append(out.F5.Epochs, fe)
	}

	return out
}

// compareEpochEntries orders epoch entries by epoch, an entry without close_seq first, then by close_seq.
func compareEpochEntries(a, b epochEntry) int {
	return cmp.Or(cmp.Compare(a.epoch, b.epoch), compareBool(a.hasClose, b.hasClose), cmp.Compare(a.closeSeq, b.closeSeq))
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// namedBoundaries returns bs ascending by seq, then by numeric boundary_kind, then by gap_start, then by gap_end,
// each kind named and each bound kept as present or absent.
func namedBoundaries(bs []boundaryEntry) []Boundary {
	out := make([]Boundary, 0, len(bs))
	for _, b := range slices.SortedFunc(slices.Values(bs), compareBoundaries) {
		out = append(out, Boundary{
			Seq: U64(b.seq), Kind: tracepack.BoundaryKind(b.kind).String(), GapStart: boundOf(b.gapStart), GapEnd: boundOf(b.gapEnd),
		})
	}

	return out
}

// compareBoundaries orders boundary entries by seq, then by numeric boundary_kind, then by gap_start, then by gap_end.
func compareBoundaries(x, y boundaryEntry) int {
	return cmp.Or(cmp.Compare(x.seq, y.seq), cmp.Compare(x.kind, y.kind), compareBound(x.gapStart, y.gapStart), compareBound(x.gapEnd, y.gapEnd))
}

// compareBound orders gap bounds: an absent bound before every present one, present bounds as i64.
func compareBound(a, b *int64) int {
	if a == nil || b == nil {
		return compareBool(a != nil, b != nil)
	}

	return cmp.Compare(*a, *b)
}

// boundOf returns the footer.json form of a gap bound, nil when absent.
func boundOf(v *int64) *I64 {
	if v == nil {
		return nil
	}

	return new(I64(*v))
}

// Marshal returns footer.json in the canonical form.
func (f *Footer) Marshal() ([]byte, error) {
	return Marshal(f)
}
