package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/arloliu/go-secs/tracepack"
)

// repairOfPatchPath is the committed pack of repair-of-patch, from this package's directory, where Generate runs:
// the vector copies it and never regenerates it, since the operation that wrote it is no longer part of tracepack.
const repairOfPatchPath = "../../testdata/corpus/repair-of-patch/pack.tpk"

// repairOfPatchSHA256 is the SHA-256 of the pack tracepack v0.3.0 committed as repair-of-patch/pack.tpk.
// The vector reads its pack from the corpus it is compared with, so this digest, not the comparison, pins its bytes.
const repairOfPatchSHA256 = "807153febe2cdef65024da257eeacd0ae55ab65594574f4995791094aff58849"

// repairVectors returns the recipes of the repair group (the tracepack corpus specification §9.4):
// the vectors kept from the repair vectors of tracepack-corpus/2,
// each for a pack that serves a format clause no other vector serves, and each read as any pack.
func repairVectors() []Recipe {
	return []Recipe{
		{
			ID: "repair-inconsistent", Title: "a finalized-inconsistent archive of generation 1: its archive header exported",
			Cites: []string{"FMT §5", "JSONL §5", "STO §6", "FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(archiveMeta, func(pack []byte) ([]byte, error) {
				return orFooterQuality(pack, 0, uint64(tracepack.QualityOrderingUncertain))
			}),
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{0},
				ExportHas: []string{
					`"pack_role":"archive","compaction_level":1`,
					`"replacement_set_size":"1","replacement_set_index":"0","scope_generation":"1","publisher_epoch":"1"`,
				},
			}),
		},
		{
			ID: "repair-not-needed-commitments", Title: "a finalized-consistent segment holding a record of quality redacted",
			Cites: []string{"FMT §9", "JSONL §6", "FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				// Record 0 becomes redacted, which no stored pack holds, and the footer's quality unions say so.
				pack, err := PatchBody(pack, 0, func(b *Body) error {
					b.Rows[rowQualityOff] |= byte(tracepack.QualityRedacted)
					return nil
				})
				if err != nil {
					return nil, err
				}

				return orFooterQuality(pack, 0, uint64(tracepack.QualityRedacted))
			}),
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true,
				ExportHas: []string{
					`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0","epoch":1,"payload_len":14,"trailing_bytes":0,"quality":["redacted"]`,
				},
			}),
		},
		repairOfPatchVector(),
	}
}

// repairBasePack writes the pack the generated repair vectors start from, of meta, in three blocks of epoch 1:
// data of seq 0, data of seqs 2 and 3, data of seq 5; the pack's seq_start is 0.
func repairBasePack(seed string, meta *tracepack.PackMeta) ([]byte, error) {
	meta.SeqStart = 0

	return writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{
		{footerData(0, 1)},
		{footerData(2, 1), footerData(3, 1)},
		{footerData(5, 1)},
	}})
}

// repairBuild returns the Build of a generated repair vector: repairBasePack of meta, after edit.
func repairBuild(meta func(seed string) *tracepack.PackMeta, edit func(pack []byte) ([]byte, error)) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		pack, err := repairBasePack(seed, meta(seed))
		if err != nil {
			return nil, err
		}
		if pack, err = edit(pack); err != nil {
			return nil, err
		}

		return &Built{Pack: pack}, nil
	}
}

// repairBaseExpect completes e as the expectation of an edited repairBasePack whose blocks all validate:
// three blocks, the seqs 0, 2, 3 and 5, the prefix ending with the blocks.
func repairBaseExpect(e *Expectation) *Expectation {
	e.Blocks, e.PrefixEnd, e.Seqs = 3, AtEnd(), []uint64{0, 2, 3, 5}

	return e
}

// archiveMeta returns the pack metadata of an archive of generation 1 of seed, its period TimeBase's UTC hour.
func archiveMeta(seed string) *tracepack.PackMeta {
	meta := segmentMeta(seed)
	meta.PackRole, meta.PeriodEnd = tracepack.PackRoleArchive, TimeBase+hourNs
	meta.ScopeGeneration, meta.PublisherEpoch, meta.CompactionLevel = new(uint64(1)), new(uint64(1)), 1
	meta.ReplacementSetID = new(IDFor(roleReplacementSetID, seed))
	meta.ReplacementSetSize, meta.ReplacementSetIndex = new(uint64(1)), new(uint64(0))

	return meta
}

// repairOfPatchVector is repair-of-patch, the corpus's only pack of pack_role repair, read like any pack a reader is given
// (the tracepack storage specification §2).
// tracepack v0.3.0 wrote it as the patch of a pack of seqs 0, 2, 3 and 5 whose middle block failed:
// it holds the blocks of seqs 0 and 5 and a coverage entry of seqs 2 and 3,
// and its last block was damaged after writing.
func repairOfPatchVector() Recipe {
	return Recipe{
		ID: "repair-of-patch", Title: "a pack of role repair whose last block fails its body CRC, read as any pack",
		Cites: []string{"STO §2", "FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelDamaged}, Source: SourceFixed,
		Build: func(string) (*Built, error) {
			pack, err := readPinned(repairOfPatchPath, repairOfPatchSHA256)
			if err != nil {
				return nil, err
			}

			return &Built{Pack: pack, Queries: []QuerySpec{allQuery()}}, nil
		},
		// Its last block fails, and its valid footer gives the range.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 2, Seqs: []uint64{0}, PrefixEnd: AtBlock(1),
			Failed:    []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}},
			Lost:      []LostWant{{First: 5, Last: 5, TimeStart: msAt(5), TimeEnd: msAt(5)}},
			ExportHas: []string{`"pack_role":"repair"`},
			Queries: []QueryWant{{ID: "all", Seqs: []uint64{0}, Incomplete: []IncompleteWant{
				CoverageHit(), BlockDefect(tracepack.ReasonCorruptBlock, 1),
			}}},
		},
	}
}

// readPinned returns the file at path after checking that its SHA-256 is the hex digest want.
func readPinned(path, want string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("corpus: %s has SHA-256 %x, not the pinned %s", path, sum, want)
	}

	return b, nil
}
