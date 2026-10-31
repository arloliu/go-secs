package tracepack_test

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/bus"
	"github.com/arloliu/go-secs/tracepack/classify"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// mergeClassifier is the classifier name of the masked merge tests' segments.
const mergeClassifier = "consumer-classifier/1"

// mergePolicy returns the policy of the masked merge tests at version: the PPID and the PPBODY of S7F3, under encryption,
// so a record whose text is one valid item has two entries.
func mergePolicy(t *testing.T, version uint64) *redact.Compiled {
	t.Helper()

	c, err := redact.Compile(redact.Policy{
		ID: "recipes", Version: version, DigestKeyID: "digest-1", Encryption: &redact.Encryption{KeyID: "enc-1"},
		ItemRules: []redact.ItemRule{
			{Stream: 7, Function: 3, Path: "1", Domain: "ppid"},
			{Stream: 7, Function: 3, Path: "2", Domain: "ppbody"},
		},
	})
	require.NoError(t, err)

	return c
}

// segmentSpec is a segment of consumerCapture that the masked merge tests write.
type segmentSpec struct {
	// first is the seq of frames[0], and frames the payloads of its data records, one per seq.
	first  uint64
	frames [][]byte
	// policy masks every record when set; nil writes an unmasked segment.
	policy *redact.Compiled
	// classifier names the capture's classifier, mergeClassifier when empty;
	// maxFrameLen, when not 0, is its limit and the segment's max_frame_len.
	classifier  string
	maxFrameLen uint64
	// perBlock is the number of records of each block, the last block holding the rest.
	perBlock int
}

// writeSegment writes the segment s describes, as a consumer of a durable bus writes it,
// its pack metadata padded for tracepack.RewriteMeta:
// each record checked, derived with the classifier and, under a policy, masked,
// its entries ordered by seq then first masked offset.
func writeSegment(t *testing.T, s segmentSpec) []byte {
	t.Helper()

	cls := classify.NewNamed(cmp.Or(s.classifier, mergeClassifier), s.maxFrameLen)
	recs := make([]tracepack.Record, 0, len(s.frames))
	var entries []tracepack.RedactionEntry
	for _, msg := range busBatch(t, s.first, s.frames...) {
		br, err := bus.CheckMessage(msg.env, msg.body)
		require.NoError(t, err)
		require.NoError(t, bus.DeriveRecord(&br.Record, cls))
		rec := br.Record
		if s.policy != nil {
			masked, es, err := s.policy.Mask(t.Context(), br.CaptureID, &br.Record, false, consumerKeys{})
			require.NoError(t, err)
			rec, entries = masked, append(entries, es...)
		}
		recs = append(recs, rec)
	}
	slices.SortStableFunc(entries, func(a, b tracepack.RedactionEntry) int {
		return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.MaskedRanges[0].Offset, b.MaskedRanges[0].Offset))
	})

	m := tracepack.PadMeta(hourMeta())
	m.SeqStart, m.Classifiers = s.first, []string{cls.Name()}
	if s.maxFrameLen > 0 {
		m.MaxFrameLens = []uint64{s.maxFrameLen}
	}
	facts := tracepack.PackFacts{AnyClassified: true}
	if s.policy != nil {
		policy := s.policy.PackPolicy()
		m.RedactionPolicy, m.Redaction = &policy, entries
		facts.AnyRedacted, facts.RedactionPolicyApplied = len(entries) > 0, true
	}
	var buf bytes.Buffer
	w, err := tracepack.NewWriter(&buf, tracepack.WriterOptions{Meta: m, CaptureID: consumerCapture, Facts: facts})
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]))
		if (i+1)%s.perBlock == 0 {
			require.NoError(t, w.Flush())
		}
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// mergeInputs returns files as merge inputs, in their order.
func mergeInputs(files ...[]byte) []tracepack.MergeInput {
	inputs := make([]tracepack.MergeInput, len(files))
	for i, f := range files {
		inputs[i] = tracepack.MergeInput{ReaderAt: bytes.NewReader(f), Size: int64(len(f))}
	}

	return inputs
}

// maskedMergeOpts returns the options of the masked merge tests, every conflict appended to conflicts.
func maskedMergeOpts(conflicts *[]tracepack.Conflict) tracepack.MergeOptions {
	return tracepack.MergeOptions{
		WriterTag: "merge-test", ScopeGeneration: 1, PublisherEpoch: 1,
		OnConflict: func(c tracepack.Conflict) { *conflicts = append(*conflicts, c) },
	}
}

// countingWriter counts the calls to its Write, so a test can require that Merge never touched its output.
type countingWriter struct {
	calls int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.calls++

	return len(p), nil
}

// requireRefusedUntouched requires a merge of files to fail with an error wrapping want and holding msg
// before it touches its output.
// It returns the conflicts the merge reported.
func requireRefusedUntouched(t *testing.T, want error, msg string, files ...[]byte) []tracepack.Conflict {
	t.Helper()

	var conflicts []tracepack.Conflict
	var dst countingWriter
	rep, err := tracepack.Merge(t.Context(), &dst, mergeInputs(files...), maskedMergeOpts(&conflicts))
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, msg)
	assert.Zero(t, dst.calls, "the output is untouched")
	assert.Equal(t, tracepack.MergeReport{Conflicts: len(conflicts)}, rep)

	return conflicts
}

// requireRefusedUnfinalized requires a merge of files, without coalescing when noCoalesce is set,
// to fail with an error wrapping want and holding msg,
// leaving in its output no finalized pack and a report that holds nothing.
func requireRefusedUnfinalized(t *testing.T, want error, msg string, noCoalesce bool, files ...[]byte) {
	t.Helper()

	var conflicts []tracepack.Conflict
	var out bytes.Buffer
	opts := maskedMergeOpts(&conflicts)
	opts.NoCoalesce = noCoalesce
	rep, err := tracepack.Merge(t.Context(), &out, mergeInputs(files...), opts)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, msg)
	assert.Equal(t, tracepack.MergeReport{}, rep)
	assert.Empty(t, conflicts)
	if out.Len() > 0 {
		v, err := tracepack.Verify(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), tracepack.VerifyOptions{})
		require.NoError(t, err)
		assert.Equal(t, tracepack.VerifyOutcomeUnfinalized, v.Outcome, "nothing published")
	}
}

// mergedSegments merges files with opts and returns the archive and the report, failing the test on error.
func mergedSegments(t *testing.T, opts tracepack.MergeOptions, files ...[]byte) ([]byte, tracepack.MergeReport) {
	t.Helper()

	var conflicts []tracepack.Conflict
	o := maskedMergeOpts(&conflicts)
	o.NoCoalesce, o.BlockThreshold = opts.NoCoalesce, opts.BlockThreshold
	var out bytes.Buffer
	rep, err := tracepack.Merge(t.Context(), &out, mergeInputs(files...), o)
	require.NoError(t, err)
	require.Empty(t, conflicts)

	return out.Bytes(), rep
}

// entriesOf returns the redaction entries of the pack file.
func entriesOf(t *testing.T, file []byte) []tracepack.RedactionEntry {
	t.Helper()

	h, _ := readPack(t, file)

	return h.Meta.Redaction
}

// insertEntry returns entries with e inserted where the order by seq then first masked offset puts it, after equal ones.
func insertEntry(entries []tracepack.RedactionEntry, e tracepack.RedactionEntry) []tracepack.RedactionEntry {
	i, _ := slices.BinarySearchFunc(entries, e, func(a, b tracepack.RedactionEntry) int {
		if c := cmp.Compare(a.Seq, b.Seq); c != 0 {
			return c
		}
		if a.MaskedRanges[0].Offset <= b.MaskedRanges[0].Offset {
			return -1
		}

		return 1
	})

	return slices.Insert(slices.Clone(entries), i, e)
}

// lastEntryOf returns a copy of the last entry of entries naming seq.
func lastEntryOf(t *testing.T, entries []tracepack.RedactionEntry, seq uint64) tracepack.RedactionEntry {
	t.Helper()

	i := slices.IndexFunc(entries, func(e tracepack.RedactionEntry) bool { return e.Seq > seq })
	if i < 0 {
		i = len(entries)
	}
	require.Positive(t, i)
	e := entries[i-1]
	require.Equal(t, seq, e.Seq)
	e.MaskedRanges, e.Digest, e.SIV = slices.Clone(e.MaskedRanges), slices.Clone(e.Digest), slices.Clone(e.SIV)

	return e
}

// Two masked segments holding one record in both merge into an archive that carries the policy, its one classifier,
// one entry per mask in seq then offset order and redaction-present, at format_minor 1,
// whose every record restores to its original, through copied, resolved and coalesced blocks
// (the tracepack storage specification §4 Merge).
func TestMergeMasked(t *testing.T) {
	t.Parallel()

	policy := mergePolicy(t, 1)
	s1f1 := busFrame(1, 1, nil)
	frames := [][]byte{
		s1f1,
		ppbody("PP1", []byte("RECIPE-ONE")),
		s1f1,
		ppbody("PP3", []byte("SHARED-RECIPE")),
		ppbody("PP0", make([]byte, 8)), // a PPBODY already zero
		ppbody("PPW", []byte("WHOLE-TEXT"), 0xFF, 0xFF), // not one valid item: a whole-text mask
		s1f1,
		ppbody("PP7", []byte("LAST-RECIPE")),
	}
	a := writeSegment(t, segmentSpec{first: 0, frames: frames[:4], policy: policy, perBlock: 2})
	b := writeSegment(t, segmentSpec{first: 3, frames: frames[3:], policy: policy, perBlock: 2})
	ea, eb := entriesOf(t, a), entriesOf(t, b)
	want := slices.Concat(ea, slices.DeleteFunc(slices.Clone(eb), func(e tracepack.RedactionEntry) bool { return e.Seq <= 3 }))
	require.Len(t, want, 9, "two entries for each of the four valid S7F3 and one for the whole text")

	tests := []struct {
		name  string
		opts  tracepack.MergeOptions
		check func(t *testing.T, rep tracepack.MergeReport)
	}{
		{name: "copied and resolved", opts: tracepack.MergeOptions{NoCoalesce: true}, check: func(t *testing.T, rep tracepack.MergeReport) {
			t.Helper()
			assert.Equal(t, 3, rep.Copied)
			assert.Equal(t, 2, rep.Resolved)
		}},
		{name: "coalesced", check: func(t *testing.T, rep tracepack.MergeReport) {
			t.Helper()
			assert.Positive(t, rep.CoalescedEncodings)
			assert.Zero(t, rep.Copied)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, rep := mergedSegments(t, tt.opts, a, b)
			tt.check(t, rep)
			h, recs := readPack(t, out)
			assert.Equal(t, uint16(1), h.FormatMinor)
			assert.Equal(t, uint32(1), h.Flags, "redaction-present")
			pp := policy.PackPolicy()
			assert.Equal(t, &pp, h.Meta.RedactionPolicy)
			assert.Equal(t, []string{mergeClassifier}, h.Meta.Classifiers)
			assert.Nil(t, h.Meta.MaxFrameLens)
			assert.Equal(t, want, h.Meta.Redaction, "every entry once, in seq then offset order")
			assert.Empty(t, h.RedactionDefects)

			require.Len(t, recs, len(frames))
			for _, rb := range recs {
				frame := frames[rb.rec.Seq]
				if rb.redaction == nil {
					assert.Equal(t, frame, rb.rec.Payload, "seq %d", rb.rec.Seq)
					continue
				}
				assert.Empty(t, rb.redaction.Defects, "seq %d", rb.rec.Seq)
				assert.NotEqual(t, frame, rb.rec.Payload, "seq %d is masked", rb.rec.Seq)
				assert.Equal(t, frame, restore(t, h, rb), "seq %d restores", rb.rec.Seq)
			}
			whole := recs[5].redaction
			require.NotNil(t, whole)
			require.Len(t, whole.Entries, 1)
			assert.Nil(t, whole.Entries[0].ItemPath, "a whole-text mask")
		})
	}
}

// A merge refuses masked inputs whose Marking is defective, whose entries for one record differ,
// or that differ from each other in policy, classifier or max_frame_len, and a masked input beside an unmasked one,
// publishing nothing (the tracepack storage specification §4 Merge).
func TestMergeMaskedRefusals(t *testing.T) {
	t.Parallel()

	policy := mergePolicy(t, 1)
	s1f1 := busFrame(1, 1, nil)
	// An S1F3 whose message text the policy does not mask: a list of two U4 items.
	s1f3 := busFrame(1, 3, secsList(secsItem(0o54, 0, 0, 0, 1), secsItem(0o54, 0, 0, 0, 2)))
	frames := [][]byte{
		s1f3, ppbody("PP1", []byte("RECIPE-ONE")), s1f3, ppbody("PP3", []byte("SHARED-RECIPE")),
		s1f1, ppbody("PP5", []byte("RECIPE-FIVE")), s1f1,
	}
	// A holds seqs 0 to 3 and B seqs 3 to 6, in blocks of two records:
	// A's block of seqs 0 and 1 is copied, A's block of seqs 2 and 3 and B's of seqs 3 and 4 are resolved.
	specA := segmentSpec{first: 0, frames: frames[:4], policy: policy, perBlock: 2}
	specB := segmentSpec{first: 3, frames: frames[3:], policy: policy, perBlock: 2}
	a, b := writeSegment(t, specA), writeSegment(t, specB)
	edit := func(file []byte, fn func(m *tracepack.PackMeta)) []byte { return tracepack.RewriteMeta(t, file, fn) }

	// clearBitEntry gives the S1F3 of seq, whose bit is clear, an entry masking its whole message text:
	// a copy of seq 1's last entry with ranges that fit the record, so the bit is the record's one defect.
	clearBitEntry := func(seq uint64) func(m *tracepack.PackMeta) {
		return func(m *tracepack.PackMeta) {
			e := lastEntryOf(t, m.Redaction, 1)
			e.Seq, e.ItemPath = seq, nil
			e.MaskedRanges = []tracepack.MaskedRange{{Offset: 14, Length: uint64(len(s1f3) - 14)}}
			m.Redaction = insertEntry(m.Redaction, e)
		}
	}
	// overlapping gives the record of seq a second encrypted entry overlapping its last.
	overlapping := func(seq uint64) func(m *tracepack.PackMeta) {
		return func(m *tracepack.PackMeta) {
			e := lastEntryOf(t, m.Redaction, seq)
			e.MaskedRanges[0].Offset++
			e.MaskedRanges[0].Length--
			m.Redaction = insertEntry(m.Redaction, e)
		}
	}
	// otherDigest changes the digest of the last entry of seq.
	otherDigest := func(seq uint64) func(m *tracepack.PackMeta) {
		return func(m *tracepack.PackMeta) {
			i := slices.IndexFunc(m.Redaction, func(e tracepack.RedactionEntry) bool { return e.Seq == seq })
			m.Redaction[i].Digest = slices.Clone(m.Redaction[i].Digest)
			m.Redaction[i].Digest[0] ^= 1
		}
	}

	t.Run("record checks", func(t *testing.T) {
		t.Parallel()

		const overlap = "the ranges of the record's encrypted entries overlap"
		clearBit := func(seq uint64) string {
			return fmt.Sprintf("seq %d: %v: seq %d, redacted false, 1 entries: %v",
				seq, tracepack.ErrRedactionDefect, seq, "marking: quality.redacted is not set exactly when an entry names the record")
		}
		// The copied cases are merged without coalescing, so the block of seqs 0 and 1 is copied verbatim.
		tests := []struct {
			name       string
			files      [][]byte
			msg        string
			noCoalesce bool
		}{
			{"an entry naming a record whose bit is clear, copied", [][]byte{edit(a, clearBitEntry(0)), b}, "block 0: " + clearBit(0), true},
			{"an entry naming a record whose bit is clear, resolved", [][]byte{edit(a, clearBitEntry(2)), b}, "block 1: " + clearBit(2), false},
			{"overlapping encrypted entries, copied", [][]byte{edit(a, overlapping(1)), b}, "block 0: seq 1: tracepack: redaction defect: seq 1: marking: " + overlap, true},
			{"overlapping encrypted entries, resolved", [][]byte{edit(a, overlapping(3)), edit(b, overlapping(3))}, "seq 3: tracepack: redaction defect: seq 3: marking: " + overlap, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				h, _ := readPack(t, tt.files[0])
				assert.Empty(t, h.RedactionDefects, "a defect found only when the record is read")
				requireRefusedUnfinalized(t, tracepack.ErrMergeInput, tt.msg, tt.noCoalesce, tt.files...)
			})
		}
	})

	t.Run("entry checks of the pack metadata", func(t *testing.T) {
		t.Parallel()

		// An entry naming seq 9, which A does not hold: the archive's entries name only the records it writes.
		orphan := edit(a, func(m *tracepack.PackMeta) {
			e := lastEntryOf(t, m.Redaction, 3)
			e.Seq = 9
			m.Redaction = append(m.Redaction, e)
		})
		// Entries out of order, a bootstrap defect.
		unordered := edit(a, func(m *tracepack.PackMeta) {
			n := len(m.Redaction)
			m.Redaction[0], m.Redaction[n-1] = m.Redaction[n-1], m.Redaction[0]
		})
		// An entry in a segment without redaction_policy.
		unmasked := writeSegment(t, segmentSpec{first: 0, frames: frames[:4], perBlock: 2})
		policyless := edit(unmasked, func(m *tracepack.PackMeta) { m.Redaction = entriesOf(t, a)[:1] })
		h, _ := readPack(t, unordered)
		require.NotEmpty(t, h.RedactionDefects)

		requireRefusedUntouched(t, tracepack.ErrMergeInput, "names seq 9, which the pack holds no record of", orphan, b)
		requireRefusedUntouched(t, tracepack.ErrMergeInput, tracepack.ErrRedactionDefect.Error(), unordered, b)
		requireRefusedUntouched(t, tracepack.ErrMergeInput, "redaction entries without redaction_policy", policyless)
	})

	t.Run("equal records with different entries", func(t *testing.T) {
		t.Parallel()

		// A copy of A whose blocks are byte-identical, so they are duplicates, and the overlapping B.
		twin := writeSegment(t, specA)
		for name, files := range map[string][][]byte{
			"duplicate blocks": {a, edit(twin, otherDigest(1))},
			"resolved blocks":  {a, edit(b, otherDigest(3))},
		} {
			conflicts := requireRefusedUntouched(t, tracepack.ErrMergeConflict, "1 conflicts of redaction entries", files...)
			require.Len(t, conflicts, 1, name)
			assert.Equal(t, consumerCapture, conflicts[0].CaptureID, name)
			assert.Len(t, conflicts[0].Versions, 2, name)
		}
	})

	t.Run("inputs that disagree", func(t *testing.T) {
		t.Parallel()

		other := func(fn func(s *segmentSpec)) []byte {
			s := specB
			fn(&s)
			return writeSegment(t, s)
		}
		tests := []struct {
			name string
			b    []byte
			msg  string
		}{
			{"another policy", other(func(s *segmentSpec) { s.policy = mergePolicy(t, 2) }), "redaction_policy differs"},
			{"another classifier", other(func(s *segmentSpec) { s.classifier = "other-classifier/1" }), "classifier differs"},
			{"a max_frame_len in one input only", other(func(s *segmentSpec) { s.maxFrameLen = 1 << 20 }), "max_frame_len differs"},
			{"an unmasked input", other(func(s *segmentSpec) { s.policy = nil }), "redaction_policy beside"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				requireRefusedUntouched(t, tracepack.ErrMergeInput, tt.msg, a, tt.b)
				requireRefusedUntouched(t, tracepack.ErrMergeInput, tt.msg, tt.b, a)
			})
		}
	})
}

// Unmasked inputs of two classifiers merge, the archive carrying both (the tracepack storage specification §4 Merge).
func TestMergeUnmaskedClassifiers(t *testing.T) {
	t.Parallel()

	s1f1 := busFrame(1, 1, nil)
	a := writeSegment(t, segmentSpec{first: 0, frames: [][]byte{s1f1, s1f1}, classifier: "b-classifier/1", perBlock: 2})
	b := writeSegment(t, segmentSpec{first: 2, frames: [][]byte{s1f1, s1f1}, classifier: "a-classifier/1", perBlock: 2})
	out, rep := mergedSegments(t, tracepack.MergeOptions{}, a, b)
	assert.Equal(t, uint64(4), rep.Records)
	h, _ := readPack(t, out)
	assert.Equal(t, uint16(0), h.FormatMinor)
	assert.Equal(t, []string{"a-classifier/1", "b-classifier/1"}, h.Meta.Classifiers)
	assert.Nil(t, h.Meta.RedactionPolicy)
}

// A merge refuses an input outside extracts whose redaction_policy lacks either encryption tag, whatever its minor,
// although a reader opens it with no redaction defect (the tracepack format specification §5).
func TestMergeEncryptionTags(t *testing.T) {
	t.Parallel()

	s1f1 := busFrame(1, 1, nil)
	frames := [][]byte{s1f1, ppbody("PP1", []byte("RECIPE-ONE"))}
	masked := writeSegment(t, segmentSpec{first: 0, frames: frames, policy: mergePolicy(t, 1), perBlock: 2})
	plain := writeSegment(t, segmentSpec{first: 0, frames: frames, perBlock: 2})
	alg := tracepack.EncryptionAlgorithmAESSIVCMAC512
	keyID := "enc-1"

	tests := []struct {
		name  string
		file  []byte
		minor uint16
		msg   string
	}{
		{"neither tag", tracepack.RewriteMeta(t, masked, func(m *tracepack.PackMeta) {
			m.RedactionPolicy.EncryptionAlgorithm, m.RedactionPolicy.EncryptionKeyID = nil, nil
		}), 1, "without encryption_algorithm"},
		{"the algorithm only", tracepack.RewriteMeta(t, masked, func(m *tracepack.PackMeta) {
			m.RedactionPolicy.EncryptionAlgorithm, m.RedactionPolicy.EncryptionKeyID = &alg, nil
		}), 1, "without encryption_key_id"},
		{"the key id only", tracepack.RewriteMeta(t, masked, func(m *tracepack.PackMeta) {
			m.RedactionPolicy.EncryptionAlgorithm, m.RedactionPolicy.EncryptionKeyID = nil, &keyID
		}), 1, "without encryption_algorithm"},
		// A segment of minor 0 under a policy without encryption, as tracepack v0.4.0 wrote one whose policy matched nothing.
		{"a minor-0 segment", tracepack.RewriteMeta(t, plain, func(m *tracepack.PackMeta) {
			m.RedactionPolicy = &tracepack.RedactionPolicy{
				PolicyID: "recipes", PolicyVersion: 1, KeyID: "digest-1", DigestAlgorithm: tracepack.DigestAlgorithmHMACSHA256,
			}
		}), 0, "without encryption_algorithm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h, _ := readPack(t, tt.file)
			assert.Equal(t, tt.minor, h.FormatMinor)
			require.NotNil(t, h.Meta.RedactionPolicy)
			assert.Empty(t, h.RedactionDefects, "no reader defect")
			requireRefusedUntouched(t, tracepack.ErrMergeInput, tt.msg, tt.file)
		})
	}
}
