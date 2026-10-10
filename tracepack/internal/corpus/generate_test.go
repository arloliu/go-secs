package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// digestEnv makes TestGenerateFreshProcesses, run as a subprocess, print the digest of Generate's output.
const digestEnv = "TRACEPACK_CORPUS_DIGEST"

// recipeByID returns a copy of the recipe of id.
func recipeByID(t *testing.T, id string) Recipe {
	t.Helper()

	recipes, err := Recipes()
	require.NoError(t, err)
	k := slices.IndexFunc(recipes, func(r Recipe) bool { return r.ID == id })
	require.GreaterOrEqual(t, k, 0, id)

	return recipes[k]
}

// corpusDigest returns a digest of files: each path and content, in path order.
func corpusDigest(files map[string][]byte) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%d\x00", k, len(files[k]))
		h.Write(files[k])
	}

	return hex.EncodeToString(h.Sum(nil))
}

func TestRecipesHaveExpectations(t *testing.T) {
	t.Parallel()

	recipes, err := Recipes()
	require.NoError(t, err)
	require.NotEmpty(t, recipes)
	for i := range recipes {
		assert.NoError(t, recipes[i].check(), recipes[i].ID)
	}

	for name, edit := range map[string]func(r *Recipe){
		"no expectation":               func(r *Recipe) { r.Expect = nil },
		"no build":                     func(r *Recipe) { r.Build = nil },
		"no outcome":                   func(r *Recipe) { r.Expect.Outcome = 0 },
		"no prefix end":                func(r *Recipe) { r.Expect.PrefixEnd = Pos{} },
		"a rejection code on a read":   func(r *Recipe) { r.Expect.Rejection = RejectBadMagic },
		"a rejection without its code": func(r *Recipe) { r.Class = ClassRejection },
	} {
		r := recipeByID(t, "basic-codec-none")
		r.Expect = new(*r.Expect)
		edit(&r)
		_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
		assert.Error(t, err, name)
	}
}

// TestGenerateFailsOnDisagreement checks that Generate fails when a read disagrees with any field of a recipe's expectation.
func TestGenerateFailsOnDisagreement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		name string
		edit func(e *Expectation)
	}{
		{"basic-codec-none", "outcome", func(e *Expectation) { e.Outcome = tracepack.OutcomeFinalizedInconsistent }},
		{"basic-codec-none", "footer valid", func(e *Expectation) { e.FooterValid = false }},
		{"basic-codec-none", "blocks", func(e *Expectation) { e.Blocks = 2 }},
		{"basic-codec-none", "a seq missing", func(e *Expectation) { e.Seqs = e.Seqs[1:] }},
		{"basic-codec-none", "seqs out of order", func(e *Expectation) { e.Seqs = slices.Concat(e.Seqs[1:2], e.Seqs[:1], e.Seqs[2:]) }},
		{"basic-codec-none", "prefix end", func(e *Expectation) { e.PrefixEnd = AtBlock(2) }},
		{"basic-codec-none", "a walk stop", func(e *Expectation) { e.WalkStop = AtEnd() }},
		{"basic-codec-none", "a failed block", func(e *Expectation) { e.Failed = []FailedWant{{1, tracepack.ReasonCorruptBlock}} }},
		{"basic-codec-none", "a disagreeing block", func(e *Expectation) { e.Disagreeing = []int{1} }},
		{"basic-codec-none", "trailer totals", func(e *Expectation) { e.TrailerTotalsDisagree = true }},
		{"basic-codec-none", "a writer defect", func(e *Expectation) {
			e.WriterDefects = []DefectWant{{tracepack.WriterDefectSeqStart, 0}}
		}},
		{"basic-codec-none", "a lost range", func(e *Expectation) { e.Lost = []LostWant{{4, 7, msAt(4), msAt(7)}} }},
		{"basic-codec-none", "unfinalized", func(e *Expectation) { e.Unfinalized = true }},
		{"basic-corrupt-middle", "lost time", func(e *Expectation) { e.Lost = []LostWant{{4, 7, msAt(4), msAt(8)}} }},
		{"basic-codec-none", "export content", func(e *Expectation) { e.ExportHas = []string{`"kind":"unknown(9)"`} }},
		{"basic-codec-none", "stats close_seq", func(e *Expectation) { e.Stats.Epochs[0].CloseSeq = new(uint64(9)) }},
		{"basic-codec-none", "stats unavailable", func(e *Expectation) { e.Stats.Available = false }},
		{"basic-codec-none", "stats boundary", func(e *Expectation) {
			e.Stats.Boundaries = []BoundaryWant{{8, tracepack.BoundaryKindStop}}
		}},
		{"basic-codec-none", "query seqs", func(e *Expectation) { e.Queries[1].Seqs = seqRange(3, 7) }},
		{"basic-codec-none", "a query missing", func(e *Expectation) { e.Queries = e.Queries[1:] }},
		{"basic-codec-none", "a query reason", func(e *Expectation) { e.Queries[0].Incomplete = []IncompleteWant{Truncated(Pos{})} }},
		{"basic-corrupt-middle", "failed cause", func(e *Expectation) { e.Failed[0].Cause = tracepack.ReasonUnknownCodec }},
		{"basic-corrupt-middle", "query block", func(e *Expectation) {
			e.Queries[0].Incomplete = []IncompleteWant{BlockDefect(tracepack.ReasonCorruptBlock, 2)}
		}},
		{"basic-corrupt-middle", "lost range", func(e *Expectation) { e.Lost = []LostWant{{4, 6, msAt(4), msAt(6)}} }},
		{"basic-bad-envelope-crc-walked", "walk stop position", func(e *Expectation) { e.WalkStop = AtBlock(2) }},
		{"basic-bad-envelope-crc-walked", "query offset", func(e *Expectation) {
			e.Queries[0].Incomplete = []IncompleteWant{Truncated(Pos{})}
		}},
		{"basic-empty-pack", "a repair", func(e *Expectation) { e.Repair = &RepairWant{Result: RepairNotNeeded} }},
		{"footer-close-twice-two-blocks", "footer.json missing", func(e *Expectation) { e.Footer = nil }},
		{"footer-close-twice-two-blocks", "a stored close_seq", func(e *Expectation) { e.Footer.Stored.Blocks[1].CloseSeqs[0].Seq = 2 }},
		{"footer-close-twice-two-blocks", "a recomputed boundary", func(e *Expectation) { e.Footer.Recomputed.F5.Boundaries = nil }},
		{"footer-f5-higher-close-seq", "footer accepted", func(e *Expectation) { e.Footer.Accepted = true }},
		{"footer-f5-higher-close-seq", "no stored part", func(e *Expectation) { e.Footer.Stored = nil }},
		{"footer-invalid-socket-close-body", "a recomputed close_seq", func(e *Expectation) {
			e.Footer.Recomputed.Blocks[0].CloseSeqs = []CloseSeq{{Epoch: 1, Seq: 2}}
		}},
		{"bootstrap-bad-magic", "rejection code", func(e *Expectation) { e.Rejection = RejectHeaderCRC }},
		{"verify-truncation-none", "a row's first cut", func(e *Expectation) { e.Cuts[3].From = AtBlock(0).Plus(2) }},
		{"verify-truncation-none", "a row's rejection", func(e *Expectation) { e.Cuts[1].Rejection = RejectShortObject }},
		{"verify-truncation-none", "a row's outcome", func(e *Expectation) { e.Cuts[2].Outcome = tracepack.OutcomeFinalizedTruncated }},
		{"verify-truncation-none", "a row's records", func(e *Expectation) { e.Cuts[4].Records = 1 }},
		{"verify-truncation-none", "a row's validated blocks", func(e *Expectation) { e.Cuts[6].Validated = 1 }},
		{"verify-truncation-none", "a row's prefix end", func(e *Expectation) { e.Cuts[5].PrefixEnd = AtBlock(0) }},
		{"verify-truncation-none", "a row's walk stop", func(e *Expectation) { e.Cuts[3].WalkStop = Pos{} }},
		{"verify-truncation-none", "a row missing", func(e *Expectation) { e.Cuts = e.Cuts[:len(e.Cuts)-1] }},
		{"verify-truncation-none", "a row's repair", func(e *Expectation) { e.Cuts[2].Repair = &RepairWant{Result: RepairNotNeeded} }},
		{"repair-middle-trusted-footer", "repair result", func(e *Expectation) { e.Repair = &RepairWant{Result: RepairNotRepairable} }},
		{"repair-middle-trusted-footer", "repair blocks", func(e *Expectation) { e.Repair.Blocks = 3 }},
		{"repair-middle-trusted-footer", "repair records", func(e *Expectation) { e.Repair.Records = 3 }},
		{"repair-middle-trusted-footer", "repair coverage seq_last", func(e *Expectation) { e.Repair.Coverage[0].Last = new(uint64(4)) }},
		{"repair-middle-trusted-footer", "repair coverage time", func(e *Expectation) { e.Repair.Coverage[0].TimeEnd = msAt(4) }},
		{"repair-middle-no-footer", "repair coverage seq_first", func(e *Expectation) { e.Repair.Coverage[0].First = 2 }},
		{"repair-middle-no-footer", "repair coverage missing", func(e *Expectation) { e.Repair.Coverage = nil }},
		{"repair-not-needed", "repair patched", func(e *Expectation) { e.Repair = &RepairWant{Result: RepairPatched, Blocks: 3, Records: 4} }},
		{"repair-truncation", "a row's repair result", func(e *Expectation) { e.Cuts[2].Repair = &RepairWant{Result: RepairNotRepairable} }},
		{"repair-truncation", "a row's repair blocks", func(e *Expectation) {
			e.Cuts[4].Repair = &RepairWant{Result: RepairPatched, Coverage: []CoverageWant{hourCoverage(2, nil)}}
		}},
		{"repair-truncation", "a row's repair coverage", func(e *Expectation) {
			e.Cuts[6].Repair = &RepairWant{Result: RepairPatched, Blocks: 2, Coverage: []CoverageWant{hourCoverage(4, new(uint64(9)))}}
		}},
		{"repair-truncation", "a row's repair records", func(e *Expectation) {
			e.Cuts[6].Repair = &RepairWant{Result: RepairPatched, Blocks: 2, Records: 4, Coverage: []CoverageWant{hourCoverage(4, nil)}}
		}},
		{"repair-truncation", "a row without its repair", func(e *Expectation) { e.Cuts[7].Repair = nil }},
		{"hsms-short-captures", "fields.json missing", func(e *Expectation) { e.Fields = nil }},
		{"basic-codec-none", "fields.json not read", func(e *Expectation) { e.Fields = []Fields{} }},
		{"hsms-short-captures", "a field available", func(e *Expectation) { e.Fields[0] = headerFields(0, shortFrameValues, fieldSessionID) }},
		{"hsms-short-captures", "a field unavailable", func(e *Expectation) {
			e.Fields[9] = headerFields(9, shortFrameValues, fieldSessionID, fieldStreamAndW, fieldFunction, fieldPType, fieldSType)
		}},
		{"hsms-control-frames", "a field value", func(e *Expectation) { e.Fields[2].Stream = new(uint8(0x81)) }},
		{"hsms-system-bytes-set-short", "no writer defect", func(e *Expectation) { e.WriterDefects = nil }},
		{"hsms-predicates", "an unavailable field not satisfying", func(e *Expectation) {
			e.Queries[3].Seqs = []uint64{0, 3, 5, 6, 7}
		}},
		{"sample-v010-rows", "the misread block agreeing", func(e *Expectation) { e.Disagreeing = nil }},
		{"sample-v010-rows", "the misread record", func(e *Expectation) { e.ExportHas = []string{`"kind":"data"`} }},
	}
	for _, tt := range tests {
		t.Run(tt.id+" "+tt.name, func(t *testing.T) {
			t.Parallel()

			r := recipeByID(t, tt.id)
			e := *r.Expect
			e.Seqs = slices.Clone(e.Seqs)
			e.Failed = slices.Clone(e.Failed)
			e.Queries = slices.Clone(e.Queries)
			e.Cuts = slices.Clone(e.Cuts)
			e.Fields = slices.Clone(e.Fields)
			if e.Repair != nil {
				e.Repair = new(*e.Repair)
				e.Repair.Coverage = slices.Clone(e.Repair.Coverage)
			}
			if e.Stats != nil {
				e.Stats = new(*e.Stats)
				e.Stats.Epochs = slices.Clone(e.Stats.Epochs)
			}
			tt.edit(&e)
			r.Expect = &e

			_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
			require.ErrorContains(t, err, "disagrees with its Expect")
		})
	}
}

// TestGenerateChecksRecipeDeclarations checks that Generate refuses a recipe whose codec or encoder dependence is wrong,
// a case naming a seq the pack does not export, and codec siblings that do not export identically.
func TestGenerateChecksRecipeDeclarations(t *testing.T) {
	t.Parallel()

	zstd := recipeByID(t, "basic-codec-zstd")
	zstd.EncoderMade = false
	_, err := generateRecipes(t.Context(), []Recipe{zstd}, "test")
	require.ErrorContains(t, err, "encoder-made")

	unknown := recipeByID(t, "basic-unknown-codec")
	unknown.Codec = CodecNone
	_, err = generateRecipes(t.Context(), []Recipe{unknown}, "test")
	require.ErrorContains(t, err, "codec")

	short := recipeByID(t, "hsms-short-captures")
	short.Cases = append(slices.Clone(short.Cases), Case{ID: "no-such-record", Seq: 10, Cites: []string{"FMT §7.2"}})
	_, err = generateRecipes(t.Context(), []Recipe{short}, "test")
	require.ErrorContains(t, err, "case no-such-record")

	none, mixed := recipeByID(t, "basic-codec-none"), recipeByID(t, "basic-codec-mixed")
	mixed.IdentitySeed = "another"
	none.IdentitySeed = "another"
	none.Build = recipeByID(t, "basic-uuid-byte-order").Build
	none.Expect = recipeByID(t, "basic-uuid-byte-order").Expect
	_, err = generateRecipes(t.Context(), []Recipe{none, mixed}, "test")
	require.ErrorContains(t, err, "share their identity")

	// A fixed pack's zstd bytes are no output of the running encoder; a generated one's would be.
	sample := recipeByID(t, "sample-v010-rows")
	sample.EncoderMade = true
	_, err = generateRecipes(t.Context(), []Recipe{sample}, "test")
	require.ErrorContains(t, err, "encoder-made")
	sample = recipeByID(t, "sample-v010-rows")
	sample.Source = ""
	_, err = generateRecipes(t.Context(), []Recipe{sample}, "test")
	require.ErrorContains(t, err, "encoder-made")
}

// TestGenerateChecksClassify checks that Generate refuses a classify.json that is not the pack's:
// a max_frame_len other than the pack's single one, a frame whose record stores another decode_status or trailing_bytes,
// a seq of no data or control record, and a record storing a byte-predicate status that it does not list.
func TestGenerateChecksClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, err string
		edit      func(c *Classify)
	}{
		{"max_frame_len", "max_frame_len", func(c *Classify) { c.MaxFrameLen++ }},
		{"decode_status", "the record stores", func(c *Classify) { c.Frames[0].DecodeStatus = tracepack.DecodeStatusOK.String() }},
		{"trailing_bytes", "the record stores", func(c *Classify) { c.Frames[7].TrailingBytes = 2 }},
		{"an annotation", "no data or control record", func(c *Classify) { c.Frames[0].Seq = 10 }},
		{"an unlisted frame", "does not list", func(c *Classify) { c.Frames = c.Frames[1:] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := recipeByID(t, "sem-decode-status-classified")
			build := r.Build
			r.Build = func(seed string) (*Built, error) {
				b, err := build(seed)
				if err != nil {
					return nil, err
				}
				b.Classify.Frames = slices.Clone(b.Classify.Frames)
				tt.edit(b.Classify)

				return b, nil
			}
			_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
			require.ErrorContains(t, err, tt.err)
		})
	}
}

// TestRecipesCheckTheirRows checks that a truncation vector, and only one, expects the rows of its table.
func TestRecipesCheckTheirRows(t *testing.T) {
	t.Parallel()

	trunc := recipeByID(t, "verify-truncation-none")
	trunc.Expect = new(*trunc.Expect)
	trunc.Expect.Cuts = nil
	require.Error(t, trunc.check(), "a truncation vector without rows")

	read := recipeByID(t, "basic-codec-none")
	read.Expect = new(*read.Expect)
	read.Expect.Cuts = []CutWant{{From: AtOffset(0), Rejection: RejectShortObject}}
	require.Error(t, read.check(), "rows on a read vector")

	repair := recipeByID(t, "repair-not-needed")
	repair.Expect = new(*repair.Expect)
	repair.Expect.Repair = nil
	require.Error(t, repair.check(), "a repair vector without its repair")
}

// TestGenerateNeedsIntactPack checks that Generate refuses a repair vector whose pack lost records without its intact pack,
// whose records the patch must hold or cover.
func TestGenerateNeedsIntactPack(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "repair-middle-trusted-footer")
	build := r.Build
	r.Build = func(seed string) (*Built, error) {
		b, err := build(seed)
		if err == nil {
			b.Repair.Intact = nil
		}

		return b, err
	}
	_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
	require.ErrorContains(t, err, "intact pack")
}

// coverageCaptureIDTag is the tag of a coverage entry's capture_id (the tracepack format specification §5).
const coverageCaptureIDTag uint16 = 0x0001

// TestGenerateUsesIntactPack checks that Generate checks a repair vector's patch against the records of its intact pack:
// a record the intact pack holds that the patch neither copies nor covers fails the vector.
func TestGenerateUsesIntactPack(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "repair-middle-trusted-footer")
	build := r.Build
	r.Build = func(seed string) (*Built, error) {
		b, err := build(seed)
		if err != nil {
			return nil, err
		}
		// Seq 4 lies outside the coverage entry of the failed block's F-3 range, seqs 2 to 3.
		b.Repair.Intact, err = writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
			{footerData(0, 1)},
			{footerData(2, 1), footerData(3, 1)},
			{footerData(4, 1), footerData(5, 1)},
		}})

		return b, err
	}
	_, err := generateRecipes(t.Context(), []Recipe{r}, "test")
	require.ErrorContains(t, err, "neither copied nor covered")
}

// TestCutExpectationChecksPatch checks that a truncation table's repair of a cut checks the cut's patch against the records it is given:
// a record outside the hour of the patch's tail entry fails the cut.
func TestCutExpectationChecksPatch(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "repair-truncation")
	b := must(r.Build(r.seed()))(t)
	export, _, err := exportPack(t.Context(), must(openBytes(t.Context(), b.Pack))(t))
	require.NoError(t, err)
	base := must(exportLines(export))(t)
	records := must(exportRecords(export))(t)
	l := must(Locate(b.Pack))(t)
	opts := RepairOptionsFor(r.seed(), b.Repair)
	const cut = 629

	c, err := cutExpectation(t.Context(), b.Pack[:cut], &l, base, records, &opts)
	require.NoError(t, err)
	require.NotNil(t, c.Repair)
	require.Equal(t, RepairPatched, c.Repair.Result)

	outside := append(slices.Clone(records), exportedRecord{Seq: 4, TS: I64(TimeBase + hourNs)})
	_, err = cutExpectation(t.Context(), b.Pack[:cut], &l, base, outside, &opts)
	require.ErrorContains(t, err, "neither copied nor covered")
}

// TestCheckPatch checks each property checkPatch requires of a patch (the tracepack format specification §16):
// finalized-consistent, its blocks those of the damaged pack, its seq_start, and every record of the intact pack held or covered.
func TestCheckPatch(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "repair-middle-trusted-footer")
	b := must(r.Build(r.seed()))(t)
	_, patch, err := RunRepair(t.Context(), b.Pack, RepairOptionsFor(r.seed(), b.Repair))
	require.NoError(t, err)
	require.NotNil(t, patch)
	intact := must(openBytes(t.Context(), b.Repair.Intact))(t)
	export, _, err := exportPack(t.Context(), intact)
	require.NoError(t, err)
	records := must(exportRecords(export))(t)
	require.Len(t, records, 4)

	_, _, err = checkPatch(t.Context(), b.Pack, patch, records)
	require.NoError(t, err, "the patch holds seqs 0 and 5 and covers seqs 2 and 3 at their times")

	tests := []struct {
		name        string
		pack, patch []byte
		records     []exportedRecord
		contains    string
	}{
		{"a seq neither copied nor covered", b.Pack, patch, append(slices.Clone(records), exportedRecord{Seq: 4, TS: I64(msAt(4))}), "record 4"},
		{"a covered seq at a time outside the entry", b.Pack, patch,
			[]exportedRecord{records[0], {Seq: 2, TS: I64(msAt(4))}, records[2], records[3]}, "record 2"},
		{"another seq_start", b.Pack, must(setSeqStart(patch, 0, 1))(t), records, "seq_start"},
		{"a patch that is not finalized-consistent", b.Pack, must(flipBody(patch, 1))(t), records, "verifies"},
		{"a block of another pack", must(setRowTime(b.Pack, 0, 0, msAt(1)))(t), patch, records, "not a block of the damaged pack"},
		{"a new entry of another capture", b.Pack, must(editMeta(patch, func(meta []byte) ([]byte, error) {
			at, err := NestedValueOffset(meta, Section{Len: len(meta)}, metaCoverageTag, 0, coverageCaptureIDTag)
			if err == nil {
				meta[at] ^= 0xFF
			}

			return meta, err
		}))(t), records, "neither copied nor covered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := checkPatch(t.Context(), tt.pack, tt.patch, tt.records)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.contains)
		})
	}
}

func TestExportLines(t *testing.T) {
	t.Parallel()

	lines, err := exportLines([]byte("{\"h\":1}\n{\"seq\":\"0\"}\n"))
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("{\"h\":1}\n"), []byte("{\"seq\":\"0\"}\n")}, lines)

	_, err = exportLines([]byte("{}"))
	require.Error(t, err)
}

// TestTruncationTableChecksExportContent checks that truncationTable refuses a base export whose record lines
// are not those of the pack, in their order, byte for byte: the record lines of each cut's export are compared, not counted.
func TestTruncationTableChecksExportContent(t *testing.T) {
	t.Parallel()

	r := recipeByID(t, "verify-truncation-none")
	pack := must(r.Build(r.seed()))(t).Pack
	export, _, err := exportPack(t.Context(), must(openBytes(t.Context(), pack))(t))
	require.NoError(t, err)
	_, err = truncationTable(t.Context(), pack, export, nil)
	require.NoError(t, err)

	lines := must(exportLines(export))(t)
	swapped := slices.Clone(lines)
	swapped[1], swapped[2] = swapped[2], swapped[1]
	changed := slices.Clone(lines)
	changed[2] = bytes.Replace(changed[2], []byte(`"mono_ns":"0"`), []byte(`"mono_ns":"1"`), 1)
	require.NotEqual(t, lines[2], changed[2])

	for name, altered := range map[string][][]byte{"record lines 1 and 2 swapped": swapped, "a byte of record line 2 changed": changed} {
		_, err := truncationTable(t.Context(), pack, bytes.Join(altered, nil), nil)
		require.ErrorContains(t, err, "is not the header and the", name)
	}
}

func TestGenerateDeterministic(t *testing.T) {
	t.Parallel()

	a, err := Generate()
	require.NoError(t, err)
	b, err := Generate()
	require.NoError(t, err)
	require.Equal(t, a, b)
}

// TestGenerateFreshProcesses checks that Generate's output is the same in two fresh processes as in this one:
// it runs this test binary twice, each run printing the digest of its output.
func TestGenerateFreshProcesses(t *testing.T) {
	if os.Getenv(digestEnv) != "" {
		files, err := Generate()
		require.NoError(t, err)
		fmt.Printf("digest=%s\n", corpusDigest(files))

		return
	}
	t.Parallel()

	files, err := Generate()
	require.NoError(t, err)
	want := corpusDigest(files)
	for range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGenerateFreshProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), digestEnv+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		require.Contains(t, string(out), "digest="+want)
	}
}

func TestZstdEncoder(t *testing.T) {
	t.Parallel()

	for listed, want := range map[string]string{
		"github.com/klauspost/compress v1.20.1\n":                                        "github.com/klauspost/compress v1.20.1",
		"github.com/klauspost/compress v1.20.1 => github.com/klauspost/compress v1.21.0": "github.com/klauspost/compress v1.21.0",
		"github.com/klauspost/compress v1.20.1 => example.com/fork v0.1.0\n":             "example.com/fork v0.1.0",
		"github.com/klauspost/compress v1.20.1 => ../compress \n":                        "../compress",
	} {
		got, err := ZstdEncoder(listed)
		require.NoError(t, err, listed)
		assert.Equal(t, want, got, listed)
	}

	for _, listed := range []string{"", "\n", "golang.org/x/other v1.0.0", "github.com/klauspost/compress", "github.com/klauspost/compress v1 v2",
		"github.com/klauspost/compress v1.20.1 => ", "github.com/klauspost/compress v1.20.1 => a b c"} {
		_, err := ZstdEncoder(listed)
		assert.ErrorIs(t, err, ErrNoEncoder, "%q", listed)
	}
}

func TestGoListEncoder(t *testing.T) {
	t.Parallel()

	listed, err := GoListEncoder()
	require.NoError(t, err)
	got, err := LinkedZstdEncoder(GoListEncoder)
	require.NoError(t, err, "the go command names the module the test links")

	// A build that replaces the module records the replacement, so only an unreplaced one must name the module itself.
	if !strings.Contains(listed, "=>") {
		assert.True(t, strings.HasPrefix(got, compressModule+" v"), "an unreplaced module records itself: %s", got)
	}
}

// TestEncoderProvenance checks which encoder identity a regeneration compares with the manifest's:
// the one the go command lists for the build, a replacement's when replaced, never the version go.mod declares,
// and none when the go command is missing or fails.
func TestEncoderProvenance(t *testing.T) {
	t.Parallel()

	const declared, listed = "v1.20.1", "v1.21.0"
	lister := func(line string, err error) func() (string, error) {
		return func() (string, error) { return line, err }
	}
	// go.mod declares A, a replacement or a workspace makes the build link B.
	matches := encoderMatches(LinkedZstdEncoder(lister(compressModule+" "+declared+" => "+compressModule+" "+listed+"\n", nil)))
	assert.False(t, matches(compressModule+" "+declared), "listed B, declared A, recorded A")
	assert.True(t, matches(compressModule+" "+listed), "listed B, recorded B")
	assert.True(t, encoderMatches(LinkedZstdEncoder(lister(compressModule+" "+listed+"\n", nil)))(compressModule+" "+listed), "listed B, recorded B, not replaced")

	unavailable := encoderMatches(LinkedZstdEncoder(lister("", ErrNoEncoder)))
	assert.False(t, unavailable(compressModule+" "+declared), "the go command missing or failing")
	assert.False(t, unavailable(UnknownEncoder), "an unknown encoder matches no record")
	assert.False(t, encoderMatches(LinkedZstdEncoder(lister("golang.org/x/other v1.0.0\n", nil)))(""), "another module listed")
}

// TestCompareCorpus checks the comparison of a regenerated corpus with the committed one:
// strictly, byte for byte; on the mismatch path, the manifest but its zstd_encoder,
// encoder-made packs by decoded equivalence and their offset-bearing files not at all, every other file byte for byte.
func TestCompareCorpus(t *testing.T) {
	t.Parallel()

	gen, err := generateRecipes(t.Context(), mustRecipes(t), "github.com/klauspost/compress v1.20.1")
	require.NoError(t, err)
	dependent, err := encoderMadeIDs()
	require.NoError(t, err)
	edited := func(edit func(c map[string][]byte)) map[string][]byte {
		c := make(map[string][]byte, len(gen))
		for k, v := range gen {
			c[k] = bytes.Clone(v)
		}
		edit(c)

		return c
	}
	manifest := func(edit func(m *Manifest)) func(c map[string][]byte) {
		return func(c map[string][]byte) {
			var m Manifest
			require.NoError(t, Unmarshal(c[FileManifest], &m))
			edit(&m)
			b, err := m.Marshal()
			require.NoError(t, err)
			c[FileManifest] = b
		}
	}
	zstdPack := "basic-codec-zstd/" + FilePack

	tests := []struct {
		name             string
		edit             func(c map[string][]byte)
		strict, mismatch bool // whether a difference is reported
	}{
		{"identical", func(map[string][]byte) {}, false, false},
		{"another recorded encoder", manifest(func(m *Manifest) { m.ZstdEncoder = "github.com/klauspost/compress v1.0.0" }), true, false},
		{"another manifest value", manifest(func(m *Manifest) { m.Vectors[0].Title = "changed" }), true, true},
		{"another spec version", manifest(func(m *Manifest) { m.SpecVersion = "2.26" }), true, true},
		{"an encoder-made pack, its footer re-encoded", func(c map[string][]byte) {
			c[zstdPack] = must(PatchFooter(c[zstdPack], func(d []byte) ([]byte, error) { return append(d[:len(d):len(d)], 0), nil }))(t)
		}, true, false},
		{"an encoder-made pack, a decoded byte changed", func(c map[string][]byte) {
			c[zstdPack] = must(PatchBody(c[zstdPack], 1, func(b *Body) error { b.Payloads[0] ^= 1; return nil }))(t)
		}, true, true},
		{"an encoder-made vector's codec none block", func(c map[string][]byte) {
			mixed := "basic-codec-mixed/" + FilePack
			l := must(Locate(c[mixed]))(t)
			c[mixed] = must(FlipByte(c[mixed], int(l.Blocks[0].Offset)+testEnvelopeLen+3))(t)
		}, true, true},
		{"an encoder-made pack's file header", func(c map[string][]byte) {
			c[zstdPack] = must(PatchHeader(c[zstdPack], func(h []byte) { h[64] = 1 }))(t)
		}, true, true},
		{"an encoder-made vector's verify.json", func(c map[string][]byte) { c["basic-codec-zstd/"+FileVerify][0] = ' ' }, true, false},
		{"an encoder-made vector's export", func(c map[string][]byte) { c["basic-codec-zstd/"+FileExport][0] = ' ' }, true, true},
		{"another vector's verify.json", func(c map[string][]byte) { c["basic-codec-none/"+FileVerify][0] = ' ' }, true, true},
		{"another vector's pack", func(c map[string][]byte) {
			c["basic-unknown-codec/"+FilePack] = must(FlipByte(c["basic-unknown-codec/"+FilePack], 100))(t)
		}, true, true},
		{"a missing file", func(c map[string][]byte) { delete(c, "basic-codec-zstd/"+FileVerify) }, true, true},
		{"an orphan", func(c map[string][]byte) { c["basic-old/"+FilePack] = []byte{1} }, true, true},
		{"a kept root file", func(c map[string][]byte) { c["README.md"] = []byte("# corpus\n") }, false, false},
		{"the fixed sample's pack", func(c map[string][]byte) {
			c["sample-v010-rows/"+FilePack] = must(FlipByte(c["sample-v010-rows/"+FilePack], 100))(t)
		}, true, true},
		{"the fixed sample's verify.json", func(c map[string][]byte) { c["sample-v010-rows/"+FileVerify][0] = ' ' }, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			committed := edited(tt.edit)
			assert.Equal(t, tt.strict, len(compareCorpus(gen, committed, true, dependent)) > 0, "strict")
			assert.Equal(t, tt.mismatch, len(compareCorpus(gen, committed, false, dependent)) > 0, "mismatch")
		})
	}
}

// TestCorpusLayoutAndReadPath writes a generated corpus to a directory, reads it back and checks it
// as TestCorpus checks the committed corpus, then checks that an edit of a committed golden fails the read path.
func TestCorpusLayoutAndReadPath(t *testing.T) {
	t.Parallel()

	gen, err := generateRecipes(t.Context(), mustRecipes(t), UnknownEncoder)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/README.md", []byte("kept\n"), 0o644))
	require.NoError(t, os.MkdirAll(dir+"/basic-old", 0o755))
	require.NoError(t, os.WriteFile(dir+"/basic-old/pack.tpk", []byte{1}, 0o644))
	require.Error(t, writeCorpus(dir, gen), "a corpus without a known encoder is not written")

	gen = edits(gen, manifestEncoder(t, gen, "github.com/klauspost/compress v1.20.1"))
	require.NoError(t, writeCorpus(dir, gen))
	got, err := readTree(dir)
	require.NoError(t, err)
	assert.Equal(t, []byte("kept\n"), got["README.md"])
	delete(got, "README.md")
	require.Equal(t, gen, got, "the orphan is removed")
	_, err = os.Stat(dir + "/basic-old")
	require.ErrorIs(t, err, os.ErrNotExist)

	var m Manifest
	require.NoError(t, Unmarshal(got[FileManifest], &m))
	assert.Empty(t, checkLayout(got, &m))
	for i := range m.Vectors {
		require.NoError(t, readCommitted(t.Context(), &m.Vectors[i], got), m.Vectors[i].ID)
	}

	// Each committed golden binds the read: a changed one fails it.
	for k, change := range map[string][2]string{
		"basic-codec-none/" + FileExport:  {`"seq":"3"`, `"seq":"4"`},
		"basic-codec-none/" + FileVerify:  {`"records": "12"`, `"records": "11"`},
		"basic-codec-none/" + FileQueries: {`"3"`, `"4"`},
	} {
		bad := edits(got, func(c map[string][]byte) { c[k] = bytes.Replace(c[k], []byte(change[0]), []byte(change[1]), 1) })
		require.NotEqual(t, got[k], bad[k])
		require.Error(t, readCommitted(t.Context(), &m.Vectors[slices.IndexFunc(m.Vectors, func(v Vector) bool { return v.ID == "basic-codec-none" })], bad), k)
	}
	assert.NotEmpty(t, checkLayout(edits(got, func(c map[string][]byte) { c["basic-codec-none/fields.json"] = []byte("[]\n") }), &m), "an orphan file")
	assert.NotEmpty(t, checkLayout(edits(got, func(c map[string][]byte) { delete(c, "basic-codec-none/"+FileVerify) }), &m), "a missing file")
	assert.NotEmpty(t, checkLayout(edits(got, func(c map[string][]byte) { c["notes.txt"] = nil }), &m), "an unknown root file")
}

func TestCheckSize(t *testing.T) {
	t.Parallel()

	require.NoError(t, checkSize(maxCorpusBytes))
	require.Error(t, checkSize(maxCorpusBytes+1))
}

// mustRecipes returns every recipe.
func mustRecipes(t *testing.T) []Recipe {
	t.Helper()

	recipes, err := Recipes()
	require.NoError(t, err)

	return recipes
}

// edits returns a copy of files after edit.
func edits(files map[string][]byte, edit func(c map[string][]byte)) map[string][]byte {
	c := make(map[string][]byte, len(files))
	for k, v := range files {
		c[k] = bytes.Clone(v)
	}
	edit(c)

	return c
}

// manifestEncoder returns an edit setting the zstd_encoder of the manifest of files to encoder.
func manifestEncoder(t *testing.T, files map[string][]byte, encoder string) func(c map[string][]byte) {
	t.Helper()

	var m Manifest
	require.NoError(t, Unmarshal(files[FileManifest], &m))
	m.ZstdEncoder = encoder
	b, err := m.Marshal()
	require.NoError(t, err)

	return func(c map[string][]byte) { c[FileManifest] = b }
}

// specFile is the corpus specification, from this package's directory; absent from a module zip.
const specFile = "../../../docs/specs/tracepack/tracepack-corpus.md"

// generatedGroups are the id prefixes of the groups the generator builds so far.
var generatedGroups = []string{
	"basic-", "bootstrap-", "footer-", "framing-", "hsms-", "multi-", "repair-", "sample-", "sem-", "validation-", "verify-",
}

// partialGroups are the generated groups whose catalogued vectors do not all have a recipe yet.
// A group leaves the list with its last vector.
var partialGroups = []string{}

// TestRecipeIDsAreCatalogued checks that every vector of a generated group that the catalogue of the tracepack corpus specification §9.4 lists has a recipe,
// a group of partialGroups excepted, and that no recipe is missing from it.
// It reads the catalogue from the specification and is skipped where the specification is absent.
func TestRecipeIDsAreCatalogued(t *testing.T) {
	t.Parallel()

	spec, err := os.ReadFile(specFile)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("the corpus specification is not in this tree")
	}
	require.NoError(t, err)
	catalogue := catalogueIDs(t, string(spec))
	require.NotEmpty(t, catalogue)

	var want []string
	recipes := mustRecipes(t)
	ids := make([]string, 0, len(recipes))
	inGroup := func(id string) bool {
		return slices.ContainsFunc(generatedGroups, func(p string) bool { return strings.HasPrefix(id, p) })
	}
	partial := func(id string) bool {
		return slices.ContainsFunc(partialGroups, func(p string) bool { return strings.HasPrefix(id, p) })
	}
	for _, r := range recipes {
		ids = append(ids, r.ID)
		assert.True(t, inGroup(r.ID), "recipe %s is in no generated group", r.ID)
	}
	for _, id := range catalogue {
		if inGroup(id) && (!partial(id) || slices.Contains(ids, id)) {
			want = append(want, id)
		}
	}
	assert.Equal(t, want, ids)
	for _, p := range partialGroups {
		assert.True(t, slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, p) }), "partial group %s has no recipe", p)
		assert.True(t, slices.ContainsFunc(catalogue, func(id string) bool { return strings.HasPrefix(id, p) && !slices.Contains(ids, id) }),
			"group %s has every catalogued vector, so it is no longer partial", p)
	}
}

// catalogueIDs returns the vector ids of the table of §9.4 of the corpus specification spec, ascending:
// the backticked ids of each row's first column.
func catalogueIDs(t *testing.T, spec string) []string {
	t.Helper()

	_, section, ok := strings.Cut(spec, "\n### 9.4 Vectors\n")
	require.True(t, ok, "no §9.4 in the specification")
	section, _, _ = strings.Cut(section, "\n## ")

	var ids []string
	for line := range strings.Lines(section) {
		cells := strings.Split(line, "|")
		if !strings.HasPrefix(line, "| `") || len(cells) < 3 {
			continue
		}
		for f := range strings.SplitSeq(cells[1], ",") {
			ids = append(ids, strings.Trim(strings.TrimSpace(f), "`"))
		}
	}
	slices.Sort(ids)

	return ids
}
