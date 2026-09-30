package tracepack

import (
	"context"
	"fmt"
	"slices"
)

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
		Unknown:   cloneRawEntries(c.Unknown),
	}
}

// cloneRawEntries returns a copy of entries whose values are copies too, or nil when entries is empty.
func cloneRawEntries(entries []RawEntry) []RawEntry {
	if len(entries) == 0 {
		return nil
	}

	out := make([]RawEntry, len(entries))
	for i, e := range entries {
		out[i] = RawEntry{Tag: e.Tag, Type: e.Type, Value: slices.Clone(e.Value)}
	}

	return out
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
// It trusts the validated footer, whose F-2 ranges only a read of the block compares with its records,
// so an index that misstates a block can hide the block's records without the read reporting it (§10).
// A walked block is never skipped.
//
// Every block Iterate reads is read in full and checked against I-2 before any of its records is used
// (the tracepack format specification §6).
// A block that fails a check, or exceeds ReaderOptions.MaxBlockLen, adds a Defect and yields nothing,
// and iteration continues with the next block.
// An indexed block whose records disagree with its F-2 entry adds a ReasonIndexMismatch Defect
// and still yields its records, which passed every check of the block itself.
//
// A record is selected when it satisfies every predicate of q.Filter:
// the time range, Kinds, Dirs and Epochs on its record header,
// and SF, SessionIDs and SystemBytes on the HSMS header fields of its payload, each available as Filter describes.
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

	f := &q.Filter
	res := Result{Incomplete: slices.Clone(r.openDefects), FooterErr: r.footerErr}
	res.Incomplete = r.appendCoverageDefects(res.Incomplete, f)

	var (
		buf  blockBuf
		item Item
	)
	for i := range r.blocks {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("tracepack: iterate: %w", err)
		}
		if r.prunes(i, f) {
			continue
		}

		d, def, err := r.readBlock(i, &buf)
		if err != nil {
			return res, err
		}
		if def != nil {
			res.Incomplete = append(res.Incomplete, *def)
		}
		if d == nil {
			continue
		}
		if err := yield(&q, i, d, &item, fn); err != nil {
			return res, err
		}
	}

	return res, nil
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

// yield calls fn with every record of block i, read as d, that q's filter selects, reusing item.
// The HSMS header fields are read from the payload once per record, only when the filter tests one.
func yield(q *Query, i int, d *decodedBlock, item *Item, fn func(*Item) error) error {
	f := &q.Filter
	fields := f.hasFieldPredicate()
	for j := range d.count() {
		h := d.header(j)
		if !f.matchHeader(&h) {
			continue
		}

		rec := storedRecord(&h, d.payload(j))
		if fields {
			if h := rec.HSMSHeader(); !f.matchFields(&h) {
				continue
			}
		}
		if !q.Payloads {
			rec.Payload = nil
		}

		*item = Item{Record: rec, HeaderExtra: h.Extra, Block: i}
		if err := fn(item); err != nil {
			return err
		}
	}

	return nil
}
