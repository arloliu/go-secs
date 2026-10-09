package corpus

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// fullFilterJSON is a filter that sets every key, in canonical form.
const fullFilterJSON = `{
  "time_from": "-5",
  "time_to": "1791000000000000000",
  "kinds": [
    "unknown",
    "data",
    "annotation",
    "unknown(9)"
  ],
  "dirs": [
    "host-to-equipment",
    "local"
  ],
  "epochs": [
    0,
    4294967295
  ],
  "sf": [
    {
      "stream": 1,
      "function": 3
    },
    {
      "stream": 1,
      "function": 13
    },
    {
      "stream": 6,
      "function": 11
    }
  ],
  "session_ids": [
    7,
    65535
  ],
  "system_bytes": "AAECAw==",
  "include_unavailable": true
}
`

func TestFilterRoundTrip(t *testing.T) {
	t.Parallel()

	var f Filter
	require.NoError(t, json.Unmarshal([]byte(fullFilterJSON), &f))
	tf, err := f.ToFilter()
	require.NoError(t, err)
	require.Equal(t, tracepack.Filter{
		TimeFrom: new(int64(-5)), TimeTo: new(testHour),
		Kinds:      []tracepack.Kind{tracepack.KindUnknown, tracepack.KindData, tracepack.KindAnnotation, tracepack.Kind(9)},
		Dirs:       []tracepack.Dir{tracepack.DirHostToEquipment, tracepack.DirLocal},
		Epochs:     []uint32{0, 4294967295},
		SF:         []tracepack.SF{{Stream: 1, Function: 3}, {Stream: 1, Function: 13}, {Stream: 6, Function: 11}},
		SessionIDs: []uint16{7, 65535}, SystemBytes: &[4]byte{0, 1, 2, 3}, IncludeUnavailable: true,
	}, tf)

	back := FilterFrom(&tf)
	require.Equal(t, fullFilterJSON, marshalJSON(t, &back))

	again, err := back.ToFilter()
	require.NoError(t, err)
	require.Equal(t, tf, again)
}

func TestFilterFromSorts(t *testing.T) {
	t.Parallel()

	tf := tracepack.Filter{
		Kinds:      []tracepack.Kind{tracepack.Kind(200), tracepack.KindControl, tracepack.KindUnknown},
		Dirs:       []tracepack.Dir{tracepack.DirLocal, tracepack.DirUnknown},
		Epochs:     []uint32{9, 2},
		SF:         []tracepack.SF{{Stream: 2, Function: 1}, {Stream: 1, Function: 2}, {Stream: 1, Function: 1}},
		SessionIDs: []uint16{3, 1},
	}
	f := FilterFrom(&tf)
	require.Equal(t, `{
  "kinds": [
    "unknown",
    "control",
    "unknown(200)"
  ],
  "dirs": [
    "unknown",
    "local"
  ],
  "epochs": [
    2,
    9
  ],
  "sf": [
    {
      "stream": 1,
      "function": 1
    },
    {
      "stream": 1,
      "function": 2
    },
    {
      "stream": 2,
      "function": 1
    }
  ],
  "session_ids": [
    1,
    3
  ]
}
`, marshalJSON(t, &f))

	empty := FilterFrom(&tracepack.Filter{Kinds: []tracepack.Kind{}, SF: []tracepack.SF{}})
	require.Equal(t, "{}\n", marshalJSON(t, &empty), "an empty predicate is no key")
}

func TestFilterRefuses(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		`{"kinds":[]}`, `{"dirs":[]}`, `{"epochs":[]}`, `{"sf":[]}`, `{"session_ids":[]}`,
		`{"kinds":["Data"]}`, `{"kinds":["unknown(0)"]}`, `{"dirs":["up"]}`, `{"system_bytes":"AAEC"}`,
	} {
		var f Filter
		require.NoError(t, json.Unmarshal([]byte(bad), &f), bad)
		_, err := f.ToFilter()
		require.Error(t, err, bad)
	}

	query := func(filter string) string {
		return "[\n  {\n    \"id\": \"q\",\n    \"cites\": [\n      \"CORPUS §5.4\"\n    ],\n    \"filter\": " + filter +
			",\n    \"expect\": {\n      \"seqs\": [],\n      \"incomplete\": []\n    }\n  }\n]\n"
	}
	var qs Queries
	require.NoError(t, Unmarshal([]byte(query("{\n      \"include_unavailable\": true\n    }")), &qs))
	for _, bad := range []string{
		"{\n      \"include_unavailable\": false\n    }",
		"{\n      \"kinds\": []\n    }",
		"{\n      \"epochs\": [\n        2,\n        1\n      ]\n    }",
	} {
		require.Error(t, Unmarshal([]byte(query(bad)), &qs), bad)
	}
}

func TestExpectFromConstructedResult(t *testing.T) {
	t.Parallel()

	captureID := testCaptureID
	coverage := []tracepack.Coverage{
		{CaptureID: &captureID, SeqFirst: new(uint64(1))},
		{SeqFirst: new(uint64(2))},
		{CaptureID: &captureID, SeqFirst: new(uint64(1))},
	}
	res := tracepack.Result{Incomplete: []tracepack.Defect{
		{Reason: tracepack.ReasonIndexMismatch, Block: 3, Offset: 700},
		{Reason: tracepack.ReasonTruncated, Block: -1, Offset: -1},
		{Reason: tracepack.ReasonCorruptBlock, Block: 1, Offset: 300},
		{Reason: tracepack.ReasonCoverage, Block: -1, Offset: -1, Coverage: &coverage[1]},
		{Reason: tracepack.ReasonCoverage, Block: -1, Offset: -1, Coverage: &coverage[0]},
		{Reason: tracepack.ReasonUnknownCodec, Block: 2, Offset: 500},
		{Reason: tracepack.ReasonCoverage, Block: -1, Offset: -1, Coverage: &coverage[2]},
	}}
	got, err := ExpectFrom([]int{2, 0, 0, 3}, []uint64{20, 5, 3, 30}, &res, coverage)
	require.NoError(t, err)
	require.Equal(t, `{
  "seqs": [
    "5",
    "3",
    "20",
    "30"
  ],
  "incomplete": [
    {
      "reason": "coverage",
      "coverage": {
        "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
        "seq_first": "1"
      }
    },
    {
      "reason": "coverage",
      "coverage": {
        "seq_first": "2"
      }
    },
    {
      "reason": "coverage",
      "coverage": {
        "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
        "seq_first": "1"
      }
    },
    {
      "reason": "truncated"
    },
    {
      "reason": "corrupt-block",
      "block": 1,
      "offset": "300"
    },
    {
      "reason": "unknown-codec",
      "block": 2,
      "offset": "500"
    },
    {
      "reason": "index-mismatch",
      "block": 3,
      "offset": "700"
    }
  ]
}
`, marshalJSON(t, &got))
}

func TestExpectFromRefuses(t *testing.T) {
	t.Parallel()

	other := tracepack.Coverage{SeqFirst: new(uint64(9))}
	tests := []struct {
		name string
		res  tracepack.Result
	}{
		{"reader limit", tracepack.Result{Incomplete: []tracepack.Defect{{Reason: tracepack.ReasonLimit, Block: 0, Offset: 80}}}},
		{"two truncations", tracepack.Result{Incomplete: []tracepack.Defect{
			{Reason: tracepack.ReasonTruncated, Block: -1, Offset: 80}, {Reason: tracepack.ReasonTruncated, Block: -1, Offset: -1},
		}}},
		{"coverage not in the metadata", tracepack.Result{Incomplete: []tracepack.Defect{{Reason: tracepack.ReasonCoverage, Block: -1, Offset: -1, Coverage: &other}}}},
		{"block defect without offset", tracepack.Result{Incomplete: []tracepack.Defect{{Reason: tracepack.ReasonCorruptBlock, Block: 0, Offset: -1}}}},
		{"truncated block", tracepack.Result{Incomplete: []tracepack.Defect{{Reason: tracepack.ReasonTruncated, Block: 0, Offset: 80}}}},
		{"removed hours", tracepack.Result{Removed: []int64{0}}},
		{"conflicts", tracepack.Result{Conflicts: []tracepack.Conflict{{}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ExpectFrom(nil, nil, &tt.res, nil)
			require.Error(t, err)
		})
	}
}

func TestRunQueryEmptyPack(t *testing.T) {
	t.Parallel()

	pack := writePack(t, testMeta(), nil, false)
	got, err := RunQuery(t.Context(), openPack(t, pack), &Filter{})
	require.NoError(t, err)
	require.Equal(t, `{
  "seqs": [],
  "incomplete": [
    {
      "reason": "truncated",
      "offset": "`+strconv.FormatUint(metaEnd(pack), 10)+`"
    }
  ]
}
`, marshalJSON(t, &got))
}

func TestRunQueryCoverageAndTruncation(t *testing.T) {
	t.Parallel()

	other := tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x09, 0x80, 0, 0, 0, 0, 0, 0, 0x09}
	captureID := testCaptureID
	meta := testMeta()
	meta.Coverage = []tracepack.Coverage{
		// Meets the filter's time range.
		{CaptureID: &captureID, SeqFirst: new(uint64(100)), SeqLast: new(uint64(110)), TimeStart: new(testHour), TimeEnd: new(testHour + 2000)},
		// Another capture.
		{CaptureID: &other, SeqFirst: new(uint64(5))},
		// No time interval: meets every query.
		{SeqFirst: new(uint64(200)), SeqLast: new(uint64(201))},
		// Past the filter's time range.
		{CaptureID: &captureID, TimeStart: new(testHour + int64(time.Hour)), TimeEnd: new(testHour + 2*int64(time.Hour))},
	}
	b0 := []tracepack.Record{dataRecord(0, 1, nil), dataRecord(1, 1, []byte("z")), dataRecord(2, 1, nil)}
	pack := writePack(t, meta, [][]tracepack.Record{b0}, false)

	f := FilterFrom(&tracepack.Filter{TimeFrom: new(testHour + 1000), TimeTo: new(testHour + int64(time.Minute))})
	got, err := RunQuery(t.Context(), openPack(t, pack), &f)
	require.NoError(t, err)
	require.Equal(t, `{
  "seqs": [
    "1",
    "2"
  ],
  "incomplete": [
    {
      "reason": "coverage",
      "coverage": {
        "capture_id": "019a2b3c-4d5e-7002-8000-000000000002",
        "seq_first": "100",
        "seq_last": "110",
        "time_start": "1791000000000000000",
        "time_end": "1791000000000002000"
      }
    },
    {
      "reason": "coverage",
      "coverage": {
        "seq_first": "200",
        "seq_last": "201"
      }
    },
    {
      "reason": "truncated",
      "offset": "`+strconv.FormatUint(metaEnd(pack)+blockLen(b0), 10)+`"
    }
  ]
}
`, marshalJSON(t, &got))
}

func TestRunQueryTrailerTotals(t *testing.T) {
	t.Parallel()

	pack := writePack(t, testMeta(), [][]tracepack.Record{{dataRecord(0, 1, nil)}, {dataRecord(1, 1, nil)}}, true)
	pack = patchTrailer(t, pack, func(tr *format.Trailer) { tr.RecordCount++ })
	got, err := RunQuery(t.Context(), openPack(t, pack), &Filter{})
	require.NoError(t, err)
	require.Equal(t, `{
  "seqs": [
    "0",
    "1"
  ],
  "incomplete": [
    {
      "reason": "truncated"
    }
  ]
}
`, marshalJSON(t, &got))
}

func TestRunQueryFailedBlock(t *testing.T) {
	t.Parallel()

	pack, blocks := gappedPack(t)
	off1 := metaEnd(pack) + blockLen(blocks[0])
	pack = flipByte(pack, int(off1+blockLen(blocks[1])-1))
	got, err := RunQuery(t.Context(), openPack(t, pack), &Filter{})
	require.NoError(t, err)
	require.Equal(t, `{
  "seqs": [
    "0",
    "1",
    "2",
    "11",
    "12"
  ],
  "incomplete": [
    {
      "reason": "corrupt-block",
      "block": 1,
      "offset": "`+strconv.FormatUint(off1, 10)+`"
    }
  ]
}
`, marshalJSON(t, &got))
}

func TestQueriesFile(t *testing.T) {
	t.Parallel()

	qs := Queries{
		{ID: "b-all", Cites: []string{"CORPUS §5.4"}, Filter: Filter{}, Expect: Expect{Seqs: []U64{}, Incomplete: []Incomplete{}}},
		{ID: "a-epoch", Cites: []string{"SEM §7.4"}, Filter: Filter{Epochs: []uint32{3}}, Expect: Expect{Seqs: []U64{7}, Incomplete: []Incomplete{}}},
	}
	b, err := qs.Marshal()
	require.NoError(t, err)
	want := `[
  {
    "id": "a-epoch",
    "cites": [
      "SEM §7.4"
    ],
    "filter": {
      "epochs": [
        3
      ]
    },
    "expect": {
      "seqs": [
        "7"
      ],
      "incomplete": []
    }
  },
  {
    "id": "b-all",
    "cites": [
      "CORPUS §5.4"
    ],
    "filter": {},
    "expect": {
      "seqs": [],
      "incomplete": []
    }
  }
]
`
	require.Equal(t, want, string(b))

	var back Queries
	require.NoError(t, Unmarshal(b, &back))
	require.Equal(t, Queries{qs[1], qs[0]}, back)

	_, err = Queries{qs[0], qs[0]}.Marshal()
	require.Error(t, err, "an id given twice")
	_, err = Queries{{ID: "x", Cites: []string{"c"}, Filter: Filter{Kinds: []string{"nope"}}, Expect: qs[0].Expect}}.Marshal()
	require.Error(t, err, "a filter ToFilter refuses")
	empty, err := Queries(nil).Marshal()
	require.NoError(t, err)
	require.Equal(t, "[]\n", string(empty))
}
