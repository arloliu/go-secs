// Package sml provides SML (SECS Message Language) parsing and encoding for
// HSMS data messages (SEMI E5/E37).
//
// # Parsing
//
// [Parse] and [ParseStrict] are the zero-config package-level entry points.
// Each allocates a fresh [Parser] internally and is safe for concurrent use:
//
//	msgs, err := sml.Parse(`
//	    S1F1 W
//	    <L[2]
//	        <A[4] 'test'>
//	        <U4[1] 42>
//	    >
//	    .
//	`)
//
// For repeated use or custom options, construct a [Parser] via [NewParser].
// A *Parser holds mutable scan state and is NOT safe for concurrent use;
// allocate one per goroutine.
//
//	p := sml.NewParser(sml.WithParserStrictMode(true))
//	msg, err := p.ParseMessage(input)
//
// # Encoding
//
// [Encode] renders a secs2.Item to its canonical SML text — identical to
// item.ToSML(). [NewEncoder] provides a configurable form:
//
//	enc := sml.NewEncoder(
//	    sml.WithASCIIQuote(sml.QuoteSingle),
//	    sml.WithSFQuote(sml.QuoteSingle),
//	    sml.WithIndent("    "),
//	)
//	text, err := enc.EncodeMessage(msg)
//
// A *Encoder is immutable after construction and safe for concurrent use.
//
// # Strict mode
//
// Parser: [WithParserStrictMode](true) decodes non-printable ASCII bytes
// encoded as 0xHH numeric tokens (e.g. 0x0A for newline).
// Encoder: [WithEncoderStrictMode](true) emits non-printable bytes as 0xHH
// tokens, producing output that a strict-mode parser can round-trip exactly.
//
// # Error model
//
// Syntax errors return a [*ParseError] carrying the byte [ParseError.Offset],
// 1-based [ParseError.Line], 1-based [ParseError.Col], and a description
// [ParseError.Msg]. Construction and validation errors (e.g. W-bit on an even
// function, item validation via [secs2.Item.Error]) are returned as wrapped
// errors — compatible with [errors.Is] and [errors.As], but NOT [*ParseError].
// Parsing is fail-fast: the first error stops parsing.
package sml
