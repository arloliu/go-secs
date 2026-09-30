package tracepack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Default ReaderOptions values, which a zero or negative field takes.
const (
	// DefaultWholeReadThreshold is the default ReaderOptions.WholeReadThreshold: 256 KiB.
	DefaultWholeReadThreshold = 256 << 10
	// DefaultHeadWindow is the default ReaderOptions.HeadWindow: 64 KiB.
	DefaultHeadWindow = 64 << 10
	// DefaultTailWindow is the default ReaderOptions.TailWindow: 256 KiB.
	DefaultTailWindow = 256 << 10
	// DefaultMaxPackMetadataLen is the default ReaderOptions.MaxPackMetadataLen: 64 MiB.
	DefaultMaxPackMetadataLen = 64 << 20
	// DefaultMaxFooterLen is the default ReaderOptions.MaxFooterLen: 64 MiB.
	DefaultMaxFooterLen = 64 << 20
	// DefaultMaxBlockLen is the default ReaderOptions.MaxBlockLen: 256 MiB.
	DefaultMaxBlockLen = 256 << 20
	// DefaultMaxWalkedBlocks is the default ReaderOptions.MaxWalkedBlocks: 1 Mi blocks.
	DefaultMaxWalkedBlocks = 1 << 20
)

// footerCRCChunk is the largest read of the sequential reads that compute the CRC of a footer over MaxFooterLen.
const footerCRCChunk = 1 << 20

// Sentinel errors returned by Open for a file it cannot read at all.
// Each is wrapped with the structure and offset concerned.
var (
	// ErrNotTracepack reports an object that is not a tracepack file:
	// it is shorter than the file header, or its magic does not match (the tracepack format specification §4).
	ErrNotTracepack = errors.New("tracepack: not a tracepack file")
	// ErrUnsupportedFormat reports a file whose format_major this package does not read
	// (the tracepack format specification §14).
	ErrUnsupportedFormat = errors.New("tracepack: unsupported format version")
	// ErrChecksum reports a CRC that does not match the bytes it covers:
	// the bytes changed after they were written.
	// Open returns it for the file header and the pack metadata;
	// PackHeader.FooterErr wraps it for the footer.
	ErrChecksum = errors.New("tracepack: checksum mismatch")
)

// ErrReadLimit reports a structure larger than a reader budget of ReaderOptions.
// It is a resource limit of the reader, not a format defect:
// a caller raises the budget to read a legitimately larger structure.
// The error that wraps it names the structure and its offset.
var ErrReadLimit = errors.New("tracepack: structure exceeds a reader budget")

// ReaderOptions configures Open.
//
// A zero or negative field takes its default.
// HeadWindow is raised to at least the 80-byte file header and TailWindow to at least the 64-byte trailer,
// so the first round always holds both.
type ReaderOptions struct {
	// WholeReadThreshold is the object size up to which Open reads the whole object in one request;
	// default 256 KiB.
	WholeReadThreshold int64
	// HeadWindow is the length of the speculative prefix read, which normally covers the pack metadata;
	// default 64 KiB.
	HeadWindow int64
	// TailWindow is the length of the suffix read, W in the tracepack format specification §13,
	// which holds the trailer and usually the footer;
	// default 256 KiB.
	TailWindow int64
	// FooterOffset is a catalog's footer location for the object, or 0 for none;
	// a real footer_offset is at least 80.
	// It is a hint: Open reads from it to the end of the object in place of the suffix read,
	// and never trusts it over the trailer.
	FooterOffset uint64
	// MaxPackMetadataLen is the largest pack metadata Open reads; above it Open fails with ErrReadLimit.
	// Default 64 MiB.
	MaxPackMetadataLen int64
	// MaxFooterLen is the largest footer Open reads and decodes, on disk and decoded;
	// above it the footer is not used, and the blocks are found by the forward walk.
	// Default 64 MiB.
	MaxFooterLen int64
	// MaxBlockLen is the largest block body a read decodes, on disk and decoded;
	// a larger block is not read and is reported as incomplete.
	// Default 256 MiB.
	MaxBlockLen int64
	// MaxWalkedBlocks is the largest number of blocks the forward walk indexes when the footer is not used,
	// which bounds the block index as MaxFooterLen bounds a footer's;
	// the walk stops at the next block, and every read reports the rest of the pack as incomplete with ReasonLimit.
	// Default 1 Mi blocks.
	MaxWalkedBlocks int
}

// Reader reads one tracepack file through an io.ReaderAt.
//
// Open reads the file header, the pack metadata, the trailer and the footer, or walks the blocks without the footer,
// so a Reader holds an immutable snapshot of them;
// it is safe for concurrent use.
type Reader struct {
	ra   io.ReaderAt
	size int64
	opts ReaderOptions
	hdr  format.FileHeader
	// metaRaw holds the pack metadata bytes, from which Header decodes a fresh copy on every call.
	metaRaw []byte
	// meta is the Reader's own decoded pack metadata; it is never handed out.
	meta      *PackMeta
	finalized bool
	// footerErr is non-nil when the footer was not used and blocks came from the forward walk.
	footerErr error
	// trailer is nil when the trailer is invalid.
	trailer *TrailerInfo
	// footer is the validated footer; nil when it was not used.
	footer *footerIndex
	// keepF3 is set when the Reader was opened to keep a valid footer's F-3 section, so rawF3 can return its lists.
	keepF3 bool
	// blocks is the block index, from the footer or the forward walk, in file order.
	blocks []BlockInfo
	// openDefects holds the defects every read of the pack carries:
	// the forward walk's stop, a pack that is not finalized, or a walk that disagrees with the trailer.
	openDefects []Defect
	// walkStop is the forward walk's stop: the envelope it could not account for; nil when it did not stop.
	walkStop *Defect
}

// PackHeader describes a pack: its file header, its pack metadata, and its finalization.
type PackHeader struct {
	// FormatMajor and FormatMinor are the file header's format version; FormatMajor is always 1.
	FormatMajor, FormatMinor uint16
	// Flags is the file header's flags, reserved bits included, as stored.
	Flags            uint32
	WriterStartUTCNs int64
	PackID           UUID
	CaptureID        UUID
	// Meta is the pack metadata, decoded afresh for every PackHeader.
	Meta *PackMeta
	// Finalized reports I-5: the trailer magic, the trailer CRC and the footer CRC match.
	Finalized bool
	// FooterErr is non-nil when the footer was not used and the blocks came from the forward walk:
	// no valid trailer, a footer CRC mismatch, a footer over MaxFooterLen (wrapping ErrReadLimit),
	// or a footer that fails to decode or validate (wrapping ErrInvalidFooter).
	FooterErr error
	// Trailer is the trailer's content; nil when the trailer is invalid.
	Trailer *TrailerInfo
}

// TrailerInfo is the content of a valid trailer (the tracepack format specification §11).
type TrailerInfo struct {
	// BlockCount is the number of blocks the trailer claims.
	BlockCount uint32
	// RecordCount is the number of records the trailer claims.
	RecordCount uint64
	// LastSeq is the seq of the last record; meaningful only when RecordCount is not 0.
	LastSeq uint64
	// FooterOffset is the file offset of the footer, where the block region ends.
	FooterOffset uint64
}

// BlockInfo describes one block of a pack.
//
// The fields up to FirstSeq come from the block's F-2 entry when the block is indexed,
// else from its block envelope, found by the forward walk.
type BlockInfo struct {
	// Offset is the file offset of the block envelope.
	Offset uint64
	// OnDiskLen is the envelope plus body length.
	OnDiskLen       uint32
	UncompressedLen uint32
	RecordCount     uint32
	BodyCRC         uint32
	RecordHeaderLen uint16
	FirstSeq        uint64
	// Indexed reports that the block comes from a valid footer;
	// the fields below are valid only when it is set.
	Indexed      bool
	LastSeq      uint64
	TSMin, TSMax int64
	EpochMin     uint32
	EpochMax     uint32
}

// readReq is one ReadAt call of a round: fill buf from file offset off.
type readReq struct {
	off int64
	buf []byte
}

// bootstrap holds what the bootstrap rounds of Open read, until the Reader's snapshot is built from it.
type bootstrap struct {
	// head holds the object's first len(head) bytes.
	head []byte
	// suffix holds the object's bytes from suffixStart to its end, the trailer last.
	suffix      []byte
	suffixStart int64
	trailer     format.Trailer
	// trailerErr is nil iff the trailer is valid.
	trailerErr error
	// meta holds the pack metadata bytes.
	meta []byte
	// footer holds the on-disk footer; nil unless the trailer is valid and footer_len is within MaxFooterLen.
	footer []byte
}

// Open bootstraps a Reader over the tracepack file ra of size bytes (the tracepack format specification §13).
//
// It reads in rounds, each a set of concurrent ReadAt calls, and checks ctx before each.
// The first round reads the whole object when it is at most WholeReadThreshold bytes or the two windows cover it.
// Otherwise it reads the first HeadWindow bytes,
// together with the bytes from a usable FooterOffset hint to the end of the object, or else the last TailWindow bytes.
// A second round, only when needed, reads the rest of the pack metadata and the rest of the footer concurrently.
// Open validates the file header, the format version and the pack metadata before any block is read,
// then decides finalization by the trailer and the footer CRC (I-5) and validates the footer (§10).
// A footer it cannot use makes it walk the blocks forward from the pack metadata instead (I-1),
// one ReadAt per block, after streaming the CRC of a footer over MaxFooterLen in reads of at most 1 MiB.
// An invalid trailer or footer never fails Open: it is reported by PackHeader.FooterErr and the read's defects.
//
// Parameters:
//   - ctx: cancels Open between reads.
//   - ra: the object; Open may issue several ReadAt calls in parallel.
//   - size: the object's size, known before reading.
//   - opts: see ReaderOptions.
//
// Returns:
//   - *Reader: the Reader; nil on error.
//   - error: ctx's error; a ReadAt error, wrapping io.ErrUnexpectedEOF for a short read;
//     ErrNotTracepack for a size below 80 or a bad file header magic;
//     io.ErrUnexpectedEOF for a pack metadata past the end of the object;
//     ErrUnsupportedFormat for a format_major other than 1;
//     ErrChecksum for a bad file header or pack metadata CRC;
//     ErrReadLimit for a pack metadata over MaxPackMetadataLen;
//     the error of UnmarshalPackMeta.
func Open(ctx context.Context, ra io.ReaderAt, size int64, opts ReaderOptions) (*Reader, error) {
	return openReader(ctx, ra, size, opts, false)
}

// openReader is Open, and with keepF3 set it also keeps a valid footer's F-3 section and each block's span in it,
// so that rawF3 returns a block's F-3 entry list as stored.
// Only a merge copies those lists; every other Reader keeps no F-3 bytes.
func openReader(ctx context.Context, ra io.ReaderAt, size int64, opts ReaderOptions, keepF3 bool) (*Reader, error) {
	if size < format.FileHeaderLen {
		return nil, fmt.Errorf("tracepack: object of %d bytes is shorter than the %d-byte file header: %w: %w",
			size, format.FileHeaderLen, ErrNotTracepack, io.ErrUnexpectedEOF)
	}

	r := &Reader{ra: ra, size: size, opts: opts.withDefaults(), keepF3: keepF3}
	b, err := r.roundOne(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.roundTwo(ctx, b); err != nil {
		return nil, err
	}
	if err := r.decodeMeta(b.meta); err != nil {
		return nil, err
	}
	if err := r.settleFooter(ctx, b); err != nil {
		return nil, err
	}

	if r.footer != nil {
		r.blocks = indexedBlocks(r.footer)

		return r, nil
	}
	if err := r.walk(ctx); err != nil {
		return nil, err
	}

	return r, nil
}

// readAt fills buf from offset off of ra.
// A short read is an error wrapping io.ErrUnexpectedEOF, since every read lies within the object's known size;
// io.EOF with a full buffer is success, as io.ReaderAt allows at the end of the object.
func readAt(ra io.ReaderAt, buf []byte, off int64) error {
	n, err := ra.ReadAt(buf, off)
	if n == len(buf) && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}

	return fmt.Errorf("tracepack: read %d bytes at offset %d: %w", len(buf), off, err)
}

// readRound checks ctx, then issues the ReadAt calls of reqs concurrently and waits for all of them.
// It returns the error of the first failing request in the order of reqs, so the result does not depend on timing.
func readRound(ctx context.Context, ra io.ReaderAt, reqs []readReq) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("tracepack: open: %w", err)
	}
	if len(reqs) == 1 {
		return readAt(ra, reqs[0].buf, reqs[0].off)
	}

	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Go(func() { errs[i] = readAt(ra, reqs[i].buf, reqs[i].off) })
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// indexedBlocks returns the block index of a validated footer, one indexed block per F-2 entry.
func indexedBlocks(idx *footerIndex) []BlockInfo {
	blocks := make([]BlockInfo, len(idx.blocks))
	for i := range idx.blocks {
		blocks[i] = idx.blocks[i].blockInfo()
	}

	return blocks
}

// blockInfo returns the BlockInfo of the indexed block whose F-2 entry and F-3 summary s holds.
func (s *blockSummary) blockInfo() BlockInfo {
	return BlockInfo{
		Offset:          s.offset,
		OnDiskLen:       s.onDiskLen,
		UncompressedLen: s.uncompressedLen,
		RecordCount:     s.recordCount,
		BodyCRC:         s.bodyCRC,
		RecordHeaderLen: s.recordHeaderLen,
		FirstSeq:        s.firstSeq,
		Indexed:         true,
		LastSeq:         s.lastSeq,
		TSMin:           s.tsMin,
		TSMax:           s.tsMax,
		EpochMin:        s.epochMin,
		EpochMax:        s.epochMax,
	}
}

// positiveOr returns v when it is positive, else def.
func positiveOr[T int | int64](v, def T) T {
	if v > 0 {
		return v
	}

	return def
}

// Header returns the pack's header: its file header, pack metadata and finalization.
//
// Every call returns a new PackHeader with Meta decoded afresh,
// so a caller may modify the result without affecting the Reader or any other PackHeader.
func (r *Reader) Header() *PackHeader {
	// Open decoded the same bytes without error, so decoding them again cannot fail.
	meta, _ := UnmarshalPackMeta(r.metaRaw)

	h := &PackHeader{
		FormatMajor:      r.hdr.FormatMajor,
		FormatMinor:      r.hdr.FormatMinor,
		Flags:            r.hdr.Flags,
		WriterStartUTCNs: r.hdr.WriterStartUTCNs,
		PackID:           UUID(r.hdr.PackID),
		CaptureID:        UUID(r.hdr.CaptureID),
		Meta:             meta,
		Finalized:        r.finalized,
		FooterErr:        r.footerErr,
	}
	if r.trailer != nil {
		t := *r.trailer
		h.Trailer = &t
	}

	return h
}

// Blocks returns a copy of the pack's block index, in file order.
//
// The blocks come from the footer when it is valid, each with Indexed set,
// else from the forward walk, which stops at the first block it cannot account for.
func (r *Reader) Blocks() []BlockInfo {
	// A pack without blocks yields an empty, non-nil slice, which slices.Clone would not.
	out := make([]BlockInfo, len(r.blocks))
	copy(out, r.blocks)

	return out
}

// withDefaults returns o with every zero or negative field set to its default
// and the windows raised to hold the file header and the trailer.
func (o ReaderOptions) withDefaults() ReaderOptions {
	o.WholeReadThreshold = positiveOr(o.WholeReadThreshold, DefaultWholeReadThreshold)
	o.HeadWindow = max(positiveOr(o.HeadWindow, DefaultHeadWindow), format.FileHeaderLen)
	o.TailWindow = max(positiveOr(o.TailWindow, DefaultTailWindow), format.TrailerLen)
	o.MaxPackMetadataLen = positiveOr(o.MaxPackMetadataLen, DefaultMaxPackMetadataLen)
	o.MaxFooterLen = positiveOr(o.MaxFooterLen, DefaultMaxFooterLen)
	o.MaxBlockLen = positiveOr(o.MaxBlockLen, DefaultMaxBlockLen)
	o.MaxWalkedBlocks = positiveOr(o.MaxWalkedBlocks, DefaultMaxWalkedBlocks)

	return o
}

// blocksStart returns 80 + pack_metadata_len, where the first block starts.
func (r *Reader) blocksStart() uint64 {
	return format.FileHeaderLen + uint64(r.hdr.PackMetadataLen)
}

// hintUsable reports whether the FooterOffset hint can locate the footer's read:
// it lies after the file header and before the trailer,
// and the read from it to the end of the object is within MaxFooterLen plus the trailer.
func (r *Reader) hintUsable() bool {
	hint := r.opts.FooterOffset
	// Open checked that size is at least 80, so trailerAt is positive.
	trailerAt := r.size - format.TrailerLen
	if hint < format.FileHeaderLen || hint >= uint64(trailerAt) {
		return false
	}

	return trailerAt-int64(hint) <= r.opts.MaxFooterLen
}

// roundOne issues the first round of reads, then checks the file header, the pack metadata's bounds and the trailer.
// A bad file header fails Open before any further read.
func (r *Reader) roundOne(ctx context.Context) (*bootstrap, error) {
	o := &r.opts
	b := &bootstrap{}

	var reqs []readReq
	if r.size <= o.WholeReadThreshold || o.HeadWindow >= r.size-o.TailWindow {
		b.head = make([]byte, r.size)
		b.suffix = b.head
		reqs = []readReq{{off: 0, buf: b.head}}
	} else {
		b.head = make([]byte, o.HeadWindow)
		b.suffixStart = r.size - o.TailWindow
		if r.hintUsable() {
			b.suffixStart = int64(o.FooterOffset)
		}
		b.suffix = make([]byte, r.size-b.suffixStart)
		reqs = []readReq{{off: 0, buf: b.head}, {off: b.suffixStart, buf: b.suffix}}
	}
	if err := readRound(ctx, r.ra, reqs); err != nil {
		return nil, err
	}

	hdr, err := format.UnmarshalFileHeader(b.head)
	if err != nil {
		if class := fileHeaderClass(err); class != nil {
			return nil, fmt.Errorf("tracepack: %w: %w", class, err)
		}

		return nil, fmt.Errorf("tracepack: %w", err)
	}
	r.hdr = hdr

	if end := r.blocksStart(); end > uint64(r.size) {
		return nil, fmt.Errorf("tracepack: pack metadata of %d bytes ends at %d, past the %d-byte object: %w",
			hdr.PackMetadataLen, end, r.size, io.ErrUnexpectedEOF)
	}
	if int64(hdr.PackMetadataLen) > o.MaxPackMetadataLen {
		return nil, fmt.Errorf("tracepack: pack metadata at offset %d: %d bytes exceed MaxPackMetadataLen %d: %w",
			format.FileHeaderLen, hdr.PackMetadataLen, o.MaxPackMetadataLen, ErrReadLimit)
	}

	b.trailer, b.trailerErr = r.parseTrailer(b.suffix[len(b.suffix)-format.TrailerLen:])

	return b, nil
}

// parseTrailer decodes the trailer and checks it against the object:
// the footer ends where the trailer starts, and starts at or after the end of the pack metadata.
func (r *Reader) parseTrailer(b []byte) (format.Trailer, error) {
	tr, err := format.UnmarshalTrailer(b)
	if err != nil {
		return format.Trailer{}, err
	}

	// UnmarshalTrailer capped both values at 2^63-1, so their sum cannot wrap.
	trailerAt := uint64(r.size - format.TrailerLen)
	if tr.FooterOffset+tr.FooterLen != trailerAt {
		return format.Trailer{}, fmt.Errorf("trailer: footer_offset %d and footer_len %d do not end at the trailer at %d",
			tr.FooterOffset, tr.FooterLen, trailerAt)
	}
	if tr.FooterOffset < r.blocksStart() {
		return format.Trailer{}, fmt.Errorf("trailer: footer_offset %d lies before the end of the pack metadata at %d",
			tr.FooterOffset, r.blocksStart())
	}

	return tr, nil
}

// roundTwo reads, in one concurrent round, the rest of the pack metadata beyond the head read
// and the rest of a footer within MaxFooterLen before the suffix read; it issues no read when neither is needed.
func (r *Reader) roundTwo(ctx context.Context, b *bootstrap) error {
	var reqs []readReq

	metaEnd := int(r.blocksStart())
	if metaEnd <= len(b.head) {
		b.meta = bytes.Clone(b.head[format.FileHeaderLen:metaEnd])
	} else {
		b.meta = make([]byte, r.hdr.PackMetadataLen)
		n := copy(b.meta, b.head[format.FileHeaderLen:])
		reqs = append(reqs, readReq{off: int64(len(b.head)), buf: b.meta[n:]})
	}

	if b.trailerErr == nil && b.trailer.FooterLen <= uint64(r.opts.MaxFooterLen) {
		fo := int64(b.trailer.FooterOffset)
		inSuffix := b.suffix[:len(b.suffix)-format.TrailerLen]
		if fo >= b.suffixStart {
			b.footer = inSuffix[fo-b.suffixStart:]
		} else {
			b.footer = make([]byte, b.trailer.FooterLen)
			n := b.suffixStart - fo
			copy(b.footer[n:], inSuffix)
			reqs = append(reqs, readReq{off: fo, buf: b.footer[:n]})
		}
	}

	if len(reqs) == 0 {
		return nil
	}

	return readRound(ctx, r.ra, reqs)
}

// decodeMeta checks the pack metadata CRC and decodes the pack metadata,
// and keeps both the bytes and the decoded value.
func (r *Reader) decodeMeta(raw []byte) error {
	if crc := format.CRC(raw); crc != r.hdr.PackMetadataCRC {
		return fmt.Errorf("tracepack: pack metadata CRC 0x%08X, pack_metadata_crc 0x%08X: %w", crc, r.hdr.PackMetadataCRC, ErrChecksum)
	}

	meta, err := UnmarshalPackMeta(raw)
	if err != nil {
		return err
	}
	r.metaRaw, r.meta = raw, meta

	return nil
}

// settleFooter decides finalization from the trailer and the footer CRC (I-5),
// then decodes and validates a footer within MaxFooterLen (§10).
// Every outcome that leaves the footer unused is recorded in footerErr, never returned;
// only a read or ctx error fails it.
func (r *Reader) settleFooter(ctx context.Context, b *bootstrap) error {
	if b.trailerErr != nil {
		r.footerErr = fmt.Errorf("tracepack: no valid trailer: %w", b.trailerErr)

		return nil
	}

	tr := &b.trailer
	r.trailer = &TrailerInfo{BlockCount: tr.BlockCount, RecordCount: tr.RecordCount, LastSeq: tr.LastSeq, FooterOffset: tr.FooterOffset}

	var crc uint32
	if b.footer != nil {
		crc = format.CRC(b.footer)
	} else {
		var err error
		if crc, err = r.streamFooterCRC(ctx, tr); err != nil {
			return err
		}
	}
	if crc != tr.FooterCRC {
		r.footerErr = fmt.Errorf("tracepack: footer at offset %d: CRC 0x%08X, footer_crc 0x%08X: %w",
			tr.FooterOffset, crc, tr.FooterCRC, ErrChecksum)

		return nil
	}

	r.finalized = true

	if b.footer == nil {
		r.footerErr = fmt.Errorf("tracepack: footer at offset %d: footer_len %d exceeds MaxFooterLen %d: %w",
			tr.FooterOffset, tr.FooterLen, r.opts.MaxFooterLen, ErrReadLimit)

		return nil
	}
	r.footer, r.footerErr = r.decodeFooter(b.footer, tr)

	return nil
}

// streamFooterCRC computes the CRC of the on-disk footer of tr over sequential reads of at most 1 MiB,
// so a footer over MaxFooterLen settles finalization without being buffered.
func (r *Reader) streamFooterCRC(ctx context.Context, tr *format.Trailer) (uint32, error) {
	buf := make([]byte, min(tr.FooterLen, footerCRCChunk))

	var crc uint32
	for off, end := tr.FooterOffset, tr.FooterOffset+tr.FooterLen; off < end; {
		chunk := buf[:min(end-off, footerCRCChunk)]
		if err := readRound(ctx, r.ra, []readReq{{off: int64(off), buf: chunk}}); err != nil {
			return 0, err
		}
		crc = format.UpdateCRC(crc, chunk)
		off += uint64(len(chunk))
	}

	return crc, nil
}

// decodeFooter decodes the CRC-valid on-disk footer of tr and validates it (§10).
//
// Returns:
//   - *footerIndex: the validated footer; nil on error.
//   - error: ErrReadLimit for a footer_uncompressed_len over MaxFooterLen;
//     ErrInvalidFooter for an unknown footer_codec, a decode failure or a failed validation.
func (r *Reader) decodeFooter(onDisk []byte, tr *format.Trailer) (*footerIndex, error) {
	if limit := min(uint64(r.opts.MaxFooterLen), math.MaxInt); tr.FooterUncompressedLen > limit {
		return nil, fmt.Errorf("tracepack: footer at offset %d: footer_uncompressed_len %d exceeds MaxFooterLen %d: %w",
			tr.FooterOffset, tr.FooterUncompressedLen, r.opts.MaxFooterLen, ErrReadLimit)
	}

	decoded, err := codec.Decode(tr.FooterCodec, nil, onDisk, int(tr.FooterUncompressedLen))
	if err != nil {
		return nil, fmt.Errorf("%w: footer_codec %d: %w", ErrInvalidFooter, tr.FooterCodec, err)
	}

	return parseFooter(decoded, tr, r.blocksStart(), r.keepF3)
}

// rawF3 returns block i's F-3 entry list exactly as the pack's validated footer stores it,
// the list a merge copies verbatim with the block (the tracepack format specification §10 aggregation rule).
// It returns nil unless the Reader was opened by openReader with keepF3 set:
// without it the Reader keeps no F-3 bytes.
// It also returns nil when the pack's blocks come from the forward walk, since the footer is invalid or not used.
// The list aliases the Reader's copy of the F-3 section, which lives as long as the Reader and is never modified;
// the caller must not modify it.
// Its capacity ends where the list does, so an append never writes into the section.
func (r *Reader) rawF3(i int) []byte {
	if r.footer == nil || !r.keepF3 {
		return nil
	}

	return r.footer.f3List(i)
}

// fileHeaderClass returns the public sentinel for an error of format.UnmarshalFileHeader:
// ErrNotTracepack for a bad magic or a short buffer, ErrChecksum for a CRC mismatch,
// ErrUnsupportedFormat for an unknown format_major, and nil for any other error.
func fileHeaderClass(err error) error {
	switch {
	case errors.Is(err, format.ErrBadMagic), errors.Is(err, format.ErrShort):
		return ErrNotTracepack
	case errors.Is(err, format.ErrCRC):
		return ErrChecksum
	case errors.Is(err, format.ErrSchema):
		return ErrUnsupportedFormat
	default:
		return nil
	}
}
