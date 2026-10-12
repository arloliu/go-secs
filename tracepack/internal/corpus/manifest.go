package corpus

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Values that identify the corpus (the tracepack corpus specification §3).
const (
	// CorpusSchema names the schemas of every file of the corpus.
	CorpusSchema = "tracepack-corpus/4"
	// SpecVersion is the spec version the goldens follow.
	SpecVersion = "2.33"
	// FormatVersion is the format revision the corpus targets;
	// a compatibility or rejection vector may hold a pack of another version.
	FormatVersion = "1.1"
)

// Export schemas of a vector's .jsonl file (the tracepack JSONL specification §5).
const (
	// JSONLSchema1 is the export schema of a pack whose format_minor is 0.
	JSONLSchema1 = "tracepack-jsonl/1"
	// JSONLSchema2 is the export schema of a pack whose format_minor is 1 or more.
	JSONLSchema2 = "tracepack-jsonl/2"
)

// JSONLSchemaOf returns the export schema of a pack of format_minor minor.
func JSONLSchemaOf(minor uint16) string {
	if minor == 0 {
		return JSONLSchema1
	}

	return JSONLSchema2
}

// Vector classes (the tracepack corpus specification §2).
const (
	ClassRead       = "read"
	ClassRejection  = "rejection"
	ClassTruncation = "truncation"
)

// Vector labels (the tracepack corpus specification §3).
const (
	LabelDamaged             = "damaged"
	LabelNonconformingWriter = "nonconforming-writer"
	LabelPreV213Sample       = "pre-v2.13-sample"
)

// Values of a vector's codec (the tracepack corpus specification §3).
const (
	CodecNone  = "none"
	CodecZstd  = "zstd"
	CodecMixed = "mixed"
)

// Values of a vector's source (the tracepack corpus specification §3).
const (
	SourceGenerated = "generated"
	SourceFixed     = "fixed"
)

// File names of a vector directory (the tracepack corpus specification §2).
const (
	FilePack       = "pack.tpk"
	FileExport     = "export.jsonl"
	FileRejection  = "rejection.json"
	FileFields     = "fields.json"
	FileFooter     = "footer.json"
	FileTruncation = "truncation.json"
)

// fileOrder lists the file names in the order of the table of the tracepack corpus specification §2,
// the order of a vector's files.
var fileOrder = []string{FilePack, FileExport, FileRejection, FileFields, FileFooter, FileTruncation}

// classFiles gives, by class, the files a vector of the class always holds and those it may hold
// (the tracepack corpus specification §2).
var classFiles = map[string]struct{ required, optional []string }{
	ClassRead:       {required: []string{FilePack, FileExport}, optional: []string{FileFields, FileFooter}},
	ClassRejection:  {required: []string{FilePack, FileRejection}, optional: []string{FileFields, FileFooter}},
	ClassTruncation: {required: []string{FilePack, FileExport, FileTruncation}, optional: []string{FileFields, FileFooter}},
}

var (
	// kebabID matches a kebab-case ASCII id.
	kebabID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// vectorGroups holds the group prefixes of vector ids (the tracepack corpus specification §9.4).
	vectorGroups = []string{
		"basic-", "framing-", "footer-", "hsms-", "validation-", "verify-", "repair-", "bootstrap-", "sem-", "sample-",
		"redact-", "thin-", "close-",
	}
)

// Manifest is manifest.json (the tracepack corpus specification §3).
type Manifest struct {
	Corpus        string   `json:"corpus"`
	SpecVersion   string   `json:"spec_version"`
	FormatVersion string   `json:"format_version"`
	ZstdEncoder   string   `json:"zstd_encoder"`
	Vectors       []Vector `json:"vectors"`
}

// Vector is one vector entry of the manifest.
type Vector struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Cites  []string `json:"cites"`
	Class  string   `json:"class"`
	Labels []string `json:"labels"`
	Codec  string   `json:"codec"`
	// JSONLSchema is the export schema of a vector of class read or truncation, JSONLSchemaOf its pack's format_minor;
	// empty, and omitted, for a rejection vector.
	JSONLSchema string   `json:"jsonl_schema,omitempty"`
	Source      string   `json:"source"`
	Files       []string `json:"files"`
	// Cases is omitted when empty: the key is never present with no element.
	Cases []Case `json:"cases,omitempty"`
}

// Case is a single-record sub-vector of a vector.
type Case struct {
	ID    string   `json:"id"`
	Seq   U64      `json:"seq"`
	Cites []string `json:"cites"`
}

// NewManifest returns the manifest of vectors, with the corpus identification of this package
// and zstdEncoder, the identity of the encoder that made the corpus's encoder-made zstd bytes.
func NewManifest(zstdEncoder string, vectors []Vector) Manifest {
	return Manifest{
		Corpus: CorpusSchema, SpecVersion: SpecVersion, FormatVersion: FormatVersion,
		ZstdEncoder: zstdEncoder, Vectors: vectors,
	}
}

// Marshal checks m and returns manifest.json in the canonical form,
// its arrays in their schema's order:
// vectors ascending by id, labels ascending, files in the order of the corpus specification §2, cases ascending by seq.
// m itself is not reordered.
//
// Returns:
//   - []byte: the file's bytes.
//   - error: an id that is not kebab-case, a vector id without a group prefix or given twice,
//     an empty title or cites, a class, label, codec, source or file name the schema does not define, a file given twice,
//     a jsonl_schema missing from a read or truncation vector, present in a rejection vector, or not an export schema,
//     a file the vector's class does not allow or a missing file it requires,
//     or a case id given twice within its vector.
func (m *Manifest) Marshal() ([]byte, error) {
	out := *m
	out.Vectors = make([]Vector, len(m.Vectors))
	for i := range m.Vectors {
		v, err := m.Vectors[i].normalized()
		if err != nil {
			return nil, err
		}
		out.Vectors[i] = v
	}
	slices.SortFunc(out.Vectors, func(a, b Vector) int { return strings.Compare(a.ID, b.ID) })
	for i := 1; i < len(out.Vectors); i++ {
		if out.Vectors[i].ID == out.Vectors[i-1].ID {
			return nil, fmt.Errorf("corpus: vector %q listed twice", out.Vectors[i].ID)
		}
	}

	return Marshal(&out)
}

// normalized returns a copy of v with its arrays in schema order, after checking its values.
func (v *Vector) normalized() (Vector, error) {
	out := *v
	if err := v.check(); err != nil {
		return Vector{}, err
	}

	out.Cites = append([]string{}, v.Cites...)
	out.Labels = append([]string{}, v.Labels...)
	slices.Sort(out.Labels)
	out.Files = append([]string{}, v.Files...)
	slices.SortFunc(out.Files, compareFiles)
	out.Cases = nil
	for _, c := range v.Cases {
		c.Cites = append([]string{}, c.Cites...)
		out.Cases = append(out.Cases, c)
	}
	slices.SortFunc(out.Cases, func(a, b Case) int { return cmp.Compare(a.Seq, b.Seq) })

	return out, nil
}

// check checks the values of v against the manifest schema.
func (v *Vector) check() error {
	if !kebabID.MatchString(v.ID) || !slices.ContainsFunc(vectorGroups, func(g string) bool { return strings.HasPrefix(v.ID, g) }) {
		return fmt.Errorf("corpus: vector id %q is not a kebab-case id with a group prefix", v.ID)
	}

	enums := []struct {
		name   string
		values []string
		in     []string
	}{
		{"class", []string{ClassRead, ClassRejection, ClassTruncation}, []string{v.Class}},
		{"codec", []string{CodecNone, CodecZstd, CodecMixed}, []string{v.Codec}},
		{"source", []string{SourceGenerated, SourceFixed}, []string{v.Source}},
		{"label", []string{LabelDamaged, LabelNonconformingWriter, LabelPreV213Sample}, v.Labels},
		{"file", fileOrder, v.Files},
	}
	for _, e := range enums {
		for _, s := range e.in {
			if !slices.Contains(e.values, s) {
				return fmt.Errorf("corpus: vector %q: %s %q is not defined", v.ID, e.name, s)
			}
		}
	}

	if err := v.checkClassFiles(); err != nil {
		return fmt.Errorf("corpus: vector %q: %w", v.ID, err)
	}
	if exported := v.Class != ClassRejection; exported != (v.JSONLSchema != "") ||
		(exported && v.JSONLSchema != JSONLSchema1 && v.JSONLSchema != JSONLSchema2) {
		return fmt.Errorf("corpus: vector %q of class %s: jsonl_schema %q", v.ID, v.Class, v.JSONLSchema)
	}

	switch {
	case v.Title == "" || len(v.Cites) == 0:
		return fmt.Errorf("corpus: vector %q: a title and at least one cite are required", v.ID)
	case hasDuplicate(v.Labels) || hasDuplicate(v.Files):
		return fmt.Errorf("corpus: vector %q: a label or file is listed twice", v.ID)
	}

	ids := make([]string, 0, len(v.Cases))
	for _, c := range v.Cases {
		if !kebabID.MatchString(c.ID) || len(c.Cites) == 0 {
			return fmt.Errorf("corpus: vector %q: case %q needs a kebab-case id and at least one cite", v.ID, c.ID)
		}
		ids = append(ids, c.ID)
	}
	if hasDuplicate(ids) {
		return fmt.Errorf("corpus: vector %q: a case id is listed twice", v.ID)
	}

	return nil
}

// compareFiles orders file names as the table of the tracepack corpus specification §2 does.
// An undefined name sorts first.
func compareFiles(a, b string) int {
	return cmp.Compare(slices.Index(fileOrder, a), slices.Index(fileOrder, b))
}

// hasDuplicate reports whether s holds a value twice.
func hasDuplicate[T cmp.Ordered](s []T) bool {
	sorted := slices.Sorted(slices.Values(s))

	return len(slices.Compact(sorted)) != len(s)
}

// checkClassFiles checks the files of v against its class (the tracepack corpus specification §2):
// every file the class requires, and no file the class neither requires nor allows.
func (v *Vector) checkClassFiles() error {
	allowed, ok := classFiles[v.Class]
	if !ok {
		return fmt.Errorf("class %q is not defined", v.Class)
	}
	for _, f := range allowed.required {
		if !slices.Contains(v.Files, f) {
			return fmt.Errorf("a vector of class %s without %s", v.Class, f)
		}
	}
	for _, f := range v.Files {
		if !slices.Contains(allowed.required, f) && !slices.Contains(allowed.optional, f) {
			return fmt.Errorf("file %s in a vector of class %s", f, v.Class)
		}
	}

	return nil
}
