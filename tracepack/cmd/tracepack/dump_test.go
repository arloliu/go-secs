package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/corpus"
)

// The counts of the committed corpus that TestDumpCorpus covers;
// a change of the corpus updates them on purpose.
const (
	corpusVectors          = 123
	corpusExports          = 107
	corpusRejections       = 16
	corpusTruncationTables = 3
	corpusTruncationRows   = 16
)

// libraryExport opens pack, exports it through the library, as dump --jsonl should, and returns the read's Result.
func libraryExport(t *testing.T, pack []byte) tracepack.Result {
	t.Helper()
	r, err := tracepack.Open(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.NoError(t, err)
	res, err := tracepack.ExportJSONL(t.Context(), r, io.Discard)
	require.NoError(t, err)

	return res
}

// wantExit returns the exit code dump --jsonl owes an export whose read has res.
func wantExit(res tracepack.Result) int {
	if res.Complete() {
		return exitClean
	}

	return exitNotClean
}

// checkRejected checks that dump --jsonl, run with cfg on the pack at path whose bytes are pack,
// rejects it at bootstrap for the corpus's code: the code of Open's error on the same bytes, whose text stderr holds.
func checkRejected(t *testing.T, cfg config, path string, pack []byte, code string) {
	t.Helper()
	_, openErr := tracepack.Open(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.Error(t, openErr)
	got, err := corpus.RejectionCode(openErr)
	require.NoError(t, err)
	require.Equal(t, code, got)

	exit, stdout, stderr := runArgs(t, cfg, "dump", "--jsonl", path)
	assert.Equal(t, exitFailed, exit)
	assert.Empty(t, stdout)
	assert.Equal(t, "tracepack dump: "+path+": "+openErr.Error()+"\n", stderr)
}

// checkExported checks that dump --jsonl, run with cfg on the pack at path whose bytes are pack,
// writes want to stdout, one incomplete line per defect of the library's read to stderr, and exits as that read says.
func checkExported(t *testing.T, cfg config, path string, pack, want []byte) {
	t.Helper()
	res := libraryExport(t, pack)

	exit, stdout, stderr := runArgs(t, cfg, "dump", "--jsonl", path)
	assert.Equal(t, wantExit(res), exit, "stderr %s", stderr)
	assert.Equal(t, string(want), stdout)
	assert.Equal(t, len(res.Incomplete), strings.Count(stderr, "incomplete: "), "stderr %s", stderr)
}

// TestDumpCorpus runs dump --jsonl on every vector of the conformance corpus:
// every export golden byte for byte, every bootstrap rejection,
// and every cut of every truncation vector, rejected or exported as its row says.
func TestDumpCorpus(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(corpusDir, corpus.FileManifest))
	require.NoError(t, err)
	var m corpus.Manifest
	require.NoError(t, corpus.Unmarshal(raw, &m))

	var exports, rejections, tables, rows atomic.Int64
	// The counts are checked once every vector's subtest has finished.
	t.Cleanup(func() {
		assert.Len(t, m.Vectors, corpusVectors)
		assert.Equal(t, int64(corpusExports), exports.Load(), "vectors with an export")
		assert.Equal(t, int64(corpusRejections), rejections.Load(), "vectors rejected at bootstrap")
		assert.Equal(t, int64(corpusTruncationTables), tables.Load(), "truncation tables")
		assert.Equal(t, int64(corpusTruncationRows), rows.Load(), "truncation rows")
	})
	for _, v := range m.Vectors {
		t.Run(v.ID, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(corpusDir, v.ID)
			path := filepath.Join(dir, corpus.FilePack)
			pack, err := os.ReadFile(path)
			require.NoError(t, err)
			read := func(name string) []byte {
				b, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)

				return b
			}
			for _, name := range v.Files {
				switch name {
				case corpus.FileExport:
					exports.Add(1)
					checkExported(t, defaultConfig(), path, pack, read(name))
				case corpus.FileRejection:
					rejections.Add(1)
					var rej corpus.Rejection
					require.NoError(t, corpus.Unmarshal(read(name), &rej))
					checkRejected(t, defaultConfig(), path, pack, rej.Rejection)
				case corpus.FileTruncation:
					tables.Add(1)
					var tr corpus.Truncation
					require.NoError(t, corpus.Unmarshal(read(name), &tr))
					rows.Add(int64(len(tr.Rows)))
					checkCuts(t, pack, read(corpus.FileExport), &tr)
				default:
					// The pack, and the files of readings dump does not make, which the corpus's own test checks.
				}
			}
		})
	}
}

// checkCuts runs dump --jsonl on every cut pack[:L] of a truncation vector, served from memory:
// a cut is rejected for the code of its row,
// or exported as the header line of golden and its row's first exported_records lines.
func checkCuts(t *testing.T, pack, golden []byte, tr *corpus.Truncation) {
	t.Helper()
	require.Equal(t, uint64(len(pack)), uint64(tr.Size))
	lines := bytes.SplitAfter(golden, []byte("\n"))
	cuts := 0
	for _, row := range tr.Rows {
		for l := uint64(row.From); l <= uint64(row.To); l++ {
			cuts++
			path := "cut.tpk"
			cut := pack[:l]
			o := &countingOpener{mem: map[string][]byte{path: cut}}
			if row.Rejection != "" {
				checkRejected(t, o.config(), path, cut, row.Rejection)

				continue
			}
			require.NotNil(t, row.ExportedRecords)
			n := int(*row.ExportedRecords)
			require.Less(t, n, len(lines))
			checkExported(t, o.config(), path, cut, bytes.Join(lines[:1+n], nil))
			if t.Failed() {
				t.Fatalf("cut at %d of row %d..%d", l, row.From, row.To)
			}
		}
	}
	assert.Equal(t, len(pack), cuts, "every cut length below the pack's size")
}

// failingWriter accepts limit bytes, then fails every write with errWrite.
type failingWriter struct {
	limit int
	n     int
}

var errWrite = errors.New("write failed")

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n+len(p) <= w.limit {
		w.n += len(p)

		return len(p), nil
	}
	k := w.limit - w.n
	w.n = w.limit

	return k, errWrite
}

// shortWriter accepts limit bytes, then accepts fewer bytes than it is given without an error.
type shortWriter struct {
	limit int
	n     int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	k := min(len(p), w.limit-w.n)
	w.n += k

	return k, nil
}

func TestDumpStdoutFails(t *testing.T) {
	t.Parallel()

	for name, w := range map[string]io.Writer{
		"failing":     &failingWriter{limit: 100},
		"short write": &shortWriter{limit: 100},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			code := runWith(t.Context(), defaultConfig(), []string{"dump", "--jsonl", corpusPack("basic-codec-zstd")}, w, &stderr)
			assert.Equal(t, exitFailed, code)
			assert.Contains(t, stderr.String(), "the output written is not an export")
		})
	}
}

func TestDumpOutputFile(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"basic-codec-zstd", "basic-corrupt-middle"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			golden, err := os.ReadFile(filepath.Join(corpusDir, id, corpus.FileExport))
			require.NoError(t, err)
			pack, err := os.ReadFile(corpusPack(id))
			require.NoError(t, err)
			res := libraryExport(t, pack)

			dir := t.TempDir()
			out := filepath.Join(dir, "out.jsonl")
			require.NoError(t, os.WriteFile(out, []byte("old"), 0o600))
			code, stdout, _ := runArgs(t, defaultConfig(), "dump", "--jsonl", "-o", out, corpusPack(id))
			assert.Equal(t, wantExit(res), code, "an incomplete export is published too")
			assert.Empty(t, stdout)
			got, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Equal(t, string(golden), string(got))
			assertOnly(t, dir, "out.jsonl")
		})
	}
}

func TestDumpOutputFileMissingDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	out := filepath.Join(dir, "missing", "out.jsonl")
	code, stdout, stderr := runArgs(t, defaultConfig(), "dump", "--jsonl", "-o", out, corpusPack("basic-codec-zstd"))
	assert.Equal(t, exitFailed, code)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "missing")
	assertOnly(t, dir)
}

// TestDumpOutputFileExportFails runs dump --jsonl -o on a pack whose second block is over the reader's MaxBlockLen:
// the export fails after the first block's lines,
// and an existing file is left as it was, with no temporary file beside it.
func TestDumpOutputFileExportFails(t *testing.T) {
	t.Parallel()

	path := corpusPack("basic-codec-none")
	pack, err := os.ReadFile(path)
	require.NoError(t, err)
	r, err := tracepack.Open(t.Context(), bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
	require.NoError(t, err)
	// The pack is uncompressed, so a block body's length on disk is its decoded length.
	blocks := r.Blocks()
	require.GreaterOrEqual(t, len(blocks), 2)
	first := int64(blocks[0].UncompressedLen)
	require.GreaterOrEqual(t, indexLarger(blocks, first), 1, "a later block larger than the first")

	cfg := defaultConfig()
	cfg.reader.MaxBlockLen = first
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	require.NoError(t, os.WriteFile(out, []byte("old"), 0o600))
	code, _, stderr := runArgs(t, cfg, "dump", "--jsonl", "-o", out, path)
	assert.Equal(t, exitFailed, code)
	assert.Contains(t, stderr, tracepack.ErrReadLimit.Error())
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got))
	assertOnly(t, dir, "out.jsonl")

	// The same failure on stdout may leave part of the export written, a prefix of it, and says it is not an export.
	golden, err := os.ReadFile(filepath.Join(corpusDir, "basic-codec-none", corpus.FileExport))
	require.NoError(t, err)
	code, stdout, stderr := runArgs(t, cfg, "dump", "--jsonl", path)
	assert.Equal(t, exitFailed, code)
	assert.True(t, strings.HasPrefix(string(golden), stdout), "stdout is a prefix of the export")
	assert.Less(t, len(stdout), len(golden))
	assert.Contains(t, stderr, "the output written is not an export")
}

// indexLarger returns the index of the first block whose decoded length is above n, or -1.
func indexLarger(blocks []tracepack.BlockInfo, n int64) int {
	for k, b := range blocks {
		if int64(b.UncompressedLen) > n {
			return k
		}
	}

	return -1
}

// assertOnly asserts that dir holds exactly the entries names.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	assert.ElementsMatch(t, names, got)
}

// fakeTemp is a temporary file over a real file whose close can be made to fail.
type fakeTemp struct {
	*os.File
	closeErr error
}

func (f *fakeTemp) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}

	return err
}

func TestPublish(t *testing.T) {
	t.Parallel()

	errClose := errors.New("close failed")
	content := func(w io.Writer) error {
		_, err := io.WriteString(w, "new")

		return err
	}
	createWith := func(closeErr error) func(dir, pattern string) (*fakeTemp, error) {
		return func(dir, pattern string) (*fakeTemp, error) {
			f, err := os.CreateTemp(dir, pattern)
			if err != nil {
				return nil, err
			}

			return &fakeTemp{File: f, closeErr: closeErr}, nil
		}
	}

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "out")
		require.NoError(t, os.WriteFile(out, []byte("old"), 0o600))
		require.NoError(t, publish(out, createWith(nil), content))
		got, err := os.ReadFile(out)
		require.NoError(t, err)
		assert.Equal(t, "new", string(got))
		assertOnly(t, dir, "out")
	})
	t.Run("write fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "out")
		require.NoError(t, os.WriteFile(out, []byte("old"), 0o600))
		err := publish(out, createWith(nil), func(w io.Writer) error {
			_, _ = io.WriteString(w, "partial")

			return errWrite
		})
		require.ErrorIs(t, err, errWrite)
		got, err := os.ReadFile(out)
		require.NoError(t, err)
		assert.Equal(t, "old", string(got))
		assertOnly(t, dir, "out")
	})
	t.Run("close fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "out")
		require.NoError(t, os.WriteFile(out, []byte("old"), 0o600))
		require.ErrorIs(t, publish(out, createWith(errClose), content), errClose)
		got, err := os.ReadFile(out)
		require.NoError(t, err)
		assert.Equal(t, "old", string(got))
		assertOnly(t, dir, "out")
	})
	t.Run("rename fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out := filepath.Join(dir, "out")
		require.NoError(t, os.MkdirAll(filepath.Join(out, "inside"), 0o700))
		require.Error(t, publish(out, createWith(nil), content))
		assertOnly(t, dir, "out")
		assertOnly(t, out, "inside")
	})
	t.Run("create fails", func(t *testing.T) {
		t.Parallel()
		out := filepath.Join(t.TempDir(), "missing", "out")
		require.Error(t, publish(out, createWith(nil), content))
	})
}

func TestDumpStderrFails(t *testing.T) {
	t.Parallel()

	// The first pack's only diagnostic is a footer error, which alone exits 0; the second's read is incomplete, which exits 1.
	for _, id := range []string{"footer-invalid-count-sums", "basic-corrupt-middle"} {
		for name, newWriter := range map[string]func() io.Writer{
			"failing":     func() io.Writer { return &failingWriter{} },
			"short write": func() io.Writer { return &shortWriter{} },
		} {
			t.Run(id+"/"+name, func(t *testing.T) {
				t.Parallel()
				var stdout bytes.Buffer
				code := runWith(t.Context(), defaultConfig(), []string{"dump", "--jsonl", corpusPack(id)}, &stdout, newWriter())
				assert.Equal(t, exitFailed, code, "diagnostics that cannot be written")
			})
		}
	}
}
