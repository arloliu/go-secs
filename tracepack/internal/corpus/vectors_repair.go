package corpus

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// hourNs is the length of a UTC hour in nanoseconds.
const hourNs = int64(time.Hour)

// metaPeriodEndTag is the pack metadata tag of period_end (the tracepack format specification §5).
const metaPeriodEndTag uint16 = 0x0011

// roleSourcePackID is the role of the pack_id of a pack a repair vector repairs into its own pack.tpk.
const roleSourcePackID = "source_pack_id"

// repairVectors returns the recipes of the repair group (the tracepack corpus specification §9.4):
// the repairs of the tracepack format specification §13 and each of its refusals.
// Most vectors repair repairBasePack after an edit; the scope hour of each is TimeBase's.
func repairVectors() []Recipe {
	cites := []string{"FMT §13", "FMT §16", "CORPUS §5.6"}
	with := func(more ...string) []string { return append(append([]string{}, cites...), more...) }

	return []Recipe{
		{
			ID: "repair-middle-trusted-footer", Title: "a middle block failing its body CRC, the footer valid: coverage from the footer's ranges",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelDamaged},
			Build:  repairBuild(segmentMeta, func(pack []byte) ([]byte, error) { return flipBody(pack, 1) }),
			Expect: middleFailedRepairExpect(false, false, footerCoverage(2, 3)),
		},
		{
			ID: "repair-middle-disagreeing-footer", Title: "a middle block failing its body CRC, a validated block disagreeing with the valid footer",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelDamaged, LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				// ordering-uncertain in block 0's F-3 quality_union and F-5's, which no record carries.
				pack, err := orFooterQuality(pack, 0, uint64(tracepack.QualityOrderingUncertain))
				if err != nil {
					return nil, err
				}

				return flipBody(pack, 1)
			}),
			// The footer is not trusted, so the new entry spans the seqs between the validated neighbours, 0 and 5,
			// and the scope's hour.
			Expect: middleFailedRepairExpect(true, false, hourCoverage(1, new(uint64(4)))),
		},
		{
			ID: "repair-middle-no-footer", Title: "a middle block failing its body CRC, the footer rejected: coverage from the neighbours' seqs",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelDamaged, LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				pack, err := rejectFooter(pack)
				if err != nil {
					return nil, err
				}

				return flipBody(pack, 1)
			}),
			// Without a valid footer the walk goes on past the failed block, whose envelope accounts for it, to footer_offset:
			// one lost run, no tail.
			Expect: middleFailedRepairExpect(false, true, hourCoverage(1, new(uint64(4)))),
		},
		repairTruncationVector(),
		{
			ID: "repair-all-failed", Title: "every block failing its body CRC, the footer valid: a patch holding only coverage",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelDamaged},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				var err error
				for i := range 3 {
					if pack, err = flipBody(pack, i); err != nil {
						return nil, err
					}
				}

				return pack, nil
			}),
			// No validated block follows a failed one, so the pack is finalized-truncated;
			// the footer gives every block's range, and the patch, without records, keeps the pack's seq_start.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 3, PrefixEnd: AtBlock(0),
				Failed: []FailedWant{
					{Block: 0, Cause: tracepack.ReasonCorruptBlock}, {Block: 1, Cause: tracepack.ReasonCorruptBlock},
					{Block: 2, Cause: tracepack.ReasonCorruptBlock},
				},
				Lost: []LostWant{
					{First: 0, Last: 0, TimeStart: msAt(0), TimeEnd: msAt(0)}, {First: 2, Last: 3, TimeStart: msAt(2), TimeEnd: msAt(3)},
					{First: 5, Last: 5, TimeStart: msAt(5), TimeEnd: msAt(5)},
				},
				Repair: &RepairWant{
					Result:   RepairPatched,
					Coverage: []CoverageWant{footerCoverage(0, 0), footerCoverage(2, 3), footerCoverage(5, 5)},
				},
			},
		},
		{
			ID: "repair-inconsistent", Title: "a finalized-inconsistent archive of generation 1: every block copied, no new coverage",
			Cites: with("STO §6"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				b, err := repairBuild(archiveMeta, func(pack []byte) ([]byte, error) {
					return orFooterQuality(pack, 0, uint64(tracepack.QualityOrderingUncertain))
				})(seed)
				if err == nil {
					// The scope's current generation is the archive's.
					b.Repair.PatchBase = new(IDFor(roleReplacementSetID, seed))
				}

				return b, err
			},
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{0},
				Repair: &RepairWant{Result: RepairPatched, Blocks: 3, Records: 4},
			}),
		},
		repairOfPatchVector(),
		{
			ID: "repair-seq-start-above", Title: "a finalized-consistent pack whose first record's seq is above seq_start",
			Cites: with("FMT §5"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuildFrom(1, segmentMeta, func(pack []byte) ([]byte, error) { return setSeqStart(pack, 1, 0) }),
			// The patch states its first record's seq as seq_start, which the generator checks.
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Seqs: []uint64{1, 3, 4, 6},
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqStart, Seq: 1}},
				Repair:        &RepairWant{Result: RepairPatched, Blocks: 3, Records: 4},
			}),
		},
		{
			ID: "repair-seq-start-below", Title: "a finalized-consistent pack whose first record's seq is below seq_start",
			Cites: with("FMT §5"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) { return setSeqStart(pack, 0, 1) }),
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true,
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqStart, Seq: 0}},
				Repair:        &RepairWant{Result: RepairPatched, Blocks: 3, Records: 4},
			}),
		},
		{
			ID: "repair-not-needed", Title: "a finalized-consistent segment: nothing to repair",
			Cites: cites, Class: ClassRepair,
			Build:  repairBuild(segmentMeta, nil),
			Expect: notNeededExpect(nil),
		},
		{
			ID: "repair-not-needed-extract", Title: "a finalized-consistent extract: nothing to repair, before its role is considered",
			Cites: with("STO §2"), Class: ClassRepair,
			Build:  repairBuild(extractMeta, nil),
			Expect: notNeededExpect([]string{`"pack_role":"extract"`}),
		},
		{
			ID: "repair-not-needed-period", Title: "a finalized-consistent segment whose period spans two UTC hours: nothing to repair",
			Cites: with("STO §2"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				// The Writer refuses such a segment, so period_end moves after writing, from a minute to two hours after TimeBase.
				return editMeta(pack, func(meta []byte) ([]byte, error) {
					at, err := EntryOffset(meta, Section{Len: len(meta)}, metaPeriodEndTag, 0)
					if err != nil {
						return nil, err
					}

					return meta, setLE64(meta, at+tlv.HeaderLen, uint64(TimeBase+segmentPeriod), uint64(TimeBase+2*hourNs))
				})
			}),
			Expect: notNeededExpect([]string{`"period_end":"1791007200000000000"`}),
		},
		{
			ID: "repair-not-needed-commitments", Title: "a finalized-consistent segment holding a redacted record: nothing to repair",
			Cites: with("FMT §9"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
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
			Expect: notNeededExpect([]string{`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0","epoch":1,"payload_len":14,"trailing_bytes":0,"quality":["redacted"]`}),
		},
		{
			ID: "repair-refused-extract", Title: "a finalized-inconsistent extract: not repairable",
			Cites: with("STO §2"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(extractMeta, func(pack []byte) ([]byte, error) {
				return orFooterQuality(pack, 0, uint64(tracepack.QualityOrderingUncertain))
			}),
			// The role is the only refusal condition the pack meets.
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{0},
				Repair: &RepairWant{Result: RepairNotRepairable},
			}),
		},
		{
			ID: "repair-refused-seq-order", Title: "a block whose first seq is below the last seq of the block before it: not repairable",
			Cites: with("FMT I-12"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, seqOrderEdit),
			// The footer states the defect, so its validation rejects it; the walk validates every block,
			// and the defect is the only refusal condition the pack meets.
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, Seqs: []uint64{0, 2, 3, 1},
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqOrder, Seq: 1}},
				Repair:        &RepairWant{Result: RepairNotRepairable},
			}),
		},
		{
			ID: "repair-refused-hour-span", Title: "a block whose records lie in two UTC hours, so also outside the scope's hour: not repairable",
			Cites: with("FMT I-13"), Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				// Block 1's last record, seq 3, moves to the next UTC hour; the F-2 entry keeps its time range.
				return setRowTime(pack, 1, 1, TimeBase+hourNs+3*int64(time.Millisecond))
			}),
			// The defect, I-13, and a validated block outside the scope's hour are two refusal conditions, which no pack separates.
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{1},
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectHourSpan, Seq: 2}},
				ExportHas:     []string{`"seq":"3","ts_utc_ns":"1791003600003000000"`},
				Repair:        &RepairWant{Result: RepairNotRepairable},
			}),
		},
		{
			ID: "repair-refused-lost-run", Title: "a failed block without a seq between its validated neighbours: not repairable",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelDamaged, LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				pack, err := seqOrderEdit(pack)
				if err != nil {
					return nil, err
				}

				return flipBody(pack, 1)
			}),
			// The footer states a seq-order defect, so it is rejected and the lost run is bounded by its neighbours, seqs 0 and 1:
			// no seq lies between them, the only refusal condition the pack meets.
			// Block 2 follows the validated block 0, so it carries no seq-order defect.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeCorruptMiddle, Blocks: 3, Seqs: []uint64{0, 1}, PrefixEnd: AtBlock(1),
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}},
				Repair: &RepairWant{Result: RepairNotRepairable},
			},
		},
		{
			ID: "repair-refused-scope-breach", Title: "a block of the UTC hour after the scope's, without the I-13 defect: not repairable",
			Cites: cites, Class: ClassRepair, Labels: []string{LabelNonconformingWriter},
			Build: repairBuild(segmentMeta, func(pack []byte) ([]byte, error) {
				// Block 2's only record, seq 5, moves to the next UTC hour; its F-2 entry keeps the time range,
				// so the pack is finalized-inconsistent and has something to repair.
				return setRowTime(pack, 2, 0, TimeBase+hourNs+5*int64(time.Millisecond))
			}),
			// The block outside the scope's hour is the only refusal condition the pack meets.
			Expect: repairBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{2},
				ExportHas: []string{`"seq":"5","ts_utc_ns":"1791003600005000000"`},
				Repair:    &RepairWant{Result: RepairNotRepairable},
			}),
		},
	}
}

// repairBasePack writes the pack most repair vectors start from, of meta, in three blocks of epoch 1:
// data of seq first, data of seqs first + 2 and first + 3, data of seq first + 5.
// The seqs between the blocks are absent,
// so that the footer's ranges and the neighbours' seqs bound a lost block differently.
// The pack's seq_start is first.
func repairBasePack(seed string, meta *tracepack.PackMeta, first uint64) ([]byte, error) {
	meta.SeqStart = first

	return writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{
		{footerData(first, 1)},
		{footerData(first+2, 1), footerData(first+3, 1)},
		{footerData(first+5, 1)},
	}})
}

// repairBuild returns the Build of a repair vector: repairBasePack of meta from seq 0, after edit when not nil.
// The unedited pack is the intact pack of the repair.
func repairBuild(meta func(seed string) *tracepack.PackMeta, edit func(pack []byte) ([]byte, error)) func(seed string) (*Built, error) {
	return repairBuildFrom(0, meta, edit)
}

// repairBuildFrom is repairBuild with the seqs from first.
func repairBuildFrom(first uint64, meta func(seed string) *tracepack.PackMeta, edit func(pack []byte) ([]byte, error)) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		intact, err := repairBasePack(seed, meta(seed), first)
		if err != nil {
			return nil, err
		}
		pack := intact
		if edit != nil {
			if pack, err = edit(intact); err != nil {
				return nil, err
			}
		}

		return &Built{Pack: pack, Repair: &RepairInput{Intact: intact}}, nil
	}
}

// repairBaseExpect completes e as the expectation of an edited repairBasePack whose blocks all validate:
// three blocks, the seqs 0, 2, 3 and 5 unless e names others, the prefix ending with the blocks.
func repairBaseExpect(e *Expectation) *Expectation {
	e.Blocks, e.PrefixEnd = 3, AtEnd()
	if e.Seqs == nil {
		e.Seqs = []uint64{0, 2, 3, 5}
	}

	return e
}

// notNeededExpect is the expectation of a finalized-consistent repairBasePack without the seq_start defect,
// whose export holds has: nothing to repair, whatever else holds (the tracepack format specification §13).
func notNeededExpect(has []string) *Expectation {
	return repairBaseExpect(&Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, ExportHas: has,
		Repair: &RepairWant{Result: RepairNotNeeded},
	})
}

// middleFailedRepairExpect is the expectation of a repairBasePack whose middle block fails its body CRC:
// corrupt-middle, block 0 disagreeing with the footer when disagree, the footer rejected when rejected,
// and a patch of blocks 0 and 2 adding coverage.
// verify.json's lost comes from a valid footer, trusted or not; a rejected footer gives none.
func middleFailedRepairExpect(disagree, rejected bool, coverage CoverageWant) *Expectation {
	e := &Expectation{
		Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: !rejected, Blocks: 3, Seqs: []uint64{0, 5}, PrefixEnd: AtBlock(1),
		Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}},
		Repair: &RepairWant{Result: RepairPatched, Blocks: 2, Records: 2, Coverage: []CoverageWant{coverage}},
	}
	if disagree {
		e.Disagreeing = []int{0}
	}
	if !rejected {
		e.Lost = []LostWant{{First: 2, Last: 3, TimeStart: msAt(2), TimeEnd: msAt(3)}}
	}

	return e
}

// footerCoverage is a new coverage entry from a valid footer: the seqs first to last, within their records' times.
func footerCoverage(first, last uint64) CoverageWant {
	return CoverageWant{First: first, Last: new(last), TimeStart: msAt(int64(first)), TimeEnd: msAt(int64(last))}
}

// hourCoverage is a new coverage entry of a lost run:
// the seqs from first to last (nil for none), within TimeBase's UTC hour.
func hourCoverage(first uint64, last *uint64) CoverageWant {
	return CoverageWant{First: first, Last: last, TimeStart: TimeBase, TimeEnd: TimeBase + hourNs - 1}
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

// extractMeta returns the pack metadata of an extract of seed, its period that of segmentMeta.
func extractMeta(seed string) *tracepack.PackMeta {
	meta := segmentMeta(seed)
	meta.PackRole, meta.ScopeGeneration, meta.ExtractFilter = tracepack.PackRoleExtract, nil, new("all records")

	return meta
}

// rejectFooter returns pack with its F-5 record_count one above its 4 records:
// a footer that passes its CRC and fails validation.
func rejectFooter(pack []byte) ([]byte, error) {
	return PatchFooter(pack, func(d []byte) ([]byte, error) {
		f5, err := F5List(d)
		if err != nil {
			return nil, err
		}

		return d, SetU64(d, f5, f5RecordCountTag, 5)
	})
}

// seqOrderEdit returns a repairBasePack whose block 2 holds seq 1 instead of 5,
// its envelope's and F-2 entry's first_seq with it:
// the block breaks I-12 after block 1, and the footer states it.
func seqOrderEdit(pack []byte) ([]byte, error) {
	return ReencodeBlocks(pack, func(i int, b *Block) error {
		if i == 2 {
			b.Headers[0].Seq, b.Envelope.FirstSeq = 1, 1
		}

		return nil
	})
}

// setRowTime returns pack with the ts_utc_ns of record k of block i set to ts; the footer keeps its values.
func setRowTime(pack []byte, i, k int, ts int64) ([]byte, error) {
	return PatchBody(pack, i, func(b *Body) error {
		binary.LittleEndian.PutUint64(b.Rows[k*b.RecordHeaderLen+rowTSOff:], uint64(ts))
		return nil
	})
}

// repairTruncationVector is repair-truncation: truncationBasePack under codec none, every cut verified and repaired.
// Every cut that opens is unfinalized, so its patch adds a tail entry from the seq after its last validated record,
// or from seq_start without one, without seq_last.
func repairTruncationVector() Recipe {
	e := truncationExpect()
	tail := func(blocks int, first uint64) *RepairWant {
		return &RepairWant{Result: RepairPatched, Blocks: blocks, Coverage: []CoverageWant{hourCoverage(first, nil)}}
	}
	for k, r := range []*RepairWant{tail(0, 0), tail(0, 0), tail(1, 2), tail(1, 2), tail(2, 4), tail(2, 4)} {
		e.Cuts[k+2].Repair = r
	}

	return Recipe{
		ID: "repair-truncation", Title: "a pack of two blocks cut at every byte offset, each cut verified and repaired",
		Cites: []string{"FMT §16", "FMT §13", "CORPUS §5.7"}, Class: ClassTruncation,
		Build: func(seed string) (*Built, error) {
			pack, err := truncationBasePack(seed, tracepack.CodecNone)

			return &Built{Pack: pack, Repair: &RepairInput{}}, err
		},
		Expect: e,
	}
}

// repairOfPatchVector is repair-of-patch: the patch of a repairBasePack whose middle block failed,
// its last block failing in turn, repaired again.
func repairOfPatchVector() Recipe {
	return Recipe{
		ID: "repair-of-patch", Title: "a patch whose last block fails its body CRC: its coverage inherited",
		Cites: []string{"FMT §13", "FMT §16", "CORPUS §5.6", "STO §6", "CORPUS §5.4"}, Class: ClassRepair, Labels: []string{LabelDamaged},
		Build: func(seed string) (*Built, error) {
			meta := segmentMeta(seed)
			meta.SeqStart = 0
			source, err := writeSpec(&packSpec{seed: seed, meta: meta, packID: IDFor(roleSourcePackID, seed), blocks: [][]tracepack.Record{
				{footerData(0, 1)},
				{footerData(2, 1), footerData(3, 1)},
				{footerData(5, 1)},
			}})
			if err != nil {
				return nil, err
			}
			if source, err = flipBody(source, 1); err != nil {
				return nil, err
			}
			// The first patch, which is pack.tpk once damaged, takes the pack_id of the vector's pack.
			opts := RepairOptionsFor(seed, &RepairInput{})
			opts.PackID = IDFor(rolePackID, seed).String()
			_, intact, err := RunRepair(context.Background(), source, opts)
			if err != nil {
				return nil, err
			}
			pack, err := flipBody(intact, 1)
			if err != nil {
				return nil, err
			}

			return &Built{Pack: pack, Queries: []QuerySpec{allQuery()}, Repair: &RepairInput{Intact: intact}}, nil
		},
		// The patch holds blocks of seqs 0 and 5 and the coverage of seqs 2 and 3;
		// its last block fails, and its valid footer gives the range.
		// The new patch carries that coverage and adds seq 5's.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 2, Seqs: []uint64{0}, PrefixEnd: AtBlock(1),
			Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}},
			Lost:   []LostWant{{First: 5, Last: 5, TimeStart: msAt(5), TimeEnd: msAt(5)}},
			Queries: []QueryWant{{ID: "all", Seqs: []uint64{0}, Incomplete: []IncompleteWant{
				CoverageHit(), BlockDefect(tracepack.ReasonCorruptBlock, 1),
			}}},
			Repair: &RepairWant{Result: RepairPatched, Blocks: 1, Records: 1, Coverage: []CoverageWant{footerCoverage(5, 5)}},
		},
	}
}
