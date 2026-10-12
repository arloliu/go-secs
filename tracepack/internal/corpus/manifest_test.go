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
			Codec: CodecNone, JSONLSchema: JSONLSchema1, Source: SourceGenerated,
			Files: []string{FileFooter, FilePack, FileExport},
			Cases: []Case{
				{ID: "width-i2", Seq: 10, Cites: []string{"CORPUS §7"}},
				{ID: "body-short", Seq: 9, Cites: []string{"CORPUS §7"}},
			},
		},
		{
			ID: "basic-corrupt-middle", Title: "a corrupt middle block", Cites: []string{"FMT §16"}, Class: ClassRead,
			Labels: []string{LabelNonconformingWriter, LabelDamaged}, Codec: CodecMixed, JSONLSchema: JSONLSchema2, Source: SourceFixed,
			Files: []string{FileFields, FilePack, FileExport},
		},
		{
			ID: "bootstrap-bad-magic", Title: "a bad magic", Cites: []string{"FMT §13"}, Class: ClassRejection,
			Codec: CodecNone, Source: SourceGenerated, Files: []string{FileRejection, FilePack},
		},
	})
	b, err := m.Marshal()
	require.NoError(t, err)
	require.Equal(t, `{
  "corpus": "tracepack-corpus/4",
  "spec_version": "2.33",
  "format_version": "1.1",
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
      "jsonl_schema": "tracepack-jsonl/2",
      "source": "fixed",
      "files": [
        "pack.tpk",
        "export.jsonl",
        "fields.json"
      ]
    },
    {
      "id": "bootstrap-bad-magic",
      "title": "a bad magic",
      "cites": [
        "FMT §13"
      ],
      "class": "rejection",
      "labels": [],
      "codec": "none",
      "source": "generated",
      "files": [
        "pack.tpk",
        "rejection.json"
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
      "jsonl_schema": "tracepack-jsonl/1",
      "source": "generated",
      "files": [
        "pack.tpk",
        "export.jsonl",
        "footer.json"
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

// TestManifestGroups checks that the groups of format 1.1 are vector groups (the tracepack corpus specification §9.4).
func TestManifestGroups(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"redact-x", "thin-x", "close-x"} {
		v := Vector{
			ID: id, Title: "x", Cites: []string{"CORPUS §9.4"}, Class: ClassRead, Codec: CodecNone, JSONLSchema: JSONLSchema2,
			Source: SourceGenerated, Files: []string{FilePack, FileExport},
		}
		m := NewManifest("e", []Vector{v})
		_, err := m.Marshal()
		require.NoError(t, err, id)
	}
}

// removedFiles are file names of tracepack-corpus/2 that tracepack-corpus/3 removes
// (the tracepack corpus specification §10).
var removedFiles = []string{
	"verify.json", "queries.json", "classify.json", "repair.json", "patch.tpk", "patch.jsonl", "patch.verify.json",
	"pack-0.tpk", "pack-0.verify.json", "pack-1.jsonl", "reads.json", "lookups.json",
}

// vectorEdit names an edit of a vector entry.
type vectorEdit struct {
	name string
	edit func(*Vector)
}

func TestManifestRefuses(t *testing.T) {
	t.Parallel()

	ok := Vector{
		ID: "basic-x", Title: "x", Cites: []string{"FMT §16"}, Class: ClassRead, Codec: CodecNone, JSONLSchema: JSONLSchema1,
		Source: SourceGenerated, Files: []string{FilePack, FileExport},
	}
	tests := make([]vectorEdit, 0, 20+len(removedFiles))
	tests = append(tests, []vectorEdit{
		{"no group prefix", func(v *Vector) { v.ID = "other-x" }},
		{"the multi- group", func(v *Vector) { v.ID = "multi-x" }},
		{"the tx- group", func(v *Vector) { v.ID = "tx-x" }},
		{"not kebab-case", func(v *Vector) { v.ID = "basic_X" }},
		{"no title", func(v *Vector) { v.Title = "" }},
		{"no cites", func(v *Vector) { v.Cites = nil }},
		{"class", func(v *Vector) { v.Class = "reading" }},
		{"the class repair", func(v *Vector) { v.Class = "repair" }},
		{"the class multi-pack", func(v *Vector) {
			v.Class, v.Files = "multi-pack", []string{"pack-0.tpk", "pack-0.verify.json", "reads.json"}
		}},
		{"codec", func(v *Vector) { v.Codec = "lz4" }},
		{"no jsonl_schema in a read vector", func(v *Vector) { v.JSONLSchema = "" }},
		{"an undefined jsonl_schema", func(v *Vector) { v.JSONLSchema = "tracepack-jsonl/3" }},
		{"a jsonl_schema in a rejection vector", func(v *Vector) { v.Class, v.Files = ClassRejection, []string{FilePack, FileRejection} }},
		{"source", func(v *Vector) { v.Source = "made" }},
		{"label", func(v *Vector) { v.Labels = []string{"broken"} }},
		{"label twice", func(v *Vector) { v.Labels = []string{LabelDamaged, LabelDamaged} }},
		{"file", func(v *Vector) { v.Files = append(v.Files, "pack.bin") }},
		{"file twice", func(v *Vector) { v.Files = append(v.Files, FilePack) }},
		{"case without cites", func(v *Vector) { v.Cases = []Case{{ID: "a", Seq: 1}} }},
		{"case id twice", func(v *Vector) {
			v.Cases = []Case{{ID: "a", Seq: 1, Cites: []string{"c"}}, {ID: "a", Seq: 2, Cites: []string{"c"}}}
		}},
	}...)
	for _, f := range removedFiles {
		tests = append(tests, vectorEdit{"the removed file " + f, func(v *Vector) { v.Files = append(v.Files, f) }})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := ok
			v.Files = append([]string{}, ok.Files...)
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

// classFilesCase is a vector of class, listing files, and whether its entry is valid.
type classFilesCase struct {
	name  string
	class string
	files []string
	ok    bool
}

// TestVectorClassFiles checks that a vector's entry lists exactly the files its class allows
// (the tracepack corpus specification §2), in the manifest and in the layout check:
// pack.tpk always; export.jsonl for read and truncation, rejection.json for rejection, truncation.json for truncation;
// fields.json and footer.json optional in every class; no other file, a file of tracepack-corpus/2 included.
func TestVectorClassFiles(t *testing.T) {
	t.Parallel()

	tests := make([]classFilesCase, 0, 14+len(removedFiles))
	tests = append(tests, []classFilesCase{
		{"a read vector", ClassRead, []string{FilePack, FileExport}, true},
		{"a read vector with fields.json and footer.json", ClassRead, []string{FilePack, FileExport, FileFields, FileFooter}, true},
		{"a rejection vector", ClassRejection, []string{FilePack, FileRejection}, true},
		{"a rejection vector with footer.json", ClassRejection, []string{FilePack, FileRejection, FileFooter}, true},
		{"a truncation vector", ClassTruncation, []string{FilePack, FileExport, FileTruncation}, true},
		{"a read vector without its pack", ClassRead, []string{FileExport}, false},
		{"a read vector without its export", ClassRead, []string{FilePack}, false},
		{"an export in a rejection vector", ClassRejection, []string{FilePack, FileRejection, FileExport}, false},
		{"a rejection vector without rejection.json", ClassRejection, []string{FilePack}, false},
		{"rejection.json in a read vector", ClassRead, []string{FilePack, FileExport, FileRejection}, false},
		{"truncation.json in a read vector", ClassRead, []string{FilePack, FileExport, FileTruncation}, false},
		{"a truncation vector without truncation.json", ClassTruncation, []string{FilePack, FileExport}, false},
		{"the class repair", "repair", []string{FilePack, FileExport}, false},
		{"the class multi-pack", "multi-pack", []string{"pack-0.tpk", "pack-0.verify.json", "reads.json"}, false},
	}...)
	for _, f := range removedFiles {
		tests = append(tests, classFilesCase{"the removed file " + f, ClassRead, []string{FilePack, FileExport, f}, false})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := Vector{
				ID: "basic-x", Title: "x", Cites: []string{"CORPUS §2"}, Class: tt.class, Codec: CodecNone, Source: SourceGenerated, Files: tt.files,
			}
			if tt.class != ClassRejection {
				v.JSONLSchema = JSONLSchema1
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
