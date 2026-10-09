package tracepack

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var errJSONLWriteBoom = errors.New("boom")

// recordingWriter keeps every byte it is given and the size of each Write call.
type recordingWriter struct {
	buf   bytes.Buffer
	sizes []int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))

	return w.buf.Write(p)
}

// failingWriter accepts its first ok calls whole,
// then accepts n bytes of the next call and returns err (nil for a short write), and counts every call.
type failingWriter struct {
	ok    int
	n     int
	err   error
	calls int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls <= w.ok {
		return len(p), nil
	}

	return min(w.n, len(p)), w.err
}

// renderJSONL returns the line fn writes, its final LF included.
func renderJSONL(t *testing.T, fn func(jw *jsonlWriter)) string {
	t.Helper()

	var b bytes.Buffer
	jw := newJSONLWriter(&b)
	fn(jw)
	require.NoError(t, jw.endLine())

	return b.String()
}

// requireChunked checks that every Write of w was at most jsonlBufferSize bytes and that w received want.
func requireChunked(t *testing.T, w *recordingWriter, want string) {
	t.Helper()

	for i, n := range w.sizes {
		require.LessOrEqual(t, n, jsonlBufferSize, "write %d", i)
		require.Positive(t, n, "write %d", i)
	}
	require.Equal(t, want, w.buf.String())
}

func TestJSONLWriter_StringEscaping(t *testing.T) {
	t.Parallel()

	// Expected forms written by hand from the tracepack JSONL specification §3.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", `""`},
		{"0x00", "\x00", `"\u0000"`},
		{"0x01", "\x01", `"\u0001"`},
		{"0x02", "\x02", `"\u0002"`},
		{"0x03", "\x03", `"\u0003"`},
		{"0x04", "\x04", `"\u0004"`},
		{"0x05", "\x05", `"\u0005"`},
		{"0x06", "\x06", `"\u0006"`},
		{"0x07", "\x07", `"\u0007"`},
		{"0x08", "\x08", `"\b"`},
		{"0x09", "\x09", `"\t"`},
		{"0x0a", "\x0a", `"\n"`},
		{"0x0b", "\x0b", `"\u000b"`},
		{"0x0c", "\x0c", `"\f"`},
		{"0x0d", "\x0d", `"\r"`},
		{"0x0e", "\x0e", `"\u000e"`},
		{"0x0f", "\x0f", `"\u000f"`},
		{"0x10", "\x10", `"\u0010"`},
		{"0x11", "\x11", `"\u0011"`},
		{"0x12", "\x12", `"\u0012"`},
		{"0x13", "\x13", `"\u0013"`},
		{"0x14", "\x14", `"\u0014"`},
		{"0x15", "\x15", `"\u0015"`},
		{"0x16", "\x16", `"\u0016"`},
		{"0x17", "\x17", `"\u0017"`},
		{"0x18", "\x18", `"\u0018"`},
		{"0x19", "\x19", `"\u0019"`},
		{"0x1a", "\x1a", `"\u001a"`},
		{"0x1b", "\x1b", `"\u001b"`},
		{"0x1c", "\x1c", `"\u001c"`},
		{"0x1d", "\x1d", `"\u001d"`},
		{"0x1e", "\x1e", `"\u001e"`},
		{"0x1f", "\x1f", `"\u001f"`},
		{"space", " ", `" "`},
		{"quote", `"`, `"\""`},
		{"backslash", `\`, `"\\"`},
		{"slash", "/", `"/"`},
		{"html", "<>&", `"<>&"`},
		{"U+007F", "\x7f", "\"\x7f\""},
		{"U+2028", "\u2028", "\"\xe2\x80\xa8\""},
		{"U+2029", "\u2029", "\"\xe2\x80\xa9\""},
		{"leading U+FEFF", "\ufeffa", "\"\xef\xbb\xbfa\""},
		{"non-ASCII", "é中😀", "\"\xc3\xa9\xe4\xb8\xad\xf0\x9f\x98\x80\""},
		{"mixed", "a\"b\\c\nd\x00e", `"a\"b\\c\nd\u0000e"`},
		{"escape at both ends", "\tx\r", `"\tx\r"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fromString := renderJSONL(t, func(jw *jsonlWriter) { writeString(jw, tt.in) })
			fromBytes := renderJSONL(t, func(jw *jsonlWriter) { writeString(jw, []byte(tt.in)) })
			require.Equal(t, tt.want+"\n", fromString)
			require.Equal(t, tt.want+"\n", fromBytes)
		})
	}
}

func TestJSONLWriter_Structure(t *testing.T) {
	t.Parallel()

	got := renderJSONL(t, func(jw *jsonlWriter) {
		jw.beginObject()
		jw.key("a")
		jw.beginArray()
		jw.endArray()
		jw.key("b")
		jw.beginObject()
		jw.key("c")
		jw.num(1)
		jw.key("d")
		jw.beginArray()
		jw.num(1)
		jw.num(2)
		writeString(jw, "x")
		jw.endArray()
		jw.endObject()
		jw.key("e")
		jw.beginObject()
		jw.endObject()
		jw.key("f")
		jw.beginArray()
		jw.beginObject()
		jw.key("x")
		jw.boolean(true)
		jw.endObject()
		jw.beginObject()
		jw.key("y")
		jw.boolean(false)
		jw.endObject()
		jw.endArray()
		jw.key("g")
		jw.base64(nil)
		jw.key("h")
		jw.beginObject()
		jw.key("enum")
		jw.enum(jsonlKindNames, 3)
		jw.key("bits")
		jw.bitSet(jsonlRecordFlagNames, 3)
		jw.key("uuid")
		jw.uuid([16]byte{0xAB, 15: 1})
		jw.key("u64")
		jw.u64(7)
		jw.key("i64")
		jw.i64(-7)
		jw.endObject()
		jw.key("i")
		jw.beginArray()
		writeString(jw, "é")
		writeString(jw, "é")
		jw.endArray()
		jw.endObject()
	})
	// Written by hand from the tracepack JSONL specification §2 and §3;
	// the two strings are canonically equivalent but keep their own bytes, C3 A9 and 65 CC 81.
	want := `{"a":[],"b":{"c":1,"d":[1,2,"x"]},"e":{},"f":[{"x":true},{"y":false}],"g":"",` +
		`"h":{"enum":"transport-event","bits":["bit(0)","mono_present"],"uuid":"ab000000-0000-0000-0000-000000000001","u64":"7","i64":"-7"},` +
		"\"i\":[\"\xc3\xa9\",\"e\xcc\x81\"]}\n"
	require.Equal(t, want, got)
}

func TestJSONLWriter_LinesResetSeparators(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	jw := newJSONLWriter(&b)
	jw.beginObject()
	jw.key("a")
	jw.num(1)
	jw.endObject()
	require.NoError(t, jw.endLine())
	jw.beginObject()
	jw.key("b")
	jw.num(2)
	jw.endObject()
	require.NoError(t, jw.endLine())
	require.Equal(t, "{\"a\":1}\n{\"b\":2}\n", b.String())
}

func TestJSONLWriter_Numbers(t *testing.T) {
	t.Parallel()

	// Expected forms written by hand from the tracepack JSONL specification §3.
	tests := []struct {
		name  string
		write func(jw *jsonlWriter)
		want  string
	}{
		{"num 0", func(jw *jsonlWriter) { jw.num(0) }, `0`},
		{"num 10", func(jw *jsonlWriter) { jw.num(10) }, `10`},
		{"num u32 max", func(jw *jsonlWriter) { jw.num(4294967295) }, `4294967295`},
		{"u64 0", func(jw *jsonlWriter) { jw.u64(0) }, `"0"`},
		{"u64 2^53-1", func(jw *jsonlWriter) { jw.u64(1<<53 - 1) }, `"9007199254740991"`},
		{"u64 2^53", func(jw *jsonlWriter) { jw.u64(1 << 53) }, `"9007199254740992"`},
		{"u64 2^53+1", func(jw *jsonlWriter) { jw.u64(1<<53 + 1) }, `"9007199254740993"`},
		{"u64 2^63-1", func(jw *jsonlWriter) { jw.u64(1<<63 - 1) }, `"9223372036854775807"`},
		{"u64 max", func(jw *jsonlWriter) { jw.u64(1<<64 - 1) }, `"18446744073709551615"`},
		{"i64 0", func(jw *jsonlWriter) { jw.i64(0) }, `"0"`},
		{"i64 -1", func(jw *jsonlWriter) { jw.i64(-1) }, `"-1"`},
		{"i64 min", func(jw *jsonlWriter) { jw.i64(-1 << 63) }, `"-9223372036854775808"`},
		{"i64 max", func(jw *jsonlWriter) { jw.i64(1<<63 - 1) }, `"9223372036854775807"`},
		{"i64 2^53-1", func(jw *jsonlWriter) { jw.i64(1<<53 - 1) }, `"9007199254740991"`},
		{"i64 2^53", func(jw *jsonlWriter) { jw.i64(1 << 53) }, `"9007199254740992"`},
		{"i64 2^53+1", func(jw *jsonlWriter) { jw.i64(1<<53 + 1) }, `"9007199254740993"`},
		{"i64 -(2^53+1)", func(jw *jsonlWriter) { jw.i64(-(1<<53 + 1)) }, `"-9007199254740993"`},
		{"true", func(jw *jsonlWriter) { jw.boolean(true) }, `true`},
		{"false", func(jw *jsonlWriter) { jw.boolean(false) }, `false`},
		{
			"uuid",
			func(jw *jsonlWriter) {
				jw.uuid([16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10})
			},
			`"01234567-89ab-cdef-fedc-ba9876543210"`,
		},
		{"nil uuid", func(jw *jsonlWriter) { jw.uuid([16]byte{}) }, `"00000000-0000-0000-0000-000000000000"`},
		{
			"uuid ff",
			func(jw *jsonlWriter) {
				jw.uuid([16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
			},
			`"ffffffff-ffff-ffff-ffff-ffffffffffff"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want+"\n", renderJSONL(t, tt.write))
		})
	}
}

func TestJSONLWriter_Base64(t *testing.T) {
	t.Parallel()

	// RFC 4648 §10 test vectors, extended to 7 bytes; "!" is 0x21.
	tests := []struct {
		in   string
		want string
	}{
		{"", `""`},
		{"f", `"Zg=="`},
		{"fo", `"Zm8="`},
		{"foo", `"Zm9v"`},
		{"foob", `"Zm9vYg=="`},
		{"fooba", `"Zm9vYmE="`},
		{"foobar", `"Zm9vYmFy"`},
		{"foobar!", `"Zm9vYmFyIQ=="`},
	}
	for _, tt := range tests {
		got := renderJSONL(t, func(jw *jsonlWriter) { jw.base64([]byte(tt.in)) })
		require.Equal(t, tt.want+"\n", got, "len %d", len(tt.in))
		require.Equal(t, `"`+base64.StdEncoding.EncodeToString([]byte(tt.in))+`"`+"\n", got, "len %d", len(tt.in))
	}
}

func TestJSONLWriter_Base64Chunked(t *testing.T) {
	t.Parallel()

	// 49152 input bytes encode to exactly 65536 characters;
	// the prefixes move the group boundaries against the buffer boundary.
	for _, prefix := range []int{0, 1, 2, 3, 5} {
		for _, n := range []int{49149, 49150, 49151, 49152, 49153, 49154, 49155, 3*jsonlBufferSize + 1, 3*jsonlBufferSize + 2} {
			p := make([]byte, n)
			for i := range p {
				p[i] = byte(i*7 + i>>8)
			}
			var w recordingWriter
			jw := newJSONLWriter(&w)
			writeString(jw, strings.Repeat("a", prefix))
			jw.base64(p)
			require.NoError(t, jw.endLine())

			want := `"` + strings.Repeat("a", prefix) + `","` + base64.StdEncoding.EncodeToString(p) + "\"\n"
			requireChunked(t, &w, want)

			// Every Write ends on a whole base64 group.
			start := prefix + 4
			end := 0
			for _, size := range w.sizes[:len(w.sizes)-1] {
				end += size
				if end > start {
					require.Zero(t, (end-start)%4, "prefix %d, len %d: a write ends inside a base64 group", prefix, n)
				}
			}
		}
	}
}

func TestJSONLWriter_EscapedStringChunked(t *testing.T) {
	t.Parallel()

	const n = 3*jsonlBufferSize + 7

	// A prefix of 0 to 5 bytes moves the escapes against the buffer boundary,
	// so the space left before the first flush takes every value below the escape's length.
	for _, esc := range []struct{ in, out string }{{"\x00", `\u0000`}, {"\n", `\n`}} {
		free := map[int]bool{}
		for prefix := range 6 {
			s := strings.Repeat("a", prefix) + strings.Repeat(esc.in, n)
			want := `"` + strings.Repeat("a", prefix) + strings.Repeat(esc.out, n) + "\"\n"

			for _, asBytes := range []bool{false, true} {
				var w recordingWriter
				jw := newJSONLWriter(&w)
				if asBytes {
					writeString(jw, []byte(s))
				} else {
					writeString(jw, s)
				}
				require.NoError(t, jw.endLine())
				requireChunked(t, &w, want)
				require.Greater(t, len(w.sizes), 3)
				free[jsonlBufferSize-w.sizes[0]] = true

				// Every Write ends on a whole escape: the escapes start after the opening quote and the prefix.
				end := 0
				for _, size := range w.sizes[:len(w.sizes)-1] {
					end += size
					require.Zero(t, (end-1-prefix)%len(esc.out), "%q, prefix %d: a write ends inside an escape", esc.out, prefix)
				}
			}
		}
		for k := 1; k < len(esc.out); k++ {
			require.True(t, free[k], "%q: no flush leaves %d bytes free", esc.out, k)
		}
	}
}

func TestJSONLWriter_MultiByteChunked(t *testing.T) {
	t.Parallel()

	// Multi-byte sequences of two, three and four bytes cross the buffer boundary at every offset;
	// they are copied as stored, so the bytes are those of the input whatever the split.
	for _, r := range []string{"é", "中", "😀"} {
		for prefix := range 4 {
			s := strings.Repeat("a", prefix) + strings.Repeat(r, jsonlBufferSize/len(r)+3) + "\n"
			var w recordingWriter
			jw := newJSONLWriter(&w)
			writeString(jw, s)
			require.NoError(t, jw.endLine())
			requireChunked(t, &w, `"`+s[:len(s)-1]+`\n"`+"\n")
			require.Len(t, w.sizes, 2)
		}
	}
}

func TestJSONLWriter_LineAtBufferCapacity(t *testing.T) {
	t.Parallel()

	// A line of exactly jsonlBufferSize bytes: the quotes and the LF take 3.
	var w recordingWriter
	jw := newJSONLWriter(&w)
	writeString(jw, strings.Repeat("a", jsonlBufferSize-3))
	require.NoError(t, jw.endLine())
	require.Equal(t, []int{jsonlBufferSize}, w.sizes)
	require.Equal(t, `"`+strings.Repeat("a", jsonlBufferSize-3)+"\"\n", w.buf.String())

	// One byte more: the LF goes alone.
	w = recordingWriter{}
	jw = newJSONLWriter(&w)
	writeString(jw, strings.Repeat("a", jsonlBufferSize-2))
	require.NoError(t, jw.endLine())
	require.Equal(t, []int{jsonlBufferSize, 1}, w.sizes)

	// A second line starts with an empty buffer.
	writeString(jw, "b")
	require.NoError(t, jw.endLine())
	require.Equal(t, []int{jsonlBufferSize, 1, 4}, w.sizes)
}

func TestJSONLWriter_WriteFailsAfterPartialChunk(t *testing.T) {
	t.Parallel()

	// The second Write accepts part of its chunk and fails.
	w := &failingWriter{ok: 1, n: 1000, err: errJSONLWriteBoom}
	jw := newJSONLWriter(w)
	writeString(jw, strings.Repeat("a", 3*jsonlBufferSize))
	err := jw.endLine()
	require.ErrorIs(t, err, errJSONLWriteBoom)
	require.NotErrorIs(t, err, io.ErrShortWrite)
	require.Equal(t, 2, w.calls)

	// Nothing more is written, and the error stays.
	jw.beginObject()
	jw.key("a")
	jw.base64(make([]byte, 2*jsonlBufferSize))
	writeString(jw, strings.Repeat("\x00", jsonlBufferSize))
	jw.u64(1)
	jw.enum(jsonlKindNames, 9)
	jw.bitSet(jsonlQualityNames, 0xffff)
	jw.endObject()
	require.ErrorIs(t, jw.endLine(), errJSONLWriteBoom)
	require.Equal(t, 2, w.calls)
}

func TestJSONLWriter_ShortWrite(t *testing.T) {
	t.Parallel()

	// The first Write accepts all but one byte of the 8-byte line without an error.
	w := &failingWriter{ok: 0, n: 7, err: nil}
	jw := newJSONLWriter(w)
	jw.beginObject()
	jw.key("a")
	jw.num(1)
	jw.endObject()
	err := jw.endLine()
	require.ErrorIs(t, err, io.ErrShortWrite)
	require.Equal(t, 1, w.calls)

	writeString(jw, "more")
	require.ErrorIs(t, jw.endLine(), io.ErrShortWrite)
	require.Equal(t, 1, w.calls)
}

func TestJSONLWriter_EnumNames(t *testing.T) {
	t.Parallel()

	// Expected forms written by hand from the tracepack JSONL specification §3.1 and the tracepack format specification §9.
	tests := []struct {
		names []string
		v     uint8
		want  string
	}{
		{jsonlKindNames, 0, `"unknown"`},
		{jsonlKindNames, 4, `"annotation"`},
		{jsonlKindNames, 5, `"unknown(5)"`},
		{jsonlKindNames, 255, `"unknown(255)"`},
		{jsonlDecodeStatusNames, 0, `"not-attempted"`},
		{jsonlDecodeStatusNames, 13, `"not-applicable"`},
		{jsonlDecodeStatusNames, 14, `"unknown(14)"`},
		{jsonlTimerNames, 0, `"none"`},
		{jsonlTimerNames, 1, `"T1"`},
		{jsonlTimerNames, 8, `"T8"`},
		{jsonlTimerNames, 9, `"unknown(9)"`},
		{jsonlPackRoleNames, 4, `"repair"`},
		{jsonlPackRoleNames, 5, `"unknown(5)"`},
		{jsonlCauseNames, 13, `"implementation-fault"`},
		{jsonlBoundaryKindNames, 4, `"stop-unclean"`},
		{jsonlDigestAlgorithmNames, 1, `"hmac-sha256"`},
		{jsonlDigestAlgorithmNames, 2, `"unknown(2)"`},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want+"\n", renderJSONL(t, func(jw *jsonlWriter) { jw.enum(tt.names, tt.v) }))
	}
}

func TestJSONLWriter_EnumTablesMatchStringMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		names  []string
		string func(v uint8) string
	}{
		{"kind", jsonlKindNames, func(v uint8) string { return Kind(v).String() }},
		{"dir", jsonlDirNames, func(v uint8) string { return Dir(v).String() }},
		{"fidelity", jsonlFidelityNames, func(v uint8) string { return Fidelity(v).String() }},
		{"decode_status", jsonlDecodeStatusNames, func(v uint8) string { return DecodeStatus(v).String() }},
		{"transport", jsonlTransportNames, func(v uint8) string { return Transport(v).String() }},
		{"capture_method", jsonlCaptureMethodNames, func(v uint8) string { return CaptureMethod(v).String() }},
		{"vantage", jsonlVantageNames, func(v uint8) string { return Vantage(v).String() }},
		{"time_source", jsonlTimeSourceNames, func(v uint8) string { return TimeSource(v).String() }},
		{"lifecycle_coverage", jsonlLifecycleCoverageNames, func(v uint8) string { return LifecycleCoverage(v).String() }},
		{"pack_role", jsonlPackRoleNames, func(v uint8) string { return PackRole(v).String() }},
		{"digest_algorithm", jsonlDigestAlgorithmNames, func(v uint8) string { return DigestAlgorithm(v).String() }},
		{"event", jsonlEventNames, func(v uint8) string { return Event(v).String() }},
		{"state", jsonlStateNames, func(v uint8) string { return State(v).String() }},
		{"timer", jsonlTimerNames, func(v uint8) string { return Timer(v).String() }},
		{"cause", jsonlCauseNames, func(v uint8) string { return Cause(v).String() }},
		{"socket_role", jsonlSocketRoleNames, func(v uint8) string { return SocketRole(v).String() }},
		{"boundary_kind", jsonlBoundaryKindNames, func(v uint8) string { return BoundaryKind(v).String() }},
		{"annotation_kind", jsonlAnnotationKindNames, func(v uint8) string { return AnnotationKind(v).String() }},
	}
	// The tables pin schema tracepack-jsonl/1: a value the format adds later has a String name but stays unknown here,
	// so only the values within a table are compared with String.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requirePlainNames(t, tt.names)
			var b bytes.Buffer
			jw := newJSONLWriter(&b)
			for v := range 256 {
				b.Reset()
				jw.enum(tt.names, uint8(v))
				require.NoError(t, jw.endLine())
				want := "unknown(" + strconv.Itoa(v) + ")"
				if v < len(tt.names) {
					want = tt.string(uint8(v))
				}
				require.Equal(t, `"`+want+"\"\n", b.String(), "value %d", v)
			}
		})
	}
}

func TestJSONLWriter_BitSets(t *testing.T) {
	t.Parallel()

	// Expected forms written by hand from the tracepack JSONL specification §3.2.
	tests := []struct {
		name  string
		names []string
		v     uint32
		want  string
	}{
		{"empty", jsonlQualityNames, 0, `[]`},
		{"header flags named", jsonlHeaderFlagNames, 1, `["redaction-present"]`},
		{"header flags high bits", jsonlHeaderFlagNames, 1 | 1<<1 | 1<<31, `["redaction-present","bit(1)","bit(31)"]`},
		{
			"quality every bit",
			jsonlQualityNames,
			0xffff,
			`["capture-boundary","ordering-uncertain","correlation-incomplete","bit(3)","direction-inferred","redacted",` +
				`"bit(6)","bit(7)","bit(8)","bit(9)","bit(10)","bit(11)","bit(12)","bit(13)","bit(14)","bit(15)"]`,
		},
		{"quality retired", jsonlQualityNames, 1<<3 | 1<<6, `["bit(3)","bit(6)"]`},
		{
			"field_validity every bit",
			jsonlFieldValidityNames,
			0xff,
			`["session_id","stream_and_w","function","ptype","stype","system_bytes","bit(6)","bit(7)"]`,
		},
		{
			"record_flags every bit",
			jsonlRecordFlagNames,
			0xff,
			`["bit(0)","mono_present","bit(2)","bit(3)","bit(4)","bit(5)","bit(6)","bit(7)"]`,
		},
		{"record_flags mono_present", jsonlRecordFlagNames, 2, `["mono_present"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want+"\n", renderJSONL(t, func(jw *jsonlWriter) { jw.bitSet(tt.names, tt.v) }))
		})
	}
}

func TestJSONLWriter_BitTablesMatchNamesMethods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		names     []string
		namesFunc func(v uint32) []string
	}{
		{"quality", jsonlQualityNames, func(v uint32) []string { return Quality(v).Names() }},
		{"field_validity", jsonlFieldValidityNames, func(v uint32) []string { return FieldValidity(v).Names() }},
		{"record_flags", jsonlRecordFlagNames, func(v uint32) []string { return RecordFlags(v).Names() }},
		{
			"header flags",
			jsonlHeaderFlagNames,
			func(v uint32) []string {
				if v == fileHeaderRedactionPresent {
					return []string{"redaction-present"}
				}

				return nil
			},
		},
	}
	// The tables pin schema tracepack-jsonl/1: a bit the format names later has a name in Names but stays unnamed here,
	// so only the bits within a table are compared with Names.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requirePlainNames(t, tt.names)
			var b bytes.Buffer
			jw := newJSONLWriter(&b)
			for bit := range 32 {
				v := uint32(1) << bit
				var want []string
				if bit < len(tt.names) {
					want = tt.namesFunc(v)
				}
				if len(want) == 0 {
					want = []string{"bit(" + strconv.Itoa(bit) + ")"}
				}
				b.Reset()
				jw.bitSet(tt.names, v)
				require.NoError(t, jw.endLine())
				var got []string
				require.NoError(t, json.Unmarshal(b.Bytes(), &got))
				require.Equal(t, want, got, "bit %d", bit)
			}
		})
	}
}

// requirePlainNames checks that every name of a table is printable ASCII that needs no JSON escaping,
// as the jsonlWriter writes names verbatim.
func requirePlainNames(t *testing.T, names []string) {
	t.Helper()

	for _, name := range names {
		for i := range len(name) {
			c := name[i]
			require.True(t, c > 0x20 && c < 0x7f && c != '"' && c != '\\', "name %q", name)
		}
	}
}

func TestJSONLWriter_NoAllocationsGrowingWithInput(t *testing.T) {
	// Not parallel: the heap statistics below would count other tests' allocations.
	const n = 4 * jsonlBufferSize
	nuls := strings.Repeat("\x00", n)
	nulBytes := []byte(nuls)
	data := make([]byte, n+2)

	jw := newJSONLWriter(io.Discard)
	allocs := testing.AllocsPerRun(10, func() {
		writeString(jw, nuls)
		writeString(jw, nulBytes)
		writeString(jw, "a\u2028\x7f😀")
		jw.base64(data)
		jw.base64(data[:5])
		jw.u64(1<<63 - 1)
		jw.i64(-1 << 63)
		jw.num(4294967295)
		jw.uuid([16]byte{1})
		jw.enum(jsonlKindNames, 200)
		jw.bitSet(jsonlQualityNames, 0xffff)
		_ = jw.endLine()
	})
	require.Zero(t, allocs)

	// The heap grows by far less than the input for a much larger value;
	// the bound leaves room for allocations of goroutines outside this test.
	big := make([]byte, 64*jsonlBufferSize)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	writeString(jw, big)
	jw.base64(big)
	require.NoError(t, jw.endLine())
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16*jsonlBufferSize))
}
