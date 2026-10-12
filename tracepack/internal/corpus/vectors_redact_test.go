package corpus

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
	"github.com/arloliu/go-secs/tracepack/classify"
	"github.com/arloliu/go-secs/tracepack/internal/marking"
	"github.com/arloliu/go-secs/tracepack/internal/siv"
	"github.com/arloliu/go-secs/tracepack/redact"
)

// builtItems builds the vector id and returns its reader and every item it yields, payloads included.
func builtItems(t *testing.T, id string) (*tracepack.Reader, []tracepack.Item) {
	t.Helper()

	r := recipeByID(t, id)
	b := must(r.Build(r.seed()))(t)
	rd := openPack(t, b.Pack)
	var items []tracepack.Item
	_, err := rd.Iterate(t.Context(), tracepack.Query{Payloads: true}, func(it *tracepack.Item) error {
		c := *it
		c.Record.Payload = bytes.Clone(it.Record.Payload)
		items = append(items, c)

		return nil
	})
	require.NoError(t, err)

	return rd, items
}

// restoreItem restores the masked data record of it, of the pack rd reads, under the first published encryption key.
func restoreItem(t *testing.T, rd *tracepack.Reader, it *tracepack.Item) ([]byte, error) {
	t.Helper()

	h := rd.Header()

	return redact.Restore(h.CaptureID, &it.Record, it.Redaction, h.Meta.RedactionPolicy, PublishedKey(EncryptionKeyID1, siv.KeySize))
}

// TestEncryptedCasesClassify checks the decode_status and trailing_bytes written by hand for each case of redact-encrypted-segment
// against the classifier, on the frames as received, before masking, under the pack's max_frame_len.
func TestEncryptedCasesClassify(t *testing.T) {
	t.Parallel()

	for _, c := range encryptedCases() {
		status, trailing := classify.Frame(c.rec.Payload, redactMaxFrameLen)
		assert.Equal(t, c.rec.DecodeStatus, status, c.id)
		assert.Equal(t, int(c.rec.TrailingBytes), trailing, c.id)
	}
}

// TestEncryptedSegmentRestores checks redact-encrypted-segment against the masking rules of the tracepack semantics specification §8:
// each case is masked as its clause says, keeps its length, decode_status and trailing bytes,
// and every masked data record restores to the record as received.
func TestEncryptedSegmentRestores(t *testing.T) {
	t.Parallel()

	noEntry := []string{
		"s6f11-unmasked", "short-payloads", "no-target-s13f6-empty-list", "no-target-s7f6-denial", "no-target-s7f26-denial",
		"no-target-s7f36-denial", "no-target-zero-length-ppbody", "no-target-header-only", "format22-two-byte",
	}
	wholeText := []string{
		"whole-text-invalid", "whole-text-trailing", "whole-text-unresolved", "s13f6-not-list", "s16f15-missing-branch", "format22-one-byte",
	}
	rd, items := builtItems(t, encryptedSegmentVector)
	cases := encryptedCases()
	require.Len(t, items, len(cases))
	for i, c := range cases {
		it := &items[i]
		got := it.Record
		require.Equal(t, c.rec.Seq, got.Seq, c.id)
		assert.Equal(t, len(c.rec.Payload), len(got.Payload), "%s: the payload keeps its length", c.id)
		assert.Equal(t, c.rec.DecodeStatus, got.DecodeStatus, c.id)
		assert.Equal(t, c.rec.TrailingBytes, got.TrailingBytes, c.id)
		if slices.Contains(noEntry, c.id) {
			assert.Nil(t, it.Redaction, c.id)
			assert.Equal(t, c.rec.Payload, got.Payload, "%s: not masked", c.id)

			continue
		}
		require.NotNil(t, it.Redaction, c.id)
		require.False(t, it.Redaction.WhollyMasked, "%s: %v", c.id, it.Redaction.Defects)
		assert.True(t, got.Quality&tracepack.QualityRedacted != 0, c.id)
		assert.Equal(t, got.Payload[:hsmsPrefixLen], c.rec.Payload[:hsmsPrefixLen], "%s: the length field and header are kept", c.id)
		if slices.Contains(wholeText, c.id) {
			require.Len(t, it.Redaction.Entries, 1, c.id)
			e := it.Redaction.Entries[0]
			assert.Nil(t, e.ItemPath, c.id)
			assert.Equal(t, redact.DomainMessageText, e.Domain, c.id)
			assert.Equal(t, []tracepack.MaskedRange{{Offset: hsmsPrefixLen, Length: uint64(len(c.rec.Payload) - hsmsPrefixLen)}}, e.MaskedRanges, c.id)
		}
		restored, err := restoreItem(t, rd, it)
		require.NoError(t, err, c.id)
		assert.Equal(t, c.rec.Payload, restored, "%s: restored", c.id)
	}

	// The S7F3 and the S7F6 of one process program in one domain have equal digests.
	assert.Equal(t, items[0].Redaction.Entries[0].Digest, items[1].Redaction.Entries[0].Digest)
	assert.NotEqual(t, items[0].Redaction.Entries[0].SIV, items[1].Redaction.Entries[0].SIV)
	// The already-zero PPBODY is listed and restored as zeros.
	last := items[len(items)-1]
	require.Len(t, last.Redaction.Entries, 1)
	assert.Equal(t, []tracepack.MaskedRange{{Offset: ppbodyOffset, Length: 16}}, last.Redaction.Entries[0].MaskedRanges)
}

// targetWant is the hand-derived Marking of a resolved item mask: an entry's concrete item path and its complete range map.
type targetWant struct {
	path   string
	ranges []tracepack.MaskedRange
}

// TestResolvedMasksTargetBytes checks the target bytes of every resolved item mask of redact-encrypted-segment
// against ranges derived by hand from each case's message text (the tracepack semantics specification §8 Masking):
// the content bytes of every leaf the rule's path selects, and nothing else.
// Every byte outside the ranges, each item and list header and a format-22 leaf's 2-byte localized header included,
// equals the record as received, and every range holds ciphertext.
func TestResolvedMasksTargetBytes(t *testing.T) {
	t.Parallel()

	at := func(off, n uint64) tracepack.MaskedRange { return tracepack.MaskedRange{Offset: off, Length: n} }
	ppbody := []targetWant{{"2", []tracepack.MaskedRange{at(24, 16)}}}
	want := map[string][]targetWant{
		"ppbody":            ppbody,
		"s7f6-same-program": ppbody,
		"declared-short":    ppbody,
		"declared-long":     ppbody,
		"already-zero":      ppbody,
		// L,4 <A PP-2> <A MDL-1> <A 1.0> L,2 {L,2 <U2 1> L,2 {<A P1> <A P2>}} {L,2 <U2 2> L,1 {<A P3>}}:
		// path 4 is the list at offset 34; its leaves' contents are the two CCODEs and the three PPARMs, two bytes each.
		"s7f23": {{"4", []tracepack.MaskedRange{at(40, 2), at(46, 2), at(50, 2), at(56, 2), at(62, 2)}}},
		// L,2 <A PP-3> <B, two length bytes, 300 bytes>: the content follows a 3-byte header at offset 22.
		"oversized": {{"2", []tracepack.MaskedRange{at(25, 300)}}},
		// L,5 <U4> <A PJ-1> <B> L,0 L,3 {<A RCP-1> <U1> L,2 {L,2 <A P1> <W 0 2 0 5>} {L,2 <A P2> <U4 30>}}:
		// the localized leaf's header at 53, its encoding at 55 and its content at 57; the U4's content at 69.
		"format22-header-kept": {
			{"5/3/1/2", []tracepack.MaskedRange{at(57, 4)}},
			{"5/3/2/2", []tracepack.MaskedRange{at(69, 4)}},
		},
		// L,4 <A DS-3> <U4> <U4> L,2 {<B chunk-1> <B chunk-2>}: the two contents at 38 and 47.
		"s13f6-fildat": {{"4/1", []tracepack.MaskedRange{at(38, 7)}}, {"4/2", []tracepack.MaskedRange{at(47, 7)}}},
	}

	_, items := builtItems(t, encryptedSegmentVector)
	seen := 0
	for i, c := range encryptedCases() {
		w, ok := want[c.id]
		if !ok {
			continue
		}
		seen++
		it := &items[i]
		require.NotNil(t, it.Redaction, c.id)
		got := make([]targetWant, 0, len(it.Redaction.Entries))
		target := make([]bool, len(c.rec.Payload))
		for _, e := range it.Redaction.Entries {
			require.NotNil(t, e.ItemPath, c.id)
			got = append(got, targetWant{*e.ItemPath, e.MaskedRanges})
			for _, r := range e.MaskedRanges {
				for k := r.Offset; k < r.Offset+r.Length; k++ {
					target[k] = true
				}
				assert.NotEqual(t, c.rec.Payload[r.Offset:r.Offset+r.Length], it.Record.Payload[r.Offset:r.Offset+r.Length],
					"%s: range %d+%d holds ciphertext", c.id, r.Offset, r.Length)
			}
		}
		require.Equal(t, w, got, c.id)
		for k, b := range it.Record.Payload {
			if !target[k] {
				assert.Equal(t, c.rec.Payload[k], b, "%s: byte %d is no target byte", c.id, k)
			}
		}
	}
	require.Equal(t, len(want), seen, "every case named is a case of the vector")
}

// TestCaptureVectorsCompare compares redact-capture-a and redact-capture-b, which pin their own bytes:
// one PPBODY in two captures has equal digests and different ciphertexts and sivs, and each restores.
func TestCaptureVectorsCompare(t *testing.T) {
	t.Parallel()

	rdA, a := builtItems(t, captureVectorA)
	rdB, b := builtItems(t, captureVectorB)
	require.NotEqual(t, rdA.Header().CaptureID, rdB.Header().CaptureID)
	ea, eb := a[1].Redaction.Entries[0], b[1].Redaction.Entries[0]
	assert.Equal(t, ea.Digest, eb.Digest)
	assert.NotEqual(t, ea.SIV, eb.SIV)
	assert.NotEqual(t, a[1].Record.Payload, b[1].Record.Payload)
	for _, x := range []struct {
		rd *tracepack.Reader
		it *tracepack.Item
	}{{rdA, &a[1]}, {rdB, &b[1]}} {
		restored, err := restoreItem(t, x.rd, x.it)
		require.NoError(t, err)
		assert.Equal(t, singlePPBODY()[0][1].Payload, restored)
	}
}

// TestDefectVectorsMasked checks that the reader reports each defect of redact-recoverable-defects and redact-overlapping-encrypted
// as the tracepack semantics specification §8 Marking says:
// the record of every defect case wholly masked for its own defect and no other,
// so each case isolates the check it names, and the valid record before them and the adjacent entries not masked at all.
func TestDefectVectorsMasked(t *testing.T) {
	t.Parallel()

	want := map[string]error{
		"empty-ranges":           marking.ErrRangesEmpty,
		"odd-ranges":             marking.ErrRangesMalformed,
		"zero-length":            marking.ErrRangeLength,
		"unsorted-ranges":        marking.ErrRangeOrder,
		"adjacent-ranges":        marking.ErrRangeOrder,
		"range-past-payload":     marking.ErrRangeOutside,
		"entry-without-redacted": marking.ErrRedactedBit,
		"redacted-without-entry": marking.ErrRedactedBit,
		"missing-siv":            marking.ErrSIVMissing,
		"ill-sized-siv":          marking.ErrSIVLength,
		"overlap":                marking.ErrEncryptedOverlap,
		"duplicate":              marking.ErrEncryptedOverlap,
	}
	for _, v := range []struct {
		id    string
		cases []defectCase
	}{
		{"redact-recoverable-defects", recoverableDefectCases()},
		{"redact-overlapping-encrypted", overlappingCases()},
	} {
		rd, items := builtItems(t, v.id)
		require.Len(t, items, len(v.cases), v.id)
		for i, c := range v.cases {
			it := &items[i]
			require.NotNil(t, it.Redaction, "%s %s", v.id, c.id)
			defect, isDefect := want[c.id]
			assert.Equal(t, isDefect, it.Redaction.WhollyMasked, "%s %s: %v", v.id, c.id, it.Redaction.Defects)
			if !isDefect {
				assert.Empty(t, it.Redaction.Defects, c.id)
				restored, err := restoreItem(t, rd, it)
				require.NoError(t, err, c.id)
				assert.Equal(t, s7f3(uint64(i), ppbodyText(), tracepack.DecodeStatusOK).Payload, restored, c.id)

				continue
			}
			require.NotEmpty(t, it.Redaction.Defects, c.id)
			for _, d := range it.Redaction.Defects {
				assert.ErrorIs(t, d.Err, tracepack.ErrRedactionDefect, c.id)
				assert.ErrorIs(t, d.Err, defect, "%s: only its own defect, %v", c.id, d.Err)
			}
		}
	}
}

// TestEncryptedArchiveRestores checks that redact-encrypted-archive carries every record of both segments once,
// masked as redact-encrypted-segment's records are, and that each restores.
func TestEncryptedArchiveRestores(t *testing.T) {
	t.Parallel()

	rd, items := builtItems(t, "redact-encrypted-archive")
	cases := encryptedCases()
	require.Len(t, items, len(cases))
	for i, it := range items {
		if it.Redaction == nil {
			assert.Equal(t, cases[i].rec.Payload, it.Record.Payload, cases[i].id)
			continue
		}
		restored, err := restoreItem(t, rd, &it)
		require.NoError(t, err, cases[i].id)
		assert.Equal(t, cases[i].rec.Payload, restored, cases[i].id)
	}
}

// TestDamagedVectorsMaskedAndThinned checks that the base pack of redact-truncation and redact-corrupt-block holds
// both format 1.1 cases: encrypted records and records unobserved by design under thinning entries;
// and that the corrupt block loses only its own records, the others keeping their Marking.
func TestDamagedVectorsMaskedAndThinned(t *testing.T) {
	t.Parallel()

	rd, items := builtItems(t, "redact-truncation")
	require.NotEmpty(t, rd.Header().Meta.Thinning)
	var masked, unobserved int
	for i := range items {
		if items[i].Redaction != nil {
			masked++
			_, err := restoreItem(t, rd, &items[i])
			require.NoError(t, err)
		}
		if rd.Unobserved(&items[i].Record) {
			unobserved++
		}
	}
	assert.Equal(t, 3, masked)
	assert.Equal(t, 3, unobserved)

	rd, items = builtItems(t, "redact-corrupt-block")
	seqs := make([]uint64, 0, len(items))
	for i := range items {
		seqs = append(seqs, items[i].Record.Seq)
		assert.Equal(t, items[i].Record.Seq%2 == 0, items[i].Redaction != nil)
		assert.Equal(t, items[i].Record.Seq%2 == 1, rd.Unobserved(&items[i].Record))
	}
	assert.Equal(t, []uint64{0, 1, 4, 5}, seqs)
}
