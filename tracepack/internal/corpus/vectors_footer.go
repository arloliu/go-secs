package corpus

import (
	"encoding/binary"
	"fmt"
	"slices"
	"time"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Positions and tags the footer recipes edit (the tracepack format specification §7.1, §10 and §11).
const (
	f2LastSeqOff  = 32
	f2TSMinOff    = 40
	f2TSMaxOff    = 48
	f2EpochMaxOff = 60

	trailerRecordCountOff = 32
	trailerLastSeqOff     = 40

	rowQualityOff = 36

	f3KindCountsTag         uint16 = 0x0001
	f3DirCountsTag          uint16 = 0x0002
	f3DecodeStatusCountsTag uint16 = 0x0003
	f3QualityUnionTag       uint16 = 0x0005
	// seqRangeTag is the tag of seq_range in F-3 and in F-5.
	seqRangeTag uint16 = 0x0009
	// retiredF3Tag and retiredF3TagLen are the retired F-3 tags 0x0004 and 0x0006, which a reader treats as unknown.
	retiredF3Tag    uint16 = 0x0004
	retiredF3TagLen uint16 = 0x0006

	f5KindCountsTag         uint16 = 0x0002
	f5DirCountsTag          uint16 = 0x0003
	f5DecodeStatusCountsTag uint16 = 0x0004
	f5TSMinTag              uint16 = 0x0005
	f5QualityUnionTag       uint16 = 0x0008

	epochRecordCountTag uint16 = 0x0002
	epochSeqFirstTag    uint16 = 0x0003
	epochSeqLastTag     uint16 = 0x0004

	// Transport-event tags of the hand-written invalid bodies (the tracepack format specification §8).
	eventEventTag        uint16 = 0x0001
	eventSocketRoleTag   uint16 = 0x0007
	eventBoundaryKindTag uint16 = 0x000D
	eventDetailTag       uint16 = 0x0012
)

// footerBaseSeqs are the seqs of footerBasePack's records: block 0 misses seq 2.
var footerBaseSeqs = []uint64{0, 1, 3, 4, 5, 6, 7}

// footerVectors returns the recipes of the footer group (the tracepack corpus specification §9.4).
func footerVectors() []Recipe {
	out := invalidFooterVectors()

	return append(out,
		Recipe{
			ID: "footer-seq-range-hides-gap", Title: "a block missing a seq whose F-3 summary has no seq_range",
			Cites: []string{"FMT §10", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: footerBaseEdit(checked(func(pack []byte) ([]byte, error) {
				return PatchFooter(pack, func(d []byte) ([]byte, error) {
					parts, err := SplitFooter(d)
					if err != nil {
						return nil, err
					}
					list, err := F3List(d, 0)
					if err != nil {
						return nil, err
					}
					dropped, err := DropEntries(d[list.Off:list.Off+list.Len], seqRangeTag)
					if err != nil {
						return nil, err
					}
					if err := parts.SetF3List(0, dropped); err != nil {
						return nil, err
					}
					// F-5 states the union of the blocks' ranges as they now read, 0 to 7, so that it does not reveal the gap.
					if parts.F5, err = replaceSeqRanges(parts.F5, tlv.SeqRange{First: 0, Last: 7}); err != nil {
						return nil, err
					}

					return parts.Join()
				})
			}, checkHiddenGap)),
			// Without seq_range, block 0's seqs 0 to 3 are four, its record_count three: the footer is invalid.
			Expect: invalidFooterExpect(),
		},
		Recipe{
			ID: "footer-retired-f3-tags-and-f4", Title: "F-3 lists holding the retired tags 0x0004 and 0x0006, and a present F-4",
			Cites: []string{"FMT §10"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: footerBaseEdit(func(pack []byte) ([]byte, error) {
				pack, err := PatchFooter(pack, func(d []byte) ([]byte, error) {
					parts, err := SplitFooter(d)
					if err != nil {
						return nil, err
					}
					for i := range parts.Entries {
						list, err := F3List(d, i)
						if err != nil {
							return nil, err
						}
						retired := tlv.AppendEntry(nil, tlv.U64Entry(retiredF3Tag, 1))
						retired = tlv.AppendEntry(retired, tlv.Entry{Tag: retiredF3TagLen, Type: tlv.TypeBytes, Value: []byte{0xA5, byte(i)}})
						if err := parts.SetF3List(i, slices.Concat(d[list.Off:list.Off+list.Len], retired)); err != nil {
							return nil, err
						}
					}
					parts.SetF4([]byte("an F-4 slot a reader ignores"))

					return parts.Join()
				})
				if err != nil {
					return nil, err
				}

				return pack, checkRetiredAndF4(pack)
			}),
			// Both are ignored: the footer is valid and agrees with the records.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2, Seqs: footerBaseSeqs, PrefixEnd: AtEnd(),
				Stats: footerBaseStats(),
			},
		},
		closeTwiceOneBlock(),
		Recipe{
			ID: "footer-f3-higher-close-seq", Title: "in one block, F-3 stating the higher close_seq of an epoch ended twice, F-5 recomputed from it",
			Cites: []string{"FMT §10", "FMT §13", "FMT §16"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				pack, err := closeOncePack(seed)
				if err != nil {
					return nil, err
				}
				pack, err = PatchFooter(pack, func(d []byte) ([]byte, error) {
					f3, err := F3List(d, 0)
					if err != nil {
						return nil, err
					}
					f5, err := F5List(d)
					if err != nil {
						return nil, err
					}
					if err := setNested(d, f3, f3EpochTag, 0, epochCloseSeqTag, 2, 3); err != nil {
						return nil, err
					}

					return d, setNested(d, f5, f5EpochTag, 0, epochCloseSeqTag, 2, 3)
				})

				return &Built{Pack: pack, Footer: true}, err
			},
			// The footer is consistent with itself, so it is valid and the statistics state 3;
			// the block's records end the epoch first at seq 2, so they disagree with its F-3 summary.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
				Disagreeing: []int{0},
				Stats:       &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1, CloseSeq: new(uint64(3))}}, Boundaries: closingStopStats()},
				Footer: &Footer{
					Stored: &FooterProjection{
						Blocks: []FooterBlock{{Block: 0, CloseSeqs: []CloseSeq{{Epoch: 1, Seq: 3}}, Boundaries: closingStop()}},
						F5:     FooterF5{Epochs: []F5Epoch{{Epoch: 1, CloseSeq: new(U64(3))}}, Boundaries: closingStop()},
					},
					Accepted:   true,
					Recomputed: closeOnceProjection(),
				},
			},
		},
		closeTwiceTwoBlocks(),
		Recipe{
			ID: "footer-f5-higher-close-seq", Title: "F-5 stating the higher close_seq of an epoch ended in two blocks",
			Cites: []string{"FMT §10", "FMT §16"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				pack, err := closeAcrossPack(seed)
				if err != nil {
					return nil, err
				}
				pack, err = PatchFooter(pack, func(d []byte) ([]byte, error) {
					f5, err := F5List(d)
					if err != nil {
						return nil, err
					}

					return d, setNested(d, f5, f5EpochTag, 0, epochCloseSeqTag, 2, 3)
				})

				return &Built{Pack: pack, Footer: true}, err
			},
			// F-5's close_seq must be the lowest of the blocks', 2: the footer is invalid and the statistics unavailable.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedInconsistent, Blocks: 2, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
				Stats: &StatsWant{},
				Footer: &Footer{
					Stored: &FooterProjection{
						Blocks: closeAcrossProjection().Blocks,
						F5:     FooterF5{Epochs: []F5Epoch{{Epoch: 1, CloseSeq: new(U64(3))}}, Boundaries: closingStop()},
					},
					Recomputed: closeAcrossProjection(),
				},
			},
		},
		Recipe{
			ID: "footer-epoch-without-closure", Title: "an epoch bounded by start and stop-unclean boundaries that no record ends",
			Cites: []string{"FMT §10", "FMT §9"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				start, err := newBoundary(0, tracepack.BoundaryKindStart)
				if err != nil {
					return nil, err
				}
				unclean, err := newBoundary(2, tracepack.BoundaryKindStopUnclean)
				if err != nil {
					return nil, err
				}
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
					{start, footerData(1, 1), unclean},
				}})

				return &Built{Pack: pack, Footer: true}, err
			},
			// Neither a start nor a stop-unclean boundary ends an epoch.
			Expect: func() *Expectation {
				bounds := []Boundary{{Seq: 0, Kind: "start"}, {Seq: 2, Kind: "stop-unclean"}}
				p := FooterProjection{
					Blocks: []FooterBlock{{Block: 0, Boundaries: bounds}},
					F5:     FooterF5{Epochs: []F5Epoch{{Epoch: 1}}, Boundaries: bounds},
				}

				return &Expectation{
					Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
					Stats: &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1}}, Boundaries: []BoundaryWant{
						{Seq: 0, Kind: tracepack.BoundaryKindStart}, {Seq: 2, Kind: tracepack.BoundaryKindStopUnclean},
					}},
					Footer: &Footer{Stored: &p, Accepted: true, Recomputed: p},
				}
			}(),
		},
		Recipe{
			ID: "footer-invalid-socket-close-body", Title: "a socket-close event whose body repeats socket_role",
			Cites: []string{"FMT §10", "FMT §8", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: invalidBodyBuild(func() []byte {
				b := tlv.AppendEntry(nil, tlv.U8Entry(eventEventTag, uint8(tracepack.EventSocketClose)))
				b = tlv.AppendEntry(b, tlv.U8Entry(eventSocketRoleTag, uint8(tracepack.SocketRoleActive)))

				return tlv.AppendEntry(b, tlv.U8Entry(eventSocketRoleTag, uint8(tracepack.SocketRoleActive)))
			}()),
			Expect: invalidBodyExpect(),
		},
		Recipe{
			ID: "footer-invalid-stop-body", Title: "a clean-stop capture-boundary event whose last entry runs past the body",
			Cites: []string{"FMT §10", "FMT §8", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: invalidBodyBuild(func() []byte {
				b := tlv.AppendEntry(nil, tlv.U8Entry(eventEventTag, uint8(tracepack.EventCaptureBoundary)))
				b = tlv.AppendEntry(b, tlv.U8Entry(eventBoundaryKindTag, uint8(tracepack.BoundaryKindStop)))
				// A detail entry whose header declares 4 bytes, followed by 1.
				b = binary.LittleEndian.AppendUint16(b, eventDetailTag)
				b = append(b, byte(tlv.TypeUTF8), 0)
				b = binary.LittleEndian.AppendUint32(b, 4)

				return append(b, 'x')
			}()),
			Expect: invalidBodyExpect(),
		},
		Recipe{
			ID: "footer-quality-bit0-data", Title: "a data record carrying quality bit 0 without being a capture-boundary record",
			Cites: []string{"FMT §10", "FMT §9", "SEM §6"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: func(seed string) (*Built, error) {
				pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
					{footerData(0, 1), footerData(1, 1), footerData(2, 1)},
				}})
				if err != nil {
					return nil, err
				}
				// The Writer clears the bit on a record that is not a capture-boundary record, so it is set by surgery,
				// and the block's F-3 quality_union and F-5's follow it.
				pack, err = PatchBody(pack, 0, func(b *Body) error {
					b.Rows[b.RecordHeaderLen+rowQualityOff] |= byte(tracepack.QualityCaptureBoundary)
					return nil
				})
				if err != nil {
					return nil, err
				}
				pack, err = orFooterQuality(pack, 0, uint64(tracepack.QualityCaptureBoundary))

				return &Built{Pack: pack, Footer: true}, err
			},
			Expect: func() *Expectation {
				p := FooterProjection{Blocks: []FooterBlock{{Block: 0}}, F5: FooterF5{Epochs: []F5Epoch{{Epoch: 1}}}}

				return &Expectation{
					Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
					ExportHas: []string{`"seq":"1","ts_utc_ns":"1791000000001000000","mono_ns":"0","epoch":1,"payload_len":14,"trailing_bytes":0,"quality":["capture-boundary"]`},
					Stats:     &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1}}},
					Footer:    &Footer{Stored: &p, Accepted: true, Recomputed: p},
				}
			}(),
		},
	)
}

// checkRetiredAndF4 checks that pack's footer holds what footer-retired-f3-tags-and-f4 is about:
// F-1 flag bit 1 set and a non-empty F-4, and in each block's F-3 list an entry of each retired tag.
func checkRetiredAndF4(pack []byte) error {
	d, err := DecodedFooter(pack)
	if err != nil {
		return err
	}
	parts, err := SplitFooter(d)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint16(parts.Prologue[prologueFlagsOff:])&footerFlagF4Present == 0 || len(parts.F4) == 0 {
		return fmt.Errorf("%w: the footer holds no F-4", ErrPackEdit)
	}
	for i := range parts.Entries {
		list, err := F3List(d, i)
		if err != nil {
			return err
		}
		for _, tag := range []uint16{retiredF3Tag, retiredF3TagLen} {
			if _, err := EntryOffset(d, list, tag, 0); err != nil {
				return fmt.Errorf("block %d's F-3 list: %w", i, err)
			}
		}
	}

	return nil
}

// invalidFooterVectors returns the footer-invalid vectors:
// footerBasePack with one footer validation clause of the tracepack format specification §10 broken, every CRC valid,
// each edit chosen so that every other clause still holds where the clause allows it.
// Two clauses cannot be broken alone:
// an empty epoch range leaves the epoch entries outside it, so footer-invalid-epoch-range also breaks the epoch-entry clause;
// and an overflowing sum is never the u64 count it should equal, so footer-invalid-overflow also breaks the exact count sums.
// footer-invalid-empty-stats breaks only its clause, in a pack without blocks.
func invalidFooterVectors() []Recipe {
	clauses := slices.Concat(layoutClauses(), summaryClauses(), boundsClauses(), trailerClauses())
	out := make([]Recipe, 0, len(clauses)+1)
	for _, c := range clauses {
		r := Recipe{
			ID: c.id, Title: c.title, Cites: []string{"FMT §10", "FMT §13"}, Class: ClassRead,
			Labels: []string{LabelNonconformingWriter},
			Build:  footerBaseEdit(c.edit), Expect: invalidFooterExpect(),
		}
		switch c.id {
		case "footer-invalid-f5-aggregate":
			// The forward walk accounts for every block and agrees with the trailer: the rejected footer alone is not incomplete.
			r.Build = withQueries(r.Build, allQuery())
			r.Expect.Queries = []QueryWant{{ID: "all", Seqs: footerBaseSeqs}}
		case "footer-invalid-trailer-record-count":
			// The walked blocks hold 7 records, the trailer says 8.
			r.Build = withQueries(r.Build, allQuery())
			r.Expect.TrailerTotalsDisagree = true
			r.Expect.Queries = []QueryWant{{ID: "all", Seqs: footerBaseSeqs, Incomplete: []IncompleteWant{Truncated(Pos{})}}}
		default:
			// Every other footer-invalid vector has only the files of its class.
		}
		out = append(out, r)
	}

	// F-3 must be empty in a pack without blocks; the 16-byte list no F-2 entry names leaves every other clause intact,
	// F-5 among them, the aggregate of no blocks.
	return append(out, Recipe{
		ID: "footer-invalid-empty-stats", Title: "a pack without blocks whose F-3 is not empty",
		Cites: []string{"FMT §10", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
		Build: func(seed string) (*Built, error) {
			pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed)})
			if err != nil {
				return nil, err
			}
			pack, err = footerEdit(func(d []byte) ([]byte, error) {
				p, err := SplitFooter(d)
				if err != nil {
					return nil, err
				}
				p.F3 = tlv.AppendEntry(p.F3, tlv.U64Entry(f3QualityUnionTag, 0))

				return p.Join()
			})(pack)

			return &Built{Pack: pack}, err
		},
		Expect: &Expectation{Outcome: tracepack.OutcomeFinalizedInconsistent, PrefixEnd: AtEnd(), Stats: &StatsWant{}},
	})
}

// footerClause is a footer validation clause of the tracepack format specification §10:
// the vector that breaks it, and the edit of footerBasePack that does.
type footerClause struct {
	id, title string
	edit      func(pack []byte) ([]byte, error)
}

// layoutClauses are the clauses on the placement of the footer's sections and of the blocks F-2 locates.
func layoutClauses() []footerClause {
	return []footerClause{
		{"footer-invalid-f2-offset", "f2_offset 80: F-2 behind 8 bytes after the prologue, every section in place", footerEdit(func(d []byte) ([]byte, error) {
			const pad = 8
			out := slices.Concat(d[:format.FooterPrologueLen], make([]byte, pad), d[format.FooterPrologueLen:])
			for _, off := range []int{prologueF2OffsetOff, prologueF3OffsetOff, prologueF4OffsetOff, prologueF5OffsetOff} {
				binary.LittleEndian.PutUint64(out[off:], binary.LittleEndian.Uint64(out[off:])+pad)
			}

			return out, nil
		})},
		{"footer-invalid-f2-entry-len", "F-2 entries of 79 bytes, without their last reserved byte", checked(footerEdit(func(d []byte) ([]byte, error) {
			p, err := SplitFooter(d)
			if err != nil {
				return nil, err
			}
			out := slices.Clone(p.Prologue)
			for _, e := range p.Entries {
				if len(e) != format.F2EntryLen {
					return nil, fmt.Errorf("%w: an F-2 entry of %d bytes", ErrPackEdit, len(e))
				}
				out = append(out, e[:format.F2EntryLen-1]...)
			}
			f3 := uint64(len(out))
			out = slices.Concat(out, p.F3, p.F4, p.F5)
			binary.LittleEndian.PutUint32(out[prologueF2EntryLenOff:], format.F2EntryLen-1)
			binary.LittleEndian.PutUint64(out[prologueF3OffsetOff:], f3)
			binary.LittleEndian.PutUint64(out[prologueF4OffsetOff:], f3+uint64(len(p.F3)))
			binary.LittleEndian.PutUint64(out[prologueF5OffsetOff:], f3+uint64(len(p.F3)+len(p.F4)))

			return out, nil
		}), checkShortEntries)},
		{"footer-invalid-sections", "F-5 placed before F-3", footerEdit(func(d []byte) ([]byte, error) {
			p, err := SplitFooter(d)
			if err != nil {
				return nil, err
			}
			out := slices.Concat(p.Prologue, slices.Concat(p.Entries...), p.F5, p.F3)
			f5 := uint64(format.FooterPrologueLen + len(p.Entries)*format.F2EntryLen)
			binary.LittleEndian.PutUint64(out[prologueF5OffsetOff:], f5)
			binary.LittleEndian.PutUint64(out[prologueF3OffsetOff:], f5+uint64(len(p.F5)))
			binary.LittleEndian.PutUint64(out[prologueF4OffsetOff:], uint64(len(out)))

			return out, nil
		})},
		{"footer-invalid-f2-contiguity", "block 1's F-2 offset one byte past the end of block 0", footerEdit(func(d []byte) ([]byte, error) {
			e, err := F2Entry(d, 1)
			if err != nil {
				return nil, err
			}
			binary.LittleEndian.PutUint64(d[e.Off+f2OffsetOff:], binary.LittleEndian.Uint64(d[e.Off+f2OffsetOff:])+1)

			return d, nil
		})},
	}
}

// summaryClauses are the clauses on the seqs and counts of F-2 and F-3, and on F-5.
func summaryClauses() []footerClause {
	return []footerClause{
		{"footer-invalid-seq-order", "block 0 claiming seqs 0, 1 and 4, its last_seq block 1's first_seq", footerEdit(func(d []byte) ([]byte, error) {
			e, err := F2Entry(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setLE64(d, e.Off+f2LastSeqOff, 3, 4); err != nil {
				return nil, err
			}
			f3, err := F3List(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setSeqRange(d, f3, 1, [2]uint64{3, 3}, [2]uint64{4, 4}); err != nil {
				return nil, err
			}
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}

			return d, setSeqRange(d, f5, 1, [2]uint64{3, 7}, [2]uint64{4, 7})
		})},
		{"footer-invalid-summary-bounds", "F-3 one byte shorter than block 1's F-3 list, which ends it", checked(footerEdit(func(d []byte) ([]byte, error) {
			return d, setLE64(d, prologueF3LenOff, binary.LittleEndian.Uint64(d[prologueF3LenOff:]), binary.LittleEndian.Uint64(d[prologueF3LenOff:])-1)
		}), checkListPastF3)},
		{"footer-invalid-seq-set", "block 0's seq_range entries in descending order", footerEdit(func(d []byte) ([]byte, error) {
			f3, err := F3List(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setSeqRange(d, f3, 0, [2]uint64{0, 1}, [2]uint64{3, 3}); err != nil {
				return nil, err
			}

			return d, setSeqRange(d, f3, 1, [2]uint64{3, 3}, [2]uint64{0, 1})
		})},
		{"footer-invalid-count-sums", "block 0's kind_counts summing to 4 of its 3 records, F-5 their aggregate", footerEdit(func(d []byte) ([]byte, error) {
			f3, err := F3List(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setCount(d, f3, f3KindCountsTag, 4, int(tracepack.KindData), 2, 3); err != nil {
				return nil, err
			}
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}

			return d, setCount(d, f5, f5KindCountsTag, 8, int(tracepack.KindData), 5, 6)
		})},
		{"footer-invalid-f5-aggregate", "F-5 content_bytes one above the blocks' uncompressed_len", footerEdit(func(d []byte) ([]byte, error) {
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}
			at, err := EntryOffset(d, f5, f5ContentBytesTag, 0)
			if err != nil {
				return nil, err
			}
			binary.LittleEndian.PutUint64(d[at+tlv.HeaderLen:], binary.LittleEndian.Uint64(d[at+tlv.HeaderLen:])+1)

			return d, nil
		})},
	}
}

// boundsClauses are the per-block clauses on F-2's header length, times and epochs,
// on F-3's epoch entries and boundaries, and on the sums that may overflow.
func boundsClauses() []footerClause {
	return []footerClause{
		{"footer-invalid-header-len", "block 0's F-2 record_header_len 43", footerEdit(func(d []byte) ([]byte, error) {
			e, err := F2Entry(d, 0)
			if err != nil {
				return nil, err
			}
			if got := binary.LittleEndian.Uint16(d[e.Off+f2RecordHeaderLenOff:]); got != format.RecordHeaderLen {
				return nil, fmt.Errorf("%w: record_header_len %d, not %d", ErrPackEdit, got, format.RecordHeaderLen)
			}
			binary.LittleEndian.PutUint16(d[e.Off+f2RecordHeaderLenOff:], format.RecordHeaderLen-1)

			return d, nil
		})},
		{"footer-invalid-ts-range", "block 0's F-2 ts_min an hour earlier, in the previous UTC hour, F-5 ts_min with it", footerEdit(func(d []byte) ([]byte, error) {
			e, err := F2Entry(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setLE64(d, e.Off+f2TSMinOff, uint64(msAt(0)), uint64(msAt(0)-int64(time.Hour))); err != nil {
				return nil, err
			}
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}
			at, err := EntryOffset(d, f5, f5TSMinTag, 0)
			if err != nil {
				return nil, err
			}

			return d, setLE64(d, at+tlv.HeaderLen, uint64(msAt(0)), uint64(msAt(0)-int64(time.Hour)))
		})},
		// No epoch entry lies inside a range whose minimum exceeds its maximum, so block 0's entry for epoch 1 lies outside it too.
		{"footer-invalid-epoch-range", "block 0's F-2 epoch_max 0, below its epoch_min 1", footerEdit(func(d []byte) ([]byte, error) {
			e, err := F2Entry(d, 0)
			if err != nil {
				return nil, err
			}

			return d, setLE32(d, e.Off+f2EpochMaxOff, 1, 0)
		})},
		{"footer-invalid-epoch-entry", "block 0's epoch entry ending at seq 4, past the block's last seq 3", footerEdit(func(d []byte) ([]byte, error) {
			f3, err := F3List(d, 0)
			if err != nil {
				return nil, err
			}

			return d, setNested(d, f3, f3EpochTag, 0, epochSeqLastTag, 3, 4)
		})},
		{"footer-invalid-boundary-seq", "block 0's boundary at seq 2, which the block lacks, F-5's with it", footerEdit(func(d []byte) ([]byte, error) {
			f3, err := F3List(d, 0)
			if err != nil {
				return nil, err
			}
			if err := setNested(d, f3, f3BoundaryTag, 0, boundarySeqTag, 0, 2); err != nil {
				return nil, err
			}
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}

			return d, setNested(d, f5, f5BoundaryTag, 0, boundarySeqTag, 0, 2)
		})},
		{"footer-invalid-overflow", "block 1's epoch record counts summing to 2^64 + 4, which wraps to its record_count 4", footerEdit(func(d []byte) ([]byte, error) {
			f3, err := F3List(d, 1)
			if err != nil {
				return nil, err
			}
			f5, err := F5List(d)
			if err != nil {
				return nil, err
			}
			// Epoch 1 holds 2 of block 1's records and 3 of block 0's, epochs 2 and 3 one each;
			// 6 + 2 × (2^63 − 1) = 2^64 + 4.
			// F-5 sums the blocks per epoch without passing 2^63 − 1.
			edits := []struct {
				list     Section
				tag      uint16
				k        int
				old, new uint64
			}{
				{f3, f3EpochTag, 0, 2, 6}, {f3, f3EpochTag, 1, 1, maxU64}, {f3, f3EpochTag, 2, 1, maxU64},
				{f5, f5EpochTag, 0, 5, 9}, {f5, f5EpochTag, 1, 1, maxU64}, {f5, f5EpochTag, 2, 1, maxU64},
			}
			for _, e := range edits {
				if err := setNested(d, e.list, e.tag, e.k, epochRecordCountTag, e.old, e.new); err != nil {
					return nil, err
				}
			}

			return d, nil
		})},
	}
}

// trailerClauses are the clauses that tie the trailer's record_count and last_seq to the footer.
func trailerClauses() []footerClause {
	return []footerClause{
		{"footer-invalid-trailer-record-count", "the trailer's record_count one above the records", func(pack []byte) ([]byte, error) {
			var err error
			out, perr := PatchTrailer(pack, func(tr []byte) { err = setLE64(tr, trailerRecordCountOff, 7, 8) })
			if perr != nil {
				return nil, perr
			}

			return out, err
		}},
		{"footer-invalid-trailer-last-seq", "the trailer's last_seq one above the last block's", func(pack []byte) ([]byte, error) {
			var err error
			out, perr := PatchTrailer(pack, func(tr []byte) { err = setLE64(tr, trailerLastSeqOff, 7, 8) })
			if perr != nil {
				return nil, perr
			}

			return out, err
		}},
	}
}

// invalidFooterExpect is the expectation of footerBasePack with an invalid footer:
// the forward walk validates both blocks and agrees with the trailer's totals,
// the invalid footer alone makes the pack finalized-inconsistent, and its statistics are unavailable.
func invalidFooterExpect() *Expectation {
	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedInconsistent, Blocks: 2, Seqs: footerBaseSeqs, PrefixEnd: AtEnd(), Stats: &StatsWant{},
	}
}

// footerBaseStats are the statistics of footerBasePack's valid footer:
// epoch 1 ended by the socket-close at seq 5, epochs 2 and 3 not ended, and the start boundary at seq 0.
func footerBaseStats() *StatsWant {
	return &StatsWant{
		Available:  true,
		Epochs:     []EpochWant{{Epoch: 1, CloseSeq: new(uint64(5))}, {Epoch: 2}, {Epoch: 3}},
		Boundaries: []BoundaryWant{{Seq: 0, Kind: tracepack.BoundaryKindStart}},
	}
}

// footerBasePack writes the pack the footer-invalid vectors edit, in two blocks:
// block 0 a start boundary (seq 0) and data (seqs 1 and 3) of epoch 1, its seqs not contiguous, so its F-3 summary has seq_range entries;
// block 1 data (seq 4) and a socket-close (seq 5) of epoch 1, then data of epochs 2 (seq 6) and 3 (seq 7).
func footerBasePack(seed string) ([]byte, error) {
	start, err := newBoundary(0, tracepack.BoundaryKindStart)
	if err != nil {
		return nil, err
	}
	closed, err := newSocketEvent(5, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}

	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
		{start, footerData(1, 1), footerData(3, 1)},
		{footerData(4, 1), closed, footerData(6, 2), footerData(7, 3)},
	}})
}

// footerBaseEdit returns the Build of a vector that is footerBasePack after edit.
func footerBaseEdit(edit func(pack []byte) ([]byte, error)) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		pack, err := footerBasePack(seed)
		if err != nil {
			return nil, err
		}
		if pack, err = edit(pack); err != nil {
			return nil, err
		}

		return &Built{Pack: pack}, nil
	}
}

// withQueries returns build with queries added to what it builds.
func withQueries(build func(seed string) (*Built, error), queries ...QuerySpec) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		b, err := build(seed)
		if err != nil {
			return nil, err
		}
		b.Queries = append(b.Queries, queries...)

		return b, nil
	}
}

// footerEdit returns the pack edit that changes the decoded footer by edit (see PatchFooter).
func footerEdit(edit func(d []byte) ([]byte, error)) func(pack []byte) ([]byte, error) {
	return func(pack []byte) ([]byte, error) {
		return PatchFooter(pack, edit)
	}
}

// closeTwiceOneBlock returns footer-close-twice-one-block: an epoch ended by a socket-close and then a clean stop in one block.
func closeTwiceOneBlock() Recipe {
	return Recipe{
		ID: "footer-close-twice-one-block", Title: "an epoch ended by a socket-close, then a clean stop, in one block",
		Cites: []string{"FMT §10", "FMT §16"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			pack, err := closeOncePack(seed)

			return &Built{Pack: pack, Footer: true}, err
		},
		// The block's F-3 entry and F-5 state the lower seq, the socket-close's.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
			Stats:  &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1, CloseSeq: new(uint64(2))}}, Boundaries: closingStopStats()},
			Footer: &Footer{Stored: new(closeOnceProjection()), Accepted: true, Recomputed: closeOnceProjection()},
		},
	}
}

// closeOncePack writes one block of epoch 1: a socket-connect (seq 0), data (seq 1), a socket-close (seq 2) and a clean stop (seq 3).
func closeOncePack(seed string) ([]byte, error) {
	recs, err := closingRecords()
	if err != nil {
		return nil, err
	}

	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{recs}})
}

// closeOnceProjection is the footer projection of closeOncePack's records.
func closeOnceProjection() FooterProjection {
	return FooterProjection{
		Blocks: []FooterBlock{{Block: 0, CloseSeqs: []CloseSeq{{Epoch: 1, Seq: 2}}, Boundaries: closingStop()}},
		F5:     FooterF5{Epochs: []F5Epoch{{Epoch: 1, CloseSeq: new(U64(2))}}, Boundaries: closingStop()},
	}
}

// closeTwiceTwoBlocks returns footer-close-twice-two-blocks: an epoch ended by a socket-close in one block and a clean stop in the next.
func closeTwiceTwoBlocks() Recipe {
	return Recipe{
		ID: "footer-close-twice-two-blocks", Title: "an epoch ended by a socket-close in block 0, then a clean stop in block 1",
		Cites: []string{"FMT §10", "FMT §16"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			pack, err := closeAcrossPack(seed)

			return &Built{Pack: pack, Footer: true}, err
		},
		// Each block's F-3 entry states its own record's seq, F-5 the lower one.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 2, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
			Stats:  &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1, CloseSeq: new(uint64(2))}}, Boundaries: closingStopStats()},
			Footer: &Footer{Stored: new(closeAcrossProjection()), Accepted: true, Recomputed: closeAcrossProjection()},
		},
	}
}

// closeAcrossPack writes the records of closeOncePack in two blocks: seqs 0 to 2, then the clean stop at seq 3.
func closeAcrossPack(seed string) ([]byte, error) {
	recs, err := closingRecords()
	if err != nil {
		return nil, err
	}

	return writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{recs[:3], recs[3:]}})
}

// closeAcrossProjection is the footer projection of closeAcrossPack's records.
func closeAcrossProjection() FooterProjection {
	return FooterProjection{
		Blocks: []FooterBlock{
			{Block: 0, CloseSeqs: []CloseSeq{{Epoch: 1, Seq: 2}}},
			{Block: 1, CloseSeqs: []CloseSeq{{Epoch: 1, Seq: 3}}, Boundaries: closingStop()},
		},
		F5: FooterF5{Epochs: []F5Epoch{{Epoch: 1, CloseSeq: new(U64(2))}}, Boundaries: closingStop()},
	}
}

// closingRecords returns the records of epoch 1 that end it twice: a socket-connect (seq 0), data (seq 1),
// a socket-close (seq 2) and a clean stop (seq 3).
func closingRecords() ([]tracepack.Record, error) {
	connect, err := newSocketEvent(0, 1, tracepack.EventSocketConnect)
	if err != nil {
		return nil, err
	}
	closed, err := newSocketEvent(2, 1, tracepack.EventSocketClose)
	if err != nil {
		return nil, err
	}
	stop, err := newBoundary(3, tracepack.BoundaryKindStop)
	if err != nil {
		return nil, err
	}

	return []tracepack.Record{connect, footerData(1, 1), closed, stop}, nil
}

// closingStop is the projection of closingRecords' boundary, the clean stop at seq 3.
func closingStop() []Boundary {
	return []Boundary{{Seq: 3, Kind: "stop"}}
}

// closingStopStats is the statistics' boundaries of closingRecords: the clean stop at seq 3.
func closingStopStats() []BoundaryWant {
	return []BoundaryWant{{Seq: 3, Kind: tracepack.BoundaryKindStop}}
}

// invalidBodyBuild returns the Build of a pack of one block of epoch 1:
// a socket-connect (seq 0), data (seq 1), a transport-event record carrying payload (seq 2) and data (seq 3).
// The Writer stores the payload as given and leaves it out of the footer's summaries.
func invalidBodyBuild(payload []byte) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		connect, err := newSocketEvent(0, 1, tracepack.EventSocketConnect)
		if err != nil {
			return nil, err
		}
		bad := tracepack.Record{
			Seq: 2, TSUTCNs: msAt(2), Epoch: 1, Kind: tracepack.KindTransportEvent, Dir: tracepack.DirLocal,
			Fidelity: tracepack.FidelityNotApplicable, DecodeStatus: tracepack.DecodeStatusNotApplicable, Payload: payload,
		}
		pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), blocks: [][]tracepack.Record{
			{connect, footerData(1, 1), bad, footerData(3, 1)},
		}})

		return &Built{Pack: pack, Footer: true}, err
	}
}

// invalidBodyExpect is the expectation of an invalidBodyBuild pack:
// the record at seq 2 is not a capture-boundary record and ends no epoch, so it adds no boundary and no close_seq,
// and verify reports its payload as a writer defect, which leaves the outcome alone.
func invalidBodyExpect() *Expectation {
	p := FooterProjection{Blocks: []FooterBlock{{Block: 0}}, F5: FooterF5{Epochs: []F5Epoch{{Epoch: 1}}}}

	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 3), PrefixEnd: AtEnd(),
		WriterDefects: []DefectWant{{Kind: tracepack.WriterDefectEventPayload, Seq: 2}},
		Stats:         &StatsWant{Available: true, Epochs: []EpochWant{{Epoch: 1}}},
		Footer:        &Footer{Stored: &p, Accepted: true, Recomputed: p},
	}
}

// footerData returns a data record of seq and epoch: an S1F1 W message with SessionID 1 and System Bytes seq.
func footerData(seq uint64, epoch uint32) tracepack.Record {
	return newData(seq, epoch, tracepack.DirHostToEquipment, dataFrame(1, 1, true, uint32(seq), nil))
}

// newBoundary returns a capture-boundary record of seq, of epoch 1, of kind k.
func newBoundary(seq uint64, k tracepack.BoundaryKind) (tracepack.Record, error) {
	return newEvent(seq, 1, &tracepack.TransportEvent{Event: tracepack.EventCaptureBoundary, BoundaryKind: new(k)})
}

// orFooterQuality returns pack with bits set in block i's F-3 quality_union and in F-5's.
func orFooterQuality(pack []byte, i int, bits uint64) ([]byte, error) {
	return PatchFooter(pack, func(d []byte) ([]byte, error) {
		f3, err := F3List(d, i)
		if err != nil {
			return nil, err
		}
		f5, err := F5List(d)
		if err != nil {
			return nil, err
		}
		for _, s := range []struct {
			list Section
			tag  uint16
		}{{f3, f3QualityUnionTag}, {f5, f5QualityUnionTag}} {
			at, err := EntryOffset(d, s.list, s.tag, 0)
			if err != nil {
				return nil, err
			}
			v := d[at+tlv.HeaderLen:]
			binary.LittleEndian.PutUint64(v, binary.LittleEndian.Uint64(v)|bits)
		}

		return d, nil
	})
}

// setLE64 sets the little-endian u64 at off in b from old to v, and fails when b does not hold old there.
func setLE64(b []byte, off int, old, v uint64) error {
	if off < 0 || off+8 > len(b) {
		return fmt.Errorf("%w: no u64 at %d of %d bytes", ErrPackEdit, off, len(b))
	}
	if got := binary.LittleEndian.Uint64(b[off:]); got != old {
		return fmt.Errorf("%w: the u64 at %d is %d, not %d", ErrPackEdit, off, got, old)
	}
	binary.LittleEndian.PutUint64(b[off:], v)

	return nil
}

// setLE32 sets the little-endian u32 at off in b from old to v, and fails when b does not hold old there.
func setLE32(b []byte, off int, old, v uint32) error {
	if off < 0 || off+4 > len(b) {
		return fmt.Errorf("%w: no u32 at %d of %d bytes", ErrPackEdit, off, len(b))
	}
	if got := binary.LittleEndian.Uint32(b[off:]); got != old {
		return fmt.Errorf("%w: the u32 at %d is %d, not %d", ErrPackEdit, off, got, old)
	}
	binary.LittleEndian.PutUint32(b[off:], v)

	return nil
}

// setNested sets, from old to v, the u64 value of the nested tag inner in the k-th entry of tag in the entry list s of b.
func setNested(b []byte, s Section, tag uint16, k int, inner uint16, old, v uint64) error {
	at, err := NestedValueOffset(b, s, tag, k, inner)
	if err != nil {
		return err
	}

	return setLE64(b, at, old, v)
}

// setSeqRange sets, from old to v, the value of the k-th seq_range entry in the entry list s of b, an F-3 list or F-5:
// its first and last seqs.
func setSeqRange(b []byte, s Section, k int, old, v [2]uint64) error {
	at, err := EntryOffset(b, s, seqRangeTag, k)
	if err != nil {
		return err
	}
	if err := setLE64(b, at+tlv.HeaderLen, old[0], v[0]); err != nil {
		return err
	}

	return setLE64(b, at+tlv.HeaderLen+8, old[1], v[1])
}

// setCount sets, from old to v, element idx of the count array of tag in the entry list s of b, of elements width bytes wide (4 or 8).
func setCount(b []byte, s Section, tag uint16, width, idx int, old, v uint64) error {
	at, err := EntryOffset(b, s, tag, 0)
	if err != nil {
		return err
	}
	if n := int(binary.LittleEndian.Uint32(b[at+4:])); (idx+1)*width > n {
		return fmt.Errorf("%w: a count array of %d bytes has no element %d", ErrPackEdit, n, idx)
	}
	off := at + tlv.HeaderLen + idx*width
	if width == 4 {
		return setLE32(b, off, uint32(old), uint32(v))
	}

	return setLE64(b, off, old, v)
}

// checked returns the pack edit that is edit followed by check of its result:
// a recipe checks this way, as it builds, that its pack holds its subject.
func checked(edit func(pack []byte) ([]byte, error), check func(pack []byte) error) func(pack []byte) ([]byte, error) {
	return func(pack []byte) ([]byte, error) {
		out, err := edit(pack)
		if err != nil {
			return nil, err
		}
		if err := check(out); err != nil {
			return nil, err
		}

		return out, nil
	}
}

// checkShortEntries checks that pack's footer is what footer-invalid-f2-entry-len is about:
// read at its 79-byte stride, each F-2 entry's offset is the envelope Locate finds for its block,
// so the entries are whole at that stride and only their length breaks the clause.
func checkShortEntries(pack []byte) error {
	l, err := Locate(pack)
	if err != nil {
		return err
	}
	d, err := DecodedFooter(pack)
	if err != nil {
		return err
	}
	stride := uint64(binary.LittleEndian.Uint32(d[prologueF2EntryLenOff:]))
	f2 := binary.LittleEndian.Uint64(d[prologueF2OffsetOff:])
	for i, s := range l.Blocks {
		at := f2 + uint64(i)*stride
		if stride != format.F2EntryLen-1 || at+stride > uint64(len(d)) || binary.LittleEndian.Uint64(d[at+f2OffsetOff:]) != s.Offset {
			return fmt.Errorf("%w: F-2 entry %d at stride %d does not locate block %d", ErrPackEdit, i, stride, i)
		}
	}

	return nil
}

// checkListPastF3 checks that pack's footer is what footer-invalid-summary-bounds is about:
// block 1's F-3 list, read at its summary_offset and summary_len, is a valid entry list inside the footer
// that ends past F-3, so only its bound breaks the clause.
func checkListPastF3(pack []byte) error {
	d, err := DecodedFooter(pack)
	if err != nil {
		return err
	}
	list, err := F3List(d, 1)
	if err != nil {
		return err
	}
	if _, err := tlv.Decode(d[list.Off : list.Off+list.Len]); err != nil {
		return fmt.Errorf("%w: block 1's F-3 list: %w", ErrPackEdit, err)
	}
	f3End := binary.LittleEndian.Uint64(d[prologueF3OffsetOff:]) + binary.LittleEndian.Uint64(d[prologueF3LenOff:])
	if uint64(list.Off+list.Len) <= f3End {
		return fmt.Errorf("%w: block 1's F-3 list ends within F-3", ErrPackEdit)
	}

	return nil
}

// checkHiddenGap checks that pack's footer is what footer-seq-range-hides-gap is about:
// F-5's seq_range entries are the union of the ranges F-2 states, first_seq to last_seq, adjacent ranges joined,
// so F-5 does not reveal the seq block 0 misses.
func checkHiddenGap(pack []byte) error {
	d, err := DecodedFooter(pack)
	if err != nil {
		return err
	}
	var union []tlv.SeqRange
	for i := range int(binary.LittleEndian.Uint32(d[prologueBlockCountOff:])) {
		e, err := F2Entry(d, i)
		if err != nil {
			return err
		}
		r := tlv.SeqRange{First: binary.LittleEndian.Uint64(d[e.Off+f2FirstSeqOff:]), Last: binary.LittleEndian.Uint64(d[e.Off+f2LastSeqOff:])}
		if n := len(union); n > 0 && union[n-1].Last+1 == r.First {
			union[n-1].Last = r.Last
			continue
		}
		union = append(union, r)
	}
	f5, err := F5List(d)
	if err != nil {
		return err
	}
	entries, err := tlv.Decode(d[f5.Off : f5.Off+f5.Len])
	if err != nil {
		return fmt.Errorf("%w: F-5: %w", ErrPackEdit, err)
	}
	var stated []tlv.SeqRange
	for _, e := range entries {
		if e.Tag != seqRangeTag {
			continue
		}
		r, err := tlv.DecodeSeqRange(e.Value)
		if err != nil {
			return fmt.Errorf("%w: F-5 seq_range: %w", ErrPackEdit, err)
		}
		stated = append(stated, r)
	}
	if !slices.Equal(union, stated) {
		return fmt.Errorf("%w: F-5 seq_range %v, F-2 states %v", ErrPackEdit, stated, union)
	}

	return nil
}

// replaceSeqRanges returns the entry list list with its seq_range entries replaced by one entry of r,
// at the place of the first, of the first one's value type.
func replaceSeqRanges(list []byte, r tlv.SeqRange) ([]byte, error) {
	entries, err := tlv.Decode(list)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	var out []byte
	replaced := false
	for _, e := range entries {
		switch {
		case e.Tag != seqRangeTag:
			out = tlv.AppendEntry(out, e)
		case !replaced:
			out = tlv.AppendEntry(out, tlv.Entry{Tag: seqRangeTag, Type: e.Type, Value: tlv.AppendSeqRange(nil, r)})
			replaced = true
		default:
			// A later seq_range entry is dropped.
		}
	}
	if !replaced {
		return nil, fmt.Errorf("%w: the list holds no seq_range entry", ErrPackEdit)
	}

	return out, nil
}
