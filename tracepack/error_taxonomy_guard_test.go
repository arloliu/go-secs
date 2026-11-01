package tracepack

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// This file guards the public error taxonomy:
// each public call that can fail is driven into each fault class the package documents,
// and its error must match the class's public sentinels and none of the other families.
// The read faults of a pack's structures are guarded by TestReadFaultSentinels and TestReadFaultNeverRelabelsReadErrors,
// the writing calls' decisions by the TestWritingDecision tests; this file covers the rest.

// taxonomyFamilies are the public sentinels a guard row checks an error against, each expected or forbidden.
var taxonomyFamilies = []error{
	ErrCorrupt, ErrChecksum, ErrInvalidFooter, ErrNotFinalized, ErrNotTracepack, ErrUnsupportedFormat, ErrReadLimit,
	ErrInvalidArgument, ErrInvalidQuery, ErrInvalidKey, ErrNotPrimary,
	ErrRefused, ErrInvalidRecord, ErrWriterFailed, ErrClosed, ErrPublishUncertain, ErrSegmentExists, ErrSegmentConflict,
	ErrMergeInput, ErrMergeLimit, ErrMergeConflict, ErrRedactionDefect, ErrSourceRead,
}

// guardRow is one fault a public call reports: the call, and the errors its error must match.
// Of taxonomyFamilies, the error matches exactly those want names or that a sentinel of want is a member of.
type guardRow struct {
	name string
	err  func(t *testing.T) error
	want []error
}

// requireFamilies requires err to match each error of want and, of taxonomyFamilies, no family that want does not name,
// a parent of a sentinel of want being named by it.
func requireFamilies(t *testing.T, err error, want ...error) {
	t.Helper()

	require.Error(t, err)
	for _, w := range want {
		require.ErrorIs(t, err, w, "%v", err)
	}
	for _, f := range taxonomyFamilies {
		if slices.ContainsFunc(want, func(w error) bool { return errors.Is(w, f) }) {
			continue
		}
		assert.NotErrorIs(t, err, f, "%v", err)
	}
}

// runGuardRows runs each row as a parallel subtest.
func runGuardRows(t *testing.T, rows []guardRow) {
	t.Helper()

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			requireFamilies(t, row.err(t), row.want...)
		})
	}
}

// TestTaxonomyInvalidArgument drives every call that refuses an argument or option it cannot take:
// the error matches ErrInvalidArgument, or ErrInvalidQuery under it, and no fault class of a pack;
// a writing call also marks it ErrRefused, no other call does.
func TestTaxonomyInvalidArgument(t *testing.T) {
	t.Parallel()

	file := optionTestPack(t)
	mergeInput := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	merge := func(edit func(*MergeOptions), inputs []MergeInput) func(t *testing.T) error {
		return func(t *testing.T) error {
			opts := mergeOpts()
			if edit != nil {
				edit(&opts)
			}
			if inputs == nil {
				inputs = inputsOf(mergeInput)
			}
			_, err := Merge(t.Context(), io.Discard, inputs, opts)

			return err
		}
	}
	nilReaders := func(t *testing.T) *txReadersSource {
		t.Helper()
		r := mustOpen(t, txPack(t, seg0, nil, txBlock(txSeqs(12)...)), ReaderOptions{})

		return &txReadersSource{readers: []*Reader{r, nil}}
	}

	runGuardRows(t, []guardRow{
		{name: "Open: a negative reader budget", want: []error{ErrInvalidArgument}, err: func(t *testing.T) error {
			_, err := Open(t.Context(), bytes.NewReader(file), int64(len(file)), ReaderOptions{MaxBlockLen: -1})
			return err
		}},
		{name: "Verify: a negative reader budget", want: []error{ErrInvalidArgument}, err: func(t *testing.T) error {
			_, err := Verify(t.Context(), bytes.NewReader(file), int64(len(file)), VerifyOptions{Reader: ReaderOptions{MaxBlockLen: -1}})
			return err
		}},
		{name: "Merge: no inputs", want: []error{ErrInvalidArgument}, err: merge(nil, []MergeInput{})},
		{name: "Merge: an input without a ReaderAt", want: []error{ErrInvalidArgument},
			err: merge(nil, []MergeInput{inputsOf(mergeInput)[0], {Size: 1}})},
		{name: "Merge: an undefined compression level", want: []error{ErrInvalidArgument},
			err: merge(func(o *MergeOptions) { o.CompressionLevel = CompressionBest + 1 }, nil)},
		{name: "Merge: a negative reader budget", want: []error{ErrInvalidArgument},
			err: merge(func(o *MergeOptions) { o.Reader.MaxBlockLen = -1 }, nil)},
		{name: "Merge: a writer tag the pack metadata cannot hold", want: []error{ErrInvalidArgument, ErrFieldValue},
			err: merge(func(o *MergeOptions) { o.WriterTag = "\xff" }, nil)},
		{name: "Merge: a PackID an input carries", want: []error{ErrInvalidArgument},
			err: merge(func(o *MergeOptions) { o.PackID = seg0 }, nil)},
		{name: "FindTransaction: a nil reader from the source", want: []error{ErrInvalidQuery}, err: func(t *testing.T) error {
			_, err := FindTransaction(t.Context(), nilReaders(t), txKeyAt(12), TxOptions{MaxHours: 1})
			return err
		}},
		{name: "FindTransactions: a nil reader from the source", want: []error{ErrInvalidQuery}, err: func(t *testing.T) error {
			from := time.Unix(0, memTestHour*hourNs)
			_, err := FindTransactions(t.Context(), nilReaders(t), TxRange{CaptureID: captureLow, From: from, To: from.Add(time.Minute)},
				TxRangeOptions{Lookup: TxOptions{MaxHours: 1}}, func(*TxPairing) error { return nil })

			return err
		}},
		{name: "EncodeDescriptor: nil", want: []error{ErrInvalidArgument}, err: func(*testing.T) error {
			_, err := EncodeDescriptor(nil)
			return err
		}},
		{name: "ParseUUID: not a UUID", want: []error{ErrInvalidArgument}, err: func(*testing.T) error {
			_, err := ParseUUID("not a uuid")
			return err
		}},
		{name: "SegmentKey: a tool_id that is no key segment", want: []error{ErrInvalidKey}, err: func(*testing.T) error {
			_, err := SegmentKey("bucket", "..", UUID{1}, 0, UUID{2})
			return err
		}},
		{name: "NewDirSink: a prefix that is not a key prefix", want: []error{ErrInvalidArgument}, err: func(t *testing.T) error {
			_, err := NewDirSink(t.TempDir(), "a/../b", DirSinkOptions{})
			return err
		}},
		{name: "DirSink: a Write after the Commit", want: []error{ErrInvalidArgument}, err: func(t *testing.T) error {
			s, err := NewDirSink(t.TempDir(), dirTestPrefix, DirSinkOptions{})
			require.NoError(t, err)
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			require.NoError(t, f.Commit(t.Context()))
			_, err = f.Write([]byte("more"))

			return err
		}},
		{name: "DirSink: a Create after Close", want: []error{fs.ErrClosed}, err: func(t *testing.T) error {
			s, err := NewDirSink(t.TempDir(), dirTestPrefix, DirSinkOptions{})
			require.NoError(t, err)
			require.NoError(t, s.Close())
			_, err = s.Create(t.Context(), dirTestInfo(0, dirTestPackA))

			return err
		}},
		{name: "NewWriter: no pack metadata", want: []error{ErrInvalidArgument, ErrRefused}, err: func(*testing.T) error {
			_, err := NewWriter(io.Discard, WriterOptions{})
			return err
		}},
		{name: "SegmentWriter.Close: a Now() not representable", want: []error{ErrInvalidRecord, ErrRefused}, err: func(t *testing.T) error {
			w, _ := newSeg(t, func(o *SegmentWriterOptions) { o.Now = nowAt(time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)) })
			return w.Close(t.Context())
		}},
	})
}

// TestTaxonomyNoCrossClass drives faults whose class a neighbouring class could be mistaken for:
// a writing call's refusal of a payload that does not decode is no corrupt pack,
// and a caller's ReadAt error is no fault of the pack, through every call that reads.
func TestTaxonomyNoCrossClass(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	block := clean.Blocks()[1]
	failing := func() *gatedReader {
		g := newGatedReader(file)
		g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }

		return g
	}
	openFailing := func(t *testing.T) *Reader {
		t.Helper()
		r, err := Open(t.Context(), failing(), int64(len(file)), uncachedWindows())
		require.NoError(t, err)

		return r
	}
	key := TxKeyOf(UUID(clean.hdr.CaptureID), block.FirstSeq+1, time.Unix(0, block.TSMin))
	undecodable := undecodableEventPayloads(t)[0]

	runGuardRows(t, []guardRow{
		{name: "Writer.Append: an event payload that does not decode", want: []error{ErrInvalidRecord, ErrRefused}, err: func(t *testing.T) error {
			w := refusalWriter(t, io.Discard, nil)
			bad := eventPayloadRecord(undecodable.kind, 0, undecodable.payload)

			return w.Append(&bad)
		}},
		{name: "FindTransaction: a caller's ReadAt error", want: []error{errInjected, ErrSourceRead}, err: func(t *testing.T) error {
			src, err := NewReaderSource([]*Reader{openFailing(t)})
			require.NoError(t, err)
			_, err = FindTransaction(t.Context(), src, key, TxOptions{MaxHours: 1})

			return err
		}},
		{name: "FindTransactions: a caller's ReadAt error", want: []error{errInjected, ErrSourceRead}, err: func(t *testing.T) error {
			src, err := NewReaderSource([]*Reader{openFailing(t)})
			require.NoError(t, err)
			at := time.Unix(0, block.TSMin)
			_, err = FindTransactions(t.Context(), src, TxRange{CaptureID: key.CaptureID, From: at, To: at.Add(time.Nanosecond)},
				TxRangeOptions{Lookup: TxOptions{MaxHours: 1}}, func(*TxPairing) error { return nil })

			return err
		}},
		{name: "Merge: a caller's ReadAt error on a block", want: []error{errInjected, ErrSourceRead}, err: func(t *testing.T) error {
			inputs := inputsOf(file)
			inputs[0].ReaderAt = failing()
			_, err := Merge(t.Context(), io.Discard, inputs, uncachedMergeOpts())

			return err
		}},
	})
}

// TestTaxonomyDescriptiveErrors drives the errors the package documents as descriptive:
// those whose classifier is a field beside Err, and those that report a fault of the package itself;
// each matches none of the families, and its classifier says what it is.
// A caller's ReadAt that ends its goroutine is driven here too: it matches ErrSourceRead alone.
func TestTaxonomyDescriptiveErrors(t *testing.T) {
	t.Parallel()

	t.Run("a coverage defect", func(t *testing.T) {
		t.Parallel()

		file := coverageCapturePack(t, seg0, captureLow, []Coverage{{}})
		run := iterate(t, mustOpen(t, file, ReaderOptions{}), Query{})
		requireDescriptiveDefect(t, run.res.Incomplete, IncompleteReasonCoverage)
	})

	t.Run("a crash gap", func(t *testing.T) {
		t.Parallel()

		run := iterate(t, mustOpen(t, uncleanPack(t, seg0, captureLow).file, ReaderOptions{}), Query{})
		requireDescriptiveDefect(t, run.res.Incomplete, IncompleteReasonCrashGap)
	})

	t.Run("a lookup gap of a coverage entry", func(t *testing.T) {
		t.Parallel()

		cov := Coverage{CaptureID: new(captureLow), SeqFirst: new(uint64(14)), SeqLast: new(uint64(14))}
		own := txPack(t, seg0, nil, txBlock(txRecord(12, nil), txNote(13, nil), txNote(14, nil), txNote(15, nil), txRecord(16, nil)))
		foreign := withIDs(t, txPack(t, seg1, func(m *PackMeta) { m.Coverage = []Coverage{cov} }, nil), seg1, captureHigh)
		res, err := FindTransaction(t.Context(), txSource(t, own, foreign), txKeyAt(12), TxOptions{MaxHours: 1})
		require.NoError(t, err)
		require.Len(t, res.Incomplete, 1)
		g := res.Incomplete[0]
		assert.Equal(t, TxGapCoverage, g.Reason, "the reason classifies the gap")
		requireFamilies(t, g.Err)
	})

	t.Run("writer defects", func(t *testing.T) {
		t.Parallel()

		p := defectTestPack(t)
		seqOverlap := func(b *testBlock) {
			b.env.FirstSeq = 2
			b.headers[0].Seq = 2
		}
		hourSpan := func(b *testBlock) {
			b.headers[1].TSUTCNs = blockTestHour + hourNs
			b.patchEntry = func(e *format.F2Entry) { e.TSMax = blockTestHour + hourNs }
		}
		for _, tt := range []struct {
			kind WriterDefectKind
			file []byte
		}{
			{WriterDefectSeqOrder, editBlock(t, p.file, 1, seqOverlap)},
			{WriterDefectHourSpan, editBlock(t, p.file, 1, hourSpan)},
			{WriterDefectSeqStart, withSeqStart(t, defaultRepairPack(t).file, 9)},
		} {
			rep := mustVerify(t, tt.file)
			n := 0
			for _, d := range rep.WriterDefects {
				if d.Kind == tt.kind {
					requireFamilies(t, d.Err)
					n++
				}
			}
			require.Positive(t, n, "a %v defect: %v", tt.kind, rep.WriterDefects)
		}
	})

	t.Run("a block Merge appends that fails its own check", func(t *testing.T) {
		t.Parallel()

		// A defect of this package, never an input's or an option's: no input or option reaches it.
		w := refusalWriter(t, io.Discard, nil)
		_, err := w.checkAppendBlock([]byte{1, 2, 3}, &blockSummary{})
		requireFamilies(t, err)
	})

	t.Run("ReadAt ending its goroutine", func(t *testing.T) {
		t.Parallel()

		// A misbehaving ReadAt of the caller's, treated as its read failing: a failed read of the source,
		// no class of a pack, nor a refused merge input.
		file, first, _ := roundPack(t)
		r := newRoundReader(file)
		r.at(first[1]).goexit = true
		r.at(first[0])
		_, err := Open(t.Context(), r, int64(len(file)), roundOptions)
		requireFamilies(t, err, ErrSourceRead)

		files := openTestFiles(t, 2)
		rs, in := gatedInputs(files)
		rs[0].at(int64(len(files[0])) - format.TrailerLen).goexit = true
		opts := openTestOpts()
		opts.Reader = roundOptions
		opts.MaxParallelOpens = 1
		_, _, err = mergeBytes(t.Context(), in, opts)
		requireFamilies(t, err, ErrSourceRead)
	})
}

// TestTaxonomyDirSinkEnvironment drives the faults of a DirSink's environment, which retrying does not cure:
// each is a plain error, matching no family, not even ErrInvalidArgument, though the call's arguments name the place.
func TestTaxonomyDirSinkEnvironment(t *testing.T) {
	t.Parallel()

	runGuardRows(t, []guardRow{
		{name: ".partial a symbolic link", err: func(t *testing.T) error {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "bucket", "traces", "staging"), 0o700))
			dirSymlink(t, filepath.Join("bucket", "traces", "staging"), filepath.Join(root, partialDir))
			_, err := NewDirSink(root, dirTestPrefix, DirSinkOptions{})

			return err
		}},
		{name: "a key's directory a symbolic link", err: func(t *testing.T) error {
			root := t.TempDir()
			s, err := NewDirSink(root, "", DirSinkOptions{})
			require.NoError(t, err)
			dirSymlink(t, partialDir, filepath.Join(root, "staging"))
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			defer func() { require.NoError(t, f.Abort()) }()

			return f.Commit(t.Context())
		}},
		{name: "a directory at the key", err: func(t *testing.T) error {
			root := t.TempDir()
			s, err := NewDirSink(root, dirTestPrefix, DirSinkOptions{NoReplace: true})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(dirTestKey(t, 0, dirTestPackA))), 0o700))
			f := writeSegment(t, s, dirTestInfo(0, dirTestPackA), "segment")
			defer func() { require.NoError(t, f.Abort()) }()

			return f.Commit(t.Context())
		}},
		{name: "a prefix aliasing .partial on a file system that folds case, as modelled", err: func(t *testing.T) error {
			fsys := newRecordFS(t, t.TempDir())
			fsys.fsys = foldFS{fsys: fsys.fsys}
			_, err := newDirSinkFS(".PARTIAL/traces", fsys, DirSinkOptions{})

			return err
		}},
		{name: "a prefix aliasing .partial on this file system, when it folds case", err: func(t *testing.T) error {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "probe"), 0o700))
			if _, err := os.Stat(filepath.Join(root, "PROBE")); err != nil {
				t.Skip("the file system of the test does not fold case")
			}
			_, err := NewDirSink(root, ".PARTIAL/traces", DirSinkOptions{})

			return err
		}},
		{name: "NewDirSink on js", err: func(t *testing.T) error {
			if runtime.GOOS != "js" {
				t.Skip("NewDirSink fails only on js, which has no file system calls it needs")
			}
			_, err := NewDirSink(t.TempDir(), dirTestPrefix, DirSinkOptions{})

			return err
		}},
	})
}

// requireDescriptiveDefect requires ds to hold a defect of reason, whose Err matches none of the families.
func requireDescriptiveDefect(t *testing.T, ds []Defect, reason IncompleteReason) {
	t.Helper()

	n := 0
	for _, d := range ds {
		if d.Reason == reason {
			requireFamilies(t, d.Err)
			n++
		}
	}
	require.Positive(t, n, "a %v defect: %v", reason, ds)
}
