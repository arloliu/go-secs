package corpus

import (
	"encoding/json"
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
	errs = append(errs, e.checkFields(out.fields)...)
	errs = append(errs, e.checkQueries(out, pos)...)
	errs = append(errs, e.checkRepair(out, captureID)...)
	errs = append(errs, e.checkCuts(out.truncation, &l, captureID)...)

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

// checkFields compares e.Fields with got, the rows of fields.json, nil when the vector has none.
func (e *Expectation) checkFields(got FieldRows) []error {
	switch {
	case e.Fields == nil && got != nil:
		return []error{fmt.Errorf("fields.json: none expected, %d rows read", len(got))}
	case e.Fields != nil && got == nil:
		return []error{fmt.Errorf("fields.json: %d rows expected, none read", len(e.Fields))}
	}
	exp := FieldRows{}
	for _, f := range e.Fields {
		f.Available = nonNil(f.Available)
		exp = append(exp, f)
	}
	if got != nil && !reflect.DeepEqual(exp, got) {
		return []error{fmt.Errorf("fields.json: expected %s, read %s", describeJSON(exp), describeJSON(got))}
	}

	return nil
}

// describeJSON returns the JSON of v, or its Go form.
func describeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}

	return string(b)
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

// checkRepair compares e.Repair with the repair of out, whose new coverage entries must name the pack's capture.
func (e *Expectation) checkRepair(out *readOutputs, captureID string) []error {
	switch {
	case e.Repair == nil && out.repair == nil:
		return nil
	case e.Repair == nil || out.repair == nil:
		return []error{fmt.Errorf("repair: expected %+v, read %+v", e.Repair, out.repair)}
	}

	r := out.repair
	got, err := repairRead(r.Result, r.Blocks, r.Records, r.CoverageAdded, captureID)
	if err != nil {
		return []error{fmt.Errorf("repair: %w", err)}
	}
	if exp := e.Repair.normal(); !reflect.DeepEqual(exp, got) {
		return []error{fmt.Errorf("repair: expected %+v, read %+v", exp, got)}
	}

	return nil
}

// repairRead returns the RepairWant of a repair result: its result, blocks, records and new coverage, each zero when absent.
// It fails for a new coverage entry of a capture other than captureID.
func repairRead(result string, blocks *int, records *U64, coverage *[]Coverage, captureID string) (RepairWant, error) {
	got := RepairWant{Result: result}
	if blocks != nil {
		got.Blocks = *blocks
	}
	if records != nil {
		got.Records = uint64(*records)
	}
	if coverage == nil {
		return got, nil
	}

	got.Coverage = []CoverageWant{}
	for _, c := range *coverage {
		if c.CaptureID == nil || *c.CaptureID != captureID {
			return RepairWant{}, fmt.Errorf("a new coverage entry of another capture_id than the pack's %s", captureID)
		}
		var w CoverageWant
		if c.SeqFirst != nil {
			w.First = uint64(*c.SeqFirst)
		}
		if c.SeqLast != nil {
			w.Last = new(uint64(*c.SeqLast))
		}
		if c.TimeStart != nil {
			w.TimeStart = int64(*c.TimeStart)
		}
		if c.TimeEnd != nil {
			w.TimeEnd = int64(*c.TimeEnd)
		}
		got.Coverage = append(got.Coverage, w)
	}

	return got, nil
}

// normal returns a copy of w whose coverage is empty rather than nil when w is patched, as a patched repair states it.
func (w *RepairWant) normal() RepairWant {
	out := *w
	if out.Result == RepairPatched {
		out.Coverage = nonNil(out.Coverage)
	}

	return out
}

// checkCuts compares e.Cuts with the rows of truncation.json t, row by row,
// read from the pack of layout l and capture captureID;
// a vector without a table expects no rows.
func (e *Expectation) checkCuts(t *Truncation, l *Layout, captureID string) []error {
	if t == nil {
		if len(e.Cuts) > 0 {
			return []error{errors.New("truncation.json: rows expected, none read")}
		}

		return nil
	}
	if len(e.Cuts) != len(t.Rows) {
		return []error{fmt.Errorf("truncation.json: %d rows expected, %d read", len(e.Cuts), len(t.Rows))}
	}

	var errs []error
	at := func(name string, p Pos) *U64 {
		if p.kind == posNone {
			return nil
		}
		off, err := p.offset(l)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return nil
		}

		return new(U64(off))
	}
	for i, c := range e.Cuts {
		name := fmt.Sprintf("truncation.json row %d", i)
		to := U64(uint64(t.Size) - 1)
		if i+1 < len(e.Cuts) {
			if next := at(name+" end", e.Cuts[i+1].From); next != nil {
				to = *next - 1
			}
		}
		exp := TruncationRow{To: to, Cut: Cut{Rejection: c.Rejection}}
		if from := at(name+" from", c.From); from != nil {
			exp.From = *from
		}
		if c.Rejection == "" {
			exp.Outcome = c.Outcome.String()
			exp.ExportedRecords = new(U64(c.Records))
			exp.Verify = &CutVerify{BlocksValidated: c.Validated, Records: U64(c.Records), WalkStop: at(name+" walk_stop", c.WalkStop)}
			if end := at(name+" prefix_end", c.PrefixEnd); end != nil {
				exp.Verify.PrefixEnd = *end
			}
		}
		got := t.Rows[i]
		// The repair parts are compared apart, as RepairWant values.
		gotRepair := got.Repair
		got.Repair = nil
		if !reflect.DeepEqual(exp, got) {
			errs = append(errs, fmt.Errorf("%s: expected %s, read %s", name, describeRow(&exp), describeRow(&got)))
		}
		if err := c.checkRepair(gotRepair, captureID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	return errs
}

// checkRepair compares the repair part of c with got, the repair part of its row, read from a pack of capture captureID.
func (c *CutWant) checkRepair(got *CutRepair, captureID string) error {
	switch {
	case c.Repair == nil && got == nil:
		return nil
	case c.Repair == nil || got == nil:
		return fmt.Errorf("expected repair %+v, read %+v", c.Repair, got)
	}

	r, err := repairRead(got.Result, got.Blocks, nil, got.CoverageAdded, captureID)
	if err != nil {
		return fmt.Errorf("repair: %w", err)
	}
	if w := c.Repair.normal(); !reflect.DeepEqual(w, r) {
		return fmt.Errorf("expected repair %+v, read %+v", w, r)
	}

	return nil
}

// describeRow returns the JSON of a truncation.json row, or its Go form.
func describeRow(r *TruncationRow) string {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Sprintf("%+v", *r)
	}

	return string(b)
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

// checkMulti refuses the expectation of a multi-pack vector that does not fit its class:
// a field other than Packs, Reads and Lookups, no pack, or neither a read nor a lookup,
// or a pack's expectation without an outcome and a prefix end or with a file that a multi-pack vector does not have.
func (e *Expectation) checkMulti() error {
	rest := *e
	rest.Packs, rest.Reads, rest.Lookups = nil, nil, nil
	if !reflect.DeepEqual(rest, Expectation{}) {
		return errors.New("a multi-pack vector expects its packs, reads and lookups and nothing else")
	}
	if len(e.Packs) == 0 || len(e.Reads) == 0 && len(e.Lookups) == 0 {
		return errors.New("a multi-pack vector expects its packs and its reads or lookups")
	}
	for n, p := range e.Packs {
		switch {
		case p == nil || p.Rejection != "" || p.Outcome == 0 || p.PrefixEnd.kind == posNone:
			return fmt.Errorf("pack %d: a pack of a multi-pack vector expects an outcome and a prefix end", n)
		case p.Outcome == tracepack.OutcomeUnfinalized && !p.Unfinalized:
			return fmt.Errorf("pack %d: an unfinalized outcome of a pack expected finalized", n)
		case p.Packs != nil || p.Reads != nil || p.Lookups != nil || p.Queries != nil || p.Fields != nil || p.Footer != nil || p.Repair != nil || p.Cuts != nil:
			return fmt.Errorf("pack %d expects a file a multi-pack vector does not have", n)
		}
	}

	return nil
}

// checkMultiPack compares e, the expectation of the multi-pack vector of seed, with the reads out of its packs:
// each pack's expectation with the reads of that pack, each read's, by id, with the read's result,
// and each lookup's, by id, with the lookup's result.
func (e *Expectation) checkMultiPack(out *readOutputs, packs [][]byte, seed string) error {
	if len(e.Packs) != len(out.packs) || len(out.packs) != len(packs) {
		return fmt.Errorf("%d packs expected, %d read", len(e.Packs), len(out.packs))
	}
	var errs []error
	layouts := make([]Layout, len(packs))
	for n, p := range e.Packs {
		if err := p.check(out.packs[n], packs[n]); err != nil {
			errs = append(errs, fmt.Errorf("pack %d: %w", n, err))
		}
		l, err := Locate(packs[n])
		if err != nil {
			return fmt.Errorf("pack %d: the pack's layout: %w", n, err)
		}
		layouts[n] = l
	}
	if len(e.Reads) != len(out.reads) {
		return errors.Join(append(errs, fmt.Errorf("reads: %d expected, %d read", len(e.Reads), len(out.reads)))...)
	}
	for i := range out.reads {
		errs = append(errs, e.Reads[i].check(&out.reads[i], layouts, seed)...)
	}
	if len(e.Lookups) != len(out.lookups) {
		return errors.Join(append(errs, fmt.Errorf("lookups: %d expected, %d looked up", len(e.Lookups), len(out.lookups)))...)
	}
	for i := range out.lookups {
		errs = append(errs, e.Lookups[i].check(&out.lookups[i], layouts, seed)...)
	}

	return errors.Join(errs...)
}

// check compares w with got, the lookup of lookups.json of its id, whose packs have the layouts layouts, by number,
// and whose captures are those of the vector of seed:
// the error form alone, or every value of the result, each array in its order.
func (w *LookupWant) check(got *LookupVector, layouts []Layout, seed string) []error {
	if w.ID != got.ID {
		return []error{fmt.Errorf("lookup: expected id %s, looked up %s", w.ID, got.ID)}
	}
	exp, err := w.expect(layouts, seed)
	if err != nil {
		return []error{fmt.Errorf("lookup %s: %w", got.ID, err)}
	}
	g := got.Expect
	var errs []error
	want := func(name string, exp, read any) {
		if !reflect.DeepEqual(exp, read) {
			errs = append(errs, fmt.Errorf("lookup %s: %s: expected %s, looked up %s", got.ID, name, describeJSON(exp), describeJSON(read)))
		}
	}
	want("error", exp.Error, g.Error)
	want("outcome", exp.Outcome, g.Outcome)
	want("key", exp.Key, g.Key)
	want("window_end", exp.WindowEnd, g.WindowEnd)
	want("records", exp.Records, g.Records)
	want("gaps", exp.Gaps, g.Gaps)
	want("searched", exp.Searched, g.Searched)
	want("conflicts", exp.Conflicts, g.Conflicts)
	want("footer_errors", exp.FooterErrors, g.FooterErrors)

	return errs
}

// expect returns w in the form of lookups.json, its packs of the layouts layouts, by number, and its captures those of the vector of seed:
// the error form alone, or the result with each array non-nil.
//
// Returns:
//   - LookupExpect: the expectation.
//   - error: a fact whose position names no pack of layouts, or no offset in it.
func (w *LookupWant) expect(layouts []Layout, seed string) (LookupExpect, error) {
	if w.Error != "" {
		return LookupExpect{Error: w.Error}, nil
	}
	e := LookupExpect{Outcome: w.Outcome, Key: w.Key}
	if w.WindowEnd != nil {
		e.WindowEnd = new(U64(*w.WindowEnd))
	}
	records := make([]LookupRecord, 0, len(w.Records))
	for _, r := range w.Records {
		r.Roles = nonNil(r.Roles)
		records = append(records, r)
	}
	searched := make([]SearchedScope, 0, len(w.Searched))
	for _, sc := range w.Searched {
		sc.Packs = nonNil(sc.Packs)
		searched = append(searched, sc)
	}
	footer := nonNil(w.FooterErrors)
	gaps := make([]Fact, 0, len(w.Gaps))
	for _, f := range w.Gaps {
		fact := f.Fact
		if f.Capture != nil {
			id := captureOf(seed, *f.Capture).String()
			if fact.Boundary != nil {
				b := *fact.Boundary
				b.CaptureID = id
				fact.Boundary = &b
			}
			if fact.Coverage != nil {
				c := *fact.Coverage
				c.CaptureID = &id
				fact.Coverage = &c
			}
		}
		if f.At.kind != posNone {
			if fact.Pack == nil || *fact.Pack < 0 || *fact.Pack >= len(layouts) {
				return LookupExpect{}, fmt.Errorf("a %s fact with a position names no pack of the vector", fact.Reason)
			}
			off, err := f.At.offset(&layouts[*fact.Pack])
			if err != nil {
				return LookupExpect{}, fmt.Errorf("a %s fact: %w", fact.Reason, err)
			}
			fact.Offset = new(U64(off))
		}
		gaps = append(gaps, fact)
	}
	conflicts := make([]ConflictEntry, 0, len(w.Conflicts))
	for _, c := range w.Conflicts {
		conflicts = append(conflicts, ConflictEntry{CaptureID: captureOf(seed, c.Capture).String(), Seq: U64(c.Seq), Versions: nonNil(c.Versions)})
	}
	e.Records, e.Gaps, e.Searched, e.Conflicts, e.FooterErrors = &records, &gaps, &searched, &conflicts, &footer

	return e, nil
}

// check compares w with got, the read of reads.json of its id, whose packs have the layouts layouts, by number,
// and whose captures are those of the vector of seed.
func (w *ReadWant) check(got *ReadVector, layouts []Layout, seed string) []error {
	if w.ID != got.ID {
		return []error{fmt.Errorf("read: expected id %s, read %s", w.ID, got.ID)}
	}
	var errs []error
	want := func(name string, exp, read any) {
		if !reflect.DeepEqual(exp, read) {
			errs = append(errs, fmt.Errorf("read %s: %s: expected %s, read %s", got.ID, name, describeJSON(exp), describeJSON(read)))
		}
	}

	g := &got.Expect
	want("error", w.Error, g.Error)
	conflicts := make([]ConflictEntry, 0, len(w.Conflicts))
	for _, c := range w.Conflicts {
		conflicts = append(conflicts, ConflictEntry{CaptureID: captureOf(seed, c.Capture).String(), Seq: U64(c.Seq), Versions: nonNil(c.Versions)})
	}
	want("conflicts", conflicts, nonNil(g.Conflicts))
	if w.Error != "" || g.Error != "" {
		if w.Items != nil || w.Incomplete != nil || w.FooterErrors != nil {
			errs = append(errs, fmt.Errorf("read %s: the error form expects its conflicts alone", got.ID))
		}

		return errs
	}

	items := make([]ReadItem, 0, len(w.Items))
	for _, it := range w.Items {
		items = append(items, ReadItem{Seq: U64(it.Seq), Pack: it.Pack, Block: it.Block, Conflict: it.Conflict})
	}
	want("items", items, *g.Items)
	want("footer_errors", nonNil(w.FooterErrors), *g.FooterErrors)

	return append(errs, w.checkIncomplete(got.ID, *g.Incomplete, layouts)...)
}

// checkIncomplete compares the incomplete reasons of w with got, those of the read of id,
// each position in the pack of its reason, of the layout of that pack in layouts.
func (w *ReadWant) checkIncomplete(id string, got []PackIncomplete, layouts []Layout) []error {
	if len(w.Incomplete) != len(got) {
		return []error{fmt.Errorf("read %s: %d incomplete reasons expected, read %s", id, len(w.Incomplete), describeJSON(got))}
	}

	var errs []error
	for k, in := range got {
		iw := w.Incomplete[k]
		block := -1
		if in.Block != nil {
			block = *in.Block
		}
		if iw.Pack != in.Pack || iw.Reason.String() != in.Reason || iw.Block != block {
			errs = append(errs, fmt.Errorf("read %s: incomplete %d: expected %s of pack %d, block %d, read %s of pack %d, block %d",
				id, k, iw.Reason, iw.Pack, iw.Block, in.Reason, in.Pack, block))

			continue
		}
		if iw.Pack < 0 || iw.Pack >= len(layouts) {
			errs = append(errs, fmt.Errorf("read %s: incomplete %d names pack %d of %d", id, k, iw.Pack, len(layouts)))

			continue
		}
		var exp *U64
		if iw.At.kind != posNone {
			off, err := iw.At.offset(&layouts[iw.Pack])
			if err != nil {
				errs = append(errs, fmt.Errorf("read %s: incomplete %d: %w", id, k, err))

				continue
			}
			exp = new(U64(off))
		}
		if !reflect.DeepEqual(exp, in.Offset) {
			errs = append(errs, fmt.Errorf("read %s: incomplete %d: expected offset %v, read %v", id, k, exp, in.Offset))
		}
	}

	return errs
}
