package tracepack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	// partialDir is the directory under a DirSink's root that holds the segments being written, outside every key area.
	partialDir = ".partial"
	// dirSinkFileMode is the permission of a segment file a DirSink creates:
	// no other user reads a segment, which holds records in full (the tracepack storage specification §3),
	// while its group may, so a group can grant an uploader read access.
	dirSinkFileMode fs.FileMode = 0o640
	// dirSinkDirMode is the permission of a directory a DirSink creates, which its group may list as dirSinkFileMode lets it read.
	dirSinkDirMode fs.FileMode = 0o750
)

// dirSegmentState is where a dirSegment stands in the SegmentFile contract.
type dirSegmentState int

const (
	// dirSegmentOpen takes writes and syncs.
	dirSegmentOpen dirSegmentState = iota
	// dirSegmentCommitting is a segment whose Commit was called and did not rename it.
	dirSegmentCommitting
	// dirSegmentPublished is a segment renamed to its key, visible, whether its directory syncs succeeded or not.
	dirSegmentPublished
	// dirSegmentAborted is a segment Abort discarded.
	dirSegmentAborted
)

// dirSink is the SegmentSink NewDirSink returns.
type dirSink struct {
	fsys dirFS
	// prefix is the bucket prefix, accepted as checkDirSinkPrefix checks it.
	prefix string
}

var _ SegmentSink = (*dirSink)(nil)

// dirSegment is a segment a dirSink is writing: a temporary file under .partial/ until Commit renames it to its key.
type dirSegment struct {
	sink *dirSink
	file dirFile
	key  string
	// temp and dir are the names of the temporary file and of the key's directory; the key names the file once committed.
	temp, dir string
	state     dirSegmentState
	// closed reports the temporary file closed, by Commit or by Abort.
	closed bool
}

var _ SegmentFile = (*dirSegment)(nil)

// dirFS is the file system a dirSink works on:
// the root opened as an os.Root, or a test's that injects faults and records the calls.
// Every name is relative to the root and '/'-separated, "." naming the root.
type dirFS interface {
	// stat returns the file information of name, following symbolic links.
	stat(name string) (fs.FileInfo, error)
	// lstat returns the file information of name, describing a symbolic link at name, not its target.
	lstat(name string) (fs.FileInfo, error)
	// create creates the file name for writing; it fails when name exists.
	create(name string) (dirFile, error)
	// mkdirAll creates the directory name and its missing parents.
	mkdirAll(name string) error
	// rename renames the file from to to.
	rename(from, to string) error
	// syncDir makes the entries of the directory name durable.
	syncDir(name string) error
	// remove removes the file name.
	remove(name string) error
}

// dirFile is a file a dirFS created.
type dirFile interface {
	io.Writer
	Sync() error
	Close() error
}

// rootFS is the dirFS of a directory opened as an os.Root, so no name resolves outside it.
type rootFS struct {
	root *os.Root
}

var _ dirFS = rootFS{}

// NewDirSink returns a SegmentSink that stores each segment as a file under root,
// at the path its segment key spells:
// <root>/<SegmentKey(prefix, info.ToolID, info.CaptureID, info.SeqFirst, info.PackID)>,
// so a local recording is laid out as a bucket is (the tracepack storage specification §3).
//
// root must be an existing directory, and its own durability is the caller's.
// NewDirSink opens root as an os.Root and makes every file-system call through it,
// so no path, .partial/ included, resolves outside root:
// a symbolic link leading out of root fails the call that meets it.
// A symbolic link inside root is refused where the sink manages directories,
// so no link makes .partial/ and a key area one directory:
// .partial itself, checked when the sink is created and at each Create,
// and every directory component of a key, prefix included, checked by Commit after it creates them and before the rename.
// Such a link fails NewDirSink or Create, or Commit with a plain error, nothing published.
// These checks guard against a misconfigured root,
// not against a process that writes to root while the sink runs, which can remove segments anyway.
// The sink keeps root open for as long as it is reachable; it has nothing to close.
// NewDirSink creates <root>/.partial/ and syncs root.
// It fails on js, where os.Root cannot exclude a link replaced after its check.
// prefix is empty, or a relative '/'-separated path none of whose components is empty, "." or "..",
// holding no '\' and no ':', so no platform reads it as another separator or as a volume,
// and whose first component is not .partial, the directory of the temporary files, so no committed segment lies among them;
// nor, when the sink is created, a name of that same directory, as a case variant is on a file system that folds case.
// It is used as given, never cleaned, so the path of each file spells its key.
// On a file system that folds case or rewrites names, as macOS and Windows can, two keys may share a path component;
// nothing escapes root even then, and no file is overwritten, since every key holds its segment's unique pack_id.
//
// Create creates the segment's temporary file, <root>/.partial/<pack_id>.tpk,
// outside every key area of the tracepack storage specification §3 and on root's file system,
// so a listing of the key areas sees only committed segments, never an open or abandoned one.
// Commit syncs and closes the temporary file, creates the key's missing directories,
// checks that none of the key's directories is a link, renames the file to its key,
// then syncs every directory from the key's directory up to root, deepest first, root included.
// It syncs the whole chain on every commit,
// so a directory that a failed commit or another sink created is made durable by the next successful one.
// Commit checks its ctx first, before the rename and before each directory sync.
// A Commit that fails before the rename, ctx's error included, published nothing.
// Once the rename succeeded the segment is visible under its key,
// and an error after it, ctx's included, wraps ErrPublishUncertain beside its cause:
// the segment is published and may not survive a system crash.
// Abort closes and removes the temporary file, ignoring one already gone, and does nothing once the rename succeeded.
// A temporary file left by a crash, or by a writer that never called Abort, stays under .partial/,
// which no listing of the key areas reaches.
// Directory syncs are skipped on Windows, which cannot sync a directory.
// NewDirSink creates files with mode 0640 and directories with mode 0750, less the process umask:
// no other user reads a segment, which holds records in full,
// while a group, such as one a setgid root gives every new file, can grant an uploader read access.
//
// A second Commit, a Commit after Abort, and a Write or Sync after Commit or Abort return an error;
// a second Abort does nothing.
// The sink is safe for concurrent use; each SegmentFile it creates is not.
//
// Parameters:
//   - root: an existing directory.
//   - prefix: the bucket prefix the keys start with, possibly empty.
//
// Returns:
//   - SegmentSink: the sink; nil on error.
//   - error: non-nil, before any file is created, on js, for a prefix outside that form, or for a root that is not an existing directory;
//     the error of creating .partial/, which a .partial leading out of root fails;
//     an error for a .partial that is a symbolic link;
//     an error for a prefix whose first component names .partial/ or leads out of root;
//     or the error of syncing root.
func NewDirSink(root, prefix string) (SegmentSink, error) {
	if runtime.GOOS == "js" {
		return nil, errors.New("tracepack: DirSink: unsupported on js, where os.Root cannot exclude a link replaced after its check")
	}
	if err := checkDirSinkPrefix(prefix); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("tracepack: DirSink root: %w", err)
	}
	s, err := newDirSinkFS(prefix, rootFS{root: r})
	if err != nil {
		_ = r.Close() // the error to report is newDirSinkFS's
		return nil, err
	}

	return s, nil
}

// newDirSinkFS is NewDirSink over the file system fsys, its root already open.
func newDirSinkFS(prefix string, fsys dirFS) (*dirSink, error) {
	if err := checkDirSinkPrefix(prefix); err != nil {
		return nil, err
	}
	if err := fsys.mkdirAll(partialDir); err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: create %s: %w", partialDir, err)
	}
	if err := checkNoLink(fsys, partialDir); err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: %w", err)
	}
	if err := checkPartialAlias(prefix, fsys); err != nil {
		return nil, err
	}
	if err := fsys.syncDir("."); err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: sync root: %w", err)
	}

	return &dirSink{fsys: fsys, prefix: prefix}, nil
}

// checkPartialAlias reports an error when the first component of prefix names the directory .partial/ names,
// as a case variant does on a file system that folds case; a first component that does not exist names nothing.
func checkPartialAlias(prefix string, fsys dirFS) error {
	if prefix == "" {
		return nil
	}
	first, _, _ := strings.Cut(prefix, "/")
	fi, err := fsys.stat(first)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tracepack: DirSink prefix %q: %w", prefix, err)
	}
	pi, err := fsys.stat(partialDir)
	if err != nil {
		return fmt.Errorf("tracepack: DirSink: %s: %w", partialDir, err)
	}
	if os.SameFile(fi, pi) {
		return fmt.Errorf("tracepack: DirSink prefix %q starts with %q, which names %s, the directory of the temporary files",
			prefix, first, partialDir)
	}

	return nil
}

// checkNoLink reports an error when name, a directory the sink manages, is a symbolic link,
// which could alias .partial/ and a key area within root, or when name cannot be examined.
func checkNoLink(fsys dirFS, name string) error {
	fi, err := fsys.lstat(name)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link, which the sink refuses where it manages directories", name)
	}

	return nil
}

// checkDirSinkPrefix reports an error for a prefix a DirSink refuses:
// one that is not empty or a relative '/'-separated path whose components are none empty, "." or "..",
// one that holds '\' or ':', or one whose first component is .partial.
func checkDirSinkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.ContainsAny(prefix, `\:`) {
		return fmt.Errorf(`tracepack: DirSink prefix %q holds '\' or ':'`, prefix)
	}
	if first, _, _ := strings.Cut(prefix, "/"); first == partialDir {
		return fmt.Errorf("tracepack: DirSink prefix %q starts with %s, the directory of the temporary files", prefix, partialDir)
	}
	for c := range strings.SplitSeq(prefix, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("tracepack: DirSink prefix %q has the component %q", prefix, c)
		}
	}

	return nil
}

// Create creates the temporary file of the segment info describes, under the key SegmentKey builds from info.
//
// Returns:
//   - SegmentFile: the segment; nil on error.
//   - error: ctx's error; the SegmentKey error, wrapping ErrInvalidKey, for a segment no key can name;
//     an error for a .partial that is a symbolic link;
//     or the error of creating the temporary file.
func (s *dirSink) Create(ctx context.Context, info SegmentInfo) (SegmentFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := SegmentKey(s.prefix, info.ToolID, info.CaptureID, info.SeqFirst, info.PackID)
	if err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: %w", err)
	}

	if err := checkNoLink(s.fsys, partialDir); err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: %w", err)
	}
	temp := partialDir + "/" + info.PackID.String() + keyPackSuffix
	file, err := s.fsys.create(temp)
	if err != nil {
		return nil, fmt.Errorf("tracepack: DirSink: create segment %s: %w", key, err)
	}

	return &dirSegment{sink: s, file: file, key: key, temp: temp, dir: path.Dir(key)}, nil
}

// keyDirs returns the directories from key's directory up to root, deepest first, root, ".", included.
// An accepted prefix and a key's other components are clean, so each name spells a directory of the key.
func keyDirs(key string) []string {
	var dirs []string
	for d := path.Dir(key); d != "."; d = path.Dir(d) {
		dirs = append(dirs, d)
	}

	return append(dirs, ".")
}

// Write writes p to the temporary file.
func (f *dirSegment) Write(p []byte) (int, error) {
	if f.state != dirSegmentOpen {
		return 0, f.misuse("Write")
	}

	return f.file.Write(p)
}

// Sync syncs the temporary file.
func (f *dirSegment) Sync() error {
	if f.state != dirSegmentOpen {
		return f.misuse("Sync")
	}

	return f.file.Sync()
}

// Commit publishes the segment under its key, as NewDirSink describes.
func (f *dirSegment) Commit(ctx context.Context) error {
	if f.state != dirSegmentOpen {
		return f.misuse("Commit")
	}
	f.state = dirSegmentCommitting

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.file.Sync(); err != nil {
		return fmt.Errorf("tracepack: DirSink: sync segment %s: %w", f.key, err)
	}
	f.closed = true
	if err := f.file.Close(); err != nil {
		return fmt.Errorf("tracepack: DirSink: close segment %s: %w", f.key, err)
	}
	fsys := f.sink.fsys
	if err := fsys.mkdirAll(f.dir); err != nil {
		return fmt.Errorf("tracepack: DirSink: create the directory of segment %s: %w", f.key, err)
	}
	dirs := keyDirs(f.key)
	for _, dir := range slices.Backward(dirs[:len(dirs)-1]) { // shallowest first, root, the last, left out
		if err := checkNoLink(fsys, dir); err != nil {
			return fmt.Errorf("tracepack: DirSink: segment %s: %w", f.key, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fsys.rename(f.temp, f.key); err != nil {
		return fmt.Errorf("tracepack: DirSink: rename segment %s: %w", f.key, err)
	}

	f.state = dirSegmentPublished
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: segment %s: %w", ErrPublishUncertain, f.key, err)
		}
		if err := fsys.syncDir(dir); err != nil {
			return fmt.Errorf("%w: segment %s: sync directory %s: %w", ErrPublishUncertain, f.key, dir, err)
		}
	}

	return nil
}

// Abort closes and removes the temporary file, ignoring one already gone;
// it does nothing once Commit renamed the segment, or after an earlier Abort.
func (f *dirSegment) Abort() error {
	if f.state == dirSegmentPublished || f.state == dirSegmentAborted {
		return nil
	}
	f.state = dirSegmentAborted

	var closeErr, removeErr error
	if !f.closed {
		f.closed = true
		if err := f.file.Close(); err != nil {
			closeErr = fmt.Errorf("tracepack: DirSink: close segment %s: %w", f.key, err)
		}
	}
	if err := f.sink.fsys.remove(f.temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		removeErr = fmt.Errorf("tracepack: DirSink: remove segment %s: %w", f.key, err)
	}

	return errors.Join(closeErr, removeErr)
}

// misuse returns the error of a call the SegmentFile contract forbids in the segment's state.
func (f *dirSegment) misuse(call string) error {
	return fmt.Errorf("tracepack: DirSink: %s on segment %s after Commit or Abort", call, f.key)
}

func (f rootFS) stat(name string) (fs.FileInfo, error) {
	return f.root.Stat(filepath.FromSlash(name))
}

func (f rootFS) lstat(name string) (fs.FileInfo, error) {
	return f.root.Lstat(filepath.FromSlash(name))
}

func (f rootFS) create(name string) (dirFile, error) {
	return f.root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, dirSinkFileMode)
}

func (f rootFS) mkdirAll(name string) error {
	return f.root.MkdirAll(filepath.FromSlash(name), dirSinkDirMode)
}

func (f rootFS) rename(from, to string) error {
	return f.root.Rename(filepath.FromSlash(from), filepath.FromSlash(to))
}

// syncDir syncs the directory name; on Windows, which cannot sync a directory, it does nothing.
func (f rootFS) syncDir(name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := f.root.Open(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}

	return err
}

func (f rootFS) remove(name string) error {
	return f.root.Remove(filepath.FromSlash(name))
}
