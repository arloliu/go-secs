package main

import (
	"bufio"
	"context"
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
// write writes a temporary file that create makes in path's directory;
// the file is renamed over path when write and the close succeed, and removed otherwise, leaving an existing path as it was.
//
// Parameters:
//   - path: the file to publish.
//   - create: makes the temporary file, as os.CreateTemp.
//   - write: writes the file's content.
//
// Returns:
//   - error: the error of create, write, the close or the rename.
func publish[F tempFile](path string, create func(dir, pattern string) (F, error), write func(io.Writer) error) error {
	tmp, err := create(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(name)
		}
	}()

	if err := write(tmp); err != nil {
		_ = tmp.Close()

		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	published = true

	return nil
}
