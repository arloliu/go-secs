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
		return Cut{Outcome: "unfinalized", ExportedRecords: new(U64(n)), Verify: &CutVerify{BlocksValidated: n, Records: U64(n), PrefixEnd: 80}}
	}
	tab := NewTruncationTable(6, nil)
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
      "outcome": "unfinalized",
      "exported_records": "0",
      "verify": {
        "blocks_validated": 0,
        "records": "0",
        "prefix_end": "80"
      }
    },
    {
      "from": "3",
      "to": "4",
      "outcome": "unfinalized",
      "exported_records": "1",
      "verify": {
        "blocks_validated": 1,
        "records": "1",
        "prefix_end": "80"
      }
    },
    {
      "from": "5",
      "to": "5",
      "rejection": "short-object"
    }
  ]
}
`, marshalJSON(t, &got))

	_, err = NewTruncationTable(2, nil).Table()
	require.Error(t, err, "a table missing cuts")
}

func TestTruncationTableRepairRows(t *testing.T) {
	t.Parallel()

	o := testRepairOptions(t, nil)
	read := Cut{Outcome: "unfinalized", ExportedRecords: new(U64(0)), Verify: &CutVerify{PrefixEnd: 80}}
	patched := read.WithRepair(&Repair{Result: RepairPatched, Blocks: new(0), Records: new(U64(0)), CoverageAdded: &[]Coverage{{SeqFirst: new(U64(0))}}})

	tab := NewTruncationTable(3, &o)
	require.Error(t, tab.Add(read), "a read cut without its repair in a repair table")
	require.NoError(t, tab.Add(RejectedCut(RejectShortObject)))
	require.Error(t, tab.Add(Cut{Rejection: RejectShortObject, Repair: patched.Repair}), "a rejected cut with a repair")
	require.NoError(t, tab.Add(patched))
	require.NoError(t, tab.Add(read.WithRepair(&Repair{Result: RepairNotRepairable})))
	got, err := tab.Table()
	require.NoError(t, err)
	require.Equal(t, `{
  "size": "3",
  "repair_options": {
    "writer": "corpus-repair",
    "pack_id": "019a2b3c-4d5e-7004-8000-000000000004",
    "codec": "none",
    "writer_start_utc_ns": "1791000000000000000"
  },
  "rows": [
    {
      "from": "0",
      "to": "0",
      "rejection": "short-object"
    },
    {
      "from": "1",
      "to": "1",
      "outcome": "unfinalized",
      "exported_records": "0",
      "verify": {
        "blocks_validated": 0,
        "records": "0",
        "prefix_end": "80"
      },
      "repair": {
        "result": "patched",
        "blocks": 0,
        "coverage_added": [
          {
            "seq_first": "0"
          }
        ]
      }
    },
    {
      "from": "2",
      "to": "2",
      "outcome": "unfinalized",
      "exported_records": "0",
      "verify": {
        "blocks_validated": 0,
        "records": "0",
        "prefix_end": "80"
      },
      "repair": {
        "result": "not-repairable"
      }
    }
  ]
}
`, marshalJSON(t, &got))

	plain := NewTruncationTable(1, nil)
	require.Error(t, plain.Add(patched), "a repair in a table without repair options")
	require.Error(t, plain.Add(Cut{}), "a cut neither rejected nor read")
}

func TestTruncationSweep(t *testing.T) {
	t.Parallel()

	b0 := []tracepack.Record{dataRecord(0, 1, []byte("ab")), dataRecord(1, 1, nil)}
	pack := writePack(t, testMeta(), [][]tracepack.Record{b0}, true)
	tab := NewTruncationTable(uint64(len(pack)), nil)
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
	read := func(records, prefixEnd uint64, walkStop string) string {
		blocks := "0"
		if records > 0 {
			blocks = "1"
		}
		s := `      "outcome": "unfinalized",
      "exported_records": "` + u(records) + `",
      "verify": {
        "blocks_validated": ` + blocks + `,
        "records": "` + u(records) + `",
        "prefix_end": "` + u(prefixEnd) + `"`
		if walkStop != "" {
			s += ",\n        \"walk_stop\": \"" + walkStop + "\""
		}

		return s + "\n      }\n"
	}
	require.Equal(t, `{
  "size": "`+u(uint64(len(pack)))+`",
  "rows": [
`+row(0, testFileHeaderLen-1, "      \"rejection\": \"short-object\"\n")+`,
`+row(testFileHeaderLen, me-1, "      \"rejection\": \"metadata-past-object\"\n")+`,
`+row(me, me, read(0, me, ""))+`,
`+row(me+1, fo-1, read(0, me, u(me)))+`,
`+row(fo, fo, read(2, fo, ""))+`,
`+row(fo+1, uint64(len(pack))-1, read(2, fo, u(fo)))+`
  ]
}
`, marshalJSON(t, &got))
}
