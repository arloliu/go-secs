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
// The Result starts with the defects every read of the pack carries, those Open found,
// and with PackHeader.FooterErr as Result.FooterErr.
// A non-nil FooterErr is also the one entry of Result.FooterErrs, for pack 0.
// Every Item and Defect names pack 0, no Item is a conflict, and Result.Conflicts stays empty.
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
// With q.Retention set, Iterate applies the retention boundary (the tracepack storage specification §5, Retention).
// The read covers the pack's scope hour, the hour of its period_start, for every role but extract,
// so a record whose timestamp lies outside that hour still belongs to it;
// an extract's record belongs to the hour of its own timestamp:
// the read covers the hours of the indexed blocks' F-2 ts_min and ts_max,
// then, as each block is decoded and before the filter, the hours of its records,
// all compared at once with the largest boundary seen.
// Iterate asks q.Retention before it reads any block, before it passes fn the first record of each block,
// after a failed ReadAt, and before it returns success.
// The deleting component deletes an object only after the boundary passed its hour,
// so a check finding every covered hour retained proves that no read before it met an object retention deleted.
// When a covered hour is removed, Iterate ends at once with an error wrapping ErrRemoved, passing fn nothing more,
// and returns the Result found so far with Removed listing the removed hours:
// everything the read reported of those hours, records passed to fn included, is superseded, and the caller discards it.
// An error from fn and ctx's error end the read without asking q.Retention.
//
// Parameters:
//   - ctx: cancels the iteration between blocks; passed to q.Retention.
//   - q: the query; see Query.
//   - fn: receives each selected record; a non-nil error stops the iteration.
//
// Returns:
//   - Result: the status of the read; complete when Incomplete and Removed are empty.
//     On an error it holds what was found before the error; Result{} for an invalid query.
//   - error: an error wrapping ErrInvalidQuery, before anything is read;
//     ctx's error, wrapped; a ReadAt error, wrapping io.ErrUnexpectedEOF for a short read;
//     the error fn returned, as is;
//     an error wrapping ErrRemoved, joined after the ReadAt error when it follows one;
//     or the error q.Retention returned, wrapped, joined after the ReadAt error when it follows one.
func (r *Reader) Iterate(ctx context.Context, q Query, fn func(*Item) error) (Result, error) {
	if err := q.validate(); err != nil {
		return Result{}, err
	}

	f := &q.Filter
	res := Result{Incomplete: slices.Clone(r.openDefects), FooterErr: r.footerErr}
	if r.footerErr != nil {
		res.FooterErrs = []PackError{{Pack: 0, Err: r.footerErr}}
	}
	res.Incomplete = r.appendCoverageDefects(res.Incomplete, f)

	// A cancelled ctx ends the read before the retention boundary is asked.
	// Without a provider, ctx is checked only before each block, as before retention.
	if q.Retention != nil {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("tracepack: iterate: %w", err)
		}
	}
	rs := newRetentionState(q.Retention)
	if err := rs.addPack(r); err != nil {
		return rs.end(res, err)
	}
	if err := rs.check(ctx); err != nil {
		return rs.end(res, err)
	}

	extract := r.meta.PackRole == PackRoleExtract
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
			// The object may be gone because a covered hour was removed meanwhile.
			return rs.end(res, rs.recheckAfter(ctx, err))
		}
		if def != nil {
			res.Incomplete = append(res.Incomplete, *def)
		}
		if d == nil {
			continue
		}
		if extract {
			if err := rs.addBlock(d); err != nil {
				return rs.end(res, err)
			}
		}
		if err := yield(ctx, &q, &rs, i, d, &item, fn); err != nil {
			return rs.end(res, err)
		}
	}
	if err := rs.check(ctx); err != nil {
		return rs.end(res, err)
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
	if !r.blocks[i].Indexed {
		return false
	}
	// An indexed block comes from the validated footer, which holds its F-3 summary.
	b := r.indexedBlock(0, i)

	return b.excludedBy(f)
}

// yield calls fn with every record of block i, read as d, that q's filter selects, reusing item.
// The HSMS header fields are read from the payload once per record, only when the filter tests one.
// Before the first record selected, it checks the retention boundary of rs,
// so the block's records reach fn only while every hour the read covers is retained.
func yield(ctx context.Context, q *Query, rs *retentionState, i int, d *decodedBlock, item *Item, fn func(*Item) error) error {
	f := &q.Filter
	fields := f.hasFieldPredicate()
	checked := false
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

		if !checked {
			if err := rs.check(ctx); err != nil {
				return err
			}
			checked = true
		}

		*item = Item{Record: rec, HeaderExtra: h.Extra, Block: i}
		if err := fn(item); err != nil {
			return err
		}
	}

	return nil
}
