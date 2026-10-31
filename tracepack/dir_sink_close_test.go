package tracepack

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirSinkModes are the two ways a DirSink publishes, each with the call that publishes a segment and its recorded sink.
var dirSinkModes = []struct {
	name string
	// publish is the operation of the call that makes a segment visible under its key.
	publish string
	sink    func(t testing.TB, root string) (*DirSink, *recordFS)
}{
	{name: "rename", publish: "rename", sink: newRecordedDirSink},
	{name: "NoReplace", publish: "link", sink: newNoReplaceDirSink},
}

// dirTestTemp returns the name of the temporary file of a segment of pack, relative to root.
func dirTestTemp(pack UUID) string {
	return partialDir + "/" + pack.String() + keyPackSuffix
}

// requireWriterPending waits until a Close is waiting for s's lock,
// so that every operation that starts afterwards waits for it;
// a writer waiting on a sync.RWMutex makes TryRLock fail.
func requireWriterPending(t *testing.T, s *DirSink) {
	t.Helper()

	require.Eventually(t, func() bool {
		if s.mu.TryRLock() {
			s.mu.RUnlock()
			return false
		}

		return true
	}, txParWatchdog, time.Millisecond, "Close waits for the sink's lock")
}

// TestDirSinkClose closes a sink twice: the first Close closes the root once, and the second does nothing.
func TestDirSinkClose(t *testing.T) {
	t.Parallel()

	s, fsys := newRecordedDirSink(t, t.TempDir())
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	assert.Equal(t, []string{"closeRoot ."}, slices.DeleteFunc(slices.Clone(fsys.calls), func(c string) bool {
		return !strings.HasPrefix(c, "closeRoot ")
	}))

	onDisk, err := NewDirSink(t.TempDir(), dirTestPrefix, DirSinkOptions{})
	require.NoError(t, err)
	require.NoError(t, onDisk.Close())
	require.NoError(t, onDisk.Close())
}

// TestDirSinkCreateAfterClose fails a Create after Close with a plain error, before any file-system call.
func TestDirSinkCreateAfterClose(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	require.NoError(t, s.Close())
	n := len(fsys.calls)
	f, err := s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
	require.EqualError(t, err, "tracepack: DirSink: Create after Close")
	assert.Nil(t, f)
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys)
	assert.Empty(t, partial)
}

// TestDirSinkCloseOpenSegment closes the sink of a segment written but not committed, in both publication modes.
// Its Commit then fails with a plain error, publishing nothing and leaving the temporary file open,
// and its Abort, after that Commit or alone, closes the temporary file and reports that it stays under .partial/.
func TestDirSinkCloseOpenSegment(t *testing.T) {
	t.Parallel()

	for _, mode := range dirSinkModes {
		for _, commit := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s commit=%v", mode.name, commit), func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				s, fsys := mode.sink(t, root)
				f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
				require.NoError(t, s.Close())
				temp := dirTestTemp(dirTestPackA)

				if commit {
					n := len(fsys.calls)
					err := f.Commit(t.Context())
					require.EqualError(t, err, "tracepack: DirSink: Commit of segment "+dirTestKey(t, 0, dirTestPackA)+" after Close")
					assert.NotErrorIs(t, err, ErrPublishUncertain)
					assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
				}
				n := len(fsys.calls)
				err := f.Abort()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "the sink is closed, so its temporary file "+temp+" stays")
				assert.Equal(t, []string{"close " + temp}, fsys.calls[n:], "the temporary file closed, not removed")
				require.NoError(t, f.Abort(), "a second Abort does nothing")

				keys, partial := dirFiles(t, root)
				assert.Empty(t, keys, "nothing published")
				assert.Equal(t, []string{temp}, partial)
			})
		}
	}
}

// TestDirSinkCloseFinishedSegments closes a sink after its segments reached their ends:
// one published, one aborted, and, without replacing, one refused because its key held the same bytes.
// Each behaves after Close as before it: Abort does nothing, and Commit returns its misuse error.
func TestDirSinkCloseFinishedSegments(t *testing.T) {
	t.Parallel()

	for _, mode := range dirSinkModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := mode.sink(t, root)
			published := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			require.NoError(t, published.Commit(t.Context()))
			aborted := writeSegment(t, s, dirTestInfo(1, dirTestPackB), "segment")
			require.NoError(t, aborted.Abort())
			finished := []SegmentFile{published, aborted}
			if mode.name == "NoReplace" {
				refused := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
				require.ErrorIs(t, refused.Commit(t.Context()), ErrSegmentExists)
				finished = append(finished, refused)
			}
			require.NoError(t, s.Close())

			n := len(fsys.calls)
			for i, f := range finished {
				require.NoError(t, f.Abort(), "segment %d", i)
				err := f.Commit(t.Context())
				require.Error(t, err, "segment %d", i)
				assert.Contains(t, err.Error(), "Commit on segment", "segment %d: the misuse error", i)
				assert.Contains(t, err.Error(), "after Commit or Abort", "segment %d: the misuse error", i)
			}
			assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{dirTestKey(t, 0, dirTestPackA)}, keys)
			assert.Empty(t, partial)
		})
	}
}

// TestDirSinkCloseWaitsForCommit pauses a Commit at a file-system call, then closes the sink beside it:
// Close waits for the Commit, which ends with the result it has without Close,
// and closes the root only after the Commit's last call.
// The pause lies before the publication, or after it and before the directory syncs,
// once with a root sync that fails, so that the Commit ends with ErrPublishUncertain; in both publication modes.
func TestDirSinkCloseWaitsForCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// afterPublish pauses after the call that publishes the segment, instead of before it.
		afterPublish bool
		// failRootSync fails the sync of root, the last directory sync.
		failRootSync bool
	}{
		{name: "before publication"},
		{name: "after publication", afterPublish: true},
		{name: "after publication, root sync fails", afterPublish: true, failRootSync: true},
	}
	for _, mode := range dirSinkModes {
		for _, tt := range tests {
			t.Run(mode.name+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				root := t.TempDir()
				s, fsys := mode.sink(t, root)
				f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
				key := dirTestKey(t, 0, dirTestPackA)

				paused, resume := make(chan struct{}), make(chan struct{})
				pause := func(call string) {
					if strings.HasPrefix(call, mode.publish+" ") {
						close(paused)
						<-resume
					}
				}
				fsys.fail = func(call string) error {
					if !tt.afterPublish {
						pause(call)
					}
					if tt.failRootSync && call == "syncDir ." {
						return errInjected
					}

					return nil
				}
				if tt.afterPublish {
					fsys.after = pause
				}

				committed, closed := make(chan error, 1), make(chan error, 1)
				go func() { committed <- f.Commit(t.Context()) }()
				recv(t, paused, "the Commit to pause")
				go func() { closed <- s.Close() }()
				requireWriterPending(t, s)
				select {
				case <-closed:
					t.Fatal("Close returned while a Commit was in progress")
				default:
				}
				close(resume)

				err := recv(t, committed, "the Commit")
				if tt.failRootSync {
					require.ErrorIs(t, err, ErrPublishUncertain)
					require.ErrorIs(t, err, errInjected)
				} else {
					require.NoError(t, err)
				}
				require.NoError(t, recv(t, closed, "the Close"))

				calls := fsys.calls
				require.Equal(t, "closeRoot .", calls[len(calls)-1], "the root closed last")
				published := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, mode.publish+" ") })
				require.GreaterOrEqual(t, published, 0)
				assert.Equal(t, dirTestSyncs(), syncCalls(calls[published:]), "every directory synced before the root closed")
				keys, partial := dirFiles(t, root)
				assert.Equal(t, []string{key}, keys)
				assert.Empty(t, partial)
				assert.Equal(t, "segment", readKey(t, root, key))
			})
		}
	}
}

// TestDirSinkCloseConcurrent runs Creates and Commits on several goroutines while another closes the sink, under -race:
// every Commit either publishes its segment, with no error, or fails before publishing anything,
// so the keys are those of the Commits that succeeded, and the temporary files those of the Commits that failed.
func TestDirSinkCloseConcurrent(t *testing.T) {
	t.Parallel()

	for _, mode := range dirSinkModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			const writers = 16
			root := t.TempDir()
			s, err := NewDirSink(root, dirTestPrefix, DirSinkOptions{NoReplace: mode.name == "NoReplace"})
			require.NoError(t, err)

			type outcome struct {
				created   bool
				commitErr error
				abortErr  error
			}
			outcomes := make([]outcome, writers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range writers {
				wg.Go(func() {
					<-start
					o := &outcomes[i]
					f, err := s.Create(t.Context(), dirTestInfo(uint64(i), UUID{0xD0, byte(i)}))
					if err != nil {
						return
					}
					o.created = true
					if _, err := f.Write([]byte("segment")); err != nil {
						o.commitErr = err
						return
					}
					o.commitErr = f.Commit(t.Context())
					if o.commitErr != nil {
						o.abortErr = f.Abort()
					}
				})
			}
			wg.Go(func() {
				<-start
				assert.NoError(t, s.Close())
			})
			close(start)
			wg.Wait()

			keys, partial := dirFiles(t, root)
			var wantKeys, wantPartial []string
			for i, o := range outcomes {
				pack := UUID{0xD0, byte(i)}
				switch {
				case !o.created:
				case o.commitErr == nil:
					wantKeys = append(wantKeys, dirTestKey(t, uint64(i), pack))
				default:
					assert.NotErrorIs(t, o.commitErr, ErrPublishUncertain, "writer %d: nothing published by a failed Commit", i)
					assert.Error(t, o.abortErr, "writer %d: the temporary file stays", i)
					wantPartial = append(wantPartial, dirTestTemp(pack))
				}
			}
			slices.Sort(wantKeys)
			slices.Sort(wantPartial)
			assert.Equal(t, wantKeys, keys)
			assert.Equal(t, wantPartial, partial)
			_, err = s.Create(t.Context(), dirTestInfo(writers, UUID{0xD0, writers}))
			require.Error(t, err, "Create after Close")
		})
	}
}

// TestDirSinkCloseRootError reports the error of closing the root, wrapped, and closes nothing on a second Close.
func TestDirSinkCloseRootError(t *testing.T) {
	t.Parallel()

	s, fsys := newRecordedDirSink(t, t.TempDir())
	fsys.fail = failCall("closeRoot", errInjected)
	err := s.Close()
	require.ErrorIs(t, err, errInjected)
	assert.True(t, strings.HasPrefix(err.Error(), "tracepack: DirSink: close root: "), err.Error())
	n := len(fsys.calls)
	require.NoError(t, s.Close())
	assert.Equal(t, fsys.calls[:n], fsys.calls)
	_, err = s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
	require.Error(t, err, "the sink is closed even though its root's close failed")
	assert.NotErrorIs(t, err, errInjected)
}
