package tracepack

import (
	"context"
	"fmt"
	"slices"
)

// readerSource is the PackSource NewReaderSource returns:
// the readers it was given, grouped by hour, never changed afterwards.
type readerSource struct {
	// hours holds the readers of each UTC hour, in the order they were given.
	hours map[int64][]*Reader
}

var _ PackSource = (*readerSource)(nil)

// NewReaderSource returns a PackSource over packs the caller opened, all of one tool,
// for FindTransaction and FindTransactions (the tracepack semantics specification §7.2).
//
// It groups the readers by the UTC hour of their period, the scope of a pack that is not an extract
// (the tracepack storage specification §2), whatever their capture:
// Readers(ctx, hour) returns, in a fresh slice and in the order given, every reader whose period lies in hour,
// and none for an hour no reader's period lies in.
// A pack of any role but extract is read like any pack the lookup is given,
// a repair, the retired role 5 and a role this package does not define included.
// It keeps the readers it is given and opens or closes none:
// the caller owns them, and keeps them open while the source is used.
// The source is safe for concurrent use, as the Readers are.
//
// Parameters:
//   - readers: the packs of the lookup; none at all leaves every hour empty.
//
// Returns:
//   - PackSource: the source; nil on error.
//   - error: an error wrapping ErrInvalidQuery for a nil reader, a Reader given twice, two readers of one pack_id,
//     readers of more than one tool_id, an extract, whose records need not lie in one scope,
//     or a reader whose period is empty or not inside one UTC hour, whose scope is then unknown.
func NewReaderSource(readers []*Reader) (PackSource, error) {
	given := make(map[*Reader]int, len(readers))
	ids := make(map[UUID]int, len(readers))
	s := &readerSource{hours: make(map[int64][]*Reader)}
	tool := ""
	for i, r := range readers {
		if r == nil {
			return nil, fmt.Errorf("%w: new reader source: reader %d is nil", ErrInvalidQuery, i)
		}
		if k, ok := given[r]; ok {
			return nil, fmt.Errorf("%w: new reader source: readers %d and %d are the same Reader", ErrInvalidQuery, k, i)
		}
		given[r] = i
		id := UUID(r.hdr.PackID)
		if k, ok := ids[id]; ok {
			return nil, fmt.Errorf("%w: new reader source: readers %d and %d hold the same pack_id %s", ErrInvalidQuery, k, i, id)
		}
		ids[id] = i
		m := r.meta
		if i == 0 {
			tool = m.ToolID
		} else if m.ToolID != tool {
			return nil, fmt.Errorf("%w: new reader source: reader 0 holds tool_id %q, reader %d tool_id %q", ErrInvalidQuery, tool, i, m.ToolID)
		}
		if m.PackRole == PackRoleExtract {
			return nil, fmt.Errorf("%w: new reader source: reader %d is an extract, which has no scope", ErrInvalidQuery, i)
		}
		hour, ok := periodHour(m.PeriodStart, m.PeriodEnd)
		if !ok {
			return nil, fmt.Errorf("%w: new reader source: reader %d has the period [%d, %d), empty or not inside one UTC hour",
				ErrInvalidQuery, i, m.PeriodStart, m.PeriodEnd)
		}
		s.hours[hour] = append(s.hours[hour], r)
	}

	return s, nil
}

// Readers returns the readers of hour, in a fresh slice, in the order they were given; nil for an hour without readers.
func (s *readerSource) Readers(ctx context.Context, hour int64) ([]*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("tracepack: reader source: %w", err)
	}

	return slices.Clone(s.hours[hour]), nil
}
