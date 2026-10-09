package corpus

import (
	"encoding/binary"
	"time"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Positions in a gathered record header and tags the verify group edits
// (the tracepack format specification §5, §7.1 and §8).
const (
	rowTSOff            = 8
	rowFieldValidityOff = 42

	metaSeqStartTag  uint16 = 0x0027
	eventCurStateTag uint16 = 0x0003
)

// verifyVectors returns the recipes of the verify group (the tracepack corpus specification §9.4).
func verifyVectors() []Recipe {
	return append(truncationVectors(), []Recipe{
		{
			ID: "verify-last-block-failed", Title: "the last block failing its body CRC, the footer valid",
			Cites: []string{"FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelDamaged},
			Build: func(seed string) (*Built, error) {
				return editedCodecPack(seed, func(pack []byte) ([]byte, error) { return flipBody(pack, 2) })
			},
			// No validated block follows the failed one, so the pack is finalized-truncated; its F-3 summary gives the lost range.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedTruncated, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 7),
				Failed: []FailedWant{{Block: 2, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(2),
				Lost: []LostWant{{First: 8, Last: 11, TimeStart: msAt(8), TimeEnd: msAt(11)}},
			},
		},
		{
			ID: "verify-corrupt-middle-walked", Title: "a middle block failing its body CRC, its envelope intact, the footer rejected",
			Cites: []string{"FMT §13", "FMT I-1", "FMT §16"}, Class: ClassRead, Labels: []string{LabelDamaged, LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				return editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					// F-5's record_count one above the records: a footer that passes its CRC and fails validation.
					rejected, err := PatchFooter(pack, func(d []byte) ([]byte, error) {
						f5, err := F5List(d)
						if err != nil {
							return nil, err
						}

						return d, SetU64(d, f5, f5RecordCountTag, 13)
					})
					if err != nil {
						return nil, err
					}

					return flipBody(rejected, 1)
				})
			},
			// Without a valid footer the walk goes on past the failed block, whose envelope accounts for it,
			// so a validated block follows the failed one: corrupt-middle.
			// The walk did not stop, so its three blocks of twelve records are compared with the trailer's totals, and agree.
			// Without a valid footer nothing gives the lost range.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeCorruptMiddle, Blocks: 3, Seqs: append(seqRange(0, 3), seqRange(8, 11)...),
				Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
			},
		},
		{
			ID: "verify-f2-disagrees", Title: "a block whose records disagree with its F-2 entry, the footer valid",
			Cites: []string{"FMT §13", "FMT §10", "CORPUS §5.4"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				b, err := editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					// Block 0's F-2 ts_max moves from its last record's time to block 1's last, F-5's maximum unchanged.
					return PatchFooter(pack, func(d []byte) ([]byte, error) {
						e, err := F2Entry(d, 0)
						if err != nil {
							return nil, err
						}

						return d, setLE64(d, e.Off+f2TSMaxOff, uint64(msAt(3)), uint64(msAt(7)))
					})
				})
				if err == nil {
					b.Queries = []QuerySpec{allQuery()}
				}

				return b, err
			},
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 11), PrefixEnd: AtEnd(),
				Disagreeing: []int{0},
				Queries:     []QueryWant{{ID: "all", Seqs: seqRange(0, 11), Incomplete: []IncompleteWant{BlockDefect(tracepack.ReasonIndexMismatch, 0)}}},
			},
		},
		{
			ID: "verify-f3-disagrees", Title: "a block whose records disagree with its F-3 summary only",
			Cites: []string{"FMT §13", "FMT §10", "CORPUS §5.4"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				b, err := editedCodecPack(seed, func(pack []byte) ([]byte, error) {
					// ordering-uncertain in block 0's F-3 quality_union and F-5's, which no record carries.
					return orFooterQuality(pack, 0, uint64(tracepack.QualityOrderingUncertain))
				})
				if err == nil {
					b.Queries = []QuerySpec{allQuery()}
				}

				return b, err
			},
			// The disagreement makes the pack finalized-inconsistent,
			// and a read reports nothing (the tracepack corpus specification §5.4).
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 11), PrefixEnd: AtEnd(),
				Disagreeing: []int{0},
				Queries:     []QueryWant{{ID: "all", Seqs: seqRange(0, 11)}},
			},
		},
		{
			ID: "verify-defect-event-field-validity", Title: "a socket-connect record whose field_validity is not 0",
			Cites: []string{"FMT §13", "FMT §7.2"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: verifyEdit(func(pack []byte) ([]byte, error) {
				// The socket-connect at seq 1 gains field_validity bit 0, session_id; no footer value states field_validity.
				return PatchBody(pack, 0, func(b *Body) error {
					b.Rows[b.RecordHeaderLen+rowFieldValidityOff] = 0x01
					return nil
				})
			}),
			Expect: verifyBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true,
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectEventFieldValidity, Seq: 1}},
				ExportHas:     []string{`"kind":"transport-event","dir":"local","fidelity":"synthesized","decode_status":"not-applicable","field_validity":["session_id"]`},
			}),
		},
		{
			ID: "verify-defect-event-payload", Title: "a state-transition event whose body is not a valid TLV body",
			Cites: []string{"FMT §13", "FMT §8", "FMT §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				// event state-transition, then cur_state as a u8 entry of two bytes: a fixed-length value of the wrong length.
				payload := tlv.AppendEntry(nil, tlv.U8Entry(eventEventTag, uint8(tracepack.EventStateTransition)))
				payload = tlv.AppendEntry(payload, tlv.Entry{Tag: eventCurStateTag, Type: tlv.TypeU8, Value: []byte{1, 2}})
				b, err := invalidBodyBuild(payload)(seed)
				if err == nil {
					b.Footer = false
				}

				return b, err
			},
			// The Writer stores the body and leaves it out of its summaries; a state-transition is no boundary and ends no epoch,
			// so the footer agrees with the records and the defect is the pack's only fault.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectEventPayload, Seq: 2}},
			},
		},
		{
			ID: "verify-defect-seq-order", Title: "a block whose first seq is below the last seq of the block before it, the footer stating it",
			Cites: []string{"FMT §13", "FMT I-12", "FMT §10"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
					{footerData(0, 1), footerData(1, 1), footerData(5, 1)},
					{footerData(6, 1), footerData(7, 1)},
				}})
				if err != nil {
					return nil, err
				}
				// Block 1's records become seqs 2 and 3, and its envelope's and F-2 entry's first_seq 2.
				pack, err = ReencodeBlocks(pack, func(i int, b *Block) error {
					if i == 1 {
						b.Headers[0].Seq, b.Headers[1].Seq, b.Envelope.FirstSeq = 2, 3, 2
					}

					return nil
				})

				return &Built{Pack: pack}, err
			},
			// The footer states the defect, so its validation rejects it (F-2 first_seq 2 after last_seq 5);
			// the walk validates both blocks by their envelopes and agrees with the trailer's totals.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, Blocks: 2, Seqs: []uint64{0, 1, 5, 2, 3}, PrefixEnd: AtEnd(),
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqOrder, Seq: 2}},
				Stats:         &StatsWant{},
			},
		},
		{
			ID: "verify-defect-hour-span", Title: "a block whose records lie in two UTC hours, its F-2 entry stating one",
			Cites: []string{"FMT §13", "FMT I-13", "FMT §10"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: verifyEdit(func(pack []byte) ([]byte, error) {
				// Block 1's last record, seq 3, moves to the next UTC hour; the F-2 entry keeps its time range.
				return PatchBody(pack, 1, func(b *Body) error {
					binary.LittleEndian.PutUint64(b.Rows[b.RecordHeaderLen+rowTSOff:], uint64(TimeBase+int64(time.Hour)))
					return nil
				})
			}),
			// A footer that stated the defect would be rejected; this one disagrees with the block's records instead.
			Expect: verifyBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Disagreeing: []int{1},
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectHourSpan, Seq: 2}},
				ExportHas:     []string{`"seq":"3","ts_utc_ns":"1791003600000000000"`},
			}),
		},
		{
			ID: "verify-seq-start-above", Title: "a first block whose first seq is above seq_start",
			Cites: []string{"FMT §13", "FMT §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: verifyEdit(func(pack []byte) ([]byte, error) { return setSeqStart(pack, 2, 0) }, 2),
			// The footer does not state seq_start, so the defect is the pack's only fault.
			Expect: verifyBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Seqs: seqRange(2, 5),
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqStart, Seq: 2}},
			}),
		},
		{
			ID: "verify-seq-start-below", Title: "a first block whose first seq is below seq_start",
			Cites: []string{"FMT §13", "FMT §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: verifyEdit(func(pack []byte) ([]byte, error) { return setSeqStart(pack, 0, 1) }),
			Expect: verifyBaseExpect(&Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true,
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqStart, Seq: 0}},
			}),
		},
		{
			ID: "verify-seq-start-after-failed", Title: "a failed first block, then a block whose first seq is seq_start",
			Cites: []string{"FMT §13", "FMT §5"}, Class: ClassRead, Labels: []string{LabelDamaged, LabelNonconformingWriter},
			Build: verifyEdit(func(pack []byte) ([]byte, error) {
				pack, err := setSeqStart(pack, 0, 2)
				if err != nil {
					return nil, err
				}

				return flipBody(pack, 0)
			}),
			// The failed block holds at least one record, so the first validated block must start above seq_start.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 2, Seqs: seqRange(2, 3),
				Failed: []FailedWant{{Block: 0, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(0),
				WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectSeqStart, Seq: 2}},
				Lost:          []LostWant{{First: 0, Last: 1, TimeStart: msAt(0), TimeEnd: msAt(1)}},
			},
		},
	}...)
}

// verifyBasePack writes the pack most verify vectors edit, its seqs from first on, in two blocks of epoch 1:
// data (first) and a socket-connect (first + 1), then data (first + 2 and first + 3); seq_start is first.
func verifyBasePack(seed string, first uint64) ([]byte, error) {
	connect, err := newSocketEvent(first+1, 1, tracepack.EventSocketConnect)
	if err != nil {
		return nil, err
	}
	meta := segmentMeta(seed)
	meta.SeqStart = first

	return writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{
		{footerData(first, 1), connect},
		{footerData(first+2, 1), footerData(first+3, 1)},
	}})
}

// verifyEdit returns the Build of a vector that is verifyBasePack after edit,
// its seqs from the optional first, else from 0.
func verifyEdit(edit func(pack []byte) ([]byte, error), first ...uint64) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		var from uint64
		if len(first) > 0 {
			from = first[0]
		}
		pack, err := verifyBasePack(seed, from)
		if err != nil {
			return nil, err
		}
		if pack, err = edit(pack); err != nil {
			return nil, err
		}

		return &Built{Pack: pack}, nil
	}
}

// verifyBaseExpect completes e as the expectation of an edited verifyBasePack whose blocks all validate:
// two blocks, the seqs 0 to 3 unless e names others, the prefix ending with the blocks.
func verifyBaseExpect(e *Expectation) *Expectation {
	e.Blocks, e.PrefixEnd = 2, AtEnd()
	if e.Seqs == nil {
		e.Seqs = seqRange(0, 3)
	}

	return e
}

// setSeqStart returns pack with its metadata's seq_start changed from old to v.
func setSeqStart(pack []byte, old, v uint64) ([]byte, error) {
	return editMeta(pack, func(meta []byte) ([]byte, error) {
		at, err := EntryOffset(meta, Section{Len: len(meta)}, metaSeqStartTag, 0)
		if err != nil {
			return nil, err
		}

		return meta, setLE64(meta, at+tlv.HeaderLen, old, v)
	})
}

// flipBody returns pack with the first byte of block i's body inverted, so that the block fails its body CRC.
func flipBody(pack []byte, i int) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	s, err := l.block(i)
	if err != nil {
		return nil, err
	}

	return FlipByte(pack, int(s.Offset)+format.EnvelopeLen)
}
