package corpus

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

func TestTruncationTableMerges(t *testing.T) {
	t.Parallel()

	read := func(n int) Cut {
		return Cut{ExportedRecords: new(U64(n)), Verify: &CutVerify{Outcome: "unfinalized", BlocksValidated: n, Records: U64(n), PrefixEnd: 80}}
	}
	tab := NewTruncationTable(6)
	for _, c := range []Cut{RejectedCut(RejectShortObject), RejectedCut(RejectShortObject), read(0), read(1), read(1), RejectedCut(RejectShortObject)} {
		require.NoError(t, tab.Add(c))
	}
	require.Error(t, tab.Add(read(1)), "every cut is given")
	got, err := tab.Table()
	require.NoError(t, err)
	require.Equal(t, `{
  "size": "6",
  "rows": [
    {
      "from": "0",
      "to": "1",
      "rejection": "short-object"
    },
    {
      "from": "2",
      "to": "2",
      "exported_records": "0"
    },
    {
      "from": "3",
      "to": "4",
      "exported_records": "1"
    },
    {
      "from": "5",
      "to": "5",
      "rejection": "short-object"
    }
  ]
}
`, marshalJSON(t, &got))
	require.Len(t, tab.Cuts(), 4, "the runs of equal expectations")

	_, err = NewTruncationTable(2).Table()
	require.Error(t, err, "a table missing cuts")
	require.Error(t, NewTruncationTable(1).Add(Cut{}), "a cut neither rejected nor read")
	require.Error(t, NewTruncationTable(1).Add(Cut{Rejection: RejectShortObject, ExportedRecords: new(U64(0))}), "a cut both rejected and read")
}

// TestTruncationTableMergesWhatItDoesNotWrite checks that two runs of cuts that differ only in their verification,
// which truncation.json does not state, are one row of the file and stay two runs in memory.
func TestTruncationTableMergesWhatItDoesNotWrite(t *testing.T) {
	t.Parallel()

	tab := NewTruncationTable(3)
	at := Cut{ExportedRecords: new(U64(2)), Verify: &CutVerify{Outcome: "unfinalized", BlocksValidated: 1, Records: 2, PrefixEnd: 90}}
	after := at
	after.Verify = &CutVerify{Outcome: "unfinalized", BlocksValidated: 1, Records: 2, PrefixEnd: 90, WalkStop: new(U64(90))}
	for _, c := range []Cut{at, after, after} {
		require.NoError(t, tab.Add(c))
	}
	got, err := tab.Table()
	require.NoError(t, err)
	require.Equal(t, []TruncationRow{{From: 0, To: 2, ExportedRecords: new(U64(2))}}, got.Rows)
	require.Equal(t, []CutRow{{From: 0, To: 0, Cut: at}, {From: 1, To: 2, Cut: after}}, tab.Cuts())
}

func TestTruncationSweep(t *testing.T) {
	t.Parallel()

	b0 := []tracepack.Record{dataRecord(0, 1, []byte("ab")), dataRecord(1, 1, nil)}
	pack := writePack(t, testMeta(), [][]tracepack.Record{b0}, true)
	tab := NewTruncationTable(uint64(len(pack)))
	for l := range len(pack) {
		cut := pack[:l]
		code, err := RejectionOf(t.Context(), cut)
		if err == nil {
			require.NoError(t, tab.Add(RejectedCut(code)))
			continue
		}
		rep := verifyPack(t, cut)
		var export bytes.Buffer
		_, err = tracepack.ExportJSONL(t.Context(), openPack(t, cut), &export)
		require.NoError(t, err)
		c, err := ReadCut(&rep, uint64(bytes.Count(export.Bytes(), []byte("\n"))-1))
		require.NoError(t, err)
		require.NoError(t, tab.Add(c))
	}
	got, err := tab.Table()
	require.NoError(t, err)

	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	me, fo := metaEnd(pack), metaEnd(pack)+blockLen(b0)
	row := func(from, to uint64, body string) string {
		return "    {\n      \"from\": \"" + u(from) + "\",\n      \"to\": \"" + u(to) + "\",\n" + body + "    }"
	}
	read := func(records uint64) string {
		return "      \"exported_records\": \"" + u(records) + "\"\n"
	}
	require.Equal(t, `{
  "size": "`+u(uint64(len(pack)))+`",
  "rows": [
`+row(0, testFileHeaderLen-1, "      \"rejection\": \"short-object\"\n")+`,
`+row(testFileHeaderLen, me-1, "      \"rejection\": \"metadata-past-object\"\n")+`,
`+row(me, fo-1, read(0))+`,
`+row(fo, uint64(len(pack))-1, read(2))+`
  ]
}
`, marshalJSON(t, &got))

	walk := func(records int, prefixEnd uint64, walkStop *U64) *CutVerify {
		return &CutVerify{Outcome: "unfinalized", BlocksValidated: records / 2, Records: U64(records), PrefixEnd: U64(prefixEnd), WalkStop: walkStop}
	}
	cuts := tab.Cuts()
	require.Len(t, cuts, 6)
	require.Equal(t, []*CutVerify{nil, nil, walk(0, me, nil), walk(0, me, new(U64(me))), walk(2, fo, nil), walk(2, fo, new(U64(fo)))},
		[]*CutVerify{cuts[0].Verify, cuts[1].Verify, cuts[2].Verify, cuts[3].Verify, cuts[4].Verify, cuts[5].Verify})
}
