package tracepack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// goldenPackID and goldenNow fix the pack_id and the writer_start_utc_ns of the golden Writer pack.
var (
	goldenPackID = UUID{0x70, 0x01, 0x02, 0x03, 0x04, 0x05, 0x76, 0x07, 0x88, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}
	goldenNow    = time.Unix(0, blockTestHour+int64(time.Minute)).UTC()
)

// goldenWriterPack writes the idle and busy benchmark hours' records into one zstd pack each,
// with every identity and time fixed, and mutate applied to the options.
func goldenWriterPack(t testing.TB, busy bool, mutate func(*WriterOptions)) []byte {
	t.Helper()

	recs := perfHour(busy, 0)
	m := readerTestMeta()
	m.PeriodStart, m.PeriodEnd = layoutStart.UnixNano(), layoutStart.Add(time.Hour).UnixNano()
	opts := WriterOptions{
		Meta: m, Facts: PackFacts{AnyClassified: true}, AssignSeq: true,
		PackID: goldenPackID, CaptureID: perfCapture, Now: func() time.Time { return goldenNow },
	}
	if mutate != nil {
		mutate(&opts)
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts)
	require.NoError(t, err)
	for i := range recs {
		require.NoError(t, w.Append(&recs[i]))
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// goldenStepsPack writes the records of seqs 1 to 3000 of the merge test hour, each a payload of its own, into one zstd pack,
// in blocks of at most threshold bytes (zero for the default), with every identity and time fixed.
// Unlike the benchmark hours, these records are the same where int is 32 bits.
func goldenStepsPack(t testing.TB, threshold int, mutate func(*WriterOptions)) []byte {
	t.Helper()

	steps := hourSteps(1, 3000, 1)
	m := readerTestMeta()
	hour := blockTestHour - blockTestHour%int64(time.Hour)
	m.PeriodStart, m.PeriodEnd, m.SeqStart = hour, hour+int64(time.Hour), 1
	opts := WriterOptions{
		Meta: m, Facts: PackFacts{AnyClassified: true}, BlockThreshold: threshold,
		PackID: goldenPackID, CaptureID: perfCapture, Now: func() time.Time { return goldenNow },
	}
	if mutate != nil {
		mutate(&opts)
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts)
	require.NoError(t, err)
	for i := range steps {
		rec := steps[i].rec
		rec.Payload = append(bytes.Clone(rec.Payload), byte(i), byte(i>>8), byte(i*7))
		require.NoError(t, w.Append(&rec))
	}
	_, err = w.Close()
	require.NoError(t, err)

	return buf.Bytes()
}

// goldenMergeCase is one merge whose archive bytes are fixed.
type goldenMergeCase struct {
	name   string
	inputs [][]byte
	mutate func(*MergeOptions)
}

// goldenMergeCases returns merges that resolve overlapping blocks, coalescing them or not, coalesce small ones, and copy blocks verbatim.
func goldenMergeCases(t testing.TB) []goldenMergeCase {
	t.Helper()

	return []goldenMergeCase{
		{"resolved", [][]byte{
			mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 30, 4)),
			mergePackCodec(t, CodecZstd, seg1, hourSteps(18, 40, 4)),
		}, nil},
		{"coalesced", [][]byte{
			mergePackCodec(t, CodecZstd, seg0, hourSteps(1, 24, 2)),
			mergePackCodec(t, CodecNone, seg1, hourSteps(25, 48, 3)),
		}, nil},
		{"resolved uncoalesced", [][]byte{
			mergePackCodec(t, CodecZstd, seg0, hourSteps(10, 30, 4)),
			mergePackCodec(t, CodecZstd, seg1, hourSteps(18, 40, 4)),
		}, func(o *MergeOptions) { o.NoCoalesce = true }},
		{"verbatim", [][]byte{
			mergePackCodec(t, CodecZstd, seg0, hourSteps(1, 24, 2)),
			mergePackCodec(t, CodecZstd, seg1, hourSteps(25, 48, 3)),
		}, func(o *MergeOptions) { o.NoCoalesce = true }},
	}
}

// goldenMerge merges c's inputs with every identity and time fixed, and mutate applied to the options.
func goldenMerge(t testing.TB, c goldenMergeCase, mutate func(*MergeOptions)) []byte {
	t.Helper()

	opts := mergeOpts()
	opts.Now = func() time.Time { return goldenNow }
	if c.mutate != nil {
		c.mutate(&opts)
	}
	if mutate != nil {
		mutate(&opts)
	}
	out, _, err := mergeBytes(t.Context(), inputsOf(c.inputs...), opts)
	require.NoError(t, err)

	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// The packs a Writer and Merge write with the zero-value options are those tracepack/v0.10.0 wrote, byte for byte,
// on every architecture the tests run on:
// the hashes were taken before CompressionLevel existed,
// and are those of the same klauspost/compress version, v1.20.1; an upgrade of it may change them.
func TestDefaultCompressionGoldenBytes(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"writer":                     "7da22d78f4d8ea59a568eb59230a6dac7b7eefeb5dcd3bd6354385eb13b7fe0f",
		"writer 4 KiB":               "10ba79269bbda0a6d869eafae2a0e4253bc16e9eee356923ed2b75bdf622fd0c",
		"merge resolved":             "581c9fe5be5aac03edd883e7d49d4015aeb65379e84616c52b6818ae67e590c0",
		"merge resolved uncoalesced": "20ab5d59997b6513c6c40dcd3852f3d382873172ecd9f1cf5d4c50364ee5441b",
		"merge coalesced":            "ddc2387845e65dda00c8ce86dcdd89c8aa2cd8340a18cb221afa088e10ce0ead",
		"merge verbatim":             "e2937ca146e607cae5ee760d37659e2317b7c93cc63721fe6c59bd6aa7342458",
	}
	got := map[string]string{
		"writer":       sha256Hex(goldenStepsPack(t, 0, nil)),
		"writer 4 KiB": sha256Hex(goldenStepsPack(t, 4<<10, nil)),
	}
	for _, c := range goldenMergeCases(t) {
		got["merge "+c.name] = sha256Hex(goldenMerge(t, c, nil))
	}
	require.Equal(t, want, got)
}
