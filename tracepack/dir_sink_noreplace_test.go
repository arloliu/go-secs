package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNoReplaceDirSink returns a dirSink over root and dirTestPrefix that never replaces a file at a key,
// whose file-system calls the returned recordFS records.
func newNoReplaceDirSink(t testing.TB, root string) (*dirSink, *recordFS) {
	t.Helper()

	fsys := newRecordFS(t, root)
	s, err := newDirSinkFS(dirTestPrefix, fsys, DirSinkNoReplace())
	require.NoError(t, err)

	return s, fsys
}

// readKey returns the bytes of the file at key under root.
func readKey(t testing.TB, root, key string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	require.NoError(t, err)

	return string(data)
}

// statKey returns the file information of the file at key under root.
func statKey(t testing.TB, root, key string) fs.FileInfo {
	t.Helper()

	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	require.NoError(t, err)

	return info
}

// requireSegmentFinished requires that f takes no Write, Sync or Commit any more,
// and that its Abort does nothing: it returns nil without a file-system call.
func requireSegmentFinished(t testing.TB, f SegmentFile, fsys *recordFS) {
	t.Helper()

	n := len(fsys.calls)
	_, err := f.Write([]byte("x"))
	require.Error(t, err)
	require.Error(t, f.Sync())
	require.Error(t, f.Commit(t.Context()))
	require.NoError(t, f.Abort())
	require.NoError(t, f.Abort())
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
}

// TestDirSinkNoReplaceCommit commits a segment into a fresh root through a sink that never replaces a file at a key:
// the file is synced and closed, the key's directories created and checked,
// the file hard-linked to its key, the temporary file removed,
// then every directory from the key's up to root synced, deepest first.
func TestDirSinkNoReplaceCommit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newNoReplaceDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(3, dirTestPackA), "segment")

	n := len(fsys.calls)
	require.NoError(t, f.Commit(t.Context()))
	temp := ".partial/" + dirTestPackA.String() + ".tpk"
	key := dirTestKey(t, 3, dirTestPackA)
	want := append([]string{
		"sync " + temp, "close " + temp,
		"mkdirAll bucket/traces/staging/EQP%2001/" + dirTestCapture.String(),
		"lstat bucket", "lstat bucket/traces", "lstat bucket/traces/staging", "lstat bucket/traces/staging/EQP%2001",
		"lstat bucket/traces/staging/EQP%2001/" + dirTestCapture.String(),
		"link " + temp + " " + key,
		"remove " + temp,
	}, dirTestSyncs()...)
	assert.Equal(t, want, fsys.calls[n:])

	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	assert.Equal(t, "segment", readKey(t, root, key))
	n = len(fsys.calls)
	require.NoError(t, f.Abort())
	assert.Equal(t, fsys.calls[:n], fsys.calls, "Abort after a successful Commit does nothing")
}

// TestDirSinkNoReplaceExisting commits a segment whose key already holds a file:
// the link fails, Commit compares the two files after it and returns ErrSegmentExists for identical bytes
// or ErrSegmentConflict for different ones, naming the key;
// the existing file stays untouched, the temporary file is removed and the segment is finished.
func TestDirSinkNoReplaceExisting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		// want is the sentinel the error wraps, other the one it does not.
		want, other error
	}{
		{name: "identical bytes", data: "segment", want: ErrSegmentExists, other: ErrSegmentConflict},
		{name: "different bytes", data: "segment, but another", want: ErrSegmentConflict, other: ErrSegmentExists},
		{name: "a prefix of the bytes", data: "segm", want: ErrSegmentConflict, other: ErrSegmentExists},
		{name: "same size, one byte apart", data: "segmenT", want: ErrSegmentConflict, other: ErrSegmentExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := newNoReplaceDirSink(t, root)
			key := dirTestKey(t, 0, dirTestPackA)
			require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment").Commit(t.Context()))
			before := statKey(t, root, key)

			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), tt.data)
			n := len(fsys.calls)
			err := f.Commit(t.Context())
			require.ErrorIs(t, err, tt.want)
			require.NotErrorIs(t, err, tt.other)
			require.NotErrorIs(t, err, ErrPublishUncertain)
			assert.Contains(t, err.Error(), key, "the error names the key")

			temp := ".partial/" + dirTestPackA.String() + ".tpk"
			calls := fsys.calls[n:]
			link := slices.Index(calls, "link "+temp+" "+key)
			require.GreaterOrEqual(t, link, 0, "Commit links the temporary file to its key")
			assert.False(t, slices.ContainsFunc(calls[:link], func(c string) bool { return strings.HasSuffix(c, " "+key) }),
				"no call on the key before the link")
			assert.Contains(t, calls[link:], "open "+key, "the existing file read after the link failed")
			assert.Contains(t, calls[link:], "open "+temp, "the segment read after the link failed")
			assert.Equal(t, "remove "+temp, calls[len(calls)-1], "the temporary file removed last")
			assert.False(t, hasCall(calls, "rename"), "no rename")
			assert.False(t, hasCall(calls, "syncDir"), "no directory sync")

			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{key}, keys)
			assert.Empty(t, partial, "the temporary file removed")
			assert.Equal(t, "segment", readKey(t, root, key), "the existing file untouched")
			after := statKey(t, root, key)
			assert.True(t, os.SameFile(before, after), "the same file at the key")
			assert.Equal(t, before.ModTime(), after.ModTime(), "the existing file not modified")
			requireSegmentFinished(t, f, fsys)
		})
	}
}

// TestDirSinkNoReplaceRace publishes a file at the key from another writer after Commit checked the key's directories
// and just before its link:
// the link fails, and Commit returns ErrSegmentExists or ErrSegmentConflict, the other writer's file kept.
func TestDirSinkNoReplaceRace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		other string
		want  error
	}{
		{name: "identical bytes", other: "segment", want: ErrSegmentExists},
		{name: "different bytes", other: "another segment", want: ErrSegmentConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := newNoReplaceDirSink(t, root)
			key := dirTestKey(t, 0, dirTestPackA)
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			fsys.fail = func(call string) error {
				if strings.HasPrefix(call, "link ") {
					return os.WriteFile(filepath.Join(root, filepath.FromSlash(key)), []byte(tt.other), 0o600)
				}

				return nil
			}

			err := f.Commit(t.Context())
			require.ErrorIs(t, err, tt.want)
			require.NotErrorIs(t, err, ErrPublishUncertain)
			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{key}, keys)
			assert.Empty(t, partial)
			assert.Equal(t, tt.other, readKey(t, root, key), "the other writer's file kept")
			fsys.fail = nil
			requireSegmentFinished(t, f, fsys)
		})
	}
}

// TestDirSinkNoReplaceConcurrent commits the same key through several sinks on one root at once,
// with identical and different bytes.
// The writers of one key share its temporary file, which Create makes exclusively,
// so a writer whose Create finds it taken retries, up to a deadline, until the holder's Commit removed it:
// exactly one publishes, and every other's Commit fails with ErrSegmentExists for the winner's bytes or ErrSegmentConflict for others.
// Sharing the temporary file, these writers reach the link one at a time;
// TestDirSinkNoReplaceRace shows a file published between the checks and the link.
func TestDirSinkNoReplaceConcurrent(t *testing.T) {
	t.Parallel()

	const (
		writers = 8
		// patience bounds how long a writer retries a Create that finds the temporary file taken.
		patience = 10 * time.Second
	)
	root := t.TempDir()
	key := dirTestKey(t, 0, dirTestPackA)
	data := func(i int) string { return "segment " + strconv.Itoa(i%2) }
	type outcome struct {
		commitErr, abortErr error
	}
	outcomes := make([]outcome, writers)
	start := make(chan struct{})
	// holder is the writer whose temporary file exists, -1 for none.
	var holder atomic.Int64
	holder.Store(-1)
	var wg sync.WaitGroup
	for i := range writers {
		s, err := NewDirSink(root, dirTestPrefix, DirSinkNoReplace())
		require.NoError(t, err)
		wg.Go(func() {
			<-start
			deadline := time.Now().Add(patience)
			f, err := s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
			for errors.Is(err, fs.ErrExist) {
				if time.Now().After(deadline) {
					err = fmt.Errorf("writer %d: the temporary file stayed taken for %v, held by writer %d: %w", i, patience, holder.Load(), err)
					break
				}
				runtime.Gosched()
				f, err = s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
			}
			if err != nil {
				outcomes[i].commitErr = err
				return
			}
			holder.Store(int64(i))
			defer holder.CompareAndSwap(int64(i), -1)
			if _, err := f.Write([]byte(data(i))); err != nil {
				outcomes[i].commitErr = err
			} else {
				outcomes[i].commitErr = f.Commit(t.Context())
			}
			if outcomes[i].commitErr != nil {
				outcomes[i].abortErr = f.Abort()
			}
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, o := range outcomes {
		if o.commitErr == nil {
			require.Equal(t, -1, winner, "a second writer published: %d after %d", i, winner)
			winner = i
		}
	}
	require.GreaterOrEqual(t, winner, 0, "one writer published")
	for i, o := range outcomes {
		switch {
		case i == winner:
		case data(i) == data(winner):
			require.ErrorIs(t, o.commitErr, ErrSegmentExists, "writer %d", i)
		default:
			require.ErrorIs(t, o.commitErr, ErrSegmentConflict, "writer %d", i)
		}
		require.NoError(t, o.abortErr, "writer %d", i)
	}
	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	assert.Equal(t, data(winner), readKey(t, root, key), "the winner's file")
}

// TestDirSinkNoReplaceLinkFails fails the link for a reason other than an existing key,
// as a file system without hard links does:
// Commit returns the link's error, plain, nothing is published, and the Abort that follows removes the temporary file.
func TestDirSinkNoReplaceLinkFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newNoReplaceDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
	fsys.fail = failCall("link ", errInjected)

	n := len(fsys.calls)
	err := f.Commit(t.Context())
	require.ErrorIs(t, err, errInjected)
	for _, sentinel := range []error{ErrPublishUncertain, ErrSegmentExists, ErrSegmentConflict} {
		require.NotErrorIs(t, err, sentinel)
	}
	assert.False(t, hasCall(fsys.calls[n:], "open"), "no comparison")
	assert.False(t, hasCall(fsys.calls[n:], "remove"), "the temporary file left to Abort")
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys, "nothing published")
	assert.Len(t, partial, 1)

	n = len(fsys.calls)
	require.NoError(t, f.Abort())
	assert.Equal(t, []string{"remove .partial/" + dirTestPackA.String() + ".tpk"}, fsys.calls[n:])
	keys, partial = dirFiles(t, root)
	assert.Empty(t, keys)
	assert.Empty(t, partial)
}

// TestDirSinkNoReplaceCompareFails fails the comparison with the existing file:
// Commit returns that error, plain, the existing file untouched, and the Abort that follows removes the temporary file.
func TestDirSinkNoReplaceCompareFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newNoReplaceDirSink(t, root)
	key := dirTestKey(t, 0, dirTestPackA)
	require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment").Commit(t.Context()))
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
	fsys.fail = failCall("open ", errInjected)

	err := f.Commit(t.Context())
	require.ErrorIs(t, err, errInjected)
	for _, sentinel := range []error{ErrPublishUncertain, ErrSegmentExists, ErrSegmentConflict} {
		require.NotErrorIs(t, err, sentinel)
	}
	_, partial := dirFiles(t, root)
	assert.Len(t, partial, 1, "the temporary file left to Abort")
	fsys.fail = nil
	require.NoError(t, f.Abort())
	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	assert.Equal(t, "segment", readKey(t, root, key))
}

// TestDirSinkNoReplaceAfterLink fails a Commit after its link succeeded:
// the segment is visible under its key, the error wraps ErrPublishUncertain beside its cause,
// and the Abort that follows does nothing.
func TestDirSinkNoReplaceAfterLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		fail func(call string) error
		// at, when set, is the call after which ctx is cancelled.
		at string
		// cause is the error the Commit error wraps beside ErrPublishUncertain; partial the temporary files left.
		cause   error
		partial int
	}{
		{name: "removing the temporary file", fail: failCall("remove ", errInjected), cause: errInjected, partial: 1},
		{name: "syncing a directory", fail: failCall("syncDir bucket/traces/staging", errInjected), cause: errInjected},
		{name: "syncing root", fail: failCall("syncDir .", errInjected), cause: errInjected},
		{name: "cancelled after the link", at: "link ", cause: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := newNoReplaceDirSink(t, root)
			key := dirTestKey(t, 0, dirTestPackA)
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fsys.fail = tt.fail
			if tt.at != "" {
				fsys.after = func(c string) {
					if strings.HasPrefix(c, tt.at) {
						cancel()
					}
				}
			}

			err := f.Commit(ctx)
			require.ErrorIs(t, err, ErrPublishUncertain)
			require.ErrorIs(t, err, tt.cause)
			require.NotErrorIs(t, err, ErrSegmentExists)
			require.NotErrorIs(t, err, ErrSegmentConflict)
			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{key}, keys, "the segment visible")
			assert.Len(t, partial, tt.partial)
			assert.Equal(t, "segment", readKey(t, root, key))

			n := len(fsys.calls)
			require.NoError(t, f.Abort())
			assert.Equal(t, fsys.calls[:n], fsys.calls, "Abort of a published segment does nothing")
		})
	}
}

// TestDirSinkReplacesByDefault commits a segment whose key already holds a file through a sink without the option:
// the rename replaces the existing file.
func TestDirSinkReplacesByDefault(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	key := dirTestKey(t, 0, dirTestPackA)
	require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "first").Commit(t.Context()))
	n := len(fsys.calls)
	require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "second").Commit(t.Context()))
	assert.False(t, hasCall(fsys.calls[n:], "link"), "no link")
	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	assert.Equal(t, "second", readKey(t, root, key), "the existing file replaced")
}

// TestDirSinkNoReplaceSegmentWriter writes a segment through a SegmentWriter
// into a key that already holds a different file:
// the SegmentWriter fails with an error wrapping ErrSegmentConflict and aborts the segment, which does nothing,
// the existing file kept.
func TestDirSinkNoReplaceSegmentWriter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, err := NewDirSink(root, "", DirSinkNoReplace())
	require.NoError(t, err)
	sink := &segmentKeySink{SegmentSink: s}
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	segFrame(t, w, 0)

	// Another writer publishes a file at the key of the open segment.
	require.NotEmpty(t, sink.keys)
	key := sink.keys[0]
	name := filepath.Join(root, filepath.FromSlash(key))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o750))
	require.NoError(t, os.WriteFile(name, []byte("another segment"), 0o600))

	err = w.Close(t.Context())
	require.ErrorIs(t, err, ErrSegmentConflict)
	require.NotErrorIs(t, err, ErrPublishUncertain)
	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	assert.Equal(t, "another segment", readKey(t, root, key))
}

// TestDirSinkNoReplaceKeyNotFile commits a segment whose key is a symbolic link or a directory:
// the link fails, and Commit, finding no regular file at the key, returns a plain error without comparing anything,
// the key left as it is and nothing published; the Abort that follows removes the temporary file.
// A symbolic link to the temporary file itself would otherwise compare the segment with itself.
func TestDirSinkNoReplaceKeyNotFile(t *testing.T) {
	t.Parallel()

	temp := ".partial/" + dirTestPackA.String() + ".tpk"
	tests := []struct {
		name string
		// plant makes the key, at keyPath under root.
		plant func(t *testing.T, root, keyPath string)
		// files are the regular files outside .partial/ after the Abort.
		files []string
	}{
		{
			name: "a symbolic link to the temporary file",
			plant: func(t *testing.T, root, keyPath string) {
				target, err := filepath.Rel(filepath.Dir(keyPath), filepath.Join(root, filepath.FromSlash(temp)))
				require.NoError(t, err)
				dirSymlink(t, target, keyPath)
			},
		},
		{
			name: "a symbolic link to a file of the same bytes",
			plant: func(t *testing.T, root, keyPath string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "same.tpk"), []byte("segment"), 0o600))
				target, err := filepath.Rel(filepath.Dir(keyPath), filepath.Join(root, "same.tpk"))
				require.NoError(t, err)
				dirSymlink(t, target, keyPath)
			},
			files: []string{"same.tpk"},
		},
		{
			name:  "a directory",
			plant: func(t *testing.T, _, keyPath string) { require.NoError(t, os.Mkdir(keyPath, 0o750)) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := newNoReplaceDirSink(t, root)
			key := dirTestKey(t, 0, dirTestPackA)
			keyPath := filepath.Join(root, filepath.FromSlash(key))
			require.NoError(t, os.MkdirAll(filepath.Dir(keyPath), 0o750))
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			tt.plant(t, root, keyPath)
			before, err := os.Lstat(keyPath)
			require.NoError(t, err)

			n := len(fsys.calls)
			err = f.Commit(t.Context())
			require.Error(t, err)
			for _, sentinel := range []error{ErrPublishUncertain, ErrSegmentExists, ErrSegmentConflict} {
				require.NotErrorIs(t, err, sentinel)
			}
			assert.Contains(t, err.Error(), key, "the error names the key")
			assert.False(t, hasCall(fsys.calls[n:], "open"), "no comparison")
			assert.False(t, hasCall(fsys.calls[n:], "remove"), "the temporary file left to Abort")
			_, partial := dirFiles(t, root)
			assert.Len(t, partial, 1)

			n = len(fsys.calls)
			require.NoError(t, f.Abort())
			assert.Equal(t, []string{"remove " + temp}, fsys.calls[n:])
			keys, partial := dirFiles(t, root)
			assert.Equal(t, tt.files, keys, "nothing published")
			assert.Empty(t, partial)
			after, err := os.Lstat(keyPath)
			require.NoError(t, err)
			assert.True(t, os.SameFile(before, after), "the key left as it is")
		})
	}
}

// TestDirSinkNoReplaceRefusedRemoveFails fails the removal of the temporary file after Commit refused the segment:
// the error wraps the refusal's sentinel beside the removal's cause,
// and the segment stays abortable, so the Abort that follows removes the temporary file.
func TestDirSinkNoReplaceRefusedRemoveFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "identical bytes", data: "segment", want: ErrSegmentExists},
		{name: "different bytes", data: "another segment", want: ErrSegmentConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, fsys := newNoReplaceDirSink(t, root)
			key := dirTestKey(t, 0, dirTestPackA)
			require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment").Commit(t.Context()))
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), tt.data)
			fsys.fail = failCall("remove ", errInjected)

			err := f.Commit(t.Context())
			require.ErrorIs(t, err, tt.want)
			require.ErrorIs(t, err, errInjected)
			require.NotErrorIs(t, err, ErrPublishUncertain)
			_, partial := dirFiles(t, root)
			assert.Len(t, partial, 1, "the temporary file left")

			fsys.fail = nil
			n := len(fsys.calls)
			require.NoError(t, f.Abort())
			assert.Equal(t, []string{"remove .partial/" + dirTestPackA.String() + ".tpk"}, fsys.calls[n:], "Abort removes it")
			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{key}, keys)
			assert.Empty(t, partial)
			assert.Equal(t, "segment", readKey(t, root, key), "the existing file untouched")
			requireSegmentFinished(t, f, fsys)
		})
	}
}

// TestDirSinkNoReplaceSegmentWriterRemoveFails closes a SegmentWriter whose segment's key holds a different file,
// the removal of the temporary file failing once:
// Close returns an error wrapping ErrSegmentConflict and the removal's cause,
// and the Abort the SegmentWriter makes after the failed Commit removes the temporary file.
func TestDirSinkNoReplaceSegmentWriterRemoveFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newNoReplaceDirSink(t, root)
	sink := &segmentKeySink{SegmentSink: s, prefix: dirTestPrefix}
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)
	segFrame(t, w, 0)
	require.Len(t, sink.keys, 1)
	key := sink.keys[0]
	name := filepath.Join(root, filepath.FromSlash(key))
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o750))
	require.NoError(t, os.WriteFile(name, []byte("another segment"), 0o600))
	removes := 0
	fsys.fail = func(call string) error {
		if strings.HasPrefix(call, "remove ") {
			removes++
			if removes == 1 {
				return errInjected
			}
		}

		return nil
	}

	err = w.Close(t.Context())
	require.ErrorIs(t, err, ErrSegmentConflict)
	require.ErrorIs(t, err, errInjected)
	require.NotErrorIs(t, err, ErrPublishUncertain)
	assert.Equal(t, 2, removes, "Commit's removal, then Abort's")
	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial, "Abort removed the temporary file")
	assert.Equal(t, "another segment", readKey(t, root, key))
}

// segmentKeySink is a SegmentSink that records the key of every segment the sink it wraps creates, under prefix.
type segmentKeySink struct {
	SegmentSink

	prefix string
	keys   []string
}

// Create records the segment's key and creates it on the sink it wraps.
func (s *segmentKeySink) Create(ctx context.Context, info SegmentInfo) (SegmentFile, error) {
	key, err := SegmentKey(s.prefix, info.ToolID, info.CaptureID, info.SeqFirst, info.PackID)
	if err != nil {
		return nil, err
	}
	s.keys = append(s.keys, key)

	return s.SegmentSink.Create(ctx, info)
}
