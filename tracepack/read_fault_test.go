package tracepack

import (
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// readFaultCorpusPack is the corpus pack the read-fault tests damage:
// three zstd blocks of seqs 0-3, 4-7 and 8-11, of one capture within one UTC hour, finalized with a valid footer.
const readFaultCorpusPack = "testdata/corpus/basic-codec-zstd/pack.tpk"

// faultSentinels are the public sentinels a read-fault test checks an error against, each expected or forbidden:
// the read faults' own, and the classes a read fault must never match, a caller's mistake and a writing call's refusal.
var faultSentinels = []error{
	ErrCorrupt, ErrChecksum, ErrInvalidFooter, ErrNotFinalized, ErrNotTracepack, ErrUnsupportedFormat, ErrReadLimit, io.ErrUnexpectedEOF,
	ErrInvalidArgument, ErrRefused,
}

// pathErr is one error a read path reported for a fault, named by the path.
type pathErr struct {
	path string
	err  error
}

// readFaultPack reads the corpus pack of the read-fault tests and its clean Reader.
func readFaultPack(t testing.TB) ([]byte, *Reader) {
	t.Helper()

	file, err := os.ReadFile(readFaultCorpusPack)
	require.NoError(t, err)
	r := mustOpen(t, file, ReaderOptions{})
	require.True(t, r.Header().Finalized)
	require.NotNil(t, r.footer)
	require.Len(t, r.Blocks(), 3)

	return file, r
}

// requireSentinels requires that every error of errs matches each sentinel of want and, of faultSentinels, no other,
// and that each line of its text names the namespace once.
func requireSentinels(t *testing.T, errs []pathErr, want ...error) {
	t.Helper()

	require.NotEmpty(t, errs, "the fault reaches at least one path")
	for _, e := range errs {
		require.Error(t, e.err, e.path)
		requireNamespaceOnce(t, e.err)
		for _, s := range faultSentinels {
			wanted := false
			for _, w := range want {
				if errors.Is(w, s) {
					wanted = true
				}
			}
			if wanted {
				assert.ErrorIs(t, e.err, s, "%s: %v", e.path, e.err)
			} else {
				assert.NotErrorIs(t, e.err, s, "%s: %v", e.path, e.err)
			}
		}
	}
}

// openFaultErrs returns the errors with which Open, Verify and Merge refuse file,
// requiring Merge's to wrap ErrMergeInput.
func openFaultErrs(t *testing.T, file []byte) []pathErr {
	t.Helper()

	_, oerr := openBytes(t, file, ReaderOptions{})
	_, verr := verifyBytes(t, file, VerifyOptions{})
	_, merr := Merge(t.Context(), io.Discard, inputsOf(file), mergeOpts())
	require.ErrorIs(t, merr, ErrMergeInput)

	return []pathErr{{"Open", oerr}, {"Verify", verr}, {"Merge", merr}}
}

// footerFaultErrs returns the footer error every read path reports for file, which opens:
// PackHeader.FooterErr, the FooterErrs of Iterate, MergeIterate, FindTransaction and ExportJSONL, VerifyReport.FooterErr,
// and Merge's refusal of the pack, which also wraps ErrMergeInput.
func footerFaultErrs(t *testing.T, file []byte, key TxKey) []pathErr {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	errs := []pathErr{{"PackHeader.FooterErr", r.Header().FooterErr}}
	packErrs := func(path string, pes []PackError) {
		require.Len(t, pes, 1, path)
		errs = append(errs, pathErr{path, pes[0].Err})
	}

	res, err := r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })
	require.NoError(t, err)
	packErrs("Iterate", res.FooterErrs)

	_, res, err = collectMergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture})
	require.NoError(t, err)
	packErrs("MergeIterate", res.FooterErrs)

	res, err = ExportJSONL(t.Context(), r, io.Discard)
	require.NoError(t, err)
	packErrs("ExportJSONL", res.FooterErrs)

	tx := findFault(t, r, key)
	require.Len(t, tx.FooterErrs, 1)
	errs = append(errs, pathErr{"FindTransaction", tx.FooterErrs[0].Err})

	rep, err := verifyBytes(t, file, VerifyOptions{})
	require.NoError(t, err)
	errs = append(errs, pathErr{"Verify", rep.FooterErr})

	_, merr := Merge(t.Context(), io.Discard, inputsOf(file), mergeOpts())
	require.ErrorIs(t, merr, ErrMergeInput)

	return append(errs, pathErr{"Merge", merr})
}

// defectFaultErrs returns the errors of every defect of reason that a read path reports for file, which opens:
// Iterate's, MergeIterate's and ExportJSONL's Result, and FindTransaction's TxGapRead or TxGapIndex gaps;
// and, unless verifyReason is 0, those of reason verifyReason in Verify's VerifyReport.Failed or VerifyReport.Disagreements.
// With merge set, it also returns Merge's refusal of the pack, which wraps ErrMergeInput.
func defectFaultErrs(t *testing.T, file []byte, reason, verifyReason IncompleteReason, key TxKey, merge bool) []pathErr {
	t.Helper()

	r := mustOpen(t, file, ReaderOptions{})
	var errs []pathErr
	defects := func(path string, reason IncompleteReason, ds []Defect) {
		n := 0
		for _, d := range ds {
			if d.Reason == reason {
				errs = append(errs, pathErr{path, d.Err})
				n++
			}
		}
		require.Positive(t, n, "%s reports a %s defect: %v", path, reason, ds)
	}

	res, err := r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })
	require.NoError(t, err)
	defects("Iterate", reason, res.Incomplete)

	_, res, err = collectMergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture})
	require.NoError(t, err)
	defects("MergeIterate", reason, res.Incomplete)

	res, err = ExportJSONL(t.Context(), r, io.Discard)
	require.NoError(t, err)
	defects("ExportJSONL", reason, res.Incomplete)

	tx := findFault(t, r, key)
	n := 0
	for _, g := range tx.Incomplete {
		if (g.Reason == TxGapRead || g.Reason == TxGapIndex) && g.Defect == reason {
			errs = append(errs, pathErr{"FindTransaction", g.Err})
			n++
		}
	}
	require.Positive(t, n, "FindTransaction reports a %s gap: %v", reason, tx.Incomplete)

	if verifyReason != 0 {
		rep, err := verifyBytes(t, file, VerifyOptions{})
		require.NoError(t, err)
		defects("Verify", verifyReason, append(rep.Failed, rep.Disagreements...))
	}

	if merge {
		_, merr := Merge(t.Context(), io.Discard, inputsOf(file), mergeOpts())
		require.ErrorIs(t, merr, ErrMergeInput)
		errs = append(errs, pathErr{"Merge", merr})
	}

	return errs
}

// findFault looks up key over r alone, whose read fails only by its defects.
func findFault(t *testing.T, r *Reader, key TxKey) TxResult {
	t.Helper()

	src, err := NewReaderSource([]*Reader{r})
	require.NoError(t, err)
	res, err := FindTransaction(t.Context(), src, key, TxOptions{MaxHours: 1})
	if err != nil {
		require.ErrorIs(t, err, ErrNotPrimary, "a lookup fails only for a key whose record is found")
	}

	return res
}

// TestReadFaultSentinels damages each structure of a corpus pack and requires that every read path the fault reaches
// reports it with the same public sentinel, beside its cause, and with no other of faultSentinels.
func TestReadFaultSentinels(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	l := layoutOf(t, file)
	blocks := clean.Blocks()
	trailerAt := uint64(l.size - format.TrailerLen)
	capture := UUID(clean.hdr.CaptureID)
	// keyIn returns the key of a seq of block i, which the fault of that block hides.
	keyIn := func(i int) TxKey {
		return TxKeyOf(capture, blocks[i].FirstSeq+1, time.Unix(0, blocks[i].TSMin))
	}

	t.Run("pack metadata", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			file []byte
			want []error
		}{
			{name: "file header CRC mismatch", file: flipByte(file, 40), want: []error{ErrChecksum}},
			{name: "CRC mismatch", file: flipByte(file, format.FileHeaderLen+2), want: []error{ErrChecksum}},
			{name: "TLV that does not decode, its CRC fixed", file: patchMeta(t, file, func(m []byte) { m[0], m[1] = 0, 0 }), want: []error{ErrCorrupt}},
			{
				name: "past the end of the object",
				file: patchHeader(t, file, func(h *format.FileHeader) { h.PackMetadataLen = uint32(l.size) }),
				want: []error{ErrCorrupt, io.ErrUnexpectedEOF},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				requireSentinels(t, openFaultErrs(t, tt.file), tt.want...)
			})
		}
	})

	t.Run("trailer and footer", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			file []byte
			want []error
		}{
			{name: "trailer CRC mismatch", file: flipByte(file, trailerAt+30), want: []error{ErrNotFinalized, ErrChecksum}},
			{name: "trailer magic absent", file: flipByte(file, trailerAt+60), want: []error{ErrNotFinalized}},
			{
				name: "trailer_version 2, its CRC fixed",
				file: patchTrailer(t, file, func(tr *format.Trailer) { tr.TrailerVersion = 2 }),
				want: []error{ErrNotFinalized, ErrCorrupt},
			},
			{
				name: "trailer record_count over the format limit, its CRC fixed",
				file: patchTrailer(t, file, func(tr *format.Trailer) { tr.RecordCount = format.MaxU64 + 1 }),
				want: []error{ErrNotFinalized, ErrCorrupt},
			},
			{
				name: "trailer footer not ending at the trailer",
				file: patchTrailer(t, file, func(tr *format.Trailer) { tr.FooterLen-- }),
				want: []error{ErrNotFinalized, ErrCorrupt},
			},
			{name: "no trailer", file: file[:l.tr.FooterOffset], want: []error{ErrNotFinalized}},
			{name: "footer CRC mismatch", file: flipByte(file, l.tr.FooterOffset+5), want: []error{ErrChecksum}},
			{name: "footer that fails validation", file: invalidFooterFile(t, file), want: []error{ErrInvalidFooter}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				requireSentinels(t, footerFaultErrs(t, tt.file, keyIn(2)), tt.want...)
			})
		}
	})

	t.Run("block", func(t *testing.T) {
		t.Parallel()

		at := blocks[1].Offset
		tests := []struct {
			name   string
			file   []byte
			reason IncompleteReason
			want   error
		}{
			{name: "body CRC mismatch", file: flipByte(file, at+format.EnvelopeLen+3), reason: IncompleteReasonCorruptBlock, want: ErrChecksum},
			{name: "envelope CRC mismatch", file: flipByte(file, at+12), reason: IncompleteReasonCorruptBlock, want: ErrChecksum},
			{
				name: "envelope disagreeing with its F-2 entry, its CRC fixed",
				file: patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.FirstSeq++ }), reason: IncompleteReasonCorruptBlock, want: ErrCorrupt,
			},
			{
				name: "unknown codec, its CRC fixed",
				file: patchEnvelope(t, file, at, func(e *format.BlockEnvelope) { e.Codec = 9 }), reason: IncompleteReasonUnknownCodec, want: ErrCorrupt,
			},
			{
				name: "records disagreeing with the F-2 entry",
				file: editBlock(t, file, 1, func(b *testBlock) { b.headers[3].TSUTCNs++ }), reason: IncompleteReasonIndexMismatch, want: ErrCorrupt,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				requireSentinels(t, defectFaultErrs(t, tt.file, tt.reason, tt.reason, keyIn(1), true), tt.want)
			})
		}
	})

	t.Run("F-3 summary disagreeing with the records", func(t *testing.T) {
		t.Parallel()

		damaged := resummarized(t, file, clean.footer.blocks, 1, func(s *blockSummary) {
			s.qualityUnion |= QualityOrderingUncertain
		})
		rep, err := verifyBytes(t, damaged, VerifyOptions{})
		require.NoError(t, err)
		require.Len(t, rep.Disagreements, 1)
		_, merr := Merge(t.Context(), io.Discard, inputsOf(damaged), mergeOpts())
		require.ErrorIs(t, merr, ErrMergeInput)
		requireSentinels(t, []pathErr{{"Verify", rep.Disagreements[0].Err}, {"Merge", merr}}, ErrCorrupt)
	})

	t.Run("forward walk", func(t *testing.T) {
		t.Parallel()

		walked := invalidFooterFile(t, file)
		at := blocks[2].Offset
		// Each fault is an IncompleteReasonTruncated defect of the reads;
		// Verify lists a walk stop among Failed with the same reason, a walk disagreeing with the trailer among Disagreements,
		// and the end of a pack that is not finalized in its outcome alone.
		tests := []struct {
			name   string
			file   []byte
			verify IncompleteReason
			want   []error
		}{
			{name: "stop at a bad envelope CRC", file: flipByte(walked, at+12), verify: IncompleteReasonTruncated, want: []error{ErrChecksum}},
			{name: "stop at a bad envelope magic", file: flipByte(walked, at), verify: IncompleteReasonTruncated, want: []error{ErrCorrupt}},
			{
				name:   "stop at a block crossing a valid trailer's footer_offset",
				file:   patchEnvelope(t, walked, at, func(e *format.BlockEnvelope) { e.BodyLen += 1 << 10 }),
				verify: IncompleteReasonTruncated, want: []error{ErrCorrupt},
			},
			{
				name:   "walk disagreeing with the trailer",
				file:   patchTrailer(t, walked, func(tr *format.Trailer) { tr.BlockCount++ }),
				verify: IncompleteReasonIndexMismatch, want: []error{ErrCorrupt},
			},
			{
				name: "stop at a block the object's end cuts short", file: file[:at+format.EnvelopeLen+5],
				verify: IncompleteReasonTruncated, want: []error{ErrNotFinalized},
			},
			{name: "end of a pack that is not finalized", file: file[:at], want: []error{ErrNotFinalized}},
			// The pack was finished, so it is damaged, not still being written: retrying it would wait forever.
			{name: "end of the walk of a valid trailer whose footer fails its CRC", file: flipByte(file, l.tr.FooterOffset+5), want: []error{ErrCorrupt}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				requireSentinels(t, defectFaultErrs(t, tt.file, IncompleteReasonTruncated, tt.verify, keyIn(2), false), tt.want...)
			})
		}
	})
}

// TestReadFaultNeverRelabelsReadErrors requires that a caller's ReadAt error, io.ErrUnexpectedEOF of a short read included,
// and a reader budget are returned as they are, never as ErrCorrupt.
func TestReadFaultNeverRelabelsReadErrors(t *testing.T) {
	t.Parallel()

	file, clean := readFaultPack(t)
	block := clean.Blocks()[1]

	t.Run("Open", func(t *testing.T) {
		t.Parallel()

		g := newGatedReader(file)
		g.fail = func(int64, int) bool { return true }
		_, err := Open(t.Context(), g, int64(len(file)), ReaderOptions{})
		require.ErrorIs(t, err, errInjected)
		requireNotCorrupt(t, err)
	})

	t.Run("short read", func(t *testing.T) {
		t.Parallel()

		_, err := Open(t.Context(), &shortReaderAt{data: file}, int64(len(file)), ReaderOptions{})
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		requireNotCorrupt(t, err)
	})

	reads := map[string]func(r *Reader) error{
		"Iterate": func(r *Reader) error {
			_, err := r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })
			return err
		},
		"MergeIterate": func(r *Reader) error {
			_, _, err := collectMergeIterate(t.Context(), []*Reader{r}, Query{}, MergeIterateOptions{Order: OrderCapture})
			return err
		},
		"ExportJSONL": func(r *Reader) error {
			_, err := ExportJSONL(t.Context(), r, io.Discard)
			return err
		},
		"Verify": func(r *Reader) error {
			_, err := r.analyze(t.Context())
			return err
		},
	}
	for name, read := range reads {
		t.Run(name+" block read", func(t *testing.T) {
			t.Parallel()

			g := newGatedReader(file)
			g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
			r, err := Open(t.Context(), g, int64(len(file)), uncachedWindows())
			require.NoError(t, err)
			err = read(r)
			require.ErrorIs(t, err, errInjected)
			requireNotCorrupt(t, err)
		})
	}

	t.Run("Merge block read", func(t *testing.T) {
		t.Parallel()

		inputs := inputsOf(file)
		g := newGatedReader(file)
		g.fail = func(off int64, _ int) bool { return off == int64(block.Offset) }
		inputs[0].ReaderAt = g
		_, err := Merge(t.Context(), io.Discard, inputs, uncachedMergeOpts())
		require.ErrorIs(t, err, errInjected)
		requireNotCorrupt(t, err)
	})

	t.Run("block over MaxBlockLen", func(t *testing.T) {
		t.Parallel()

		r := mustOpen(t, file, ReaderOptions{MaxBlockLen: int64(block.UncompressedLen) - 1})
		res, err := r.Iterate(t.Context(), Query{}, func(*Item) error { return nil })
		require.NoError(t, err)
		require.NotEmpty(t, res.Incomplete)
		for _, d := range res.Incomplete {
			require.Equal(t, IncompleteReasonLimit, d.Reason)
			require.ErrorIs(t, d.Err, ErrReadLimit)
			requireNotCorrupt(t, d.Err)
		}
	})
}

// TestParseFileHeaderCapsMetadataBudget opens a header whose pack_metadata_len is 2^31 under a raised MaxPackMetadataLen:
// where int is 32 bits no such length is addressable, so the budget is capped and the header refused with ErrReadLimit;
// where int is 64 bits the cap lies above every u32 length, and the header passes.
func TestParseFileHeaderCapsMetadataBudget(t *testing.T) {
	t.Parallel()

	file, _ := readFaultPack(t)
	head := patchHeader(t, file, func(h *format.FileHeader) { h.PackMetadataLen = 1 << 31 })
	r := &Reader{size: 3 << 30, opts: ReaderOptions{MaxPackMetadataLen: 4 << 30}.withDefaults()}
	err := r.parseFileHeader(head)
	if strconv.IntSize == 32 {
		require.ErrorIs(t, err, ErrReadLimit)
		requireNotCorrupt(t, err)

		return
	}
	require.NoError(t, err)
	assert.Greater(t, int64(math.MaxInt-format.FileHeaderLen), int64(math.MaxUint32))
}

// TestOpenCapsMetadataBudget opens, where int is 32 bits, an object over 2 GiB whose pack_metadata_len is 2^31,
// under a raised MaxPackMetadataLen: Open fails with ErrReadLimit before allocating the pack metadata.
// Where int is 64 bits it is skipped, since such a length passes and Open would allocate 2 GiB for it.
func TestOpenCapsMetadataBudget(t *testing.T) {
	t.Parallel()

	if strconv.IntSize != 32 {
		t.Skip("the pack metadata budget is capped only where int is 32 bits")
	}

	file, _ := readFaultPack(t)
	head := patchHeader(t, file, func(h *format.FileHeader) { h.PackMetadataLen = 1 << 31 })[:format.FileHeaderLen]
	ra := sparseReaderAt{head: head}
	_, err := Open(t.Context(), ra, 3<<30, ReaderOptions{MaxPackMetadataLen: 4 << 30})
	require.ErrorIs(t, err, ErrReadLimit)
	requireNotCorrupt(t, err)
}

// requireNotCorrupt requires that err matches no class of a damaged, foreign or unfinished pack:
// neither ErrCorrupt nor ErrChecksum, ErrNotTracepack or ErrNotFinalized.
func requireNotCorrupt(t *testing.T, err error) {
	t.Helper()

	for _, class := range []error{ErrCorrupt, ErrChecksum, ErrNotTracepack, ErrNotFinalized} {
		require.NotErrorIs(t, err, class, "%v", err)
	}
}

// sparseReaderAt is an io.ReaderAt over an object whose first bytes are head and whose other bytes are zero.
type sparseReaderAt struct {
	head []byte
}

// ReadAt fills p with the object's bytes at off.
func (s sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	if off < int64(len(s.head)) {
		copy(p, s.head[off:])
	}

	return len(p), nil
}

var _ io.ReaderAt = sparseReaderAt{}
