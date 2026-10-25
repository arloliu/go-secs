package corpus

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// Positions and tags the version-aware vectors edit (the tracepack format specification §4, §5 and §14).
const (
	headerFormatMinorOff = 10

	metaRedactionPolicyTag uint16 = 0x0031
	metaRedactionTag       uint16 = 0x0032
	// metaThinningTag is thinning from format 1.1; in a pack of format_minor 0 the number is unknown.
	metaThinningTag uint16 = 0x0033
	// metaRecorderInstanceNameTag is recorder_instance_name from format 1.2; in a pack of an earlier minor the number is unknown.
	metaRecorderInstanceNameTag uint16 = 0x0034
	// standInRecorderInstanceNameTag is the private tag a Writer writes in place of metaRecorderInstanceNameTag,
	// which the newest registry knows and so refuses in PackMeta.Unknown; surgery then renumbers it.
	standInRecorderInstanceNameTag uint16 = 0x8000 | metaRecorderInstanceNameTag
	// unassignedMetaTag is a top-level pack metadata tag that no format minor up to 1.2 assigns.
	unassignedMetaTag uint16 = 0x0035
	// standInThinningTag is the private tag a Writer writes in place of metaThinningTag,
	// which the newest registry knows and so refuses in PackMeta.Unknown; surgery then renumbers it.
	standInThinningTag uint16 = 0x8000 | metaThinningTag

	// Nested tags of format 1.1 that a pack of format_minor 0 holds as unknowns.
	policyEncryptionAlgorithmTag uint16 = 0x0005
	policyEncryptionKeyIDTag     uint16 = 0x0006
	policyIDTag                  uint16 = 0x0001
	redactionMaskedRangesTag     uint16 = 0x0003
	redactionDomainTag           uint16 = 0x0004
	redactionDigestTag           uint16 = 0x0005
	redactionSIVTag              uint16 = 0x0006

	// qualityBit7 is quality bit 7: unobserved from format 1.1, reserved in a pack of format_minor 0.
	qualityBit7 = uint64(1) << 7
)

// Values of the version-aware extracts.
const (
	// corpusDeviceName is the recorder_instance_name of the format 1.2 vectors.
	corpusDeviceName = "eqp"
	extractPolicyID  = "policy-extract"
	extractDomain    = "recipe"
	extractFilter    = "the records of corpus-tool, seqs 0 to 2"
	// extractPadding pads a string the surgery later shortens, so the pack metadata keeps its length.
	extractPadding = "_"
	// secondClassifier and the max_frame_len values are those an extract inherits from two sources classified apart.
	secondClassifier = "tracepack-sem3-previous"
)

// Values the format 1.0 packs hold as unknown entries in place of format 1.1's.
var (
	newTagThinning1 = []byte("thinning-1")
	newTagThinning2 = []byte("thinning-2")
	newTagAlgorithm = []byte{0x01}
	newTagKeyID     = []byte(EncryptionKeyID1)
	newTagSIV1      = []byte("0123456789abcdef")
	newTagSIV2      = []byte("fedcba9876543210")
)

// versionVectors returns the version-aware vectors of the basic group (the tracepack corpus specification §9.1 and §9.4):
// format 1.0 packs holding the numbers format 1.1 assigns, format 1.0 extracts under a policy, and a pack of minor 2.
func versionVectors() []Recipe {
	return []Recipe{
		{
			ID: "basic-new-tags-segment-1-0", Title: "a format 1.0 segment holding tag 0x0033 twice and a record carrying quality bit 7",
			Cites: []string{"FMT §5", "FMT §14", "FMT §9", "JSONL §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: newTagsSegment,
			// In a pack of format_minor 0 the numbers keep their format 1.0 status: an unknown tag and a reserved bit.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0`,
					`"unknown":[{"tag":51,"value_type":7,"value":"dGhpbm5pbmctMQ=="},{"tag":51,"value_type":7,"value":"dGhpbm5pbmctMg=="}]`,
					`"seq":"1","ts_utc_ns":"1791000000001000000","mono_ns":"0","epoch":1,"payload_len":14,"trailing_bytes":0,"quality":["bit(7)"]`,
				},
			},
		},
		{
			ID:    "basic-new-tags-extract-1-0",
			Title: "a format 1.0 extract whose redaction_policy holds nested 0x0005 and 0x0006 and whose redaction entry holds 0x0006 twice",
			Cites: []string{"FMT §5", "FMT §14", "SEM §8", "JSONL §4"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: newTagsExtract,
			// Each nested number is unknown in its registry of format 1.0, an opaque value kept in stored order.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0,"flags":["redaction-present"]`,
					`"unknown":[{"tag":5,"value_type":7,"value":"AQ=="},{"tag":6,"value_type":7,"value":"dGVzdC1lbmMtMQ=="}]`,
					`"unknown":[{"tag":6,"value_type":7,"value":"MDEyMzQ1Njc4OWFiY2RlZg=="},{"tag":6,"value_type":7,"value":"ZmVkY2JhOTg3NjU0MzIxMA=="}]`,
					`"quality":["redacted"]`,
				},
			},
		},
		{
			ID: "basic-extract-policy-no-classifier-1-0", Title: "a format 1.0 extract under a policy that matched nothing, without classifier",
			Cites: []string{"FMT §5", "SEM §8", "FMT §14"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				return writeExtract(seed, nil, nil)
			},
			// The policy is written though nothing matched; no record is classified, so no classifier is required.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0,"flags":[]`,
					`"redaction_policy":{"policy_id":"policy-extract","policy_version":"1","key_id":"test-digest-1","digest_algorithm":"hmac-sha256"}`,
					`"decode_status":"not-attempted"`,
				},
			},
		},
		{
			ID:    "basic-extract-policy-multi-classifier-1-0",
			Title: "a format 1.0 extract under a policy with two classifier and two max_frame_len values inherited from its sources",
			Cites: []string{"FMT §5", "SEM §8", "FMT §14"}, Class: ClassRead,
			Build: func(seed string) (*Built, error) {
				return writeExtract(seed, []string{corpusClassifier, secondClassifier}, []uint64{semMaxFrameLen, 2 * semMaxFrameLen})
			},
			// An extract keeps the format 1.0 rule: several classifier and max_frame_len values are allowed.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0`,
					`"classifier":["tracepack-sem3","tracepack-sem3-previous"]`,
					`"max_frame_len":["40","80"]`,
					`"redaction_policy":{"policy_id":"policy-extract"`,
					`"decode_status":"ok"`,
				},
			},
		},
		{
			ID: "basic-new-tags-segment-1-1", Title: "a format 1.1 segment holding tag 0x0034 as utf8",
			Cites: []string{"FMT §5", "FMT §14", "JSONL §5"}, Class: ClassRead,
			Build: newTagsSegment11,
			// In a pack of format_minor 1 the number keeps its format 1.1 status: an unknown tag, exported under /2.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1`,
					`"unknown":[{"tag":52,"value_type":6,"value":"ZXFw"}]`,
				},
			},
		},
		{
			ID: "basic-recorder-instance-name", Title: "a format 1.2 segment carrying recorder_instance_name",
			Cites: []string{"FMT §5", "FMT §14", "JSONL §5"}, Class: ClassRead,
			Build: recorderInstanceNameSegment,
			// The tag is format 1.2's: the writer writes format_minor 2 and the export names it under /3.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/3","format_major":1,"format_minor":2`,
					`"recorder_instance_name":"eqp"`,
				},
			},
		},
		{
			ID: "basic-future-minor", Title: "a readable pack of format_minor 3 holding an unknown tag",
			Cites: []string{"FMT §14", "FMT §5", "JSONL §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
			Build: futureMinor,
			// A newer minor is read with the newest registry and exported under the newest schema, its unknown tag kept.
			Expect: &Expectation{
				Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
				ExportHas: []string{
					`"schema":"tracepack-jsonl/3","format_major":1,"format_minor":3`,
					`"unknown":[{"tag":53,"value_type":7,"value":"ZnV0dXJl"}]`,
				},
			},
		},
	}
}

// newTagsSegment builds basic-new-tags-segment-1-0:
// a segment of three data records in one block whose pack metadata holds tag 0x0033 twice, of value type bytes,
// and whose record of seq 1 stores quality bit 7.
// The Writer writes neither: it refuses a known tag among the unknown entries and a record carrying an unobserved bit
// in a pack without thinning, so the entries are written under a private tag and renumbered,
// and the bit is set in the record's stored quality and the footer's quality unions.
func newTagsSegment(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.Unknown = []tracepack.RawEntry{
		{Tag: standInThinningTag, Type: uint8(tlv.TypeBytes), Value: newTagThinning1},
		{Tag: standInThinningTag, Type: uint8(tlv.TypeBytes), Value: newTagThinning2},
	}
	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{
		{footerData(0, 1), footerData(1, 1), footerData(2, 1)},
	}})
	if err != nil {
		return nil, err
	}
	if pack, err = renumberMetaTag(pack, standInThinningTag, metaThinningTag); err != nil {
		return nil, err
	}
	if pack, err = setStoredQuality(pack, 0, 1, qualityBit7); err != nil {
		return nil, err
	}

	return &Built{Pack: pack}, nil
}

// newTagsExtract builds basic-new-tags-extract-1-0:
// an extract of format_minor 0 holding three data records in one block, of which seq 0, an S7F3, is masked by zeros
// and named by the one redaction entry;
// its redaction_policy holds nested 0x0005 and 0x0006 and its entry nested 0x0006 twice, each of value type bytes.
// The nested entries go in by surgery that keeps the pack metadata's length:
// the Writer writes a policy_id and a domain padded by the bytes the entries take, which the surgery gives back.
func newTagsExtract(seed string) (*Built, error) {
	policyExtra := 2*tlv.HeaderLen + len(newTagAlgorithm) + len(newTagKeyID)
	entryExtra := 2*tlv.HeaderLen + len(newTagSIV1) + len(newTagSIV2)

	meta := extractMeta(seed, extractPolicyID+strings.Repeat(extractPadding, policyExtra))
	masked, entry, err := zeroMaskedS7F3(0)
	if err != nil {
		return nil, err
	}
	entry.Domain += strings.Repeat(extractPadding, entryExtra)
	meta.Redaction = []tracepack.RedactionEntry{entry}
	pack, err := writeSpec(&packSpec{
		seed: seed, meta: meta, facts: tracepack.PackFacts{AnyRedacted: true, RedactionPolicyApplied: true},
		blocks: [][]tracepack.Record{{masked, footerData(1, 1), footerData(2, 1)}},
	})
	if err != nil {
		return nil, err
	}

	pack, err = editMeta(pack, func(m []byte) ([]byte, error) {
		return rewriteNestedEntries(m, map[uint16]nestedRewrite{
			metaRedactionPolicyTag: {pad: policyIDTag, keep: extractPolicyID, extra: []tlv.Entry{
				tlv.BytesEntry(policyEncryptionAlgorithmTag, newTagAlgorithm), tlv.BytesEntry(policyEncryptionKeyIDTag, newTagKeyID),
			}},
			metaRedactionTag: {pad: redactionDomainTag, keep: extractDomain, extra: []tlv.Entry{
				tlv.BytesEntry(redactionSIVTag, newTagSIV1), tlv.BytesEntry(redactionSIVTag, newTagSIV2),
			}},
		})
	})
	if err != nil {
		return nil, err
	}

	return &Built{Pack: pack}, nil
}

// writeExtract writes a format 1.0 extract of seed under extractMeta's policy, which matched nothing:
// three data records in one block, of which seq 1 is classified ok when classifiers are given,
// with classifiers and maxFrameLens as the extract's classifier and max_frame_len values.
func writeExtract(seed string, classifiers []string, maxFrameLens []uint64) (*Built, error) {
	meta := extractMeta(seed, extractPolicyID)
	meta.Classifiers, meta.MaxFrameLens = classifiers, maxFrameLens
	classified := footerData(1, 1)
	facts := tracepack.PackFacts{RedactionPolicyApplied: true}
	if len(classifiers) > 0 {
		classified.DecodeStatus, facts.AnyClassified = tracepack.DecodeStatusOK, true
	}
	pack, err := writeSpec(&packSpec{
		seed: seed, meta: meta, facts: facts, blocks: [][]tracepack.Record{{footerData(0, 1), classified, footerData(2, 1)}},
	})

	return &Built{Pack: pack}, err
}

// extractMeta returns the pack metadata of a format 1.0 extract of seed: segmentMeta's, of pack_role extract,
// without scope_generation, with an extract_filter, and a redaction_policy of policyID without encryption.
func extractMeta(seed, policyID string) *tracepack.PackMeta {
	meta := segmentMeta(seed)
	meta.PackRole, meta.ScopeGeneration, meta.ExtractFilter = tracepack.PackRoleExtract, nil, new(extractFilter)
	meta.RedactionPolicy = &tracepack.RedactionPolicy{
		PolicyID: policyID, PolicyVersion: 1, KeyID: DigestKeyID1, DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
	}

	return meta
}

// zeroMaskedS7F3 returns an S7F3 of seq, carrying ppbodyText, whose PPBODY content an extract writer masked by zeros,
// and its redaction entry: path 2, the content's range, the extract domain and the digest of the original PPBODY item
// under the first published digest key.
func zeroMaskedS7F3(seq uint64) (tracepack.Record, tracepack.RedactionEntry, error) {
	frame := dataFrame(7, 3, true, uint32(seq), ppbodyText())
	n := uint64(len(ppbodyContent()))
	clear(frame[ppbodyOffset : ppbodyOffset+n])
	r := newData(seq, 1, tracepack.DirHostToEquipment, frame)
	r.Quality |= tracepack.QualityRedacted

	digest, err := redact.Digest(PublishedKey(DigestKeyID1, digestKeySize), extractDomain, ppbodyItem())
	if err != nil {
		return tracepack.Record{}, tracepack.RedactionEntry{}, err
	}

	return r, tracepack.RedactionEntry{
		Seq: seq, ItemPath: new("2"), MaskedRanges: []tracepack.MaskedRange{{Offset: ppbodyOffset, Length: n}},
		Domain: extractDomain, Digest: digest,
	}, nil
}

// futureMinor builds basic-future-minor: a segment of two data records holding an unassigned top-level tag,
// whose file header then states format_minor 3, which no Writer of this module writes.
func futureMinor(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.Unknown = []tracepack.RawEntry{{Tag: unassignedMetaTag, Type: uint8(tlv.TypeBytes), Value: []byte("future")}}
	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{{footerData(0, 1), footerData(1, 1)}}})
	if err != nil {
		return nil, err
	}
	pack, err = PatchHeader(pack, func(h []byte) { binary.LittleEndian.PutUint16(h[headerFormatMinorOff:], 3) })

	return &Built{Pack: pack}, err
}

// newTagsSegment11 builds basic-new-tags-segment-1-1:
// a segment of format 1.1, by its thinning entry, of two data records whose pack metadata holds tag 0x0034 as utf8.
// The Writer refuses a known tag among the unknown entries, so the entry is written under a private tag and renumbered.
func newTagsSegment11(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.Thinning = []tracepack.Thinning{{Stream: 1, Function: 3, Source: fdcPollSource}}
	meta.Unknown = []tracepack.RawEntry{{Tag: standInRecorderInstanceNameTag, Type: uint8(tlv.TypeUTF8), Value: []byte(corpusDeviceName)}}
	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{{footerData(0, 1), footerData(1, 1)}}})
	if err != nil {
		return nil, err
	}
	pack, err = renumberMetaTag(pack, standInRecorderInstanceNameTag, metaRecorderInstanceNameTag)

	return &Built{Pack: pack}, err
}

// recorderInstanceNameSegment builds basic-recorder-instance-name:
// a segment of two data records whose pack metadata carries recorder_instance_name, which makes it format 1.2.
func recorderInstanceNameSegment(seed string) (*Built, error) {
	meta := segmentMeta(seed)
	meta.RecorderInstanceName = new(corpusDeviceName)
	pack, err := writeSpec(&packSpec{seed: seed, meta: meta, blocks: [][]tracepack.Record{{footerData(0, 1), footerData(1, 1)}}})
	if err != nil {
		return nil, err
	}

	return &Built{Pack: pack}, nil
}

// renumberMetaTag returns pack with every top-level pack metadata entry of tag from renumbered to, its value kept.
func renumberMetaTag(pack []byte, from, to uint16) ([]byte, error) {
	return editMeta(pack, func(m []byte) ([]byte, error) {
		entries, err := tlv.Decode(m)
		if err != nil {
			return nil, err
		}
		found := false
		var out []byte
		for _, e := range entries {
			if e.Tag == from {
				e.Tag, found = to, true
			}
			out = tlv.AppendEntry(out, e)
		}
		if !found {
			return nil, fmt.Errorf("corpus: the pack metadata holds no entry of tag 0x%04X", from)
		}

		return out, nil
	})
}

// setStoredQuality returns pack with bits set in the stored quality of record k of block i,
// and in the block's F-3 quality_union and F-5's.
func setStoredQuality(pack []byte, i, k int, bits uint64) ([]byte, error) {
	pack, err := PatchBody(pack, i, func(b *Body) error {
		at := k*b.RecordHeaderLen + rowQualityOff
		if at >= len(b.Rows) {
			return fmt.Errorf("corpus: block %d holds no record %d", i, k)
		}
		b.Rows[at] |= byte(bits)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return orFooterQuality(pack, i, bits)
}

// nestedRewrite is the rewrite of a top-level entry's nested list:
// the utf8 value of tag pad becomes keep, and extra is appended.
type nestedRewrite struct {
	pad   uint16
	keep  string
	extra []tlv.Entry
}

// rewriteNestedEntries returns the pack metadata m with the nested list of each entry whose tag rewrites names rewritten,
// failing unless every rewritten value keeps its length, so the pack metadata does too.
func rewriteNestedEntries(m []byte, rewrites map[uint16]nestedRewrite) ([]byte, error) {
	entries, err := tlv.Decode(m)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, e := range entries {
		if rw, ok := rewrites[e.Tag]; ok {
			nested, err := tlv.Decode(e.Value)
			if err != nil {
				return nil, err
			}
			var v []byte
			for _, n := range nested {
				if n.Tag == rw.pad {
					n.Value = []byte(rw.keep)
				}
				v = tlv.AppendEntry(v, n)
			}
			for _, x := range rw.extra {
				v = tlv.AppendEntry(v, x)
			}
			if len(v) != len(e.Value) {
				return nil, fmt.Errorf("corpus: the rewrite of entry 0x%04X takes %d bytes, not %d", e.Tag, len(v), len(e.Value))
			}
			e.Value = v
		}
		out = tlv.AppendEntry(out, e)
	}

	return out, nil
}
