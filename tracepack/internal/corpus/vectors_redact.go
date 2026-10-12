package corpus

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// Digest domains of the redaction vectors (the tracepack semantics specification §8 Baseline rules).
const (
	ppbodyDomain      = "ppbody"
	formattedPPDomain = "formatted-pp"
	datasetDomain     = "dataset"
	rcpparvalDomain   = "rcpparval"
	sourceTextDomain  = "source-text"
)

// Values of the packs masked at write.
const (
	// redactPolicyID is the policy_id of the redaction vectors' policy.
	redactPolicyID = "corpus-baseline"
	// redactMaxFrameLen is the single max_frame_len of a pack masked at write: the oversized S7F3 exceeds it.
	redactMaxFrameLen = 256
	// redactFlushInterval is the flush_interval_ns of the consumer that writes them: one second.
	redactFlushInterval uint64 = 1_000_000_000
	// encryptedSegmentVector is the vector whose cases cover the masking clauses.
	encryptedSegmentVector = "redact-encrypted-segment"
	// redactPaddingTag is the private tag of the entry whose value pads the pack metadata of a vector edited after writing,
	// so the edited pack metadata keeps the length of the stored one.
	redactPaddingTag uint16 = 0x8000 | metaRedactionTag
	// redactPaddingLen is the length of the padding entry's value as the Writer writes it.
	redactPaddingLen = 512
)

// Vectors whose packs hold one PPBODY masked in two captures.
const (
	captureVectorA = "redact-capture-a"
	captureVectorB = "redact-capture-b"
	// capturePPBODYSeq is the seq of the masked S7F3 in each of them.
	capturePPBODYSeq uint64 = 1
)

// ppbodyOffset is the payload offset of the PPBODY content in the S7F3 of ppbodyText:
// the 14-byte frame prefix, the list header, the PPID item and the PPBODY item header.
const ppbodyOffset uint64 = hsmsPrefixLen + 2 + 2 + 4 + 2

// unparsedText is the raw value of the unparsed-entry annotation redact-annotation-zeroed masks.
const unparsedText = "unparsed source line"

// wholeTextInvalid is a message text that is not one valid item: a binary item shorter than its declared length.
var wholeTextInvalid = []byte{0x21, 0x05, 'a', 'b'}

// ppbodyContent returns the content bytes of the PPBODY the redaction vectors mask.
func ppbodyContent() []byte {
	return []byte("recipe body 0001")
}

// ppbodyItem returns the encoded PPBODY item: a binary item holding ppbodyContent, its header included,
// the original a resolved mask digests.
func ppbodyItem() []byte {
	return item(0o10, ppbodyContent()...)
}

// ppbodyText returns the message text of an S7F3 or S7F6 carrying the process program PP-1 with ppbodyItem:
// a list of the ASCII PPID and the binary PPBODY.
func ppbodyText() []byte {
	return slices.Concat(list(2), item(0o20, []byte("PP-1")...), ppbodyItem())
}

// redactPolicy returns the redaction vectors' policy under the digest key digestKeyID,
// with encryption under the first published encryption key:
// item rules of the baseline of the tracepack semantics specification §8 for the messages the vectors carry,
// and an annotation rule for unparsed-entry annotations.
func redactPolicy(digestKeyID string) (*redact.Compiled, error) {
	p := redact.Policy{
		ID: redactPolicyID, Version: 1, DigestKeyID: digestKeyID,
		ItemRules: []redact.ItemRule{
			{Stream: 7, Function: 3, Path: "2", Domain: ppbodyDomain},
			{Stream: 7, Function: 6, Path: "2", Domain: ppbodyDomain},
			{Stream: 7, Function: 36, Path: "3", Domain: ppbodyDomain},
			{Stream: 7, Function: 23, Path: "4", Domain: formattedPPDomain},
			{Stream: 7, Function: 26, Path: "4", Domain: formattedPPDomain},
			{Stream: 13, Function: 6, Path: "4/*", Domain: datasetDomain},
			{Stream: 16, Function: 11, Path: "5/3/*/2", Domain: rcpparvalDomain},
			{Stream: 16, Function: 15, Path: "2/*/4/3/*/2", Domain: rcpparvalDomain},
		},
		AnnotationRules: []redact.AnnotationRule{{Kind: tracepack.AnnotationKindUnparsedEntry, Domain: sourceTextDomain}},
		Encryption:      &redact.Encryption{KeyID: EncryptionKeyID1},
	}

	return redact.Compile(p)
}

// maskedMeta returns the pack metadata of a segment of seed that a consumer of a durable bus masks at write:
// a decoded-message capture on the capture clock (busCaptureMeta) with the consumer's flush_interval_ns,
// its one classifier and its one max_frame_len.
func maskedMeta(seed string) *tracepack.PackMeta {
	meta := busCaptureMeta(seed, tracepack.CaptureMethodDecodedMessage)
	meta.FlushIntervalNs = new(redactFlushInterval)
	meta.Classifiers, meta.MaxFrameLens = []string{corpusClassifier}, []uint64{redactMaxFrameLen}

	return meta
}

// maskedSpec is a pack a consumer masks at write: its records as received, classified, block by block.
type maskedSpec struct {
	// seed is the identity seed of the pack's capture_id, and of its pack_id unless packID is set.
	seed   string
	packID tracepack.UUID
	meta   *tracepack.PackMeta
	policy *redact.Compiled
	blocks [][]tracepack.Record
	// pad adds the private padding entry to the pack metadata, for a vector edited after writing (rewriteMeta).
	pad bool
}

// writeMasked masks the records of s under s.policy,
// as a consumer masks its flush batch (the tracepack storage specification §4):
// every record with the capture's published keys, a record carrying quality.unobserved left untouched,
// the entries collected in seq then first masked offset order into the pack metadata with the policy,
// and only then the pack written with the public Writer.
// A record that an entry s.meta already holds names was masked by hand: it is written as given, with those entries.
func writeMasked(s *maskedSpec) ([]byte, error) {
	capture := IDFor(roleCaptureID, s.seed)
	meta := *s.meta
	hand := meta.Redaction
	named := func(seq uint64) bool {
		return slices.ContainsFunc(hand, func(e tracepack.RedactionEntry) bool { return e.Seq == seq })
	}
	pol := s.policy.PackPolicy()
	meta.RedactionPolicy, meta.Redaction = &pol, slices.Clone(hand)
	facts := tracepack.PackFacts{RedactionPolicyApplied: true, DurableRecorder: true, AnyRedacted: len(hand) > 0}
	blocks := make([][]tracepack.Record, len(s.blocks))
	for i, b := range s.blocks {
		for j := range b {
			r := &b[j]
			facts.AnyClassified = facts.AnyClassified || classifiedStatus(r.DecodeStatus)
			facts.AnyOversized = facts.AnyOversized || r.DecodeStatus == tracepack.DecodeStatusOversized
			if named(r.Seq) {
				blocks[i] = append(blocks[i], *r)
				continue
			}
			masked, entries, err := s.policy.Mask(capture, r, r.Quality&tracepack.QualityUnobserved != 0, corpusKeys{})
			if err != nil {
				return nil, fmt.Errorf("corpus: mask seq %d: %w", r.Seq, err)
			}
			blocks[i] = append(blocks[i], masked)
			meta.Redaction = append(meta.Redaction, entries...)
			facts.AnyRedacted = facts.AnyRedacted || len(entries) > 0
		}
	}
	slices.SortStableFunc(meta.Redaction, compareEntries)
	if s.pad {
		meta.Unknown = append(slices.Clone(meta.Unknown), tracepack.RawEntry{
			Tag: redactPaddingTag, Type: uint8(tlv.TypeBytes), Value: make([]byte, redactPaddingLen),
		})
	}

	return writeSpec(&packSpec{seed: s.seed, packID: s.packID, meta: &meta, facts: facts, blocks: blocks})
}

// classifiedStatus reports whether status is a classification, neither not-attempted nor not-applicable.
func classifiedStatus(status tracepack.DecodeStatus) bool {
	return status != tracepack.DecodeStatusNotAttempted && status != tracepack.DecodeStatusNotApplicable
}

// compareEntries orders redaction entries by seq, then by first masked offset (the tracepack semantics specification §8 Marking).
func compareEntries(a, b tracepack.RedactionEntry) int {
	first := func(e tracepack.RedactionEntry) uint64 {
		if len(e.MaskedRanges) == 0 {
			return 0
		}

		return e.MaskedRanges[0].Offset
	}

	return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(first(a), first(b)))
}

// rewriteMeta returns pack, written by writeMasked with its padding entry, with its pack metadata decoded, changed by edit,
// and encoded again with the padding entry shortened by what the change adds, so the pack metadata keeps its length
// and nothing after it moves.
// It is how a vector holds redaction entries a Writer refuses to write.
func rewriteMeta(pack []byte, edit func(m *tracepack.PackMeta) error) ([]byte, error) {
	return editMeta(pack, func(stored []byte) ([]byte, error) {
		m, err := tracepack.UnmarshalPackMetaMinor(stored, 1)
		if err != nil {
			return nil, err
		}
		if err := edit(m); err != nil {
			return nil, err
		}
		k := slices.IndexFunc(m.Unknown, func(e tracepack.RawEntry) bool { return e.Tag == redactPaddingTag })
		if k < 0 {
			return nil, fmt.Errorf("corpus: the pack metadata holds no padding entry")
		}
		m.Unknown[k].Value = nil
		short, err := m.MarshalBinary()
		if err != nil {
			return nil, err
		}
		if len(short) > len(stored) {
			return nil, fmt.Errorf("corpus: the edited pack metadata takes %d bytes, more than the stored %d", len(short), len(stored))
		}
		m.Unknown[k].Value = make([]byte, len(stored)-len(short))

		return m.MarshalBinary()
	})
}

// encryptedCase is a record of redact-encrypted-segment: its case id and clauses, and the record as received, classified.
type encryptedCase struct {
	id    string
	cites []string
	rec   tracepack.Record
}

// redactVectors returns the recipes of the redact group (the tracepack corpus specification §9.1 and §9.4).
func redactVectors() []Recipe {
	return []Recipe{
		encryptedSegmentRecipe(),
		secondDigestKeyRecipe(),
		captureRecipe(captureVectorA),
		captureRecipe(captureVectorB),
		annotationZeroedRecipe(),
		matchedNothingRecipe(),
		recoverableDefectsRecipe(),
		overlappingEncryptedRecipe(),
		encryptedArchiveRecipe(),
		redactTruncationRecipe(),
		redactCorruptBlockRecipe(),
	}
}

// s7f3 returns the S7F3 W of seq, SessionID 1, carrying text, from the host, classified status.
func s7f3(seq uint64, text []byte, status tracepack.DecodeStatus) tracepack.Record {
	return message(seq, 7, 3, true, text, status)
}

// message returns the data message SsFf of seq, SessionID 1 and System Bytes seq, W set when wbit,
// carrying text, from the host when wbit and from the equipment otherwise, classified status.
func message(seq uint64, s, f byte, wbit bool, text []byte, status tracepack.DecodeStatus) tracepack.Record {
	dir := tracepack.DirEquipmentToHost
	if wbit {
		dir = tracepack.DirHostToEquipment
	}
	r := newData(seq, 1, dir, dataFrame(s, f, wbit, uint32(seq), text))
	r.DecodeStatus = status

	return r
}

// withFrame returns r carrying frame instead, its field_validity that of the frame's captured bytes.
func withFrame(r tracepack.Record, frame []byte) tracepack.Record {
	r.Payload = frame
	r.SetCapturedFieldValidity()

	return r
}

// item2 returns a SECS-II item of the format code code with two length bytes and the body body.
func item2(code byte, body []byte) []byte {
	return append([]byte{code<<2 | 2, byte(len(body) >> 8), byte(len(body))}, body...)
}

// localized returns a format-22 item: the 2-byte localized string header naming UCS-2 (1), then text.
func localized(text ...byte) []byte {
	return item(0o22, append([]byte{0x00, 0x01}, text...)...)
}

// rcpParams returns the message text of an S16F11 whose recipe variable parameters, at path 5/3, are the pairs params:
// a list of DATAID, PRJOBID, MF, an empty list of carriers and the recipe: its RCPSPEC, its method and its parameters.
func rcpParams(params ...[]byte) []byte {
	var pairs []byte
	for i, v := range params {
		pairs = slices.Concat(pairs, list(2), item(0o20, 'P', byte('1'+i)), v)
	}

	return slices.Concat(list(5), item(0o54, 0, 0, 0, 1), item(0o20, []byte("PJ-1")...), item(0o10, 0x0D), list(0),
		list(3), item(0o20, []byte("RCP-1")...), item(0o51, 1), list(byte(len(params))), pairs)
}

// dataset returns the message text of an S13F6 whose data set, at path 4, is items: a list of DATAID, the data set's id,
// its record count and items, or the single item notList in place of the list.
func dataset(id string, notList []byte, items ...[]byte) []byte {
	head := slices.Concat(list(4), item(0o20, []byte(id)...), item(0o54, 0, 0, 0, 1), item(0o54, 0, 0, 0, byte(len(items))))
	if notList != nil {
		return slices.Concat(head, notList)
	}

	return slices.Concat(head, list(byte(len(items))), slices.Concat(items...))
}

// encryptedCases returns the records of redact-encrypted-segment, one per case, ascending by seq from 0,
// each classified by hand from the tracepack semantics specification §3.
func encryptedCases() []encryptedCase {
	ok, cite := tracepack.DecodeStatusOK, []string{"SEM §8"}
	declared := func(seq uint64, d int) tracepack.Record {
		r := s7f3(seq, ppbodyText(), tracepack.DecodeStatusLengthMismatch)
		frame := slices.Clone(r.Payload)
		binary.BigEndian.PutUint32(frame, uint32(len(frame)-4+d))

		return withFrame(r, frame)
	}
	ccode := func(n byte, params ...[]byte) []byte {
		return slices.Concat(list(2), item(0o52, 0, n), list(byte(len(params))), slices.Concat(params...))
	}
	cases := []encryptedCase{
		{"ppbody", []string{"SEM §8", "FMT §5"}, s7f3(0, ppbodyText(), ok)},
		{"s7f6-same-program", cite, message(0, 7, 6, false, ppbodyText(), ok)},
		{"s7f23", cite, message(0, 7, 23, true, slices.Concat(list(4), item(0o20, []byte("PP-2")...), item(0o20, []byte("MDL-1")...),
			item(0o20, []byte("1.0")...), list(2),
			ccode(1, item(0o20, 'P', '1'), item(0o20, 'P', '2')), ccode(2, item(0o20, 'P', '3'))), ok)},
		{"s6f11-unmasked", cite, message(0, 6, 11, true, ppbodyText(), ok)},
		{"declared-short", []string{"SEM §8", "SEM §3"}, declared(0, -1)},
		{"declared-long", []string{"SEM §8", "SEM §3"}, declared(0, 1)},
		{"oversized", []string{"SEM §8", "SEM §3"}, s7f3(0, slices.Concat(list(2), item(0o20, []byte("PP-3")...),
			item2(0o10, bytes.Repeat([]byte("big-"), 75))), tracepack.DecodeStatusOversized)},
		{"short-payloads", cite, withFrame(s7f3(0, nil, tracepack.DecodeStatusShortFrame), dataFrame(7, 3, true, 7, nil)[:10])},
		{"whole-text-invalid", cite, s7f3(0, wholeTextInvalid, tracepack.DecodeStatusItemDecodeError)},
		{"whole-text-trailing", cite, s7f3(0, append(ppbodyText(), 0, 0), tracepack.DecodeStatusOKWithTrailing)},
		{"whole-text-unresolved", cite, s7f3(0, slices.Concat(list(1), item(0o20, []byte("PP-4")...)), ok)},
		{"s13f6-not-list", cite, message(0, 13, 6, false, dataset("DS-1", item(0o20, []byte("not-a-list")...)), ok)},
		{"s16f15-missing-branch", cite, message(0, 16, 15, true, slices.Concat(list(2), item(0o54, 0, 0, 0, 1), list(1),
			list(3), item(0o20, []byte("PJ-2")...), item(0o10, 0x0D), item(0o20, 'x')), ok)},
		{"no-target-s13f6-empty-list", cite, message(0, 13, 6, false, dataset("DS-2", nil), ok)},
		{"no-target-s7f6-denial", cite, message(0, 7, 6, false, list(0), ok)},
		{"no-target-s7f26-denial", cite, message(0, 7, 26, false, list(0), ok)},
		{"no-target-s7f36-denial", cite, message(0, 7, 36, false, list(0), ok)},
		{"no-target-zero-length-ppbody", cite, s7f3(0, slices.Concat(list(2), item(0o20, []byte("PP-5")...), item(0o10)), ok)},
		{"no-target-header-only", cite, s7f3(0, nil, ok)},
		{"format22-header-kept", cite, message(0, 16, 11, true, rcpParams(localized(0, '2', 0, '5'), item(0o54, 0, 0, 0, 30)), ok)},
		{"format22-two-byte", cite, message(0, 16, 11, true, rcpParams(localized()), ok)},
		{"format22-one-byte", cite, message(0, 16, 11, true, rcpParams(item(0o22, 0x00)), tracepack.DecodeStatusItemDecodeError)},
		{"s13f6-fildat", cite, message(0, 13, 6, false, dataset("DS-3", nil, item(0o10, []byte("chunk-1")...), item(0o10, []byte("chunk-2")...)), ok)},
		{"already-zero", cite, s7f3(0, slices.Concat(list(2), item(0o20, []byte("PP-6")...), item(0o10, make([]byte, 16)...)), ok)},
	}
	for i := range cases {
		seq := uint64(i)
		r := &cases[i].rec
		r.Seq, r.TSUTCNs = seq, msAt(int64(seq))
		if len(r.Payload) >= hsmsPrefixLen {
			binary.BigEndian.PutUint32(r.Payload[10:], uint32(seq))
		}
	}
	// The trailing case's excess.
	cases[9].rec.TrailingBytes = 2

	return cases
}

// encryptedRecords returns the records of encryptedCases from first to last, inclusive, in blocks of eight seqs.
func encryptedRecords(first, last int) [][]tracepack.Record {
	var blocks [][]tracepack.Record
	for i, c := range encryptedCases()[first : last+1] {
		if i%8 == 0 {
			blocks = append(blocks, nil)
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], c.rec)
	}

	return blocks
}

// encryptedSegmentRecipe returns redact-encrypted-segment.
func encryptedSegmentRecipe() Recipe {
	all := encryptedCases()
	cases := make([]Case, 0, len(all))
	for i, c := range all {
		cases = append(cases, Case{ID: c.id, Seq: U64(i), Cites: c.cites})
	}

	return Recipe{
		ID: encryptedSegmentVector, Title: "a segment masked at write under a policy with encryption, one case per masking clause",
		Cites: []string{"SEM §8", "FMT §5", "FMT §9", "FMT §14", "STO §4", "JSONL §5"}, Class: ClassRead, Cases: cases,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID1)
			if err != nil {
				return nil, err
			}
			pack, err := writeMasked(&maskedSpec{seed: seed, meta: maskedMeta(seed), policy: policy, blocks: encryptedRecords(0, 23)})

			return &Built{Pack: pack}, err
		},
		Expect: encryptedSegmentExpect(),
	}
}

// encryptedSegmentExpect is the expectation of redact-encrypted-segment, from the tracepack semantics specification §8:
// a pack of minor 1 under the policy with both encryption tags, redaction-present set;
// the ppbody entry, its digest the published ppbody case and its siv the published ppbody-segment case,
// the S7F6's digest equal to it;
// a masked record keeping its length, decode_status and trailing bytes; the S6F11 of the same text not masked;
// the whole-text entry of the invalid text without item_path, in domain message-text,
// its digest the published message-text case;
// and one entry per FILDAT.
func encryptedSegmentExpect() *Expectation {
	ppbody := entryJSON(0, "2", ppbodyDomain, publishedDigest("ppbody"), publishedSIV("ppbody-segment"), ppbodyOffset, uint64(len(ppbodyContent())))
	fildat := hsmsPrefixLen + uint64(len(dataset("DS-3", nil))) + 2

	return &Expectation{
		Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 23), PrefixEnd: AtEnd(),
		ExportHas: []string{
			`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1,"flags":["redaction-present"]`,
			`"redaction_policy":{"policy_id":"corpus-baseline","policy_version":"1","key_id":"test-digest-1","digest_algorithm":"hmac-sha256",` +
				`"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"test-enc-1"}`,
			ppbody,
			`{"seq":"1","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset, uint64(len(ppbodyContent()))) + `","domain":"ppbody","digest":"` +
				publishedDigest("ppbody") + `"`,
			`"seq":"0","ts_utc_ns":"1791000000000000000","mono_ns":"0","epoch":1,"payload_len":40,"trailing_bytes":0,"quality":["redacted"],` +
				`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"ok"`,
			`"seq":"3","ts_utc_ns":"1791000000003000000","mono_ns":"0","epoch":1,"payload_len":40,"trailing_bytes":0,"quality":[],`,
			`"seq":"4","ts_utc_ns":"1791000000004000000","mono_ns":"0","epoch":1,"payload_len":40,"trailing_bytes":0,"quality":["redacted"],` +
				`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"length-mismatch"`,
			`"seq":"9","ts_utc_ns":"1791000000009000000","mono_ns":"0","epoch":1,"payload_len":42,"trailing_bytes":2,"quality":["redacted"],` +
				`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"ok-with-trailing"`,
			`{"seq":"8","masked_ranges":"` + rangesB64(hsmsPrefixLen, uint64(len(wholeTextInvalid))) + `","domain":"message-text","digest":"` +
				publishedDigest("message-text") + `","siv":"`,
			`{"seq":"22","item_path":"4/1","masked_ranges":"` + rangesB64(fildat, 7) + `","domain":"dataset"`,
			`{"seq":"22","item_path":"4/2","masked_ranges":"` + rangesB64(fildat+9, 7) + `","domain":"dataset"`,
		},
	}
}

// entryJSON returns the export of a redaction entry with one masked range (the tracepack JSONL specification §4),
// its item_path omitted when path is empty.
func entryJSON(seq uint64, path, domain, digest, sivB64 string, off, n uint64) string {
	s := fmt.Sprintf(`{"seq":"%d",`, seq)
	if path != "" {
		s += `"item_path":"` + path + `",`
	}
	s += `"masked_ranges":"` + rangesB64(off, n) + `","domain":"` + domain + `","digest":"` + digest + `"`
	if sivB64 != "" {
		s += `,"siv":"` + sivB64 + `"`
	}

	return s + "}"
}

// rangesB64 returns the export of a masked_ranges value of the pairs offsetsAndLengths: base64 of their little-endian u64 values.
func rangesB64(offsetsAndLengths ...uint64) string {
	var b []byte
	for _, v := range offsetsAndLengths {
		b = binary.LittleEndian.AppendUint64(b, v)
	}

	return b64(b)
}

// publishedDigest returns the digest of the primitives.json hmac case name, in base64.
func publishedDigest(name string) string {
	var p Primitives
	if err := p.AddKeys(); err != nil {
		return "error: " + err.Error()
	}
	for _, c := range p.HMAC {
		if c.Name == name {
			return b64(c.Digest)
		}
	}

	return "no hmac case " + name
}

// publishedSIV returns the siv of the primitives.json aes_siv case name, the first 16 bytes of its output, in base64.
func publishedSIV(name string) string {
	var p Primitives
	if err := p.AddKeys(); err != nil {
		return "error: " + err.Error()
	}
	for _, c := range p.AESSIV {
		if c.Name == name {
			return b64(c.Output[:siv.SIVSize])
		}
	}

	return "no aes_siv case " + name
}

// singlePPBODY returns the records of a pack holding one masked PPBODY: an S1F1 (seq 0), then the S7F3 of ppbodyText (seq 1).
func singlePPBODY() [][]tracepack.Record {
	ppbody := s7f3(capturePPBODYSeq, ppbodyText(), tracepack.DecodeStatusOK)

	return [][]tracepack.Record{{message(0, 1, 1, true, nil, tracepack.DecodeStatusOK), ppbody}}
}

// secondDigestKeyRecipe returns redact-second-digest-key.
func secondDigestKeyRecipe() Recipe {
	return Recipe{
		ID: "redact-second-digest-key", Title: "the ppbody S7F3 under the second digest key and key id",
		Cites: []string{"SEM §8", "FMT §5"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID2)
			if err != nil {
				return nil, err
			}
			pack, err := writeMasked(&maskedSpec{seed: seed, meta: maskedMeta(seed), policy: policy, blocks: singlePPBODY()})

			return &Built{Pack: pack}, err
		},
		// Another key gives another digest of the same original, the published ppbody-second-key case.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"key_id":"test-digest-2"`,
				`{"seq":"1","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset, uint64(len(ppbodyContent()))) +
					`","domain":"ppbody","digest":"` + publishedDigest("ppbody-second-key") + `"`,
			},
		},
	}
}

// captureRecipe returns redact-capture-a or redact-capture-b, id, of one PPBODY masked in the capture its seed names.
func captureRecipe(id string) Recipe {
	name := "ppbody-capture-a"
	if id == captureVectorB {
		name = "ppbody-capture-b"
	}

	return Recipe{
		ID: id, Title: "one PPBODY masked in a capture of its own, its digest that of the other capture's",
		Cites: []string{"SEM §8"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID1)
			if err != nil {
				return nil, err
			}
			pack, err := writeMasked(&maskedSpec{seed: seed, meta: maskedMeta(seed), policy: policy, blocks: singlePPBODY()})

			return &Built{Pack: pack}, err
		},
		// The digest is the published ppbody case in both captures, the siv the capture's own published case.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
			ExportHas: []string{
				entryJSON(capturePPBODYSeq, "2", ppbodyDomain, publishedDigest("ppbody"), publishedSIV(name),
					ppbodyOffset, uint64(len(ppbodyContent()))),
			},
		},
	}
}

// annotationZeroedRecipe returns redact-annotation-zeroed.
func annotationZeroedRecipe() Recipe {
	return Recipe{
		ID: "redact-annotation-zeroed", Title: "an unparsed-entry annotation masked through raw under a policy with encryption",
		Cites: []string{"SEM §8", "FMT §8"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID1)
			if err != nil {
				return nil, err
			}
			note, err := newAnnotation(0, 1, &tracepack.Annotation{AnnotationKind: tracepack.AnnotationKindUnparsedEntry, Raw: []byte(unparsedText)})
			if err != nil {
				return nil, err
			}
			pack, err := writeMasked(&maskedSpec{seed: seed, meta: maskedMeta(seed), policy: policy, blocks: [][]tracepack.Record{
				{note, message(1, 1, 1, true, nil, tracepack.DecodeStatusOK)},
			}})

			return &Built{Pack: pack}, err
		},
		// An annotation's target bytes become zeros and keep no original, so its entry has no siv;
		// the digest is the published source-text case.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"flags":["redaction-present"]`,
				`"domain":"source-text","digest":"` + publishedDigest("source-text") + `"}`,
				`"quality":["redacted"],"kind":"annotation"`,
				`"body":{"annotation_kind":"unparsed-entry","raw":"` + b64(make([]byte, len(unparsedText))) + `"}`,
			},
		},
	}
}

// matchedNothingRecipe returns redact-matched-nothing.
func matchedNothingRecipe() Recipe {
	return Recipe{
		ID: "redact-matched-nothing", Title: "a segment under a policy with encryption that matched nothing",
		Cites: []string{"SEM §8", "FMT §5", "FMT §14"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID1)
			if err != nil {
				return nil, err
			}
			pack, err := writeMasked(&maskedSpec{seed: seed, meta: maskedMeta(seed), policy: policy, blocks: [][]tracepack.Record{{
				message(0, 1, 1, true, nil, tracepack.DecodeStatusOK), message(1, 1, 2, false, item(0o20, []byte("MDL")...), tracepack.DecodeStatusOK),
			}}})

			return &Built{Pack: pack}, err
		},
		// The policy is written though nothing matched: minor 1 for its encryption tags, no entry, redaction-present clear.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 1), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1,"flags":[]`,
				`"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"test-enc-1"}`,
			},
		},
	}
}

// defectCase is a record of redact-recoverable-defects or redact-overlapping-encrypted and the edit of its entries.
type defectCase struct {
	id string
	// masked makes the record an S7F3 the policy masks;
	// otherwise it is an S1F1 nothing matches, carrying the S7F3's message text,
	// so a range of an S7F3's entry lies inside its payload and past the frame prefix.
	masked bool
	// edit changes the record's entries, as Mask wrote them, after writing; nil keeps them.
	edit func(seq uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry
}

// recoverableDefectCases are the cases of redact-recoverable-defects:
// one recoverable entry defect of the tracepack semantics specification §8 Marking each,
// after isolation, a valid masked record before them.
func recoverableDefectCases() []defectCase {
	ranges := func(rs ...tracepack.MaskedRange) func(uint64, []tracepack.RedactionEntry) []tracepack.RedactionEntry {
		return func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			es[0].MaskedRanges = rs
			return es
		}
	}
	at := func(off, n uint64) tracepack.MaskedRange { return tracepack.MaskedRange{Offset: off, Length: n} }

	return []defectCase{
		{id: "isolation", masked: true},
		{id: "empty-ranges", masked: true, edit: ranges()},
		{id: "odd-ranges", masked: true, edit: func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			es[0].MaskedRanges, es[0].RawMaskedRanges = nil, binary.LittleEndian.AppendUint64(nil, ppbodyOffset)
			return es
		}},
		{id: "zero-length", masked: true, edit: ranges(at(ppbodyOffset, 0))},
		{id: "unsorted-ranges", masked: true, edit: ranges(at(ppbodyOffset+8, 8), at(ppbodyOffset, 4))},
		{id: "adjacent-ranges", masked: true, edit: ranges(at(ppbodyOffset, 8), at(ppbodyOffset+8, 8))},
		{id: "range-past-payload", masked: true, edit: ranges(at(ppbodyOffset, 100))},
		// An entry valid in every other check names a record of the same length without redacted.
		{id: "entry-without-redacted", edit: func(seq uint64, _ []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			return []tracepack.RedactionEntry{validEntry(seq)}
		}},
		{id: "redacted-without-entry", masked: true, edit: func(uint64, []tracepack.RedactionEntry) []tracepack.RedactionEntry { return nil }},
		{id: "missing-siv", masked: true, edit: func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			es[0].SIV, es[0].HasSIV = nil, false
			return es
		}},
		{id: "ill-sized-siv", masked: true, edit: func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			es[0].SIV = es[0].SIV[:siv.SIVSize-1]
			return es
		}},
	}
}

// overlappingCases are the cases of redact-overlapping-encrypted: encrypted entries of one record overlapping,
// one an exact duplicate of another, and entries only adjacent, which the Writer accepts.
func overlappingCases() []defectCase {
	return []defectCase{
		{id: "overlap", masked: true, edit: func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			second := es[0]
			second.MaskedRanges = []tracepack.MaskedRange{{Offset: ppbodyOffset + 4, Length: 8}}
			second.SIV = bytes.Repeat([]byte{0xA5}, siv.SIVSize)
			return append(es, second)
		}},
		{id: "duplicate", masked: true, edit: func(_ uint64, es []tracepack.RedactionEntry) []tracepack.RedactionEntry {
			return append(es, es[0])
		}},
		{id: "adjacent", masked: true},
	}
}

// validEntry returns an entry of the form Mask writes for a masked S7F3 of seq:
// path 2, the PPBODY content's range, domain ppbody, and a siv of the right length;
// it names a record the policy does not mask.
func validEntry(seq uint64) tracepack.RedactionEntry {
	return tracepack.RedactionEntry{
		Seq: seq, ItemPath: new("2"), MaskedRanges: []tracepack.MaskedRange{{Offset: ppbodyOffset, Length: uint64(len(ppbodyContent()))}},
		Domain: ppbodyDomain, Digest: make([]byte, redact.DigestSize), SIV: make([]byte, siv.SIVSize), HasSIV: true,
	}
}

// defectBuild returns the Build of a vector of cases: one record per case, ascending by seq from 0, in one block,
// masked at write with the padding entry, then each case's entries changed by its edit (rewriteMeta).
// The case adjacent of overlappingCases is masked by hand: its PPBODY content encrypted as two entries of adjacent ranges.
func defectBuild(cases []defectCase) func(seed string) (*Built, error) {
	return func(seed string) (*Built, error) {
		policy, err := redactPolicy(DigestKeyID1)
		if err != nil {
			return nil, err
		}
		capture := IDFor(roleCaptureID, seed)
		var recs []tracepack.Record
		var handMasked []tracepack.RedactionEntry
		for i, c := range cases {
			seq := uint64(i)
			switch {
			case c.id == "adjacent":
				r, es, err := adjacentMasked(capture, seq)
				if err != nil {
					return nil, err
				}
				recs, handMasked = append(recs, r), append(handMasked, es...)
			case c.masked:
				recs = append(recs, s7f3(seq, ppbodyText(), tracepack.DecodeStatusOK))
			default:
				recs = append(recs, message(seq, 1, 1, true, ppbodyText(), tracepack.DecodeStatusOK))
			}
		}
		meta := maskedMeta(seed)
		meta.Redaction = handMasked
		pack, err := writeMasked(&maskedSpec{seed: seed, meta: meta, policy: policy, blocks: [][]tracepack.Record{recs}, pad: true})
		if err != nil {
			return nil, err
		}
		pack, err = rewriteMeta(pack, func(m *tracepack.PackMeta) error {
			var out []tracepack.RedactionEntry
			for i, c := range cases {
				seq := uint64(i)
				var es []tracepack.RedactionEntry
				for _, e := range m.Redaction {
					if e.Seq == seq {
						es = append(es, e)
					}
				}
				if c.edit != nil {
					es = c.edit(seq, es)
				}
				out = append(out, es...)
			}
			m.Redaction = out

			return nil
		})

		return &Built{Pack: pack}, err
	}
}

// adjacentMasked returns the S7F3 of seq carrying ppbodyText with its PPBODY content encrypted as two entries,
// the first half and the second, whose ranges are adjacent:
// each sealed under the published key with the associated data of the capture, the seq and its own first offset,
// each digest that of its own original bytes.
func adjacentMasked(capture tracepack.UUID, seq uint64) (tracepack.Record, []tracepack.RedactionEntry, error) {
	r := s7f3(seq, ppbodyText(), tracepack.DecodeStatusOK)
	r.Payload = slices.Clone(r.Payload)
	key := PublishedKey(EncryptionKeyID1, siv.KeySize)
	half := uint64(len(ppbodyContent())) / 2
	var entries []tracepack.RedactionEntry
	for _, off := range []uint64{ppbodyOffset, ppbodyOffset + half} {
		target := r.Payload[off : off+half]
		digest, err := redact.Digest(PublishedKey(DigestKeyID1, digestKeySize), ppbodyDomain, target)
		if err != nil {
			return tracepack.Record{}, nil, err
		}
		ad := siv.AssociatedData(capture, seq, off)
		iv, ct, err := siv.Seal(key, ad[:], target)
		if err != nil {
			return tracepack.Record{}, nil, err
		}
		copy(target, ct)
		entries = append(entries, tracepack.RedactionEntry{
			Seq: seq, ItemPath: new("2"), MaskedRanges: []tracepack.MaskedRange{{Offset: off, Length: half}},
			Domain: ppbodyDomain, Digest: digest, SIV: iv[:], HasSIV: true,
		})
	}
	r.Quality |= tracepack.QualityRedacted

	return r, entries, nil
}

// defectCaseList returns the manifest cases of cases, one per record, ascending by seq from 0.
func defectCaseList(cases []defectCase, cites []string) []Case {
	out := make([]Case, 0, len(cases))
	for i, c := range cases {
		out = append(out, Case{ID: c.id, Seq: U64(i), Cites: cites})
	}

	return out
}

// recoverableDefectsRecipe returns redact-recoverable-defects.
func recoverableDefectsRecipe() Recipe {
	cases := recoverableDefectCases()

	return Recipe{
		ID: "redact-recoverable-defects", Title: "one recoverable redaction entry defect per record, after a valid masked record",
		Cites: []string{"SEM §8", "FMT §5", "FMT §13"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
		Cases: defectCaseList(cases, []string{"SEM §8"}),
		Build: defectBuild(cases),
		// Every defect is recoverable: the pack opens, every record is exported, and the entries are kept as stored.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, uint64(len(cases)-1)), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"schema":"tracepack-jsonl/2"`,
				`{"seq":"1","item_path":"2","masked_ranges":"","domain":"ppbody"`,
				`{"seq":"2","item_path":"2","masked_ranges":"` + b64(binary.LittleEndian.AppendUint64(nil, ppbodyOffset)) + `","domain":"ppbody"`,
				`{"seq":"6","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset, 100) + `","domain":"ppbody"`,
			},
		},
	}
}

// overlappingEncryptedRecipe returns redact-overlapping-encrypted.
func overlappingEncryptedRecipe() Recipe {
	cases := overlappingCases()

	return Recipe{
		ID: "redact-overlapping-encrypted", Title: "encrypted entries of one record that overlap, an exact duplicate, and entries only adjacent",
		Cites: []string{"SEM §8", "FMT §5"}, Class: ClassRead, Labels: []string{LabelNonconformingWriter},
		Cases: defectCaseList(cases, []string{"SEM §8"}),
		Build: defectBuild(cases),
		// The pack opens with every entry kept; the adjacent entries are each sealed at their own first offset.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 2), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`{"seq":"0","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset+4, 8) + `","domain":"ppbody"`,
				`{"seq":"2","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset, 8) + `","domain":"ppbody"`,
				`{"seq":"2","item_path":"2","masked_ranges":"` + rangesB64(ppbodyOffset+8, 8) + `","domain":"ppbody"`,
			},
		},
	}
}

// encryptedArchiveRecipe returns redact-encrypted-archive.
func encryptedArchiveRecipe() Recipe {
	return Recipe{
		ID: "redact-encrypted-archive", Title: "redact-encrypted-segment's records merged from two segments, one record in both",
		Cites: []string{"STO §4", "SEM §8", "FMT §5", "FMT §14"}, Class: ClassRead,
		Build: func(seed string) (*Built, error) {
			policy, err := redactPolicy(DigestKeyID1)
			if err != nil {
				return nil, err
			}
			var inputs [][]byte
			for _, part := range []struct {
				name        string
				first, last int
			}{{"a", 0, 12}, {"b", 12, 23}} {
				meta := maskedMeta(seed)
				meta.SeqStart = uint64(part.first)
				pack, err := writeMasked(&maskedSpec{
					seed: seed, packID: IDFor(rolePackID, seed+"/"+part.name), meta: meta, policy: policy,
					blocks: encryptedRecords(part.first, part.last),
				})
				if err != nil {
					return nil, err
				}
				inputs = append(inputs, pack)
			}
			pack, err := mergePacks(seed, inputs)

			return &Built{Pack: pack}, err
		},
		// The inputs overlap at seq 12 and their blocks are small, so the archive holds one block, resolved and coalesced (the tracepack storage specification §4 Merge);
		// it carries the policy and every entry once, the record of seq 12 among them, and the inputs' minor.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 1, Seqs: seqRange(0, 23), PrefixEnd: AtEnd(),
			ExportHas: []string{
				`"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1,"flags":["redaction-present"]`,
				`"pack_role":"archive"`,
				`"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"test-enc-1"}`,
				entryJSON(0, "2", ppbodyDomain, publishedDigest("ppbody"), publishedSIVOf(encryptedSegmentArchive, 0), ppbodyOffset, uint64(len(ppbodyContent()))),
			},
		},
	}
}

// encryptedSegmentArchive is the identity seed of redact-encrypted-archive's capture.
const encryptedSegmentArchive = "redact-encrypted-archive"

// publishedSIVOf returns, in base64, the siv of the ppbody entry of the S7F3 of seq in the capture of seed:
// the PPBODY content sealed under the first published encryption key at ppbodyOffset.
func publishedSIVOf(seed string, seq uint64) string {
	ad := siv.AssociatedData(IDFor(roleCaptureID, seed), seq, ppbodyOffset)
	iv, _, err := siv.Seal(PublishedKey(EncryptionKeyID1, siv.KeySize), ad[:], ppbodyContent())
	if err != nil {
		return "error: " + err.Error()
	}

	return b64(iv[:])
}

// mergePacks merges inputs, segments of the capture of seed, into an archive of generation 1 with identifiers from seed,
// the corpus clock and codec none.
func mergePacks(seed string, inputs [][]byte) ([]byte, error) {
	in := make([]tracepack.MergeInput, 0, len(inputs))
	for _, p := range inputs {
		in = append(in, tracepack.MergeInput{ReaderAt: bytes.NewReader(p), Size: int64(len(p))})
	}
	var out bytes.Buffer
	var conflicts []tracepack.Conflict
	_, err := tracepack.Merge(context.Background(), &out, in, tracepack.MergeOptions{
		Writer: corpusWriter, ScopeGeneration: 1, PublisherEpoch: 1,
		ReplacementSetID: IDFor(roleReplacementSetID, seed), PackID: IDFor(rolePackID, seed), Now: corpusNow, Uncompressed: true,
		OnConflict: func(c tracepack.Conflict) { conflicts = append(conflicts, c) },
	})
	if err != nil {
		return nil, fmt.Errorf("corpus: merge (%d conflicts): %w", len(conflicts), err)
	}

	return out.Bytes(), nil
}

// damagedBlocks are the records of the base pack of redact-truncation and redact-corrupt-block, masked and thinned:
// three blocks, each a masked record and an unobserved poll: the ppbody S7F3 (seq 0) and an S1F3 (seq 1),
// the S7F6 of the same program (seq 2) and its S1F4 reply (seq 3), the S7F3 of an invalid text (seq 4) and an S1F3 (seq 5).
func damagedBlocks() [][]tracepack.Record {
	poll := func(seq uint64, f byte) tracepack.Record {
		return unobserved(message(seq, 1, f, f == 3, slices.Concat(list(1), item(0o54, 0, 0, 0, 1)), tracepack.DecodeStatusNotAttempted))
	}
	reply := message(2, 7, 6, false, ppbodyText(), tracepack.DecodeStatusOK)

	return [][]tracepack.Record{
		{s7f3(0, ppbodyText(), tracepack.DecodeStatusOK), poll(1, 3)},
		{reply, poll(3, 4)},
		{s7f3(4, wholeTextInvalid, tracepack.DecodeStatusItemDecodeError), poll(5, 3)},
	}
}

// damagedBase writes the base pack of redact-truncation and redact-corrupt-block: damagedBlocks masked at write
// in a segment that declares S1F3 and S1F4 thinned.
func damagedBase(seed string) ([]byte, error) {
	policy, err := redactPolicy(DigestKeyID1)
	if err != nil {
		return nil, err
	}
	meta := maskedMeta(seed)
	meta.Thinning = []tracepack.Thinning{{Stream: 1, Function: 3, Source: fdcPollSource}, {Stream: 1, Function: 4, Source: fdcPollSource}}

	return writeMasked(&maskedSpec{seed: seed, meta: meta, policy: policy, blocks: damagedBlocks()})
}

// redactTruncationRecipe returns redact-truncation.
func redactTruncationRecipe() Recipe {
	return Recipe{
		ID: "redact-truncation", Title: "a masked and thinned pack of three blocks cut at every byte offset",
		Cites: []string{"FMT §13", "SEM §8", "SEM §2", "CORPUS §5.7"}, Class: ClassTruncation,
		Build: func(seed string) (*Built, error) {
			pack, err := damagedBase(seed)

			return &Built{Pack: pack}, err
		},
		// Every cut that opens keeps the records of the blocks wholly before it, masked and unobserved as written,
		// and its export is the base's header line and their lines.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeFinalizedConsistent, FooterValid: true, Blocks: 3, Seqs: seqRange(0, 5), PrefixEnd: AtEnd(),
			ExportHas: []string{`"schema":"tracepack-jsonl/2"`, `"quality":["unobserved"]`, `"quality":["redacted"]`, `"thinning":[`},
			Cuts:      blockCuts([]uint64{2, 4, 6}),
		},
	}
}

// blockCuts returns the cut expectations of a finalized pack of len(records) blocks,
// block i ending after records[i] records (the tracepack format specification §13):
// a cut inside the file header is too short, one inside the pack metadata ends before it;
// every other cut opens unfinalized with the blocks wholly before it validated,
// and a cut that ends inside a block, or inside the footer and trailer, stops the walk there.
func blockCuts(records []uint64) []CutWant {
	read := func(from Pos, validated int, recs uint64, end Pos, stopped bool) CutWant {
		c := CutWant{From: from, Outcome: tracepack.OutcomeUnfinalized, Validated: validated, Records: recs, PrefixEnd: end}
		if stopped {
			c.WalkStop = end
		}

		return c
	}
	cuts := []CutWant{
		{From: AtOffset(0), Rejection: RejectShortObject},
		{From: AtOffset(format.FileHeaderLen), Rejection: RejectMetadataPastObject},
	}
	var before uint64
	for i, end := range records {
		cuts = append(cuts, read(AtBlock(i), i, before, AtBlock(i), false), read(AtBlock(i).Plus(1), i, before, AtBlock(i), true))
		before = end
	}

	return append(cuts, read(AtEnd(), len(records), before, AtEnd(), false), read(AtEnd().Plus(1), len(records), before, AtEnd(), true))
}

// redactCorruptBlockRecipe returns redact-corrupt-block.
func redactCorruptBlockRecipe() Recipe {
	return Recipe{
		ID: "redact-corrupt-block", Title: "a masked and thinned pack whose middle block fails its body CRC",
		Cites: []string{"FMT §13", "FMT I-2", "SEM §8"}, Class: ClassRead, Labels: []string{LabelDamaged},
		Build: func(seed string) (*Built, error) {
			pack, err := damagedBase(seed)
			if err != nil {
				return nil, err
			}
			l, err := Locate(pack)
			if err != nil {
				return nil, err
			}
			pack, err = FlipByte(pack, int(l.Blocks[1].Offset)+format.EnvelopeLen+3)

			return &Built{Pack: pack}, err
		},
		// The failed block loses only its own records; its valid footer gives their range.
		Expect: &Expectation{
			Outcome: tracepack.OutcomeCorruptMiddle, FooterValid: true, Blocks: 3, Seqs: []uint64{0, 1, 4, 5},
			Failed: []FailedWant{{Block: 1, Cause: tracepack.ReasonCorruptBlock}}, PrefixEnd: AtBlock(1),
			Lost:      []LostWant{{First: 2, Last: 3, TimeStart: msAt(2), TimeEnd: msAt(3)}},
			ExportHas: []string{`"schema":"tracepack-jsonl/2"`},
		},
	}
}

// b64 returns b in standard base64 with padding, the form of bytes in an export (the tracepack JSONL specification §3).
func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
