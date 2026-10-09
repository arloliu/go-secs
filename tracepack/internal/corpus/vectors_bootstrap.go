package corpus

import (
	"encoding/binary"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// Positions and tags the bootstrap group edits (the tracepack format specification §4 and §5).
const (
	headerFormatMajorOff = 8

	metaToolIDTag             uint16 = 0x0002
	metaTransportTag          uint16 = 0x0003
	metaClassifierTag         uint16 = 0x0008
	metaCaptureOriginUTCNsTag uint16 = 0x000A
	metaCoverageTag           uint16 = 0x0016
	metaDeviceIDTag           uint16 = 0x0025
	metaReplacementSetSizeTag uint16 = 0x002A
	metaReplacementSetIdxTag  uint16 = 0x002B

	// privateClassifierTag is the private tag bootstrap-record-conditioned-tag gives its classifier entry,
	// so that the pack has no classifier.
	privateClassifierTag uint16 = 0x8000 | metaClassifierTag
	// overLimitU64 is a u64 value one above the format's limit (the tracepack format specification §2).
	overLimitU64 = uint64(maxU64) + 1
)

// bootstrapVectors returns the recipes of the bootstrap group (the tracepack corpus specification §9.4):
// one rejection vector per cause, each an edit of an empty pack with that cause alone,
// and a pack that opens although its records require a tag it lacks.
func bootstrapVectors() []Recipe {
	damaged, nonconforming := []string{LabelDamaged}, []string{LabelNonconformingWriter}
	reject := func(id, title, code string, labels []string, cites []string, build func(seed string) ([]byte, error)) Recipe {
		return Recipe{
			ID: id, Title: title, Cites: append(cites, "FMT §13", "CORPUS §5.3"), Class: ClassRejection, Labels: labels,
			Build: func(seed string) (*Built, error) {
				pack, err := build(seed)

				return &Built{Pack: pack}, err
			},
			Expect: &Expectation{Rejection: code},
		}
	}
	metaEdit := func(edit func(meta []byte) ([]byte, error)) func(seed string) ([]byte, error) {
		return emptyEdit(func(pack []byte) ([]byte, error) { return editMeta(pack, edit) })
	}
	appendEntry := func(e tlv.Entry) func(seed string) ([]byte, error) {
		return metaEdit(func(meta []byte) ([]byte, error) { return tlv.AppendEntry(meta, e), nil })
	}
	setU64 := func(tag uint16, v uint64) func(meta []byte) ([]byte, error) {
		return func(meta []byte) ([]byte, error) { return meta, SetU64(meta, Section{Len: len(meta)}, tag, v) }
	}

	return []Recipe{
		reject("bootstrap-short-object", "a file header cut before its last byte", RejectShortObject, damaged, []string{"FMT §4"},
			emptyEdit(func(pack []byte) ([]byte, error) { return pack[:format.FileHeaderLen-1], nil })),
		reject("bootstrap-bad-magic", "a file header whose magic is not tracepack's, its CRC valid", RejectBadMagic, nonconforming, []string{"FMT §4"},
			emptyEdit(func(pack []byte) ([]byte, error) { return PatchHeader(pack, func(h []byte) { h[1] = 'X' }) })),
		reject("bootstrap-header-crc", "a file header whose stored header_crc is wrong", RejectHeaderCRC, damaged, []string{"FMT §4"},
			emptyEdit(func(pack []byte) ([]byte, error) { return FlipByte(pack, fileHeaderCRCOff) })),
		reject("bootstrap-unsupported-version", "a file header of format_major 2, its CRC valid", RejectUnsupportedVersion, nil, []string{"FMT §4", "FMT §14"},
			emptyEdit(func(pack []byte) ([]byte, error) {
				return PatchHeader(pack, func(h []byte) { binary.LittleEndian.PutUint16(h[headerFormatMajorOff:], 2) })
			})),
		reject("bootstrap-metadata-past-object", "a pack cut before the last byte of its pack metadata", RejectMetadataPastObject, damaged, []string{"FMT §4"},
			emptyEdit(func(pack []byte) ([]byte, error) { return pack[:len(pack)-1], nil })),
		reject("bootstrap-metadata-crc", "a file header whose stored pack_metadata_crc is wrong, its header_crc valid", RejectMetadataCRC, damaged, []string{"FMT §4"},
			emptyEdit(func(pack []byte) ([]byte, error) {
				return PatchHeader(pack, func(h []byte) { h[headerPackMetadataCRCOff] ^= 0xFF })
			})),
		reject("bootstrap-entry-framing", "pack metadata ending with an entry whose value runs past the list", RejectMetadataEntryList, nonconforming, []string{"FMT §5"},
			// An unknown tag of value type bytes declaring 4 bytes, of which 2 follow.
			metaEdit(func(meta []byte) ([]byte, error) {
				return append(meta, 0x00, 0x01, byte(tlv.TypeBytes), 0x00, 0x04, 0x00, 0x00, 0x00, 0x01, 0x02), nil
			})),
		reject("bootstrap-entry-type", "pack metadata whose transport entry is of value type utf8", RejectMetadataEntryList, nonconforming, []string{"FMT §5"},
			metaEdit(func(meta []byte) ([]byte, error) {
				at, err := EntryOffset(meta, Section{Len: len(meta)}, metaTransportTag, 0)
				if err != nil {
					return nil, err
				}
				meta[at+2] = byte(tlv.TypeUTF8)

				return meta, nil
			})),
		reject("bootstrap-entry-value", "pack metadata whose device_id is 2^63", RejectMetadataEntryList, nonconforming, []string{"FMT §5", "FMT §2", "CORPUS §8"},
			appendEntry(tlv.U64Entry(metaDeviceIDTag, overLimitU64))),
		reject("bootstrap-entry-repetition", "pack metadata holding tool_id twice", RejectMetadataEntryList, nonconforming, []string{"FMT §5"},
			appendEntry(tlv.UTF8Entry(metaToolIDTag, corpusToolID))),
		reject("bootstrap-entry-nested", "pack metadata holding a coverage entry whose nested list breaks framing", RejectMetadataEntryList, nonconforming, []string{"FMT §5"},
			// The nested list is three bytes, shorter than an entry header.
			appendEntry(tlv.Entry{Tag: metaCoverageTag, Type: tlv.TypeTLV, Value: []byte{0x02, 0x00, 0x04}})),
		reject("bootstrap-required-tag", "pack metadata without tool_id", RejectMetadataRequired, nonconforming, []string{"FMT §5"},
			metaEdit(func(meta []byte) ([]byte, error) { return DropEntries(meta, metaToolIDTag) })),
		reject("bootstrap-conditional-tag", "a capture-clock pack without capture_origin_utc_ns", RejectMetadataRequired, nonconforming, []string{"FMT §5", "SEM §4"},
			func(seed string) ([]byte, error) {
				meta := segmentMeta(seed)
				meta.TimeSource = tracepack.TimeSourceCaptureClock
				meta.CaptureOriginUTCNs, meta.CaptureOriginMonoNs, meta.ClockStepToleranceNs = new(TimeBase), new(int64(0)), new(uint64(1_000_000))
				pack, err := writeSpec(&packSpec{seed: seed, meta: meta, open: true})
				if err != nil {
					return nil, err
				}

				return editMeta(pack, func(m []byte) ([]byte, error) { return DropEntries(m, metaCaptureOriginUTCNsTag) })
			}),
		reject("bootstrap-replacement-set-size", "a generation whose replacement_set_size is 2", RejectReplacementSet, nonconforming, []string{"FMT §5", "STO §6"},
			generationEdit(setU64(metaReplacementSetSizeTag, 2))),
		reject("bootstrap-replacement-set-index", "a generation whose replacement_set_index is 1", RejectReplacementSet, nonconforming, []string{"FMT §5", "STO §6"},
			generationEdit(setU64(metaReplacementSetIdxTag, 1))),
		{
			ID: "bootstrap-record-conditioned-tag", Title: "a classified record in a pack without classifier",
			Cites: []string{"FMT §13", "FMT §5", "FMT I-11"}, Class: ClassRead, Labels: nonconforming,
			Build: func(seed string) (*Built, error) {
				meta := segmentMeta(seed)
				meta.Classifiers = []string{corpusClassifier}
				rec := footerData(0, 1)
				rec.DecodeStatus = tracepack.DecodeStatusOK
				pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{{rec, footerData(1, 1)}}})
				if err != nil {
					return nil, err
				}
				// The classifier entry becomes a private tag of the same length, so nothing after the metadata moves.
				pack, err = editMeta(pack, func(m []byte) ([]byte, error) {
					at, err := EntryOffset(m, Section{Len: len(m)}, metaClassifierTag, 0)
					if err != nil {
						return nil, err
					}
					binary.LittleEndian.PutUint16(m[at:], privateClassifierTag)

					return m, nil
				})

				return &Built{Pack: pack}, err
			},
			// The requirement depends on the records, so it binds the writer and never rejects the pack;
			// verify reports no writer defect for it, and the footer is the Writer's.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
				ExportHas: []string{`"decode_status":"ok"`, `{"tag":32776,"value_type":6,"value":"dHJhY2VwYWNrLXNlbTM="}`},
			},
		},
	}
}

// emptyEdit returns the build of a pack that is the empty pack of the seed, a file header and pack metadata, after edit.
func emptyEdit(edit func(pack []byte) ([]byte, error)) func(seed string) ([]byte, error) {
	return func(seed string) ([]byte, error) {
		pack, err := writeSpec(&packSpec{seed: seed, meta: segmentMeta(seed), open: true})
		if err != nil {
			return nil, err
		}

		return edit(pack)
	}
}

// generationEdit returns the build of a generation 1 archive without blocks whose pack metadata edit changes.
func generationEdit(edit func(meta []byte) ([]byte, error)) func(seed string) ([]byte, error) {
	return func(seed string) ([]byte, error) {
		meta := segmentMeta(seed)
		meta.PackRole, meta.PeriodEnd = tracepack.PackRoleArchive, TimeBase+3_600_000_000_000
		meta.ScopeGeneration, meta.PublisherEpoch, meta.CompactionLevel = new(uint64(1)), new(uint64(1)), 1
		meta.ReplacementSetID = new(IDFor(roleReplacementSetID, seed))
		meta.ReplacementSetSize, meta.ReplacementSetIndex = new(uint64(1)), new(uint64(0))
		pack, err := writeSpec(&packSpec{seed: seed, meta: meta, open: true})
		if err != nil {
			return nil, err
		}

		return editMeta(pack, edit)
	}
}

// editMeta returns pack with its pack metadata changed by edit, which gets a copy of it (see PatchMeta).
func editMeta(pack []byte, edit func(meta []byte) ([]byte, error)) ([]byte, error) {
	var editErr error
	out, err := PatchMeta(pack, func(meta []byte) []byte {
		m, err := edit(slices.Clone(meta))
		if err != nil {
			editErr = err
			return meta
		}

		return m
	})
	if err != nil {
		return nil, err
	}
	if editErr != nil {
		return nil, editErr
	}

	return out, nil
}
