package tracepack

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// ErrRemoved reports a read that met an hour removed by retention (the tracepack storage specification §5, Retention):
// an hour the read covers lies before the first hour its Retention reports retained.
// A read that ends with it supersedes what it reported of the removed hours, records passed to its callback included;
// Result.Removed lists those hours.
// FindTransaction fails with it when the primary's hour is removed.
var ErrRemoved = errors.New("tracepack: hour removed by retention")

// Retention provides the retention boundary (the tracepack storage specification §5, Retention):
// every UTC hour before the one RetainedFrom returns is removed.
//
// It must never fall behind the component deleting removed hours:
// before that component deletes an object of an hour, every provider a reader asks must already report the hour removed.
// A value that runs ahead of the deletions is safe; a cached value is safe only if the deleting component waits out its staleness before deleting.
// The boundary never moves back; a reader keeps the largest value it has seen.
// An implementation must be safe for concurrent use and cheap, an atomic load rather than a remote call,
// since a read asks it at every block it emits.
type Retention interface {
	// RetainedFrom returns the first UTC hour still retained, numbered as HourOf numbers it;
	// every hour before it is removed.
	// An error ends the read, lookup or Observe that asked, wrapped.
	RetainedFrom(ctx context.Context) (int64, error)
}

// retentionState is what one read knows of the retention boundary:
// the largest boundary its provider returned, and the hours the read covers.
//
// A nil provider removes nothing: every method is then a no-op, and no hour is kept.
// Hours and boundaries are compared as int64 hours, never converted to nanoseconds,
// and "seen" is a flag of its own, since hour 0 and negative hours are valid.
type retentionState struct {
	provider Retention
	// boundary is the largest value the provider returned; set when seen.
	boundary int64
	seen     bool
	// covered holds the hours the read covers, each added from evidence:
	// a pack's scope hour, an extract block's F-2 or record hours.
	covered map[int64]struct{}
	// minCovered is the smallest hour of covered; set when covered is non-empty.
	minCovered int64
}

// newRetentionState returns the retention state of a read over provider, nil for none.
func newRetentionState(provider Retention) retentionState {
	return retentionState{provider: provider}
}

// sample asks the provider for the boundary and keeps the largest value seen.
//
// Returns:
//   - error: nil, or the provider's error, wrapped.
func (s *retentionState) sample(ctx context.Context) error {
	if s.provider == nil {
		return nil
	}

	from, err := s.provider.RetainedFrom(ctx)
	if err != nil {
		return fmt.Errorf("tracepack: retention boundary: %w", err)
	}
	if !s.seen || from > s.boundary {
		s.boundary, s.seen = from, true
	}

	return nil
}

// removedHour reports whether hour h lies before the largest boundary seen.
func (s *retentionState) removedHour(h int64) bool {
	return s.seen && h < s.boundary
}

// add records hour h as covered by the read, and compares a newly covered hour at once with the largest boundary seen,
// without asking the provider.
//
// Returns:
//   - error: nil, or an error wrapping ErrRemoved when h is newly covered and removed.
func (s *retentionState) add(h int64) error {
	if s.provider == nil {
		return nil
	}
	s.cover(h)

	return s.compare()
}

// addPack records the hours a read of r covers before it reads a block:
// the scope hour of its period_start for every role but extract;
// for an extract, the hours of the F-2 ts_min and ts_max of each indexed block.
// The hours of an extract's records are added as each block is decoded, by addBlock.
// Every hour is recorded before any is compared with the largest boundary seen,
// so a removal lists all of them that are removed.
//
// Returns:
//   - error: nil, or an error wrapping ErrRemoved when a covered hour is removed.
func (s *retentionState) addPack(r *Reader) error {
	if s.provider == nil {
		return nil
	}
	if r.meta.PackRole != PackRoleExtract {
		return s.add(hourOf(r.meta.PeriodStart))
	}

	for i := range r.blocks {
		if b := &r.blocks[i]; b.Indexed {
			s.cover(hourOf(b.TSMin))
			s.cover(hourOf(b.TSMax))
		}
	}

	return s.compare()
}

// addBlock records the hours of the records of d, a decoded block of an extract, before any of them is filtered:
// an extract's record belongs to the hour of its own ts_utc_ns, whatever the block's F-2 entry says.
// Every distinct hour of the block is recorded before any is compared with the largest boundary seen,
// so a removal lists all of them that are removed.
//
// Returns:
//   - error: nil, or an error wrapping ErrRemoved when a covered hour is removed.
func (s *retentionState) addBlock(d *decodedBlock) error {
	if s.provider == nil {
		return nil
	}

	var last int64
	for j := range d.count() {
		h := hourOf(d.header(j).TSUTCNs)
		// Records of one block mostly share an hour, so only a change of hour is looked up.
		if j > 0 && h == last {
			continue
		}
		last = h
		s.cover(h)
	}

	return s.compare()
}

// cover records hour h as covered by the read.
func (s *retentionState) cover(h int64) {
	if _, ok := s.covered[h]; ok {
		return
	}

	if s.covered == nil {
		s.covered = make(map[int64]struct{})
	}
	if len(s.covered) == 0 || h < s.minCovered {
		s.minCovered = h
	}
	s.covered[h] = struct{}{}
}

// compare compares every covered hour with the largest boundary seen, without asking the provider.
// A read ends at its first removal, so only an hour covered since the last comparison can be removed.
//
// Returns:
//   - error: nil, or an error wrapping ErrRemoved when a covered hour is removed.
func (s *retentionState) compare() error {
	if len(s.covered) > 0 && s.removedHour(s.minCovered) {
		return s.removedErr(s.minCovered)
	}

	return nil
}

// check asks the provider for the boundary and compares every covered hour with the largest boundary seen.
//
// Returns:
//   - error: nil; the provider's error, wrapped; or an error wrapping ErrRemoved when a covered hour is removed.
func (s *retentionState) check(ctx context.Context) error {
	if s.provider == nil {
		return nil
	}
	if err := s.sample(ctx); err != nil {
		return err
	}

	return s.compare()
}

// recheckAfter checks the boundary after a read failed with cause, an open or read I/O failure:
// a covered hour now removed may be why the object was gone.
// Callers do not call it for a ctx error or for a limit of their own, such as a refused reservation, which end a read as they are;
// Iterate and MergeIterate call it after every failed ReadAt, whatever its error wraps, ErrReadLimit included.
//
// Returns:
//   - error: cause, as is, when the check passes;
//     else cause joined with the error check returned, an error wrapping ErrRemoved or the provider's error, wrapped,
//     both reachable by errors.Is.
func (s *retentionState) recheckAfter(ctx context.Context, cause error) error {
	if err := s.check(ctx); err != nil {
		return errors.Join(cause, err)
	}

	return cause
}

// removed returns the covered hours before the largest boundary seen, ascending, or nil when there is none.
func (s *retentionState) removed() []int64 {
	if len(s.covered) == 0 || !s.removedHour(s.minCovered) {
		return nil
	}

	var out []int64
	for h := range s.covered {
		if s.removedHour(h) {
			out = append(out, h)
		}
	}
	slices.Sort(out)

	return out
}

// end returns res with Removed set, and err, for a read that ends with err.
// A read ends as soon as it finds a covered hour removed, so Removed is non-nil only beside an error wrapping ErrRemoved.
func (s *retentionState) end(res Result, err error) (Result, error) {
	res.Removed = s.removed()

	return res, err
}

// removedErr returns the error wrapping ErrRemoved for the covered hour h, removed.
func (s *retentionState) removedErr(h int64) error {
	return fmt.Errorf("tracepack: hour %d is removed, the first hour retained being %d: %w", h, s.boundary, ErrRemoved)
}
