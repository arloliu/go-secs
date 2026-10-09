package corpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/tracepack"
)

// canonIndent is the indentation of one nesting level in the canonical form.
const canonIndent = "  "

// ErrNotCanonical reports JSON text that is not in the corpus's canonical form,
// or a value that has no canonical form: a null, a number that is not an unsigned integer, or a malformed decimal string.
var ErrNotCanonical = errors.New("corpus: not canonical JSON")

// U64 is a u64 value, written as a decimal string (the tracepack JSONL specification §3).
type U64 uint64

// I64 is an i64 value, written as a decimal string (the tracepack JSONL specification §3).
type I64 int64

// Document is the content of a corpus file, which encodes itself in its canonical form,
// its arrays sorted into the orders its schema defines.
type Document interface {
	Marshal() ([]byte, error)
}

// Coverage is a coverage entry rendered as a TLV object under the coverage registry
// (the tracepack JSONL specification §4): one key per known nested tag present, in tag order,
// then the unknown nested entries in stored order, the key omitted when there is none.
type Coverage struct {
	CaptureID *string        `json:"capture_id,omitempty"`
	SeqFirst  *U64           `json:"seq_first,omitempty"`
	SeqLast   *U64           `json:"seq_last,omitempty"`
	TimeStart *I64           `json:"time_start,omitempty"`
	TimeEnd   *I64           `json:"time_end,omitempty"`
	Unknown   []UnknownEntry `json:"unknown,omitempty"`
}

// UnknownEntry is a nested entry whose tag the registry does not know,
// with its raw value (the tracepack JSONL specification §4).
type UnknownEntry struct {
	Tag       uint16 `json:"tag"`
	ValueType uint8  `json:"value_type"`
	Value     []byte `json:"value"`
}

// canonWriter renders JSON tokens in the canonical form.
type canonWriter struct {
	buf []byte
}

var (
	_ json.Marshaler   = U64(0)
	_ json.Unmarshaler = (*U64)(nil)
	_ json.Marshaler   = I64(0)
	_ json.Unmarshaler = (*I64)(nil)
)

// CoverageFrom renders c as a coverage object.
func CoverageFrom(c *tracepack.Coverage) Coverage {
	var out Coverage
	if c.CaptureID != nil {
		out.CaptureID = new(c.CaptureID.String())
	}
	if c.SeqFirst != nil {
		out.SeqFirst = new(U64(*c.SeqFirst))
	}
	if c.SeqLast != nil {
		out.SeqLast = new(U64(*c.SeqLast))
	}
	if c.TimeStart != nil {
		out.TimeStart = new(I64(*c.TimeStart))
	}
	if c.TimeEnd != nil {
		out.TimeEnd = new(I64(*c.TimeEnd))
	}
	for _, e := range c.Unknown {
		// A nil value would encode as null; an empty value is "".
		out.Unknown = append(out.Unknown, UnknownEntry{Tag: e.Tag, ValueType: e.Type, Value: append([]byte{}, e.Value...)})
	}

	return out
}

// Marshal encodes v with encoding/json and returns it in the canonical form (Canonical).
//
// Returns:
//   - []byte: the canonical JSON text, ending in LF.
//   - error: the error of encoding/json,
//     or ErrNotCanonical for a value with no canonical form, such as a nil slice encoded as null.
func Marshal(v any) ([]byte, error) {
	compact, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return Canonical(compact)
}

// Canonical returns the JSON text data in the canonical form of the tracepack corpus specification §4:
// two-space indentation, one member or element per line, an empty object or array as {} or [],
// keys in the order data holds them, strings escaped only as the tracepack JSONL specification §3 escapes them,
// and a final LF.
// It accepts only the values the corpus schemas use: objects, arrays, strings, booleans and unsigned integers.
//
// Returns:
//   - []byte: the canonical text.
//   - error: ErrNotCanonical for a null, a number that is not an unsigned integer without leading zeros,
//     or text after the value; the error of encoding/json for malformed text.
func Canonical(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var w canonWriter
	if err := w.value(dec, 0); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: text after the value", ErrNotCanonical)
	}

	return append(w.buf, '\n'), nil
}

// Unmarshal decodes the canonical JSON text data into doc and requires that doc encodes back to data byte for byte,
// so a key out of order, an array out of its schema's order, an empty array where the schema omits the key,
// or any other form but the canonical one is refused.
//
// Returns:
//   - error: the error of encoding/json, unknown keys included, or of doc's Marshal,
//     or an error wrapping ErrNotCanonical when data is not the canonical encoding of what it decodes to.
func Unmarshal(data []byte, doc Document) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("%w: text after the value", ErrNotCanonical)
	}

	again, err := doc.Marshal()
	if err != nil {
		return err
	}
	if !bytes.Equal(again, data) {
		return fmt.Errorf("%w: the text differs from the canonical encoding of its value", ErrNotCanonical)
	}

	return nil
}

// parseUUID parses a UUID in its canonical lowercase form.
func parseUUID(s string) (tracepack.UUID, error) {
	u, err := tracepack.ParseUUID(s)
	if err != nil {
		return tracepack.UUID{}, err
	}
	if u.String() != s {
		return tracepack.UUID{}, fmt.Errorf("%w: UUID %q is not lowercase", ErrNotCanonical, s)
	}

	return u, nil
}

// unquoteDecimal returns the digits of the JSON string b holding a decimal integer,
// with its sign when signed allows one,
// and refuses every other form: no '+', no leading zero but "0" itself, no "-0".
func unquoteDecimal(b []byte, signed bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("%w: a decimal string expected, got %s", ErrNotCanonical, b)
	}
	digits := s
	if signed {
		digits = strings.TrimPrefix(s, "-")
	}
	switch {
	case digits == "" || strings.Trim(digits, "0123456789") != "":
		return "", fmt.Errorf("%w: %q is not a decimal integer", ErrNotCanonical, s)
	case len(digits) > 1 && digits[0] == '0', s == "-0":
		return "", fmt.Errorf("%w: %q is not in its shortest form", ErrNotCanonical, s)
	}

	return s, nil
}

// MarshalJSON writes v as a decimal string.
func (v U64) MarshalJSON() ([]byte, error) {
	return append(strconv.AppendUint([]byte{'"'}, uint64(v), 10), '"'), nil
}

// UnmarshalJSON reads a decimal string in its canonical form.
func (v *U64) UnmarshalJSON(b []byte) error {
	s, err := unquoteDecimal(b, false)
	if err != nil {
		return err
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	*v = U64(n)

	return nil
}

// MarshalJSON writes v as a decimal string.
func (v I64) MarshalJSON() ([]byte, error) {
	return append(strconv.AppendInt([]byte{'"'}, int64(v), 10), '"'), nil
}

// UnmarshalJSON reads a decimal string in its canonical form.
func (v *I64) UnmarshalJSON(b []byte) error {
	s, err := unquoteDecimal(b, true)
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	*v = I64(n)

	return nil
}

// value renders the next JSON value of dec, at nesting depth depth.
func (w *canonWriter) value(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}

	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			return w.container(dec, depth, '{', '}', true)
		}

		return w.container(dec, depth, '[', ']', false)
	case string:
		w.str(t)
	case json.Number:
		if !isUnsignedInteger(string(t)) {
			return fmt.Errorf("%w: number %s is not an unsigned integer", ErrNotCanonical, t)
		}
		w.buf = append(w.buf, t...)
	case bool:
		w.buf = strconv.AppendBool(w.buf, t)
	default:
		return fmt.Errorf("%w: null", ErrNotCanonical)
	}

	return nil
}

// container renders an object (keyed) or an array whose opening delimiter dec has just returned:
// each member on its own line, indented one level below depth, and the closing delimiter on its own line;
// an empty container is the two delimiters alone.
func (w *canonWriter) container(dec *json.Decoder, depth int, open, closing byte, keyed bool) error {
	w.buf = append(w.buf, open)
	n := 0
	for dec.More() {
		if n > 0 {
			w.buf = append(w.buf, ',')
		}
		n++
		w.newline(depth + 1)
		if keyed {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			// encoding/json only returns a string token in a key position.
			name, _ := key.(string)
			w.str(name)
			w.buf = append(w.buf, ':', ' ')
		}
		if err := w.value(dec, depth+1); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if n > 0 {
		w.newline(depth)
	}
	w.buf = append(w.buf, closing)

	return nil
}

// newline starts a line indented to depth.
func (w *canonWriter) newline(depth int) {
	w.buf = append(w.buf, '\n')
	for range depth {
		w.buf = append(w.buf, canonIndent...)
	}
}

// str writes s as a JSON string with the minimal escaping of the tracepack JSONL specification §3:
// '"' and '\\', the five short escapes, \u00xx in lowercase hex for every other code point below U+0020,
// and every other code point as its UTF-8 bytes.
func (w *canonWriter) str(s string) {
	const hex = "0123456789abcdef"
	w.buf = append(w.buf, '"')
	for i := range len(s) {
		c := s[i]
		switch c {
		case '"', '\\':
			w.buf = append(w.buf, '\\', c)
		case '\b':
			w.buf = append(w.buf, '\\', 'b')
		case '\t':
			w.buf = append(w.buf, '\\', 't')
		case '\n':
			w.buf = append(w.buf, '\\', 'n')
		case '\f':
			w.buf = append(w.buf, '\\', 'f')
		case '\r':
			w.buf = append(w.buf, '\\', 'r')
		default:
			if c < 0x20 {
				w.buf = append(w.buf, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			} else {
				w.buf = append(w.buf, c)
			}
		}
	}
	w.buf = append(w.buf, '"')
}

// isUnsignedInteger reports whether s is the decimal form of an unsigned integer: digits only, no leading zero but "0".
func isUnsignedInteger(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}

	return !slices.ContainsFunc([]byte(s), func(c byte) bool { return c < '0' || c > '9' })
}
