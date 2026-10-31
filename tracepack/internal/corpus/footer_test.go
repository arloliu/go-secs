package corpus

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// closurePack returns a finalized pack of two blocks whose records end epochs and mark boundaries:
// block 0 ends epoch 1 by a socket-close at seq 1 and again by a clean stop at seq 2;
// block 1 holds epoch 2's gap boundary at seq 5, with a gap_start only, and its socket-close at seq 6, a boundary without a kind at seq 7,
// an unclean stop at seq 8 with both bounds, which ends no epoch,
// and at seq 9 a socket-close whose body is not valid, which ends nothing.
func closurePack(t *testing.T) []byte {
	t.Helper()

	kind := func(k tracepack.BoundaryKind) *tracepack.TransportEvent {
		return &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: &k}
	}
	closeEv := &tracepack.TransportEvent{Event: tracepack.EventSocketClose}
	gap := kind(tracepack.BoundaryKindGap)
	gap.GapStart = new(int64(-5))
	unclean := kind(tracepack.BoundaryKindStopUnclean)
	unclean.GapStart, unclean.GapEnd = new(int64(math.MinInt64)), new(int64(math.MaxInt64))
	invalid := eventRecord(t, 9, 3, closeEv)
	// A second event entry: the tag is not repeatable, so the body is not a valid TLV body.
	// The Writer refuses it, so the pack holds a placeholder in its place until it is written.
	body := tlv.AppendEntry(invalid.Payload, tlv.U8Entry(0x0001, uint8(tracepack.EventSocketClose)))
	invalid.Payload = must(placeholderEvent(len(body)))(t)

	blocks := [][]tracepack.Record{
		{dataRecord(0, 1, nil), eventRecord(t, 1, 1, closeEv), eventRecord(t, 2, 1, kind(tracepack.BoundaryKindStop)), dataRecord(3, 2, nil)},
		{
			dataRecord(4, 2, nil), eventRecord(t, 5, 2, gap), eventRecord(t, 6, 2, closeEv),
			eventRecord(t, 7, 3, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary}),
			eventRecord(t, 8, 3, unclean), invalid,
		},
	}

	return must(replacePayload(writePack(t, testMeta(), blocks, true), 1, invalid.Payload, body))(t)
}

// closureProjection is the projection of closurePack, written from the rules of the tracepack format specification §10.
const closureProjection = `{
  "blocks": [
    {
      "block": 0,
      "close_seqs": [
        {
          "epoch": 1,
          "seq": "1"
        }
      ],
      "boundaries": [
        {
          "seq": "2",
          "kind": "stop"
        }
      ]
    },
    {
      "block": 1,
      "close_seqs": [
        {
          "epoch": 2,
          "seq": "6"
        }
      ],
      "boundaries": [
        {
          "seq": "5",
          "kind": "gap",
          "gap_start": "-5"
        },
        {
          "seq": "7",
          "kind": "unknown"
        },
        {
          "seq": "8",
          "kind": "stop-unclean",
          "gap_start": "-9223372036854775808",
          "gap_end": "9223372036854775807"
        }
      ]
    }
  ],
  "f5": {
    "epochs": [
      {
        "epoch": 1,
        "close_seq": "1"
      },
      {
        "epoch": 2,
        "close_seq": "6"
      },
      {
        "epoch": 3
      }
    ],
    "boundaries": [
      {
        "seq": "2",
        "kind": "stop"
      },
      {
        "seq": "5",
        "kind": "gap",
        "gap_start": "-5"
      },
      {
        "seq": "7",
        "kind": "unknown"
      },
      {
        "seq": "8",
        "kind": "stop-unclean",
        "gap_start": "-9223372036854775808",
        "gap_end": "9223372036854775807"
      }
    ],
    "seq_ranges": [
      {
        "first": "0",
        "last": "9"
      }
    ]
  }
}
`

func TestFooterOfClosures(t *testing.T) {
	t.Parallel()

	pack := closurePack(t)
	stored, err := ReadStoredFooter(pack)
	require.NoError(t, err)
	require.Equal(t, closureProjection, marshalJSON(t, &stored))

	recomputed, err := RecomputeFooter(t.Context(), openPack(t, pack))
	require.NoError(t, err)
	require.Equal(t, closureProjection, marshalJSON(t, &recomputed))

	f, err := FooterOf(t.Context(), pack)
	require.NoError(t, err)
	b, err := f.Marshal()
	require.NoError(t, err)
	require.Equal(t, "{\n  \"stored\": "+indentTail(closureProjection)+",\n  \"accepted\": true,\n  \"recomputed\": "+indentTail(closureProjection)+"\n}\n", string(b))
}

func TestFooterStoredApartFromValidation(t *testing.T) {
	t.Parallel()

	pack := closurePack(t)
	// F-5 states close_seq 3 for epoch 2, which no block states:
	// footer validation rejects the footer, whose stored values the projection still shows.
	pack = must(PatchFooter(pack, func(d []byte) ([]byte, error) {
		at := must(NestedValueOffset(d, must(F5List(d))(t), f5EpochTag, 1, epochCloseSeqTag))(t)
		binary.LittleEndian.PutUint64(d[at:], 3)
		return d, nil
	}))(t)

	f, err := FooterOf(t.Context(), pack)
	require.NoError(t, err)
	require.False(t, f.Accepted)
	require.NotNil(t, f.Stored)
	require.Equal(t, []F5Epoch{{Epoch: 1, CloseSeq: new(U64(1))}, {Epoch: 2, CloseSeq: new(U64(3))}, {Epoch: 3}}, f.Stored.F5.Epochs)
	require.Equal(t, closureProjection, marshalJSON(t, &f.Recomputed), "the records still give the true values")

	unfinalized := writePack(t, testMeta(), [][]tracepack.Record{{dataRecord(0, 1, nil)}}, false)
	f, err = FooterOf(t.Context(), unfinalized)
	require.NoError(t, err)
	require.Nil(t, f.Stored)
	require.Equal(t, `{
  "accepted": false,
  "recomputed": {
    "blocks": [
      {
        "block": 0,
        "close_seqs": [],
        "boundaries": []
      }
    ],
    "f5": {
      "epochs": [
        {
          "epoch": 1
        }
      ],
      "boundaries": [],
      "seq_ranges": [
        {
          "first": "0",
          "last": "0"
        }
      ]
    }
  }
}
`, marshalJSON(t, &f))
}

// TestReadStoredFooterSeqRangeAboveLimit checks that an F-5 seq_range of 16 bytes is readable
// whatever its endpoints hold (the tracepack corpus specification §5.8 checks its length alone):
// with an endpoint above 2^63-1 the stored projection states it, while footer validation rejects the footer.
func TestReadStoredFooterSeqRangeAboveLimit(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		first, last uint64
	}{
		{"last above 2^63-1", 0, 1 << 63},
		{"both above 2^63-1", math.MaxUint64 - 1, math.MaxUint64},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pack := must(PatchFooter(closurePack(t), func(d []byte) ([]byte, error) {
				at := must(EntryOffset(d, must(F5List(d))(t), seqRangeTag, 0))(t) + tlv.HeaderLen
				binary.LittleEndian.PutUint64(d[at:], tt.first)
				binary.LittleEndian.PutUint64(d[at+8:], tt.last)

				return d, nil
			}))(t)
			stored, err := ReadStoredFooter(pack)
			require.NoError(t, err)
			require.Equal(t, []SeqRange{{First: U64(tt.first), Last: U64(tt.last)}}, stored.F5.SeqRanges)

			f, err := FooterOf(t.Context(), pack)
			require.NoError(t, err)
			require.NotNil(t, f.Stored, "the footer stays structurally readable")
			require.Equal(t, stored, *f.Stored)
			require.False(t, f.Accepted)
		})
	}
}

func TestReadStoredFooterReadsWhatValidationRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(t *testing.T, d []byte)
	}{
		// Footer validation requires record_header_len >= 44; the projection does not read it.
		{"record_header_len below 44", func(t *testing.T, d []byte) {
			binary.LittleEndian.PutUint16(d[must(F2Entry(d, 0))(t).Off+f2RecordHeaderLenOff:], 43)
		}},
		{"footer_layout_version 2", func(_ *testing.T, d []byte) { binary.LittleEndian.PutUint16(d[0:], 2) }},
		{"summary_len 0", func(t *testing.T, d []byte) {
			binary.LittleEndian.PutUint32(d[must(F2Entry(d, 0))(t).Off+f2SummaryLenOff:], 0)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pack := must(PatchFooter(closurePack(t), func(d []byte) ([]byte, error) { tt.edit(t, d); return d, nil }))(t)
			_, err := ReadStoredFooter(pack)
			require.NoError(t, err)
			f, err := FooterOf(t.Context(), pack)
			require.NoError(t, err)
			require.False(t, f.Accepted)
		})
	}
}

func TestReadStoredFooterUnreadable(t *testing.T) {
	t.Parallel()

	footerEdits := []struct {
		name string
		edit func(t *testing.T, d []byte) []byte
	}{
		{"no F-1 prologue", func(_ *testing.T, d []byte) []byte { return d[:format.FooterPrologueLen-1] }},
		{"f2_entry_len 79", func(_ *testing.T, d []byte) []byte {
			// One block, so the first entry, whose fields a stride of 79 does not move, is the only one read.
			binary.LittleEndian.PutUint32(d[prologueBlockCountOff:], 1)
			binary.LittleEndian.PutUint32(d[prologueF2EntryLenOff:], 79)
			return d
		}},
		{"F-2 past the footer", func(_ *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint32(d[prologueBlockCountOff:], 1<<20)
			return d
		}},
		{"F-3 past the footer", func(_ *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint64(d[prologueF3LenOff:], uint64(len(d)))
			return d
		}},
		{"F-5 past the footer", func(_ *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint64(d[prologueF5OffsetOff:], 1<<63)
			return d
		}},
		{"F-3 list outside F-3", func(t *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint32(d[must(F2Entry(d, 1))(t).Off+f2SummaryLenOff:], uint32(binary.LittleEndian.Uint64(d[prologueF3LenOff:])+1))
			return d
		}},
		{"F-3 list framing", func(t *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint16(d[must(EntryOffset(d, must(F3List(d, 0))(t), f3BoundaryTag, 0))(t):], 0)
			return d
		}},
		{"F-5 framing", func(t *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint16(d[must(EntryOffset(d, must(F5List(d))(t), f5EpochTag, 0))(t):], 0)
			return d
		}},
		{"epoch entry not tlv", func(t *testing.T, d []byte) []byte {
			d[must(EntryOffset(d, must(F3List(d, 0))(t), f3EpochTag, 0))(t)+2] = byte(tlv.TypeBytes)
			return d
		}},
		{"epoch entry without epoch", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F3List(d, 0))(t), f3EpochTag, 0, epochEpochTag))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], 0x0009)
			return d
		}},
		{"epoch entry with two epochs", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F5List(d))(t), f5EpochTag, 0, 0x0002))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], epochEpochTag)
			return d
		}},
		{"epoch above 2^32-1", func(t *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint64(d[must(NestedValueOffset(d, must(F5List(d))(t), f5EpochTag, 2, epochEpochTag))(t):], 1<<32)
			return d
		}},
		{"two close_seqs", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F3List(d, 0))(t), f3EpochTag, 0, 0x0002))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], epochCloseSeqTag)
			return d
		}},
		{"close_seq above 2^63-1", func(t *testing.T, d []byte) []byte {
			binary.LittleEndian.PutUint64(d[must(NestedValueOffset(d, must(F3List(d, 1))(t), f3EpochTag, 0, epochCloseSeqTag))(t):], 1<<63)
			return d
		}},
		{"boundary without seq", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F3List(d, 1))(t), f3BoundaryTag, 0, boundarySeqTag))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], 0x0009)
			return d
		}},
		{"boundary_kind not u8", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F5List(d))(t), f5BoundaryTag, 3, boundaryKindTag))(t)
			d[at-tlv.HeaderLen+2] = byte(tlv.TypeBool)
			return d
		}},
		{"two gap_starts", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F5List(d))(t), f5BoundaryTag, 3, boundaryGapEndTag))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], boundaryGapStartTag)
			return d
		}},
		{"two gap_ends", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F3List(d, 1))(t), f3BoundaryTag, 2, boundaryGapStartTag))(t)
			binary.LittleEndian.PutUint16(d[at-tlv.HeaderLen:], boundaryGapEndTag)
			return d
		}},
		{"gap_start not i64", func(t *testing.T, d []byte) []byte {
			at := must(NestedValueOffset(d, must(F3List(d, 1))(t), f3BoundaryTag, 0, boundaryGapStartTag))(t)
			d[at-tlv.HeaderLen+2] = byte(tlv.TypeU64)
			return d
		}},
		{"seq_range of 15 bytes", func(t *testing.T, d []byte) []byte {
			s := must(F5List(d))(t)
			entries := must(tlv.Decode(d[s.Off : s.Off+s.Len]))(t)
			var list []byte
			for _, e := range entries {
				if e.Tag == seqRangeTag {
					e.Value = e.Value[:15]
				}
				list = tlv.AppendEntry(list, e)
			}
			require.Equal(t, len(d), s.Off+s.Len, "F-5 ends the footer")
			d = append(d[:s.Off], list...)
			binary.LittleEndian.PutUint64(d[prologueF5LenOff:], uint64(len(list)))

			return d
		}},
	}
	for _, tt := range footerEdits {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pack := must(PatchFooter(closurePack(t), func(d []byte) ([]byte, error) { return tt.edit(t, d), nil }))(t)
			_, err := ReadStoredFooter(pack)
			require.ErrorIs(t, err, ErrFooterUnreadable)
			f, err := FooterOf(t.Context(), pack)
			require.NoError(t, err)
			require.Nil(t, f.Stored)
		})
	}

	packEdits := []struct {
		name string
		edit func(t *testing.T, pack []byte) []byte
	}{
		{"not finalized", func(_ *testing.T, pack []byte) []byte { return pack[:len(pack)-1] }},
		{"trailer CRC", func(t *testing.T, pack []byte) []byte {
			return must(FlipByte(pack, len(pack)-testTrailerLen+trailerCRCOff))(t)
		}},
		{"footer before the pack metadata ends", func(t *testing.T, pack []byte) []byte {
			// A pack_metadata_len past footer_offset: the header is not validated, only read.
			out := bytes.Clone(pack)
			binary.LittleEndian.PutUint32(out[16:], uint32(len(pack)))
			return out
		}},
		{"footer_len", func(t *testing.T, pack []byte) []byte {
			return must(PatchTrailer(pack, func(tr []byte) { addU64(tr, trailerFooterLenOff, ^uint64(0)) }))(t)
		}},
		{"footer CRC", func(t *testing.T, pack []byte) []byte {
			return must(FlipByte(pack, int(must(Locate(pack))(t).End)+1))(t)
		}},
		{"footer codec", func(t *testing.T, pack []byte) []byte {
			return must(PatchTrailer(pack, func(tr []byte) { tr[trailerFooterCodecOff] = 9 }))(t)
		}},
		{"footer_uncompressed_len", func(t *testing.T, pack []byte) []byte {
			return must(PatchTrailer(pack, func(tr []byte) { addU64(tr, trailerUncompressedOff, 1) }))(t)
		}},
	}
	for _, tt := range packEdits {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ReadStoredFooter(tt.edit(t, closurePack(t)))
			require.ErrorIs(t, err, ErrFooterUnreadable)
		})
	}

	_, err := ReadStoredFooter(make([]byte, testFileHeaderLen-1))
	require.ErrorIs(t, err, ErrFooterUnreadable)

	huge := must(PatchTrailer(closurePack(t), func(tr []byte) {
		binary.LittleEndian.PutUint64(tr[trailerUncompressedOff:], tracepack.DefaultMaxFooterLen+1)
	}))(t)
	_, err = ReadStoredFooter(huge)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrFooterUnreadable, "a reader limit is never a golden")
}

// indentTail indents every line of the canonical text s but the first by two spaces and drops its final LF,
// to nest s as a member's value.
func indentTail(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ")
}

// TestFooterOfEpochClosedInTwoBlocks checks the block-local and pack-wide close_seq of an epoch two blocks end
// (the tracepack format specification §10): each F-3 entry states its own block's closure, F-5 the lowest.
func TestFooterOfEpochClosedInTwoBlocks(t *testing.T) {
	t.Parallel()

	stop := tracepack.BoundaryKindStop
	blocks := [][]tracepack.Record{
		{dataRecord(0, 1, nil), eventRecord(t, 1, 1, &tracepack.TransportEvent{Event: tracepack.EventSocketClose})},
		{dataRecord(2, 1, nil), eventRecord(t, 3, 1, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: &stop})},
	}
	f, err := FooterOf(t.Context(), writePack(t, testMeta(), blocks, true))
	require.NoError(t, err)

	const projection = `{
  "blocks": [
    {
      "block": 0,
      "close_seqs": [
        {
          "epoch": 1,
          "seq": "1"
        }
      ],
      "boundaries": []
    },
    {
      "block": 1,
      "close_seqs": [
        {
          "epoch": 1,
          "seq": "3"
        }
      ],
      "boundaries": [
        {
          "seq": "3",
          "kind": "stop"
        }
      ]
    }
  ],
  "f5": {
    "epochs": [
      {
        "epoch": 1,
        "close_seq": "1"
      }
    ],
    "boundaries": [
      {
        "seq": "3",
        "kind": "stop"
      }
    ],
    "seq_ranges": [
      {
        "first": "0",
        "last": "3"
      }
    ]
  }
}
`
	require.True(t, f.Accepted)
	require.NotNil(t, f.Stored)
	require.Equal(t, projection, marshalJSON(t, f.Stored))
	require.Equal(t, projection, marshalJSON(t, &f.Recomputed))
}

// TestFooterProjectionOrder checks the orders of the tracepack corpus specification §5.8 on entries equal in their leading keys:
// boundaries by seq, then numeric boundary_kind, then gap_start, then gap_end, an absent bound before every present one
// and present bounds compared as i64; seq ranges by first, then last.
func TestFooterProjectionOrder(t *testing.T) {
	t.Parallel()

	bound := func(v int64) *int64 { return &v }
	p := projection{
		f5Boundary: []boundaryEntry{
			{seq: 4, kind: 3, gapStart: bound(1), gapEnd: bound(2)},
			{seq: 4, kind: 3, gapStart: bound(1)},
			{seq: 4, kind: 3, gapStart: bound(-1), gapEnd: bound(9)},
			{seq: 4, kind: 3, gapEnd: bound(0)},
			{seq: 4, kind: 2, gapStart: bound(7)},
			{seq: 3, kind: 4},
			{seq: 4, kind: 3, gapStart: bound(1), gapEnd: bound(-2)},
		},
		f5SeqRanges: []tlv.SeqRange{{First: 5, Last: 9}, {First: 0, Last: 3}, {First: 5, Last: 6}},
	}
	got := p.sorted().F5
	type b struct {
		seq      uint64
		kind     string
		from, to string
	}
	bs := make([]b, 0, len(got.Boundaries))
	for _, x := range got.Boundaries {
		e := b{seq: uint64(x.Seq), kind: x.Kind, from: "-", to: "-"}
		if x.GapStart != nil {
			e.from = strconv.FormatInt(int64(*x.GapStart), 10)
		}
		if x.GapEnd != nil {
			e.to = strconv.FormatInt(int64(*x.GapEnd), 10)
		}
		bs = append(bs, e)
	}
	require.Equal(t, []b{
		{3, "stop-unclean", "-", "-"},
		{4, "stop", "7", "-"},
		{4, "gap", "-", "0"},
		{4, "gap", "-1", "9"},
		{4, "gap", "1", "-"},
		{4, "gap", "1", "-2"},
		{4, "gap", "1", "2"},
	}, bs)
	require.Equal(t, []SeqRange{{First: 0, Last: 3}, {First: 5, Last: 6}, {First: 5, Last: 9}}, got.SeqRanges)
	require.Equal(t, "{\n  \"epochs\": [],\n  \"boundaries\": [],\n  \"seq_ranges\": []\n}\n",
		marshalJSON(t, (&projection{}).sorted().F5), "seq_ranges is present when empty")
}
