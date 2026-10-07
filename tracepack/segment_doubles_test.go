package tracepack

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// sinkOp names an operation of a memSink or of a file it created, for counting and fault injection.
type sinkOp int

const (
	sinkCreate sinkOp = iota
	sinkWrite
	sinkSync
	// sinkCommit is a Commit before the segment is published.
	sinkCommit
	// sinkPublished is a Commit once the segment is published, before it returns.
	sinkPublished
	sinkAbort
	sinkOpCount
)

// memSink is a SegmentSink holding its segments in memory, with fault injection and call counts.
//
// Create and Commit observe ctx, as a sink that renames or uploads does:
// a done ctx fails Create, and fails Commit before publication with ctx's error
// or after publication with an error wrapping ErrPublishUncertain and ctx's error.
// Create checks ctx before the fault hook runs, Commit after it, before and after publication.
// A Commit publishes the segment's bytes under its segment key (the tracepack storage specification §3), prefix "".
// An Abort after publication leaves the segment published.
// Every operation the SegmentFile contract forbids, such as a write after Commit or a second Abort, is recorded in misuse.
type memSink struct {
	// fault, when set, runs at the start of each operation, or for sinkPublished right after publication,
	// with n the number of earlier calls of op on the sink; a non-nil error fails the operation.
	// A sinkPublished error is returned wrapping ErrPublishUncertain.
	fault func(op sinkOp, n int) error
	// afterCommit, when set, runs when a Commit is about to return nil, so a test can cancel a context there.
	afterCommit func()
	// calls counts each operation, sinkPublished counting the Commits that published.
	calls [sinkOpCount]int
	// infos holds the SegmentInfo of every Create that succeeded, in order, and files the files created.
	infos []SegmentInfo
	files []*memFile
	// published maps a key to the bytes committed under it, and keys lists the keys in commit order.
	published map[string][]byte
	keys      []string
	misuse    []string
}

// memFile is one segment a memSink is writing.
type memFile struct {
	sink      *memSink
	info      SegmentInfo
	key       string
	buf       bytes.Buffer
	published bool
	committed bool
	commits   int
	aborts    int
}

var (
	_ SegmentSink = (*memSink)(nil)
	_ SegmentFile = (*memFile)(nil)
)

// newMemSink returns an empty memSink.
func newMemSink() *memSink {
	return &memSink{published: map[string][]byte{}}
}

// failAt returns a fault hook that fails the n-th call (from 0) of op with an error wrapping errInjected.
func failAt(op sinkOp, n int) func(sinkOp, int) error {
	return func(o sinkOp, i int) error {
		if o == op && i == n {
			return fmt.Errorf("%s %d: %w", o, i, errInjected)
		}

		return nil
	}
}

// String names op.
func (op sinkOp) String() string {
	return [...]string{"create", "write", "sync", "commit", "published", "abort"}[op]
}

// inject counts op and runs the fault hook for it.
func (s *memSink) inject(op sinkOp) error {
	n := s.calls[op]
	s.calls[op]++
	if s.fault == nil {
		return nil
	}

	return s.fault(op, n)
}

// Create starts a segment under the segment key info names.
func (s *memSink) Create(ctx context.Context, info SegmentInfo) (SegmentFile, error) {
	// ctx is checked before the fault hook, so a hook that cancels it lets this Create succeed.
	if err := ctx.Err(); err != nil {
		s.calls[sinkCreate]++
		return nil, err
	}
	if err := s.inject(sinkCreate); err != nil {
		return nil, err
	}
	key, err := SegmentKey("", info.ToolID, info.CaptureID, info.SeqFirst, info.PackID)
	if err != nil {
		return nil, err
	}

	f := &memFile{sink: s, info: info, key: key}
	s.infos = append(s.infos, info)
	s.files = append(s.files, f)

	return f, nil
}

// aborts returns the Abort calls on the sink's files.
func (s *memSink) aborts() int {
	return s.calls[sinkAbort]
}

// segments opens every published segment, in commit order.
func (s *memSink) segments(t testing.TB) []*Reader {
	t.Helper()

	out := make([]*Reader, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, mustOpen(t, s.published[k], ReaderOptions{}))
	}

	return out
}

// records returns the records of every published segment, payloads included, one slice per segment in commit order.
func (s *memSink) records(t testing.TB) [][]Record {
	t.Helper()

	out := make([][]Record, 0, len(s.keys))
	for _, r := range s.segments(t) {
		run := iterate(t, r, Query{Payloads: true})
		require.Empty(t, run.res.Incomplete, "a published segment reads complete")
		recs := make([]Record, 0, len(run.items))
		for _, it := range run.items {
			recs = append(recs, it.rec)
		}
		out = append(out, recs)
	}

	return out
}

// noMisuse fails the test when the SegmentWriter broke the SegmentFile contract.
func (s *memSink) noMisuse(t testing.TB) {
	t.Helper()
	require.Empty(t, s.misuse, "SegmentFile contract")
}

// misused records an operation the SegmentFile contract forbids.
func (f *memFile) misused(op string) {
	f.sink.misuse = append(f.sink.misuse, fmt.Sprintf("%s on segment %s", op, f.key))
}

// done reports whether the SegmentWriter is finished with f.
func (f *memFile) done() bool {
	return f.committed || f.aborts > 0
}

// Write appends p to the segment.
func (f *memFile) Write(p []byte) (int, error) {
	if f.done() {
		f.misused("write after commit or abort")
	}
	if err := f.sink.inject(sinkWrite); err != nil {
		return 0, err
	}

	return f.buf.Write(p)
}

// Sync does nothing but count.
func (f *memFile) Sync() error {
	if f.done() {
		f.misused("sync after commit or abort")
	}

	return f.sink.inject(sinkSync)
}

// Commit publishes the segment.
func (f *memFile) Commit(ctx context.Context) error {
	f.commits++
	if f.commits > 1 || f.aborts > 0 {
		f.misused("commit after commit or abort")
	}
	if err := f.sink.inject(sinkCommit); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s := f.sink
	s.published[f.key] = bytes.Clone(f.buf.Bytes())
	s.keys = append(s.keys, f.key)
	f.published = true
	if err := s.inject(sinkPublished); err != nil {
		return fmt.Errorf("%w: %w", ErrPublishUncertain, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrPublishUncertain, err)
	}
	f.committed = true
	if s.afterCommit != nil {
		s.afterCommit()
	}

	return nil
}

// Abort discards the segment unless it was published.
func (f *memFile) Abort() error {
	f.aborts++
	if f.aborts > 1 || f.committed {
		f.misused("abort after a successful commit or abort")
	}

	return f.sink.inject(sinkAbort)
}
