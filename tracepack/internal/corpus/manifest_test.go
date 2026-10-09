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

	// Packs 0 to 10, listed backwards: Marshal orders them by number, not by name.
	files, want := make([]string, 0, 24), make([]string, 0, 24)
	for n := 10; n >= 0; n-- {
		files = append(files, PackVerifyFile(n), PackFile(n))
	}
	for n := range 11 {
		want = append(want, PackFile(n), PackVerifyFile(n))
	}
	files = append([]string{FileLookups}, append(files, FileReads)...)
	want = append(want, FileReads, FileLookups)
	m := NewManifest("e", []Vector{{
		ID: "tx-files", Title: "numbered packs", Cites: []string{"CORPUS §2"}, Class: ClassMultiPack, Codec: CodecNone,
		Source: SourceGenerated, Files: files,
	}})
	b, err := m.Marshal()
	require.NoError(t, err)
	var got Manifest
	require.NoError(t, Unmarshal(b, &got))
	require.Equal(t, want, got.Vectors[0].Files)
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

// TestVectorClassFiles checks that a vector's entry lists the files its class allows (the tracepack corpus specification §2),
// in both directions: no numbered pack, reads.json or lookups.json in a single-pack vector,
// and in a multi-pack vector packs numbered from 0 without a gap, each with its verification report, a result file, and nothing else.
func TestVectorClassFiles(t *testing.T) {
	t.Parallel()

	multi := []string{PackFile(0), PackVerifyFile(0), PackFile(1), PackVerifyFile(1), FileReads}
	single := []string{FilePack, FileExport, FileVerify}
	tests := []struct {
		name  string
		class string
		files []string
		ok    bool
	}{
		{"a multi-pack vector", ClassMultiPack, multi, true},
		{"a multi-pack vector with lookups only", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0), FileLookups}, true},
		{"a read vector", ClassRead, single, true},
		{"a gap in the pack numbers", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0), PackFile(2), PackVerifyFile(2), FileReads}, false},
		{"no pack 0", ClassMultiPack, []string{PackFile(1), PackVerifyFile(1), FileReads}, false},
		{"a missing verification report", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0), PackFile(1), FileReads}, false},
		{"a verification report without its pack", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0), PackVerifyFile(1), FileReads}, false},
		{"no reads or lookups", ClassMultiPack, []string{PackFile(0), PackVerifyFile(0)}, false},
		{"no packs", ClassMultiPack, []string{FileReads}, false},
		{"pack.tpk in a multi-pack vector", ClassMultiPack, append([]string{FilePack}, multi...), false},
		{"verify.json in a multi-pack vector", ClassMultiPack, append([]string{FileVerify}, multi...), false},
		{"export.jsonl in a multi-pack vector", ClassMultiPack, append([]string{FileExport}, multi...), false},
		{"queries.json in a multi-pack vector", ClassMultiPack, append([]string{FileQueries}, multi...), false},
		{"a numbered pack in a read vector", ClassRead, append([]string{PackFile(0)}, single...), false},
		{"a numbered verification report in a repair vector", ClassRepair, append([]string{PackVerifyFile(0)}, single...), false},
		{"reads.json in a read vector", ClassRead, append([]string{FileReads}, single...), false},
		{"lookups.json in a rejection vector", ClassRejection, []string{FilePack, FileRejection, FileLookups}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := Vector{
				ID: "multi-x", Title: "x", Cites: []string{"CORPUS §2"}, Class: tt.class, Codec: CodecNone, Source: SourceGenerated, Files: tt.files,
			}
			m := NewManifest("e", []Vector{v})
			_, err := m.Marshal()
			committed := map[string][]byte{FileManifest: nil, FilePrimitives: nil}
			for _, f := range tt.files {
				committed[v.ID+"/"+f] = nil
			}
			problems := checkLayout(committed, &m)
			if tt.ok {
				require.NoError(t, err)
				require.Empty(t, problems)

				return
			}
			require.Error(t, err)
			require.NotEmpty(t, problems, "the layout check")
		})
	}
}
