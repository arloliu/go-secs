package corpus

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

func TestVerifyFromConstructedReport(t *testing.T) {
	t.Parallel()

	captureID := testCaptureID
	defects := tracepack.VerifyReport{
		Outcome: tracepack.OutcomeCorruptMiddle, Finalized: true, FooterErr: errors.New("footer"),
		Blocks: 5, Validated: 3, Records: 9, PrefixEnd: 500,
		// Out of file order, with the walk stop between failed blocks.
		Failed: []tracepack.Defect{
			{Reason: tracepack.ReasonUnknownCodec, Block: 3, Offset: 800},
			{Reason: tracepack.ReasonTruncated, Block: -1, Offset: 1200},
			{Reason: tracepack.ReasonCorruptBlock, Block: 1, Offset: 500},
		},
		// Block 2 disagrees with its F-2 entry and its F-3 summary; the trailer's totals disagree.
		Disagreements: []tracepack.Defect{
			{Reason: tracepack.ReasonIndexMismatch, Block: 4, Offset: 1000},
			{Reason: tracepack.ReasonIndexMismatch, Block: 2, Offset: 600},
			{Reason: tracepack.ReasonIndexMismatch, Block: 2, Offset: 600},
			{Reason: tracepack.ReasonIndexMismatch, Block: -1, Offset: -1},
		},
		WriterDefects: []tracepack.WriterDefect{
			{Kind: tracepack.WriterDefectHourSpan, Block: 2, Offset: 600, Seq: 20},
			{Kind: tracepack.WriterDefectEventPayload, Block: 2, Offset: 600, Seq: 22},
			{Kind: tracepack.WriterDefectFieldValidity, Block: 2, Offset: 600, Seq: 21},
			{Kind: tracepack.WriterDefectEventFieldValidity, Block: 2, Offset: 600, Seq: 22},
			{Kind: tracepack.WriterDefectSeqOrder, Block: 2, Offset: 600, Seq: 20},
			{Kind: tracepack.WriterDefectFieldValidity, Block: 0, Offset: 300, Seq: 3},
			{Kind: tracepack.WriterDefectSeqStart, Block: 0, Offset: 300, Seq: 2},
		},
		Lost: []tracepack.Coverage{
			{CaptureID: &captureID, SeqFirst: new(uint64(30)), SeqLast: new(uint64(30)), TimeStart: new(int64(-5)), TimeEnd: new(int64(5))},
			{CaptureID: &captureID, SeqFirst: new(uint64(10)), SeqLast: new(uint64(12)), TimeStart: new(int64(1)), TimeEnd: new(int64(2))},
		},
	}
	v, err := VerifyFrom(&defects)
	require.NoError(t, err)
	require.Equal(t, `{
  "outcome": "corrupt-middle",
  "finalized": true,
  "footer_valid": false,
  "blocks_located": 5,
  "blocks_validated": 3,
  "records": "9",
  "prefix_end": "500",
  "failed_blocks": [
    {
      "block": 1,
      "offset": "500",
      "cause": "corrupt-block"
    },
    {
      "block": 3,
      "offset": "800",
      "cause": "unknown-codec"
    }
  ],
  "walk_stop": "1200",
  "disagreeing_blocks": [
    {
      "block": 2,
      "offset": "600"
    },
    {
      "block": 4,
      "offset": "1000"
    }
  ],
  "trailer_totals_disagree": true,
  "writer_defects": [
    {
      "kind": "seq-start",
      "block": 0,
      "offset": "300",
      "seq": "2"
    },
    {
      "kind": "field-validity",
      "block": 0,
      "offset": "300",
      "seq": "3"
    },
    {
      "kind": "seq-order",
      "block": 2,
      "offset": "600",
      "seq": "20"
    },
    {
      "kind": "field-validity",
      "block": 2,
      "offset": "600",
      "seq": "21"
    },
    {
      "kind": "event-field-validity",
      "block": 2,
      "offset": "600",
      "seq": "22"
    },
    {
      "kind": "event-payload",
      "block": 2,
      "offset": "600",
      "seq": "22"
    },
    {
      "kind": "hour-span",
      "block": 2,
      "offset": "600",
      "seq": "20"
    }
  ],
  "lost": [
    {
      "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
      "seq_first": "10",
      "seq_last": "12",
      "time_start": "1",
      "time_end": "2"
    },
    {
      "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
      "seq_first": "30",
      "seq_last": "30",
      "time_start": "-5",
      "time_end": "5"
    }
  ]
}
`, marshalJSON(t, &v))
}

func TestVerifyFromRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rep  tracepack.VerifyReport
	}{
		{"reader limit", tracepack.VerifyReport{Failed: []tracepack.Defect{{Reason: tracepack.ReasonLimit, Block: -1, Offset: 80}}}},
		{"two walk stops", tracepack.VerifyReport{Failed: []tracepack.Defect{
			{Reason: tracepack.ReasonTruncated, Block: -1, Offset: 80}, {Reason: tracepack.ReasonTruncated, Block: -1, Offset: 90},
		}}},
		{"failed block without offset", tracepack.VerifyReport{Failed: []tracepack.Defect{{Reason: tracepack.ReasonCorruptBlock, Block: 0, Offset: -1}}}},
		{"failed block of another reason", tracepack.VerifyReport{Failed: []tracepack.Defect{{Reason: tracepack.ReasonIndexMismatch, Block: 0, Offset: 80}}}},
		{"disagreement of another reason", tracepack.VerifyReport{Disagreements: []tracepack.Defect{{Reason: tracepack.ReasonCorruptBlock, Block: 0, Offset: 80}}}},
		{"two totals disagreements", tracepack.VerifyReport{Disagreements: []tracepack.Defect{
			{Reason: tracepack.ReasonIndexMismatch, Block: -1, Offset: -1}, {Reason: tracepack.ReasonIndexMismatch, Block: -1, Offset: -1},
		}}},
		{"undefined writer defect", tracepack.VerifyReport{WriterDefects: []tracepack.WriterDefect{{Kind: 0, Block: 0, Offset: 80}}}},
		{"writer defect without a block", tracepack.VerifyReport{WriterDefects: []tracepack.WriterDefect{{Kind: tracepack.WriterDefectHourSpan, Block: -1, Offset: 80}}}},
		{"lost entry without seq_first", tracepack.VerifyReport{Lost: []tracepack.Coverage{{SeqLast: new(uint64(1))}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := VerifyFrom(&tt.rep)
			require.Error(t, err)
		})
	}
}

func TestVerifyEmptyPack(t *testing.T) {
	t.Parallel()

	pack := writePack(t, testMeta(), nil, false)
	rep := verifyPack(t, pack)
	v, err := VerifyFrom(&rep)
	require.NoError(t, err)

	end := strconv.FormatUint(metaEnd(pack), 10)
	require.Equal(t, uint64(len(pack)), metaEnd(pack), "an empty pack is the file header and the pack metadata")
	require.Equal(t, `{
  "outcome": "unfinalized",
  "finalized": false,
  "footer_valid": false,
  "blocks_located": 0,
  "blocks_validated": 0,
  "records": "0",
  "prefix_end": "`+end+`",
  "failed_blocks": [],
  "disagreeing_blocks": [],
  "trailer_totals_disagree": false,
  "writer_defects": [],
  "lost": []
}
`, marshalJSON(t, &v))
}

func TestVerifyUnfinalizedEveryBlockRead(t *testing.T) {
	t.Parallel()

	b0 := []tracepack.Record{dataRecord(0, 1, []byte("ab")), dataRecord(1, 1, nil)}
	b1 := []tracepack.Record{dataRecord(2, 1, []byte("cde"))}
	pack := writePack(t, testMeta(), [][]tracepack.Record{b0, b1}, false)
	rep := verifyPack(t, pack)
	v, err := VerifyFrom(&rep)
	require.NoError(t, err)

	end := metaEnd(pack) + blockLen(b0) + blockLen(b1)
	require.Equal(t, uint64(len(pack)), end)
	require.Equal(t, `{
  "outcome": "unfinalized",
  "finalized": false,
  "footer_valid": false,
  "blocks_located": 2,
  "blocks_validated": 2,
  "records": "3",
  "prefix_end": "`+strconv.FormatUint(end, 10)+`",
  "failed_blocks": [],
  "disagreeing_blocks": [],
  "trailer_totals_disagree": false,
  "writer_defects": [],
  "lost": []
}
`, marshalJSON(t, &v))
}

func TestVerifyTrailerTotalsDisagree(t *testing.T) {
	t.Parallel()

	b0 := []tracepack.Record{dataRecord(0, 1, []byte("ab")), dataRecord(1, 1, nil)}
	b1 := []tracepack.Record{dataRecord(2, 1, nil)}
	pack := writePack(t, testMeta(), [][]tracepack.Record{b0, b1}, true)
	// A trailer record_count the footer's F-2 entries do not sum to rejects the footer (the tracepack format specification §10).
	pack = patchTrailer(t, pack, func(tr *format.Trailer) { tr.RecordCount++ })
	rep := verifyPack(t, pack)
	v, err := VerifyFrom(&rep)
	require.NoError(t, err)

	end := strconv.FormatUint(metaEnd(pack)+blockLen(b0)+blockLen(b1), 10)
	require.Equal(t, `{
  "outcome": "finalized-inconsistent",
  "finalized": true,
  "footer_valid": false,
  "blocks_located": 2,
  "blocks_validated": 2,
  "records": "3",
  "prefix_end": "`+end+`",
  "failed_blocks": [],
  "disagreeing_blocks": [],
  "trailer_totals_disagree": true,
  "writer_defects": [],
  "lost": []
}
`, marshalJSON(t, &v))
}

// gappedPack returns a finalized pack of three blocks whose middle block holds the seqs 3, 4, 7, 8 and 10,
// three seq ranges in its F-3 summary, with the blocks' records.
func gappedPack(t *testing.T) ([]byte, [][]tracepack.Record) {
	t.Helper()

	blocks := [][]tracepack.Record{
		{dataRecord(0, 1, nil), dataRecord(1, 1, nil), dataRecord(2, 1, nil)},
		{dataRecord(3, 1, []byte("x")), dataRecord(4, 1, nil), dataRecord(7, 1, nil), dataRecord(8, 1, nil), dataRecord(10, 1, nil)},
		{dataRecord(11, 2, nil), dataRecord(12, 2, nil)},
	}

	return writePack(t, testMeta(), blocks, true), blocks
}

func TestVerifyFailedBlockWithSeveralRanges(t *testing.T) {
	t.Parallel()

	pack, blocks := gappedPack(t)
	off1 := metaEnd(pack) + blockLen(blocks[0])
	// The last payload byte of the middle block: its body CRC fails, its envelope and the footer stay valid.
	pack = flipByte(pack, int(off1+blockLen(blocks[1])-1))
	rep := verifyPack(t, pack)
	v, err := VerifyFrom(&rep)
	require.NoError(t, err)

	off := strconv.FormatUint(off1, 10)
	ts := func(seq int64) string { return strconv.FormatInt(testHour+seq*int64(time.Microsecond), 10) }
	lost := func(first, last string) string {
		return `    {
      "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
      "seq_first": "` + first + `",
      "seq_last": "` + last + `",
      "time_start": "` + ts(3) + `",
      "time_end": "` + ts(10) + `"
    }`
	}
	require.Equal(t, `{
  "outcome": "corrupt-middle",
  "finalized": true,
  "footer_valid": true,
  "blocks_located": 3,
  "blocks_validated": 2,
  "records": "5",
  "prefix_end": "`+off+`",
  "failed_blocks": [
    {
      "block": 1,
      "offset": "`+off+`",
      "cause": "corrupt-block"
    }
  ],
  "disagreeing_blocks": [],
  "trailer_totals_disagree": false,
  "writer_defects": [],
  "lost": [
`+lost("3", "4")+`,
`+lost("7", "8")+`,
`+lost("10", "10")+`
  ]
}
`, marshalJSON(t, &v))
}
