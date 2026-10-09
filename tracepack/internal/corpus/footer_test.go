package corpus

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// closurePack returns a finalized pack of two blocks whose records end epochs and mark boundaries:
// block 0 ends epoch 1 by a socket-close at seq 1 and again by a clean stop at seq 2;
// block 1 holds epoch 2's gap boundary at seq 5 and its socket-close at seq 6, a boundary without a kind at seq 7,
// an unclean stop at seq 8, which ends no epoch, and at seq 9 a socket-close whose body is not valid, which ends nothing.
func closurePack(t *testing.T) []byte {
	t.Helper()

	kind := func(k tracepack.BoundaryKind) *tracepack.TransportEvent {
		return &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: &k}
	}
	closeEv := &tracepack.TransportEvent{Event: tracepack.EventSocketClose}
	invalid := eventRecord(t, 9, 3, closeEv)
	// A second event entry: the tag is not repeatable, so the body is not a valid TLV body.
	invalid.Payload = tlv.AppendEntry(invalid.Payload, tlv.U8Entry(0x0001, uint8(tracepack.EventSocketClose)))

	blocks := [][]tracepack.Record{
		{dataRecord(0, 1, nil), eventRecord(t, 1, 1, closeEv), eventRecord(t, 2, 1, kind(tracepack.BoundaryKindStop)), dataRecord(3, 2, nil)},
		{
			dataRecord(4, 2, nil), eventRecord(t, 5, 2, kind(tracepack.BoundaryKindGap)), eventRecord(t, 6, 2, closeEv),
			eventRecord(t, 7, 3, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary}),
			eventRecord(t, 8, 3, kind(tracepack.BoundaryKindStopUnclean)), invalid,
		},
	}

	return writePack(t, testMeta(), blocks, true)
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
          "kind": "gap"
        },
        {
          "seq": "7",
          "kind": "unknown"
        },
        {
          "seq": "8",
          "kind": "stop-unclean"
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
        "kind": "gap"
      },
      {
        "seq": "7",
        "kind": "unknown"
      },
      {
        "seq": "8",
        "kind": "stop-unclean"
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
      "boundaries": []
    }
  }
}
`, marshalJSON(t, &f))
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
    ]
  }
}
`
	require.True(t, f.Accepted)
	require.NotNil(t, f.Stored)
	require.Equal(t, projection, marshalJSON(t, f.Stored))
	require.Equal(t, projection, marshalJSON(t, &f.Recomputed))
}
