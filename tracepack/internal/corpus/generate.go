package corpus

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Files at the corpus root (the tracepack corpus specification §2).
const (
	FileManifest   = "manifest.json"
	FilePrimitives = "primitives.json"
)

// Identity of the zstd encoder (the tracepack corpus specification §6.1).
const (
	// compressModule is the module whose zstd encoder makes the corpus's encoder-made zstd bytes.
	compressModule = "github.com/klauspost/compress"
	// UnknownEncoder is the zstd_encoder Generate records when it cannot establish the encoder.
	UnknownEncoder = "unknown"
)

// ErrNoEncoder reports that the zstd encoder the build links cannot be established.
var ErrNoEncoder = errors.New("corpus: the linked zstd encoder is not known")

// groups lists the vector groups of the corpus, one function per vectors_<group>.go file.
// A group adds its vectors by adding its function here.
var groups = []func() []Recipe{
	basicVectors,
	bootstrapVectors,
	footerVectors,
	framingVectors,
	hsmsVectors,
	repairVectors,
	sampleVectors,
	semVectors,
	validationVectors,
	verifyVectors,
	versionVectors,
	thinVectors,
	redactVectors,
	closeVectors,
}

// Recipe describes one vector of the corpus and how the generator makes it (the tracepack corpus specification §3).
type Recipe struct {
	// ID, Title, Cites, Class, Labels and Cases are the vector's manifest entry.
	ID     string
	Title  string
	Cites  []string
	Class  string
	Labels []string
	Cases  []Case
	// Codec is the manifest's codec of the pack, CodecNone when empty.
	// The generator checks it against the pack's blocks and footer.
	Codec string
	// Source is SourceGenerated when empty.
	Source string
	// IdentitySeed seeds the vector's identifiers (see IDFor); the vector's ID when empty.
	// Vectors that differ only in codec share a seed, so they share every identifier and export identically,
	// which the generator checks.
	IdentitySeed string
	// EncoderMade reports a pack that holds a block body or footer of codec zstd made by the running zstd encoder,
	// so that a regeneration under another encoder compares the vector by decoded equivalence
	// (the tracepack corpus specification §6.1).
	// The generator checks it: such a body decodes in full, and a hand-built damaged frame does not.
	EncoderMade bool
	// Build makes the vector's pack and the inputs of its optional files from the identity seed.
	Build func(seed string) (*Built, error)
	// Classify holds the classifier expectations of the pack's records, written by hand; nil for none.
	// The generator checks the statuses the pack stores against them,
	// and the tests of the classify package check the classifier against them (ClassifierVectors).
	Classify *Classify
	// Expect is what the vector must show, written by hand from the specification, never from the generator's output.
	// Every recipe has one; the generator refuses a recipe without it and fails, before writing anything,
	// when a read disagrees with it.
	// Its verification, query and cut expectations are checked in memory and written to no file.
	Expect *Expectation
}

// Built is what a recipe's Build makes: the pack and the inputs of the vector's optional files and of its queries.
type Built struct {
	// Pack is pack.tpk.
	Pack []byte
	// Queries are the queries the generator runs over the pack, without their results, which the reads give;
	// it checks the results against the expectation and writes no file of them.
	Queries []QuerySpec
	// Fields adds fields.json.
	Fields bool
	// Footer adds footer.json.
	Footer bool
}

// QuerySpec is a query over a vector's pack without its expect.
type QuerySpec struct {
	ID     string
	Filter Filter
}

// Pos is a file offset a recipe names by the pack's layout, as Locate finds it, or by its value,
// plus a number of bytes.
// The zero Pos names no offset.
type Pos struct {
	kind  posKind
	block int
	// at is the offset a Pos of kind posOffset names.
	at uint64
	// plus is added to the offset the Pos names.
	plus uint64
}

// posKind says what a Pos names.
type posKind uint8

const (
	posNone posKind = iota
	posBlock
	posEnd
	posOffset
)

// Expectation is a vector's expectation, written by hand from the specification.
// The generator checks every field against its reads of the vector before it writes anything;
// the verification report, the queries and the verification of each cut are read in memory only.
type Expectation struct {
	// Rejection is the bootstrap rejection code of a rejection vector.
	Rejection string
	// Outcome is the verification outcome of any other vector.
	Outcome tracepack.VerifyOutcome
	// Unfinalized reports a pack that is not finalized (I-5): the verification report's finalized is its negation.
	Unfinalized bool
	// FooterValid is the verification report's footer_valid.
	FooterValid bool
	// Blocks is the number of blocks located.
	Blocks int
	// Seqs are the seqs of the exported records, in export order.
	Seqs []uint64
	// Failed are the failed blocks with their causes, ascending by block.
	Failed []FailedWant
	// WalkStop is where the forward walk stopped; the zero Pos when it did not.
	WalkStop Pos
	// PrefixEnd is where the validated prefix ends; it is required.
	PrefixEnd Pos
	// Disagreeing are the indices of the blocks whose records disagree with their F-2 entry or F-3 summary, ascending.
	Disagreeing []int
	// TrailerTotalsDisagree is the verification report's trailer_totals_disagree.
	TrailerTotalsDisagree bool
	// WriterDefects are the writer defects, in the verification report's order.
	WriterDefects []DefectWant
	// Lost are the verification report's lost entries, in order; each also names the pack's capture_id, which the generator checks.
	Lost []LostWant
	// ExportHas are strings export.jsonl must contain, each a value the JSONL specification renders.
	ExportHas []string
	// Stats, when set, are the statistics Reader.Stats gives.
	Stats *StatsWant
	// Footer is footer.json of a vector whose Built asks for it, and must then be set:
	// its stored part, whether the footer is accepted, and its recomputed part.
	Footer *Footer
	// Fields is fields.json of a vector whose Built asks for it, and must then be set:
	// one row per data or control record, ascending by seq.
	Fields []Fields
	// Queries are the results of each query of Built.Queries, by id.
	Queries []QueryWant
	// Cuts are the expectations of the cuts of a truncation vector, ascending, one per run of cut lengths that share it:
	// each names where its run starts, and its run ends where the next starts, the last at the end of the pack.
	// Runs whose rows of truncation.json are equal are one row of the file.
	Cuts []CutWant
}

// CutWant is the expectation of a run of cuts: the first cut length it covers and the expectation of its cuts,
// a rejection code or the outcome, the records exported and validated and where the validated prefix and the walk end.
// Positions are those of the base pack, which every cut shares up to its length.
type CutWant struct {
	From      Pos
	Rejection string
	Outcome   tracepack.VerifyOutcome
	Validated int
	Records   uint64
	PrefixEnd Pos
	WalkStop  Pos
}

// FailedWant is a failed block and its cause, IncompleteReasonCorruptBlock or IncompleteReasonUnknownCodec.
type FailedWant struct {
	Block int
	Cause tracepack.IncompleteReason
}

// DefectWant is a writer defect: its kind and its seq.
type DefectWant struct {
	Kind tracepack.WriterDefectKind
	Seq  uint64
}

// LostWant is a lost entry of the verification report: a closed range of seqs and the time interval of its block.
type LostWant struct {
	First, Last        uint64
	TimeStart, TimeEnd int64
}

// StatsWant is what Reader.Stats gives: whether the statistics are available, and when they are,
// every epoch with its close_seq, ascending by epoch, and every boundary, ascending by seq.
type StatsWant struct {
	Available  bool
	Epochs     []EpochWant
	Boundaries []BoundaryWant
}

// EpochWant is an epoch of the statistics and its close_seq, nil when no record ends it.
type EpochWant struct {
	Epoch    uint32
	CloseSeq *uint64
}

// BoundaryWant is a boundary of the statistics.
type BoundaryWant struct {
	Seq  uint64
	Kind tracepack.BoundaryKind
}

// QueryWant is the result of the query of id: the seqs it yields and its incomplete reasons, in their order.
type QueryWant struct {
	ID         string
	Seqs       []uint64
	Incomplete []IncompleteWant
}

// IncompleteWant is an incomplete reason of a query: its reason, its block (-1 for none) and its offset.
type IncompleteWant struct {
	Reason tracepack.IncompleteReason
	Block  int
	At     Pos
}

// file is one file of a vector.
type file struct {
	name string
	data []byte
}

// readInputs is what the reads of a vector take besides its pack.
type readInputs struct {
	class    string
	queries  []QuerySpec
	fields   bool
	footer   bool
	classify *Classify
}

// readOutputs is the reads of a vector: its files, in the order of the tracepack corpus specification §2,
// and what Expect is checked against, the verification report, the queries and the cuts' verification read in memory only.
type readOutputs struct {
	files     []file
	rejection string
	// jsonlSchema is the export schema of a pack that opens, by its format_minor.
	jsonlSchema string
	verify      Verify
	export      []byte
	seqs        []uint64
	queries     []QueryVector
	stats       tracepack.PackStats
	statsOK     bool
	fields      FieldRows
	footer      *Footer
	// cuts are the expectations of every run of cuts of a truncation vector, before truncation.json merges them.
	cuts []CutRow
}

// AtBlock names the envelope of block i.
func AtBlock(i int) Pos {
	return Pos{kind: posBlock, block: i}
}

// AtEnd names where the walk of the pack's blocks ends:
// footer_offset in a finalized pack, else the end of the last whole block, or the end of the pack metadata without one.
func AtEnd() Pos {
	return Pos{kind: posEnd}
}

// AtOffset names the file offset off.
func AtOffset(off uint64) Pos {
	return Pos{kind: posOffset, at: off}
}

// Plus returns the position n bytes after p.
func (p Pos) Plus(n uint64) Pos {
	p.plus += n
	return p
}

// Truncated is the pack-level truncated reason of a query, at at (the zero Pos for none).
func Truncated(at Pos) IncompleteWant {
	return IncompleteWant{Reason: tracepack.IncompleteReasonTruncated, Block: -1, At: at}
}

// BlockDefect is the reason of a query for block i, at its envelope.
func BlockDefect(reason tracepack.IncompleteReason, i int) IncompleteWant {
	return IncompleteWant{Reason: reason, Block: i, At: AtBlock(i)}
}

// CoverageHit is the coverage reason of a query.
func CoverageHit() IncompleteWant {
	return IncompleteWant{Reason: tracepack.IncompleteReasonCoverage, Block: -1}
}

// Recipes returns every recipe of the corpus, ascending by id.
//
// Returns:
//   - []Recipe: the recipes.
//   - error: an id given twice.
func Recipes() ([]Recipe, error) {
	out := make([]Recipe, 0, len(groups))
	for _, g := range groups {
		out = append(out, g()...)
	}
	slices.SortStableFunc(out, func(a, b Recipe) int { return strings.Compare(a.ID, b.ID) })
	for i := 1; i < len(out); i++ {
		if out[i].ID == out[i-1].ID {
			return nil, fmt.Errorf("corpus: vector %q has two recipes", out[i].ID)
		}
	}

	return out, nil
}

// Generate builds every vector of the corpus and returns its files, keyed by their path from the corpus root,
// "manifest.json", "primitives.json" and "<id>/<file>".
// It builds the vectors one after another and checks each against its Expect, so it fails, before any file is written,
// when the implementation disagrees with a recipe.
// The manifest records the zstd encoder LinkedZstdEncoder establishes with GoListEncoder, or UnknownEncoder.
//
// Returns:
//   - map[string][]byte: the files.
//   - error: a recipe that is refused, fails to build, or disagrees with its Expect.
func Generate() (map[string][]byte, error) {
	recipes, err := Recipes()
	if err != nil {
		return nil, err
	}
	encoder, err := LinkedZstdEncoder(GoListEncoder)
	if err != nil {
		encoder = UnknownEncoder
	}

	return generateRecipes(context.Background(), recipes, encoder)
}

// GoListEncoder names the github.com/klauspost/compress module the build of the current directory links:
// it runs "go list -m" on it there, with the process's environment, so GOWORK, GOFLAGS and replace directives apply,
// and returns its line, "<path> <version>", followed by " => <path> <version>" for a replaced module.
// A test binary does not record its dependencies in its build information, so the go command names the module instead.
//
// Returns:
//   - string: the line.
//   - error: ErrNoEncoder when the go command is not found or fails.
func GoListEncoder() (string, error) {
	goCmd, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoEncoder, err)
	}
	// A nil Stdin reads from the null device, and a nil Env inherits the process's environment.
	cmd := exec.Command(goCmd, "list", "-m", "-f", "{{.Path}} {{.Version}}{{with .Replace}} => {{.Path}} {{.Version}}{{end}}", compressModule)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: go list: %w: %s", ErrNoEncoder, err, bytes.TrimSpace(stderr.Bytes()))
	}

	return string(out), nil
}

// LinkedZstdEncoder returns the identity of the zstd encoder the current build links, from the module line list returns (see ZstdEncoder).
//
// Parameters:
//   - list: names the module; GoListEncoder outside tests.
//
// Returns:
//   - string: the identity.
//   - error: ErrNoEncoder when list fails or its line names no module.
func LinkedZstdEncoder(list func() (string, error)) (string, error) {
	line, err := list()
	if err != nil {
		return "", err
	}

	return ZstdEncoder(line)
}

// ZstdEncoder returns the identity of the zstd encoder that the module line listed names, as GoListEncoder writes it:
// the path and version of the github.com/klauspost/compress module, or of its replacement when it is replaced,
// as "<path> <version>", or the path alone for a replacement without a version (a local directory).
// It is what the build links, a workspace's selection included, not a go.mod requirement.
//
// Returns:
//   - string: the identity.
//   - error: ErrNoEncoder for a line that is not the module's.
func ZstdEncoder(listed string) (string, error) {
	module, replace, replaced := strings.Cut(strings.TrimSpace(listed), " => ")
	m := strings.Fields(module)
	if len(m) == 0 || len(m) > 2 || m[0] != compressModule {
		return "", fmt.Errorf("%w: %q does not name %s", ErrNoEncoder, listed, compressModule)
	}
	if replaced {
		if m = strings.Fields(replace); len(m) == 0 || len(m) > 2 {
			return "", fmt.Errorf("%w: %q names no replacement", ErrNoEncoder, listed)
		}
	}
	if len(m) == 1 {
		if !replaced {
			return "", fmt.Errorf("%w: %q gives %s no version", ErrNoEncoder, listed, compressModule)
		}

		return m[0], nil
	}

	return m[0] + " " + m[1], nil
}

// generateRecipes builds the vectors of recipes, in order, and the root files, recording encoder in the manifest.
func generateRecipes(ctx context.Context, recipes []Recipe, encoder string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	vectors := make([]Vector, 0, len(recipes))
	type sibling struct {
		seed, id string
		export   []byte
	}
	var siblings []sibling
	for i := range recipes {
		rec := &recipes[i]
		v, out, err := generateVector(ctx, rec)
		if err != nil {
			return nil, fmt.Errorf("corpus: vector %s: %w", rec.ID, err)
		}
		for _, f := range out.files {
			files[rec.ID+"/"+f.name] = f.data
		}
		vectors = append(vectors, v)

		seed := rec.seed()
		if k := slices.IndexFunc(siblings, func(s sibling) bool { return s.seed == seed }); k >= 0 {
			if !bytes.Equal(siblings[k].export, out.export) {
				return nil, fmt.Errorf("corpus: vectors %s and %s share their identity but not their export", siblings[k].id, rec.ID)
			}
		} else {
			siblings = append(siblings, sibling{seed: seed, id: rec.ID, export: out.export})
		}
	}

	prim, err := corpusPrimitives(files)
	if err != nil {
		return nil, err
	}
	if err := prim.AddKeys(); err != nil {
		return nil, err
	}
	if files[FilePrimitives], err = prim.Marshal(); err != nil {
		return nil, err
	}
	m := NewManifest(encoder, vectors)
	if files[FileManifest], err = m.Marshal(); err != nil {
		return nil, err
	}

	return files, nil
}

// seed returns the recipe's identity seed.
func (rec *Recipe) seed() string {
	return cmp.Or(rec.IdentitySeed, rec.ID)
}

// check refuses a recipe that cannot be generated: no Build, or no Expect fitting its class.
func (rec *Recipe) check() error {
	e := rec.Expect
	switch {
	case rec.Build == nil:
		return errors.New("no Build")
	case e == nil:
		return errors.New("no Expect")
	case rec.Class == ClassRejection && (e.Rejection == "" || e.Outcome != 0):
		return errors.New("a rejection vector expects a rejection code and nothing else")
	case rec.Class != ClassRejection && (e.Rejection != "" || e.Outcome == 0 || e.PrefixEnd.kind == posNone):
		return errors.New("a vector that opens expects an outcome and a prefix end")
	case e.Outcome == tracepack.VerifyOutcomeUnfinalized && !e.Unfinalized:
		return errors.New("an unfinalized outcome of a pack expected finalized")
	case (rec.Class == ClassTruncation) != (len(e.Cuts) > 0):
		return errors.New("a truncation vector, and only one, expects its cuts")
	}

	return nil
}

// generateVector builds the vector of rec and checks it against rec's Expect.
func generateVector(ctx context.Context, rec *Recipe) (Vector, *readOutputs, error) {
	if err := rec.check(); err != nil {
		return Vector{}, nil, err
	}
	built, err := rec.Build(rec.seed())
	if err != nil {
		return Vector{}, nil, err
	}

	in := &readInputs{
		class: rec.Class, queries: built.Queries, fields: built.Fields, footer: built.Footer, classify: rec.Classify,
	}
	out, err := readVector(ctx, built.Pack, in)
	if err != nil {
		return Vector{}, nil, err
	}
	if err := rec.Expect.check(out, built.Pack); err != nil {
		return Vector{}, nil, fmt.Errorf("disagrees with its Expect: %w", err)
	}
	for _, c := range rec.Cases {
		if !slices.Contains(out.seqs, uint64(c.Seq)) {
			return Vector{}, nil, fmt.Errorf("case %s names seq %d, which the pack does not export", c.ID, c.Seq)
		}
	}

	return rec.vector(out)
}

// vector checks rec's codec declarations against the reads out and returns rec's manifest entry, listing the files of out.
func (rec *Recipe) vector(out *readOutputs) (Vector, *readOutputs, error) {
	if err := rec.checkCodec(out); err != nil {
		return Vector{}, nil, err
	}

	vec := Vector{
		ID: rec.ID, Title: rec.Title, Cites: rec.Cites, Class: rec.Class, Labels: nonNil(rec.Labels),
		Codec: cmp.Or(rec.Codec, CodecNone), JSONLSchema: out.jsonlSchema, Source: cmp.Or(rec.Source, SourceGenerated),
		Cases: rec.Cases,
	}
	for _, f := range out.files {
		vec.Files = append(vec.Files, f.name)
	}

	return vec, out, nil
}

// checkCodec checks rec's Codec and EncoderMade against pack.tpk of out, when Locate reads it:
// the manifest codec, and whether the pack holds encoder-made bytes.
// A fixed pack's zstd bytes were made by whatever encoder wrote it once, never by the running one,
// so it never depends on the encoder (the tracepack corpus specification §6.1).
func (rec *Recipe) checkCodec(out *readOutputs) error {
	made := false
	packCodec := ""
	for _, f := range out.files {
		if f.name != FilePack {
			continue
		}
		c, m, err := packCodecs(f.data)
		if err != nil {
			// A pack whose structures Locate refuses is declared by hand alone.
			continue
		}
		packCodec = c
		made = m && rec.Source != SourceFixed
	}
	if want := cmp.Or(rec.Codec, CodecNone); packCodec != "" && packCodec != want {
		return fmt.Errorf("its pack's codec is %s, the recipe says %s", packCodec, want)
	}
	if made != rec.EncoderMade {
		return fmt.Errorf("its pack holds encoder-made zstd bytes: %v, the recipe says %v", made, rec.EncoderMade)
	}

	return nil
}

// packCodecs returns the manifest codec of pack, from its blocks' codecs and, when finalized, its footer's,
// and whether it holds an encoder-made zstd body or footer: a zstd block body that decodes in full, or a zstd footer.
func packCodecs(pack []byte) (string, bool, error) {
	l, err := Locate(pack)
	if err != nil {
		return "", false, err
	}

	var codecs []uint8
	made := false
	for _, s := range l.Blocks {
		blk := pack[s.Offset : s.Offset+s.Len]
		codecs = append(codecs, blk[envelopeCodecOff])
		if blk[envelopeCodecOff] == codec.Zstd {
			if _, _, err := decodeBlock(blk); err == nil {
				made = true
			}
		}
	}
	if l.Finalized {
		c := pack[len(pack)-format.TrailerLen+trailerFooterCodecOff]
		codecs = append(codecs, c)
		made = made || c == codec.Zstd
	}

	switch {
	case !slices.ContainsFunc(codecs, func(c uint8) bool { return c != codec.None }):
		return CodecNone, made, nil
	case !slices.ContainsFunc(codecs, func(c uint8) bool { return c != codec.Zstd }):
		return CodecZstd, made, nil
	default:
		return CodecMixed, made, nil
	}
}

// readVector reads pack as a reader of the corpus does and returns the files the reads give, pack.tpk first:
// rejection.json for a rejection vector;
// else export.jsonl, and fields.json, footer.json or truncation.json as in asks.
// It also verifies the pack and runs the queries and the classifier checks in asks, in memory, for Expect.
// It is how the generator makes the goldens and how the conformance test reads the committed packs again.
func readVector(ctx context.Context, pack []byte, in *readInputs) (*readOutputs, error) {
	out := &readOutputs{files: []file{{FilePack, pack}}}
	if in.class == ClassRejection {
		code, err := RejectionOf(ctx, pack)
		if err != nil {
			return nil, err
		}
		out.rejection = code

		return out, out.add(FileRejection, &Rejection{Rejection: code})
	}

	r, err := openBytes(ctx, pack)
	if err != nil {
		return nil, err
	}
	if out.export, out.seqs, err = exportPack(ctx, r); err != nil {
		return nil, err
	}
	out.jsonlSchema = JSONLSchemaOf(r.Header().FormatMinor)
	if err := checkExportSchema(out.export, out.jsonlSchema); err != nil {
		return nil, err
	}
	out.files = append(out.files, file{FileExport, out.export})
	if out.verify, err = verifyBytes(ctx, pack); err != nil {
		return nil, err
	}
	out.stats, out.statsOK = r.Stats()

	if err := out.readOptional(ctx, r, pack, in); err != nil {
		return nil, err
	}

	if in.class == ClassTruncation {
		tb, err := truncationTable(ctx, pack, out.export)
		if err != nil {
			return nil, err
		}
		t, err := tb.Table()
		if err != nil {
			return nil, err
		}
		if err := out.add(FileTruncation, &t); err != nil {
			return nil, err
		}
		out.cuts = tb.Cuts()
	}

	slices.SortStableFunc(out.files, func(a, b file) int { return compareFiles(a.name, b.name) })

	return out, nil
}

// readOptional runs the queries and the classifier checks in asks, in memory,
// and adds the optional files in asks for: fields.json and footer.json.
func (out *readOutputs) readOptional(ctx context.Context, r *tracepack.Reader, pack []byte, in *readInputs) error {
	for _, q := range in.queries {
		e, err := RunQuery(ctx, r, &q.Filter)
		if err != nil {
			return fmt.Errorf("query %s: %w", q.ID, err)
		}
		out.queries = append(out.queries, QueryVector{ID: q.ID, Filter: q.Filter, Expect: e})
	}
	if in.fields {
		rows, err := ReadFields(ctx, r)
		if err != nil {
			return err
		}
		if err := out.add(FileFields, rows); err != nil {
			return err
		}
		out.fields = rows
	}
	if in.classify != nil {
		if err := checkClassify(ctx, r, in.classify); err != nil {
			return err
		}
	}
	if in.footer {
		f, err := FooterOf(ctx, pack)
		if err != nil {
			return err
		}
		if err := out.checkFooter(&f); err != nil {
			return err
		}
		out.footer = &f
		if err := out.add(FileFooter, &f); err != nil {
			return err
		}
	}

	return nil
}

// checkFooter checks footer.json f against the statistics of out (the tracepack corpus specification §5.8):
// the statistics of an accepted footer are its stored F-5, every epoch with its close_seq and every boundary.
// Whether f is accepted is FooterOf's own decision, which the recipes' expectations check.
func (out *readOutputs) checkFooter(f *Footer) error {
	if !f.Accepted {
		return nil
	}
	if f.Stored == nil || !out.statsOK {
		return errors.New("an accepted footer without its stored part or its statistics")
	}

	stats := FooterF5{Epochs: []F5Epoch{}, Boundaries: []Boundary{}, SeqRanges: []SeqRange{}}
	for _, e := range out.stats.Epochs {
		fe := F5Epoch{Epoch: e.Epoch}
		if e.CloseSeq != nil {
			fe.CloseSeq = new(U64(*e.CloseSeq))
		}
		stats.Epochs = append(stats.Epochs, fe)
	}
	bs := make([]boundaryEntry, 0, len(out.stats.Boundaries))
	for _, b := range out.stats.Boundaries {
		bs = append(bs, boundaryEntry{seq: b.Seq, kind: uint8(b.Kind), gapStart: b.GapStart, gapEnd: b.GapEnd})
	}
	// The statistics list the boundaries of one seq in the footer's order, which footer.json sorts.
	stats.Boundaries = namedBoundaries(bs)
	for _, r := range out.stats.SeqRanges {
		stats.SeqRanges = append(stats.SeqRanges, SeqRange{First: U64(r.First), Last: U64(r.Last)})
	}
	stored := f.Stored.F5
	if stored.SeqRanges == nil {
		stored.SeqRanges = []SeqRange{}
	}
	if !reflect.DeepEqual(stats, stored) {
		return fmt.Errorf("the statistics %+v are not the stored F-5 %+v", stats, f.Stored.F5)
	}

	return nil
}

// exportedRecord is the seq of a record line of an export.
type exportedRecord struct {
	Seq U64 `json:"seq"`
}

// add appends the file name holding doc's canonical form.
func (out *readOutputs) add(name string, doc Document) error {
	b, err := doc.Marshal()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	out.files = append(out.files, file{name, b})

	return nil
}

// openBytes opens the in-memory pack.
func openBytes(ctx context.Context, pack []byte) (*tracepack.Reader, error) {
	return tracepack.Open(ctx, bytes.NewReader(pack), int64(len(pack)), tracepack.ReaderOptions{})
}

// verifyBytes verifies the in-memory pack and returns the projection of its verification report.
func verifyBytes(ctx context.Context, pack []byte) (Verify, error) {
	rep, err := tracepack.Verify(ctx, bytes.NewReader(pack), int64(len(pack)), tracepack.VerifyOptions{})
	if err != nil {
		return Verify{}, err
	}

	return VerifyFrom(&rep)
}

// exportPack returns the export of r and the seqs of its record lines, in order.
func exportPack(ctx context.Context, r *tracepack.Reader) ([]byte, []uint64, error) {
	var buf bytes.Buffer
	if _, err := tracepack.ExportJSONL(ctx, r, &buf); err != nil {
		return nil, nil, err
	}
	records, err := exportRecords(buf.Bytes())
	if err != nil {
		return nil, nil, err
	}
	seqs := make([]uint64, 0, len(records))
	for _, rec := range records {
		seqs = append(seqs, uint64(rec.Seq))
	}

	return buf.Bytes(), seqs, nil
}

// exportHeader is the schema of an export's header line.
type exportHeader struct {
	Schema string `json:"schema"`
}

// checkExportSchema checks that the header line of export names schema, the export schema its pack's format_minor gives
// (the tracepack JSONL specification §5).
func checkExportSchema(export []byte, schema string) error {
	lines, err := exportLines(export)
	if err != nil {
		return err
	}
	var h exportHeader
	if err := json.Unmarshal(lines[0], &h); err != nil {
		return fmt.Errorf("export header line: %w", err)
	}
	if h.Schema != schema {
		return fmt.Errorf("the export names schema %q, the pack's format_minor gives %q", h.Schema, schema)
	}

	return nil
}

// exportRecords returns the seq and ts_utc_ns of each record line of export, in order.
func exportRecords(export []byte) ([]exportedRecord, error) {
	lines, err := exportLines(export)
	if err != nil {
		return nil, err
	}
	out := make([]exportedRecord, 0, len(lines)-1)
	for _, line := range lines[1:] {
		var rec exportedRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("export record line: %w", err)
		}
		out = append(out, rec)
	}

	return out, nil
}

// exportLines splits an export into its lines, each with its LF; the header line comes first.
func exportLines(export []byte) ([][]byte, error) {
	if len(export) == 0 || export[len(export)-1] != '\n' {
		return nil, errors.New("an export that does not end in LF")
	}

	lines := bytes.SplitAfter(export, []byte("\n"))

	// The final LF leaves an empty element after the last line.
	return lines[:len(lines)-1], nil
}

// checkClassify checks the classifier expectations c against the pack r reads:
// the pack's single max_frame_len, a frame per data or control record of the pack
// that stores the frame's decode_status and trailing_bytes,
// every record storing a byte-predicate status listed,
// and every data or control record either a frame or a format-only record, never both.
func checkClassify(ctx context.Context, r *tracepack.Reader, c *Classify) error {
	m := r.Header().Meta.MaxFrameLens
	if len(m) != 1 || m[0] != c.MaxFrameLen {
		return fmt.Errorf("the expected max_frame_len %d is not the pack's single max_frame_len %v", c.MaxFrameLen, m)
	}
	stored := make(map[uint64]ClassifyFrame)
	var classified []uint64
	_, err := r.Iterate(ctx, tracepack.Query{}, func(it *tracepack.Item) error {
		rec := &it.Record
		if rec.Kind == tracepack.KindData || rec.Kind == tracepack.KindControl {
			stored[rec.Seq] = ClassifyFrame{Seq: rec.Seq, DecodeStatus: rec.DecodeStatus, TrailingBytes: rec.TrailingBytes}
		}
		if slices.Contains(bytePredicateStatuses, rec.DecodeStatus) {
			classified = append(classified, rec.Seq)
		}

		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range c.Frames {
		s, ok := stored[f.Seq]
		switch {
		case !ok:
			return fmt.Errorf("the expectations list seq %d, which no data or control record of the pack has", f.Seq)
		case s.DecodeStatus != f.DecodeStatus || s.TrailingBytes != f.TrailingBytes:
			return fmt.Errorf("the expectations list seq %d as %s with %d trailing bytes, the record stores %s with %d",
				f.Seq, f.DecodeStatus, f.TrailingBytes, s.DecodeStatus, s.TrailingBytes)
		}
	}
	for _, seq := range classified {
		if !slices.ContainsFunc(c.Frames, func(f ClassifyFrame) bool { return f.Seq == seq }) {
			return fmt.Errorf("the record of seq %d stores a byte-predicate status, which the expectations do not list", seq)
		}
	}
	for seq := range stored {
		frame := slices.ContainsFunc(c.Frames, func(f ClassifyFrame) bool { return f.Seq == seq })
		if frame == slices.Contains(c.FormatOnly, seq) {
			return fmt.Errorf("the data or control record of seq %d is not exactly one of a frame and a format-only record", seq)
		}
	}
	for _, seq := range c.FormatOnly {
		if _, ok := stored[seq]; !ok {
			return fmt.Errorf("the expectations list seq %d as format-only, which no data or control record of the pack has", seq)
		}
	}

	return nil
}

// truncationTable returns the table of every cut of pack, whose export is export: each cut's rejection, or its export and verification.
// It checks the property of the tracepack corpus specification §5.7 on every cut that opens:
// the cut's export is the header line of export and the record lines of the blocks wholly before the cut.
func truncationTable(ctx context.Context, pack, export []byte) (*TruncationTable, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	base, err := exportLines(export)
	if err != nil {
		return nil, err
	}

	t := NewTruncationTable(uint64(len(pack)))
	for n := range len(pack) {
		cut := pack[:n]
		c, err := cutExpectation(ctx, cut, &l, base)
		if err != nil {
			return nil, fmt.Errorf("cut %d: %w", n, err)
		}
		if err := t.Add(c); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// cutExpectation returns the expectation of cut, a prefix of the pack of layout l whose export's lines are base.
func cutExpectation(ctx context.Context, cut []byte, l *Layout, base [][]byte) (Cut, error) {
	r, err := openBytes(ctx, cut)
	if err != nil {
		code, err := RejectionCode(err)
		if err != nil {
			return Cut{}, err
		}

		return RejectedCut(code), nil
	}

	export, _, err := exportPack(ctx, r)
	if err != nil {
		return Cut{}, err
	}
	whole := 0
	for _, s := range l.Blocks {
		if s.Offset+s.Len <= uint64(len(cut)) {
			whole += int(binary.LittleEndian.Uint32(cut[s.Offset+envelopeRecordCountOff:]))
		}
	}
	if whole+1 > len(base) || !bytes.Equal(export, bytes.Join(base[:whole+1], nil)) {
		return Cut{}, fmt.Errorf("the export is not the header and the %d record lines of the blocks before the cut", whole)
	}

	rep, err := tracepack.Verify(ctx, bytes.NewReader(cut), int64(len(cut)), tracepack.VerifyOptions{})
	if err != nil {
		return Cut{}, err
	}

	return ReadCut(&rep, uint64(whole))
}

// offset returns the file offset p names in the pack of layout l.
func (p Pos) offset(l *Layout) (uint64, error) {
	switch p.kind {
	case posBlock:
		s, err := l.block(p.block)
		if err != nil {
			return 0, err
		}

		return s.Offset + p.plus, nil
	case posEnd:
		return l.End + p.plus, nil
	case posOffset:
		return p.at + p.plus, nil
	case posNone:
		return 0, errors.New("no position")
	default:
		return 0, fmt.Errorf("position of kind %d", p.kind)
	}
}
