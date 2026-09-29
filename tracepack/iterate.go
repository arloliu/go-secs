package tracepack

import (
	"context"
	"fmt"
	"slices"
)

// iterPlan is what a query decides once, before Iterate reads any block.
type iterPlan struct {
	f *Filter
	// full reads every block in full; otherwise header-only.
	full bool
	// level is the ReadLevel of every yielded record.
	level ReadLevel
	// payloadCopies evaluates the copy-field predicates on the payload's values instead of the stored copies:
	// f tests a copy field in an authoritative query over a pack that is not attested.
	// It implies full.
	payloadCopies bool
	// headerValidated lists every block read in Result.HeaderValidated.
	headerValidated bool
}

// anyCounted reports whether values is empty or counts holds a count other than 0 for one of values,
// counts being indexed by value and a missing element counting 0.
func anyCounted[T ~uint8](counts []uint32, values []T) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if int(v) < len(counts) && counts[v] != 0 {
			return true
		}
	}

	return false
}

// cloneCoverage returns a copy of c that shares nothing with it.
func cloneCoverage(c *Coverage) *Coverage {
	return &Coverage{
		CaptureID: clonePtr(c.CaptureID),
		SeqFirst:  clonePtr(c.SeqFirst),
		SeqLast:   clonePtr(c.SeqLast),
		TimeStart: clonePtr(c.TimeStart),
		TimeEnd:   clonePtr(c.TimeEnd),
	}
}

// clonePtr returns a pointer to a copy of *v, or nil when v is nil.
func clonePtr[T any](v *T) *T {
	if v == nil {
		return nil
	}

	return new(*v)
}

// Iterate yields the records of the pack that q selects, in file order,
// and returns the status of the read (the tracepack semantics specification §7.4).
//
// The Result starts with the defects every read of the pack carries, those Open found, and PackHeader.FooterErr.
// Next come the coverage entries of the pack metadata that intersect the query (the tracepack format specification §5):
// an entry of the pack's capture, an absent capture_id included, whose inclusive time interval meets [TimeFrom, TimeTo),
// an absent bound being unbounded, or whose first bound, of time or seq, exceeds its last;
// the Filter has no seq range, so every entry's seq range intersects it.
// Each such Defect carries its own copy of the entry.
// Then Iterate visits the blocks in file order, checking ctx before each.
// It skips a block the footer excludes: an indexed block whose F-2 time range misses the query's,
// whose F-2 epoch range holds none of Epochs, or whose F-3 kind or direction counts are 0 for every one of Kinds or Dirs.
// These fields are not copies, so skipping never makes the result rely on header copies;
// it trusts the validated footer, whose F-2 ranges only a read of the block compares with its records.
// A walked block is never skipped.
//
// Iterate reads a block in full when q.Payloads is set,
// or when an authoritative query with a copy-field predicate meets a pack that is not attested;
// otherwise it reads the block header-only, at ReadAttested for an attested pack and ReadHeaderOnly otherwise.
// A block that fails a check of its read level, or exceeds ReaderOptions.MaxBlockLen, adds a Defect and yields nothing,
// and iteration continues with the next block.
// A provisional query with a copy-field predicate over a pack that is not attested
// lists every block it read in Result.HeaderValidated, also when the block yields no record.
//
// A record is selected when it satisfies every predicate of q.Filter:
// the time range, Kinds, Dirs and Epochs on its stored header;
// the copy-field predicates on the payload's values (Record.SetHeaderCopies) for an authoritative query over a pack that is not attested,
// and on the stored copies otherwise.
// fn receives each selected record as an Item that is valid only during the call;
// Iterate reuses the Item and its buffers.
//
// Parameters:
//   - ctx: cancels the iteration between blocks.
//   - q: the query; see Query.
//   - fn: receives each selected record; a non-nil error stops the iteration.
//
// Returns:
//   - Result: the status of the read; complete when Incomplete is empty.
//     On an error it holds what was found before the error; Result{} for an invalid query.
//   - error: an error wrapping ErrInvalidQuery, before anything is read;
//     ctx's error, wrapped; a ReadAt error, wrapping io.ErrUnexpectedEOF for a short read;
//     or the error fn returned, as is.
func (r *Reader) Iterate(ctx context.Context, q Query, fn func(*Item) error) (Result, error) {
	if err := q.validate(); err != nil {
		return Result{}, err
	}

	p := r.plan(&q)
	res := Result{Incomplete: slices.Clone(r.openDefects), FooterErr: r.footerErr}
	res.Incomplete = r.appendCoverageDefects(res.Incomplete, p.f)

	var (
		buf  blockBuf
		item Item
	)
	for i := range r.blocks {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("tracepack: iterate: %w", err)
		}
		if r.prunes(i, p.f) {
			continue
		}

		d, def, err := r.readBlock(i, p.full, &buf)
		if err != nil {
			return res, err
		}
		if def != nil {
			res.Incomplete = append(res.Incomplete, *def)
			continue
		}
		if p.headerValidated {
			res.HeaderValidated = append(res.HeaderValidated, i)
		}
		if err := p.yield(i, d, &item, fn); err != nil {
			return res, err
		}
	}

	return res, nil
}

// plan returns the read level and the evaluation of the copy-field predicates of q over r
// (the tracepack semantics specification §7.4).
func (r *Reader) plan(q *Query) iterPlan {
	f := &q.Filter
	authoritative := q.Mode == QueryAuthoritative
	unattestedCopies := !r.attested && f.hasCopyPredicate()
	p := iterPlan{
		f:               f,
		payloadCopies:   authoritative && unattestedCopies,
		headerValidated: !authoritative && unattestedCopies,
	}
	p.full = q.Payloads || p.payloadCopies

	switch {
	case p.full:
		p.level = ReadFull
	case r.attested:
		p.level = ReadAttested
	default:
		p.level = ReadHeaderOnly
	}

	return p
}

// appendCoverageDefects appends to dst a ReasonCoverage defect for every coverage entry of the pack metadata
// that intersects the query of f (the tracepack format specification §5), in the entries' order.
// Each defect carries its own copy of the entry, so a caller cannot reach the Reader's.
func (r *Reader) appendCoverageDefects(dst []Defect, f *Filter) []Defect {
	capture := UUID(r.hdr.CaptureID)
	for k := range r.meta.Coverage {
		c := &r.meta.Coverage[k]
		if c.CaptureID != nil && *c.CaptureID != capture {
			continue
		}
		// An inverted entry intersects every query of its capture.
		// The Filter has no seq range, so only the time interval can miss the query.
		if !inverted(c.SeqFirst, c.SeqLast) && !inverted(c.TimeStart, c.TimeEnd) && !overlaps(c.TimeStart, c.TimeEnd, f.TimeFrom, f.TimeTo) {
			continue
		}

		dst = append(dst, Defect{
			Reason:   ReasonCoverage,
			Block:    -1,
			Offset:   -1,
			Coverage: cloneCoverage(c),
			Err:      fmt.Errorf("tracepack: coverage entry %d of the pack metadata intersects the query: records of its range were lost", k),
		})
	}

	return dst
}

// prunes reports whether the footer excludes block i from the query of f:
// the block is indexed, and its F-2 time range misses the query's, its F-2 epoch range holds none of f.Epochs,
// or its F-3 kind_counts or dir_counts are 0 for every value of f.Kinds or f.Dirs, a missing element counting 0
// (the tracepack format specification §10).
func (r *Reader) prunes(i int, f *Filter) bool {
	info := &r.blocks[i]
	if !info.Indexed {
		return false
	}
	if !overlaps(&info.TSMin, &info.TSMax, f.TimeFrom, f.TimeTo) {
		return true
	}
	if len(f.Epochs) > 0 && !slices.ContainsFunc(f.Epochs, func(e uint32) bool { return e >= info.EpochMin && e <= info.EpochMax }) {
		return true
	}

	// An indexed block comes from the validated footer, which holds its F-3 summary.
	s := &r.footer.blocks[i]

	return !anyCounted(s.kindCounts, f.Kinds) || !anyCounted(s.dirCounts, f.Dirs)
}

// yield calls fn with every record of block i, read as d, that p's filter selects, reusing item.
//
// For a full read it computes the payload's positional copies once per record,
// for both the copy-field predicates, when p evaluates the payload, and Item.CopyMismatch.
func (p *iterPlan) yield(i int, d *decodedBlock, item *Item, fn func(*Item) error) error {
	var want Record
	for j := range d.count() {
		h := d.header(j)
		if !p.f.matchHeader(&h) {
			continue
		}

		rec := storedRecord(&h, d.payload(j))
		if p.full {
			want = rec
			want.SetHeaderCopies()
		}
		if !p.matchCopies(&rec, &want) {
			continue
		}

		*item = Item{Record: rec, HeaderExtra: h.Extra, Block: i, Level: p.level}
		if p.full {
			item.CopyMismatch = copyMismatch(&rec, &want)
		}
		if err := fn(item); err != nil {
			return err
		}
	}

	return nil
}

// matchCopies reports whether a record satisfies the copy-field predicates of p's filter:
// on want, its payload's values as Record.SetHeaderCopies computes them, when p evaluates the payload,
// else on rec, its stored copies and field_validity.
// A filter without a copy-field predicate matches every record.
func (p *iterPlan) matchCopies(rec, want *Record) bool {
	src := rec
	if p.payloadCopies {
		src = want
	}
	c := copiesOf(src)

	return p.f.matchCopies(&c)
}
