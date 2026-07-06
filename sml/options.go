package sml

// ParserOption configures a Parser at construction time. Options are applied
// once in NewParser; a Parser's configuration is immutable thereafter.
type ParserOption func(*Parser)

// WithParserStrictMode selects strict ASCII parsing: escape sequences and
// hex-encoded non-printable bytes are interpreted (vs the non-strict fast path,
// the default, which assumes well-formed ASCII and no escapes).
func WithParserStrictMode(strict bool) ParserOption {
	return func(p *Parser) { p.strict = strict }
}

// QuoteStyle selects the quoting used when rendering SML text.
type QuoteStyle int

const (
	QuoteDouble QuoteStyle = iota // "value"  (default)
	QuoteSingle                   // 'value'
	QuoteNone                     // stream/function rendering only; invalid for ASCII data
)

// EncoderOption configures an Encoder at construction time.
type EncoderOption func(*Encoder)

// WithEncoderStrictMode escapes/hex-encodes non-printable ASCII bytes so the
// output is valid, round-trippable SML (vs emitting raw bytes when non-strict).
func WithEncoderStrictMode(strict bool) EncoderOption {
	return func(e *Encoder) { e.strict = strict }
}

// WithASCIIQuote selects the quote for ASCII/JIS-8 string values.
//
// QuoteNone is invalid for string data and is treated as QuoteDouble.
//
// Note: Localized (W) items are always rendered with Go-style double-quoting
// (strconv.Quote) regardless of this option.
func WithASCIIQuote(q QuoteStyle) EncoderOption {
	return func(e *Encoder) {
		if q == QuoteNone {
			q = QuoteDouble
		}
		e.asciiQuote = q
	}
}

// WithSFQuote selects the stream/function quoting in EncodeMessage's header line.
func WithSFQuote(q QuoteStyle) EncoderOption {
	return func(e *Encoder) { e.sfQuote = q }
}

// BinaryStyle selects how binary (B) item bytes are rendered in SML text.
type BinaryStyle int

const (
	// BinaryHex renders binary item bytes as hex (e.g. 0xAB). This is the default.
	BinaryHex BinaryStyle = iota
	// BinaryLiteral renders binary item bytes as unpadded base-2 literals (e.g. 0b10101011).
	BinaryLiteral
)

// WithBinaryStyle selects hex (0xAB, the default) or binary-literal (0b..) rendering
// for binary items.
//
// The parser reads both forms regardless of this option.
func WithBinaryStyle(s BinaryStyle) EncoderOption {
	return func(e *Encoder) { e.binaryStyle = s }
}

// WithIndent sets the per-level indentation unit used for list nesting (default "  ").
func WithIndent(unit string) EncoderOption {
	return func(e *Encoder) { e.indent = unit }
}
