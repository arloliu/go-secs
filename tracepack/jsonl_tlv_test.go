package tracepack

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack/internal/format"
	"github.com/arloliu/go-secs/tracepack/internal/tlv"
)

// jsonlTLV encodes entries, in order, as an entry list.
func jsonlTLV(entries ...tlv.Entry) []byte {
	var b []byte
	for _, e := range entries {
		b = tlv.AppendEntry(b, e)
	}

	return b
}

// jsonlUUID returns a UUID whose 16 bytes are all c.
func jsonlUUID(c byte) [16]byte {
	var u [16]byte
	for i := range u {
		u[i] = c
	}

	return u
}

// jsonlTestReader opens a pack that holds only a file header and the pack metadata meta,
// with the header's flags stored as given, reserved bits included.
func jsonlTestReader(t *testing.T, hdr format.FileHeader, meta []byte) *Reader {
	t.Helper()

	return mustOpen(t, jsonlHeadOnlyPack(hdr, meta), ReaderOptions{})
}

// jsonlHeadOnlyPack returns a pack that holds only a file header and the pack metadata meta,
// with the header's flags stored as given, reserved bits included.
func jsonlHeadOnlyPack(hdr format.FileHeader, meta []byte) []byte {
	hdr.PackMetadataLen = uint32(len(meta))
	hdr.PackMetadataCRC = format.CRC(meta)
	file := format.AppendFileHeader(nil, &hdr)
	binary.LittleEndian.PutUint32(file[12:], hdr.Flags)
	binary.LittleEndian.PutUint32(file[76:], format.CRC(file[:76]))

	return append(file, meta...)
}

// renderJSONLHeader returns the header line writeJSONLHeader writes for r.
func renderJSONLHeader(t *testing.T, r *Reader) string {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, writeJSONLHeader(newJSONLWriter(&b), r))

	return b.String()
}

// renderJSONLObject returns the TLV object reg renders for the entry list b, without the line's LF.
func renderJSONLObject(t *testing.T, reg *jsonlRegistry, b []byte) string {
	t.Helper()

	line := renderJSONL(t, func(jw *jsonlWriter) { reg.render(jw, b) })

	return strings.TrimSuffix(line, "\n")
}

// jsonlEveryTagMeta returns a pack metadata holding every known tag,
// with repeated, out-of-order, unknown, private and retired entries and unknown nested tags.
func jsonlEveryTagMeta() []byte {
	u64s := func(vs ...uint64) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.LittleEndian.AppendUint64(b, v)
		}

		return b
	}

	return jsonlTLV(
		tlv.U64Entry(0x0027, 0),
		tlv.UTF8Entry(0x0002, "EQ-01"),
		tlv.U8Entry(0x0003, 1),
		tlv.U8Entry(0x0004, 3),
		tlv.U8Entry(0x0005, 9),
		tlv.UTF8Entry(0x0006, "rec 1.0"),
		tlv.UTF8Entry(0x0007, ""),
		tlv.UTF8Entry(0x0008, "c/1"),
		tlv.Entry{Tag: 0x0014, Type: tlv.TypeU64, Value: u64s(1)},
		tlv.UTF8Entry(0x0008, "c/1"),
		tlv.U8Entry(0x0009, 1),
		tlv.I64Entry(0x000A, -1),
		tlv.I64Entry(0x000B, 0),
		tlv.UTF8Entry(0x000C, "Asia/Taipei"),
		tlv.UTF8Entry(0x000D, "secsgem/1"),
		tlv.UTF8Entry(0x000E, "a.log"),
		tlv.BytesEntry(0x8001, []byte{1, 2, 3}),
		tlv.UTF8Entry(0x000E, "b.log"),
		tlv.U64Entry(0x000F, 65536),
		tlv.I64Entry(0x0011, 3600000000000),
		tlv.I64Entry(0x0010, 0),
		tlv.U8Entry(0x0012, 2),
		tlv.BoolEntry(0x0013, false),
		tlv.UUIDEntry(0x0015, jsonlUUID(0x11)),
		tlv.NestedEntry(0x0016, []tlv.Entry{
			tlv.UUIDEntry(0x0001, jsonlUUID(0xAB)),
			tlv.U64Entry(0x0002, 1),
			tlv.U64Entry(0x0003, tlv.MaxU64),
			tlv.I64Entry(0x0004, -1<<63),
			tlv.UTF8Entry(0x0009, "x"),
			tlv.I64Entry(0x0005, 0),
		}),
		tlv.NestedEntry(0x0016, nil),
		tlv.UTF8Entry(0x0017, "\xef\xbb\xbfline1\nline2"),
		tlv.U8Entry(0x0018, 5),
		tlv.U8Entry(0x0019, 0),
		tlv.UUIDEntry(0x001A, jsonlUUID(0x22)),
		tlv.UUIDEntry(0x001B, jsonlUUID(0x33)),
		tlv.UUIDEntry(0x001C, jsonlUUID(0x44)),
		tlv.UTF8Entry(0x001D, "seq>=1"),
		tlv.UTF8Entry(0x001E, "F12"),
		tlv.UTF8Entry(0x001F, "M"),
		tlv.UTF8Entry(0x0020, "1.2"),
		tlv.UTF8Entry(0x0021, "EAP 3"),
		tlv.UTF8Entry(0x0022, "10.0.0.1:5000"),
		tlv.UTF8Entry(0x0023, "10.0.0.2:5000"),
		tlv.U8Entry(0x0024, 1),
		tlv.U64Entry(0x0025, 0),
		tlv.NestedEntry(0x0026, []tlv.Entry{
			tlv.U64Entry(0x0003, 45000),
			tlv.U64Entry(0x0009, 9),
			tlv.U64Entry(0x0002, 10000),
		}),
		tlv.U64Entry(0x0028, 1000000),
		tlv.UUIDEntry(0x0029, jsonlUUID(0x55)),
		tlv.U64Entry(0x002A, 1),
		tlv.U64Entry(0x002B, 0),
		tlv.U64Entry(0x002C, 5000000000),
		tlv.U64Entry(0x002D, 1),
		tlv.U64Entry(0x002E, 7),
		tlv.UUIDEntry(0x002F, jsonlUUID(0x66)),
		tlv.NestedEntry(0x0031, []tlv.Entry{
			tlv.UTF8Entry(0x0001, "p"),
			tlv.U64Entry(0x0002, 2),
			tlv.BytesEntry(0x0010, nil),
			tlv.UTF8Entry(0x0006, "e1"),
			tlv.UTF8Entry(0x0003, "k1"),
			tlv.U8Entry(0x0004, 1),
			tlv.U8Entry(0x0005, 1),
		}),
		tlv.NestedEntry(0x0032, []tlv.Entry{
			tlv.U64Entry(0x0001, 5),
			tlv.UTF8Entry(0x0002, ""),
			tlv.BytesEntry(0x0003, u64s(14, 4)),
			tlv.UTF8Entry(0x0004, "d"),
			tlv.BytesEntry(0x0005, []byte{0xAB, 0xCD, 0xEF}),
			tlv.BytesEntry(0x0006, bytes.Repeat([]byte{0xFF}, 16)),
			{Tag: 0x0007, Type: tlv.TypeBool, Value: []byte{2}},
		}),
		tlv.NestedEntry(0x0033, []tlv.Entry{
			tlv.UTF8Entry(0x0003, "s"),
			tlv.U64Entry(0x0002, 3),
			tlv.U64Entry(0x0001, 1),
			tlv.UTF8Entry(0x0004, "x"),
		}),
		tlv.Entry{Tag: 0x0040, Type: 9, Value: []byte("zz")},
	)
}

// A pack metadata holding every known tag, with repeated, out-of-order, unknown, private and retired entries
// and unknown nested tags, renders as the tracepack JSONL specification §4 and §5 define.
func TestJSONLHeaderLine_EveryKnownTag(t *testing.T) {
	t.Parallel()

	meta := jsonlEveryTagMeta()
	hdr := format.FileHeader{
		FormatMajor:      1,
		FormatMinor:      3,
		Flags:            0x8000_0001,
		WriterStartUTCNs: 1791000000000000000,
		PackID:           format.UUID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		CaptureID:        jsonlUUID(0xAB),
	}

	// Written by hand from the tracepack JSONL specification §3–§5.
	want := `{"schema":"tracepack-jsonl/2","format_major":1,"format_minor":3,"flags":["redaction-present","bit(31)"],` +
		`"writer_start_utc_ns":"1791000000000000000",` +
		`"pack_id":"00010203-0405-0607-0809-0a0b0c0d0e0f","capture_id":"abababab-abab-abab-abab-abababababab",` +
		`"metadata":{` +
		`"tool_id":"EQ-01","transport":"hsms-ss","capture_method":"log","vantage":"unknown(9)","recorder":"rec 1.0","writer":"",` +
		`"classifier":["c/1","c/1"],"time_source":"capture-clock","capture_origin_utc_ns":"-1","capture_origin_mono_ns":"0",` +
		`"source_tz":"Asia/Taipei","source_dialect":"secsgem/1","source_ref":["a.log","b.log"],"max_frame_len":["65536"],` +
		`"period_start":"0","period_end":"3600000000000","lifecycle_coverage":"none","quality_evaluated":false,` +
		`"supersedes":["11111111-1111-1111-1111-111111111111"],` +
		`"coverage":[{"capture_id":"abababab-abab-abab-abab-abababababab","seq_first":"1","seq_last":"9223372036854775807",` +
		`"time_start":"-9223372036854775808","time_end":"0","unknown":[{"tag":9,"value_type":6,"value":"eA=="}]},{}],` +
		"\"notes\":\"\xef\xbb\xbfline1\\nline2\"," +
		`"pack_role":"unknown(5)","compaction_level":0,"compacted_from":["22222222-2222-2222-2222-222222222222"],` +
		`"recorder_instance_id":"33333333-3333-3333-3333-333333333333","previous_capture_id":"44444444-4444-4444-4444-444444444444",` +
		`"extract_filter":"seq>=1","site_id":"F12","equipment_model":"M","equipment_sw_rev":"1.2","host_software":"EAP 3",` +
		`"host_endpoint":"10.0.0.1:5000","equipment_endpoint":"10.0.0.2:5000","equipment_connect_mode":"passive","device_id":"0",` +
		`"hsms_timers":{"T2":"10000","T3":"45000","unknown":[{"tag":9,"value_type":4,"value":"CQAAAAAAAAA="}]},` +
		`"seq_start":"0","clock_step_tolerance_ns":"1000000","replacement_set_id":"55555555-5555-5555-5555-555555555555",` +
		`"replacement_set_size":"1","replacement_set_index":"0","flush_interval_ns":"5000000000","scope_generation":"1",` +
		`"publisher_epoch":"7","patch_base":"66666666-6666-6666-6666-666666666666",` +
		`"redaction_policy":{"policy_id":"p","policy_version":"2","key_id":"k1","digest_algorithm":"hmac-sha256",` +
		`"encryption_algorithm":"aes-siv-cmac-512","encryption_key_id":"e1","unknown":[{"tag":16,"value_type":7,"value":""}]},` +
		`"redaction":[{"seq":"5","item_path":"","masked_ranges":"DgAAAAAAAAAEAAAAAAAAAA==","domain":"d","digest":"q83v",` +
		`"siv":"/////////////////////w==","unknown":[{"tag":7,"value_type":2,"value":"Ag=="}]}],` +
		`"thinning":[{"stream":"1","function":"3","source":"s","unknown":[{"tag":4,"value_type":6,"value":"eA=="}]}],` +
		`"unknown":[{"tag":20,"value_type":4,"value":"AQAAAAAAAAA="},{"tag":32769,"value_type":7,"value":"AQID"},` +
		`{"tag":64,"value_type":9,"value":"eno="}]` +
		"}}\n"

	r := jsonlTestReader(t, hdr, meta)
	got := renderJSONLHeader(t, r)
	require.Equal(t, want, got)
	require.True(t, json.Valid([]byte(got)))

	keys := slices.Sorted(maps.Keys(tlv.PackMetadata.AtMinor(hdr.FormatMinor)))
	var obj struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &obj))
	assert.Len(t, obj.Metadata, len(keys)+1, "a key per known tag, and unknown")
}

// A pack metadata holding only the required tags, with zero, false and empty values and an empty nested list,
// renders every present value and no absent tag.
func TestJSONLHeaderLine_ZeroValuesBesideAbsentTags(t *testing.T) {
	t.Parallel()

	meta := jsonlTLV(
		tlv.UTF8Entry(0x0002, ""),
		tlv.U8Entry(0x0003, 0),
		tlv.U8Entry(0x0004, 0),
		tlv.U8Entry(0x0005, 0),
		tlv.UTF8Entry(0x0006, ""),
		tlv.UTF8Entry(0x0007, ""),
		tlv.U8Entry(0x0009, 0),
		tlv.I64Entry(0x0010, 0),
		tlv.I64Entry(0x0011, 0),
		tlv.U8Entry(0x0012, 0),
		tlv.BoolEntry(0x0013, false),
		tlv.U8Entry(0x0018, 0),
		tlv.U8Entry(0x0019, 0),
		tlv.UUIDEntry(0x001B, [16]byte{}),
		tlv.NestedEntry(0x0026, nil),
		tlv.U64Entry(0x0027, 0),
		tlv.U64Entry(0x002D, 0),
	)

	// Written by hand from the tracepack JSONL specification §3–§5.
	want := `{"schema":"tracepack-jsonl/1","format_major":1,"format_minor":0,"flags":[],"writer_start_utc_ns":"0",` +
		`"pack_id":"00000000-0000-0000-0000-000000000000","capture_id":"00000000-0000-0000-0000-000000000000",` +
		`"metadata":{"tool_id":"","transport":"unknown","capture_method":"unknown","vantage":"unknown","recorder":"","writer":"",` +
		`"time_source":"unknown","period_start":"0","period_end":"0","lifecycle_coverage":"unknown","quality_evaluated":false,` +
		`"pack_role":"unknown","compaction_level":0,"recorder_instance_id":"00000000-0000-0000-0000-000000000000",` +
		`"hsms_timers":{},"seq_start":"0","scope_generation":"0"}}` + "\n"

	r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1}, meta)
	assert.Equal(t, want, renderJSONLHeader(t, r))
}

// The header line keeps the first write error, wrapped.
func TestJSONLHeaderLine_WriteError(t *testing.T) {
	t.Parallel()

	r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1}, mustMarshalMeta(t, readerTestMeta()))
	w := &failingWriter{err: errJSONLWriteBoom}
	err := writeJSONLHeader(newJSONLWriter(w), r)
	require.ErrorIs(t, err, errJSONLWriteBoom)
	assert.Equal(t, 1, w.calls)
}

// Every enum-typed tag renders an unknown value as unknown(<n>) and the value 0 by its name,
// and hsms_timers names each timer it holds.
func TestJSONLRender_UnknownAndZeroEnums(t *testing.T) {
	t.Parallel()

	meta := func(enum uint8, policy []tlv.Entry, timers ...tlv.Entry) []byte {
		return jsonlTLV(
			tlv.UTF8Entry(0x0002, "t"), tlv.U8Entry(0x0003, enum), tlv.U8Entry(0x0004, enum), tlv.U8Entry(0x0005, 1),
			tlv.UTF8Entry(0x0006, "r"), tlv.UTF8Entry(0x0007, "w"), tlv.U8Entry(0x0009, enum), tlv.I64Entry(0x0010, 0),
			tlv.I64Entry(0x0011, 1), tlv.U8Entry(0x0012, enum), tlv.BoolEntry(0x0013, true), tlv.U8Entry(0x0018, 1),
			tlv.U8Entry(0x0019, 0), tlv.UUIDEntry(0x001B, jsonlUUID(1)), tlv.U8Entry(0x0024, enum),
			tlv.NestedEntry(0x0026, timers), tlv.U64Entry(0x0027, 0), tlv.NestedEntry(0x0031, policy),
		)
	}
	policy := func(digest uint8) []tlv.Entry {
		return []tlv.Entry{tlv.UTF8Entry(1, "p"), tlv.U64Entry(2, 1), tlv.UTF8Entry(3, "k"), tlv.U8Entry(4, digest)}
	}

	tests := []struct {
		name string
		reg  *jsonlRegistry
		in   []byte
		want string // written by hand from the tracepack JSONL specification §3.1 and §4
	}{
		{
			"pack metadata, unknown values", jsonlPackMetadataRegistry,
			meta(9, policy(2), tlv.U64Entry(1, 1), tlv.U64Entry(4, 4), tlv.U64Entry(5, 5), tlv.U64Entry(6, 6), tlv.U64Entry(7, 7), tlv.U64Entry(8, 8)),
			`{"tool_id":"t","transport":"unknown(9)","capture_method":"unknown(9)","vantage":"host","recorder":"r","writer":"w",` +
				`"time_source":"unknown(9)","period_start":"0","period_end":"1","lifecycle_coverage":"unknown(9)","quality_evaluated":true,` +
				`"pack_role":"segment","compaction_level":0,"recorder_instance_id":"01010101-0101-0101-0101-010101010101",` +
				`"equipment_connect_mode":"unknown(9)","hsms_timers":{"T1":"1","T4":"4","T5":"5","T6":"6","T7":"7","T8":"8"},` +
				`"seq_start":"0","redaction_policy":{"policy_id":"p","policy_version":"1","key_id":"k","digest_algorithm":"unknown(2)"}}`,
		},
		{
			"pack metadata, zero values", jsonlPackMetadataRegistry, meta(0, policy(0)),
			`{"tool_id":"t","transport":"unknown","capture_method":"unknown","vantage":"host","recorder":"r","writer":"w",` +
				`"time_source":"unknown","period_start":"0","period_end":"1","lifecycle_coverage":"unknown","quality_evaluated":true,` +
				`"pack_role":"segment","compaction_level":0,"recorder_instance_id":"01010101-0101-0101-0101-010101010101",` +
				`"equipment_connect_mode":"unknown","hsms_timers":{},` +
				`"seq_start":"0","redaction_policy":{"policy_id":"p","policy_version":"1","key_id":"k","digest_algorithm":"unknown"}}`,
		},
		{
			"redaction_policy /2, unknown values", jsonlRedactionPolicyRegistryV2,
			jsonlTLV(append(policy(2), tlv.U8Entry(5, 2), tlv.UTF8Entry(6, "e"))...),
			`{"policy_id":"p","policy_version":"1","key_id":"k","digest_algorithm":"unknown(2)",` +
				`"encryption_algorithm":"unknown(2)","encryption_key_id":"e"}`,
		},
		{
			"redaction_policy /2, zero values", jsonlRedactionPolicyRegistryV2,
			jsonlTLV(append(policy(0), tlv.U8Entry(5, 0), tlv.UTF8Entry(6, ""))...),
			`{"policy_id":"p","policy_version":"1","key_id":"k","digest_algorithm":"unknown",` +
				`"encryption_algorithm":"unknown","encryption_key_id":""}`,
		},
		{
			"transport-event, unknown event, timer and boundary_kind", jsonlTransportEventRegistry,
			jsonlTLV(tlv.U8Entry(0x0001, 8), tlv.U8Entry(0x0006, 9), tlv.U8Entry(0x000D, 5)),
			`{"event":"unknown(8)","timer":"unknown(9)","boundary_kind":"unknown(5)"}`,
		},
		{
			"annotation, unknown annotation_kind", jsonlAnnotationRegistry,
			jsonlTLV(tlv.U8Entry(0x0001, 5), tlv.UTF8Entry(0x0005, "")),
			`{"annotation_kind":"unknown(5)","text":""}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := tt.reg.valid(tt.in, 0)
			require.True(t, ok, "the list is valid under its registry")
			assert.Equal(t, tt.want, renderJSONLObject(t, tt.reg, tt.in))
		})
	}
}

// FuzzJSONLPackMetadata checks that, under each schema, the validity walk accepts exactly the pack metadata
// tlv.ValidateMinor accepts at the minor of a pack the schema is written for
// (0 for tracepack-jsonl/1, 1 for tracepack-jsonl/2),
// and that a valid pack metadata renders as one valid JSON object.
func FuzzJSONLPackMetadata(f *testing.F) {
	plain, err := readerTestMeta().MarshalBinary()
	require.NoError(f, err)
	f.Add(jsonlEveryTagMeta())
	f.Add(plain)
	f.Add(jsonlV2Meta())

	f.Fuzz(func(t *testing.T, b []byte) {
		entries, decodeErr := tlv.Decode(b)
		for _, minor := range []uint16{0, 1} {
			reg := jsonlSchemaOf(minor).meta
			_, got := reg.valid(b, 0)
			want := decodeErr == nil && tlv.ValidateMinor(entries, tlv.PackMetadata, minor) == nil
			require.Equal(t, want, got, "minor %d", minor)
			if !got {
				continue
			}

			var out bytes.Buffer
			jw := newJSONLWriter(&out)
			reg.render(jw, b)
			require.NoError(t, jw.endLine())
			require.True(t, json.Valid(out.Bytes()), "minor %d: %q", minor, out.Bytes())
		}
	})
}

// Transport-event and annotation bodies render every tag with its enum names, in ascending tag order.
func TestJSONLRender_Bodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		reg  *jsonlRegistry
		in   []byte
		want string // written by hand from the tracepack JSONL specification §3 and §4
	}{
		{
			"transport-event, every tag, stored in reverse",
			jsonlTransportEventRegistry,
			jsonlTLV(
				tlv.I64Entry(0x0013, -5),
				tlv.UTF8Entry(0x0012, "a<b>&/\u2028"),
				tlv.I64Entry(0x0011, 2),
				tlv.I64Entry(0x0010, 1),
				tlv.U64Entry(0x000F, 9),
				tlv.U64Entry(0x000E, 8),
				tlv.U8Entry(0x000D, 4),
				tlv.BytesEntry(0x000C, []byte{0, 0, 0, 7}),
				tlv.U64Entry(0x000B, 3),
				tlv.U64Entry(0x000A, 1),
				tlv.U64Entry(0x0009, 0),
				tlv.BoolEntry(0x0008, true),
				tlv.U8Entry(0x0007, 2),
				tlv.U8Entry(0x0006, 3),
				tlv.UTF8Entry(0x0005, "vendor:X"),
				tlv.U8Entry(0x0004, 9),
				tlv.U8Entry(0x0003, 3),
				tlv.U8Entry(0x0002, 2),
				tlv.U8Entry(0x0001, 1),
			),
			`{"event":"state-transition","prev_state":"not-selected","cur_state":"selected","cause":"timer-expiry",` +
				`"cause_raw":"vendor:X","timer":"T3","socket_role":"active","inferred":true,"primary_session_id":"0",` +
				`"primary_stream":"1","primary_function":"3","primary_system_bytes":"AAAABw==","boundary_kind":"stop-unclean",` +
				`"boundary_seq_first":"8","boundary_seq_last":"9","gap_start":"1","gap_end":"2","detail":"a<b>&/` + "\u2028" + `",` +
				`"clock_step_ns":"-5"}`,
		},
		{
			"transport-event, enums at zero and unknown",
			jsonlTransportEventRegistry,
			jsonlTLV(
				tlv.U8Entry(0x0001, 0),
				tlv.U8Entry(0x0002, 0),
				tlv.U8Entry(0x0003, 200),
				tlv.U8Entry(0x0004, 14),
				tlv.U8Entry(0x0006, 0),
				tlv.U8Entry(0x0007, 3),
				tlv.U8Entry(0x000D, 0),
			),
			`{"event":"unknown","prev_state":"unknown","cur_state":"unknown(200)","cause":"unknown(14)","timer":"none",` +
				`"socket_role":"unknown(3)","boundary_kind":"unknown"}`,
		},
		{
			"annotation, every tag",
			jsonlAnnotationRegistry,
			jsonlTLV(
				tlv.U64Entry(0x0009, 4),
				tlv.U64Entry(0x0008, 3),
				tlv.U64Entry(0x0007, 2),
				tlv.BytesEntry(0x0006, []byte{0xFF}),
				tlv.UTF8Entry(0x0005, ""),
				tlv.U64Entry(0x0004, 1),
				tlv.U64Entry(0x0003, 0),
				tlv.UUIDEntry(0x0002, jsonlUUID(0xCD)),
				tlv.U8Entry(0x0001, 4),
			),
			`{"annotation_kind":"unrecognised-line","ref_capture_id":"cdcdcdcd-cdcd-cdcd-cdcd-cdcdcdcdcdcd","ref_seq_first":"0",` +
				`"ref_seq_last":"1","text":"","raw":"/w==","source_index":"2","source_offset":"3","source_len":"4"}`,
		},
		{
			"annotation, leading BOM kept and unknown entries opaque",
			jsonlAnnotationRegistry,
			jsonlTLV(
				tlv.Entry{Tag: 0x0050, Type: tlv.TypeUTF8, Value: []byte{0xFF}},
				tlv.U8Entry(0x0001, 0),
				tlv.Entry{Tag: 0x0051, Type: tlv.TypeBool, Value: []byte{2}},
				tlv.UTF8Entry(0x0005, "\xef\xbb\xbfhi"),
				tlv.Entry{Tag: 0x0052, Type: tlv.TypeTLV, Value: []byte{1, 2, 3}},
				tlv.Entry{Tag: 0x9000, Type: 200, Value: nil},
				tlv.Entry{Tag: 0x9000, Type: 200, Value: nil},
			),
			"{\"annotation_kind\":\"unknown\",\"text\":\"\xef\xbb\xbfhi\"," +
				`"unknown":[{"tag":80,"value_type":6,"value":"/w=="},{"tag":81,"value_type":2,"value":"Ag=="},` +
				`{"tag":82,"value_type":8,"value":"AQID"},{"tag":36864,"value_type":200,"value":""},` +
				`{"tag":36864,"value_type":200,"value":""}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := tt.reg.valid(tt.in, 0)
			require.True(t, ok, "the list is valid under its registry")
			assert.Equal(t, tt.want, renderJSONLObject(t, tt.reg, tt.in))
		})
	}
}

// jsonlBodyCase is a payload and whether it is a valid TLV body of its record kind.
type jsonlBodyCase struct {
	name    string
	kind    Kind
	payload []byte
	valid   bool
}

// jsonlBodyCases returns the body validity cases of the tracepack format specification §5 and §8.
func jsonlBodyCases() []jsonlBodyCase {
	note := tlv.U8Entry(0x0001, 1)
	text := tlv.UTF8Entry(0x0005, "t")
	event := tlv.U8Entry(0x0001, 1)
	sysBytes := func(n int) tlv.Entry { return tlv.BytesEntry(0x000C, make([]byte, n)) }
	validNote := jsonlTLV(note, text)
	// A header claiming 100 value bytes, followed by only 2.
	malformed := []byte{0x60, 0x00, byte(tlv.TypeBytes), 0, 100, 0, 0, 0, 1, 2}

	return []jsonlBodyCase{
		{"annotation with text", KindAnnotation, validNote, true},
		{"annotation with raw", KindAnnotation, jsonlTLV(note, tlv.BytesEntry(0x0006, []byte{0xFF})), true},
		{"annotation with both text and raw", KindAnnotation, jsonlTLV(note, text, tlv.BytesEntry(0x0006, nil)), false},
		{"annotation with neither text nor raw", KindAnnotation, jsonlTLV(note), false},
		{"annotation with empty text", KindAnnotation, jsonlTLV(note, tlv.UTF8Entry(0x0005, "")), true},
		{"annotation with empty raw", KindAnnotation, jsonlTLV(note, tlv.BytesEntry(0x0006, nil)), true},
		{"annotation without annotation_kind", KindAnnotation, jsonlTLV(text), false},
		{"annotation with an unknown annotation_kind", KindAnnotation, jsonlTLV(tlv.U8Entry(0x0001, 200), text), true},
		{"annotation_kind of the wrong type", KindAnnotation, jsonlTLV(tlv.U64Entry(0x0001, 1), text), false},
		{"text with a leading BOM", KindAnnotation, jsonlTLV(note, tlv.UTF8Entry(0x0005, "\xef\xbb\xbfa")), true},
		{"known text of invalid UTF-8", KindAnnotation, jsonlTLV(note, tlv.Entry{Tag: 0x0005, Type: tlv.TypeUTF8, Value: []byte{0xFF}}), false},
		{"known u64 of 2^63", KindAnnotation, jsonlTLV(note, text, tlv.U64Entry(0x0003, 1<<63)), false},
		{"known u64 of 2^63-1", KindAnnotation, jsonlTLV(note, text, tlv.U64Entry(0x0003, 1<<63-1)), true},
		{"known tag repeated", KindAnnotation, jsonlTLV(note, text, tlv.U64Entry(0x0003, 1), tlv.U64Entry(0x0003, 1)), false},
		{"unknown utf8 of invalid UTF-8", KindAnnotation, jsonlTLV(note, text, tlv.Entry{Tag: 0x0050, Type: tlv.TypeUTF8, Value: []byte{0xFF}}), true},
		{"unknown bool of 2", KindAnnotation, jsonlTLV(note, text, tlv.Entry{Tag: 0x0050, Type: tlv.TypeBool, Value: []byte{2}}), true},
		{"unknown u64 of 2^63", KindAnnotation, jsonlTLV(note, text, tlv.U64Entry(0x0050, 1<<63)), true},
		{"unknown tlv holding a malformed list", KindAnnotation, jsonlTLV(note, text, tlv.Entry{Tag: 0x0050, Type: tlv.TypeTLV, Value: []byte{1}}), true},
		{"unknown value type", KindAnnotation, jsonlTLV(note, text, tlv.Entry{Tag: 0x0050, Type: 9, Value: []byte{1, 2, 3}}), true},
		{"unknown fixed-length type of the wrong length", KindAnnotation, jsonlTLV(note, text, tlv.Entry{Tag: 0x0050, Type: tlv.TypeU64, Value: []byte{1, 2, 3}}), false},
		{
			"repeated unknown and private tags", KindAnnotation,
			jsonlTLV(note, tlv.UTF8Entry(0x0050, "a"), tlv.UTF8Entry(0x0050, "a"), text, tlv.BytesEntry(0x8000, nil), tlv.BytesEntry(0xFFFF, nil), tlv.BytesEntry(0x8000, nil)),
			true,
		},
		{"tag 0", KindAnnotation, append(bytes.Clone(validNote), 0, 0, byte(tlv.TypeBytes), 0, 0, 0, 0, 0), false},
		{"not a TLV list", KindAnnotation, []byte("abc"), false},
		{"empty payload", KindAnnotation, nil, false},
		{"trailing bytes after the last entry", KindAnnotation, append(bytes.Clone(validNote), 0, 0, 0), false},
		{"a valid prefix and a malformed final entry", KindAnnotation, append(bytes.Clone(validNote), malformed...), false},

		{"transport-event with event only", KindTransportEvent, jsonlTLV(event), true},
		{"transport-event without event", KindTransportEvent, jsonlTLV(tlv.U8Entry(0x0002, 1)), false},
		{
			"transport-event with unknown enum values", KindTransportEvent,
			jsonlTLV(tlv.U8Entry(0x0001, 99), tlv.U8Entry(0x0002, 99), tlv.U8Entry(0x0004, 99), tlv.U8Entry(0x0006, 99), tlv.U8Entry(0x000D, 99)),
			true,
		},
		{"primary_system_bytes of 0 bytes", KindTransportEvent, jsonlTLV(event, sysBytes(0)), false},
		{"primary_system_bytes of 3 bytes", KindTransportEvent, jsonlTLV(event, sysBytes(3)), false},
		{"primary_system_bytes of 4 bytes", KindTransportEvent, jsonlTLV(event, sysBytes(4)), true},
		{"primary_system_bytes of 5 bytes", KindTransportEvent, jsonlTLV(event, sysBytes(5)), false},
		{"known bool of 2", KindTransportEvent, jsonlTLV(event, tlv.Entry{Tag: 0x0008, Type: tlv.TypeBool, Value: []byte{2}}), false},
		{"known utf8 of invalid UTF-8", KindTransportEvent, jsonlTLV(event, tlv.Entry{Tag: 0x0012, Type: tlv.TypeUTF8, Value: []byte{0xC3}}), false},
		{"known u64 of 2^63", KindTransportEvent, jsonlTLV(event, tlv.U64Entry(0x000E, 1<<63)), false},
		{"unknown entries opaque", KindTransportEvent, jsonlTLV(tlv.Entry{Tag: 0x0050, Type: tlv.TypeBool, Value: []byte{2}}, event), true},
		{"an annotation body as a transport-event", KindTransportEvent, validNote, true},
		{"a transport-event body with text and raw", KindTransportEvent, jsonlTLV(event, tlv.UTF8Entry(0x0005, "t"), tlv.BytesEntry(0x0006, nil)), false},
		{"a data record checked as an annotation", KindData, validNote, true},
	}
}

// jsonlValidBody decides exactly the valid TLV bodies of the tracepack format specification §8,
// and agrees with checkEventPayload on every case.
func TestJSONLValidBody(t *testing.T) {
	t.Parallel()

	for _, tt := range jsonlBodyCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.valid, jsonlValidBody(tt.kind, tt.payload))
			assert.Equal(t, checkEventPayload(tt.kind, tt.payload) == nil, jsonlValidBody(tt.kind, tt.payload),
				"the predicate agrees with checkEventPayload")
		})
	}
}

// jsonlValidBody agrees with checkEventPayload on random bodies built from valid and damaged entries.
func TestJSONLValidBody_RandomAgreement(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	cases := jsonlBodyCases()
	valid := 0
	for range 20000 {
		kind := KindTransportEvent
		if rng.IntN(2) == 0 {
			kind = KindAnnotation
		}
		payload := randomJSONLBody(rng, cases)
		got := jsonlValidBody(kind, payload)
		require.Equal(t, checkEventPayload(kind, payload) == nil, got, "kind %v payload %x", kind, payload)
		if got {
			valid++
		}
	}
	assert.Greater(t, valid, 500, "the generator reaches valid bodies")
}

// randomJSONLBody builds a payload from a random mix of entries known to the body registries, unknown entries,
// and pieces of the case payloads, sometimes with a byte changed or cut.
func randomJSONLBody(rng *rand.Rand, cases []jsonlBodyCase) []byte {
	var b []byte
	for range rng.IntN(5) {
		switch rng.IntN(6) {
		case 0:
			b = tlv.AppendEntry(b, tlv.U8Entry(1, uint8(rng.IntN(3))))
		case 1:
			b = tlv.AppendEntry(b, tlv.UTF8Entry(5, "t"))
		case 2:
			b = tlv.AppendEntry(b, tlv.BytesEntry(uint16(rng.IntN(14)+1), make([]byte, rng.IntN(6))))
		case 3:
			b = tlv.AppendEntry(b, tlv.Entry{Tag: uint16(rng.IntN(20) + 1), Type: tlv.ValueType(rng.IntN(10)), Value: make([]byte, []int{0, 1, 8, 16}[rng.IntN(4)])})
		case 4:
			b = append(b, cases[rng.IntN(len(cases))].payload...)
		default:
			b = tlv.AppendEntry(b, tlv.U64Entry(uint16(rng.IntN(10)+1), rng.Uint64()>>rng.IntN(2)))
		}
	}
	if len(b) > 0 && rng.IntN(4) == 0 {
		b[rng.IntN(len(b))] ^= byte(rng.IntN(255) + 1)
	}
	if len(b) > 0 && rng.IntN(8) == 0 {
		b = b[:rng.IntN(len(b))]
	}

	return b
}

// FuzzJSONLValidBody checks that the body predicate agrees with checkEventPayload,
// and that a valid body renders as one valid JSON object.
func FuzzJSONLValidBody(f *testing.F) {
	for _, c := range jsonlBodyCases() {
		f.Add(uint8(c.kind), c.payload)
	}

	f.Fuzz(func(t *testing.T, kind uint8, payload []byte) {
		k := Kind(kind)
		got := jsonlValidBody(k, payload)
		require.Equal(t, checkEventPayload(k, payload) == nil, got)
		if !got {
			return
		}

		reg := jsonlAnnotationRegistry
		if k == KindTransportEvent {
			reg = jsonlTransportEventRegistry
		}
		var out bytes.Buffer
		jw := newJSONLWriter(&out)
		reg.render(jw, payload)
		require.NoError(t, jw.endLine())
		require.True(t, json.Valid(out.Bytes()), "%q", out.Bytes())
	})
}

// The validity walk applies the rules tlv.Validate applies, the nesting limit counted the same way.
func TestJSONLValid_NestingDepthLimit(t *testing.T) {
	t.Parallel()

	chainReg := tlv.Registry{}
	chainReg[1] = tlv.Field{Name: "next", Type: tlv.TypeTLV, Nested: &chainReg}
	chain := &jsonlRegistry{reg: chainReg}
	chain.nested = map[uint16]*jsonlRegistry{1: chain}
	nest := func(levels int) []byte {
		e := tlv.NestedEntry(1, nil)
		for range levels - 1 {
			e = tlv.NestedEntry(1, []tlv.Entry{e})
		}

		return jsonlTLV(e)
	}

	for _, levels := range []int{1, tlv.MaxNestingDepth, tlv.MaxNestingDepth + 1, tlv.MaxNestingDepth + 5} {
		b := nest(levels)
		entries, err := tlv.Decode(b)
		require.NoError(t, err)
		_, ok := chain.valid(b, 0)
		assert.Equal(t, tlv.Validate(entries, chainReg) == nil, ok, "%d levels", levels)
		assert.Equal(t, levels <= tlv.MaxNestingDepth, ok, "%d levels", levels)
	}
}

// Under each schema, the validity walk rejects what tlv.ValidateMinor rejects in nested values,
// and accepts what it accepts, at the minor of a pack the schema is written for:
// under tracepack-jsonl/1 the numbers format 1.1 assigns are unknown, whatever their value types,
// and under tracepack-jsonl/2 they are checked as format 1.1 defines them.
func TestJSONLValid_AgreesWithValidate(t *testing.T) {
	t.Parallel()

	policy := []tlv.Entry{tlv.UTF8Entry(1, "p"), tlv.U64Entry(2, 1), tlv.UTF8Entry(3, "k"), tlv.U8Entry(4, 1)}
	required := []tlv.Entry{
		tlv.UTF8Entry(0x0002, "t"), tlv.U8Entry(0x0003, 1), tlv.U8Entry(0x0004, 1), tlv.U8Entry(0x0005, 1),
		tlv.UTF8Entry(0x0006, "r"), tlv.UTF8Entry(0x0007, "w"), tlv.U8Entry(0x0009, 1), tlv.I64Entry(0x0010, 0),
		tlv.I64Entry(0x0011, 1), tlv.U8Entry(0x0012, 1), tlv.BoolEntry(0x0013, true), tlv.U8Entry(0x0018, 1),
		tlv.U8Entry(0x0019, 0), tlv.UUIDEntry(0x001B, jsonlUUID(1)), tlv.U64Entry(0x0027, 0),
	}
	with := func(extra ...tlv.Entry) []tlv.Entry { return append(slices.Clone(required), extra...) }
	thinning := []tlv.Entry{tlv.U64Entry(1, 1), tlv.U64Entry(2, 3), tlv.UTF8Entry(3, "s")}
	entry := []tlv.Entry{tlv.U64Entry(1, 0), tlv.BytesEntry(3, nil), tlv.UTF8Entry(4, "d"), tlv.BytesEntry(5, nil)}

	tests := []struct {
		name    string
		entries []tlv.Entry
		// valid1 and valid2 are the walk's verdicts under tracepack-jsonl/1 and tracepack-jsonl/2.
		valid1, valid2 bool
	}{
		{"required tags", with(), true, true},
		{"a required tag missing", required[1:], false, false},
		{"a non-repeatable tag repeated", with(tlv.UTF8Entry(0x0017, "a"), tlv.UTF8Entry(0x0017, "a")), false, false},
		{"a repeatable tag repeated", with(tlv.UTF8Entry(0x0008, "a"), tlv.UTF8Entry(0x0008, "a")), true, true},
		{"a retired tag repeated", with(tlv.UTF8Entry(0x0001, "a"), tlv.U64Entry(0x0001, 1<<63)), true, true},
		{"redaction_policy complete", with(tlv.NestedEntry(0x0031, policy)), true, true},
		{"redaction_policy missing a nested required tag", with(tlv.NestedEntry(0x0031, policy[1:])), false, false},
		{"redaction_policy with a nested bad value", with(tlv.NestedEntry(0x0031, append(slices.Clone(policy), tlv.U64Entry(2, 1<<63)))), false, false},
		{"coverage holding a malformed list", with(tlv.Entry{Tag: 0x0016, Type: tlv.TypeTLV, Value: []byte{1, 2}}), false, false},
		{"coverage with a nested known tag of the wrong type", with(tlv.NestedEntry(0x0016, []tlv.Entry{tlv.U8Entry(2, 1)})), false, false},
		{"hsms_timers with a repeated timer", with(tlv.NestedEntry(0x0026, []tlv.Entry{tlv.U64Entry(1, 1), tlv.U64Entry(1, 1)})), false, false},
		{"hsms_timers with unknown nested tags", with(tlv.NestedEntry(0x0026, []tlv.Entry{tlv.U64Entry(9, 1<<63), tlv.U64Entry(9, 1)})), true, true},
		{"a known tlv tag of another type", with(tlv.BytesEntry(0x0026, nil)), false, false},
		{"thinning as bytes, repeated", with(tlv.BytesEntry(0x0033, nil), tlv.BytesEntry(0x0033, nil)), true, false},
		{"redaction_policy with encryption tags as bytes", with(tlv.NestedEntry(0x0031,
			append(slices.Clone(policy), tlv.BytesEntry(5, nil), tlv.BytesEntry(6, nil), tlv.BytesEntry(6, nil)))), true, false},
		{"thinning, repeated", with(tlv.NestedEntry(0x0033, thinning), tlv.NestedEntry(0x0033, thinning)), true, true},
		{"thinning missing a nested required tag", with(tlv.NestedEntry(0x0033, thinning[1:])), true, false},
		{"thinning with a nested bad value", with(tlv.NestedEntry(0x0033, append(slices.Clone(thinning), tlv.U64Entry(1, 1<<63)))), true, false},
		{"redaction_policy with both encryption tags", with(tlv.NestedEntry(0x0031,
			append(slices.Clone(policy), tlv.U8Entry(5, 1), tlv.UTF8Entry(6, "e")))), true, true},
		{"redaction with a siv of any length", with(tlv.NestedEntry(0x0032, append(slices.Clone(entry), tlv.BytesEntry(6, []byte{1})))), true, true},
		{"redaction with a siv repeated", with(tlv.NestedEntry(0x0032,
			append(slices.Clone(entry), tlv.BytesEntry(6, nil), tlv.BytesEntry(6, nil)))), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := jsonlTLV(tt.entries...)
			entries, err := tlv.Decode(b)
			require.NoError(t, err)
			for minor, want := range []bool{tt.valid1, tt.valid2} {
				_, ok := jsonlSchemaOf(uint16(minor)).meta.valid(b, 0)
				assert.Equal(t, want, ok, "minor %d", minor)
				assert.Equal(t, tlv.ValidateMinor(entries, tlv.PackMetadata, uint16(minor)) == nil, ok,
					"the walk agrees with tlv.ValidateMinor at minor %d", minor)
			}
		})
	}
}

// Validating and rendering an entry list allocates nothing, whatever the length of its values and the number of its entries.
func TestJSONLRender_NoAllocations(t *testing.T) {
	// Not parallel: testing.AllocsPerRun counts the allocations of every goroutine.
	note := tlv.U8Entry(0x0001, 1)
	manyUnknown := jsonlTLV(note, tlv.UTF8Entry(0x0005, "t"))
	for range 10000 {
		manyUnknown = tlv.AppendEntry(manyUnknown, tlv.BytesEntry(0x0050, nil))
	}
	meta := mustMarshalMeta(t, readerTestMeta())
	for range 10000 {
		meta = tlv.AppendEntry(meta, tlv.UTF8Entry(0x0008, "c/1"))
	}
	r := jsonlTestReader(t, format.FileHeader{FormatMajor: 1}, meta)

	// A malformed final entry: a u8 of length 9.
	badEntry := []byte{1, 0, 1, 0, 9, 0, 0, 0}
	// An invalid list costs the one error value of its first offending entry, however long the list.
	event := jsonlTLV(tlv.U8Entry(0x0001, 1), tlv.UTF8Entry(0x0012, strings.Repeat("d\x00", 1<<19)))
	for range 10000 {
		event = tlv.AppendEntry(event, tlv.U64Entry(0x0050, 1))
	}
	tests := []struct {
		name      string
		kind      Kind
		payload   []byte
		maxAllocs float64
	}{
		{"transport-event, large detail and many unknown entries", KindTransportEvent, event, 0},
		{"large raw", KindAnnotation, jsonlTLV(note, tlv.BytesEntry(0x0006, make([]byte, 4<<20))), 0},
		{"large unknown bytes", KindAnnotation, jsonlTLV(note, tlv.UTF8Entry(0x0005, "t"), tlv.BytesEntry(0x0050, make([]byte, 4<<20))), 0},
		{"large text", KindAnnotation, jsonlTLV(note, tlv.UTF8Entry(0x0005, strings.Repeat("\x00", 1<<20))), 0},
		{"many empty unknown entries", KindAnnotation, manyUnknown, 0},
		{"a malformed final entry", KindAnnotation, append(jsonlTLV(note, tlv.UTF8Entry(0x0005, "t")), badEntry...), 1},
		{"a malformed final entry after many entries", KindAnnotation, append(bytes.Clone(manyUnknown), badEntry...), 1},
		{"a large known value failing its content rule", KindAnnotation, jsonlTLV(note, tlv.UTF8Entry(0x0005, strings.Repeat("a", 1<<20)+"\xff")), 1},
	}
	jw := newJSONLWriter(discardWriter{})
	for _, tt := range tests {
		allocs := testing.AllocsPerRun(5, func() {
			if jsonlValidBody(tt.kind, tt.payload) {
				jsonlBodyRegistry(tt.kind).render(jw, tt.payload)
			}
			_ = jw.endLine()
		})
		assert.LessOrEqual(t, allocs, tt.maxAllocs, tt.name)
		if tt.kind == KindTransportEvent {
			assert.True(t, jsonlValidBody(tt.kind, tt.payload), tt.name)
		}
	}

	allocs := testing.AllocsPerRun(5, func() {
		_ = writeJSONLHeader(jw, r)
	})
	assert.Zero(t, allocs, "a header line with many repeated classifier values")

	r = jsonlTestReader(t, format.FileHeader{FormatMajor: 1, FormatMinor: 1}, jsonlV2Meta())
	allocs = testing.AllocsPerRun(5, func() {
		_ = writeJSONLHeader(jw, r)
	})
	assert.Zero(t, allocs, "a header line of schema tracepack-jsonl/2")
}

// discardWriter accepts every Write, like io.Discard, but is a distinct type so no fast path applies.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// mustMarshalMeta encodes m and fails the test on error.
func mustMarshalMeta(t *testing.T, m *PackMeta) []byte {
	t.Helper()

	b, err := m.MarshalBinary()
	require.NoError(t, err)

	return b
}

// jsonlPinRow is one tag of a registry as the tracepack JSONL specification §3.1 and the format specification §5 and §8 give it.
type jsonlPinRow struct {
	tag        uint16
	name       string
	vt         tlv.ValueType
	repeatable bool
	required   bool
	enum       []string
	nested     *jsonlRegistry
}

// jsonlPinRegistry is a registry of the export and the rows it must hold.
type jsonlPinRegistry struct {
	name string
	reg  *jsonlRegistry
	rows []jsonlPinRow
}

// pinEncryptionAlgorithm holds the names of the encryption_algorithm enum, which only schema tracepack-jsonl/2 names,
// written from the tracepack format specification §9 and the JSONL specification §3.1.
var pinEncryptionAlgorithm = []string{"unknown", "aes-siv-cmac-512"}

// Enum names of schema tracepack-jsonl/1, written from the tracepack format specification §9 and the JSONL specification §3.1.
var (
	pinTransport         = []string{"unknown", "hsms-ss", "secs1-normalised"}
	pinCaptureMethod     = []string{"unknown", "raw-stream", "decoded-message", "log", "generator"}
	pinVantage           = []string{"unknown", "host", "equipment", "intermediary", "network", "none"}
	pinTimeSource        = []string{"unknown", "capture-clock", "source-log", "generator"}
	pinLifecycleCoverage = []string{"unknown", "subscribed", "none"}
	pinPackRole          = []string{"unknown", "segment", "archive", "extract", "repair"}
	pinDigestAlgorithm   = []string{"unknown", "hmac-sha256"}
	pinEvent             = []string{"unknown", "state-transition", "timer-expiry", "socket-accept", "socket-connect", "socket-close", "capture-boundary", "clock-step"}
	pinState             = []string{"unknown", "not-connected", "not-selected", "selected"}
	pinTimer             = []string{"none", "T1", "T2", "T3", "T4", "T5", "T6", "T7", "T8"}
	pinCause             = []string{
		"unknown", "local-open", "local-close", "select-accepted", "select-rejected", "local-deselect", "peer-deselect",
		"local-separate", "peer-separate", "timer-expiry", "linktest-failure", "transport-error", "peer-close", "implementation-fault",
	}
	pinSocketRole     = []string{"unknown", "passive", "active"}
	pinBoundaryKind   = []string{"unknown", "start", "stop", "gap", "stop-unclean"}
	pinAnnotationKind = []string{"unknown", "note", "unparsed-entry", "skipped-bytes", "unrecognised-line"}
)

// Every registry the export uses, every enum it names and every bit name are those of its schema:
// a registry change fails this test until the schema value and the JSONL specification change with it.
// Schema tracepack-jsonl/1 renders the format 1.0 view of the registries, so the tags of format 1.1 are not among them;
// schema tracepack-jsonl/2 renders the format 1.1 view, which adds them.
func TestJSONLRegistriesPinned(t *testing.T) {
	t.Parallel()

	const (
		u8, boolean, i64, u64 = tlv.TypeU8, tlv.TypeBool, tlv.TypeI64, tlv.TypeU64
		uuid, utf8, raw, list = tlv.TypeUUID, tlv.TypeUTF8, tlv.TypeBytes, tlv.TypeTLV
	)
	registries := []jsonlPinRegistry{
		{"pack metadata", jsonlPackMetadataRegistry, []jsonlPinRow{
			{0x0002, "tool_id", utf8, false, true, nil, nil},
			{0x0003, "transport", u8, false, true, pinTransport, nil},
			{0x0004, "capture_method", u8, false, true, pinCaptureMethod, nil},
			{0x0005, "vantage", u8, false, true, pinVantage, nil},
			{0x0006, "recorder", utf8, false, true, nil, nil},
			{0x0007, "writer", utf8, false, true, nil, nil},
			{0x0008, "classifier", utf8, true, false, nil, nil},
			{0x0009, "time_source", u8, false, true, pinTimeSource, nil},
			{0x000A, "capture_origin_utc_ns", i64, false, false, nil, nil},
			{0x000B, "capture_origin_mono_ns", i64, false, false, nil, nil},
			{0x000C, "source_tz", utf8, false, false, nil, nil},
			{0x000D, "source_dialect", utf8, false, false, nil, nil},
			{0x000E, "source_ref", utf8, true, false, nil, nil},
			{0x000F, "max_frame_len", u64, true, false, nil, nil},
			{0x0010, "period_start", i64, false, true, nil, nil},
			{0x0011, "period_end", i64, false, true, nil, nil},
			{0x0012, "lifecycle_coverage", u8, false, true, pinLifecycleCoverage, nil},
			{0x0013, "quality_evaluated", boolean, false, true, nil, nil},
			{0x0015, "supersedes", uuid, true, false, nil, nil},
			{0x0016, "coverage", list, true, false, nil, jsonlCoverageRegistry},
			{0x0017, "notes", utf8, false, false, nil, nil},
			{0x0018, "pack_role", u8, false, true, pinPackRole, nil},
			{0x0019, "compaction_level", u8, false, true, nil, nil},
			{0x001A, "compacted_from", uuid, true, false, nil, nil},
			{0x001B, "recorder_instance_id", uuid, false, true, nil, nil},
			{0x001C, "previous_capture_id", uuid, false, false, nil, nil},
			{0x001D, "extract_filter", utf8, false, false, nil, nil},
			{0x001E, "site_id", utf8, false, false, nil, nil},
			{0x001F, "equipment_model", utf8, false, false, nil, nil},
			{0x0020, "equipment_sw_rev", utf8, false, false, nil, nil},
			{0x0021, "host_software", utf8, false, false, nil, nil},
			{0x0022, "host_endpoint", utf8, false, false, nil, nil},
			{0x0023, "equipment_endpoint", utf8, false, false, nil, nil},
			{0x0024, "equipment_connect_mode", u8, false, false, pinSocketRole, nil},
			{0x0025, "device_id", u64, false, false, nil, nil},
			{0x0026, "hsms_timers", list, false, false, nil, jsonlHSMSTimersRegistry},
			{0x0027, "seq_start", u64, false, true, nil, nil},
			{0x0028, "clock_step_tolerance_ns", u64, false, false, nil, nil},
			{0x0029, "replacement_set_id", uuid, false, false, nil, nil},
			{0x002A, "replacement_set_size", u64, false, false, nil, nil},
			{0x002B, "replacement_set_index", u64, false, false, nil, nil},
			{0x002C, "flush_interval_ns", u64, false, false, nil, nil},
			{0x002D, "scope_generation", u64, false, false, nil, nil},
			{0x002E, "publisher_epoch", u64, false, false, nil, nil},
			{0x002F, "patch_base", uuid, false, false, nil, nil},
			{0x0031, "redaction_policy", list, false, false, nil, jsonlRedactionPolicyRegistry},
			{0x0032, "redaction", list, true, false, nil, jsonlRedactionRegistry},
		}},
		{"coverage", jsonlCoverageRegistry, []jsonlPinRow{
			{0x0001, "capture_id", uuid, false, false, nil, nil},
			{0x0002, "seq_first", u64, false, false, nil, nil},
			{0x0003, "seq_last", u64, false, false, nil, nil},
			{0x0004, "time_start", i64, false, false, nil, nil},
			{0x0005, "time_end", i64, false, false, nil, nil},
		}},
		{"hsms_timers", jsonlHSMSTimersRegistry, []jsonlPinRow{
			{1, "T1", u64, false, false, nil, nil},
			{2, "T2", u64, false, false, nil, nil},
			{3, "T3", u64, false, false, nil, nil},
			{4, "T4", u64, false, false, nil, nil},
			{5, "T5", u64, false, false, nil, nil},
			{6, "T6", u64, false, false, nil, nil},
			{7, "T7", u64, false, false, nil, nil},
			{8, "T8", u64, false, false, nil, nil},
		}},
		{"redaction_policy", jsonlRedactionPolicyRegistry, []jsonlPinRow{
			{0x0001, "policy_id", utf8, false, true, nil, nil},
			{0x0002, "policy_version", u64, false, true, nil, nil},
			{0x0003, "key_id", utf8, false, true, nil, nil},
			{0x0004, "digest_algorithm", u8, false, true, pinDigestAlgorithm, nil},
		}},
		{"redaction", jsonlRedactionRegistry, []jsonlPinRow{
			{0x0001, "seq", u64, false, true, nil, nil},
			{0x0002, "item_path", utf8, false, false, nil, nil},
			{0x0003, "masked_ranges", raw, false, true, nil, nil},
			{0x0004, "domain", utf8, false, true, nil, nil},
			{0x0005, "digest", raw, false, true, nil, nil},
		}},
		{"transport-event", jsonlTransportEventRegistry, []jsonlPinRow{
			{0x0001, "event", u8, false, true, pinEvent, nil},
			{0x0002, "prev_state", u8, false, false, pinState, nil},
			{0x0003, "cur_state", u8, false, false, pinState, nil},
			{0x0004, "cause", u8, false, false, pinCause, nil},
			{0x0005, "cause_raw", utf8, false, false, nil, nil},
			{0x0006, "timer", u8, false, false, pinTimer, nil},
			{0x0007, "socket_role", u8, false, false, pinSocketRole, nil},
			{0x0008, "inferred", boolean, false, false, nil, nil},
			{0x0009, "primary_session_id", u64, false, false, nil, nil},
			{0x000A, "primary_stream", u64, false, false, nil, nil},
			{0x000B, "primary_function", u64, false, false, nil, nil},
			{0x000C, "primary_system_bytes", raw, false, false, nil, nil},
			{0x000D, "boundary_kind", u8, false, false, pinBoundaryKind, nil},
			{0x000E, "boundary_seq_first", u64, false, false, nil, nil},
			{0x000F, "boundary_seq_last", u64, false, false, nil, nil},
			{0x0010, "gap_start", i64, false, false, nil, nil},
			{0x0011, "gap_end", i64, false, false, nil, nil},
			{0x0012, "detail", utf8, false, false, nil, nil},
			{0x0013, "clock_step_ns", i64, false, false, nil, nil},
		}},
		{"annotation", jsonlAnnotationRegistry, []jsonlPinRow{
			{0x0001, "annotation_kind", u8, false, true, pinAnnotationKind, nil},
			{0x0002, "ref_capture_id", uuid, false, false, nil, nil},
			{0x0003, "ref_seq_first", u64, false, false, nil, nil},
			{0x0004, "ref_seq_last", u64, false, false, nil, nil},
			{0x0005, "text", utf8, false, false, nil, nil},
			{0x0006, "raw", raw, false, false, nil, nil},
			{0x0007, "source_index", u64, false, false, nil, nil},
			{0x0008, "source_offset", u64, false, false, nil, nil},
			{0x0009, "source_len", u64, false, false, nil, nil},
		}},
	}
	rowsOf := func(name string) []jsonlPinRow {
		i := slices.IndexFunc(registries, func(r jsonlPinRegistry) bool { return r.name == name })
		require.GreaterOrEqual(t, i, 0, name)

		return slices.Clone(registries[i].rows)
	}

	// Schema tracepack-jsonl/2 knows every tag of tracepack-jsonl/1, the bodies' registries unchanged,
	// and the tags format 1.1 adds.
	meta2 := rowsOf("pack metadata")
	for i := range meta2 {
		switch meta2[i].tag {
		case 0x0031:
			meta2[i].nested = jsonlRedactionPolicyRegistryV2
		case 0x0032:
			meta2[i].nested = jsonlRedactionRegistryV2
		default:
		}
	}
	meta2 = append(meta2, jsonlPinRow{0x0033, "thinning", list, true, false, nil, jsonlThinningRegistry})
	policy2 := append(rowsOf("redaction_policy"),
		jsonlPinRow{0x0005, "encryption_algorithm", u8, false, false, pinEncryptionAlgorithm, nil},
		jsonlPinRow{0x0006, "encryption_key_id", utf8, false, false, nil, nil},
	)
	redaction2 := append(rowsOf("redaction"), jsonlPinRow{0x0006, "siv", raw, false, false, nil, nil})
	thinning := []jsonlPinRow{
		{0x0001, "stream", u64, false, true, nil, nil},
		{0x0002, "function", u64, false, true, nil, nil},
		{0x0003, "source", utf8, false, true, nil, nil},
	}
	v2 := []jsonlPinRegistry{
		{"pack metadata /2", jsonlPackMetadataRegistryV2, meta2},
		{"redaction_policy /2", jsonlRedactionPolicyRegistryV2, policy2},
		{"redaction /2", jsonlRedactionRegistryV2, redaction2},
		{"thinning /2", jsonlThinningRegistry, thinning},
	}
	for _, rr := range slices.Concat(registries, v2) {
		t.Run(rr.name, func(t *testing.T) {
			t.Parallel()
			requireJSONLRegistry(t, rr.reg, rr.rows)
		})
	}
}

// requireJSONLRegistry checks that reg holds exactly rows: the same tags, and for each the same name, value type,
// flags, enum names and nested registry.
func requireJSONLRegistry(t *testing.T, reg *jsonlRegistry, rows []jsonlPinRow) {
	t.Helper()

	tags := make([]uint16, 0, len(rows))
	enums, nested := 0, 0
	for _, row := range rows {
		tags = append(tags, row.tag)
		require.Less(t, int(row.tag), jsonlTagLimit, "tag 0x%04X fits a tag set", row.tag)

		f, known := reg.reg[row.tag]
		require.True(t, known, "tag 0x%04X", row.tag)
		assert.Equal(t, row.name, f.Name, "tag 0x%04X", row.tag)
		assert.Equal(t, row.vt, f.Type, "tag 0x%04X", row.tag)
		assert.Equal(t, row.repeatable, f.Repeatable, "tag 0x%04X repeatable", row.tag)
		assert.Equal(t, row.required, f.Required, "tag 0x%04X required", row.tag)
		assert.Equal(t, row.enum, reg.enums[row.tag], "tag 0x%04X enum", row.tag)
		assert.Same(t, row.nested, reg.nested[row.tag], "tag 0x%04X nested registry", row.tag)
		assert.Equal(t, row.nested == nil, f.Nested == nil, "tag 0x%04X nests a registry exactly when the live registry does", row.tag)
		if row.enum != nil {
			enums++
		}
		if row.nested != nil {
			nested++
			require.NotNil(t, f.Nested)
			assert.Equal(t, reflect.ValueOf(*f.Nested).Pointer(), reflect.ValueOf(row.nested.reg).Pointer(),
				"tag 0x%04X: the walk descends with the registry tlv.Validate uses", row.tag)
		}
	}
	assert.Equal(t, tags, slices.Sorted(maps.Keys(reg.reg)), "no other tag is known")
	assert.Len(t, reg.enums, enums, "no other tag holds an enum")
	assert.Len(t, reg.nested, nested, "no other tag nests a registry")
}

// The enum and bit names of schemas tracepack-jsonl/1 and tracepack-jsonl/2
// are those the tracepack format specification §9 and the JSONL specification §3.1 and §3.2 give.
func TestJSONLNameTablesPinned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"kind", jsonlKindNames, []string{"unknown", "data", "control", "transport-event", "annotation"}},
		{"dir", jsonlDirNames, []string{"unknown", "host-to-equipment", "equipment-to-host", "local"}},
		{"fidelity", jsonlFidelityNames, []string{"unknown", "wire-exact", "re-encoded", "reconstructed", "synthesized", "not-applicable"}},
		{"decode_status", jsonlDecodeStatusNames, []string{
			"not-attempted", "ok", "ok-with-trailing", "short-frame", "length-mismatch", "bad-ptype", "bad-stype",
			"control-with-body", "oversized", "item-decode-error", "reconstructed-ok", "parse-failed", "build-rejected", "not-applicable",
		}},
		{"transport", jsonlTransportNames, pinTransport},
		{"capture_method", jsonlCaptureMethodNames, pinCaptureMethod},
		{"vantage", jsonlVantageNames, pinVantage},
		{"time_source", jsonlTimeSourceNames, pinTimeSource},
		{"lifecycle_coverage", jsonlLifecycleCoverageNames, pinLifecycleCoverage},
		{"pack_role", jsonlPackRoleNames, pinPackRole},
		{"digest_algorithm", jsonlDigestAlgorithmNames, pinDigestAlgorithm},
		{"event", jsonlEventNames, pinEvent},
		{"state", jsonlStateNames, pinState},
		{"timer", jsonlTimerNames, pinTimer},
		{"cause", jsonlCauseNames, pinCause},
		{"socket_role", jsonlSocketRoleNames, pinSocketRole},
		{"boundary_kind", jsonlBoundaryKindNames, pinBoundaryKind},
		{"annotation_kind", jsonlAnnotationKindNames, pinAnnotationKind},
		{"header flags", jsonlHeaderFlagNames, []string{"redaction-present"}},
		{"quality", jsonlQualityNames, []string{"capture-boundary", "ordering-uncertain", "correlation-incomplete", "", "direction-inferred", "redacted"}},
		{"field_validity", jsonlFieldValidityNames, []string{"session_id", "stream_and_w", "function", "ptype", "stype", "system_bytes"}},
		{"record_flags", jsonlRecordFlagNames, []string{"", "mono_present"}},
		{"encryption_algorithm /2", jsonlEncryptionAlgorithmNames, pinEncryptionAlgorithm},
		{"quality /2", jsonlQualityNamesV2, []string{
			"capture-boundary", "ordering-uncertain", "correlation-incomplete", "", "direction-inferred", "redacted", "", "unobserved",
		}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.got, tt.name)
	}
}
