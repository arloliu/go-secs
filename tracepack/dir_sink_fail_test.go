package tracepack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDirSeg returns a SegmentWriter of dirTestTool over a recorded dirSink in a new root, fsys.root,
// its files counted by the returned countSink,
// after appending a frame to its first segment.
func newDirSeg(t testing.TB) (*SegmentWriter, *countSink, *recordFS) {
	t.Helper()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	sink := &countSink{SegmentSink: s}
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	segFrame(t, w, time.Second)

	return w, sink, fsys
}

// failCall returns a fault hook that fails, with err, every call whose entry starts with call.
func failCall(call string, err error) func(string) error {
	return func(c string) error {
		if strings.HasPrefix(c, call) {
			return err
		}

		return nil
	}
}

// requireDirSegFailed requires that w failed with err and makes no file-system call any more.
func requireDirSegFailed(t testing.TB, w *SegmentWriter, fsys *recordFS, err error) {
	t.Helper()

	n := len(fsys.calls)
	require.ErrorIs(t, w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame), ErrWriterFailed)
	require.ErrorIs(t, w.Rotate(t.Context()), ErrWriterFailed)
	assert.Equal(t, err, w.Close(t.Context()), "Close returns the failure")
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call after the failure")
}

// requireDirSegKeys requires that root holds n committed segments, each a finalized segment, and partial temporary files.
func requireDirSegKeys(t testing.TB, root string, n, partial int) {
	t.Helper()

	keys, temps := dirFiles(t, root)
	require.Len(t, keys, n, "segments visible")
	assert.Len(t, temps, partial, "temporary files left")
	for _, key := range keys {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		require.NoError(t, err)
		assert.True(t, mustOpen(t, data, ReaderOptions{}).Header().Finalized)
	}
}

// TestDirSinkSegmentWriterFaults fails the file-system calls of the commit a frame of the next hour causes:
// before the rename the error is plain and nothing is visible,
// after it the segment is visible and the error wraps ErrPublishUncertain;
// either way the SegmentWriter aborts the segment once and fails, and an Abort error is joined after the cause.
func TestDirSinkSegmentWriterFaults(t *testing.T) {
	t.Parallel()

	removeErr := errors.New("remove failed")
	tests := []struct {
		name string
		fail func(call string) error
		// uncertain reports the error wrapping ErrPublishUncertain;
		// visible and partial are the segments visible and the temporary files left.
		uncertain        bool
		visible, partial int
		// joined reports the Abort error joined after the cause.
		joined bool
	}{
		{name: "closing the temporary file", fail: failCall("close ", errInjected)},
		{name: "creating the key's directories", fail: failCall("mkdirAll ", errInjected)},
		{name: "checking the key's directories", fail: failCall("lstat ", errInjected)},
		{name: "the rename", fail: failCall("rename ", errInjected)},
		{
			name: "the rename, then Abort's remove",
			fail: func(c string) error {
				if strings.HasPrefix(c, "remove ") {
					return removeErr
				}

				return failCall("rename ", errInjected)(c)
			},
			partial: 1, joined: true,
		},
		{name: "syncing the key's directory", fail: failCall("syncDir bucket/traces/staging/EQP%2001/", errInjected), uncertain: true, visible: 1},
		{name: "syncing root", fail: failCall("syncDir .", errInjected), uncertain: true, visible: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink, fsys := newDirSeg(t)
			root := fsys.root
			fsys.fail = tt.fail
			n := len(fsys.calls)

			err := w.AppendFrame(t.Context(), segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame)
			require.ErrorIs(t, err, errInjected)
			assert.Equal(t, tt.uncertain, errors.Is(err, ErrPublishUncertain), "ErrPublishUncertain")
			assert.Equal(t, [2]int{1, 1}, [2]int{sink.commits, sink.aborts}, "Commit and Abort calls")
			requireDirSegKeys(t, root, tt.visible, tt.partial)
			assert.Equal(t, !tt.uncertain, hasCall(fsys.calls[n:], "remove"), "Abort removes only an unpublished segment")
			if tt.joined {
				var joined interface{ Unwrap() []error }
				require.ErrorAs(t, err, &joined)
				parts := joined.Unwrap()
				require.Len(t, parts, 2)
				require.ErrorIs(t, parts[0], errInjected, "the cause first")
				require.ErrorIs(t, parts[1], removeErr, "the Abort error after it")
			}
			requireDirSegFailed(t, w, fsys, err)
		})
	}
}

// TestDirSinkSegmentWriterCancellation cancels the context of the commit a frame of the next hour causes:
// before the rename the error is ctx's alone and nothing is visible;
// after the rename the segment is visible and the error wraps ErrPublishUncertain and context.Canceled;
// after a successful Commit the SegmentWriter fails without aborting the committed segment.
func TestDirSinkSegmentWriterCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// at is the call after which ctx is cancelled.
		at        string
		uncertain bool
		visible   int
		aborts    int
	}{
		{name: "after closing the temporary file", at: "close ", aborts: 1},
		{name: "after creating the key's directories", at: "mkdirAll ", aborts: 1},
		{name: "after checking a key's directory", at: "lstat ", aborts: 1},
		{name: "after the rename", at: "rename ", uncertain: true, visible: 1, aborts: 1},
		{name: "after a directory sync", at: "syncDir bucket/traces/staging/EQP%2001/", uncertain: true, visible: 1, aborts: 1},
		{name: "after a successful Commit", at: "syncDir .", visible: 1, aborts: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, sink, fsys := newDirSeg(t)
			root := fsys.root
			ctx, cancel := context.WithCancel(t.Context())
			fsys.after = func(c string) {
				if strings.HasPrefix(c, tt.at) {
					cancel()
				}
			}
			n := len(fsys.calls)

			err := w.AppendFrame(ctx, segAt(time.Hour), DirHostToEquipment, 1, blockTestFrame)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, tt.uncertain, errors.Is(err, ErrPublishUncertain), "ErrPublishUncertain")
			assert.Equal(t, [2]int{1, tt.aborts}, [2]int{sink.commits, sink.aborts}, "Commit and Abort calls")
			requireDirSegKeys(t, root, tt.visible, 0)
			assert.Equal(t, tt.visible == 0, hasCall(fsys.calls[n:], "remove"), "Abort removes only an unpublished segment")
			assert.False(t, hasCall(fsys.calls[n:], "create"), "no segment created")
			requireDirSegFailed(t, w, fsys, err)
		})
	}
}
