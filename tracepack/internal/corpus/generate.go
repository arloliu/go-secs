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
	repairVectors,
	validationVectors,
	verifyVectors,
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
	// EncoderMade reports a pack or patch that holds a block body or footer of codec zstd made by the running zstd encoder,
	// so that a regeneration under another encoder compares the vector by decoded equivalence
	// (the tracepack corpus specification §6.1).
	// The generator checks it: such a body decodes in full, and a hand-built damaged frame does not.
	EncoderMade bool
	// Build makes the vector's pack and the inputs of its optional files from the identity seed.
	Build func(seed string) (*Built, error)
	// Expect is what the vector must show, written by hand from the specification, never from the generator's output.
	// Every recipe has one; the generator refuses a recipe without it and fails, before writing anything,
	// when a read disagrees with it.
	Expect *Expectation
}

// Built is what a recipe's Build makes: the pack and the inputs of the vector's optional files.
type Built struct {
	// Pack is pack.tpk.
	Pack []byte
	// Queries are the query vectors of queries.json, without their results, which the read gives.
	Queries []QuerySpec
	// Fields adds fields.json.
	Fields bool
	// Footer adds footer.json.
	Footer bool
	// Classify is classify.json, written by hand; nil for none.
	Classify *Classify
	// Repair, for a repair vector or a truncation vector whose table holds each cut's repair,
	// holds what the repair options take from the vector; nil otherwise.
	Repair *RepairInput
}

// QuerySpec is a query vector of queries.json without its expect.
type QuerySpec struct {
	ID     string
	Cites  []string
	Filter Filter
}

// RepairInput is what a vector's repair options take from the vector;
// the writer, the clock and the patch's pack_id are the corpus's (see RepairOptionsFor).
type RepairInput struct {
	// PatchBase is the patch_base of a scope that has a generation; nil otherwise.
	PatchBase *tracepack.UUID
	// Intact is the pack before its damage, whose every record the patch must hold or cover by a coverage entry
	// (the tracepack format specification §16);
	// it is required of a repair vector whose pack lost records, and optional otherwise,
	// when the pack's own records serve.
	// The generator checks the patch against it and never writes it.
	Intact []byte
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
// The generator checks every field against its reads of the vector before it writes anything.
type Expectation struct {
	// Rejection is the bootstrap rejection code of a rejection vector.
	Rejection string
	// Outcome is the verification outcome of any other vector.
	Outcome tracepack.Outcome
	// Unfinalized reports a pack that is not finalized (I-5): verify.json's finalized is its negation.
	Unfinalized bool
	// FooterValid is verify.json's footer_valid.
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
	// TrailerTotalsDisagree is verify.json's trailer_totals_disagree.
	TrailerTotalsDisagree bool
	// WriterDefects are the writer defects, in verify.json's order.
	WriterDefects []DefectWant
	// Lost are verify.json's lost entries, in order; each also names the pack's capture_id, which the generator checks.
	Lost []LostWant
	// ExportHas are strings export.jsonl must contain, each a value the JSONL specification renders.
	ExportHas []string
	// Stats, when set, are the statistics Reader.Stats gives.
	Stats *StatsWant
	// Footer is footer.json of a vector whose Built asks for it, and must then be set:
	// its stored part, whether the footer is accepted, and its recomputed part.
	Footer *Footer
	// Queries are the results of each query of Built.Queries, by id.
	Queries []QueryWant
	// Repair is the expected repair of a repair vector.
	Repair *RepairWant
	// Cuts are the rows of truncation.json of a truncation vector, ascending, one per row:
	// each names where its row starts, and its row ends where the next starts, the last at the end of the pack.
	Cuts []CutWant
}

// CutWant is a row of truncation.json: the first cut length it covers and the expectation of its cuts,
// a rejection code or the outcome, the records exported and validated and where the validated prefix and the walk end,
// and in a table with repair options the cut's repair.
// Positions are those of the base pack, which every cut shares up to its length.
type CutWant struct {
	From      Pos
	Rejection string
	Outcome   tracepack.Outcome
	Validated int
	Records   uint64
	PrefixEnd Pos
	WalkStop  Pos
	// Repair is the repair of every cut of the row, in a table with repair options; nil otherwise.
	// A row states no records copied, so its Records is 0.
	Repair *RepairWant
}

// FailedWant is a failed block and its cause, ReasonCorruptBlock or ReasonUnknownCodec.
type FailedWant struct {
	Block int
	Cause tracepack.IncompleteReason
}

// DefectWant is a writer defect: its kind and its seq.
type DefectWant struct {
	Kind tracepack.WriterDefectKind
	Seq  uint64
}

// LostWant is a lost entry of verify.json: a closed range of seqs and the time interval of its block.
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

// QueryWant is the result of the query of id: the seqs it yields and its incomplete reasons, in queries.json's order.
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

// RepairWant is the expected repair: its result and, when patched, the blocks and records copied and the new coverage.
type RepairWant struct {
	Result   string
	Blocks   int
	Records  uint64
	Coverage []CoverageWant
}

// CoverageWant is a new coverage entry of a patch: its seq_first, its seq_last (nil when absent) and its time interval.
// The generator also checks that it names the pack's capture.
type CoverageWant struct {
	First              uint64
	Last               *uint64
	TimeStart, TimeEnd int64
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
	repair   *RepairOptions
	// intact is RepairInput.Intact; nil when the pack's own records are those the patch must hold or cover.
	intact []byte
}

// readOutputs is the reads of a vector: its files, in the order of the tracepack corpus specification §2,
// and what Expect is checked against.
type readOutputs struct {
	files      []file
	rejection  string
	verify     Verify
	export     []byte
	seqs       []uint64
	queries    []QueryVector
	stats      tracepack.PackStats
	statsOK    bool
	footer     *Footer
	repair     *Repair
	truncation *Truncation
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
	return IncompleteWant{Reason: tracepack.ReasonTruncated, Block: -1, At: at}
}

// BlockDefect is the reason of a query for block i, at its envelope.
func BlockDefect(reason tracepack.IncompleteReason, i int) IncompleteWant {
	return IncompleteWant{Reason: reason, Block: i, At: AtBlock(i)}
}

// CoverageHit is the coverage reason of a query.
func CoverageHit() IncompleteWant {
	return IncompleteWant{Reason: tracepack.ReasonCoverage, Block: -1}
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

// RepairOptionsFor returns the repair options of a vector of seed:
// the corpus's writer, the patch's pack_id from the seed, codec none, the corpus clock, and in's patch_base.
func RepairOptionsFor(seed string, in *RepairInput) RepairOptions {
	out := RepairOptions{
		Writer: corpusWriter, PackID: IDFor(roleRepairPackID, seed).String(), Codec: tracepack.CodecNone.String(),
		WriterStartUTCNs: I64(TimeBase),
	}
	if in.PatchBase != nil {
		out.PatchBase = new(in.PatchBase.String())
	}

	return out
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
	case e.Outcome == tracepack.OutcomeUnfinalized && !e.Unfinalized:
		return errors.New("an unfinalized outcome of a pack expected finalized")
	case (rec.Class == ClassTruncation) != (len(e.Cuts) > 0):
		return errors.New("a truncation vector, and only one, expects the rows of its table")
	case rec.Class == ClassRepair && e.Repair == nil:
		return errors.New("a repair vector expects its repair")
	}

	return nil
}

// generateVector builds the vector of rec and checks it against rec's Expect.
func generateVector(ctx context.Context, rec *Recipe) (Vector, *readOutputs, error) {
	if err := rec.check(); err != nil {
		return Vector{}, nil, err
	}
	seed := rec.seed()
	built, err := rec.Build(seed)
	if err != nil {
		return Vector{}, nil, err
	}

	in := &readInputs{
		class: rec.Class, queries: built.Queries, fields: built.Fields, footer: built.Footer, classify: built.Classify,
	}
	switch {
	case rec.Class == ClassRepair && built.Repair == nil:
		return Vector{}, nil, errors.New("a repair vector needs its repair input")
	case built.Repair != nil && rec.Class != ClassRepair && rec.Class != ClassTruncation:
		return Vector{}, nil, errors.New("only a repair or truncation vector is repaired")
	case built.Repair != nil:
		in.repair, in.intact = new(RepairOptionsFor(seed, built.Repair)), built.Repair.Intact
	default:
		// Neither repaired nor refused.
	}
	out, err := readVector(ctx, built.Pack, in)
	if err != nil {
		return Vector{}, nil, err
	}
	v := &out.verify
	if lost := len(v.FailedBlocks) > 0 || v.WalkStop != nil || !v.Finalized; rec.Class == ClassRepair && lost && in.intact == nil {
		return Vector{}, nil, errors.New("a repair vector whose pack lost records needs its intact pack")
	}
	if err := rec.Expect.check(out, built.Pack); err != nil {
		return Vector{}, nil, fmt.Errorf("disagrees with its Expect: %w", err)
	}
	if err := rec.checkCodec(out); err != nil {
		return Vector{}, nil, err
	}

	vec := Vector{
		ID: rec.ID, Title: rec.Title, Cites: rec.Cites, Class: rec.Class, Labels: nonNil(rec.Labels),
		Codec: cmp.Or(rec.Codec, CodecNone), Source: cmp.Or(rec.Source, SourceGenerated), Cases: rec.Cases,
	}
	for _, f := range out.files {
		vec.Files = append(vec.Files, f.name)
	}

	return vec, out, nil
}

// checkCodec checks rec's Codec and EncoderMade against the packs of out, when Locate reads them.
func (rec *Recipe) checkCodec(out *readOutputs) error {
	made := false
	for _, f := range out.files {
		if f.name != FilePack && f.name != FilePatch {
			continue
		}
		c, m, err := packCodecs(f.data)
		if err != nil {
			// A pack whose structures Locate refuses is declared by hand alone.
			continue
		}
		if f.name == FilePack && c != cmp.Or(rec.Codec, CodecNone) {
			return fmt.Errorf("its pack's codec is %s, the recipe says %s", c, cmp.Or(rec.Codec, CodecNone))
		}
		made = made || m
	}
	if made != rec.EncoderMade {
		return fmt.Errorf("its packs hold encoder-made zstd bytes: %v, the recipe says %v", made, rec.EncoderMade)
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
// else export.jsonl and verify.json, and queries.json, fields.json, classify.json, footer.json,
// repair.json with the patch's files, or truncation.json as in asks.
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
	out.files = append(out.files, file{FileExport, out.export})
	if out.verify, err = verifyBytes(ctx, pack); err != nil {
		return nil, err
	}
	if err := out.add(FileVerify, &out.verify); err != nil {
		return nil, err
	}
	out.stats, out.statsOK = r.Stats()

	if err := out.readOptional(ctx, r, pack, in); err != nil {
		return nil, err
	}

	switch in.class {
	case ClassRepair:
		if in.repair == nil {
			return nil, errors.New("a repair vector without its repair options")
		}
		if err := out.readRepair(ctx, pack, in.repair, in.intact); err != nil {
			return nil, err
		}
	case ClassTruncation:
		t, err := truncationTable(ctx, pack, out.export, in.repair)
		if err != nil {
			return nil, err
		}
		if err := out.add(FileTruncation, &t); err != nil {
			return nil, err
		}
		out.truncation = &t
	default:
		// A read vector has no other file.
	}

	slices.SortStableFunc(out.files, func(a, b file) int {
		return cmp.Compare(slices.Index(fileOrder, a.name), slices.Index(fileOrder, b.name))
	})

	return out, nil
}

// readOptional adds the optional files in asks for: queries.json, fields.json, classify.json and footer.json.
func (out *readOutputs) readOptional(ctx context.Context, r *tracepack.Reader, pack []byte, in *readInputs) error {
	if len(in.queries) > 0 {
		for _, q := range in.queries {
			e, err := RunQuery(ctx, r, &q.Filter)
			if err != nil {
				return fmt.Errorf("query %s: %w", q.ID, err)
			}
			out.queries = append(out.queries, QueryVector{ID: q.ID, Cites: q.Cites, Filter: q.Filter, Expect: e})
		}
		if err := out.add(FileQueries, Queries(out.queries)); err != nil {
			return err
		}
	}
	if in.fields {
		rows, err := ReadFields(ctx, r)
		if err != nil {
			return err
		}
		if err := out.add(FileFields, rows); err != nil {
			return err
		}
	}
	if in.classify != nil {
		if err := checkClassify(r, in.classify, out.seqs); err != nil {
			return err
		}
		if err := out.add(FileClassify, in.classify); err != nil {
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

// checkFooter checks footer.json f against the other reads of out (the tracepack corpus specification §5.8):
// accepted is verify.json's footer_valid, and the statistics of an accepted footer are its stored F-5,
// every epoch with its close_seq and every boundary.
func (out *readOutputs) checkFooter(f *Footer) error {
	if f.Accepted != out.verify.FooterValid {
		return fmt.Errorf("footer.json accepted is %v, verify.json footer_valid %v", f.Accepted, out.verify.FooterValid)
	}
	if !f.Accepted {
		return nil
	}
	if f.Stored == nil || !out.statsOK {
		return errors.New("an accepted footer without its stored part or its statistics")
	}

	stats := FooterF5{Epochs: []F5Epoch{}, Boundaries: []Boundary{}}
	for _, e := range out.stats.Epochs {
		fe := F5Epoch{Epoch: e.Epoch}
		if e.CloseSeq != nil {
			fe.CloseSeq = new(U64(*e.CloseSeq))
		}
		stats.Epochs = append(stats.Epochs, fe)
	}
	for _, b := range out.stats.Boundaries {
		stats.Boundaries = append(stats.Boundaries, Boundary{Seq: U64(b.Seq), Kind: b.Kind.String()})
	}
	if !reflect.DeepEqual(stats, f.Stored.F5) {
		return fmt.Errorf("the statistics %+v are not the stored F-5 %+v", stats, f.Stored.F5)
	}

	return nil
}

// readRepair repairs pack under opts and adds repair.json and, when patched,
// the patch, its export and its verification report, after checkPatch accepts the patch.
// The records the patch must hold or cover are those of intact, or of pack when intact is nil.
func (out *readOutputs) readRepair(ctx context.Context, pack []byte, opts *RepairOptions, intact []byte) error {
	rep, patch, err := RunRepair(ctx, pack, *opts)
	if err != nil {
		return err
	}
	out.repair = &rep
	if err := out.add(FileRepair, &rep); err != nil {
		return err
	}
	if patch == nil {
		return nil
	}

	source := out.export
	if intact != nil {
		r, err := openBytes(ctx, intact)
		if err != nil {
			return fmt.Errorf("intact pack: %w", err)
		}
		if source, _, err = exportPack(ctx, r); err != nil {
			return fmt.Errorf("intact pack: %w", err)
		}
	}
	records, err := exportRecords(source)
	if err != nil {
		return err
	}
	export, v, err := checkPatch(ctx, pack, patch, records)
	if err != nil {
		return err
	}
	out.files = append(out.files, file{FilePatch, patch}, file{FilePatchExport, export})

	return out.add(FilePatchVerify, &v)
}

// exportedRecord is the seq and ts_utc_ns of a record line of an export.
type exportedRecord struct {
	Seq U64 `json:"seq"`
	TS  I64 `json:"ts_utc_ns"`
}

// checkPatch checks patch, a repair of pack, against the tracepack format specification §13 and §16,
// and returns its export and its verify.json:
// the patch verifies finalized-consistent; each of its blocks is a block of pack, byte for byte;
// its seq_start is its first record's seq, or pack's seq_start when it holds no record;
// and every record of source it does not hold lies in one of its coverage entries, its own or those it inherits.
// An entry holds a record of the patch's capture whose seq and ts_utc_ns lie within its bounds,
// an absent bound being unbounded.
func checkPatch(ctx context.Context, pack, patch []byte, source []exportedRecord) ([]byte, Verify, error) {
	pr, err := openBytes(ctx, patch)
	if err != nil {
		return nil, Verify{}, fmt.Errorf("patch: %w", err)
	}
	export, seqs, err := exportPack(ctx, pr)
	if err != nil {
		return nil, Verify{}, fmt.Errorf("patch: %w", err)
	}
	v, err := verifyBytes(ctx, patch)
	if err != nil {
		return nil, Verify{}, fmt.Errorf("patch: %w", err)
	}
	if v.Outcome != tracepack.OutcomeFinalizedConsistent.String() {
		return nil, Verify{}, fmt.Errorf("the patch verifies %s", v.Outcome)
	}
	if err := patchBlocksCopied(pack, patch); err != nil {
		return nil, Verify{}, err
	}

	h := pr.Header()
	var want uint64
	if len(seqs) > 0 {
		want = seqs[0]
	} else {
		r, err := openBytes(ctx, pack)
		if err != nil {
			return nil, Verify{}, err
		}
		want = r.Header().Meta.SeqStart
	}
	if h.Meta.SeqStart != want {
		return nil, Verify{}, fmt.Errorf("the patch's seq_start is %d, not %d", h.Meta.SeqStart, want)
	}
	for _, rec := range source {
		if slices.Contains(seqs, uint64(rec.Seq)) {
			continue
		}
		if !slices.ContainsFunc(h.Meta.Coverage, func(c tracepack.Coverage) bool { return covers(&c, h.CaptureID, rec) }) {
			return nil, Verify{}, fmt.Errorf("record %d at %d is neither copied nor covered by the patch", rec.Seq, rec.TS)
		}
	}

	return export, v, nil
}

// covers reports whether the coverage entry c of a pack of capture holds rec:
// c's capture, and c's seq and time bounds around rec's.
func covers(c *tracepack.Coverage, capture tracepack.UUID, rec exportedRecord) bool {
	seq, ts := uint64(rec.Seq), int64(rec.TS)

	return (c.CaptureID == nil || *c.CaptureID == capture) &&
		(c.SeqFirst == nil || *c.SeqFirst <= seq) && (c.SeqLast == nil || seq <= *c.SeqLast) &&
		(c.TimeStart == nil || *c.TimeStart <= ts) && (c.TimeEnd == nil || ts <= *c.TimeEnd)
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

// verifyBytes verifies the in-memory pack and returns its verify.json.
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

// checkClassify checks that c is classify.json of the pack r reads:
// the pack's single max_frame_len, and a frame per record seqs holds.
func checkClassify(r *tracepack.Reader, c *Classify, seqs []uint64) error {
	m := r.Header().Meta.MaxFrameLens
	if len(m) != 1 || m[0] != uint64(c.MaxFrameLen) {
		return fmt.Errorf("classify.json's max_frame_len %d is not the pack's single max_frame_len %v", c.MaxFrameLen, m)
	}
	for _, f := range c.Frames {
		if !slices.Contains(seqs, uint64(f.Seq)) {
			return fmt.Errorf("classify.json lists seq %d, which no record of the pack has", f.Seq)
		}
	}

	return nil
}

// patchBlocksCopied checks that every block of patch is a block of pack, byte for byte.
func patchBlocksCopied(pack, patch []byte) error {
	pl, err := Locate(pack)
	if err != nil {
		return err
	}
	ql, err := Locate(patch)
	if err != nil {
		return err
	}
	for i, s := range ql.Blocks {
		blk := patch[s.Offset : s.Offset+s.Len]
		if !slices.ContainsFunc(pl.Blocks, func(d Span) bool { return bytes.Equal(pack[d.Offset:d.Offset+d.Len], blk) }) {
			return fmt.Errorf("block %d of the patch is not a block of the damaged pack", i)
		}
	}

	return nil
}

// truncationTable returns truncation.json of pack, whose export is export:
// each cut's rejection, or its outcome, export and verification and, with repair options, its repair.
// It checks the property of the tracepack corpus specification §5.7 on every cut that opens:
// the cut's export is the header line of export and the record lines of the blocks wholly before the cut;
// and it checks every patch of a cut with checkPatch, against the records of pack.
func truncationTable(ctx context.Context, pack, export []byte, repair *RepairOptions) (Truncation, error) {
	l, err := Locate(pack)
	if err != nil {
		return Truncation{}, err
	}
	base, err := exportLines(export)
	if err != nil {
		return Truncation{}, err
	}
	records, err := exportRecords(export)
	if err != nil {
		return Truncation{}, err
	}

	t := NewTruncationTable(uint64(len(pack)), repair)
	for n := range len(pack) {
		cut := pack[:n]
		c, err := cutExpectation(ctx, cut, &l, base, records, repair)
		if err != nil {
			return Truncation{}, fmt.Errorf("cut %d: %w", n, err)
		}
		if err := t.Add(c); err != nil {
			return Truncation{}, err
		}
	}

	return t.Table()
}

// cutExpectation returns the expectation of cut, a prefix of the pack of layout l whose export's lines are base
// and whose records are records.
func cutExpectation(ctx context.Context, cut []byte, l *Layout, base [][]byte, records []exportedRecord, repair *RepairOptions) (Cut, error) {
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
	c, err := ReadCut(&rep, uint64(whole))
	if err != nil || repair == nil {
		return c, err
	}
	rp, patch, err := RunRepair(ctx, cut, *repair)
	if err != nil {
		return Cut{}, err
	}
	if patch != nil {
		if _, _, err := checkPatch(ctx, cut, patch, records); err != nil {
			return Cut{}, err
		}
	}

	return c.WithRepair(&rp), nil
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
