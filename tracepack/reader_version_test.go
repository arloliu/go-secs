package tracepack

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionExport returns the canonical JSONL export of file and the Result ExportJSONL returns with it.
func versionExport(t *testing.T, file []byte) ([]byte, Result) {
	t.Helper()

	var out bytes.Buffer
	res, err := ExportJSONL(t.Context(), mustOpen(t, file, ReaderOptions{}), &out)
	require.NoError(t, err)

	return out.Bytes(), res
}

// versionIterate reads every record of file with Iterate and returns their seqs and the Result.
func versionIterate(t *testing.T, file []byte) ([]uint64, Result) {
	t.Helper()

	var seqs []uint64
	res, err := mustOpen(t, file, ReaderOptions{}).Iterate(t.Context(), Query{}, func(it *Item) error {
		seqs = append(seqs, it.Record.Seq)
		return nil
	})
	require.NoError(t, err)

	return seqs, res
}

// versionSeqs returns the seqs of recs.
func versionSeqs(recs []Record) []uint64 {
	out := make([]uint64, len(recs))
	for i := range recs {
		out[i] = recs[i].Seq
	}

	return out
}

// TestUnknownTrailerVersion checks that a trailer_version other than 1 makes the trailer not valid
// (the tracepack format specification §11, §14):
// the pack opens, is not finalized, and is read by the forward walk (§13);
// with no valid trailer to end the block region at footer_offset,
// the walk stops at the footer, which it cannot account for as a block.
func TestUnknownTrailerVersion(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 40)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	footerAt := int64(layoutOf(t, p.file).tr.FooterOffset)
	wantExport, _ := versionExport(t, p.file)
	require.Equal(t, 1+len(recs), bytes.Count(wantExport, []byte("\n")), "a header line and one line per record")

	for _, version := range []uint16{0, 2, 0xFFFF} {
		t.Run(versionName(version), func(t *testing.T) {
			t.Parallel()

			file := patchTrailer(t, p.file, func(tr *format.Trailer) { tr.TrailerVersion = version })

			r := mustOpen(t, file, ReaderOptions{})
			h := r.Header()
			assert.False(t, h.Finalized)
			assert.Nil(t, h.Trailer)
			require.Error(t, h.FooterErr, "the footer was not used")
			assert.Equal(t, walkedInfos(p.blocks), r.Blocks())

			seqs, res := versionIterate(t, file)
			assert.Equal(t, versionSeqs(recs), seqs, "every walked record is read")
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, IncompleteReasonTruncated, res.Incomplete[0].Reason)
			assert.Equal(t, []int64{footerAt}, defectOffsets(t, res.Incomplete), "the read is truncated at the footer")
			require.Error(t, soleFooterErr(t, res))

			rep, err := verifyBytes(t, file, VerifyOptions{})
			require.NoError(t, err)
			assert.Equal(t, VerifyOutcomeUnfinalized, rep.Outcome)
			assert.False(t, rep.Finalized)
			require.Error(t, rep.FooterErr)
			assert.Equal(t, len(p.blocks), rep.Validated)
			assert.Equal(t, uint64(len(recs)), rep.Records)
			assert.Equal(t, []int64{footerAt}, defectOffsets(t, rep.Failed), "the walk stops at the footer")

			export, exportRes := versionExport(t, file)
			assert.Equal(t, wantExport, export, "the export holds every walked record")
			assert.Equal(t, []int64{footerAt}, defectOffsets(t, exportRes.Incomplete))
		})
	}
}

// TestUnknownFooterLayoutVersion checks that a footer_layout_version other than 1 makes the footer not valid
// (the tracepack format specification §10, §14):
// the pack stays finalized, its blocks come from the forward walk to footer_offset (§13),
// and the walk is compared with the trailer's block_count and record_count,
// so the read is complete when they agree and truncated when they do not.
func TestUnknownFooterLayoutVersion(t *testing.T) {
	t.Parallel()

	recs := hourRecords(3, 40)
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, recs)
	_, decoded, _ := splitPack(t, p.file)
	wantExport, _ := versionExport(t, p.file)
	require.Equal(t, 1+len(recs), bytes.Count(wantExport, []byte("\n")), "a header line and one line per record")

	for _, version := range []uint16{0, 2} {
		t.Run(versionName(version), func(t *testing.T) {
			t.Parallel()

			file := refooter(t, p.file, patchPrologue(t, decoded, func(pro *format.FooterPrologue) {
				pro.FooterLayoutVersion = version
			}))

			t.Run("totals agree", func(t *testing.T) {
				t.Parallel()

				r := mustOpen(t, file, ReaderOptions{})
				h := r.Header()
				assert.True(t, h.Finalized, "finalization depends on the footer CRC, not on the footer's content")
				require.NotNil(t, h.Trailer)
				require.ErrorIs(t, h.FooterErr, ErrInvalidFooter)
				assert.Equal(t, walkedInfos(p.blocks), r.Blocks())

				seqs, res := versionIterate(t, file)
				assert.Equal(t, versionSeqs(recs), seqs)
				assert.Empty(t, res.Incomplete, "the walk accounts for the trailer's blocks and records")
				require.ErrorIs(t, soleFooterErr(t, res), ErrInvalidFooter)

				rep, err := verifyBytes(t, file, VerifyOptions{})
				require.NoError(t, err)
				assert.Equal(t, VerifyOutcomeFinalizedInconsistent, rep.Outcome, "the footer is invalid")
				assert.True(t, rep.Finalized)
				assert.Empty(t, rep.Failed)
				assert.Empty(t, rep.Disagreements)
				assert.Equal(t, uint64(len(recs)), rep.Records)

				export, exportRes := versionExport(t, file)
				assert.Equal(t, wantExport, export)
				assert.Empty(t, exportRes.Incomplete)
			})

			t.Run("record_count disagrees", func(t *testing.T) {
				t.Parallel()

				short := patchTrailer(t, file, func(tr *format.Trailer) { tr.RecordCount-- })

				seqs, res := versionIterate(t, short)
				assert.Equal(t, versionSeqs(recs), seqs)
				assert.Equal(t, []int64{-1}, defectOffsets(t, res.Incomplete), "the walk disagrees with the trailer")

				rep, err := verifyBytes(t, short, VerifyOptions{})
				require.NoError(t, err)
				assert.Equal(t, VerifyOutcomeFinalizedInconsistent, rep.Outcome)
				assert.Empty(t, rep.Failed)
				require.Len(t, rep.Disagreements, 1)
				assert.Equal(t, -1, rep.Disagreements[0].Block)
			})
		})
	}
}

// TestFooterZstdWindowBound checks that a footer the Writer encodes with zstd
// declares a window no larger than the larger of 8 MiB and its decoded length, and never one above 64 MiB
// (the tracepack format specification §2).
// The footer exceeds 8 MiB, so its frame declares a window rather than its content size,
// and the window is read by the zstd library's own header parser.
func TestFooterZstdWindowBound(t *testing.T) {
	t.Parallel()

	// Each single-record block adds about 250 bytes to the footer.
	recs := make([]Record, 36000)
	for i := range recs {
		recs[i] = testDataRecord(uint64(i), blockTestHour+int64(i), 1)
	}
	p := writeReaderPack(t, readerPackConfig{codec: CodecZstd, threshold: 1}, recs)
	tr := layoutOf(t, p.file).tr
	require.Equal(t, uint8(CodecZstd), tr.FooterCodec)
	require.Greater(t, tr.FooterUncompressedLen, uint64(8<<20), "the footer must exceed 8 MiB")

	var h zstd.Header
	require.NoError(t, h.Decode(p.file[tr.FooterOffset:]))
	window := h.WindowSize
	if h.SingleSegment {
		window = h.FrameContentSize
	}
	assert.LessOrEqual(t, window, max(uint64(8<<20), tr.FooterUncompressedLen))
	assert.LessOrEqual(t, window, uint64(64<<20))
}

// versionName names the subtest of a version value.
func versionName(v uint16) string {
	return "version " + strconv.FormatUint(uint64(v), 10)
}
