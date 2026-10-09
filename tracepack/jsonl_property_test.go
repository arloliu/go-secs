package tracepack

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// jsonlOracleEnum returns the JSON name of the enum value v under names (the tracepack JSONL specification §3).
func jsonlOracleEnum(names []string, v uint8) string {
	if int(v) < len(names) {
		return names[v]
	}

	return fmt.Sprintf("unknown(%d)", v)
}

// jsonlOracleBits returns the JSON names of the set bits of v under names (the tracepack JSONL specification §3).
func jsonlOracleBits(names []string, v uint32) []string {
	out := []string{}
	for bit := range 32 {
		switch {
		case v&(1<<bit) == 0:
		case bit < len(names) && names[bit] != "":
			out = append(out, names[bit])
		default:
			out = append(out, fmt.Sprintf("bit(%d)", bit))
		}
	}

	return out
}

// jsonlRawBlock is a block decoded from the pack's bytes by the test, apart from the reader:
// its record header rows gathered as stored, and its payloads.
type jsonlRawBlock struct {
	rhl      int
	rows     []byte
	payloads [][]byte
}

// jsonlRawBlockAt decodes the block whose envelope starts file[off:].
func jsonlRawBlockAt(t testing.TB, file []byte, off uint64) *jsonlRawBlock {
	t.Helper()

	env, err := format.UnmarshalBlockEnvelope(file[off:])
	require.NoError(t, err)
	start := off + format.EnvelopeLen
	decoded, err := codec.Decode(env.Codec, nil, file[start:start+uint64(env.BodyLen)], int(env.UncompressedLen))
	require.NoError(t, err)

	n, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
	b := &jsonlRawBlock{rhl: rhl, rows: format.UntransposeHeaders(nil, decoded[:n*rhl], n, rhl)}
	at := n * rhl
	for j := range n {
		size := int(binary.LittleEndian.Uint32(b.rows[j*rhl+28:]))
		b.payloads = append(b.payloads, decoded[at:at+size])
		at += size
	}

	return b
}

// requireJSONLLineOf requires line to be the record line of record j of b, every field read from its raw header bytes.
func requireJSONLLineOf(t testing.TB, line string, b *jsonlRawBlock, j int) {
	t.Helper()

	row := b.rows[j*b.rhl : (j+1)*b.rhl]
	payload := b.payloads[j]
	got := parseJSONLRecord(t, line)
	le := binary.LittleEndian

	assert.Equal(t, strconv.FormatUint(le.Uint64(row[0:]), 10), got.Seq)
	assert.Equal(t, strconv.FormatInt(int64(le.Uint64(row[8:])), 10), got.TSUTCNs)
	assert.Equal(t, strconv.FormatInt(int64(le.Uint64(row[16:])), 10), got.MonoNs)
	assert.Equal(t, le.Uint32(row[24:]), got.Epoch)
	assert.Equal(t, le.Uint32(row[28:]), got.PayloadLen)
	assert.Equal(t, le.Uint32(row[32:]), got.TrailingBytes)
	assert.Equal(t, jsonlOracleBits(jsonlQualityNames, uint32(le.Uint16(row[36:]))), got.Quality)
	assert.Equal(t, jsonlOracleEnum(jsonlKindNames, row[38]), got.Kind)
	assert.Equal(t, jsonlOracleEnum(jsonlDirNames, row[39]), got.Dir)
	assert.Equal(t, jsonlOracleEnum(jsonlFidelityNames, row[40]), got.Fidelity)
	assert.Equal(t, jsonlOracleEnum(jsonlDecodeStatusNames, row[41]), got.DecodeStatus)
	assert.Equal(t, jsonlOracleBits(jsonlFieldValidityNames, uint32(row[42])), got.FieldValidity)
	assert.Equal(t, jsonlOracleBits(jsonlRecordFlagNames, uint32(row[43])), got.RecordFlags)
	assert.Equal(t, b.rhl, int(got.RecordHeaderLen))
	if b.rhl > format.RecordHeaderLen {
		assert.Equal(t, row[format.RecordHeaderLen:], got.HeaderExtra)
	} else {
		assert.Nil(t, got.HeaderExtra)
	}
	assert.Equal(t, nilIfEmpty(payload), nilIfEmpty(got.Payload))

	kind := Kind(row[38])
	hasBody := (kind == KindTransportEvent || kind == KindAnnotation) && checkEventPayload(kind, payload) == nil
	require.Equal(t, hasBody, got.Body != nil, "kind %v payload %x", kind, payload)
	if !hasBody {
		return
	}
	var body struct {
		Event          string  `json:"event"`
		AnnotationKind string  `json:"annotation_kind"`
		Text           *string `json:"text"`
		Raw            []byte  `json:"raw"`
	}
	require.NoError(t, json.Unmarshal(got.Body, &body))
	if kind == KindTransportEvent {
		ev, err := UnmarshalTransportEvent(payload)
		require.NoError(t, err)
		assert.Equal(t, jsonlOracleEnum(jsonlEventNames, uint8(ev.Event)), body.Event)

		return
	}
	a, err := UnmarshalAnnotation(payload)
	require.NoError(t, err)
	assert.Equal(t, jsonlOracleEnum(jsonlAnnotationKindNames, uint8(a.AnnotationKind)), body.AnnotationKind)
	assert.Equal(t, a.Text, body.Text)
	assert.Equal(t, nilIfEmpty(a.Raw), nilIfEmpty(body.Raw))
}

// Random packs export as lines that parse back to the records Iterate yields,
// every field checked against the record header bytes gathered by the test from the pack's own bytes.
func TestExportJSONL_ParseBackProperty(t *testing.T) {
	t.Parallel()

	packs, records, wide := 0, 0, 0
	for seed := range uint64(40) {
		for _, file := range genReadOf(t, seed).files {
			r := mustOpen(t, file, ReaderOptions{})
			lines := requireExportIsIterate(t, r)
			for _, line := range lines {
				require.True(t, json.Valid([]byte(line)))
			}
			run := iterate(t, r, Query{Payloads: true})
			blocks := r.Blocks()

			var (
				raw  *jsonlRawBlock
				prev = -1
				j    int
			)
			for k, it := range run.items {
				if it.block != prev {
					raw, prev, j = jsonlRawBlockAt(t, file, blocks[it.block].Offset), it.block, 0
				}
				requireJSONLLineOf(t, lines[1+k], raw, j)
				if raw.rhl > format.RecordHeaderLen {
					wide++
				}
				j++
			}
			packs++
			records += len(run.items)
		}
	}
	t.Logf("%d packs, %d records, %d of wide headers", packs, records, wide)
	assert.Positive(t, wide)
}

// Every prefix of a pack either fails Open, when it lacks part of the file header or the pack metadata,
// or exports exactly what Iterate reads of it, without an error:
// the header line alone for the prefix that ends with the pack metadata.
func TestExportJSONL_TruncationSweep(t *testing.T) {
	t.Parallel()

	for _, v := range sweepVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()

			file := sweepPack(t, v.codec, v.validate)
			start := int(mustOpen(t, file, v.opts).blocksStart())
			lines := map[string]int{}
			for n := range len(file) + 1 {
				r, err := openBytes(t, file[:n], v.opts)
				if n < start {
					require.Error(t, err, "prefix of %d bytes", n)
					continue
				}
				require.NoError(t, err, "prefix of %d bytes", n)

				got := requireExportIsIterate(t, r)
				if n == start {
					require.Len(t, got, 1, "the prefix ending with the pack metadata exports the header line alone")
				}
				if n == len(file) {
					require.Len(t, got, 1+len(sweepRecords(t)))
				}
				lines[strconv.Itoa(len(got)-1)]++
			}
			t.Logf("record lines per prefix: %v", lines)
			assert.Greater(t, len(lines), 3, "prefixes cut every block")
		})
	}
}

// jsonlMeta returns the pack metadata of the reader test packs without the tags in drop, followed by extra.
func jsonlMeta(t *testing.T, drop []uint16, extra ...tlv.Entry) []byte {
	t.Helper()

	entries, err := tlv.Decode(mustMarshalMeta(t, readerTestMeta()))
	require.NoError(t, err)
	entries = slices.DeleteFunc(entries, func(e tlv.Entry) bool { return slices.Contains(drop, e.Tag) })

	return jsonlTLV(append(entries, extra...)...)
}

// A pack whose file header or pack metadata a reader rejects has no export;
// every other requirement of the pack metadata binds writers, and its values are exported as stored.
func TestExportJSONL_Bootstrap(t *testing.T) {
	t.Parallel()

	hdr := format.FileHeader{FormatMajor: 1, CaptureID: jsonlUUID(0x0C)}
	set := tlv.UUIDEntry(0x0029, jsonlUUID(0x55))
	redaction := func(ranges []byte) tlv.Entry {
		return tlv.NestedEntry(0x0032, []tlv.Entry{
			tlv.U64Entry(0x0001, 5), tlv.BytesEntry(0x0003, ranges), tlv.UTF8Entry(0x0004, "d"), tlv.BytesEntry(0x0005, []byte{1}),
		})
	}
	u64s := func(vs ...uint64) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.LittleEndian.AppendUint64(b, v)
		}

		return b
	}

	rejected := []struct {
		name string
		file []byte
		is   error
	}{
		{"replacement_set_size 2", jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil, set, tlv.U64Entry(0x002A, 2), tlv.U64Entry(0x002B, 0))), ErrFieldValue},
		{"replacement_set_index 1", jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil, set, tlv.U64Entry(0x002A, 1), tlv.U64Entry(0x002B, 1))), ErrFieldValue},
		{"missing tool_id, required always", jsonlHeadOnlyPack(hdr, jsonlMeta(t, []uint16{0x0002})), nil},
		{"missing scope_generation of a segment", jsonlHeadOnlyPack(hdr, jsonlMeta(t, []uint16{0x002D})), ErrRequiredTag},
		{"capture-clock without capture origins", jsonlHeadOnlyPack(hdr, jsonlMeta(t, []uint16{0x0009}, tlv.U8Entry(0x0009, 1))), ErrRequiredTag},
		{"bad header CRC", flipByte(jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil)), 20), ErrChecksum},
		{"bad pack metadata CRC", flipByte(jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil)), format.FileHeaderLen+2), ErrChecksum},
		// A current reader limitation: the format treats these redaction entry defects as recoverable,
		// but Open fails on them today.
		{"masked_ranges not whole elements, reader limitation", jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil, redaction(make([]byte, 7)))), nil},
		{"masked_ranges of an odd count, reader limitation", jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil, redaction(u64s(1)))), nil},
		{"masked_ranges element over 2^63-1, reader limitation", jsonlHeadOnlyPack(hdr, jsonlMeta(t, nil, redaction(u64s(1<<63, 1)))), nil},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := openBytes(t, tt.file, ReaderOptions{})
			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
		})
	}

	// Written by hand from the tracepack JSONL specification §4 and §5: the metadata as stored.
	accepted := []struct {
		name string
		meta []byte
		want string
	}{
		{
			"a segment storing scope_generation 1",
			jsonlMeta(t, []uint16{0x002D}, tlv.U64Entry(0x002D, 1), tlv.U64Entry(0x002E, 7), set, tlv.U64Entry(0x002A, 1), tlv.U64Entry(0x002B, 0)),
			`"replacement_set_size":"1","replacement_set_index":"0","scope_generation":"1","publisher_epoch":"7"`,
		},
		{"the retired pack_role 5", jsonlMeta(t, []uint16{0x0018}, tlv.U8Entry(0x0018, 5)), `"pack_role":"unknown(5)"`},
		{"replacement set of one", jsonlMeta(t, nil, set, tlv.U64Entry(0x002A, 1), tlv.U64Entry(0x002B, 0)),
			`"replacement_set_size":"1","replacement_set_index":"0"`},
		{"missing classifier, required by the records", jsonlMeta(t, []uint16{0x0008}), `"writer":"wr","time_source":"unknown"`},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, jsonlHeadOnlyPack(hdr, tt.meta), ReaderOptions{})
			out, res, err := runExportJSONL(t, r)
			require.NoError(t, err)
			lines := jsonlLines(t, out)
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tt.want)
			assert.False(t, res.Complete(), "a pack without a trailer is not finalized")
		})
	}

	t.Run("pack metadata over MaxPackMetadataLen", func(t *testing.T) {
		t.Parallel()

		meta := jsonlMeta(t, nil)
		file := jsonlHeadOnlyPack(hdr, meta)
		_, err := openBytes(t, file, ReaderOptions{MaxPackMetadataLen: int64(len(meta) - 1)})
		require.ErrorIs(t, err, ErrReadLimit)

		r := mustOpen(t, file, ReaderOptions{MaxPackMetadataLen: int64(len(meta))})
		out, _, err := runExportJSONL(t, r)
		require.NoError(t, err)
		canonical, _, err := runExportJSONL(t, mustOpen(t, file, ReaderOptions{}))
		require.NoError(t, err)
		assert.Equal(t, canonical, out)
	})
}

// jsonlFuzzLimit bounds the budgets the fuzz target opens packs with, so a fuzzed length allocates little.
const jsonlFuzzLimit = 1 << 20

// FuzzExportJSONL exports fuzzed packs under fuzzed reader budgets.
// It never panics; a successful export is valid JSON lines holding Iterate's records, with Iterate's Result;
// a failed one fails only on a reader budget, with nothing written for the footer or the walk,
// and the header line and the lines of the blocks before it for a block.
// No ReasonLimit defect is ever listed.
// Defects are compared without their errors' text:
// the zstd decoder may describe one damaged stream differently from one read to the next.
func FuzzExportJSONL(f *testing.F) {
	big := []Record{testDataRecord(0, blockTestHour, 1), bigDataRecord(1, blockTestHour+hourNs, 5000), testDataRecord(2, blockTestHour+2*hourNs, 1)}
	shared, lists := sharedCountsF3Pack(f, mergePack(f, seg0, nil, hourSteps(1, 4, 1)), 1<<10)
	seeds := []struct {
		file                []byte
		maxBlock, maxFooter int64
		maxWalked           uint16
	}{
		{file: sweepPack(f, CodecNone, false)},
		{file: sweepPack(f, CodecZstd, true)},
		{file: jsonlExactPack(f)},
		{file: writeReaderPack(f, readerPackConfig{}, big).file, maxBlock: 4096},
		{file: writeReaderPack(f, readerPackConfig{open: true}, big).file, maxBlock: 4096},
		{file: writeReaderPack(f, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 4)).file, maxWalked: 2},
		{file: writeReaderPack(f, readerPackConfig{codec: CodecZstd}, hourRecords(3, 4)).file, maxFooter: 16},
		{file: shared, maxFooter: int64(lists / 2)},
	}
	for _, s := range seeds {
		f.Add(s.file, s.maxBlock, s.maxFooter, s.maxWalked)
	}
	// The packs of the conformance corpus, each under the default budgets.
	corpusPacks, err := filepath.Glob(filepath.Join("testdata", "corpus", "*", "*.tpk"))
	require.NoError(f, err)
	for _, p := range corpusPacks {
		file, err := os.ReadFile(p)
		require.NoError(f, err)
		f.Add(file, int64(0), int64(0), uint16(0))
	}

	f.Fuzz(func(t *testing.T, data []byte, maxBlock, maxFooter int64, maxWalked uint16) {
		data = bytes.Clone(data)
		resealBlocks(data)
		opts := ReaderOptions{
			MaxPackMetadataLen: jsonlFuzzLimit,
			MaxBlockLen:        min(positiveOr(maxBlock, jsonlFuzzLimit), jsonlFuzzLimit),
			MaxFooterLen:       min(positiveOr(maxFooter, jsonlFuzzLimit), jsonlFuzzLimit),
			MaxWalkedBlocks:    int(maxWalked),
		}
		r, err := openBytes(t, data, opts)
		if err != nil {
			return
		}

		var w bytes.Buffer
		res, err := ExportJSONL(t.Context(), r, &w)
		for _, d := range res.Incomplete {
			require.NotEqual(t, ReasonLimit, d.Reason, "a budget is never listed")
		}
		run, iterErr := runQuery(t.Context(), r, Query{Payloads: true})
		require.NoError(t, iterErr)
		lines := jsonlLines(t, w.String())
		for _, line := range lines {
			require.True(t, json.Valid([]byte(line)), "%q", line)
		}

		// The footer and walk budgets are read from Iterate's public Result, not from the fields ExportJSONL checks.
		bootstrapLimit := errors.Is(run.res.FooterErr, ErrReadLimit) ||
			slices.ContainsFunc(run.res.Incomplete, func(d Defect) bool { return d.Reason == ReasonLimit && d.Block < 0 })
		blockLimit := slices.IndexFunc(run.res.Incomplete, func(d Defect) bool { return d.Reason == ReasonLimit && d.Block >= 0 })
		if err == nil {
			require.False(t, bootstrapLimit, "a footer or walk budget fails the export")
			require.Negative(t, blockLimit, "a block budget fails the export")
			require.Len(t, lines, 1+len(run.items))
			requireRecordLines(t, lines[1:], run.items)
			requireSameResultAs(t, run.res, res, false)

			return
		}
		// A bytes.Reader never fails, and ctx is never cancelled, so only a budget fails the export.
		require.ErrorIs(t, err, ErrReadLimit)
		if bootstrapLimit {
			require.Empty(t, lines, "nothing is written for a footer or walk budget")
			want, _ := iterateResultBefore(t, r, 0)
			requireSameResultAs(t, want, res, false)

			return
		}
		require.GreaterOrEqual(t, blockLimit, 0, "a block over MaxBlockLen")
		want, items := iterateResultBefore(t, r, run.res.Incomplete[blockLimit].Block)
		requireSameResultAs(t, want, res, false)
		require.NotEmpty(t, lines)
		require.Equal(t, renderJSONLHeader(t, r), lines[0]+"\n")
		requireRecordLines(t, lines[1:], items)
	})
}
