package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

// Outcome is the verification outcome of a pack (the tracepack format specification §13).
//
// The zero value is not an outcome; String prints it and any undefined value as "unknown(<n>)".
type Outcome uint8

const (
	// OutcomeFinalizedConsistent reports a finalized pack whose blocks all validate
	// and whose footer and trailer agree with them.
	OutcomeFinalizedConsistent Outcome = iota + 1
	// OutcomeFinalizedInconsistent reports a finalized pack whose blocks all validate,
	// while its footer is invalid or disagrees with the records or the trailer:
	// no record is lost, only the index is wrong.
	OutcomeFinalizedInconsistent
	// OutcomeFinalizedTruncated reports a finalized pack in which a block fails or the forward walk stops,
	// with no validated block after it.
	OutcomeFinalizedTruncated
	// OutcomeUnfinalized reports a pack that is not finalized (I-5).
	OutcomeUnfinalized
	// OutcomeCorruptMiddle reports a pack in which a failed block is followed by a validated one.
	OutcomeCorruptMiddle
)

var _ fmt.Stringer = Outcome(0)

// String returns the tracepack format specification's name for o, or "unknown(<n>)" for a value it does not define.
func (o Outcome) String() string {
	switch o {
	case OutcomeFinalizedConsistent:
		return "finalized-consistent"
	case OutcomeFinalizedInconsistent:
		return "finalized-inconsistent"
	case OutcomeFinalizedTruncated:
		return "finalized-truncated"
	case OutcomeUnfinalized:
		return "unfinalized"
	case OutcomeCorruptMiddle:
		return "corrupt-middle"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(o))
	}
}

// WriterDefectKind classifies a writer defect in a validated block (the tracepack format specification §13).
//
// The zero value is not a kind; String prints it and any undefined value as "unknown(<n>)".
type WriterDefectKind uint8

const (
	// WriterDefectFieldValidity reports a data or control record
	// whose field_validity marks an HSMS header field its payload lacks (§7.2).
	WriterDefectFieldValidity WriterDefectKind = iota + 1
	// WriterDefectEventFieldValidity reports a transport-event or annotation record whose field_validity is not 0 (§7.2).
	WriterDefectEventFieldValidity
	// WriterDefectEventPayload reports a transport-event or annotation record whose payload is not a valid TLV body (§8).
	WriterDefectEventPayload
	// WriterDefectSeqOrder reports a block whose first seq is not above the last seq of the validated block before it (I-12).
	WriterDefectSeqOrder
	// WriterDefectHourSpan reports a block whose records lie in more than one UTC hour (I-13).
	WriterDefectHourSpan
)

var _ fmt.Stringer = WriterDefectKind(0)

// String returns the kind's name, or "unknown(<n>)" for a value this package does not define.
func (k WriterDefectKind) String() string {
	switch k {
	case WriterDefectFieldValidity:
		return "field-validity"
	case WriterDefectEventFieldValidity:
		return "event-field-validity"
	case WriterDefectEventPayload:
		return "event-payload"
	case WriterDefectSeqOrder:
		return "seq-order"
	case WriterDefectHourSpan:
		return "hour-span"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// WriterDefect is a defect a writer left in a validated block.
// It is reported beside the outcome and never changes it (the tracepack format specification §13).
type WriterDefect struct {
	// Kind classifies the defect.
	Kind WriterDefectKind
	// Block is the index of the block in file order, as VerifyReport.Blocks counts them.
	Block int
	// Offset is the file offset of the block's envelope.
	Offset int64
	// Seq is the seq of the record concerned;
	// for WriterDefectSeqOrder and WriterDefectHourSpan, the block's first seq.
	Seq uint64
	// Err describes the defect.
	Err error
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Reader configures the bootstrap and the reader budgets; a zero field takes its default, as for Open.
	Reader ReaderOptions
}

// VerifyReport is the result of Verify (the tracepack format specification §13).
type VerifyReport struct {
	// Outcome is the pack's verification outcome.
	Outcome Outcome
	// Finalized reports I-5: the trailer magic and CRC match, the trailer places the footer after the pack metadata,
	// and the footer CRC matches.
	Finalized bool
	// FooterErr is PackHeader.FooterErr: non-nil when the footer was not used and the blocks came from the forward walk.
	FooterErr error
	// Blocks is the number of blocks located: the F-2 entries of a valid footer, else the envelopes the forward walk found.
	Blocks int
	// Validated is the number of located blocks that did not fail.
	Validated int
	// Records is the number of records in the validated blocks.
	Records uint64
	// PrefixEnd is the file offset where the validated prefix ends:
	// the envelope of the first failed block, else the point where the forward walk stopped,
	// else the end of the last block, or the end of the pack metadata for a pack without blocks.
	PrefixEnd uint64
	// Failed lists, in file order, every failed block (ReasonCorruptBlock or ReasonUnknownCodec)
	// and the point where the forward walk stopped (ReasonTruncated, Block -1).
	Failed []Defect
	// Disagreements lists, with ReasonIndexMismatch, every validated block whose records disagree with its F-2 entry or F-3 summary,
	// and, with Block -1, walked blocks that disagree with the trailer's block_count or record_count.
	Disagreements []Defect
	// WriterDefects lists the writer defects of the validated blocks, in file order.
	WriterDefects []WriterDefect
	// Lost holds, when the footer is valid, one coverage entry per seq range of each failed block:
	// the pack's capture, the range, and the block's F-2 time range.
	Lost []Coverage
}

// classifyOutcome returns the outcome of a pack (the tracepack format specification §13), the first of these that applies:
// corrupt-middle when a failed block precedes a validated one;
// unfinalized when the pack is not finalized;
// finalized-truncated when a block failed or the forward walk stopped;
// finalized-inconsistent when the footer is invalid or disagrees with the records or the trailer;
// else finalized-consistent.
//
// failed holds, for every located block in file order, whether it failed;
// a walk stop lies after every located block, so no validated block follows it.
func classifyOutcome(finalized bool, failed []bool, walkStopped, inconsistent bool) Outcome {
	seenFailed := false
	for _, f := range failed {
		if f {
			seenFailed = true
		} else if seenFailed {
			return OutcomeCorruptMiddle
		}
	}

	switch {
	case !finalized:
		return OutcomeUnfinalized
	case seenFailed || walkStopped:
		return OutcomeFinalizedTruncated
	case inconsistent:
		return OutcomeFinalizedInconsistent
	default:
		return OutcomeFinalizedConsistent
	}
}

// Verify checks a pack as the tracepack format specification §13 describes and reports its outcome.
//
// It bootstraps the pack as Open does, then reads every located block in full:
// the F-2 entries of a valid footer, which pass a block whose envelope fails,
// else the envelopes the forward walk found, which stops at an envelope it cannot account for.
// A block fails on its envelope, its body CRC, its codec, I-2, or its envelope's agreement with its F-2 entry;
// a block that does not fail is validated.
// Verify reads blocks one at a time and keeps none of their records.
//
// Parameters:
//   - ctx: cancels the verification between blocks.
//   - ra: the pack's bytes.
//   - size: the pack's size in bytes.
//   - opts: the bootstrap and the reader budgets.
//
// Returns:
//   - VerifyReport: the outcome and its evidence; the zero VerifyReport on an error.
//   - error: every error of Open, for a file header or pack metadata that cannot be read;
//     an error wrapping ErrReadLimit for a block over MaxBlockLen,
//     for the footer of a finalized pack over MaxFooterLen, on disk or decoded,
//     or for a forward walk that reached MaxWalkedBlocks;
//     ctx's error, wrapped; a ReadAt error, wrapping io.ErrUnexpectedEOF for a short read.
func Verify(ctx context.Context, ra io.ReaderAt, size int64, opts VerifyOptions) (VerifyReport, error) {
	r, err := Open(ctx, ra, size, opts.Reader)
	if err != nil {
		return VerifyReport{}, err
	}

	return r.verify(ctx)
}

// verify reads every located block of r in full and builds the VerifyReport of Verify.
func (r *Reader) verify(ctx context.Context) (VerifyReport, error) {
	if r.finalized && errors.Is(r.footerErr, ErrReadLimit) {
		return VerifyReport{}, fmt.Errorf("tracepack: verify: %w", r.footerErr)
	}
	if r.walkStop != nil && r.walkStop.Reason == ReasonLimit {
		return VerifyReport{}, fmt.Errorf("tracepack: verify: %w", r.walkStop.Err)
	}

	rep := VerifyReport{Finalized: r.finalized, FooterErr: r.footerErr, Blocks: len(r.blocks), PrefixEnd: r.blocksStart()}
	failed := make([]bool, len(r.blocks))
	inPrefix := true
	// prevSeq is the last seq of the latest validated block; valid iff hasPrev.
	var (
		prevSeq uint64
		hasPrev bool
	)

	var buf blockBuf
	for i := range r.blocks {
		if err := ctx.Err(); err != nil {
			return VerifyReport{}, fmt.Errorf("tracepack: verify: %w", err)
		}

		info := &r.blocks[i]
		d, def, err := r.readBlock(i, &buf)
		if err != nil {
			return VerifyReport{}, err
		}
		if def != nil && def.Reason == ReasonLimit {
			return VerifyReport{}, fmt.Errorf("tracepack: verify: %w", def.Err)
		}
		if d == nil {
			failed[i] = true
			inPrefix = false
			rep.Failed = append(rep.Failed, *def)
			rep.Lost = r.appendLost(rep.Lost, i)

			continue
		}
		if def != nil {
			rep.Disagreements = append(rep.Disagreements, *def)
		}
		if r.footer != nil {
			if err := checkSummary(&r.footer.blocks[i], d); err != nil {
				rep.Disagreements = append(rep.Disagreements, *blockDefect(i, info, ReasonIndexMismatch, err))
			}
		}

		rep.WriterDefects = appendWriterDefects(rep.WriterDefects, i, info, d, prevSeq, hasPrev)
		if n := d.count(); n > 0 {
			prevSeq, hasPrev = d.header(n-1).Seq, true
		}

		rep.Validated++
		rep.Records += uint64(d.count())
		if inPrefix {
			rep.PrefixEnd = info.Offset + uint64(info.OnDiskLen)
		}
	}
	if r.walkStop != nil {
		rep.Failed = append(rep.Failed, *r.walkStop)
	} else if err := r.walkTotalsErr(); err != nil {
		rep.Disagreements = append(rep.Disagreements, Defect{Reason: ReasonIndexMismatch, Block: -1, Offset: -1, Err: err})
	}

	inconsistent := r.footerErr != nil || len(rep.Disagreements) > 0
	rep.Outcome = classifyOutcome(r.finalized, failed, r.walkStop != nil, inconsistent)

	return rep, nil
}

// appendLost appends to dst, when the footer is valid, one coverage entry per seq range of failed block i:
// the pack's capture, the range, and the block's F-2 time range.
func (r *Reader) appendLost(dst []Coverage, i int) []Coverage {
	if r.footer == nil {
		return dst
	}

	s := &r.footer.blocks[i]
	for _, sr := range s.seqRanges {
		dst = append(dst, Coverage{
			CaptureID: new(UUID(r.hdr.CaptureID)),
			SeqFirst:  new(sr.first), SeqLast: new(sr.last),
			TimeStart: new(s.tsMin), TimeEnd: new(s.tsMax),
		})
	}

	return dst
}

// walkTotalsErr compares a finalized pack's walked blocks with its trailer's block_count and record_count,
// the check a valid footer's validation makes otherwise (the tracepack format specification §10);
// nil when the footer is valid, the pack is not finalized, or the totals agree.
func (r *Reader) walkTotalsErr() error {
	if r.footer != nil || !r.finalized {
		return nil
	}

	var records uint64
	for i := range r.blocks {
		records = addSat(records, uint64(r.blocks[i].RecordCount))
	}
	if uint64(len(r.blocks)) != uint64(r.trailer.BlockCount) || records != r.trailer.RecordCount {
		return fmt.Errorf("tracepack: forward walk found %d blocks of %d records, the trailer claims %d blocks of %d records",
			len(r.blocks), records, r.trailer.BlockCount, r.trailer.RecordCount)
	}

	return nil
}

// checkSummary compares stated, the F-3 summary of an indexed block, with the summary its records d give,
// built as the Writer builds it (the tracepack format specification §10):
// count arrays equal once trailing zeros are dropped, quality_union and seq ranges exactly equal,
// epoch entries equal as a set keyed by epoch, and boundary entries equal as a multiset.
// The F-2 fields are compared by the block read itself.
func checkSummary(stated *blockSummary, d *decodedBlock) error {
	var got blockSummary
	for j := range d.count() {
		h := d.header(j)
		var ev *TransportEvent
		if Kind(h.Kind) == KindTransportEvent {
			// A payload that does not decode contributes nothing, as in the Writer's summary.
			ev, _ = UnmarshalTransportEvent(d.payload(j))
		}
		got.addRecord(&h, ev)
	}

	counts := [...]struct {
		name      string
		got, want []uint32
	}{
		{"kind_counts", got.kindCounts, stated.kindCounts},
		{"dir_counts", got.dirCounts, stated.dirCounts},
		{"decode_status_counts", got.decodeStatusCounts, stated.decodeStatusCounts},
	}
	for _, c := range counts {
		if !slices.Equal(trimZeros(c.got), trimZeros(c.want)) {
			return fmt.Errorf("F-3 %s %v, the records give %v", c.name, c.want, c.got)
		}
	}

	switch {
	case got.qualityUnion != stated.qualityUnion:
		return fmt.Errorf("F-3 quality_union 0x%04X, the records give 0x%04X", uint16(stated.qualityUnion), uint16(got.qualityUnion))
	case !slices.Equal(got.seqRanges, stated.seqRanges):
		return fmt.Errorf("F-3 seq_range entries %v, the records hold %v", stated.seqRanges, got.seqRanges)
	case !slices.Equal(slices.SortedFunc(slices.Values(got.epochs), compareEpochs), slices.SortedFunc(slices.Values(stated.epochs), compareEpochs)):
		return fmt.Errorf("F-3 epoch entries differ from the records' epochs")
	case !equalBoundarySets(got.boundaries, stated.boundaries):
		return fmt.Errorf("F-3 boundary entries differ from the records' boundaries")
	}

	return nil
}

// appendWriterDefects appends to dst the writer defects of validated block i, read as d
// (the tracepack format specification §13), in this order:
// a first seq not above prevSeq, the last seq of the validated block before it, when hasPrev (I-12);
// per record, a data or control record whose field_validity marks a field its payload lacks (§7.2),
// and a transport-event or annotation record whose field_validity is not 0 or whose payload is not a valid TLV body (§8);
// then records in more than one UTC hour (I-13).
// Reserved field_validity bits are ignored (I-9), and a record of an unknown kind is not checked.
func appendWriterDefects(dst []WriterDefect, i int, info *BlockInfo, d *decodedBlock, prevSeq uint64, hasPrev bool) []WriterDefect {
	n := d.count()
	if n == 0 {
		return dst
	}
	add := func(kind WriterDefectKind, seq uint64, err error) {
		dst = append(dst, WriterDefect{Kind: kind, Block: i, Offset: int64(info.Offset), Seq: seq, Err: err})
	}

	first := d.header(0)
	if hasPrev && first.Seq <= prevSeq {
		add(WriterDefectSeqOrder, first.Seq, fmt.Errorf(
			"tracepack: block %d at offset %d: first seq %d is not above the previous block's last seq %d", i, info.Offset, first.Seq, prevSeq))
	}

	tsMin, tsMax := first.TSUTCNs, first.TSUTCNs
	for j := range n {
		h := d.header(j)
		tsMin, tsMax = min(tsMin, h.TSUTCNs), max(tsMax, h.TSUTCNs)

		payload := d.payload(j)
		fv := FieldValidity(h.FieldValidity) & fieldValidityMask
		// A record of another kind, unclassified or outside the registry, has no rule to check.
		if kind := Kind(h.Kind); kind == KindData || kind == KindControl {
			if beyond := fv &^ capturedFields(len(payload)); beyond != 0 {
				add(WriterDefectFieldValidity, h.Seq, fmt.Errorf("tracepack: record seq %d: field_validity marks %s, but the payload is %d bytes: %w",
					h.Seq, beyond, len(payload), ErrFieldValidity))
			}
		} else if kind == KindTransportEvent || kind == KindAnnotation {
			if fv != 0 {
				add(WriterDefectEventFieldValidity, h.Seq, fmt.Errorf("tracepack: %s record seq %d: field_validity %s, not 0", kind, h.Seq, fv))
			}
			if err := checkEventPayload(kind, payload); err != nil {
				add(WriterDefectEventPayload, h.Seq, fmt.Errorf("tracepack: %s record seq %d: %w", kind, h.Seq, err))
			}
		}
	}

	if hourOf(tsMin) != hourOf(tsMax) {
		add(WriterDefectHourSpan, first.Seq, fmt.Errorf(
			"tracepack: block %d at offset %d: records span %d to %d, more than one UTC hour", i, info.Offset, tsMin, tsMax))
	}

	return dst
}

// checkEventPayload decodes the TLV body of a transport-event or annotation record, kind, and returns its error (§8).
func checkEventPayload(kind Kind, payload []byte) error {
	var err error
	if kind == KindTransportEvent {
		_, err = UnmarshalTransportEvent(payload)
	} else {
		_, err = UnmarshalAnnotation(payload)
	}

	return err
}
