package tracepack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writingDecision is what a caller of a writing call does with its error, the first match deciding:
// a later call on a writer that already reported its end first, then the cause of the call that failed.
type writingDecision string

const (
	// decideClosed: the writer was closed; stop using it, its output complete and kept.
	decideClosed writingDecision = "closed"
	// decideFailed: the writer failed earlier; stop using it, the output's disposition decided at that failure.
	decideFailed writingDecision = "failed"
	// decideUncertain: the segment is visible but its durability unknown; keep it.
	decideUncertain writingDecision = "publish uncertain"
	// decideExists: identical bytes are stored at the key; the segment counts as committed.
	decideExists writingDecision = "segment exists"
	// decideConflict: other bytes are stored at the key; alert, do not retry.
	decideConflict writingDecision = "segment conflict"
	// decideCtx: the call's ctx is done; stop.
	decideCtx writingDecision = "ctx"
	// decideRefused: the input determines the error; park or fix it.
	decideRefused writingDecision = "refused"
	// decideTransient: an error of the destination, Syncer, sink or segment file; it may be transient.
	decideTransient writingDecision = "transient"
)

// decideWriting returns the decision a caller of a writing call takes for err, in the documented order.
func decideWriting(err error) writingDecision {
	switch {
	case errors.Is(err, ErrClosed):
		return decideClosed
	case errors.Is(err, ErrWriterFailed):
		return decideFailed
	case errors.Is(err, ErrPublishUncertain):
		return decideUncertain
	case errors.Is(err, ErrSegmentExists):
		return decideExists
	case errors.Is(err, ErrSegmentConflict):
		return decideConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return decideCtx
	case errors.Is(err, ErrRefused):
		return decideRefused
	default:
		return decideTransient
	}
}

// TestWritingDecisionClosed closes a Writer and a SegmentWriter successfully:
// every later call returns ErrClosed, unmarked, and the output is complete and kept.
func TestWritingDecisionClosed(t *testing.T) {
	t.Parallel()

	t.Run("Writer", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		w := refusalWriter(t, &out, nil)
		r := testDataRecord(0, blockTestHour+1, 1)
		require.NoError(t, w.Append(&r))
		_, err := w.Close()
		require.NoError(t, err)
		pack := bytes.Clone(out.Bytes())

		r = testDataRecord(1, blockTestHour+2, 1)
		_, err = w.Close()
		later := []error{w.Append(&r), w.Flush(), err}
		for _, err := range later {
			assert.Equal(t, decideClosed, decideWriting(err))
			assert.NotErrorIs(t, err, ErrRefused)
		}
		assert.Equal(t, pack, out.Bytes(), "nothing written after Close")
		assert.True(t, mustOpen(t, pack, ReaderOptions{}).Header().Finalized, "the pack is kept")
	})
	t.Run("SegmentWriter", func(t *testing.T) {
		t.Parallel()

		w, sink := newSeg(t, nil)
		segFrame(t, w, time.Second)
		require.NoError(t, w.Close(t.Context()))
		calls := sink.calls

		ev := TransportEvent{Event: EventStateTransition}
		r := segMonoRecord(2*time.Second, 2*time.Second)
		later := map[string]error{
			"AppendFrame": w.AppendFrame(t.Context(), segAt(2*time.Second), DirHostToEquipment, 1, blockTestFrame, 0),
			"AppendEvent": w.AppendEvent(t.Context(), segAt(2*time.Second), 1, &ev),
			"Append":      w.Append(t.Context(), &r),
			"Tick":        w.Tick(t.Context(), segAt(time.Hour)),
			"Rotate":      w.Rotate(t.Context()),
			"Close":       w.Close(t.Context()),
		}
		for call, err := range later {
			assert.Equal(t, decideClosed, decideWriting(err), call)
			assert.NotErrorIs(t, err, ErrRefused, call)
		}
		assert.Equal(t, calls, sink.calls, "no sink call after Close")
		require.Len(t, sink.keys, 1)
		assert.True(t, sink.segments(t)[0].Header().Finalized, "the committed segment is kept")
		sink.noMisuse(t)
	})
}

// TestWritingDecisionFailure fails a SegmentWriter in the ways the decision table tells apart,
// each after a committed segment, and requires the failing call's decision,
// the later call's ErrWriterFailed, checked before the cause it wraps,
// and the committed segment still published.
func TestWritingDecisionFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// fail makes the call that fails, ctx its context and cancel the function that cancels it.
		fail func(t *testing.T, ctx context.Context, cancel func(), w *SegmentWriter, sink *memSink) error
		want writingDecision
		// published is the segments published once the call failed.
		published int
	}{
		{"a sink failure", func(_ *testing.T, ctx context.Context, _ func(), w *SegmentWriter, sink *memSink) error {
			sink.fault = failAt(sinkWrite, sink.calls[sinkWrite])
			return w.Rotate(ctx) // the open block's write fails
		}, decideTransient, 1},
		{"a cancellation after publication", func(_ *testing.T, ctx context.Context, cancel func(), w *SegmentWriter, sink *memSink) error {
			sink.fault = cancelAt(sinkPublished, cancel)
			return w.Close(ctx)
		}, decideUncertain, 2},
		{"a cancellation before publication", func(_ *testing.T, ctx context.Context, cancel func(), w *SegmentWriter, sink *memSink) error {
			sink.fault = cancelAt(sinkCommit, cancel)
			return w.Close(ctx)
		}, decideCtx, 1},
		{"a refusal that fails the writer", func(_ *testing.T, ctx context.Context, _ func(), w *SegmentWriter, _ *memSink) error {
			w.seg.w.encodedHook = truncateBlock
			return w.Close(ctx)
		}, decideRefused, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink := newSeg(t, nil)
			segFrame(t, w, time.Second)
			require.NoError(t, w.Rotate(t.Context()), "the first segment committed")
			segFrame(t, w, 2*time.Second)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := tt.fail(t, ctx, cancel, w, sink)
			require.Error(t, err)
			assert.Equal(t, tt.want, decideWriting(err))
			assert.Len(t, sink.keys, tt.published, "segments published")
			sink.noMisuse(t)

			calls := sink.calls
			r := segMonoRecord(3*time.Second, 3*time.Second)
			later := w.Append(t.Context(), &r)
			assert.Equal(t, decideFailed, decideWriting(later))
			assert.Equal(t, decideFailed, decideWriting(w.Close(t.Context())))
			if tt.want == decideUncertain {
				assert.ErrorIs(t, later, ErrPublishUncertain, "the cause stays in the chain, so ErrWriterFailed is checked first")
			}
			requireFailedClose(t, w, err)
			assert.Equal(t, calls, sink.calls, "no sink call after the failure")
			assert.True(t, sink.segments(t)[0].Header().Finalized, "the segment committed before the failure stays published")
		})
	}
}

// TestWritingDecisionRefusal refuses a record, which leaves the SegmentWriter usable:
// the refusal is marked, and a later call succeeds.
func TestWritingDecisionRefusal(t *testing.T) {
	t.Parallel()

	w, sink := newSeg(t, nil)
	err := w.AppendEvent(t.Context(), segAt(time.Second), 1, nil)
	assert.Equal(t, decideRefused, decideWriting(err))
	require.ErrorIs(t, err, ErrInvalidRecord)
	segFrame(t, w, 2*time.Second)
	require.NoError(t, w.Close(t.Context()))
	sink.noMisuse(t)
}

// TestWritingDecisionSegmentKey commits a segment through a DirSink that never replaces a file at a key,
// whose key already holds the segment's own bytes or other bytes, as a rerun of a reproducible conversion finds it:
// Rotate or Close returns an error wrapping ErrSegmentExists or ErrSegmentConflict, matchable through the SegmentWriter,
// neither marked ErrRefused nor taken for an invalid argument or an uncertain publication;
// the SegmentWriter failed, and a later call's ErrWriterFailed still carries the sentinel.
func TestWritingDecisionSegmentKey(t *testing.T) {
	t.Parallel()

	for _, same := range []bool{true, false} {
		want, sentinel := decideConflict, ErrSegmentConflict
		if same {
			want, sentinel = decideExists, ErrSegmentExists
		}
		for _, call := range []string{"Rotate", "Close"} {
			t.Run(string(want)+" at "+call, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				s, err := NewDirSink(root, "", DirSinkOptions{NoReplace: true})
				require.NoError(t, err)
				t.Cleanup(func() { _ = s.Close() })
				sink := &presetSink{SegmentSink: s, root: root, same: same}
				opts := segOptions(sink)
				opts.Capture.EquipmentID = dirTestEquipment
				w, err := NewSegmentWriter(t.Context(), opts)
				require.NoError(t, err)
				segFrame(t, w, time.Second)

				if call == "Rotate" {
					err = w.Rotate(t.Context())
				} else {
					err = w.Close(t.Context())
				}
				assert.Equal(t, want, decideWriting(err))
				require.ErrorIs(t, err, sentinel)
				requireNotRefused(t, err)
				require.NotErrorIs(t, err, ErrInvalidArgument)
				require.NotErrorIs(t, err, ErrPublishUncertain)
				keys, partial := dirFiles(t, root)
				assert.Len(t, keys, 1, "the existing file stays at the key")
				assert.Empty(t, partial, "the temporary file removed")

				assert.Equal(t, decideFailed, decideWriting(w.Close(t.Context())))
				// The cause stays in the chain, so ErrWriterFailed is checked first.
				requireFailedClose(t, w, err, sentinel)
			})
		}
	}
}

// presetSink is a SegmentSink that, at each segment's Commit, first stores at the segment's key under root
// the bytes written to it, or those bytes with one more, as another writer that published there would have.
type presetSink struct {
	SegmentSink

	root string
	same bool
}

// presetFile is a segment of a presetSink, keeping the bytes written to it.
type presetFile struct {
	SegmentFile

	sink *presetSink
	path string
	buf  bytes.Buffer
}

// Create creates the segment on the sink it wraps.
func (s *presetSink) Create(ctx context.Context, info SegmentInfo) (SegmentFile, error) {
	key, err := SegmentKey("", info.EquipmentID, info.CaptureID, info.SeqFirst, info.PackID)
	if err != nil {
		return nil, err
	}
	f, err := s.SegmentSink.Create(ctx, info)
	if err != nil {
		return nil, err
	}

	return &presetFile{SegmentFile: f, sink: s, path: filepath.Join(s.root, filepath.FromSlash(key))}, nil
}

// Write keeps p and writes it to the segment.
func (f *presetFile) Write(p []byte) (int, error) {
	f.buf.Write(p)
	return f.SegmentFile.Write(p)
}

// Commit stores the preset bytes at the key, then commits the segment.
func (f *presetFile) Commit(ctx context.Context) error {
	data := f.buf.Bytes()
	if !f.sink.same {
		data = append(bytes.Clone(data), 0)
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(f.path, data, 0o600); err != nil {
		return err
	}

	return f.SegmentFile.Commit(ctx)
}
