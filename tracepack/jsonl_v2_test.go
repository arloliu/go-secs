package tracepack

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// jsonlV2SIV is the siv value of the redaction entry of jsonlV2Meta.
var jsonlV2SIV = bytes.Repeat([]byte{0x5A}, 16)

// jsonlV2Required returns the tags the pack metadata of a segment requires.
func jsonlV2Required() []tlv.Entry {
	return []tlv.Entry{
		tlv.UTF8Entry(0x0002, "t"), tlv.U8Entry(0x0003, 1), tlv.U8Entry(0x0004, 0), tlv.U8Entry(0x0005, 1),
		tlv.UTF8Entry(0x0006, "r"), tlv.UTF8Entry(0x0007, "w"), tlv.U8Entry(0x0009, 0), tlv.I64Entry(0x0010, 0),
		tlv.I64Entry(0x0011, 1), tlv.U8Entry(0x0012, 1), tlv.BoolEntry(0x0013, true), tlv.U8Entry(0x0018, 1),
		tlv.U8Entry(0x0019, 0), tlv.UUIDEntry(0x001B, jsonlUUID(1)), tlv.U64Entry(0x0027, 0), tlv.U64Entry(0x002D, 0),
	}
}

// jsonlV2Thinning returns the nested entries of the two thinning entries of jsonlV2Meta:
// the first stored in reverse tag order with an unknown nested tag, the second in tag order.
func jsonlV2Thinning() (first, second []tlv.Entry) {
	first = []tlv.Entry{
		tlv.UTF8Entry(0x0003, "fdc"), tlv.U64Entry(0x0009, 9), tlv.U64Entry(0x0002, 3), tlv.U64Entry(0x0001, 1),
	}
	second = []tlv.Entry{tlv.U64Entry(0x0001, 1), tlv.U64Entry(0x0002, 4), tlv.UTF8Entry(0x0003, "fdc")}

	return first, second
}

// jsonlV2Meta returns a pack metadata holding every number format 1.1 assigns,
// each nested list stored in reverse tag order but the second thinning entry:
// two thinning entries, a redaction_policy with both encryption tags and a redaction entry with a siv.
func jsonlV2Meta() []byte {
	first, second := jsonlV2Thinning()
	u64s := binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(nil, 14), 4)

	return jsonlTLV(append(jsonlV2Required(),
		tlv.NestedEntry(0x0033, first),
		tlv.NestedEntry(0x0031, []tlv.Entry{
			tlv.UTF8Entry(0x0006, "test-enc-1"), tlv.U8Entry(0x0005, 1), tlv.U8Entry(0x0004, 1),
			tlv.UTF8Entry(0x0003, "k"), tlv.U64Entry(0x0002, 1), tlv.UTF8Entry(0x0001, "p"),
		}),
		tlv.NestedEntry(0x0032, []tlv.Entry{
			tlv.BytesEntry(0x0006, jsonlV2SIV), tlv.BytesEntry(0x0005, []byte{0xAB, 0xCD, 0xEF}),
			tlv.UTF8Entry(0x0004, "d"), tlv.BytesEntry(0x0003, u64s), tlv.U64Entry(0x0001, 5),
		}),
		tlv.NestedEntry(0x0033, second),
	)...)
}

// jsonlV2RequiredJSON is the rendering of jsonlV2Required's tags, without the braces of the metadata object.
const jsonlV2RequiredJSON = `"tool_id":"t","transport":"hsms-ss","capture_method":"unknown","vantage":"host","recorder":"r","writer":"w",` +
	`"time_source":"unknown","period_start":"0","period_end":"1","lifecycle_coverage":"subscribed","quality_evaluated":true,` +
	`"pack_role":"segment","compaction_level":0,"recorder_instance_id":"01010101-0101-0101-0101-010101010101","seq_start":"0",` +
	`"scope_generation":"0"`

// jsonlV2HeaderStart is the start of the header line of a pack of jsonlTestReader's zero file header but for the minor,
// up to the metadata object's first key.
func jsonlV2HeaderStart(schema string, minor int) string {
	return `{"schema":"tracepack-jsonl/` + schema + `","format_major":1,"format_minor":` + strconv.Itoa(minor) +
		`,"flags":[],"writer_start_utc_ns":"0","pack_id":"00000000-0000-0000-0000-000000000000",` +
		`"capture_id":"00000000-0000-0000-0000-000000000000","metadata":{`
}

// jsonlThinnedPack returns a pack of thinnedMeta written by the Writer, of format minor 1:
// an S1F4 at seq 0 with a 3-byte message text, and an unobserved S1F4 at seq 1.
func jsonlThinnedPack(t *testing.T) []byte {
	t.Helper()

	return writeMinorPack(t, WriterOptions{Meta: thinnedMeta()},
		frameRecord(0, hsmsFrame(1, 4, []byte{0x21, 0x01, 0x00})), unobservedRecord(1))
}

// jsonlExportLines exports file and returns its lines, each without its LF.
func jsonlExportLines(t *testing.T, file []byte) []string {
	t.Helper()

	out, _, err := runExportJSONL(t, mustOpen(t, file, ReaderOptions{}))
	require.NoError(t, err)

	return jsonlLines(t, out)
}

// The schema of the export follows from the pack's format minor alone (the tracepack JSONL specification §5 and §9):
// tracepack-jsonl/1 for minor 0 and tracepack-jsonl/2 for every other minor, a minor this module does not know included;
// every export of the conformance corpus is reproduced byte for byte under the schema of its pack's minor.
func TestExportSchemaByMinor(t *testing.T) {
	t.Parallel()

	exports, err := filepath.Glob(filepath.Join("testdata", "corpus", "*", "export.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, exports)
	for _, path := range exports {
		want, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := os.ReadFile(filepath.Join(filepath.Dir(path), "pack.tpk"))
		require.NoError(t, err)
		r := mustOpen(t, file, ReaderOptions{})
		schema := `{"schema":"tracepack-jsonl/1",`
		if r.Header().FormatMinor != 0 {
			schema = `{"schema":"tracepack-jsonl/2",`
		}

		out, _, err := runExportJSONL(t, r)
		require.NoError(t, err, path)
		require.True(t, strings.HasPrefix(out, schema), path)
		require.Equal(t, string(want), out, path)
	}

	lines := jsonlExportLines(t, jsonlThinnedPack(t))
	assert.True(t, strings.HasPrefix(lines[0], `{"schema":"tracepack-jsonl/2","format_major":1,"format_minor":1,`), lines[0])

	// A minor this module does not know: the tags of format 1.1 are named, and a tag a later minor may define is unknown.
	first, _ := jsonlV2Thinning()
	meta := jsonlTLV(append(jsonlV2Required(), tlv.NestedEntry(0x0033, first), tlv.UTF8Entry(0x0034, "x"))...)
	for _, minor := range []uint16{2, 0xFFFF} {
		r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: minor}, meta)
		out, _, err := runExportJSONL(t, r)
		require.NoError(t, err)
		want := `{"schema":"tracepack-jsonl/2","format_major":1,"format_minor":` + map[uint16]string{2: "2", 0xFFFF: "65535"}[minor] +
			`,"flags":[],"writer_start_utc_ns":"0","pack_id":"00000000-0000-0000-0000-000000000000",` +
			`"capture_id":"00000000-0000-0000-0000-000000000000","metadata":{` + jsonlV2RequiredJSON + `,` +
			`"thinning":[{"stream":"1","function":"3","source":"fdc","unknown":[{"tag":9,"value_type":4,"value":"CQAAAAAAAAA="}]}],` +
			`"unknown":[{"tag":52,"value_type":6,"value":"eA=="}]}}` + "\n"
		assert.Equal(t, want, out, "minor %d", minor)
	}
}

// Under tracepack-jsonl/2 the numbers format 1.1 assigns are named: thinning and its nested tags,
// encryption_algorithm by its enum name, encryption_key_id, siv, and the quality bit 7 unobserved;
// under tracepack-jsonl/1 the same numbers in a pack of minor 0 are unknown and bit(7)
// (the tracepack JSONL specification §3.1, §3.2 and §4).
func TestExportV2Names(t *testing.T) {
	t.Parallel()

	meta := jsonlV2Meta()
	first, second := jsonlV2Thinning()
	b64 := base64.StdEncoding.EncodeToString

	// Written by hand from the tracepack JSONL specification §3–§5.
	policy := `"redaction_policy":{"policy_id":"p","policy_version":"1","key_id":"k","digest_algorithm":"hmac-sha256"`
	entry := `"redaction":[{"seq":"5","masked_ranges":"DgAAAAAAAAAEAAAAAAAAAA==","domain":"d","digest":"q83v"`
	want2 := jsonlV2HeaderStart("2", 1) + jsonlV2RequiredJSON + `,` +
		policy + `,"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"test-enc-1"},` +
		entry + `,"siv":"` + b64(jsonlV2SIV) + `"}],` +
		`"thinning":[{"stream":"1","function":"3","source":"fdc","unknown":[{"tag":9,"value_type":4,"value":"CQAAAAAAAAA="}]},` +
		`{"stream":"1","function":"4","source":"fdc"}]}}` + "\n"
	want1 := jsonlV2HeaderStart("1", 0) + jsonlV2RequiredJSON + `,` +
		policy + `,"unknown":[{"tag":6,"value_type":6,"value":"dGVzdC1lbmMtMQ=="},{"tag":5,"value_type":1,"value":"AQ=="}]},` +
		entry + `,"unknown":[{"tag":6,"value_type":7,"value":"` + b64(jsonlV2SIV) + `"}]}],` +
		`"unknown":[{"tag":51,"value_type":8,"value":"` + b64(jsonlTLV(first...)) + `"},` +
		`{"tag":51,"value_type":8,"value":"` + b64(jsonlTLV(second...)) + `"}]}}` + "\n"

	r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: 1}, meta)
	assert.Equal(t, want2, renderJSONLHeader(t, r))
	r = jsonlTestReader(t, format.FileHeader{FormatMajor: 1}, meta)
	assert.Equal(t, want1, renderJSONLHeader(t, r))

	// The same records in a pack of minor 0: the stored bit 7 is reserved there, and thinning unknown.
	file := patchHeader(t, jsonlThinnedPack(t), func(h *format.FileHeader) { h.FormatMinor = 0 })
	lines := jsonlExportLines(t, file)
	require.Len(t, lines, 3)
	assert.True(t, strings.HasPrefix(lines[0], `{"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0,`), lines[0])
	assert.Contains(t, lines[0], `"unknown":[{"tag":51,"value_type":8,`)
	assert.NotContains(t, lines[0], `"thinning"`)
	assert.Equal(t, []string{}, parseJSONLRecord(t, lines[1]).Quality)
	assert.Equal(t, []string{"bit(7)"}, parseJSONLRecord(t, lines[2]).Quality)
}

// The keys of the objects format 1.1 adds or extends follow ascending tag order, whatever the stored order,
// with unknown nested tags last (the tracepack JSONL specification §4).
func TestExportV2NestedOrder(t *testing.T) {
	t.Parallel()

	r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: 1}, jsonlV2Meta())
	var line struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(renderJSONLHeader(t, r)), &line))
	keys := jsonlTopKeys(t, string(line.Metadata))
	assert.Equal(t, []string{"redaction_policy", "redaction", "thinning"}, keys[len(keys)-3:], "the metadata keys in tag order")

	var meta struct {
		Policy    json.RawMessage   `json:"redaction_policy"`
		Redaction []json.RawMessage `json:"redaction"`
		Thinning  []json.RawMessage `json:"thinning"`
	}
	require.NoError(t, json.Unmarshal(line.Metadata, &meta))
	assert.Equal(t, []string{"policy_id", "policy_version", "key_id", "digest_algorithm", "encryption_algorithm", "encryption_key_id"},
		jsonlTopKeys(t, string(meta.Policy)))
	require.Len(t, meta.Redaction, 1)
	assert.Equal(t, []string{"seq", "masked_ranges", "domain", "digest", "siv"}, jsonlTopKeys(t, string(meta.Redaction[0])))
	require.Len(t, meta.Thinning, 2)
	assert.Equal(t, []string{"stream", "function", "source", "unknown"}, jsonlTopKeys(t, string(meta.Thinning[0])))
	assert.Equal(t, []string{"stream", "function", "source"}, jsonlTopKeys(t, string(meta.Thinning[1])))
}

// An unobserved record written by the Writer exports with quality ["unobserved"] under tracepack-jsonl/2,
// beside a record of the same stream and function whose quality is empty (the tracepack JSONL specification §3.2).
func TestExportV2Unobserved(t *testing.T) {
	t.Parallel()

	lines := jsonlExportLines(t, jsonlThinnedPack(t))
	require.Len(t, lines, 3)
	assert.True(t, strings.HasPrefix(lines[0], `{"schema":"tracepack-jsonl/2",`), lines[0])
	assert.Contains(t, lines[0], `"thinning":[{"stream":"1","function":"3","source":"fdc interval poll"},`+
		`{"stream":"1","function":"4","source":"fdc interval poll"}]`)

	plain, unobserved := parseJSONLRecord(t, lines[1]), parseJSONLRecord(t, lines[2])
	assert.Equal(t, "0", plain.Seq)
	assert.Equal(t, []string{}, plain.Quality)
	assert.Equal(t, "1", unobserved.Seq)
	assert.Equal(t, []string{"unobserved"}, unobserved.Quality)
	assert.Equal(t, "not-attempted", unobserved.DecodeStatus)
	assert.Contains(t, lines[2], `"payload_len":20,"trailing_bytes":0,"quality":["unobserved"],"kind":"data",`)
}
