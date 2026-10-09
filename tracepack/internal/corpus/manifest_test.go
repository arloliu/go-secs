package corpus

import (
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
  "corpus": "tracepack-corpus/1",
  "spec_version": "2.27",
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

	m := NewManifest("e", []Vector{ok, ok})
	_, err := m.Marshal()
	require.Error(t, err, "a vector id twice")
	m = NewManifest("e", []Vector{ok})
	_, err = m.Marshal()
	require.NoError(t, err)
}
