package tracepack

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrPublishUncertain reports a segment that a sink made visible under its key
// but could not make durable: a Commit that failed after the segment was published.
// The segment may not survive a system crash.
// A SegmentFile's Commit returns an error wrapping it beside the cause,
// and a SegmentWriter keeps it in the error it returns,
// so a recorder can tell a published segment from one that was never published.
var ErrPublishUncertain = errors.New("tracepack: segment published, durability uncertain")

// SegmentSink stores the segments a SegmentWriter writes (the tracepack storage specification §3 and §4).
type SegmentSink interface {
	// Create starts a new segment described by s.
	// Nothing of it may be visible under its key before Commit.
	Create(ctx context.Context, s SegmentInfo) (SegmentFile, error)
}

// SegmentFile is one segment a SegmentSink is writing.
//
// A SegmentWriter writes the segment's bytes in file order, calls Sync after every block and after the trailer,
// then Commit once the segment is finalized, or Abort after any failure.
// It calls Commit at most once and Abort at most once, and Abort exactly once after a failed Commit.
// Write, Sync and Abort take no context: the sink bounds each by its own deadline, as an Object bounds ReadAt.
type SegmentFile interface {
	io.Writer
	// Sync makes the bytes written so far durable.
	Sync() error
	// Commit publishes the finalized segment under its key.
	// A Commit that fails before the segment became visible publishes nothing;
	// one that fails after it became visible, but before it is durable, returns an error wrapping ErrPublishUncertain.
	Commit(ctx context.Context) error
	// Abort discards a segment that was not committed;
	// after a Commit that failed once the segment became visible it leaves the segment published.
	Abort() error
}

// SegmentInfo describes a segment when a SegmentWriter opens it:
// what its key holds (the tracepack storage specification §3), its hour and its period,
// so a sink derives the key from the segment it writes and never from a tool of its own.
type SegmentInfo struct {
	// ToolID is the capture's tool_id.
	ToolID string
	// CaptureID is the capture's capture_id.
	CaptureID UUID
	// PackID is the segment's pack_id.
	PackID UUID
	// SeqFirst is the segment's seq_start, the seq of its first record.
	SeqFirst uint64
	// Hour is the UTC hour of the segment's period, as HourOf numbers it.
	Hour int64
	// PeriodStart and PeriodEnd are the segment's period, end exclusive, in UTC.
	PeriodStart, PeriodEnd time.Time
}
