package tracepack

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireMarkingRefused checks that err refuses a record for its Marking:
// a *FieldError naming redaction, wrapping ErrMetadataCommitment and ErrRedactionDefect.
func requireMarkingRefused(t *testing.T, err error) {
	t.Helper()

	require.ErrorIs(t, err, ErrMetadataCommitment)
	require.ErrorIs(t, err, ErrRedactionDefect)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "redaction", fe.Field)
}

// maskedWriter returns a Writer of a pack masked at write over m, with redaction-present set,
// whose blocks close past threshold bytes, and its output.
func maskedWriter(t *testing.T, m *PackMeta, threshold int) (*Writer, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Meta: m, Facts: PackFacts{AnyRedacted: true}, BlockThreshold: threshold})
	require.NoError(t, err)

	return w, &buf
}

// In a pack masked at write the Writer checks the Marking of every record it appends, both ways
// (the tracepack semantics specification §8): a record carrying quality.redacted that no entry names,
// and a record an entry names that does not carry it, are refused before they take a seq or close a block.
// Close refuses an entry naming a seq no appended record carried.
func TestWriterMarkingAtAppend(t *testing.T) {
	t.Parallel()

	t.Run("refused both ways at append", func(t *testing.T) {
		t.Parallel()

		plain := frameRecord(0, hsmsFrame(1, 4, []byte{0x41, 0x04, 'a', 'b', 'c', 'd'}))
		// The open block holds plain, and any record appended next closes it first.
		w, buf := maskedWriter(t, maskedMeta(true, 1, 3), recordHeaderLen+len(plain.Payload)+1)
		require.NoError(t, w.Append(&plain))
		written, offset := buf.Len(), w.offset

		unnamed := redactedRecord(2)
		requireMarkingRefused(t, w.Append(&unnamed))
		named := frameRecord(1, hsmsFrame(7, 3, []byte{0x41, 0x04, 0, 0, 0, 0}))
		requireMarkingRefused(t, w.Append(&named))
		assert.Equal(t, written, buf.Len(), "no block closed or written")
		assert.Equal(t, offset, w.offset)
		assert.Equal(t, uint64(1), w.nextSeq, "no seq taken")
		assert.Equal(t, [3]uint64{1, 0, 0}, [3]uint64{uint64(w.block.summary.recordCount), w.block.summary.firstSeq, w.block.summary.lastSeq},
			"the refused records are in no block")

		masked := redactedRecord(1)
		require.NoError(t, w.Append(&masked), "the next record's seq is unchanged")
		masked = redactedRecord(3)
		require.NoError(t, w.Append(&masked))
		_, err := w.Close()
		require.NoError(t, err)
		run := iterate(t, mustOpen(t, buf.Bytes(), ReaderOptions{}), Query{})
		assert.Equal(t, []uint64{0, 1, 3}, run.seqs())
	})

	t.Run("the checks of the record and its entries", func(t *testing.T) {
		t.Parallel()

		note := maskedNote(1)
		tests := []struct {
			name string
			meta *PackMeta
			rec  Record
		}{
			{"a data entry without siv under encryption", maskedMeta(false, 1), redactedRecord(1)},
			{"a range inside the frame prefix", func() *PackMeta {
				m := maskedMeta(true, 1)
				m.Redaction[0].MaskedRanges[0].Offset = 10
				return m
			}(), redactedRecord(1)},
			{"a range past the payload", func() *PackMeta {
				m := maskedMeta(true, 1)
				m.Redaction[0].MaskedRanges[0].Length = 5
				return m
			}(), redactedRecord(1)},
			{"an annotation entry with siv", func() *PackMeta {
				m := maskedMeta(false)
				m.Redaction = []RedactionEntry{maskedNoteEntry(1, true)}
				return m
			}(), note},
			{"a transport event carrying redacted", maskedMeta(true, 1), func() Record {
				r := testEventRecord(t, 1, blockTestHour+1, 1, &TransportEvent{Event: EventSocketClose})
				r.Quality = QualityRedacted
				return r
			}()},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				w, buf := maskedWriter(t, tt.meta, 0)
				plain := frameRecord(0, hsmsFrame(1, 4, nil))
				require.NoError(t, w.Append(&plain))
				written := buf.Len()
				requireMarkingRefused(t, w.Append(&tt.rec))
				assert.Equal(t, uint64(1), w.nextSeq, "no seq taken")
				assert.Equal(t, uint32(1), w.block.summary.recordCount, "the refused record is in no block")
				assert.Equal(t, written, buf.Len(), "no block written")
			})
		}

		valid := maskedMeta(false)
		valid.Redaction = []RedactionEntry{maskedNoteEntry(1, false)}
		w, _ := maskedWriter(t, valid, 0)
		plain := frameRecord(0, hsmsFrame(1, 4, nil))
		require.NoError(t, w.Append(&plain))
		require.NoError(t, w.Append(&note), "an annotation entry without siv")
	})

	t.Run("an entry naming no appended record", func(t *testing.T) {
		t.Parallel()

		w, buf := maskedWriter(t, maskedMeta(true, 1, 5), 0)
		for _, r := range []Record{frameRecord(0, hsmsFrame(1, 4, nil)), redactedRecord(1), frameRecord(6, hsmsFrame(1, 4, nil))} {
			require.NoError(t, w.Append(&r))
		}
		_, err := w.Close()
		require.ErrorIs(t, err, ErrMetadataCommitment)
		var fe *FieldError
		require.ErrorAs(t, err, &fe)
		assert.Equal(t, "redaction", fe.Field)
		assert.Contains(t, err.Error(), "seq 5")
		assert.Equal(t, VerifyOutcomeUnfinalized, mustVerify(t, buf.Bytes()).Outcome, "neither footer nor trailer")
		_, err = w.Close()
		require.ErrorIs(t, err, ErrWriterFailed)
	})

	t.Run("entries the pack metadata cannot hold", func(t *testing.T) {
		t.Parallel()

		unsorted := maskedMeta(true, 1, 3)
		unsorted.Redaction[0], unsorted.Redaction[1] = unsorted.Redaction[1], unsorted.Redaction[0]
		empty := maskedMeta(true, 1)
		empty.Redaction[0].MaskedRanges[0].Length = 0
		shortSIV := maskedMeta(true, 1)
		shortSIV.Redaction[0].SIV = shortSIV.Redaction[0].SIV[:15]
		redacted := PackFacts{AnyRedacted: true}
		requireCommitmentRefused(t, WriterOptions{Meta: unsorted, Facts: redacted}, "redaction")
		requireCommitmentRefused(t, WriterOptions{Meta: empty, Facts: redacted}, "redaction")
		requireCommitmentRefused(t, WriterOptions{Meta: shortSIV, Facts: redacted}, "redaction")
		requireCommitmentRefused(t, WriterOptions{Meta: maskedMeta(true, 1)}, "redaction-present")
	})

	t.Run("an extract keeps the rules of format 1.0", func(t *testing.T) {
		t.Parallel()

		// An extract's entry may name a record that does not carry quality.redacted, or none.
		file := writeMinorPack(t, WriterOptions{Meta: extractMinorMeta(0, 1, 7), Facts: PackFacts{AnyRedacted: true}},
			frameRecord(0, hsmsFrame(1, 4, nil)), redactedRecord(1))
		assert.Equal(t, uint16(0), mustOpen(t, file, ReaderOptions{}).Header().FormatMinor)
	})
}

// After NewWriter returns, changing the caller's pack metadata changes neither the metadata written
// nor the Marking checks at append, which follow what was written.
func TestWriterMetadataCopied(t *testing.T) {
	t.Parallel()

	m := maskedMeta(true, 1, 3)
	want := maskedMeta(true, 1, 3)
	w, buf := maskedWriter(t, m, 0)

	m.RedactionPolicy.EncryptionAlgorithm = nil
	*m.RedactionPolicy.EncryptionKeyID = "changed"
	m.Redaction[0].Seq = 2
	m.Redaction[1].MaskedRanges[0] = MaskedRange{Offset: 1, Length: 100}
	m.Redaction[1].SIV[0] ^= 0xFF
	m.Redaction[1].SIV, m.Redaction[1].HasSIV = nil, false
	m.Redaction = append(m.Redaction[:1], RedactionEntry{Seq: 4})

	unnamed := redactedRecord(2)
	requireMarkingRefused(t, w.Append(&unnamed))
	for _, r := range []Record{frameRecord(0, hsmsFrame(1, 4, nil)), redactedRecord(1), redactedRecord(3)} {
		require.NoError(t, w.Append(&r))
	}
	_, err := w.Close()
	require.NoError(t, err, "the entries written name exactly the records appended")

	got := mustOpen(t, buf.Bytes(), ReaderOptions{}).Header().Meta
	assert.Equal(t, want.RedactionPolicy, got.RedactionPolicy)
	assert.Equal(t, want.Redaction, got.Redaction)
}

// A block copied whole counts as carried only the seqs it holds, not the gaps between them:
// Close refuses an entry naming a seq inside a gap of a copied block, writing neither footer nor trailer.
func TestWriterMarkingRawBlocks(t *testing.T) {
	t.Parallel()

	redacted := PackFacts{AnyRedacted: true}
	src := maskedMeta(true, 1, 9)
	src.SeqStart = 1
	file := writeMinorPack(t, WriterOptions{Meta: src, Facts: redacted}, redactedRecord(1), redactedRecord(9))
	_, blocks := readSourceBlocks(t, file)
	require.Len(t, blocks, 1)
	require.Equal(t, [2]uint64{1, 9}, [2]uint64{blocks[0].sum.firstSeq, blocks[0].sum.lastSeq}, "one block with a seq gap")

	copyTo := func(seqs ...uint64) (*Writer, *bytes.Buffer) {
		m := maskedMeta(true, seqs...)
		archiveMeta(m)
		m.SeqStart = 1
		var out bytes.Buffer
		w, err := startWriter(&out, WriterOptions{Meta: m, Facts: redacted})
		require.NoError(t, err)
		require.NoError(t, w.appendBlock(blocks[0].raw, &blocks[0].sum, nil))

		return w, &out
	}

	w, out := copyTo(1, 9)
	_, err := w.Close()
	require.NoError(t, err, "entries for the seqs the block holds")
	assert.Equal(t, VerifyOutcomeFinalizedConsistent, mustVerify(t, out.Bytes()).Outcome)

	w, out = copyTo(1, 5, 9)
	written := out.Len()
	_, err = w.Close()
	require.ErrorIs(t, err, ErrMetadataCommitment)
	require.ErrorIs(t, err, ErrRedactionDefect)
	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	assert.Equal(t, "redaction", fe.Field)
	assert.Contains(t, err.Error(), "seq 5")
	assert.Equal(t, written, out.Len(), "neither footer nor trailer")
	assert.Equal(t, VerifyOutcomeUnfinalized, mustVerify(t, out.Bytes()).Outcome)
}
