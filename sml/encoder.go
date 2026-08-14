package sml

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// Encoder renders secs2 Items / hsms messages to SML text.
//
// NewEncoder's default options reproduce secs2.Item.ToSML() byte-for-byte for valid item trees at or below [secs2.MaxListDepth].
// Nil-like items render as empty text.
// Deeper trees produce a diagnostic from item encoding and an error from message encoding.
// An Encoder is immutable after construction and safe for concurrent use.
type Encoder struct {
	strict      bool
	asciiQuote  QuoteStyle // QuoteDouble | QuoteSingle
	sfQuote     QuoteStyle
	binaryStyle BinaryStyle
	indent      string
}

// NewEncoder returns an Encoder.
// With no options, Encode(item) equals item.ToSML() for valid item trees at or below [secs2.MaxListDepth].
// EncodeMessage uses an UNQUOTED canonical S/F header (e.g. "S1F1 W").
func NewEncoder(opts ...EncoderOption) *Encoder {
	e := &Encoder{asciiQuote: QuoteDouble, sfQuote: QuoteNone, binaryStyle: BinaryHex, indent: "  "}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Encode renders item to SML text using the Encoder's configured options.
// It returns empty text for a nil-like item.
// For an item tree deeper than [secs2.MaxListDepth], it returns a diagnostic of the form "<!sml encode error: ...>".
func (e *Encoder) Encode(it secs2.Item) string {
	s, err := e.encodeChecked(it)
	if err != nil {
		return fmt.Sprintf("<!sml encode error: %v>", err)
	}

	return s
}

func (e *Encoder) encodeChecked(it secs2.Item) (string, error) {
	var sb strings.Builder
	if err := e.encodeItem(&sb, it, 0); err != nil {
		return "", err
	}

	return sb.String(), nil
}

// AppendEncode appends item's SML text to dst.
// It appends nothing for a nil-like item and appends the Encode diagnostic for a tree deeper than [secs2.MaxListDepth].
func (e *Encoder) AppendEncode(dst []byte, it secs2.Item) []byte {
	return append(dst, e.Encode(it)...)
}

func (e *Encoder) quoteByte() byte {
	if e.asciiQuote == QuoteSingle {
		return '\''
	}
	return '"'
}

func (e *Encoder) encodeItem(sb *strings.Builder, it secs2.Item, level int) error {
	if isNilItem(it) {
		return nil
	}

	switch {
	case it.IsEmpty():
		// EmptyItem.ToSML() == ""
	case it.IsList():
		if level >= secs2.MaxListDepth {
			return fmt.Errorf("sml: list nesting depth exceeds maximum allowed: %d", secs2.MaxListDepth)
		}

		return e.encodeList(sb, it, level)
	case it.IsASCII():
		s, _ := it.ToASCII()
		e.encodeString(sb, "A", s, e.strict) // ASCII honors strict (0xHH tokens)
	case it.IsJIS8():
		s, _ := it.ToJIS8()
		e.encodeString(sb, "J", s, false) // JIS8 has no strict numeric-token grammar — always raw-quoted
	case it.IsLocalizedStr():
		s, _ := it.ToLocalizedStr()
		sb.WriteString("<W ")
		sb.WriteString(strconv.Quote(s)) // matches secs2 %q
		sb.WriteByte('>')
	case it.IsBinary():
		e.encodeBinary(sb, it)
	case it.IsBoolean():
		e.encodeBoolean(sb, it)
	case it.IsInt8(), it.IsInt16(), it.IsInt32(), it.IsInt64():
		e.encodeInt(sb, it)
	case it.IsUint8(), it.IsUint16(), it.IsUint32(), it.IsUint64():
		e.encodeUint(sb, it)
	case it.IsFloat32(), it.IsFloat64():
		e.encodeFloat(sb, it)
	default:
		// unknown item type: no output
	}

	return nil
}

// encodeString renders an ASCII/JIS8 string. strict==true (ASCII only) emits
// quoted runs + 0xHH tokens; otherwise the raw value in quotes (matches ToSML).
func (e *Encoder) encodeString(sb *strings.Builder, tok, s string, strict bool) {
	sb.WriteByte('<')
	sb.WriteString(tok)
	sb.WriteByte('[')
	sb.WriteString(strconv.Itoa(len(s)))
	sb.WriteString("] ")
	if strict {
		// strict emits its OWN quoted runs + 0xHH tokens (Task 3 Step 5).
		writeStrictASCII(sb, s, e.quoteByte())
	} else {
		// non-strict: raw value in quotes — matches secs2 ToSML byte-for-byte.
		q := e.quoteByte()
		sb.WriteByte(q)
		sb.WriteString(s)
		sb.WriteByte(q)
	}
	sb.WriteByte('>')
}

func (e *Encoder) encodeBinary(sb *strings.Builder, it secs2.Item) {
	n := it.Size()
	sb.WriteString("<B[")
	sb.WriteString(strconv.Itoa(n))
	sb.WriteByte(']')
	for i := range n {
		b, _ := it.ByteAt(i)
		if e.binaryStyle == BinaryLiteral {
			// UNPADDED base-2 (e.g. 0b1, 0b10, 0b11111111), matching secs2 ToSML's opt-in form.
			sb.WriteString(" 0b")
			sb.WriteString(strconv.FormatInt(int64(b), 2))
		} else {
			fmt.Fprintf(sb, " 0x%02X", b)
		}
	}
	sb.WriteByte('>')
}

func (e *Encoder) encodeBoolean(sb *strings.Builder, it secs2.Item) {
	sb.WriteString("<BOOLEAN[")
	sb.WriteString(strconv.Itoa(it.Size()))
	sb.WriteByte(']')
	for v := range it.Bools() {
		if v {
			sb.WriteString(" True")
		} else {
			sb.WriteString(" False")
		}
	}
	sb.WriteByte('>')
}

func (e *Encoder) intByteSize(it secs2.Item) int {
	switch {
	case it.IsInt8(), it.IsUint8():
		return 1
	case it.IsInt16(), it.IsUint16():
		return 2
	case it.IsInt32(), it.IsUint32():
		return 4
	default:
		return 8
	}
}

func (e *Encoder) encodeInt(sb *strings.Builder, it secs2.Item) {
	sb.WriteString("<I")
	sb.WriteString(strconv.Itoa(e.intByteSize(it)))
	sb.WriteByte('[')
	sb.WriteString(strconv.Itoa(it.Size()))
	sb.WriteByte(']')
	for v := range it.Ints() {
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatInt(v, 10))
	}
	sb.WriteByte('>')
}

func (e *Encoder) encodeUint(sb *strings.Builder, it secs2.Item) {
	sb.WriteString("<U")
	sb.WriteString(strconv.Itoa(e.intByteSize(it)))
	sb.WriteByte('[')
	sb.WriteString(strconv.Itoa(it.Size()))
	sb.WriteByte(']')
	for v := range it.Uints() {
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatUint(v, 10))
	}
	sb.WriteByte('>')
}

func (e *Encoder) encodeFloat(sb *strings.Builder, it secs2.Item) {
	bs, prec := 8, 17
	if it.IsFloat32() {
		bs, prec = 4, 9
	}
	sb.WriteString("<F")
	sb.WriteString(strconv.Itoa(bs))
	sb.WriteByte('[')
	sb.WriteString(strconv.Itoa(it.Size()))
	sb.WriteByte(']')
	for v := range it.Floats() {
		sb.WriteByte(' ')
		sb.WriteString(strconv.FormatFloat(v, 'G', prec, bs*8))
	}
	sb.WriteByte('>')
}

func (e *Encoder) encodeList(sb *strings.Builder, it secs2.Item, level int) error {
	ind := strings.Repeat(e.indent, level)
	if it.Size() == 0 {
		sb.WriteString(ind)
		sb.WriteString("<L[0]>")

		return nil
	}
	sb.WriteString(ind)
	sb.WriteString("<L[")
	sb.WriteString(strconv.Itoa(it.Size()))
	sb.WriteString("]\n")
	child := strings.Repeat(e.indent, level+1)
	for c := range it.Items() {
		if !isNilItem(c) && c.IsList() {
			if err := e.encodeItem(sb, c, level+1); err != nil {
				return err
			}
		} else {
			sb.WriteString(child)
			if err := e.encodeItem(sb, c, level+1); err != nil {
				return err
			}
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(ind)
	sb.WriteByte('>')

	return nil
}

// EncodeMessage renders the message header line then its body.
//
// The body is obtained via msg.Item(), which for a decoded (raw-frame) message decodes lazily and may return an error —
// propagated, not swallowed.
// A message with an empty body renders as just the header line followed by the terminating ".".
// A nil message or a body tree deeper than [secs2.MaxListDepth] returns an error.
func (e *Encoder) EncodeMessage(msg *hsms.DataMessage) (string, error) {
	if msg == nil {
		return "", fmt.Errorf("sml: nil data message")
	}

	item, err := msg.Item()
	if err != nil {
		return "", fmt.Errorf("sml: encode message body: %w", err) // wrap lazy-decode error, per spec §7
	}
	var sb strings.Builder
	e.writeHeader(&sb, msg)
	sb.WriteByte('\n')
	if err := e.encodeItem(&sb, item, 0); err != nil {
		return "", fmt.Errorf("sml: encode message body: %w", err)
	}
	sb.WriteString("\n.")

	return sb.String(), nil
}

func (e *Encoder) writeHeader(sb *strings.Builder, msg *hsms.DataMessage) {
	e.writeSFQuote(sb)
	sb.WriteByte('S')
	sb.WriteString(strconv.Itoa(int(msg.Stream())))
	sb.WriteByte('F')
	sb.WriteString(strconv.Itoa(int(msg.Function())))
	e.writeSFQuote(sb)
	if msg.WaitBit() {
		sb.WriteString(" W")
	}
}

func (e *Encoder) writeSFQuote(sb *strings.Builder) {
	switch e.sfQuote { //nolint:exhaustive // QuoteNone and unknown values handled by default (no output)
	case QuoteSingle:
		sb.WriteByte('\'')
	case QuoteDouble:
		sb.WriteByte('"')
	default:
		// QuoteNone: no quote character
	}
}

// Encode renders item to SML text using default (non-strict, canonical) options.
// For a valid item tree at or below [secs2.MaxListDepth], the output equals item.ToSML() byte-for-byte.
// It returns empty text for a nil-like item and an "<!sml encode error: ...>" diagnostic for a deeper tree.
func Encode(it secs2.Item) string { return NewEncoder().Encode(it) }

// EncodeStrict renders item with strict (round-trippable) ASCII escaping.
// It returns empty text for a nil-like item
// and an "<!sml encode error: ...>" diagnostic for a tree deeper than [secs2.MaxListDepth].
func EncodeStrict(it secs2.Item) string {
	return NewEncoder(WithEncoderStrictMode(true)).Encode(it)
}

// EncodeMessage renders msg to SML text using an encoder configured by opts.
// The defaults are hex binary, double-quoted ASCII, and an unquoted S/F header.
//
// It mirrors the item-level Encode shortcut.
// The body is decoded lazily via msg.Item(); a decode error is returned, not swallowed.
// A nil message or a body tree deeper than [secs2.MaxListDepth] returns an error.
func EncodeMessage(msg *hsms.DataMessage, opts ...EncoderOption) (string, error) {
	return NewEncoder(opts...).EncodeMessage(msg)
}

// MustEncodeMessage is EncodeMessage for logging/tests where a body-decode error is not actionable;
// on error it returns a diagnostic string of the form "<!sml encode error: ...>" rather than panicking,
// so it is always safe to embed in a log line.
//
// This deliberately deviates from Go's usual Must* convention (panic-on-error): the whole point of this function is
// that a single malformed inbound message must never be able to crash the process trying to log it.
func MustEncodeMessage(msg *hsms.DataMessage, opts ...EncoderOption) string {
	s, err := EncodeMessage(msg, opts...)
	if err != nil {
		return fmt.Sprintf("<!sml encode error: %v>", err)
	}

	return s
}

func isNilItem(it secs2.Item) bool {
	if it == nil {
		return true
	}

	v := reflect.ValueOf(it)
	k := v.Kind()

	return (k == reflect.Chan || k == reflect.Func || k == reflect.Map || k == reflect.Pointer || k == reflect.Slice) && v.IsNil()
}

// writeStrictASCII renders s as printable runs quoted with `quote` and
// non-printable bytes as 0xHH numeric tokens, space-separated — the exact form
// parseASCIIStrict reads back. Empty string renders as an empty quoted run.
func writeStrictASCII(sb *strings.Builder, s string, quote byte) {
	if s == "" {
		sb.WriteByte(quote)
		sb.WriteByte(quote)
		return
	}
	const hex = "0123456789ABCDEF"
	first := true
	inRun := false
	sep := func() {
		if !first {
			sb.WriteByte(' ')
		}
		first = false
	}
	endRun := func() {
		if inRun {
			sb.WriteByte(quote)
			inRun = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f { // printable: extend/open a quoted run
			if !inRun {
				sep()
				sb.WriteByte(quote)
				inRun = true
			}
			if c == quote || c == '\\' || c == '>' {
				sb.WriteByte('\\')
			}
			sb.WriteByte(c)
		} else { // non-printable: close any run, emit a 0xHH token
			endRun()
			sep()
			sb.WriteByte('0')
			sb.WriteByte('x')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0x0f])
		}
	}
	endRun()
}
