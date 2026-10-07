package tracepack

import (
	"context"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// dirTestPrefix is the bucket prefix of the DirSink tests.
	dirTestPrefix = "bucket/traces"
	// dirTestTool is the tool_id of the DirSink tests, which a key spells escaped, EQP%2001.
	dirTestTool = "EQP 01"
)

var (
	dirTestCapture = UUID{0xC0, 1}
	dirTestPackA   = UUID{0xA0, 1}
	dirTestPackB   = UUID{0xB0, 1}
)

// recordFS is a dirFS over another, the root opened as an os.Root, that records every call and injects faults.
type recordFS struct {
	// root is the path of the root fsys works in.
	root string
	fsys dirFS
	// calls lists each call in order, "<op> <name>", every name as the dirSink gives it, relative to root;
	// a rename lists both of its names, and a file's calls name the file.
	calls []string
	// fail, when set, runs before each call with its entry in calls; a non-nil error fails the call without making it.
	fail func(call string) error
	// after, when set, runs after each call that succeeded, with its entry in calls.
	after func(call string)
}

// recordFile is a file a recordFS created.
type recordFile struct {
	fsys *recordFS
	name string
	f    dirFile
}

// countSink is a SegmentSink that counts the Commit and Abort calls on the files the sink it wraps creates.
type countSink struct {
	SegmentSink

	commits, aborts int
}

// countFile is a file a countSink created.
type countFile struct {
	SegmentFile

	sink *countSink
}

// foldFS is a dirFS over another that folds every name to lower case, as a file system that folds case resolves it.
type foldFS struct {
	fsys dirFS
}

var (
	_ dirFS       = (*recordFS)(nil)
	_ dirFS       = foldFS{}
	_ SegmentSink = (*countSink)(nil)
)

// newRecordedDirSink returns a dirSink over root and dirTestPrefix whose file-system calls the returned recordFS records.
func newRecordedDirSink(t testing.TB, root string) (*dirSink, *recordFS) {
	t.Helper()

	fsys := newRecordFS(t, root)
	s, err := newDirSinkFS(dirTestPrefix, fsys)
	require.NoError(t, err)

	return s, fsys
}

// newRecordFS returns a recordFS over root opened as an os.Root, which is closed when the test ends.
func newRecordFS(t testing.TB, root string) *recordFS {
	t.Helper()

	r, err := os.OpenRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, r.Close()) })

	return &recordFS{root: root, fsys: rootFS{root: r}}
}

// dirTestInfo returns the SegmentInfo of a segment of dirTestTool's capture dirTestCapture, of seqFirst and pack, in blockTestHour.
func dirTestInfo(seqFirst uint64, pack UUID) SegmentInfo {
	return SegmentInfo{
		ToolID: dirTestTool, CaptureID: dirTestCapture, PackID: pack, SeqFirst: seqFirst, Hour: hourOf(blockTestHour),
		PeriodStart: time.Unix(0, blockTestHour).UTC(), PeriodEnd: time.Unix(0, blockTestHour+int64(DefaultFlushInterval)).UTC(),
	}
}

// dirTestKey returns the key of the segment dirTestInfo(seqFirst, pack) describes, under dirTestPrefix.
func dirTestKey(t testing.TB, seqFirst uint64, pack UUID) string {
	t.Helper()

	key, err := SegmentKey(dirTestPrefix, dirTestTool, dirTestCapture, seqFirst, pack)
	require.NoError(t, err)

	return key
}

// dirTestSyncs returns the directory syncs a commit of a segment of dirTestCapture under dirTestPrefix makes,
// as recordFS lists them:
// from the key's directory up to root, deepest first.
func dirTestSyncs() []string {
	return []string{
		"syncDir bucket/traces/staging/EQP%2001/" + dirTestCapture.String(),
		"syncDir bucket/traces/staging/EQP%2001",
		"syncDir bucket/traces/staging",
		"syncDir bucket/traces",
		"syncDir bucket",
		"syncDir .",
	}
}

// dirFiles returns the regular files under root, symbolic links left out and never followed,
// as '/'-separated paths relative to root, in byte order:
// keys every file outside .partial/, and partial the files under it.
func dirFiles(t testing.TB, root string) (keys, partial []string) {
	t.Helper()

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, partialDir+"/") {
			partial = append(partial, rel)
		} else {
			keys = append(keys, rel)
		}

		return nil
	})
	require.NoError(t, err)

	return keys, partial
}

// syncCalls returns the directory syncs among calls.
func syncCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "syncDir ") {
			out = append(out, c)
		}
	}

	return out
}

// hasCall reports whether calls holds a call of op.
func hasCall(calls []string, op string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, op+" ") })
}

// processUmask returns the process umask, read from the mode of a file created in dir with every permission bit,
// without changing it for the tests running in parallel.
func processUmask(t testing.TB, dir string) fs.FileMode {
	t.Helper()

	name := filepath.Join(dir, "umask-probe")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o777) //nolint:gosec // a probe in the test's own directory
	require.NoError(t, err)
	require.NoError(t, f.Close())
	info, err := os.Stat(name)
	require.NoError(t, err)
	require.NoError(t, os.Remove(name))

	return 0o777 &^ info.Mode().Perm()
}

// writeSegment creates the segment info describes on s, writes data to it and returns it, failing the test on error.
func writeSegment(t testing.TB, s SegmentSink, info SegmentInfo, data string) SegmentFile {
	t.Helper()

	f, err := s.Create(t.Context(), info)
	require.NoError(t, err)
	_, err = f.Write([]byte(data))
	require.NoError(t, err)
	require.NoError(t, f.Sync())

	return f
}

func TestNewDirSinkRefusesPrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{
		".", "..", "a/../b", "a/./b", "a/..", "a//b", "/a", "a/", "/",
		`a\b`, `\a`, "C:", "c:/x", "a/b:c",
		".partial", ".partial/x",
	} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, err := NewDirSink(root, prefix)
			require.Error(t, err)
			assert.Nil(t, s)
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			assert.Empty(t, entries, "no file created")

			fsys := newRecordFS(t, root)
			_, err = newDirSinkFS(prefix, fsys)
			require.Error(t, err)
			assert.Empty(t, fsys.calls, "refused before any file-system call")
		})
	}
}

func TestNewDirSinkAcceptsPrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"", "a", dirTestPrefix, "...", "a.b/~c_d-e/%41", "x y", "a/.partial", ".partials"} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			s, err := NewDirSink(root, prefix)
			require.NoError(t, err)
			info, err := os.Stat(filepath.Join(root, partialDir))
			require.NoError(t, err)
			assert.True(t, info.IsDir(), ".partial/ created")

			// The file's path under root spells the key byte for byte.
			f := writeSegment(t, s, dirTestInfo(7, dirTestPackA), "segment")
			require.NoError(t, f.Commit(t.Context()))
			key, err := SegmentKey(prefix, dirTestTool, dirTestCapture, 7, dirTestPackA)
			require.NoError(t, err)
			keys, partial := dirFiles(t, root)
			assert.Equal(t, []string{key}, keys)
			assert.Empty(t, partial)
		})
	}
}

// TestNewDirSinkRefusesJS runs only on js, where NewDirSink fails before it creates anything.
func TestNewDirSinkRefusesJS(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "js" {
		t.Skip("NewDirSink fails only on js")
	}
	root := t.TempDir()
	s, err := NewDirSink(root, dirTestPrefix)
	require.ErrorContains(t, err, "unsupported on js")
	assert.Nil(t, s)
	assert.Empty(t, dirNames(t, root), "nothing created")
}

func TestNewDirSinkRefusesRoot(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	missing := filepath.Join(parent, "missing")
	partialFile := filepath.Join(parent, "partial-file")
	require.NoError(t, os.Mkdir(partialFile, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(partialFile, partialDir), []byte("x"), 0o600))

	for _, root := range []string{missing, file, "", partialFile} {
		s, err := NewDirSink(root, dirTestPrefix)
		require.Error(t, err, "root %q", root)
		assert.Nil(t, s)
	}
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"file", "partial-file"}, names, "nothing created beside them")
}

// dirSymlink makes link a symbolic link to target, skipping the test where symbolic links cannot be made.
func dirSymlink(t testing.TB, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}

// dirNames returns the names of the entries of dir, in byte order.
func dirNames(t testing.TB, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// TestDirSinkConfinedToRoot gives root a symbolic link leading out of it, to a directory outside:
// staging, the key area of a segment without a prefix, makes the Commit fail before the rename, with a plain error;
// .partial, or the first component of the prefix, makes NewDirSink fail.
// Nothing is created outside root.
func TestDirSinkConfinedToRoot(t *testing.T) {
	t.Parallel()

	t.Run("staging", func(t *testing.T) {
		t.Parallel()

		root, outside := t.TempDir(), t.TempDir()
		dirSymlink(t, outside, filepath.Join(root, "staging"))
		s, err := NewDirSink(root, "")
		require.NoError(t, err)
		f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
		err = f.Commit(t.Context())
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPublishUncertain, "nothing published")
		require.NoError(t, f.Abort())
		assert.Empty(t, dirNames(t, outside), "nothing created outside root")
		assert.Empty(t, dirNames(t, filepath.Join(root, partialDir)), "Abort removed the temporary file")
	})
	for _, link := range []string{partialDir, "bucket"} {
		t.Run(link, func(t *testing.T) {
			t.Parallel()

			root, outside := t.TempDir(), t.TempDir()
			dirSymlink(t, outside, filepath.Join(root, link))
			s, err := NewDirSink(root, dirTestPrefix)
			require.Error(t, err)
			assert.Nil(t, s)
			assert.Empty(t, dirNames(t, outside), "nothing created outside root")
		})
	}
}

// TestNewDirSinkRefusesPartialAlias gives prefixes whose first component names .partial/ under another name:
// a symbolic link inside root, and a case variant on a file system that folds case, which foldFS models.
// NewDirSink refuses each, and accepts a first component naming another directory,
// though a commit refuses one that is a link.
func TestNewDirSinkRefusesPartialAlias(t *testing.T) {
	t.Parallel()

	t.Run("a symbolic link", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		dirSymlink(t, partialDir, filepath.Join(root, "ALIAS"))
		for _, prefix := range []string{"ALIAS", "ALIAS/traces"} {
			s, err := NewDirSink(root, prefix)
			require.ErrorContains(t, err, "the directory of the temporary files", "prefix %q", prefix)
			assert.Nil(t, s)
		}
		assert.Equal(t, []string{partialDir, "ALIAS"}, dirNames(t, root))
		assert.Empty(t, dirNames(t, filepath.Join(root, partialDir)))

		require.NoError(t, os.Mkdir(filepath.Join(root, "bucket"), 0o700))
		dirSymlink(t, "bucket", filepath.Join(root, "other"))
		s, err := NewDirSink(root, "other/traces")
		require.NoError(t, err, "a link to another directory")
		requireCommitRefused(t, s, root)
	})

	t.Run("a case variant", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(root, "bucket"), 0o700))
		for _, tt := range []struct {
			prefix string
			refuse bool
		}{{".PARTIAL", true}, {".Partial/traces", true}, {"BUCKET/traces", false}, {"missing/traces", false}} {
			fsys := newRecordFS(t, root)
			fsys.fsys = foldFS{fsys: fsys.fsys}
			s, err := newDirSinkFS(tt.prefix, fsys)
			if !tt.refuse {
				require.NoError(t, err, "prefix %q", tt.prefix)

				continue
			}
			require.ErrorContains(t, err, "the directory of the temporary files", "prefix %q", tt.prefix)
			assert.Nil(t, s)
			assert.False(t, hasCall(fsys.calls, "syncDir"), "prefix %q: refused before root is synced", tt.prefix)
		}
	})
}

// requireCommitRefused requires that a segment created on s fails its Commit for a symbolic link, with a plain error,
// and that its Abort leaves no file under root, at a key or under .partial/.
func requireCommitRefused(t testing.TB, s SegmentSink, root string) {
	t.Helper()

	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
	err := f.Commit(t.Context())
	require.ErrorContains(t, err, "is a symbolic link")
	assert.NotErrorIs(t, err, ErrPublishUncertain, "nothing published")
	require.NoError(t, f.Abort())
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys, "nothing published")
	assert.Empty(t, partial, "nothing among the temporary files, the temporary file removed")
}

// TestDirSinkRefusesLinks gives root symbolic links inside it where the sink manages directories:
// a directory of a key, aliasing .partial/ or another directory, fails the Commit before the rename, with a plain error;
// .partial, aliasing a key area or replaced by a link after the sink was created, fails NewDirSink or Create.
func TestDirSinkRefusesLinks(t *testing.T) {
	t.Parallel()

	t.Run("staging aliasing .partial without a prefix", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		s, err := NewDirSink(root, "")
		require.NoError(t, err)
		dirSymlink(t, partialDir, filepath.Join(root, "staging"))
		requireCommitRefused(t, s, root)
	})
	t.Run("a prefix directory aliasing a directory under .partial", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, partialDir, "x"), 0o700))
		require.NoError(t, os.Mkdir(filepath.Join(root, "a"), 0o700))
		dirSymlink(t, filepath.Join("..", partialDir, "x"), filepath.Join(root, "a", "b"))
		s, err := NewDirSink(root, "a/b")
		require.NoError(t, err, "a link below the prefix's first component")
		requireCommitRefused(t, s, root)
	})
	t.Run("a key's tool directory aliasing another directory", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "bucket", "traces", "staging"), 0o700))
		require.NoError(t, os.Mkdir(filepath.Join(root, "elsewhere"), 0o700))
		dirSymlink(t, filepath.Join("..", "..", "..", "elsewhere"), filepath.Join(root, "bucket", "traces", "staging", "EQP%2001"))
		s, err := NewDirSink(root, dirTestPrefix)
		require.NoError(t, err)
		requireCommitRefused(t, s, root)
	})
	t.Run(".partial aliasing a key area", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "bucket", "traces", "staging"), 0o700))
		dirSymlink(t, filepath.Join("bucket", "traces", "staging"), filepath.Join(root, partialDir))
		s, err := NewDirSink(root, dirTestPrefix)
		require.ErrorContains(t, err, "is a symbolic link")
		assert.Nil(t, s)
	})
	t.Run(".partial replaced by a link", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		s, fsys := newRecordedDirSink(t, root)
		require.NoError(t, os.Remove(filepath.Join(root, partialDir)))
		require.NoError(t, os.Mkdir(filepath.Join(root, "elsewhere"), 0o700))
		dirSymlink(t, "elsewhere", filepath.Join(root, partialDir))
		n := len(fsys.calls)
		f, err := s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
		require.ErrorContains(t, err, "is a symbolic link")
		assert.Nil(t, f)
		assert.False(t, hasCall(fsys.calls[n:], "create"), "no file created")
		assert.Empty(t, dirNames(t, filepath.Join(root, "elsewhere")))
	})
}

func TestNewDirSinkSyncsRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, fsys := newRecordedDirSink(t, root)
	assert.Equal(t, []string{"mkdirAll .partial", "lstat .partial", "stat bucket", "syncDir ."}, fsys.calls, "bucket/ does not exist yet")

	// A second sink on the same root finds .partial/ and syncs root again.
	s, fsys := newRecordedDirSink(t, root)
	assert.Equal(t, []string{"mkdirAll .partial", "lstat .partial", "stat bucket", "syncDir ."}, fsys.calls)

	// Once a commit created bucket/, a new sink compares it with .partial/; without a prefix it compares nothing.
	require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment").Commit(t.Context()))
	_, fsys = newRecordedDirSink(t, root)
	assert.Equal(t, []string{"mkdirAll .partial", "lstat .partial", "stat bucket", "stat .partial", "syncDir ."}, fsys.calls)
	fsys = newRecordFS(t, root)
	_, err := newDirSinkFS("", fsys)
	require.NoError(t, err)
	assert.Equal(t, []string{"mkdirAll .partial", "lstat .partial", "syncDir ."}, fsys.calls)
}

func TestDirSinkCreate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	n := len(fsys.calls)

	for _, edit := range []func(*SegmentInfo){
		func(i *SegmentInfo) { i.ToolID = "" },
		func(i *SegmentInfo) { i.ToolID = ".." },
		func(i *SegmentInfo) { i.SeqFirst = math.MaxInt64 + 1 },
	} {
		info := dirTestInfo(0, dirTestPackA)
		edit(&info)
		f, err := s.Create(t.Context(), info)
		require.ErrorIs(t, err, ErrInvalidKey)
		assert.Nil(t, f)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Create(ctx, dirTestInfo(0, dirTestPackA))
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")

	// The temporary file is <pack_id>.tpk under .partial/, checked not to be a link, created exclusively.
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "first")
	assert.Equal(t, []string{"lstat .partial", "create .partial/" + dirTestPackA.String() + ".tpk"}, fsys.calls[n:n+2])
	_, err = s.Create(t.Context(), dirTestInfo(0, dirTestPackA))
	require.ErrorIs(t, err, fs.ErrExist)
	require.NoError(t, f.Commit(t.Context()))
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dirTestKey(t, 0, dirTestPackA))))
	require.NoError(t, err)
	assert.Equal(t, "first", string(data), "the first file kept")
}

// TestDirSinkCommit commits a segment into a fresh root: the file is synced and closed,
// the key's directories created and checked, shallowest first, not to be links, the file renamed to its key,
// then every directory from the key's up to root synced, deepest first.
func TestDirSinkCommit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(3, dirTestPackA), "segment")
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys, "nothing visible before Commit")
	assert.Len(t, partial, 1)

	n := len(fsys.calls)
	require.NoError(t, f.Commit(t.Context()))
	temp := ".partial/" + dirTestPackA.String() + ".tpk"
	key := dirTestKey(t, 3, dirTestPackA)
	want := append([]string{
		"sync " + temp, "close " + temp,
		"mkdirAll bucket/traces/staging/EQP%2001/" + dirTestCapture.String(),
		"lstat bucket", "lstat bucket/traces", "lstat bucket/traces/staging", "lstat bucket/traces/staging/EQP%2001",
		"lstat bucket/traces/staging/EQP%2001/" + dirTestCapture.String(),
		"rename " + temp + " " + key,
	}, dirTestSyncs()...)
	assert.Equal(t, want, fsys.calls[n:])

	keys, partial = dirFiles(t, root)
	assert.Equal(t, []string{key}, keys)
	assert.Empty(t, partial)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	require.NoError(t, err)
	assert.Equal(t, "segment", string(data))
}

// TestDirSinkAbortClosesFile fails a Commit in its file sync, before it closes the file:
// the error is plain, and Abort closes the file before it removes it.
func TestDirSinkAbortClosesFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
	fsys.fail = failCall("sync ", errInjected)
	err := f.Commit(t.Context())
	require.ErrorIs(t, err, errInjected)
	assert.NotErrorIs(t, err, ErrPublishUncertain)
	require.NoError(t, f.Abort())
	temp := ".partial/" + dirTestPackA.String() + ".tpk"
	require.GreaterOrEqual(t, len(fsys.calls), 3)
	assert.Equal(t, []string{"sync " + temp, "close " + temp, "remove " + temp}, fsys.calls[len(fsys.calls)-3:])
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys)
	assert.Empty(t, partial)
}

// TestDirSinkModes commits a segment and checks the modes of what the sink created:
// 0640 for the file, 0750 for .partial/ and every directory of the key, less the process umask.
func TestDirSinkModes(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	root := t.TempDir()
	umask := processUmask(t, root)
	s, _ := newRecordedDirSink(t, root)
	require.NoError(t, writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment").Commit(t.Context()))

	key := dirTestKey(t, 0, dirTestPackA)
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o640)&^umask, info.Mode().Perm(), "the segment file")
	dirs := []string{partialDir}
	for d := path.Dir(key); d != "."; d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	require.Len(t, dirs, 6, ".partial and the key's five directories")
	for _, d := range dirs {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(d)))
		require.NoError(t, err)
		assert.Equal(t, fs.FileMode(0o750)&^umask, info.Mode().Perm(), "directory %s", d)
	}
}

// TestDirSinkCommitCanceled commits with a done ctx: Commit fails with ctx's error alone before any file-system call,
// and the Abort that follows removes the temporary file.
func TestDirSinkCommitCanceled(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n := len(fsys.calls)
	err := f.Commit(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrPublishUncertain)
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
	require.NoError(t, f.Abort())
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys)
	assert.Empty(t, partial)
}

// TestDirSinkCommitRetry fails a commit in its directory syncs, after the rename,
// then commits another segment through a new sink on the same root: its commit syncs the whole chain again,
// the directories the failed commit created included.
func TestDirSinkCommitRetry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "first")
	fsys.fail = func(call string) error {
		if call == "syncDir bucket/traces/staging" {
			return errInjected
		}

		return nil
	}
	n := len(fsys.calls)
	err := f.Commit(t.Context())
	require.ErrorIs(t, err, ErrPublishUncertain)
	require.ErrorIs(t, err, errInjected)
	assert.Equal(t, dirTestSyncs()[:3], syncCalls(fsys.calls[n:]), "the syncs up to the failed one")
	require.NoError(t, f.Abort())
	assert.False(t, hasCall(fsys.calls[n:], "remove"), "Abort of a published segment removes nothing")

	s, fsys = newRecordedDirSink(t, root)
	f = writeSegment(t, s, dirTestInfo(5, dirTestPackB), "second")
	n = len(fsys.calls)
	require.NoError(t, f.Commit(t.Context()))
	assert.Equal(t, dirTestSyncs(), syncCalls(fsys.calls[n:]))

	keys, partial := dirFiles(t, root)
	assert.Equal(t, []string{dirTestKey(t, 0, dirTestPackA), dirTestKey(t, 5, dirTestPackB)}, keys)
	assert.Empty(t, partial)
}

// TestDirSinkSegmentFileMisuse calls a segment's methods out of the SegmentFile contract:
// every call after a Commit or an Abort fails without a file-system call, but Abort, which does nothing.
func TestDirSinkSegmentFileMisuse(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	s, fsys := newRecordedDirSink(t, root)

	// After a successful Commit.
	f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "committed")
	require.NoError(t, f.Commit(t.Context()))
	n := len(fsys.calls)
	require.Error(t, f.Commit(t.Context()))
	_, err := f.Write([]byte("x"))
	require.Error(t, err)
	require.Error(t, f.Sync())
	require.NoError(t, f.Abort())
	require.NoError(t, f.Abort())
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")
	keys, _ := dirFiles(t, root)
	assert.Equal(t, []string{dirTestKey(t, 0, dirTestPackA)}, keys, "the committed segment stays")

	// After an Abort, which closed and removed the file.
	f = writeSegment(t, s, dirTestInfo(1, dirTestPackB), "aborted")
	n = len(fsys.calls)
	require.NoError(t, f.Abort())
	temp := ".partial/" + dirTestPackB.String() + ".tpk"
	assert.Equal(t, []string{"close " + temp, "remove " + temp}, fsys.calls[n:], "close, then remove")
	_, partial := dirFiles(t, root)
	assert.Empty(t, partial)
	n = len(fsys.calls)
	require.Error(t, f.Commit(t.Context()))
	_, err = f.Write([]byte("x"))
	require.Error(t, err)
	require.Error(t, f.Sync())
	require.NoError(t, f.Abort())
	assert.Equal(t, fsys.calls[:n], fsys.calls, "no file-system call")

	// Abort ignores a temporary file already gone.
	info := dirTestInfo(2, UUID{0xD0})
	f = writeSegment(t, s, info, "gone")
	require.NoError(t, os.Remove(filepath.Join(root, partialDir, info.PackID.String()+".tpk")))
	require.NoError(t, f.Abort())

	// A Commit that failed before the rename is not retried.
	f = writeSegment(t, s, dirTestInfo(3, UUID{0xE0}), "failed")
	fsys.fail = func(call string) error {
		if strings.HasPrefix(call, "rename ") {
			return errInjected
		}

		return nil
	}
	require.ErrorIs(t, f.Commit(t.Context()), errInjected)
	fsys.fail = nil
	require.Error(t, f.Commit(t.Context()))
	require.NoError(t, f.Abort())
	keys, partial = dirFiles(t, root)
	assert.Equal(t, []string{dirTestKey(t, 0, dirTestPackA)}, keys)
	assert.Empty(t, partial)
}

// TestDirSinkSegmentWriter writes a capture through a SegmentWriter into a DirSink:
// while a segment is open, and with a temporary file abandoned under .partial/,
// the key areas hold only committed segments;
// every committed file lies at its key, which parses as a segment key naming the tool of the SegmentInfo,
// and opens as the segment it names, with the capture's records;
// and a store source listing the root through an ObjectStore observes every segment.
func TestDirSinkSegmentWriter(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{dirTestPrefix, ""} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			t.Parallel()
			writeCaptureIntoDirSink(t, prefix)
		})
	}
}

// writeCaptureIntoDirSink runs TestDirSinkSegmentWriter under prefix.
func writeCaptureIntoDirSink(t *testing.T, prefix string) {
	t.Helper()

	root := t.TempDir()
	sink, err := NewDirSink(root, prefix)
	require.NoError(t, err)
	opts := segOptions(sink)
	opts.Capture.ToolID = dirTestTool
	opts.Now = nowAt(segAt(time.Hour + time.Minute))
	w, err := NewSegmentWriter(t.Context(), opts)
	require.NoError(t, err)

	segFrame(t, w, time.Second)
	keys, partial := dirFiles(t, root)
	assert.Empty(t, keys, "the open segment is not visible")
	assert.Len(t, partial, 1)

	segFrame(t, w, 6*time.Minute) // the next period: the first segment is committed
	keys, partial = dirFiles(t, root)
	assert.Len(t, keys, 1)
	assert.Len(t, partial, 1)

	segFrame(t, w, time.Hour) // the next hour
	abandoned := dirTestInfo(0, UUID{0xAB})
	_ = writeSegment(t, sink, abandoned, "abandoned, never committed or aborted")
	require.NoError(t, w.Close(t.Context()))

	keys, partial = dirFiles(t, root)
	assert.Equal(t, []string{".partial/" + abandoned.PackID.String() + ".tpk"}, partial)
	require.Len(t, keys, 3)
	h0 := hourOf(blockTestHour)
	store := newMemStore()
	var next uint64
	for i, key := range keys {
		p, err := parseSegmentKey(prefix, key)
		require.NoError(t, err, "key %d", i)
		assert.Equal(t, dirTestTool, p.tool)
		assert.Equal(t, w.CaptureID(), p.capture)
		assert.True(t, strings.HasPrefix(key, keyArea(prefix, "staging")+"EQP%2001/"), "key %q", key)

		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		require.NoError(t, err)
		store.put(key, data)
		r := mustOpen(t, data, ReaderOptions{})
		h := r.Header()
		assert.Equal(t, p.pack, h.PackID, "key %d pack_id", i)
		assert.Equal(t, w.CaptureID(), h.CaptureID, "key %d capture_id", i)
		assert.Equal(t, p.seqFirst, h.Meta.SeqStart, "key %d seq_first", i)
		assert.True(t, h.Finalized)
		for _, it := range iterate(t, r, Query{Payloads: true}).items {
			assert.Equal(t, next, it.rec.Seq, "key %d: seqs continue across segments", i)
			next++
		}
	}
	assert.Equal(t, uint64(5), next, "start, three frames and stop")
	// The abandoned file is in the store too, under .partial/, which no listing of a key area reaches.
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(partial[0])))
	require.NoError(t, err)
	store.put(partial[0], data)

	cat := newFakeCatalog()
	cat.setIndexed(w.CaptureID(), h0, false)
	cat.setIndexed(w.CaptureID(), h0+1, false)
	src := newTestStoreSource(t, store, cat, func(o *StoreSourceOptions) { o.Prefix, o.Tool = prefix, dirTestTool })
	o, err := src.Observe(t.Context(), w.CaptureID(), h0, h0+2)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, o.Close()) })
	for h, want := range map[int64]int{h0: 2, h0 + 1: 1} {
		sc, err := o.Scope(t.Context(), h)
		require.NoError(t, err)
		assert.False(t, sc.Indexed)
		assert.Len(t, sc.Readers, want, "hour %d", h)
	}
}

// Create creates a file on the sink it wraps and counts its calls.
func (s *countSink) Create(ctx context.Context, info SegmentInfo) (SegmentFile, error) {
	f, err := s.SegmentSink.Create(ctx, info)
	if err != nil {
		return nil, err
	}

	return &countFile{SegmentFile: f, sink: s}, nil
}

// Commit counts the call and commits.
func (f *countFile) Commit(ctx context.Context) error {
	f.sink.commits++

	return f.SegmentFile.Commit(ctx)
}

// Abort counts the call and aborts.
func (f *countFile) Abort() error {
	f.sink.aborts++

	return f.SegmentFile.Abort()
}

// record records the call op on names and makes it with do, between the fault hooks.
func (s *recordFS) record(op string, do func() error, names ...string) error {
	call := strings.Join(append([]string{op}, names...), " ")
	s.calls = append(s.calls, call)
	if s.fail != nil {
		if err := s.fail(call); err != nil {
			return err
		}
	}
	if err := do(); err != nil {
		return err
	}
	if s.after != nil {
		s.after(call)
	}

	return nil
}

func (s *recordFS) stat(name string) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := s.record("stat", func() (err error) {
		info, err = s.fsys.stat(name)
		return err
	}, name)

	return info, err
}

func (s *recordFS) lstat(name string) (fs.FileInfo, error) {
	var info fs.FileInfo
	err := s.record("lstat", func() (err error) {
		info, err = s.fsys.lstat(name)
		return err
	}, name)

	return info, err
}

func (s *recordFS) create(name string) (dirFile, error) {
	var f dirFile
	err := s.record("create", func() (err error) {
		f, err = s.fsys.create(name)
		return err
	}, name)
	if err != nil {
		return nil, err
	}

	return &recordFile{fsys: s, name: name, f: f}, nil
}

func (s *recordFS) mkdirAll(name string) error {
	return s.record("mkdirAll", func() error { return s.fsys.mkdirAll(name) }, name)
}

func (s *recordFS) rename(from, to string) error {
	return s.record("rename", func() error { return s.fsys.rename(from, to) }, from, to)
}

func (s *recordFS) syncDir(name string) error {
	return s.record("syncDir", func() error { return s.fsys.syncDir(name) }, name)
}

func (s *recordFS) remove(name string) error {
	return s.record("remove", func() error { return s.fsys.remove(name) }, name)
}

func (f *recordFile) Write(p []byte) (int, error) {
	var n int
	err := f.fsys.record("write", func() (err error) {
		n, err = f.f.Write(p)
		return err
	}, f.name)

	return n, err
}

func (f *recordFile) Sync() error {
	return f.fsys.record("sync", f.f.Sync, f.name)
}

func (f *recordFile) Close() error {
	return f.fsys.record("close", f.f.Close, f.name)
}

func (f foldFS) stat(name string) (fs.FileInfo, error) {
	return f.fsys.stat(strings.ToLower(name))
}

func (f foldFS) lstat(name string) (fs.FileInfo, error) {
	return f.fsys.lstat(strings.ToLower(name))
}

func (f foldFS) create(name string) (dirFile, error) {
	return f.fsys.create(strings.ToLower(name))
}

func (f foldFS) mkdirAll(name string) error {
	return f.fsys.mkdirAll(strings.ToLower(name))
}

func (f foldFS) rename(from, to string) error {
	return f.fsys.rename(strings.ToLower(from), strings.ToLower(to))
}

func (f foldFS) syncDir(name string) error {
	return f.fsys.syncDir(strings.ToLower(name))
}

func (f foldFS) remove(name string) error {
	return f.fsys.remove(strings.ToLower(name))
}
