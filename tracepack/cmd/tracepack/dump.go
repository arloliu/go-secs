package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/arloliu/go-secs/tracepack"
)

const dumpUsage = "tracepack dump --jsonl [-o <file>] <pack>"

// exportBuffer is the size of the buffer between the export and its destination.
const exportBuffer = 64 << 10

// tempFile is the temporary file publish writes.
type tempFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

// runDump runs dump: the canonical JSONL export of one pack, to stdout or to the file -o names.
func runDump(ctx context.Context, cfg config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("dump")
	jsonl := fs.Bool("jsonl", false, "write the canonical JSONL export, the only format")
	out := fs.String("o", "", "write the export to `file`, replaced only when the export succeeds")
	if code := parseFlags(fs, dumpUsage, args, stdout, stderr); code >= 0 {
		return code
	}
	if !*jsonl {
		return usageError(fs, dumpUsage, stderr, "dump needs --jsonl, its only format")
	}
	if fs.NArg() != 1 {
		return usageError(fs, dumpUsage, stderr, "dump takes exactly one pack")
	}
	path := fs.Arg(0)

	f, size, err := cfg.open(path)
	if err != nil {
		return failed(stderr, "dump", path, err)
	}
	defer f.Close()
	r, err := tracepack.Open(ctx, f, size, cfg.reader)
	if err != nil {
		return failed(stderr, "dump", path, err)
	}

	var res tracepack.Result
	export := func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, exportBuffer)
		var err error
		if res, err = tracepack.ExportJSONL(ctx, r, bw); err != nil {
			return err
		}

		return bw.Flush()
	}
	if *out == "" {
		if err := export(stdout); err != nil {
			failed(stderr, "dump", path, err)
			fmt.Fprintln(stderr, "tracepack dump: the output written is not an export")

			return exitFailed
		}
	} else if err := publish(*out, os.CreateTemp, export); err != nil {
		return failed(stderr, "dump", path, err)
	}

	return reportDump(stderr, r, res)
}

// reportDump writes the diagnostics of an export that succeeded to stderr and returns its exit code:
// one line per incomplete reason, footer error and redaction defect;
// exitFailed when they cannot be written, else exitNotClean when res is incomplete.
func reportDump(stderr io.Writer, r *tracepack.Reader, res tracepack.Result) int {
	rw := newReportWriter(stderr)
	for _, d := range res.Incomplete {
		rw.linef("incomplete", "%s", defectText(d))
	}
	for _, fe := range res.FooterErrs {
		rw.linef("footer-error", "%v", fe.Err)
	}
	for _, rd := range r.Header().RedactionDefects {
		rw.linef("redaction-defect", "%v", rd)
	}
	if err := rw.flush(); err != nil {
		return exitFailed
	}

	if !res.Complete() {
		return exitNotClean
	}

	return exitClean
}

// publish writes a file at path through write, so that path is replaced only by a file write completed:
// write writes a temporary file that create makes in path's directory, which is synced and closed;
// the file is renamed over path when write, the sync and the close succeed, and removed otherwise, leaving an existing path as it was.
// After the rename, path's directory is synced where the system allows it, so the rename itself is durable.
//
// Parameters:
//   - path: the file to publish.
//   - create: makes the temporary file, as os.CreateTemp.
//   - write: writes the file's content.
//
// Returns:
//   - error: the error of create, write, the sync, the close or the rename,
//     joined with the error of closing or removing the temporary file after it, each naming the temporary file;
//     or, with path published, the error of syncing its directory.
func publish[F tempFile](path string, create func(dir, pattern string) (F, error), write func(io.Writer) error) error {
	tmp, err := create(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := writeTemp(tmp, name, write); err != nil {
		return errors.Join(err, removeTemp(name))
	}
	if err := os.Rename(name, path); err != nil {
		return errors.Join(err, removeTemp(name))
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%s published, but its directory not synced: %w", path, err)
	}

	return nil
}

// writeTemp writes tmp, the temporary file name, through write, then syncs and closes it;
// it closes tmp whatever fails, joining the close's error, which names the file, to the first failure.
func writeTemp(tmp tempFile, name string, write func(io.Writer) error) error {
	err := write(tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); cerr != nil {
		return errors.Join(err, fmt.Errorf("close temporary file %s: %w", name, cerr))
	}

	return err
}

// removeTemp removes the temporary file name and returns the error, which names the file, of a removal that leaves it behind.
func removeTemp(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary file %s: %w", name, err)
	}

	return nil
}
