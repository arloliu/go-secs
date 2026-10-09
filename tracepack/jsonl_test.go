package tracepack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// jsonlRecordKeys is the order of the keys of a record line (the tracepack JSONL specification §6),
// header_extra and body included.
var jsonlRecordKeys = []string{
	"seq", "ts_utc_ns", "mono_ns", "epoch", "payload_len", "trailing_bytes", "quality", "kind", "dir", "fidelity",
	"decode_status", "field_validity", "record_flags", "record_header_len", "header_extra", "payload", "body",
}

// jsonlRecordLine is a record line decoded by encoding/json, the test's own parser.
type jsonlRecordLine struct {
	Seq             string          `json:"seq"`
	TSUTCNs         string          `json:"ts_utc_ns"`
	MonoNs          string          `json:"mono_ns"`
	Epoch           uint32          `json:"epoch"`
	PayloadLen      uint32          `json:"payload_len"`
	TrailingBytes   uint32          `json:"trailing_bytes"`
	Quality         []string        `json:"quality"`
	Kind            string          `json:"kind"`
	Dir             string          `json:"dir"`
	Fidelity        string          `json:"fidelity"`
	DecodeStatus    string          `json:"decode_status"`
	FieldValidity   []string        `json:"field_validity"`
	RecordFlags     []string        `json:"record_flags"`
	RecordHeaderLen uint16          `json:"record_header_len"`
	HeaderExtra     []byte          `json:"header_extra"`
	Payload         []byte          `json:"payload"`
	Body            json.RawMessage `json:"body"`
}

// shortReaderAt serves data, but a read at off returns one byte less than asked, with err.
type shortReaderAt struct {
	data []byte
	off  int64
	err  error
}

// ReadAt implements io.ReaderAt.
func (s *shortReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(s.data).ReadAt(p, off)
	if off != s.off {
		return n, err
	}

	return len(p) - 1, s.err
}

// runExportJSONL runs ExportJSONL over r into a buffer and returns what it wrote, its Result and its error.
func runExportJSONL(t testing.TB, r *Reader) (string, Result, error) {
	t.Helper()

	var b bytes.Buffer
	res, err := ExportJSONL(t.Context(), r, &b)

	return b.String(), res, err
}

// jsonlLines splits the output of an export into its lines, each without its LF;
// every line, the last included, must end with one.
func jsonlLines(t testing.TB, out string) []string {
	t.Helper()

	if out == "" {
		return nil
	}
	require.True(t, strings.HasSuffix(out, "\n"), "the last line ends with an LF")

	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// jsonlTopKeys returns the keys of the JSON object line, in order.
func jsonlTopKeys(t testing.TB, line string) []string {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(line))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)
		key, ok := tok.(string)
		require.True(t, ok, "a key, got %v", tok)
		keys = append(keys, key)
		var v json.RawMessage
		require.NoError(t, dec.Decode(&v))
	}

	return keys
}

// parseJSONLRecord decodes the record line line, which must hold the keys of the tracepack JSONL specification §6 in order
// and nothing else.
func parseJSONLRecord(t testing.TB, line string) jsonlRecordLine {
	t.Helper()

	require.True(t, json.Valid([]byte(line)), "%q", line)
	keys := jsonlTopKeys(t, line)
	k := 0
	for _, want := range jsonlRecordKeys {
		if k < len(keys) && keys[k] == want {
			k++
		}
	}
	require.Len(t, keys, k, "record line keys %v out of order or unknown", keys)

	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	var rec jsonlRecordLine
	require.NoError(t, dec.Decode(&rec))

	return rec
}

// requireSameDefectsAs requires got to hold the defects of want, in order: the same reasons, places and coverage entries,
// and, when sameText is set, errors of the same text.
func requireSameDefectsAs(t testing.TB, want, got []Defect, sameText bool) {
	t.Helper()

	require.Len(t, got, len(want), "got %v, want %v", got, want)
	for i := range want {
		w, g := &want[i], &got[i]
		assert.Equal(t, w.Reason, g.Reason, "defect %d", i)
		assert.Equal(t, w.Pack, g.Pack, "defect %d", i)
		assert.Equal(t, w.Block, g.Block, "defect %d", i)
		assert.Equal(t, w.Offset, g.Offset, "defect %d", i)
		assert.Equal(t, w.Coverage, g.Coverage, "defect %d", i)
		require.Error(t, g.Err, "defect %d", i)
		if sameText {
			assert.Equal(t, w.Err.Error(), g.Err.Error(), "defect %d", i)
		}
	}
}

// requireSameResult requires got to equal want: Incomplete, FooterErr and FooterErrs each compared on its own.
func requireSameResult(t testing.TB, want, got Result) {
	t.Helper()

	requireSameResultAs(t, want, got, true)
}

// requireSameResultAs is requireSameResult, comparing the text of the defects' errors only when sameText is set.
func requireSameResultAs(t testing.TB, want, got Result, sameText bool) {
	t.Helper()

	requireSameDefectsAs(t, want.Incomplete, got.Incomplete, sameText)
	assert.Empty(t, got.Conflicts)
	assert.Empty(t, got.Removed)
	if want.FooterErr == nil {
		require.NoError(t, got.FooterErr)
	} else {
		require.EqualError(t, got.FooterErr, want.FooterErr.Error())
	}
	require.Len(t, got.FooterErrs, len(want.FooterErrs))
	for i := range want.FooterErrs {
		assert.Equal(t, want.FooterErrs[i].Pack, got.FooterErrs[i].Pack)
		require.EqualError(t, got.FooterErrs[i].Err, want.FooterErrs[i].Err.Error())
	}
}

// requireRecordLines requires lines to be the record lines of items, in order:
// each line's seq, record_header_len, header_extra and payload those of its item.
func requireRecordLines(t testing.TB, lines []string, items []iterItem) {
	t.Helper()

	require.Len(t, lines, len(items))
	for k, it := range items {
		rec := parseJSONLRecord(t, lines[k])
		assert.Equal(t, strconv.FormatUint(it.rec.Seq, 10), rec.Seq, "line %d", k)
		assert.Equal(t, format.RecordHeaderLen+len(it.extra), int(rec.RecordHeaderLen), "line %d", k)
		assert.Equal(t, it.extra, rec.HeaderExtra, "line %d", k)
		assert.Equal(t, nilIfEmpty(it.rec.Payload), nilIfEmpty(rec.Payload), "line %d", k)
	}
}

// requireExportIsIterate exports r and requires the export of what Iterate(Query{Payloads: true}) reads:
// no error, the header line, one record line per record it yields, and the same Result.
// It returns the export's lines.
func requireExportIsIterate(t *testing.T, r *Reader) []string {
	t.Helper()

	out, res, err := runExportJSONL(t, r)
	require.NoError(t, err)
	run := iterate(t, r, Query{Payloads: true})

	lines := jsonlLines(t, out)
	require.NotEmpty(t, lines)
	assert.Equal(t, renderJSONLHeader(t, r), lines[0]+"\n")
	requireRecordLines(t, lines[1:], run.items)
	requireSameResult(t, run.res, res)

	return lines
}

// iterateResultBefore returns the Result of Iterate(Query{Payloads: true}) over r as ExportJSONL returns it when it fails
// at a reader budget before block: without ReasonLimit defects and the defects of block and the blocks after it.
func iterateResultBefore(t testing.TB, r *Reader, block int) (Result, []iterItem) {
	t.Helper()

	run := iterate(t, r, Query{Payloads: true})
	res := run.res
	res.Incomplete = slices.DeleteFunc(slices.Clone(res.Incomplete), func(d Defect) bool {
		return d.Reason == ReasonLimit || d.Block >= block
	})
	items := slices.DeleteFunc(slices.Clone(run.items), func(it iterItem) bool { return it.block >= block })

	return res, items
}

// jsonlExactPack returns an unfinalized pack of four blocks, two records each, with record_header_len 44, 45, 47 and 55,
// whose record headers and payloads are set as stored, reserved bits and unknown enum values included:
//
//	block 0: 1 data, mono_ns without mono_present; 2 control holding an annotation body
//	block 1: 3 transport-event with a valid body; 4 transport-event whose primary_system_bytes has 3 bytes
//	block 2: 5 annotation with raw; 6 data holding an annotation body
//	block 3: 7 kind 0, unknown enum values and reserved bits, no payload; 8 annotation with a field_validity writer defect
func jsonlExactPack(t testing.TB) []byte {
	t.Helper()

	note := jsonlTLV(tlv.U8Entry(0x0001, 1), tlv.UTF8Entry(0x0005, "t"))
	event := jsonlTLV(
		tlv.U8Entry(0x0001, 1), tlv.U8Entry(0x0002, 2), tlv.U8Entry(0x0003, 3), tlv.U8Entry(0x0004, 99),
		tlv.U8Entry(0x0006, 3), tlv.BytesEntry(0x000C, []byte{0, 0, 0, 7}),
	)
	badEvent := jsonlTLV(tlv.U8Entry(0x0001, 1), tlv.BytesEntry(0x000C, []byte{1, 2, 3}))
	raw := jsonlTLV(tlv.U8Entry(0x0001, 3), tlv.BytesEntry(0x0006, []byte{0xFF, 0}))
	quoted := jsonlTLV(tlv.U8Entry(0x0001, 1), tlv.UTF8Entry(0x0005, `a"b`))
	const ts = 1791000000000000000

	type row struct {
		h       format.RecordHeader
		payload []byte
	}
	blocks := [4][2]row{
		{
			{format.RecordHeader{Seq: 1, TSUTCNs: ts + 1, MonoNs: 5000, Epoch: 1, Kind: 1, Dir: 1, Fidelity: 1, DecodeStatus: 1, FieldValidity: 0x3F},
				[]byte{0, 0, 0, 2, 0xAB, 0xCD}},
			{format.RecordHeader{Seq: 2, TSUTCNs: ts + 2, Epoch: 1, TrailingBytes: 3, Quality: 3, Kind: 2, Dir: 2, Fidelity: 2, DecodeStatus: 13, RecordFlags: 2},
				note},
		},
		{
			{format.RecordHeader{Seq: 3, TSUTCNs: ts + 3, MonoNs: 7, Epoch: 2, Kind: 3, Dir: 3, Fidelity: 4, DecodeStatus: 13, RecordFlags: 2, Extra: []byte{0xE0}},
				event},
			{format.RecordHeader{Seq: 4, TSUTCNs: ts + 4, MonoNs: 8, Epoch: 2, Kind: 3, Dir: 3, Fidelity: 4, DecodeStatus: 13, RecordFlags: 2, Extra: []byte{0xE1}},
				badEvent},
		},
		{
			{format.RecordHeader{Seq: 5, TSUTCNs: ts + 5, MonoNs: 9, Epoch: 2, Kind: 4, Dir: 3, Fidelity: 5, DecodeStatus: 13, RecordFlags: 2, Extra: []byte{0xA0, 0xA1, 0xA2}},
				raw},
			{format.RecordHeader{Seq: 6, TSUTCNs: ts + 6, MonoNs: 10, Epoch: 2, Kind: 1, Dir: 2, Fidelity: 1, DecodeStatus: 1, FieldValidity: 0x3F, RecordFlags: 2, Extra: []byte{0xB0, 0xB1, 0xB2}},
				note},
		},
		{
			{format.RecordHeader{Seq: 7, TSUTCNs: -5, MonoNs: -1, Kind: 0, Dir: 200, Fidelity: 9, DecodeStatus: 14, Extra: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
				nil},
			{format.RecordHeader{Seq: 8, TSUTCNs: -1, MonoNs: 11, Epoch: 3, Kind: 4, Dir: 3, Fidelity: 5, DecodeStatus: 13, FieldValidity: 1, RecordFlags: 2, Extra: []byte{10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}},
				quoted},
		},
	}

	steps := make([]footerTestStep, 8)
	for i := range steps {
		steps[i] = footerTestStep{rec: testDataRecord(uint64(i+1), blockTestHour+int64(i), 1), flush: i%2 == 1}
	}
	file := writeRepairPack(t, CodecNone, nil, true, steps).file

	return rebuildPack(t, file, func(i int, b *testBlock) {
		require.Len(t, b.headers, 2)
		for j := range b.headers {
			r := blocks[i][j]
			r.h.PayloadLen = uint32(len(r.payload))
			b.headers[j], b.payloads[j] = r.h, r.payload
		}
		if i == 3 {
			// Bits a record header encoder masks: quality bits 0, 3, 6 and 15, field_validity bits 0, 6 and 7,
			// record_flags bits 0, 1, 2 and 7, all in record 7.
			b.patchRows = func(rows []byte, _ int) {
				binary.LittleEndian.PutUint16(rows[36:], 0x8049)
				rows[42], rows[43] = 0xC1, 0x87
			}
		}
	})
}

// Record lines hold every record header field as stored, the block's record_header_len and extension area,
// the payload, and a body exactly for a valid transport-event or annotation body.
func TestExportJSONL_RecordLines(t *testing.T) {
	t.Parallel()

	r := mustOpen(t, jsonlExactPack(t), ReaderOptions{})

	// Written by hand from the tracepack JSONL specification §3 and §6.
	const (
		allFV = `["session_id","stream_and_w","function","ptype","stype","system_bytes"]`
		annot = `"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"not-applicable",`
		event = `"kind":"transport-event","dir":"local","fidelity":"synthesized","decode_status":"not-applicable",`
	)
	want := []string{
		`{"seq":"1","ts_utc_ns":"1791000000000000001","mono_ns":"5000","epoch":1,"payload_len":6,"trailing_bytes":0,"quality":[],` +
			`"kind":"data","dir":"host-to-equipment","fidelity":"wire-exact","decode_status":"ok","field_validity":` + allFV + `,` +
			`"record_flags":[],"record_header_len":44,"payload":"AAAAAqvN"}`,
		`{"seq":"2","ts_utc_ns":"1791000000000000002","mono_ns":"0","epoch":1,"payload_len":18,"trailing_bytes":3,` +
			`"quality":["capture-boundary","ordering-uncertain"],"kind":"control","dir":"equipment-to-host","fidelity":"re-encoded",` +
			`"decode_status":"not-applicable","field_validity":[],"record_flags":["mono_present"],"record_header_len":44,` +
			`"payload":"AQABAAEAAAABBQAGAAEAAAB0"}`,
		`{"seq":"3","ts_utc_ns":"1791000000000000003","mono_ns":"7","epoch":2,"payload_len":57,"trailing_bytes":0,"quality":[],` + event +
			`"field_validity":[],"record_flags":["mono_present"],"record_header_len":45,"header_extra":"4A==",` +
			`"payload":"AQABAAEAAAABAgABAAEAAAACAwABAAEAAAADBAABAAEAAABjBgABAAEAAAADDAAHAAQAAAAAAAAH",` +
			`"body":{"event":"state-transition","prev_state":"not-selected","cur_state":"selected","cause":"unknown(99)","timer":"T3",` +
			`"primary_system_bytes":"AAAABw=="}}`,
		`{"seq":"4","ts_utc_ns":"1791000000000000004","mono_ns":"8","epoch":2,"payload_len":20,"trailing_bytes":0,"quality":[],` + event +
			`"field_validity":[],"record_flags":["mono_present"],"record_header_len":45,"header_extra":"4Q==",` +
			`"payload":"AQABAAEAAAABDAAHAAMAAAABAgM="}`,
		`{"seq":"5","ts_utc_ns":"1791000000000000005","mono_ns":"9","epoch":2,"payload_len":19,"trailing_bytes":0,"quality":[],` + annot +
			`"field_validity":[],"record_flags":["mono_present"],"record_header_len":47,"header_extra":"oKGi",` +
			`"payload":"AQABAAEAAAADBgAHAAIAAAD/AA==","body":{"annotation_kind":"skipped-bytes","raw":"/wA="}}`,
		`{"seq":"6","ts_utc_ns":"1791000000000000006","mono_ns":"10","epoch":2,"payload_len":18,"trailing_bytes":0,"quality":[],` +
			`"kind":"data","dir":"equipment-to-host","fidelity":"wire-exact","decode_status":"ok","field_validity":` + allFV + `,` +
			`"record_flags":["mono_present"],"record_header_len":47,"header_extra":"sLGy","payload":"AQABAAEAAAABBQAGAAEAAAB0"}`,
		`{"seq":"7","ts_utc_ns":"-5","mono_ns":"-1","epoch":0,"payload_len":0,"trailing_bytes":0,` +
			`"quality":["capture-boundary","bit(3)","bit(6)","bit(15)"],"kind":"unknown","dir":"unknown(200)","fidelity":"unknown(9)",` +
			`"decode_status":"unknown(14)","field_validity":["session_id","bit(6)","bit(7)"],` +
			`"record_flags":["bit(0)","mono_present","bit(2)","bit(7)"],"record_header_len":55,"header_extra":"AAECAwQFBgcICQo=",` +
			`"payload":""}`,
		`{"seq":"8","ts_utc_ns":"-1","mono_ns":"11","epoch":3,"payload_len":20,"trailing_bytes":0,"quality":[],` + annot +
			`"field_validity":["session_id"],"record_flags":["mono_present"],"record_header_len":55,"header_extra":"CgkIBwYFBAMCAQA=",` +
			`"payload":"AQABAAEAAAABBQAGAAMAAABhImI=","body":{"annotation_kind":"note","text":"a\"b"}}`,
	}

	out, res, err := runExportJSONL(t, r)
	require.NoError(t, err)
	lines := jsonlLines(t, out)
	require.Len(t, lines, 1+len(want))
	assert.Equal(t, renderJSONLHeader(t, r), lines[0]+"\n")
	for i, w := range want {
		assert.Equal(t, w, lines[1+i], "record line %d", i)
	}
	assert.Equal(t, out, strings.Join(append(lines[:1:1], want...), "\n")+"\n")
	requireExportIsIterate(t, r)
	require.Len(t, res.Incomplete, 1, "the pack is not finalized")
}

// The example of the tracepack JSONL specification §10 is the record line of its record.
func TestExportJSONL_SpecExample(t *testing.T) {
	t.Parallel()

	rec := Record{
		Seq: 7, TSUTCNs: 1791000000000000000, MonoNs: 5000, MonoPresent: true, Epoch: 1,
		Kind: KindAnnotation, Dir: DirLocal, Fidelity: FidelityNotApplicable, DecodeStatus: DecodeStatusNotApplicable,
		Payload: jsonlTLV(tlv.U8Entry(0x0001, uint8(AnnotationKindNote)), tlv.UTF8Entry(0x0005, "a<b")),
	}
	file := writeRepairPack(t, CodecZstd, nil, false, []footerTestStep{{rec: rec}}).file

	out, res, err := runExportJSONL(t, mustOpen(t, file, ReaderOptions{}))
	require.NoError(t, err)
	assert.True(t, res.Complete())
	lines := jsonlLines(t, out)
	require.Len(t, lines, 2)
	assert.Equal(t,
		`{"seq":"7","ts_utc_ns":"1791000000000000000","mono_ns":"5000","epoch":1,"payload_len":20,"trailing_bytes":0,"quality":[],`+
			`"kind":"annotation","dir":"local","fidelity":"not-applicable","decode_status":"not-applicable","field_validity":[],`+
			`"record_flags":["mono_present"],"record_header_len":44,"payload":"AQABAAEAAAABBQAGAAMAAABhPGI=",`+
			`"body":{"annotation_kind":"note","text":"a<b"}}`,
		lines[1])
}

// 64-bit fields are decimal strings and 32-bit fields numbers, exact at their endpoints and around 2^53.
func TestExportJSONL_NumericEndpoints(t *testing.T) {
	t.Parallel()

	seqs := []uint64{1<<53 - 1, 1 << 53, 1<<53 + 1, 1<<63 - 1}
	steps := make([]footerTestStep, len(seqs))
	for i, seq := range seqs {
		steps[i] = footerTestStep{rec: testDataRecord(seq, blockTestHour+int64(i), 1)}
	}
	file := writeRepairPack(t, CodecNone, nil, true, steps).file
	headers := []format.RecordHeader{
		{Seq: seqs[0], TSUTCNs: -1 << 63, MonoNs: 1<<63 - 1, Epoch: 1<<32 - 1, TrailingBytes: 1<<32 - 1},
		{Seq: seqs[1], TSUTCNs: 1 << 53, MonoNs: -(1<<53 + 1), Epoch: 1 << 31},
		{Seq: seqs[2], TSUTCNs: 1<<53 + 1, MonoNs: 1<<53 - 1},
		{Seq: seqs[3], TSUTCNs: 1<<63 - 1, MonoNs: -1 << 63},
	}
	file = rebuildPack(t, file, func(_ int, b *testBlock) {
		require.Len(t, b.headers, len(headers))
		copy(b.headers, headers)
		clear(b.payloads)
	})

	// Written by hand from the tracepack JSONL specification §3 and §6.
	const rest = `"quality":[],"kind":"unknown","dir":"unknown","fidelity":"unknown","decode_status":"not-attempted",` +
		`"field_validity":[],"record_flags":[],"record_header_len":44,"payload":""}`
	want := []string{
		`{"seq":"9007199254740991","ts_utc_ns":"-9223372036854775808","mono_ns":"9223372036854775807","epoch":4294967295,` +
			`"payload_len":0,"trailing_bytes":4294967295,` + rest,
		`{"seq":"9007199254740992","ts_utc_ns":"9007199254740992","mono_ns":"-9007199254740993","epoch":2147483648,` +
			`"payload_len":0,"trailing_bytes":0,` + rest,
		`{"seq":"9007199254740993","ts_utc_ns":"9007199254740993","mono_ns":"9007199254740991","epoch":0,` +
			`"payload_len":0,"trailing_bytes":0,` + rest,
		`{"seq":"9223372036854775807","ts_utc_ns":"9223372036854775807","mono_ns":"-9223372036854775808","epoch":0,` +
			`"payload_len":0,"trailing_bytes":0,` + rest,
	}

	out, _, err := runExportJSONL(t, mustOpen(t, file, ReaderOptions{}))
	require.NoError(t, err)
	assert.Equal(t, want, jsonlLines(t, out)[1:])
}

// The export depends on the records, not on how the blocks are encoded,
// and two exports of one pack are byte-identical.
func TestExportJSONL_CodecIndependentAndDeterministic(t *testing.T) {
	t.Parallel()

	file := sweepPack(t, CodecNone, true)
	other := otherCodec(t, file)
	require.NotEqual(t, file, other)

	none := mustOpen(t, file, ReaderOptions{})
	zstd := mustOpen(t, other, ReaderOptions{})
	require.Equal(t, uint8(CodecZstd), func() uint8 {
		var buf blockBuf
		d, _, err := zstd.readBlock(0, &buf)
		require.NoError(t, err)

		return d.env.Codec
	}())

	a, _, err := runExportJSONL(t, none)
	require.NoError(t, err)
	b, _, err := runExportJSONL(t, zstd)
	require.NoError(t, err)
	assert.Equal(t, a, b)
	assert.Len(t, jsonlLines(t, a), 1+len(sweepRecords(t)))

	again, _, err := runExportJSONL(t, none)
	require.NoError(t, err)
	assert.Equal(t, a, again)
}

// The export holds the records Iterate(Query{Payloads: true}) yields and returns its Result:
// coverage entries by the matching rule, failed and disagreeing blocks, the forward walk without a usable footer.
func TestExportJSONL_SameAsIterate(t *testing.T) {
	t.Parallel()

	other, capture := UUID{0x0E}, UUID(jsonlUUID(0xC0))
	from, to := blockTestHour+5, blockTestHour+1
	seq1, seq9 := uint64(1), uint64(9)
	coverage := []Coverage{
		{CaptureID: &other, TimeStart: &from},
		{TimeStart: &to, TimeEnd: &from},
		{CaptureID: &capture, SeqFirst: &seq9, SeqLast: &seq1, TimeStart: &from, TimeEnd: &to},
		{},
		{CaptureID: &capture, Unknown: []RawEntry{{Tag: 9, Type: 6, Value: []byte("x")}}},
	}
	covered := withIDs(t, writeReaderPack(t, readerPackConfig{meta: func(m *PackMeta) { m.Coverage = coverage }}, hourRecords(2, 3)).file,
		UUID{0x70}, capture)

	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 4))
	at := closed.blocks[1].offset
	bodyByte := at + format.EnvelopeLen + uint64(closed.blocks[1].onDiskLen-format.EnvelopeLen)/2
	mismatch, _ := indexMismatchPack(t, func(b *testBlock) { b.headers[0].TSUTCNs++ })

	// An F-3 summary that misstates block 1's kinds, under a footer that still validates.
	f3 := reindexedWith(t, closed.file, func(i int, s *blockSummary) {
		if i == 1 {
			s.kindCounts = []uint32{0, s.kindCounts[KindData] - 1, 1}
		}
	})
	rep, err := verifyBytes(t, f3, VerifyOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, rep.Disagreements, "the F-3 summary disagrees with the block")

	// An event record and an annotation record whose payloads are not valid bodies.
	invalid := writeReaderPack(t, readerPackConfig{}, []Record{
		testDataRecord(0, blockTestHour, 1),
		{Seq: 1, TSUTCNs: blockTestHour + 1, Kind: KindTransportEvent, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable, Payload: []byte("abc")},
		{Seq: 2, TSUTCNs: blockTestHour + 2, Kind: KindAnnotation, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable,
			Payload: jsonlTLV(tlv.U8Entry(0x0001, 1))},
	}).file

	tests := []struct {
		name  string
		file  []byte
		check func(t *testing.T, lines []string, res Result)
	}{
		{"complete", closed.file, func(t *testing.T, lines []string, res Result) {
			assert.True(t, res.Complete())
			assert.Len(t, lines, 13)
		}},
		{"coverage entries", covered, func(t *testing.T, _ []string, res Result) {
			require.Len(t, res.Incomplete, 4, "every entry but the other capture's")
			for i, c := range []int{1, 2, 3, 4} {
				assert.Equal(t, ReasonCoverage, res.Incomplete[i].Reason)
				assert.Equal(t, coverage[c], *res.Incomplete[i].Coverage)
			}
		}},
		{"corrupt middle block", flipByte(closed.file, bodyByte), func(t *testing.T, lines []string, res Result) {
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, ReasonCorruptBlock, res.Incomplete[0].Reason)
			assert.Equal(t, 1, res.Incomplete[0].Block)
			assert.Len(t, lines, 9, "blocks 0 and 2")
		}},
		{"unknown codec", patchEnvelope(t, closed.file, at, func(e *format.BlockEnvelope) { e.Codec = 7 }), func(t *testing.T, lines []string, res Result) {
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, ReasonUnknownCodec, res.Incomplete[0].Reason)
			assert.Len(t, lines, 9)
		}},
		{"bad envelope, usable footer", flipByte(closed.file, at+8), func(t *testing.T, lines []string, res Result) {
			require.NoError(t, res.FooterErr)
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, ReasonCorruptBlock, res.Incomplete[0].Reason)
			assert.Len(t, lines, 9, "the footer locates block 2")
		}},
		{"bad envelope, no usable footer", flipByte(invalidFooterFile(t, closed.file), at+8), func(t *testing.T, lines []string, res Result) {
			require.ErrorIs(t, res.FooterErr, ErrInvalidFooter)
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, ReasonTruncated, res.Incomplete[0].Reason)
			assert.Len(t, lines, 5, "the walk stops at block 1")
		}},
		{"unused footer, complete walk", invalidFooterFile(t, closed.file), func(t *testing.T, lines []string, res Result) {
			require.ErrorIs(t, res.FooterErr, ErrInvalidFooter)
			assert.Empty(t, res.Incomplete, "a footer error alone leaves the read complete")
			assert.Len(t, lines, 13)
		}},
		{"F-2 disagreement", mismatch, func(t *testing.T, lines []string, res Result) {
			require.Len(t, res.Incomplete, 1)
			assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Reason)
			assert.Len(t, lines, 121, "the disagreeing block is exported")
		}},
		{"F-3 disagreement only", f3, func(t *testing.T, lines []string, res Result) {
			assert.True(t, res.Complete(), "a read does not compare F-3 summaries")
			assert.Len(t, lines, 13)
		}},
		{"invalid bodies", invalid, func(t *testing.T, lines []string, res Result) {
			assert.True(t, res.Complete(), "an invalid body adds nothing")
			require.Len(t, lines, 4)
			for _, line := range lines[1:] {
				assert.Nil(t, parseJSONLRecord(t, line).Body)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := mustOpen(t, tt.file, ReaderOptions{})
			lines := requireExportIsIterate(t, r)
			_, res, err := runExportJSONL(t, r)
			require.NoError(t, err)
			tt.check(t, lines, res)
		})
	}
}

// A data or control record whose payload happens to be a valid annotation body gets no body:
// only transport-event and annotation records carry one.
func TestExportJSONL_BodyOnlyForEventKinds(t *testing.T) {
	t.Parallel()

	note := jsonlTLV(tlv.U8Entry(0x0001, 1), tlv.UTF8Entry(0x0005, "t"))
	require.True(t, jsonlValidBody(KindData, note), "the body predicate checks any other kind as an annotation")

	for _, kind := range []Kind{KindUnknown, KindData, KindControl, KindTransportEvent, KindAnnotation, Kind(200)} {
		file := writeReaderPack(t, readerPackConfig{open: true}, []Record{testDataRecord(1, blockTestHour, 1)}).file
		file = rebuildPack(t, file, func(_ int, b *testBlock) {
			b.headers[0].Kind, b.headers[0].PayloadLen = uint8(kind), uint32(len(note))
			b.payloads[0] = note
		})
		out, _, err := runExportJSONL(t, mustOpen(t, file, ReaderOptions{}))
		require.NoError(t, err)
		rec := parseJSONLRecord(t, jsonlLines(t, out)[1])
		// The same bytes are a valid transport-event body too: event state-transition and cause_raw "t".
		want := map[Kind]string{
			KindAnnotation:     `{"annotation_kind":"note","text":"t"}`,
			KindTransportEvent: `{"event":"state-transition","cause_raw":"t"}`,
			KindUnknown:        "",
			KindData:           "",
			KindControl:        "",
		}[kind]
		assert.Equal(t, want, string(rec.Body), "kind %v", kind)
	}
}

// A reader budget fails the export instead of leaving blocks out:
// the footer and walk budgets before anything is written, a block budget when the export reaches the block.
// With sufficient budgets the export is the canonical one.
func TestExportJSONL_ReaderBudgets(t *testing.T) {
	t.Parallel()

	big := func(first bool) []byte {
		recs := []Record{testDataRecord(0, blockTestHour, 1), bigDataRecord(1, blockTestHour+hourNs, 20_000), testDataRecord(2, blockTestHour+2*hourNs, 1)}
		if first {
			recs[0] = bigDataRecord(0, blockTestHour, 20_000)
		}

		return writeReaderPack(t, readerPackConfig{}, recs).file
	}
	zstd := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 40)).file
	zl := layoutOf(t, zstd).tr
	require.Greater(t, zl.FooterUncompressedLen, zl.FooterLen)
	shared, lists := sharedCountsF3Pack(t, mergePack(t, seg0, nil, hourSteps(1, 32, 1)), 64<<10)
	open := writeReaderPack(t, readerPackConfig{codec: CodecZstd, open: true}, hourRecords(3, 4)).file
	closed := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, hourRecords(3, 4))
	damaged := flipByte(closed.file, closed.blocks[1].offset+8)

	tests := []struct {
		name string
		file []byte
		// over is a budget the pack exceeds, enough one it fits.
		over, enough ReaderOptions
		// block is the block over MaxBlockLen, or -1 when the footer or the walk is over its budget.
		block int
	}{
		{"MaxBlockLen, first block", big(true), ReaderOptions{MaxBlockLen: 4096}, ReaderOptions{MaxBlockLen: 1 << 20}, 0},
		{"MaxBlockLen, middle block", big(false), ReaderOptions{MaxBlockLen: 4096}, ReaderOptions{MaxBlockLen: 1 << 20}, 1},
		{"MaxFooterLen, on disk", zstd, ReaderOptions{MaxFooterLen: int64(zl.FooterLen) - 1}, ReaderOptions{MaxFooterLen: int64(zl.FooterUncompressedLen)}, -1},
		{"MaxFooterLen, decoded", zstd, ReaderOptions{MaxFooterLen: int64(zl.FooterLen)}, ReaderOptions{MaxFooterLen: int64(zl.FooterUncompressedLen)}, -1},
		{"MaxFooterLen, F-3 lists", shared, ReaderOptions{MaxFooterLen: int64(lists / 2)}, ReaderOptions{MaxFooterLen: int64(lists)}, -1},
		{"MaxWalkedBlocks", open, ReaderOptions{MaxWalkedBlocks: 2}, ReaderOptions{MaxWalkedBlocks: 3}, -1},
		{"footer and walk", closed.file, ReaderOptions{MaxFooterLen: 16, MaxWalkedBlocks: 1}, ReaderOptions{MaxWalkedBlocks: 1}, -1},
		{"damaged envelope recoverable only by the footer", damaged, ReaderOptions{MaxFooterLen: 16}, ReaderOptions{}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			canonical, _, err := runExportJSONL(t, mustOpen(t, tt.file, ReaderOptions{}))
			require.NoError(t, err)

			r := mustOpen(t, tt.file, tt.enough)
			out, _, err := runExportJSONL(t, r)
			require.NoError(t, err)
			assert.Equal(t, canonical, out, "a sufficient budget gives the canonical export")
			requireExportIsIterate(t, r)

			r = mustOpen(t, tt.file, tt.over)
			out, res, err := runExportJSONL(t, r)
			require.ErrorIs(t, err, ErrReadLimit)
			for _, d := range res.Incomplete {
				assert.NotEqual(t, ReasonLimit, d.Reason, "a budget is never listed")
			}
			if tt.block < 0 {
				assert.Empty(t, out, "nothing is written")
				want := iterate(t, r, Query{Payloads: true}).res
				want.Incomplete = slices.DeleteFunc(want.Incomplete, func(d Defect) bool { return d.Reason == ReasonLimit || d.Block >= 0 })
				requireSameResult(t, want, res)

				return
			}

			want, items := iterateResultBefore(t, r, tt.block)
			requireSameResult(t, want, res)
			lines := jsonlLines(t, out)
			assert.Equal(t, renderJSONLHeader(t, r), lines[0]+"\n")
			requireRecordLines(t, lines[1:], items)
			assert.Equal(t, canonical[:len(out)], out, "the output is a prefix of the canonical export")
		})
	}
}

// Write errors, short writes, cancellation and read errors end the export with the error and the Result found so far.
func TestExportJSONL_Errors(t *testing.T) {
	t.Parallel()

	from := blockTestHour
	coverage := []Coverage{{TimeStart: &from, Unknown: []RawEntry{{Tag: 0x40, Type: 9, Value: []byte{1, 2}}}}}
	// Two blocks of three records, with a coverage entry holding an unknown nested entry, under a footer that is not valid.
	file := invalidFooterFile(t, writeReaderPack(t, readerPackConfig{codec: CodecZstd, meta: func(m *PackMeta) { m.Coverage = coverage }},
		hourRecords(2, 3)).file)
	r := mustOpen(t, file, ReaderOptions{})
	run := iterate(t, r, Query{Payloads: true})
	canonical, _, err := runExportJSONL(t, r)
	require.NoError(t, err)
	lines := jsonlLines(t, canonical)
	require.Len(t, lines, 7)

	// requireStart checks the Result of an export that failed before any block was read.
	requireStart := func(t *testing.T, res Result) {
		t.Helper()

		require.Len(t, res.Incomplete, 1)
		assert.Equal(t, ReasonCoverage, res.Incomplete[0].Reason)
		assert.Equal(t, coverage[0].Unknown, res.Incomplete[0].Coverage.Unknown)
		require.ErrorIs(t, res.FooterErr, ErrInvalidFooter)
		require.Len(t, res.FooterErrs, 1)
		assert.Equal(t, 0, res.FooterErrs[0].Pack)
		require.ErrorIs(t, res.FooterErrs[0].Err, ErrInvalidFooter)
	}

	t.Run("writer fails on the header line", func(t *testing.T) {
		t.Parallel()

		w := &failingWriter{err: errJSONLWriteBoom}
		res, err := ExportJSONL(t.Context(), r, w)
		require.ErrorIs(t, err, errJSONLWriteBoom)
		assert.Equal(t, 1, w.calls)
		requireStart(t, res)
	})
	t.Run("writer fails on a later record line", func(t *testing.T) {
		t.Parallel()

		// One Write per line: the header line and two record lines pass, the third record line fails.
		w := &failingWriter{ok: 3, err: errJSONLWriteBoom}
		res, err := ExportJSONL(t.Context(), r, w)
		require.ErrorIs(t, err, errJSONLWriteBoom)
		assert.Equal(t, 4, w.calls)
		requireSameResult(t, run.res, res)
	})
	t.Run("short write", func(t *testing.T) {
		t.Parallel()

		w := &failingWriter{ok: 1, n: 5}
		res, err := ExportJSONL(t.Context(), r, w)
		require.ErrorIs(t, err, io.ErrShortWrite)
		assert.Equal(t, 2, w.calls)
		requireStart(t, res)
	})
	t.Run("cancelled before the header line", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		empty := jsonlTestReader(t, format.FileHeader{FormatMajor: 1}, mustMarshalMeta(t, readerTestMeta()))
		var b bytes.Buffer
		res, err := ExportJSONL(ctx, empty, &b)
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, b.String())
		requireSameResult(t, iterate(t, empty, Query{Payloads: true}).res, res)
	})
	t.Run("cancelled between blocks", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// The context is cancelled while block 0 is written: its records are written, block 1 is not read.
		w := &cancelWriter{n: 2, cancel: cancel}
		res, err := ExportJSONL(ctx, r, w)
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, strings.Join(lines[:4], "\n")+"\n", w.buf.String())
		requireStart(t, res)
	})
	t.Run("index mismatch, then a write failure", func(t *testing.T) {
		t.Parallel()

		mismatch, _ := indexMismatchPack(t, func(b *testBlock) { b.headers[0].TSUTCNs++ })
		mr := mustOpen(t, mismatch, ReaderOptions{})
		// The header line and block 0's 40 record lines pass; block 1's first record line fails.
		w := &failingWriter{ok: 41, err: errJSONLWriteBoom}
		res, err := ExportJSONL(t.Context(), mr, w)
		require.ErrorIs(t, err, errJSONLWriteBoom)
		require.Len(t, res.Incomplete, 1, "the disagreement is listed before the block's records are written")
		assert.Equal(t, ReasonIndexMismatch, res.Incomplete[0].Reason)
		assert.Equal(t, 1, res.Incomplete[0].Block)
		require.NoError(t, res.FooterErr)
		assert.Empty(t, res.FooterErrs)
	})

	blocks := r.Blocks()
	for _, tt := range []struct {
		name string
		err  error
		is   error
	}{
		{"short read without an error", nil, io.ErrUnexpectedEOF},
		{"short read with io.EOF", io.EOF, io.ErrUnexpectedEOF},
		{"short read with another error", errInjected, errInjected},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sr := mustOpen(t, file, ReaderOptions{})
			sr.ra = &shortReaderAt{data: file, off: int64(blocks[1].Offset), err: tt.err}
			var b bytes.Buffer
			res, err := ExportJSONL(t.Context(), sr, &b)
			require.ErrorIs(t, err, tt.is)
			_, iterErr := runQuery(t.Context(), sr, Query{Payloads: true})
			require.EqualError(t, err, iterErr.Error(), "the error Iterate returns")
			assert.Equal(t, strings.Join(lines[:4], "\n")+"\n", b.String())
			requireStart(t, res)
		})
	}
}

// A line longer than the writer's buffer reaches the destination in pieces of at most 64 KiB,
// a payload in whole base64 groups and a text in whole escapes, and parses back to the stored bytes.
func TestExportJSONL_StreamsLongLines(t *testing.T) {
	t.Parallel()

	const n = 3*jsonlBufferSize + 1
	data := randomDataRecord(0, blockTestHour, n)
	nuls := strings.Repeat("\x00", n)
	note, err := (&Annotation{AnnotationKind: AnnotationKindNote, Text: &nuls}).MarshalBinary()
	require.NoError(t, err)
	annotation := Record{Seq: 1, TSUTCNs: blockTestHour + 1, Kind: KindAnnotation, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable, Payload: note}
	file := writeReaderPack(t, readerPackConfig{codec: CodecZstd}, []Record{data, annotation}).file
	r := mustOpen(t, file, ReaderOptions{})

	var w recordingWriter
	res, err := ExportJSONL(t.Context(), r, &w)
	require.NoError(t, err)
	assert.True(t, res.Complete())
	out := w.buf.String()
	requireChunked(t, &w, out)
	assert.Greater(t, len(w.sizes), 8)

	lines := jsonlLines(t, out)
	require.Len(t, lines, 3)
	assert.True(t, strings.HasSuffix(lines[1], `,"payload":"`+base64.StdEncoding.EncodeToString(data.Payload)+`"}`))
	assert.True(t, strings.HasSuffix(lines[2], `,"body":{"annotation_kind":"note","text":"`+strings.Repeat(`\u0000`, n)+`"}}`))
	assert.Equal(t, data.Payload, parseJSONLRecord(t, lines[1]).Payload)
	var body struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(parseJSONLRecord(t, lines[2]).Body, &body))
	assert.Equal(t, nuls, body.Text)
}

// Rendering a record line allocates nothing on top of the block the reader holds,
// whatever the payload length and record_header_len.
func TestExportJSONL_RenderingWorkspace(t *testing.T) {
	// Not parallel: testing.AllocsPerRun and the heap statistics count every goroutine's allocations.
	for _, wide := range []bool{false, true} {
		var perRun [2]float64
		for k, n := range []int{1 << 10, 4 << 20} {
			raw, err := (&Annotation{AnnotationKind: AnnotationKindSkippedBytes, Raw: make([]byte, n)}).MarshalBinary()
			require.NoError(t, err)
			recs := []Record{
				bigDataRecord(0, blockTestHour, n),
				{Seq: 1, TSUTCNs: blockTestHour, Kind: KindAnnotation, Dir: DirLocal, DecodeStatus: DecodeStatusNotApplicable, Payload: raw},
			}
			file := writeReaderPack(t, readerPackConfig{threshold: 64 << 20}, recs).file
			if wide {
				file = rebuildPack(t, file, func(_ int, b *testBlock) { widen(b) })
			}
			r := mustOpen(t, file, ReaderOptions{})
			var buf blockBuf
			d, def, err := r.readBlock(0, &buf)
			require.NoError(t, err)
			require.Nil(t, def)
			require.Equal(t, 2, d.count())

			jw := newJSONLWriter(discardWriter{})
			render := func() {
				for j := range d.count() {
					_ = writeJSONLRecord(jw, d, j)
				}
			}
			assert.Zero(t, testing.AllocsPerRun(5, render), "wide %v, %d-byte payloads", wide, n)

			const runs = 8
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range runs {
				render()
			}
			runtime.ReadMemStats(&after)
			perRun[k] = float64(after.TotalAlloc-before.TotalAlloc) / runs
		}
		t.Logf("wide %v: %v bytes per rendering of 1 KiB and 4 MiB payloads", wide, perRun)
		assert.Less(t, perRun[1], float64(4<<10), "wide %v: the workspace does not grow with the payload", wide)
	}
}
