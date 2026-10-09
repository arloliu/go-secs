package corpus

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// canonDoc is a document of two keys.
type canonDoc struct {
	A U64    `json:"a"`
	B []bool `json:"b"`
}

func TestCanonical(t *testing.T) {
	t.Parallel()

	in := []byte(`{"b":[1,{"x":true,"y":[]},{}],"a":"q\"\\\b\t\n\f\r\u0001\u001F\u007f\/<>&\u2028` + " é" + `","c":{}}`)
	want := "{\n" +
		"  \"b\": [\n" +
		"    1,\n" +
		"    {\n" +
		"      \"x\": true,\n" +
		"      \"y\": []\n" +
		"    },\n" +
		"    {}\n" +
		"  ],\n" +
		"  \"a\": \"q\\\"\\\\\\b\\t\\n\\f\\r\\u0001\\u001f\x7f/<>&  é\",\n" +
		"  \"c\": {}\n" +
		"}\n"

	got, err := Canonical(in)
	require.NoError(t, err)
	require.Equal(t, want, string(got))

	again, err := Canonical(got)
	require.NoError(t, err)
	require.Equal(t, want, string(again), "the canonical form is a fixed point")
}

func TestCanonicalRefuses(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`null`, `{"a":null}`, `[1.5]`, `[-1]`, `[1e3]`, `[01]`, `{} {}`, `[1`} {
		_, err := Canonical([]byte(in))
		require.Error(t, err, in)
	}
}

func TestMarshalRefusesNull(t *testing.T) {
	t.Parallel()

	_, err := Marshal(struct {
		A []string `json:"a"`
	}{})
	require.ErrorIs(t, err, ErrNotCanonical)
}

func TestDecimalStrings(t *testing.T) {
	t.Parallel()

	got, err := Marshal(struct {
		U    U64 `json:"u"`
		Min  I64 `json:"min"`
		Max  I64 `json:"max"`
		Zero I64 `json:"zero"`
	}{U: 18446744073709551615, Min: -9223372036854775808, Max: 9223372036854775807})
	require.NoError(t, err)
	require.Equal(t, `{
  "u": "18446744073709551615",
  "min": "-9223372036854775808",
  "max": "9223372036854775807",
  "zero": "0"
}
`, string(got))

	var u U64
	var i I64
	for _, bad := range []string{`"01"`, `"+1"`, `"-1"`, `""`, `1`, `"1.0"`, `"18446744073709551616"`, `" 1"`} {
		require.Error(t, u.UnmarshalJSON([]byte(bad)), bad)
	}
	for _, bad := range []string{`"-0"`, `"-01"`, `"+1"`, `"--1"`, `"9223372036854775808"`, `-1`} {
		require.Error(t, i.UnmarshalJSON([]byte(bad)), bad)
	}
	require.NoError(t, i.UnmarshalJSON([]byte(`"-9223372036854775808"`)))
	require.Equal(t, I64(-9223372036854775808), i)
	require.NoError(t, u.UnmarshalJSON([]byte(`"0"`)))
	require.Equal(t, U64(0), u)
}

func TestUnmarshalRequiresCanonical(t *testing.T) {
	t.Parallel()

	var d canonDoc
	require.NoError(t, Unmarshal([]byte("{\n  \"a\": \"7\",\n  \"b\": []\n}\n"), &d))
	require.Equal(t, canonDoc{A: 7, B: []bool{}}, d)

	for _, bad := range []string{
		"{\n  \"b\": [],\n  \"a\": \"7\"\n}\n", // key order
		"{\"a\":\"7\",\"b\":[]}",               // form
		"{\n  \"a\": \"7\",\n  \"b\": [],\n  \"c\": 1\n}\n",
		"{\n  \"a\": \"7\",\n  \"b\": []\n}\n{}\n",
	} {
		require.Error(t, Unmarshal([]byte(bad), &d), bad)
	}
}

func TestCoverageFrom(t *testing.T) {
	t.Parallel()

	id := tracepack.UUID{0x01, 0x92, 0x3c, 0x4d, 0x5e, 0x6f, 0x70, 0x81, 0x92, 0xa3, 0xb4, 0xc5, 0xd6, 0xe7, 0xf8, 0x09}
	c := tracepack.Coverage{
		CaptureID: &id, SeqFirst: new(uint64(5)), SeqLast: new(uint64(9223372036854775807)),
		TimeStart: new(int64(-9223372036854775808)), TimeEnd: new(int64(0)),
		Unknown: []tracepack.RawEntry{{Tag: 0x8001, Type: 6, Value: []byte("hi")}, {Tag: 0x0006, Type: 9, Value: nil}},
	}
	got, err := Marshal(CoverageFrom(&c))
	require.NoError(t, err)
	require.Equal(t, `{
  "capture_id": "01923c4d-5e6f-7081-92a3-b4c5d6e7f809",
  "seq_first": "5",
  "seq_last": "9223372036854775807",
  "time_start": "-9223372036854775808",
  "time_end": "0",
  "unknown": [
    {
      "tag": 32769,
      "value_type": 6,
      "value": "aGk="
    },
    {
      "tag": 6,
      "value_type": 9,
      "value": ""
    }
  ]
}
`, string(got))

	got, err = Marshal(CoverageFrom(&tracepack.Coverage{SeqFirst: new(uint64(1))}))
	require.NoError(t, err)
	require.Equal(t, "{\n  \"seq_first\": \"1\"\n}\n", string(got))
}

// Marshal returns d in the canonical form.
func (d *canonDoc) Marshal() ([]byte, error) {
	return Marshal(d)
}

// TestCoverageMatchesJSONLHeader checks that CoverageFrom renders each coverage entry as the export's header line does
// (the tracepack JSONL specification §4), unknown nested entries included.
func TestCoverageMatchesJSONLHeader(t *testing.T) {
	t.Parallel()

	other := tracepack.UUID{0x01, 0x9a, 0x2b, 0x3c, 0x4d, 0x5e, 0x70, 0x09, 0x80, 0, 0, 0, 0, 0, 0, 0x09}
	meta := testMeta()
	meta.Coverage = []tracepack.Coverage{
		{
			CaptureID: &other, SeqFirst: new(uint64(1)), SeqLast: new(uint64(9223372036854775807)),
			TimeStart: new(int64(-9223372036854775808)), TimeEnd: new(int64(0)),
			Unknown: []tracepack.RawEntry{
				{Tag: 0x8001, Type: 6, Value: []byte("h\"i<>&\u2028")},
				{Tag: 0x0040, Type: 9, Value: []byte{}},
				{Tag: 0x0006, Type: 8, Value: []byte{1, 2, 3}},
			},
		},
		{SeqFirst: new(uint64(2))},
		{Unknown: []tracepack.RawEntry{{Tag: 0x7FFF, Type: 4, Value: bytes.Repeat([]byte{0xff}, 8)}}},
		{},
	}
	r := openPack(t, writePack(t, meta, [][]tracepack.Record{{dataRecord(0, 1, nil)}}, true))
	var out bytes.Buffer
	_, err := tracepack.ExportJSONL(t.Context(), r, &out)
	require.NoError(t, err)

	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var hdr, metadata map[string]json.RawMessage
	var coverage []json.RawMessage
	require.NoError(t, json.Unmarshal(line, &hdr))
	require.NoError(t, json.Unmarshal(hdr["metadata"], &metadata))
	require.NoError(t, json.Unmarshal(metadata["coverage"], &coverage))
	got := r.Header().Meta.Coverage
	require.Len(t, coverage, len(meta.Coverage))
	require.Len(t, got, len(meta.Coverage))
	for i := range got {
		want, err := Canonical(coverage[i])
		require.NoError(t, err)
		require.Equal(t, string(want), marshalJSON(t, CoverageFrom(&got[i])), "entry %d", i)
	}
}
