package corpus

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
)

// Verify is verify.json, the canonical form of a verification report (the tracepack corpus specification §5.2).
type Verify struct {
	Outcome         string        `json:"outcome"`
	Finalized       bool          `json:"finalized"`
	FooterValid     bool          `json:"footer_valid"`
	BlocksLocated   int           `json:"blocks_located"`
	BlocksValidated int           `json:"blocks_validated"`
	Records         U64           `json:"records"`
	PrefixEnd       U64           `json:"prefix_end"`
	FailedBlocks    []FailedBlock `json:"failed_blocks"`
	// WalkStop is the offset of the envelope the forward walk could not account for; nil when the walk did not stop.
	WalkStop              *U64           `json:"walk_stop,omitempty"`
	DisagreeingBlocks     []BlockAt      `json:"disagreeing_blocks"`
	TrailerTotalsDisagree bool           `json:"trailer_totals_disagree"`
	WriterDefects         []WriterDefect `json:"writer_defects"`
	Lost                  []Coverage     `json:"lost"`
}

// FailedBlock is a failed block of verify.json: its index, its envelope's offset and its cause.
type FailedBlock struct {
	Block  int    `json:"block"`
	Offset U64    `json:"offset"`
	Cause  string `json:"cause"`
}

// BlockAt is a block of verify.json named by its index and its envelope's offset.
type BlockAt struct {
	Block  int `json:"block"`
	Offset U64 `json:"offset"`
}

// WriterDefect is a writer defect of verify.json.
type WriterDefect struct {
	Kind   string `json:"kind"`
	Block  int    `json:"block"`
	Offset U64    `json:"offset"`
	Seq    U64    `json:"seq"`
}

// VerifyFrom returns the verify.json of rep, sorted into the orders of the tracepack corpus specification §5.2:
// failed blocks and disagreeing blocks ascending by block,
// a block that disagrees with both its F-2 entry and its F-3 summary listed once;
// writer defects block by block,
// within a block seq-start, seq-order, the per-record defects in record order, then hour-span;
// lost coverage by block, then by seq_first.
// The failed entry with block -1 is the walk stop, and the disagreement with block -1 the trailer's totals.
//
// Returns:
//   - Verify: the projection.
//   - error: a defect of the reader's own limits, which is never a golden;
//     a defect of a reason or a writer defect of a kind verify.json does not hold;
//     a block defect without an offset; more than one walk stop or totals disagreement; a lost entry without seq_first.
func VerifyFrom(rep *tracepack.VerifyReport) (Verify, error) {
	v := Verify{
		Outcome: rep.Outcome.String(), Finalized: rep.Finalized, FooterValid: rep.Finalized && rep.FooterErr == nil,
		BlocksLocated: rep.Blocks, BlocksValidated: rep.Validated, Records: U64(rep.Records), PrefixEnd: U64(rep.PrefixEnd),
		FailedBlocks: []FailedBlock{}, DisagreeingBlocks: []BlockAt{}, WriterDefects: []WriterDefect{}, Lost: []Coverage{},
	}

	if err := v.addFailed(rep.Failed); err != nil {
		return Verify{}, err
	}
	if err := v.addDisagreements(rep.Disagreements); err != nil {
		return Verify{}, err
	}
	if err := v.addWriterDefects(rep.WriterDefects); err != nil {
		return Verify{}, err
	}
	lost, err := sortedCoverage(rep.Lost)
	if err != nil {
		return Verify{}, err
	}
	v.Lost = lost

	return v, nil
}

// Marshal returns verify.json in the canonical form.
func (v *Verify) Marshal() ([]byte, error) {
	return Marshal(v)
}

// sortedCoverage renders entries as coverage objects ascending by seq_first.
// The coverage the tracepack format specification §13 derives for failed blocks
// lies within seq ranges that ascend from block to block
// (a valid footer's seqs ascend across its F-2 entries, and a lost run's bounds follow the validated blocks around it),
// so ascending seq_first is the order by block, then by seq_first.
func sortedCoverage(entries []tracepack.Coverage) ([]Coverage, error) {
	idx := make([]int, len(entries))
	for i := range entries {
		if entries[i].SeqFirst == nil {
			return nil, fmt.Errorf("corpus: coverage entry %d has no seq_first to order it by", i)
		}
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return cmp.Compare(*entries[a].SeqFirst, *entries[b].SeqFirst) })

	out := make([]Coverage, 0, len(entries))
	for _, i := range idx {
		out = append(out, CoverageFrom(&entries[i]))
	}

	return out, nil
}

// blockOffset returns the offset of the envelope of block defect d, which every block defect carries.
func blockOffset(d *tracepack.Defect) (U64, error) {
	if d.Offset < 0 {
		return 0, fmt.Errorf("corpus: %s defect of block %d has no offset", d.Reason, d.Block)
	}

	return U64(d.Offset), nil
}

// addFailed adds the failed blocks and the walk stop of failed, the VerifyReport's Failed.
func (v *Verify) addFailed(failed []tracepack.Defect) error {
	for i := range failed {
		d := &failed[i]
		switch {
		case d.Reason == tracepack.ReasonLimit:
			return fmt.Errorf("corpus: a reader limit was reached: %w", d.Err)
		case d.Block < 0 && d.Reason == tracepack.ReasonTruncated && d.Offset >= 0:
			if v.WalkStop != nil {
				return fmt.Errorf("corpus: the report holds two walk stops")
			}
			v.WalkStop = new(U64(d.Offset))
		case d.Block >= 0 && (d.Reason == tracepack.ReasonCorruptBlock || d.Reason == tracepack.ReasonUnknownCodec):
			off, err := blockOffset(d)
			if err != nil {
				return err
			}
			v.FailedBlocks = append(v.FailedBlocks, FailedBlock{Block: d.Block, Offset: off, Cause: d.Reason.String()})
		default:
			return fmt.Errorf("corpus: failed entry of reason %s and block %d is not a failed block or a walk stop", d.Reason, d.Block)
		}
	}
	slices.SortStableFunc(v.FailedBlocks, func(a, b FailedBlock) int { return cmp.Compare(a.Block, b.Block) })

	return nil
}

// addDisagreements adds the disagreeing blocks, each once, and the trailer totals' disagreement
// of ds, the VerifyReport's Disagreements.
func (v *Verify) addDisagreements(ds []tracepack.Defect) error {
	for i := range ds {
		d := &ds[i]
		switch {
		case d.Reason != tracepack.ReasonIndexMismatch:
			return fmt.Errorf("corpus: disagreement of reason %s", d.Reason)
		case d.Block < 0:
			if v.TrailerTotalsDisagree {
				return fmt.Errorf("corpus: the report holds two trailer totals disagreements")
			}
			v.TrailerTotalsDisagree = true
		default:
			off, err := blockOffset(d)
			if err != nil {
				return err
			}
			v.DisagreeingBlocks = append(v.DisagreeingBlocks, BlockAt{Block: d.Block, Offset: off})
		}
	}
	slices.SortStableFunc(v.DisagreeingBlocks, func(a, b BlockAt) int { return cmp.Compare(a.Block, b.Block) })
	v.DisagreeingBlocks = slices.CompactFunc(v.DisagreeingBlocks, func(a, b BlockAt) bool { return a.Block == b.Block })

	return nil
}

// addWriterDefects adds ws, the VerifyReport's WriterDefects, in the order of the tracepack format specification §13:
// block by block, within a block seq-start, seq-order, the per-record defects in record order, then hour-span.
// Within a validated block seqs strictly increase (I-2), so record order is seq order.
func (v *Verify) addWriterDefects(ws []tracepack.WriterDefect) error {
	type ranked struct {
		w          *tracepack.WriterDefect
		stage, sub int
	}
	rs := make([]ranked, 0, len(ws))
	for i := range ws {
		stage, sub, ok := writerDefectRank(ws[i].Kind)
		if !ok {
			return fmt.Errorf("corpus: writer defect of kind %s", ws[i].Kind)
		}
		if ws[i].Block < 0 || ws[i].Offset < 0 {
			return fmt.Errorf("corpus: %s writer defect without a block", ws[i].Kind)
		}
		rs = append(rs, ranked{w: &ws[i], stage: stage, sub: sub})
	}
	slices.SortStableFunc(rs, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(a.w.Block, b.w.Block), cmp.Compare(a.stage, b.stage), cmp.Compare(a.w.Seq, b.w.Seq), cmp.Compare(a.sub, b.sub))
	})

	for _, r := range rs {
		v.WriterDefects = append(v.WriterDefects, WriterDefect{Kind: r.w.Kind.String(), Block: r.w.Block, Offset: U64(r.w.Offset), Seq: U64(r.w.Seq)})
	}

	return nil
}

// writerDefectRank returns where a writer defect of kind k lies within its block (the tracepack format specification §13):
// stage 0 seq-start, 1 seq-order, 2 the per-record defects, 3 hour-span;
// within a record, sub orders field-validity, event-field-validity and event-payload.
func writerDefectRank(k tracepack.WriterDefectKind) (stage, sub int, ok bool) {
	switch k {
	case tracepack.WriterDefectSeqStart:
		return 0, 0, true
	case tracepack.WriterDefectSeqOrder:
		return 1, 0, true
	case tracepack.WriterDefectFieldValidity:
		return 2, 0, true
	case tracepack.WriterDefectEventFieldValidity:
		return 2, 1, true
	case tracepack.WriterDefectEventPayload:
		return 2, 2, true
	case tracepack.WriterDefectHourSpan:
		return 3, 0, true
	default:
		return 0, 0, false
	}
}
