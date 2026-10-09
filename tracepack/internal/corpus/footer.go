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

// Tags the projection reads: the epoch and boundary entries of an F-3 list and of F-5, and their nested tags
// (the tracepack format specification §10).
const (
	f3EpochTag    uint16 = 0x0007
	f3BoundaryTag uint16 = 0x0008
	f5EpochTag    uint16 = 0x000A
	f5BoundaryTag uint16 = 0x000B

	epochEpochTag    uint16 = 0x0001
	epochCloseSeqTag uint16 = 0x0007

	boundarySeqTag  uint16 = 0x0001
	boundaryKindTag uint16 = 0x0002
)

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

// Boundary is one boundary entry: its seq and its boundary_kind name.
type Boundary struct {
	Seq  U64    `json:"seq"`
	Kind string `json:"kind"`
}

// FooterF5 is the projection of F-5: one object per epoch entry, and the boundary entries.
type FooterF5 struct {
	Epochs     []F5Epoch  `json:"epochs"`
	Boundaries []Boundary `json:"boundaries"`
}

// F5Epoch is one F-5 epoch entry: its epoch and its close_seq, if any.
type F5Epoch struct {
	Epoch    uint32 `json:"epoch"`
	CloseSeq *U64   `json:"close_seq,omitempty"`
}

// projection is a footer projection before it is sorted and named.
type projection struct {
	blocks     []blockProjection
	f5Epochs   []epochEntry
	f5Boundary []boundaryEntry
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

// boundaryEntry is a boundary entry: its seq and its numeric boundary_kind.
type boundaryEntry struct {
	seq  uint64
	kind uint8
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
	decoded, err := decodedFooter(pack)
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
// a boundary per capture-boundary record, its boundary_kind 0 when the payload carries none;
// F-5 holds one epoch per epoch of the records, with the lowest close_seq of the blocks, and every boundary.
//
// Returns:
//   - FooterProjection: the projection, sorted as footer.json orders it.
//   - error: the error of the read, or a block the reader's budget kept out, which is never a golden.
func RecomputeFooter(ctx context.Context, r *tracepack.Reader) (FooterProjection, error) {
	var (
		p      projection
		epochs []uint32
	)
	res, err := r.Iterate(ctx, tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		if n := len(p.blocks); n == 0 || p.blocks[n-1].block != it.Block {
			p.blocks = append(p.blocks, blockProjection{block: it.Block})
		}
		b := &p.blocks[len(p.blocks)-1]
		rec := &it.Record
		epochs = append(epochs, rec.Epoch)
		boundary, kind, ends := eventFacts(rec)
		if boundary {
			b.boundaries = append(b.boundaries, boundaryEntry{seq: rec.Seq, kind: kind})
		}
		if ends {
			b.epochs = append(b.epochs, epochEntry{epoch: rec.Epoch, closeSeq: rec.Seq, hasClose: true})
		}

		return nil
	})
	if err != nil {
		return FooterProjection{}, err
	}
	if slices.ContainsFunc(res.Incomplete, func(d tracepack.Defect) bool { return d.Reason == tracepack.ReasonLimit }) {
		return FooterProjection{}, fmt.Errorf("corpus: a block is over a reader budget")
	}

	p.aggregate(epochs)

	return p.sorted(), nil
}

// eventFacts reports whether rec is a capture-boundary record,
// with its numeric boundary_kind, 0 when its payload carries none,
// and whether it ends its epoch: a socket-close event or a clean stop.
// Only a transport-event record whose payload is a valid TLV body is either (the tracepack format specification §10).
func eventFacts(rec *tracepack.Record) (boundary bool, kind uint8, ends bool) {
	if rec.Kind != tracepack.KindTransportEvent {
		return false, 0, false
	}
	ev, err := tracepack.UnmarshalTransportEvent(rec.Payload)
	if err != nil {
		return false, 0, false
	}
	if ev.Event == tracepack.EventSocketClose {
		return false, 0, true
	}
	if ev.Event != tracepack.EventCaptureBoundary {
		return false, 0, false
	}
	if ev.BoundaryKind != nil {
		kind = uint8(*ev.BoundaryKind)
	}

	return true, kind, kind == uint8(tracepack.BoundaryKindStop)
}

// decodedFooter returns pack's decoded footer, after the checks that make the pack finalized (I-5)
// and the footer decodable under footer_codec to footer_uncompressed_len bytes.
// The file header is not validated: only its pack_metadata_len is read, to place the footer after the pack metadata.
func decodedFooter(pack []byte) ([]byte, error) {
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
		epochs, boundaries, err := readList(f3[so:so+sl], f3EpochTag, f3BoundaryTag)
		if err != nil {
			return projection{}, fmt.Errorf("block %d's F-3 list: %w", i, err)
		}
		p.blocks = append(p.blocks, blockProjection{block: int(i), epochs: epochs, boundaries: boundaries})
	}

	var err error
	if p.f5Epochs, p.f5Boundary, err = readList(d[f5Off:f5Off+f5Len], f5EpochTag, f5BoundaryTag); err != nil {
		return projection{}, fmt.Errorf("F-5: %w", err)
	}

	return p, nil
}

// inside reports whether the n bytes from off lie inside a section of size bytes.
func inside(off, n, size uint64) bool {
	return off <= size && n <= size-off
}

// readList reads the epoch entries (tag epochTag) and boundary entries (tag boundaryTag) of the entry list b,
// which must pass the framing rules of the tracepack format specification §5,
// each such entry of value type tlv holding a framed entry list
// with exactly one epoch, a u64 at most 2^32-1, and at most one close_seq, a u64,
// or exactly one seq, a u64, and exactly one boundary_kind, a u8.
// Every other entry, and every other nested tag, is left unread.
func readList(b []byte, epochTag, boundaryTag uint16) ([]epochEntry, []boundaryEntry, error) {
	entries, err := tlv.Decode(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrFooterUnreadable, err)
	}

	var (
		epochs     []epochEntry
		boundaries []boundaryEntry
	)
	for _, e := range entries {
		if e.Tag != epochTag && e.Tag != boundaryTag {
			continue
		}
		if err := e.CheckValue(tlv.TypeTLV); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrFooterUnreadable, err)
		}
		nested, err := tlv.Decode(e.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: entry 0x%04X: %w", ErrFooterUnreadable, e.Tag, err)
		}
		if e.Tag == epochTag {
			ep, err := readEpoch(nested)
			if err != nil {
				return nil, nil, err
			}
			epochs = append(epochs, ep)
		} else {
			bd, err := readBoundary(nested)
			if err != nil {
				return nil, nil, err
			}
			boundaries = append(boundaries, bd)
		}
	}

	return epochs, boundaries, nil
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

// readBoundary reads a boundary entry's nested entries: exactly one seq, a u64, and exactly one boundary_kind, a u8.
func readBoundary(nested []tlv.Entry) (boundaryEntry, error) {
	var (
		out         boundaryEntry
		seqs, kinds int
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
		default:
			// A nested tag the projection does not read.
		}
		if err != nil {
			return boundaryEntry{}, fmt.Errorf("%w: boundary entry: %w", ErrFooterUnreadable, err)
		}
	}
	if seqs != 1 || kinds != 1 {
		return boundaryEntry{}, fmt.Errorf("%w: boundary entry holds %d seq and %d boundary_kind tags", ErrFooterUnreadable, seqs, kinds)
	}

	return out, nil
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
// boundaries ascending by seq, then by numeric boundary_kind, each named.
func (p *projection) sorted() FooterProjection {
	out := FooterProjection{Blocks: []FooterBlock{}, F5: FooterF5{Epochs: []F5Epoch{}, Boundaries: namedBoundaries(p.f5Boundary)}}

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

// namedBoundaries returns bs ascending by seq, then by numeric boundary_kind, each kind named.
func namedBoundaries(bs []boundaryEntry) []Boundary {
	out := make([]Boundary, 0, len(bs))
	for _, b := range slices.SortedFunc(slices.Values(bs), func(x, y boundaryEntry) int {
		return cmp.Or(cmp.Compare(x.seq, y.seq), cmp.Compare(x.kind, y.kind))
	}) {
		out = append(out, Boundary{Seq: U64(b.seq), Kind: tracepack.BoundaryKind(b.kind).String()})
	}

	return out
}

// Marshal returns footer.json in the canonical form.
func (f *Footer) Marshal() ([]byte, error) {
	return Marshal(f)
}
