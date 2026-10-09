package tracepack

import (
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
)

// jsonlBufferSize is the capacity of the one buffer a jsonlWriter encodes into,
// so a line of any length reaches the destination in pieces of at most this many bytes.
const jsonlBufferSize = 64 << 10

// Enum names of schema tracepack-jsonl/1, indexed by value (the tracepack JSONL specification §3.1).
// A value past the end of its table has no name and is written "unknown(<n>)":
// the retired pack_role 5 is such a value.
var (
	jsonlKindNames         = []string{"unknown", "data", "control", "transport-event", "annotation"}
	jsonlDirNames          = []string{"unknown", "host-to-equipment", "equipment-to-host", "local"}
	jsonlFidelityNames     = []string{"unknown", "wire-exact", "re-encoded", "reconstructed", "synthesized", "not-applicable"}
	jsonlDecodeStatusNames = []string{
		"not-attempted", "ok", "ok-with-trailing", "short-frame", "length-mismatch", "bad-ptype", "bad-stype",
		"control-with-body", "oversized", "item-decode-error", "reconstructed-ok", "parse-failed", "build-rejected", "not-applicable",
	}
	jsonlTransportNames         = []string{"unknown", "hsms-ss", "secs1-normalised"}
	jsonlCaptureMethodNames     = []string{"unknown", "raw-stream", "decoded-message", "log", "generator"}
	jsonlVantageNames           = []string{"unknown", "host", "equipment", "intermediary", "network", "none"}
	jsonlTimeSourceNames        = []string{"unknown", "capture-clock", "source-log", "generator"}
	jsonlLifecycleCoverageNames = []string{"unknown", "subscribed", "none"}
	jsonlPackRoleNames          = []string{"unknown", "segment", "archive", "extract", "repair"}
	jsonlDigestAlgorithmNames   = []string{"unknown", "hmac-sha256"}
	jsonlEventNames             = []string{
		"unknown", "state-transition", "timer-expiry", "socket-accept", "socket-connect", "socket-close", "capture-boundary", "clock-step",
	}
	jsonlStateNames = []string{"unknown", "not-connected", "not-selected", "selected"}
	jsonlTimerNames = []string{"none", "T1", "T2", "T3", "T4", "T5", "T6", "T7", "T8"}
	jsonlCauseNames = []string{
		"unknown", "local-open", "local-close", "select-accepted", "select-rejected", "local-deselect", "peer-deselect",
		"local-separate", "peer-separate", "timer-expiry", "linktest-failure", "transport-error", "peer-close", "implementation-fault",
	}
	jsonlSocketRoleNames     = []string{"unknown", "passive", "active"}
	jsonlBoundaryKindNames   = []string{"unknown", "start", "stop", "gap", "stop-unclean"}
	jsonlAnnotationKindNames = []string{"unknown", "note", "unparsed-entry", "skipped-bytes", "unrecognised-line"}
)

// Bit names of schema tracepack-jsonl/1, indexed by bit (the tracepack JSONL specification §3.2).
// An empty name, or a bit past the end of its table, has no name and is written "bit(<n>)".
var (
	jsonlHeaderFlagNames    = []string{"redaction-present"}
	jsonlQualityNames       = []string{"capture-boundary", "ordering-uncertain", "correlation-incomplete", "", "direction-inferred", "redacted"}
	jsonlFieldValidityNames = []string{"session_id", "stream_and_w", "function", "ptype", "stype", "system_bytes"}
	jsonlRecordFlagNames    = []string{"", "mono_present"}
)

// jsonlWriter writes JSON lines in the byte form of the tracepack JSONL specification §2–§3 to an io.Writer.
//
// It encodes into one buffer of jsonlBufferSize bytes, allocated once,
// and passes the buffer to the destination when the next indivisible piece does not fit and at the end of each line,
// so a line may take several Write calls and memory never grows with a value's length.
// An escape sequence and a base64 group are such indivisible pieces: each lands whole in one Write.
//
// Values and keys are separated by commas automatically:
// a key or value written after a completed value is preceded by one,
// so a caller writes keys, values, and the ends of objects and arrays in order and nothing else.
//
// The first write error is kept: from then on nothing is written and endLine returns it.
//
// The zero value is not usable: its buffer has no capacity, so a write would flush forever without progress.
// newJSONLWriter returns a usable one.
type jsonlWriter struct {
	w         io.Writer
	buf       []byte
	err       error
	needComma bool
}

// newJSONLWriter returns a jsonlWriter writing to w.
func newJSONLWriter(w io.Writer) *jsonlWriter {
	return &jsonlWriter{w: w, buf: make([]byte, 0, jsonlBufferSize)}
}

// writeVerbatim copies s into jw's buffer, flushing whenever the buffer is full.
func writeVerbatim[T ~string | ~[]byte](jw *jsonlWriter, s T) {
	for len(s) > 0 && jw.err == nil {
		if len(jw.buf) == cap(jw.buf) {
			jw.flush()

			continue
		}
		n := copy(jw.buf[len(jw.buf):cap(jw.buf)], s)
		jw.buf = jw.buf[:len(jw.buf)+n]
		s = s[n:]
	}
}

// writeEscaped writes the contents of a JSON string holding s, escaped per the tracepack JSONL specification §3,
// without the quotes.
// s is valid UTF-8, so only ASCII bytes are ever escaped and every byte of a multi-byte sequence is copied as stored.
func writeEscaped[T ~string | ~[]byte](jw *jsonlWriter, s T) {
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		writeVerbatim(jw, s[start:i])
		jw.escape(c)
		start = i + 1
	}
	writeVerbatim(jw, s[start:])
}

// writeString writes s as a JSON string value.
func writeString[T ~string | ~[]byte](jw *jsonlWriter, s T) {
	jw.sep()
	jw.writeByte('"')
	writeEscaped(jw, s)
	jw.writeByte('"')
	jw.needComma = true
}

// key writes the key of the next object member.
// Keys are the fixed names of the tracepack JSONL specification, which need no escaping.
func (jw *jsonlWriter) key(name string) {
	jw.sep()
	jw.writeByte('"')
	writeVerbatim(jw, name)
	writeVerbatim(jw, `":`)
	jw.needComma = false
}

// beginObject starts an object value.
func (jw *jsonlWriter) beginObject() {
	jw.sep()
	jw.writeByte('{')
	jw.needComma = false
}

// endObject ends the innermost open object.
func (jw *jsonlWriter) endObject() {
	jw.writeByte('}')
	jw.needComma = true
}

// beginArray starts an array value.
func (jw *jsonlWriter) beginArray() {
	jw.sep()
	jw.writeByte('[')
	jw.needComma = false
}

// endArray ends the innermost open array.
func (jw *jsonlWriter) endArray() {
	jw.writeByte(']')
	jw.needComma = true
}

// endLine ends the line, passes everything buffered to the destination,
// and returns the first write error of jw, if any.
func (jw *jsonlWriter) endLine() error {
	jw.writeByte('\n')
	jw.flush()
	jw.needComma = false

	return jw.err
}

// num writes a u8, u16 or u32 value as a JSON number.
func (jw *jsonlWriter) num(v uint32) {
	jw.sep()
	var tmp [10]byte
	jw.writePiece(strconv.AppendUint(tmp[:0], uint64(v), 10))
	jw.needComma = true
}

// u64 writes a u64 value as a JSON string holding its decimal form.
func (jw *jsonlWriter) u64(v uint64) {
	jw.sep()
	var tmp [22]byte
	b := append(tmp[:0], '"')
	b = strconv.AppendUint(b, v, 10)
	jw.writePiece(append(b, '"'))
	jw.needComma = true
}

// i64 writes an i64 value as a JSON string holding its decimal form.
func (jw *jsonlWriter) i64(v int64) {
	jw.sep()
	var tmp [22]byte
	b := append(tmp[:0], '"')
	b = strconv.AppendInt(b, v, 10)
	jw.writePiece(append(b, '"'))
	jw.needComma = true
}

// boolean writes v as a JSON boolean.
func (jw *jsonlWriter) boolean(v bool) {
	jw.sep()
	if v {
		writeVerbatim(jw, "true")
	} else {
		writeVerbatim(jw, "false")
	}
	jw.needComma = true
}

// uuid writes u, 16 bytes in stored order, as a JSON string holding its canonical lowercase form.
func (jw *jsonlWriter) uuid(u [16]byte) {
	const hex = "0123456789abcdef"

	jw.sep()
	var tmp [38]byte
	b := append(tmp[:0], '"')
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hex[c>>4], hex[c&0x0f])
	}
	jw.writePiece(append(b, '"'))
	jw.needComma = true
}

// base64 writes p as a JSON string holding its standard base64 form with padding.
// Whole 3-byte groups are encoded straight into the buffer as many as fit at a time, the final partial group last.
func (jw *jsonlWriter) base64(p []byte) {
	jw.sep()
	jw.writeByte('"')
	for len(p) >= 3 && jw.err == nil {
		groups := min((cap(jw.buf)-len(jw.buf))/4, len(p)/3)
		if groups == 0 {
			jw.flush()

			continue
		}
		n := len(jw.buf)
		base64.StdEncoding.Encode(jw.buf[n:n+groups*4], p[:groups*3])
		jw.buf = jw.buf[:n+groups*4]
		p = p[groups*3:]
	}
	if jw.err != nil {
		return
	}
	if len(p) > 0 {
		var tail [4]byte
		base64.StdEncoding.Encode(tail[:], p)
		jw.writePiece(tail[:])
	}
	jw.writeByte('"')
	jw.needComma = true
}

// enum writes v as a JSON string holding its name in names, or "unknown(<n>)" when it has none.
func (jw *jsonlWriter) enum(names []string, v uint8) {
	jw.sep()
	if int(v) < len(names) && names[v] != "" {
		jw.writeByte('"')
		writeVerbatim(jw, names[v])
		jw.writeByte('"')
	} else {
		jw.unnamed("unknown(", uint64(v))
	}
	jw.needComma = true
}

// bitSet writes v as a JSON array of the names in names of its set bits, in ascending bit order,
// a set bit without a name as "bit(<n>)".
func (jw *jsonlWriter) bitSet(names []string, v uint32) {
	jw.beginArray()
	for bit := range 32 {
		if v&(1<<bit) == 0 {
			continue
		}
		jw.sep()
		if bit < len(names) && names[bit] != "" {
			jw.writeByte('"')
			writeVerbatim(jw, names[bit])
			jw.writeByte('"')
		} else {
			jw.unnamed("bit(", uint64(bit))
		}
		jw.needComma = true
	}
	jw.endArray()
}

// unnamed writes the JSON string prefix, n in decimal, then ")".
func (jw *jsonlWriter) unnamed(prefix string, n uint64) {
	jw.writeByte('"')
	writeVerbatim(jw, prefix)
	var tmp [22]byte
	b := strconv.AppendUint(tmp[:0], n, 10)
	jw.writePiece(append(b, ')', '"'))
}

// escape writes the escape sequence of the byte c, which is below 0x20, '"' or '\'.
func (jw *jsonlWriter) escape(c byte) {
	const hex = "0123456789abcdef"

	var tmp [6]byte
	b := append(tmp[:0], '\\')
	switch c {
	case '"', '\\':
		b = append(b, c)
	case '\b':
		b = append(b, 'b')
	case '\t':
		b = append(b, 't')
	case '\n':
		b = append(b, 'n')
	case '\f':
		b = append(b, 'f')
	case '\r':
		b = append(b, 'r')
	default:
		b = append(b, 'u', '0', '0', hex[c>>4], hex[c&0x0f])
	}
	jw.writePiece(b)
}

// sep writes the comma that separates a key or value from the completed value before it.
func (jw *jsonlWriter) sep() {
	if jw.needComma {
		jw.writeByte(',')
		jw.needComma = false
	}
}

// writeByte writes the byte c.
func (jw *jsonlWriter) writeByte(c byte) {
	if jw.err != nil {
		return
	}
	if len(jw.buf) == cap(jw.buf) {
		jw.flush()
		if jw.err != nil {
			return
		}
	}
	jw.buf = append(jw.buf, c)
}

// writePiece writes p, at most a few dozen bytes, whole into one Write: it flushes first when p does not fit.
func (jw *jsonlWriter) writePiece(p []byte) {
	if jw.err != nil {
		return
	}
	if cap(jw.buf)-len(jw.buf) < len(p) {
		jw.flush()
		if jw.err != nil {
			return
		}
	}
	jw.buf = append(jw.buf, p...)
}

// flush passes the buffered bytes to the destination and empties the buffer.
// A Write that accepts fewer bytes than given without an error fails with io.ErrShortWrite.
func (jw *jsonlWriter) flush() {
	if jw.err != nil || len(jw.buf) == 0 {
		return
	}
	n, err := jw.w.Write(jw.buf)
	if err == nil && n < len(jw.buf) {
		err = io.ErrShortWrite
	}
	if err != nil {
		jw.err = fmt.Errorf("tracepack: write the JSONL export: %w", err)
	}
	jw.buf = jw.buf[:0]
}
