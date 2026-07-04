# SML Parser and Encoder

The `sml` package provides SML (SECS Message Language) parsing and encoding for
HSMS data messages (SEMI E5/E37).

Since SML lacks a single definitive standard, various formatting conventions
exist — different quoting styles for stream/function codes, optional message
names, line comments, and different ASCII quoting. This package supports all
common variations through configuration options.

## Parsing

### Quick start

The package-level functions `Parse` and `ParseStrict` are the simplest entry
points. Each allocates a fresh parser internally and is safe for concurrent use:

```go
msgs, err := sml.Parse(`
    S1F1 W
    <L[2]
        <A[4] 'test'>
        <U4[1] 42>
    >
    .
`)
```

`Parse` uses the default non-strict (fast) ASCII parser. `ParseStrict` enables
strict ASCII decoding (see [Strict Mode](#strict-mode) below).

### Custom parser

For repeated parsing or custom options, construct a `*Parser` with `NewParser`:

```go
p := sml.NewParser(sml.WithParserStrictMode(true))

// Parse one or more messages from a single input string (fail-fast)
msgs, err := p.Parse(input)

// Parse exactly one message
msg, err := p.ParseMessage(input)

// Parse only the message header (stream, function, W-bit); body is discarded
msg, err := p.ParseHeader(input)
```

A `*Parser` holds mutable scan state. It is **not safe for concurrent use**:
allocate one per goroutine, or use the package-level `Parse`/`ParseStrict`
functions (which each allocate a fresh parser).

### Parser option

| Option | Description |
|--------|-------------|
| `WithParserStrictMode(bool)` | Enable strict ASCII parsing (default: `false`) |

## Encoding

### Quick start

`Encode` renders a `secs2.Item` to its canonical SML text, identical to
`item.ToSML()`:

```go
text := sml.Encode(item)
```

`EncodeStrict` renders with non-printable ASCII bytes hex-escaped:

```go
text := sml.EncodeStrict(item)
```

### Custom encoder

`NewEncoder` provides configurable encoding. A `*Encoder` is immutable after
construction and **safe for concurrent use**:

```go
enc := sml.NewEncoder(
    sml.WithASCIIQuote(sml.QuoteSingle),
    sml.WithSFQuote(sml.QuoteSingle),
    sml.WithIndent("    "),
)

// Encode a secs2.Item to SML text
text := enc.Encode(item)

// Append SML text to an existing byte slice
dst = enc.AppendEncode(dst, item)

// Encode a full HSMS message (header line + body + terminating ".")
text, err := enc.EncodeMessage(msg)
```

The default `NewEncoder()` (no options) produces the same output as
`item.ToSML()`, with an unquoted S/F header (e.g. `S1F1 W`).

### Encoder options

| Option | Values | Default | Description |
|--------|--------|---------|-------------|
| `WithASCIIQuote(QuoteStyle)` | `QuoteDouble`, `QuoteSingle` | `QuoteDouble` | Quote character for ASCII/JIS-8 string values |
| `WithSFQuote(QuoteStyle)` | `QuoteDouble`, `QuoteSingle`, `QuoteNone` | `QuoteNone` | Quote character for the S/F header in `EncodeMessage` |
| `WithIndent(string)` | any string | `"  "` (two spaces) | Per-level indentation for list nesting |
| `WithEncoderStrictMode(bool)` | `true`/`false` | `false` | Hex-escape non-printable ASCII bytes |
| `WithBinaryStyle(BinaryStyle)` | `BinaryHex`, `BinaryLiteral` | `BinaryHex` | Rendering for binary (`B`) item bytes: hex (`0xAB`) or binary-literal (`0b10101011`); the parser reads both regardless of this option |

`QuoteStyle` constants:

| Constant | Rendering | Notes |
|----------|-----------|-------|
| `QuoteDouble` | `"value"` | Default for ASCII data |
| `QuoteSingle` | `'value'` | |
| `QuoteNone` | no quotes | Default for S/F header; not valid for ASCII data (falls back to `QuoteDouble`) |

## Strict Mode

Strict mode applies independently to parsing and encoding.

**Parser** (`WithParserStrictMode(true)`):
- Decodes non-printable ASCII bytes represented as hex tokens (e.g. `0x0A` for
  newline, `0x09` for tab).
- Handles escape sequences (`\\`, `\'`, `\"`).

**Encoder** (`WithEncoderStrictMode(true)`):
- Emits non-printable bytes (< 0x20 or >= 0x7F) as `0xHH` tokens.
- Printable runs are enclosed in the configured quote character, with internal
  quotes and backslashes escaped.
- Output is round-trippable through a strict-mode parser.

Example: strict-encoded ASCII containing a newline:

```
<A[16] 'first line' 0x0A 'second line'>
```

## Error Model

Not all parse failures are `*ParseError`:

- **Syntax errors** (invalid token, malformed item, bad header) are returned as `*sml.ParseError`
  and carry precise position information. Test with `errors.As`.
- **Empty-input** (`ParseMessage`/`ParseHeader` called with empty or whitespace-only input)
  returns the sentinel `sml.ErrNoMessage`. Test with `errors.Is`.
- **Construction or validation failures** (returned by the underlying hsms/secs2 libraries) are
  plain or wrapped errors, not `*ParseError`. Test with `errors.Is`/`errors.As` as appropriate.

All parsing is fail-fast: the first error stops further processing.

```go
msgs, err := sml.Parse(input)
if err != nil {
    var pe *sml.ParseError
    if errors.As(err, &pe) {
        fmt.Printf("syntax error at line %d, col %d (offset %d): %s\n",
            pe.Line, pe.Col, pe.Offset, pe.Msg)
    }
}

// Empty input returns ErrNoMessage, not *ParseError:
msg, err := p.ParseMessage("")
if errors.Is(err, sml.ErrNoMessage) {
    // input was empty
}
```

`ParseError` fields:

| Field | Type | Description |
|-------|------|-------------|
| `Offset` | `int` | 0-based byte offset into the input |
| `Line` | `int` | 1-based line number |
| `Col` | `int` | 1-based column number |
| `Msg` | `string` | Human-readable description of the syntax error |

## Concurrency

| Type / Function | Concurrent use |
|-----------------|---------------|
| `*Parser` | **Not safe** — holds mutable scan state; use one per goroutine |
| `*Encoder` | **Safe** — immutable config after construction |
| `sml.Parse`, `sml.ParseStrict` | **Safe** — each allocates a fresh parser |

## Removed in v2

The following v1 APIs have been removed:

- `ParseHSMS`, `HSMSParser`, `NewHSMSParser` — replaced by `Parse` / `NewParser`
- `ParseHSMSSlow` (report-all mode) — the v2 parser is always fail-fast
- `RawSMLItem` and lazy item parsing
- Package-global `WithStrictMode` — replaced by `WithParserStrictMode` and `WithEncoderStrictMode`
- `secs2.WithASCIIStrictMode`, `secs2.ASCIIQuote`, `secs2.UseASCII*Quote` — replaced by encoder options
- `hsms.UseStreamFunction*Quote` functions — replaced by `WithSFQuote`
