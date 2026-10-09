package corpus

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManifestMarshal(t *testing.T) {
	t.Parallel()

	m := NewManifest("github.com/klauspost/compress v1.20.1", []Vector{
		{
			ID: "sem-item-validity", Title: "item validity", Cites: []string{"SEM §3", "CORPUS §7"}, Class: ClassRead,
			Codec: CodecNone, Source: SourceGenerated,
			Files: []string{FileClassify, FileVerify, FilePack, FileExport},
			Cases: []Case{
				{ID: "width-i2", Seq: 10, Cites: []string{"CORPUS §7"}},
				{ID: "body-short", Seq: 9, Cites: []string{"CORPUS §7"}},
			},
		},
		{
			ID: "basic-corrupt-middle", Title: "a corrupt middle block", Cites: []string{"FMT §16"}, Class: ClassRead,
			Labels: []string{LabelNonconformingWriter, LabelDamaged}, Codec: CodecMixed, Source: SourceFixed,
			Files: []string{FileQueries, FilePack, FileVerify, FileExport},
		},
	})
	b, err := m.Marshal()
	require.NoError(t, err)
	require.Equal(t, `{
  "corpus": "tracepack-corpus/2",
  "spec_version": "2.30",
  "format_version": "1.0",
  "jsonl_schema": "tracepack-jsonl/1",
  "zstd_encoder": "github.com/klauspost/compress v1.20.1",
  "vectors": [
    {
      "id": "basic-corrupt-middle",
      "title": "a corrupt middle block",
      "cites": [
        "FMT §16"
      ],
      "class": "read",
      "labels": [
        "damaged",
        "nonconforming-writer"
      ],
      "codec": "mixed",
      "source": "fixed",
      "files": [
        "pack.tpk",
        "export.jsonl",
        "verify.json",
        "queries.json"
      ]
    },
    {
      "id": "sem-item-validity",
      "title": "item validity",
      "cites": [
        "SEM §3",
        "CORPUS §7"
      ],
      "class": "read",
      "labels": [],
      "codec": "none",
      "source": "generated",
      "files": [
        "pack.tpk",
        "export.jsonl",
        "verify.json",
        "classify.json"
      ],
      "cases": [
        {
          "id": "body-short",
          "seq": "9",
          "cites": [
            "CORPUS §7"
          ]
        },
        {
          "id": "width-i2",
          "seq": "10",
          "cites": [
            "CORPUS §7"
          ]
        }
      ]
    }
  ]
}
`, string(b))
	require.Equal(t, "sem-item-validity", m.Vectors[0].ID, "Marshal does not reorder m")

	var back Manifest
	require.NoError(t, Unmarshal(b, &back))
	again, err := back.Marshal()
	require.NoError(t, err)
	require.Equal(t, string(b), string(again))
}

func TestManifestRefuses(t *testing.T) {
	t.Parallel()

	ok := Vector{ID: "basic-x", Title: "x", Cites: []string{"FMT §16"}, Class: ClassRead, Codec: CodecNone, Source: SourceGenerated, Files: []string{FilePack}}
	tests := []struct {
		name string
		edit func(*Vector)
	}{
		{"no group prefix", func(v *Vector) { v.ID = "other-x" }},
		{"not kebab-case", func(v *Vector) { v.ID = "basic_X" }},
		{"no title", func(v *Vector) { v.Title = "" }},
		{"no cites", func(v *Vector) { v.Cites = nil }},
		{"class", func(v *Vector) { v.Class = "reading" }},
		{"codec", func(v *Vector) { v.Codec = "lz4" }},
		{"source", func(v *Vector) { v.Source = "made" }},
		{"label", func(v *Vector) { v.Labels = []string{"broken"} }},
		{"label twice", func(v *Vector) { v.Labels = []string{LabelDamaged, LabelDamaged} }},
		{"file", func(v *Vector) { v.Files = []string{"pack.bin"} }},
		{"file twice", func(v *Vector) { v.Files = []string{FilePack, FilePack} }},
		{"case without cites", func(v *Vector) { v.Cases = []Case{{ID: "a", Seq: 1}} }},
		{"case id twice", func(v *Vector) {
			v.Cases = []Case{{ID: "a", Seq: 1, Cites: []string{"c"}}, {ID: "a", Seq: 2, Cites: []string{"c"}}}
		}},
		{"pack number with a leading zero", func(v *Vector) { v.Files = []string{"pack-01.tpk"} }},
		{"pack number with a sign", func(v *Vector) { v.Files = []string{"pack--1.tpk"} }},
		{"pack number out of range", func(v *Vector) { v.Files = []string{"pack-99999999999999999999.tpk"} }},
		{"numbered export", func(v *Vector) { v.Files = []string{"pack-1.jsonl"} }},
		{"numbered file twice", func(v *Vector) { v.Files = []string{PackFile(1), PackFile(1)} }},
		{"multi-pack case", func(v *Vector) {
			v.ID, v.Class, v.Files = "tx-x", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0), FileLookups}
			v.Cases = []Case{{ID: "a", Seq: 1, Cites: []string{"c"}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := ok
			tt.edit(&v)
			m := NewManifest("e", []Vector{v})
			_, err := m.Marshal()
			require.Error(t, err)
		})
	}

	multi := Vector{
		ID: "multi-x", Title: "x", Cites: []string{"SEM §7.4"}, Class: ClassMultiPack, Codec: CodecNone, Source: SourceGenerated,
		Files: []string{PackFile(0), PackVerifyFile(0), FileReads},
	}
	m := NewManifest("e", []Vector{multi})
	_, err := m.Marshal()
	require.NoError(t, err, "a multi-pack vector")

	m = NewManifest("e", []Vector{ok, ok})
	_, err = m.Marshal()
	require.Error(t, err, "a vector id twice")
	m = NewManifest("e", []Vector{ok})
	_, err = m.Marshal()
	require.NoError(t, err)
}

func TestManifestMultiPackFiles(t *testing.T) {
	t.Parallel()

	m := NewManifest("e", []Vector{{
		ID: "tx-files", Title: "numbered packs", Cites: []string{"CORPUS §2"}, Class: ClassMultiPack, Codec: CodecNone,
		Source: SourceGenerated,
		Files: []string{
			FileLookups, PackVerifyFile(10), PackFile(2), FileReads, PackVerifyFile(0), PackFile(10), PackVerifyFile(2), PackFile(0),
		},
	}})
	b, err := m.Marshal()
	require.NoError(t, err)
	require.Contains(t, string(b), `"files": [
        "pack-0.tpk",
        "pack-0.verify.json",
        "pack-2.tpk",
        "pack-2.verify.json",
        "pack-10.tpk",
        "pack-10.verify.json",
        "reads.json",
        "lookups.json"
      ]`)
	require.NotContains(t, string(b), `"cases"`)

	for _, tt := range []struct {
		name   string
		n      int
		verify bool
		ok     bool
	}{
		{"pack-0.tpk", 0, false, true},
		{"pack-12.verify.json", 12, true, true},
		{"pack-012.tpk", 0, false, false},
		{"pack-1.jsonl", 0, false, false},
		{"pack.tpk", 0, false, false},
	} {
		n, verify, ok := ParseNumberedFile(tt.name)
		require.Equal(t, []any{tt.n, tt.verify, tt.ok}, []any{n, verify, ok}, tt.name)
	}
	require.Negative(t, compareFiles(FileTruncation, PackFile(0)), "the numbered files follow truncation.json")
	require.Negative(t, compareFiles(PackVerifyFile(math.MaxInt), FileReads), "reads.json follows every numbered file")
}
