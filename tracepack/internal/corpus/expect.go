package corpus

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// check compares e with the reads out of pack, field by field, and returns every disagreement.
// It also checks that every block offset the reads name is the envelope Locate finds for that block,
// when Locate finds as many blocks as the verification locates.
func (e *Expectation) check(out *readOutputs, pack []byte) error {
	var errs []error
	want := func(name string, exp, got any) {
		if !reflect.DeepEqual(exp, got) {
			errs = append(errs, fmt.Errorf("%s: expected %v, read %v", name, exp, got))
		}
	}

	if e.Rejection != "" {
		want("rejection", e.Rejection, out.rejection)

		return errors.Join(errs...)
	}

	l, lerr := Locate(pack)
	if lerr != nil {
		return fmt.Errorf("the pack's layout: %w", lerr)
	}
	pos := func(name string, p Pos, got *U64) {
		switch {
		case p.kind == posNone:
			want(name, (*U64)(nil), got)
		case got == nil:
			errs = append(errs, fmt.Errorf("%s: expected a position, read none", name))
		default:
			off, err := p.offset(&l)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				return
			}
			want(name, U64(off), *got)
		}
	}

	v := &out.verify
	want("outcome", e.Outcome.String(), v.Outcome)
	want("finalized", !e.Unfinalized, v.Finalized)
	want("footer_valid", e.FooterValid, v.FooterValid)
	want("blocks_located", e.Blocks, v.BlocksLocated)
	want("blocks_validated", e.Blocks-len(e.Failed), v.BlocksValidated)
	want("seqs", nonNil(e.Seqs), nonNil(out.seqs))
	want("records", U64(len(e.Seqs)), v.Records)
	failed := []FailedWant{}
	for _, f := range v.FailedBlocks {
		failed = append(failed, FailedWant{Block: f.Block, Cause: reasonNamed(f.Cause)})
	}
	want("failed_blocks", nonNil(e.Failed), failed)
	pos("walk_stop", e.WalkStop, v.WalkStop)
	pos("prefix_end", e.PrefixEnd, &v.PrefixEnd)
	disagreeing := []int{}
	for _, d := range v.DisagreeingBlocks {
		disagreeing = append(disagreeing, d.Block)
	}
	want("disagreeing_blocks", nonNil(e.Disagreeing), disagreeing)
	want("trailer_totals_disagree", e.TrailerTotalsDisagree, v.TrailerTotalsDisagree)
	defects := []DefectWant{}
	for _, d := range v.WriterDefects {
		defects = append(defects, DefectWant{Kind: defectNamed(d.Kind), Seq: uint64(d.Seq)})
	}
	want("writer_defects", nonNil(e.WriterDefects), defects)
	lost := []LostWant{}
	captureID := tracepack.UUID(pack[headerCaptureIDOff : headerCaptureIDOff+16]).String()
	for _, c := range v.Lost {
		var w LostWant
		if c.SeqFirst != nil && c.SeqLast != nil && c.TimeStart != nil && c.TimeEnd != nil {
			w = LostWant{First: uint64(*c.SeqFirst), Last: uint64(*c.SeqLast), TimeStart: int64(*c.TimeStart), TimeEnd: int64(*c.TimeEnd)}
		}
		lost = append(lost, w)
		if c.CaptureID == nil || *c.CaptureID != captureID {
			errs = append(errs, fmt.Errorf("lost: an entry of another capture_id than the pack's %s", captureID))
		}
	}
	want("lost", nonNil(e.Lost), lost)
	for _, s := range e.ExportHas {
		if !strings.Contains(string(out.export), s) {
			errs = append(errs, fmt.Errorf("export.jsonl does not hold %s", s))
		}
	}
	if len(l.Blocks) == v.BlocksLocated {
		errs = append(errs, blockOffsets(&l, v, out.queries)...)
	}

	errs = append(errs, e.checkStats(out)...)
	if want := normalFooter(e.Footer); !reflect.DeepEqual(want, out.footer) {
		errs = append(errs, fmt.Errorf("footer.json: expected %s, read %s", describe(want), describe(out.footer)))
	}
	errs = append(errs, e.checkQueries(out, pos)...)
	errs = append(errs, e.checkRepair(out)...)

	return errors.Join(errs...)
}

// checkStats compares e.Stats, when set, with the statistics of out.
func (e *Expectation) checkStats(out *readOutputs) []error {
	if e.Stats == nil {
		return nil
	}
	got := StatsWant{Available: out.statsOK, Epochs: []EpochWant{}, Boundaries: []BoundaryWant{}}
	for _, ep := range out.stats.Epochs {
		got.Epochs = append(got.Epochs, EpochWant{Epoch: ep.Epoch, CloseSeq: ep.CloseSeq})
	}
	for _, b := range out.stats.Boundaries {
		got.Boundaries = append(got.Boundaries, BoundaryWant{Seq: b.Seq, Kind: b.Kind})
	}
	exp := StatsWant{Available: e.Stats.Available, Epochs: nonNil(e.Stats.Epochs), Boundaries: nonNil(e.Stats.Boundaries)}
	if !reflect.DeepEqual(exp, got) {
		return []error{fmt.Errorf("stats: expected %+v, read %+v", exp, got)}
	}

	return nil
}

// checkQueries compares e.Queries with the queries of out, one by one and by id.
func (e *Expectation) checkQueries(out *readOutputs, pos func(string, Pos, *U64)) []error {
	if len(e.Queries) != len(out.queries) {
		return []error{fmt.Errorf("queries: %d expected, %d read", len(e.Queries), len(out.queries))}
	}

	var errs []error
	for i, q := range out.queries {
		w := e.Queries[i]
		if w.ID != q.ID {
			errs = append(errs, fmt.Errorf("query %d: expected id %s, read %s", i, w.ID, q.ID))
			continue
		}
		seqs := []uint64{}
		for _, s := range q.Expect.Seqs {
			seqs = append(seqs, uint64(s))
		}
		if !slices.Equal(nonNil(w.Seqs), seqs) {
			errs = append(errs, fmt.Errorf("query %s: expected seqs %v, read %v", q.ID, w.Seqs, seqs))
		}
		if len(w.Incomplete) != len(q.Expect.Incomplete) {
			errs = append(errs, fmt.Errorf("query %s: expected %d incomplete reasons, read %+v", q.ID, len(w.Incomplete), q.Expect.Incomplete))
			continue
		}
		for k, in := range q.Expect.Incomplete {
			iw := w.Incomplete[k]
			block := -1
			if in.Block != nil {
				block = *in.Block
			}
			if iw.Reason.String() != in.Reason || iw.Block != block {
				errs = append(errs, fmt.Errorf("query %s: incomplete %d: expected %s of block %d, read %s of block %d",
					q.ID, k, iw.Reason, iw.Block, in.Reason, block))
			}
			pos(fmt.Sprintf("query %s incomplete %d", q.ID, k), iw.At, in.Offset)
		}
	}

	return errs
}

// checkRepair compares e.Repair with the repair of out.
func (e *Expectation) checkRepair(out *readOutputs) []error {
	switch {
	case e.Repair == nil && out.repair == nil:
		return nil
	case e.Repair == nil || out.repair == nil:
		return []error{fmt.Errorf("repair: expected %+v, read %+v", e.Repair, out.repair)}
	}

	r := out.repair
	got := RepairWant{Result: r.Result}
	if r.Blocks != nil {
		got.Blocks = *r.Blocks
	}
	if r.Records != nil {
		got.Records = uint64(*r.Records)
	}
	if r.CoverageAdded != nil {
		got.Coverage = []CoverageWant{}
		for _, c := range *r.CoverageAdded {
			var w CoverageWant
			if c.SeqFirst != nil {
				w.First = uint64(*c.SeqFirst)
			}
			if c.SeqLast != nil {
				w.Last = new(uint64(*c.SeqLast))
			}
			got.Coverage = append(got.Coverage, w)
		}
	}
	exp := *e.Repair
	if exp.Result == RepairPatched {
		exp.Coverage = nonNil(exp.Coverage)
	}
	if !reflect.DeepEqual(exp, got) {
		return []error{fmt.Errorf("repair: expected %+v, read %+v", exp, got)}
	}

	return nil
}

// blockOffsets checks that every block a verification or a query names carries the offset of the envelope Locate finds for it.
func blockOffsets(l *Layout, v *Verify, queries []QueryVector) []error {
	var errs []error
	at := func(what string, block int, off U64) {
		s, err := l.block(block)
		if err != nil || U64(s.Offset) != off {
			errs = append(errs, fmt.Errorf("%s names block %d at offset %d, which is not its envelope", what, block, off))
		}
	}
	for _, f := range v.FailedBlocks {
		at("failed_blocks", f.Block, f.Offset)
	}
	for _, d := range v.DisagreeingBlocks {
		at("disagreeing_blocks", d.Block, d.Offset)
	}
	for _, d := range v.WriterDefects {
		at("writer_defects", d.Block, d.Offset)
	}
	for _, q := range queries {
		for _, in := range q.Expect.Incomplete {
			if in.Block != nil && in.Offset != nil {
				at("query "+q.ID, *in.Block, *in.Offset)
			}
		}
	}

	return errs
}

// normalFooter returns a copy of f whose absent arrays are empty, as footer.json writes them; nil for nil.
func normalFooter(f *Footer) *Footer {
	if f == nil {
		return nil
	}
	proj := func(p FooterProjection) FooterProjection {
		out := FooterProjection{Blocks: []FooterBlock{}, F5: FooterF5{Epochs: nonNil(p.F5.Epochs), Boundaries: nonNil(p.F5.Boundaries)}}
		for _, b := range p.Blocks {
			out.Blocks = append(out.Blocks, FooterBlock{Block: b.Block, CloseSeqs: nonNil(b.CloseSeqs), Boundaries: nonNil(b.Boundaries)})
		}

		return out
	}
	out := &Footer{Accepted: f.Accepted, Recomputed: proj(f.Recomputed)}
	if f.Stored != nil {
		out.Stored = new(proj(*f.Stored))
	}

	return out
}

// describe returns the canonical JSON of f, or "none".
func describe(f *Footer) string {
	if f == nil {
		return "none"
	}
	b, err := f.Marshal()
	if err != nil {
		return fmt.Sprintf("%+v", *f)
	}

	return string(b)
}

// nonNil returns s, or an empty slice for nil, so that an expectation of nothing compares equal to an empty read.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}

	return s
}

// reasonNamed returns the incomplete reason whose String is name, or 0.
func reasonNamed(name string) tracepack.IncompleteReason {
	r, _ := enumByName[tracepack.IncompleteReason](name)

	return r
}

// defectNamed returns the writer defect kind whose String is name, or 0.
func defectNamed(name string) tracepack.WriterDefectKind {
	k, _ := enumByName[tracepack.WriterDefectKind](name)

	return k
}
