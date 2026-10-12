package tracepack

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// minor0Golden returns the pack of testdata/minor0_new_numbers and the export tracepack v0.4.0 wrote of it:
// a format 1.0 extract holding the numbers format 1.1 assigns (see its README.md).
func minor0Golden(t *testing.T) (pack, export []byte) {
	t.Helper()

	pack, err := os.ReadFile("testdata/minor0_new_numbers/pack.tpk")
	require.NoError(t, err)
	export, err = os.ReadFile("testdata/minor0_new_numbers/export.jsonl")
	require.NoError(t, err)

	return pack, export
}

// requireMinor0Numbers checks that m, the pack metadata of the minor-0 golden pack,
// keeps the numbers of format 1.1 unknown.
func requireMinor0Numbers(t *testing.T, m *PackMeta) {
	t.Helper()

	require.NotNil(t, m)
	assert.Equal(t, []RawEntry{
		{Tag: tagThinning, Type: uint8(tlv.TypeBytes), Value: []byte("thinning-1")},
		{Tag: tagThinning, Type: uint8(tlv.TypeBytes), Value: []byte("thinning-2")},
	}, m.Unknown)
	assert.Nil(t, m.Thinning)
	require.NotNil(t, m.RedactionPolicy)
	assert.Nil(t, m.RedactionPolicy.EncryptionAlgorithm)
	assert.Nil(t, m.RedactionPolicy.EncryptionKeyID)
	require.Len(t, m.Redaction, 1)
	assert.False(t, m.Redaction[0].HasSIV)
	assert.Nil(t, m.Redaction[0].SIV)
}

// A reader decodes a pack's metadata by the minor its file header states (the tracepack format specification §5 and §14):
// in a pack of minor 0 the numbers format 1.1 assigns, opaque, repeated and of the wrong value types for format 1.1,
// stay unknown, through every way the metadata is decoded, and a stored bit 7 means nothing.
func TestReaderVersionAware(t *testing.T) {
	t.Parallel()

	file, _ := minor0Golden(t)
	r := mustOpen(t, file, ReaderOptions{})
	h := r.Header()
	require.Equal(t, uint16(0), h.FormatMinor)
	requireMinor0Numbers(t, h.Meta)
	info := r.Info()
	requireMinor0Numbers(t, info.Meta())
	requireMinor0Numbers(t, info.Meta())

	hdr, err := format.UnmarshalFileHeader(file)
	require.NoError(t, err)
	raw := file[format.FileHeaderLen : format.FileHeaderLen+int(hdr.PackMetadataLen)]
	p, err := NewPackInfoMinor(h.PackID, h.CaptureID, 0, raw)
	require.NoError(t, err)
	requireMinor0Numbers(t, p.Meta())
	requireMinor0Numbers(t, p.Meta())
	// Minor 1 knows the numbers: the entry's siv is not repeatable, and the other values have the wrong types.
	_, err = NewPackInfoMinor(h.PackID, h.CaptureID, 1, raw)
	require.ErrorIs(t, err, tlv.ErrDuplicate)
	_, err = NewPackInfo(h.PackID, h.CaptureID, raw)
	require.ErrorIs(t, err, tlv.ErrDuplicate, "NewPackInfo decodes with the newest registry")
	_, err = openBytes(t, patchHeader(t, file, func(fh *format.FileHeader) { fh.FormatMinor = 1 }), ReaderOptions{})
	require.ErrorIs(t, err, tlv.ErrDuplicate, "the same metadata in a pack of minor 1 is invalid")

	// Without the entry's second siv, the first value of the wrong type is found.
	entries, err := tlv.Decode(raw)
	require.NoError(t, err)
	var noRedaction []byte
	for _, e := range entries {
		if e.Tag != tagRedaction {
			noRedaction = tlv.AppendEntry(noRedaction, e)
		}
	}
	_, err = NewPackInfoMinor(h.PackID, h.CaptureID, 0, noRedaction)
	require.NoError(t, err)
	_, err = NewPackInfoMinor(h.PackID, h.CaptureID, 1, noRedaction)
	require.ErrorIs(t, err, tlv.ErrType)

	var seen int
	_, err = r.Iterate(t.Context(), Query{Payloads: true}, func(it *Item) error {
		if it.Record.Seq == 1 {
			seen++
			assert.True(t, it.Record.Quality.Has(QualityUnobserved), "the stored bit 7 is kept")
			assert.False(t, r.Unobserved(&it.Record), "a minor-0 pack gives bit 7 no meaning")
		}

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, seen)
	assert.False(t, r.Unobserved(nil))
}

// The canonical export of a pack of minor 0 holding the numbers format 1.1 assigns is the export tracepack v0.4.0 wrote,
// byte for byte (the tracepack format specification §5, §14 and §15):
// the schema line, the repeated unknown entries in stored order, the nested opaque values and bit(7).
func TestExportMinor0NewNumbers(t *testing.T) {
	t.Parallel()

	file, want := minor0Golden(t)
	var got bytes.Buffer
	_, err := ExportJSONL(t.Context(), mustOpen(t, file, ReaderOptions{}), &got)
	require.NoError(t, err)
	require.Equal(t, string(want), got.String())
}

// encryptedPolicy returns a redaction policy carrying both encryption tags, as the masked-at-write profile requires.
func encryptedPolicy() *RedactionPolicy {
	p := plainPolicy()
	p.EncryptionAlgorithm, p.EncryptionKeyID = new(EncryptionAlgorithmAESSIVCMAC512), new("test-enc-1")

	return p
}

// plainPolicy returns a redaction policy without encryption tags, as an extract's policy is.
func plainPolicy() *RedactionPolicy {
	return &RedactionPolicy{PolicyID: "p", PolicyVersion: 1, KeyID: "test-digest-1", DigestAlgorithm: DigestAlgorithmHMACSHA256}
}

// maskedMeta returns repairTestMeta as the pack metadata of a segment masked at write:
// the capture's one classifier, a policy with encryption, and an entry for each seq of seqs,
// whose entry carries a siv when siv is set.
func maskedMeta(siv bool, seqs ...uint64) *PackMeta {
	m := repairTestMeta()
	m.RedactionPolicy = encryptedPolicy()
	m.Redaction = maskEntries(siv, seqs...)

	return m
}

// extractMinorMeta returns repairTestMeta as the pack metadata of an extract under a policy without encryption,
// with an entry for each seq of seqs.
func extractMinorMeta(seqs ...uint64) *PackMeta {
	m := repairTestMeta()
	m.PackRole, m.ExtractFilter, m.ScopeGeneration = PackRoleExtract, new("S7F3"), nil
	m.RedactionPolicy = plainPolicy()
	m.Redaction = maskEntries(false, seqs...)

	return m
}

// maskEntries returns an entry for each seq of seqs masking the 4 value bytes of redactedRecord's text,
// each carrying a 16-byte siv when siv is set.
func maskEntries(siv bool, seqs ...uint64) []RedactionEntry {
	if len(seqs) == 0 {
		return nil
	}

	entries := make([]RedactionEntry, 0, len(seqs))
	for _, seq := range seqs {
		e := RedactionEntry{
			Seq: seq, ItemPath: new(""), MaskedRanges: []MaskedRange{{Offset: 16, Length: 4}}, Domain: "d", Digest: make([]byte, 32),
		}
		if siv {
			e.SIV, e.HasSIV = bytes.Repeat([]byte{0x5A}, 16), true
		}
		entries = append(entries, e)
	}

	return entries
}

// redactedRecord returns an S7F3 record of seq carrying quality.redacted, its 4-byte ASCII value masked.
func redactedRecord(seq uint64) Record {
	r := frameRecord(seq, hsmsFrame(7, 3, []byte{0x41, 0x04, 0, 0, 0, 0}))
	r.Quality = QualityRedacted

	return r
}

// writeMinorPack writes recs into a new pack under opts and returns its bytes, failing the test on any error.
func writeMinorPack(t *testing.T, opts WriterOptions, recs ...Record) []byte {
	t.Helper()

	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts)
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]), "record %d", i)
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// requireCommitmentRefused checks that NewWriter refuses opts with a *FieldError naming field and wrapping ErrMetadataCommitment,
// writing nothing.
func requireCommitmentRefused(t *testing.T, opts WriterOptions, field string) {
	t.Helper()

	var buf bytes.Buffer
	_, err := NewWriter(&buf, opts)
	require.ErrorIs(t, err, ErrMetadataCommitment)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, field, fe.Field)
	assert.Zero(t, buf.Len())
}

// A Writer writes format_minor 1 exactly when the pack holds a format 1.1 feature, and 0 otherwise
// (the tracepack format specification §14).
func TestWriterFormatMinor(t *testing.T) {
	t.Parallel()

	plain := func(seq uint64) Record { return frameRecord(seq, hsmsFrame(1, 4, []byte{0x21, 0x01, 0x00})) }
	archive := func() *PackMeta { m := repairTestMeta(); archiveMeta(m); return m }
	extractNoPolicy := func() *PackMeta { m := extractMinorMeta(); m.RedactionPolicy = nil; return m }
	// entryOnly is a segment whose one format 1.1 feature is a redaction entry: no policy, siv, thinning or masked record.
	entryOnly := func() *PackMeta { m := repairTestMeta(); m.Redaction = maskEntries(false, 0); return m }
	redacted := PackFacts{AnyRedacted: true}

	tests := []struct {
		name  string
		meta  *PackMeta
		facts PackFacts
		recs  []Record
		minor uint16
	}{
		{"a plain segment", repairTestMeta(), PackFacts{}, []Record{plain(0)}, 0},
		{"an archive", archive(), PackFacts{}, []Record{plain(0)}, 0},
		{"an extract without a policy", extractNoPolicy(), PackFacts{}, []Record{plain(0)}, 0},
		{"an extract under a policy", extractMinorMeta(0), redacted, []Record{redactedRecord(0), plain(1)}, 0},
		{"a segment with thinning", thinnedMeta(), PackFacts{}, []Record{plain(0)}, 1},
		{"a segment under a policy with encryption", maskedMeta(false), PackFacts{}, []Record{plain(0)}, 1},
		{"a segment with an entry and no other feature", entryOnly(), PackFacts{}, nil, 1},
		{"a segment whose entry carries siv", maskedMeta(true, 0), redacted, []Record{redactedRecord(0), plain(1)}, 1},
		{"a segment with an unobserved record", thinnedMeta(), PackFacts{}, []Record{unobservedRecord(0), plain(1)}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file := writeMinorPack(t, WriterOptions{Meta: tt.meta, Facts: tt.facts}, tt.recs...)
			hdr, err := format.UnmarshalFileHeader(file)
			require.NoError(t, err)
			assert.Equal(t, tt.minor, hdr.FormatMinor)
			assert.Equal(t, tt.facts.AnyRedacted, hdr.Flags&fileHeaderRedactionPresent != 0, "redaction-present")

			var export bytes.Buffer
			_, err = ExportJSONL(t.Context(), mustOpen(t, file, ReaderOptions{}), &export)
			require.NoError(t, err)
			schema := "tracepack-jsonl/1"
			if tt.minor != 0 {
				schema = "tracepack-jsonl/2"
			}
			assert.Contains(t, export.String(), fmt.Sprintf(`{"schema":%q,"format_major":1,"format_minor":%d,`, schema, tt.minor))
		})
	}

	t.Run("an extract whose entry carries siv", func(t *testing.T) {
		t.Parallel()
		m := extractMinorMeta()
		m.Redaction = maskEntries(true, 0)
		requireCommitmentRefused(t, WriterOptions{Meta: m, Facts: redacted}, "siv")
	})
}

// Each condition of the format 1.1 feature list (the tracepack format specification §14) selects minor 1 on its own.
// An entry with siv is reachable only here: the Writer refuses siv in an extract,
// and any entry outside an extract already selects minor 1 by the first condition.
func TestFormatMinorOfEachFeature(t *testing.T) {
	t.Parallel()

	extract := func() *PackMeta { m := extractMinorMeta(); m.RedactionPolicy = nil; return m }
	tests := []struct {
		name  string
		meta  func() *PackMeta
		minor uint16
	}{
		{"a segment without a feature", repairTestMeta, 0},
		{"an extract with an entry", func() *PackMeta { m := extract(); m.Redaction = maskEntries(false, 0); return m }, 0},
		{"an extract under a policy without encryption", func() *PackMeta { m := extract(); m.RedactionPolicy = plainPolicy(); return m }, 0},
		{"an entry outside an extract", func() *PackMeta { m := repairTestMeta(); m.Redaction = maskEntries(false, 0); return m }, 1},
		{"a policy with encryption tags", func() *PackMeta { m := extract(); m.RedactionPolicy = encryptedPolicy(); return m }, 1},
		{"a policy with only encryption_key_id", func() *PackMeta {
			m := extract()
			m.RedactionPolicy = plainPolicy()
			m.RedactionPolicy.EncryptionKeyID = new("")

			return m
		}, 1},
		{"an entry with siv", func() *PackMeta { m := extract(); m.Redaction = maskEntries(true, 0); return m }, 1},
		{"an entry with a present empty siv", func() *PackMeta {
			m := extract()
			m.Redaction = maskEntries(false, 0)
			m.Redaction[0].HasSIV = true

			return m
		}, 1},
		{"a thinning entry", func() *PackMeta { m := extract(); m.Thinning = thinnedMeta().Thinning; return m }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.minor, formatMinorOf(tt.meta()))
		})
	}
}

// A pack masked at write, one whose role is not extract and that carries redaction_policy,
// carries the capture's one classifier and at most one max_frame_len (the tracepack storage specification §4, Masking);
// an extract under a policy keeps the rules of format 1.0.
func TestWriterMaskedAtWriteProfile(t *testing.T) {
	t.Parallel()

	with := func(edit func(m *PackMeta)) *PackMeta { m := maskedMeta(false); edit(m); return m }
	requireCommitmentRefused(t, WriterOptions{Meta: with(func(m *PackMeta) { m.RedactionPolicy = plainPolicy() })}, "encryption_algorithm")
	requireCommitmentRefused(t, WriterOptions{Meta: with(func(m *PackMeta) { m.Classifiers = []string{"a", "b"} })}, "classifier")
	requireCommitmentRefused(t, WriterOptions{Meta: with(func(m *PackMeta) { m.Classifiers = nil })}, "classifier")
	requireCommitmentRefused(t, WriterOptions{Meta: with(func(m *PackMeta) { m.MaxFrameLens = []uint64{100, 4096} })}, "max_frame_len")

	plain := frameRecord(0, hsmsFrame(1, 4, nil))
	writeMinorPack(t, WriterOptions{Meta: maskedMeta(false)}, plain)
	writeMinorPack(t, WriterOptions{Meta: with(func(m *PackMeta) { m.MaxFrameLens = []uint64{4096} })}, plain)

	several := extractMinorMeta()
	several.Classifiers = []string{"a", "b"}
	writeMinorPack(t, WriterOptions{Meta: several}, plain)
	none := extractMinorMeta()
	none.Classifiers = nil
	unclassified := plain
	unclassified.DecodeStatus = DecodeStatusNotAttempted
	writeMinorPack(t, WriterOptions{Meta: none}, unclassified)
}

// A redacted record outside an extract is accepted only in a pack masked at write, whose header sets redaction-present
// (the tracepack format specification §4); elsewhere it is refused as before.
func TestWriterRedactedOutsideExtract(t *testing.T) {
	t.Parallel()

	file := writeMinorPack(t, WriterOptions{Meta: maskedMeta(true, 0, 2), Facts: PackFacts{AnyRedacted: true}},
		redactedRecord(0), frameRecord(1, hsmsFrame(1, 4, nil)), redactedRecord(2))
	r := mustOpen(t, file, ReaderOptions{})
	assert.Equal(t, fileHeaderRedactionPresent, r.Header().Flags)

	unmasked := repairTestMeta()
	unmasked.Redaction = maskEntries(false, 0)
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: unmasked})
	require.NoError(t, err)
	rec := redactedRecord(0)
	err = w.Append(&rec)
	require.ErrorIs(t, err, ErrMetadataCommitment)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "pack_role", fe.Field)

	requireCommitmentRefused(t, WriterOptions{Meta: unmasked, Facts: PackFacts{AnyRedacted: true}}, "redaction-present")
}

// A pack masked at write carries both encryption tags, and an extract's policy neither (the tracepack format specification §5):
// the Writer refuses any other policy.
func TestWriterEncryptionTags(t *testing.T) {
	t.Parallel()

	alg, key := new(EncryptionAlgorithmAESSIVCMAC512), new("test-enc-1")
	tags := func(m *PackMeta, a *EncryptionAlgorithm, k *string) *PackMeta {
		m.RedactionPolicy.EncryptionAlgorithm, m.RedactionPolicy.EncryptionKeyID = a, k

		return m
	}

	requireCommitmentRefused(t, WriterOptions{Meta: tags(maskedMeta(false), nil, nil)}, "encryption_algorithm")
	requireCommitmentRefused(t, WriterOptions{Meta: tags(maskedMeta(false), alg, nil)}, "encryption_key_id")
	requireCommitmentRefused(t, WriterOptions{Meta: tags(maskedMeta(false), nil, key)}, "encryption_algorithm")
	writeMinorPack(t, WriterOptions{Meta: tags(maskedMeta(false), alg, key)})

	requireCommitmentRefused(t, WriterOptions{Meta: tags(extractMinorMeta(), alg, nil)}, "encryption_algorithm")
	requireCommitmentRefused(t, WriterOptions{Meta: tags(extractMinorMeta(), nil, key)}, "encryption_key_id")
	requireCommitmentRefused(t, WriterOptions{Meta: tags(extractMinorMeta(), alg, key)}, "encryption_algorithm")
	writeMinorPack(t, WriterOptions{Meta: tags(extractMinorMeta(), nil, nil)})
}

// The masked-at-write profile is given-seq only: its entries and their associated data bind each record's seq
// before the Writer sees the record, so the Writer neither assigns seqs nor inserts clock-step records.
func TestWriterMaskedGivenSeq(t *testing.T) {
	t.Parallel()

	clock := func(m *PackMeta) *PackMeta {
		m.TimeSource = TimeSourceCaptureClock
		m.CaptureOriginUTCNs, m.CaptureOriginMonoNs, m.ClockStepToleranceNs = new(blockTestHour), new(int64(0)), new(uint64(1000))

		return m
	}
	for _, opts := range []WriterOptions{
		{Meta: maskedMeta(false), AssignSeq: true},
		{Meta: clock(maskedMeta(false)), AssignSeq: true, DetectClockSteps: true},
	} {
		var buf bytes.Buffer
		_, err := NewWriter(&buf, opts)
		require.Error(t, err)
		assert.Zero(t, buf.Len())
	}

	writeMinorPack(t, WriterOptions{Meta: extractMinorMeta(), AssignSeq: true})
	writeMinorPack(t, WriterOptions{Meta: clock(extractMinorMeta()), AssignSeq: true, DetectClockSteps: true})
}

// Each presence state of the encryption tags and of siv survives encoding, decoding, the Reader's accessors and a copy
// (the tracepack format specification §5): neither, either one or both tags, whatever their values, and no, an empty or a 16-byte siv.
func TestRedactionPresence(t *testing.T) {
	t.Parallel()

	policies := []struct {
		name string
		alg  *EncryptionAlgorithm
		key  *string
	}{
		{"neither", nil, nil},
		{"algorithm only", new(EncryptionAlgorithmAESSIVCMAC512), nil},
		{"algorithm 0 only", new(EncryptionAlgorithmUnknown), nil},
		{"key id only", nil, new("k")},
		{"empty key id only", nil, new("")},
		{"both", new(EncryptionAlgorithmAESSIVCMAC512), new("k")},
		{"both zero", new(EncryptionAlgorithmUnknown), new("")},
	}
	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			m := maskedMeta(false, 0, 1, 2)
			m.RedactionPolicy.EncryptionAlgorithm, m.RedactionPolicy.EncryptionKeyID = p.alg, p.key
			m.Redaction[1].HasSIV = true
			m.Redaction[2].SIV, m.Redaction[2].HasSIV = []byte("0123456789abcdef"), true
			raw, err := m.MarshalBinary()
			require.NoError(t, err)

			decoded, err := UnmarshalPackMetaMinor(raw, 1)
			require.NoError(t, err)
			assert.Equal(t, m, decoded)

			r := mustOpen(t, jsonlHeadOnlyPack(format.FileHeader{FormatMajor: 1, FormatMinor: 1}, raw), ReaderOptions{})
			assert.Equal(t, m, r.Header().Meta)
			info := r.Info()
			assert.Equal(t, m, info.Meta())
			assert.Equal(t, m, info.Meta())

			cp := *r.Header().Meta
			again, err := cp.MarshalBinary()
			require.NoError(t, err)
			assert.Equal(t, raw, again, "a copy writes back what was decoded")
		})
	}
}

// The raw-block commitment path a merge uses accepts the blocks of a pack masked at write
// holding redacted records its entries name, and refuses them outside the profile, as Append does.
func TestWriterMaskedRawBlocks(t *testing.T) {
	t.Parallel()

	redacted := PackFacts{AnyRedacted: true}
	src := writeMinorPack(t, WriterOptions{Meta: maskedMeta(true, 0), Facts: redacted},
		redactedRecord(0), frameRecord(1, hsmsFrame(1, 4, nil)))
	_, blocks := readSourceBlocks(t, src)

	archive := maskedMeta(true, 0)
	archiveMeta(archive)
	out := copyBlocks(t, WriterOptions{Meta: archive, Facts: redacted}, blocks)
	h := mustOpen(t, out, ReaderOptions{}).Header()
	assert.Equal(t, uint16(1), h.FormatMinor)
	assert.Equal(t, fileHeaderRedactionPresent, h.Flags)

	unmasked := repairTestMeta()
	archiveMeta(unmasked)
	var buf bytes.Buffer
	w, err := startWriter(&buf, WriterOptions{Meta: unmasked})
	require.NoError(t, err)
	err = w.appendBlock(blocks[0].raw, &blocks[0].sum, nil)
	require.ErrorIs(t, err, ErrMetadataCommitment)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "pack_role", fe.Field)
}
