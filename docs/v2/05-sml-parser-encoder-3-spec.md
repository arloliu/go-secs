# Sub-project 3 — sml parser/encoder adaptation (design / spec)

**Status:** design approved 2026-06-30, ready for implementation plan.
**Depends on:** sub-project 1 (`docs/v2/02-secs2-immutable-item-spec.md`) — the immutable `secs2.Item`,
its all-at-once constructors (`A`/`L`/`U1…`/`New*`), read API (`To*`/`iter.Seq`/`*At`), and `ToSML()`;
sub-project 2a (`docs/v2/03-hsms-immutable-message-2a-spec.md`) — `hsms.NewDataMessage` and
`*hsms.DataMessage`. Proposal decisions D1–D8 (`docs/v2/00-v2-proposal.md`); SP3 scope at proposal
line 486 ("sml parser/encoder adaptation (all-at-once construction; strict-mode via config)") and the
"configurable SML rendering is owned by the sml package" rule (`02-secs2-immutable-item-spec.md:51`,
`146-148`; proposal `220-223`).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`.

---

## 1. Goal

Bring the `sml` package (SECS Message Language — the text format for SECS-II messages) onto the v2
immutable model, and consolidate it. Concretely: parse SML text into immutable `secs2.Item` trees and
`*hsms.DataMessage`s using v2's all-at-once construction and the new `hsms.NewDataMessage`
signature/error; replace v1's package-global strict-mode (and its removed `secs2.WithASCIIStrictMode`
dependency) with **per-instance config**; and give `sml` ownership of **configurable** SML rendering via
an `Encoder`, while `secs2.Item.ToSML()` remains the zero-config canonical default. Consolidate the two
v1 parsers into one and drop v1 cruft (`RawSMLItem`, the lexer-backed slow parser).

## 2. Scope (sub-project 3)

**In scope:**
- **One** SML parser — the v1 fast direct-scan recursive-descent parser (`parser.go`), restructured
  around per-instance config; **fail-fast** (first error wins).
- A configurable **`sml.Encoder`** (item/message → SML text) owning the rendering knobs (strict-mode,
  ASCII quote style, stream/function quote style, indent). `secs2.Item.ToSML()` is unchanged and stays
  the canonical default; default-config `Encoder` output equals `ToSML()` byte-for-byte.
- Per-instance config via **functional options** for both `Parser` and `Encoder`; package-level
  convenience functions (`Parse`/`ParseStrict`, `Encode`/`EncodeStrict`) using default/strict config.
- A typed `*ParseError` carrying the input position (byte offset → line/col).
- Deleting v1 cruft: `parser_slow.go`, `lexer.go` (the slow parser + its hand-written lexer), and
  `raw_sml.go` (`RawSMLItem`).
- Updating `sml/doc.go` and `sml/README.md` to the v2 surface (README via the doc-sync skill).

**Out of scope (later / explicitly not built):**
- **No lexer/parser rewrite.** The proven fast direct-scan scanner is kept and adapted, not replaced.
- **No report-all error mode.** v1's `ParseHSMSSlow` (`([]*DataMessage, []error)`, accumulate every
  error) is dropped. It is purely additive and can return later as a `ParseAll` variant with
  message-boundary recovery if a consumer needs batch validation.
- **No `RawSMLItem` / lazy SML parsing.** Removed; re-addable as an immutable lazy `Item` if justified.
- **No change to `secs2.Item.ToSML()`** — it stays in `secs2` as the canonical default (it cannot take
  `sml` options: it is a frozen interface method and `secs2` must not import `sml` — that is a cycle).
- No connection/transport work (SP5), no `gem` (SP7).

## 3. Decisions adopted

| Topic | Decision | Source |
|-------|----------|--------|
| Ambition | **Adapt + consolidate** — compile on v2 + targeted cleanup; not a rewrite, not minimal-only. | Q (SP3 scope) |
| Parser count | **One** parser (the fast direct-scan recursive-descent one). Delete `parser_slow.go` + `lexer.go`. | Q (error model) |
| Error reporting | **Fail-fast**, first error returned. Report-all dropped (additive later). | Q (error model) |
| Lazy items | **`RawSMLItem` dropped** (`raw_sml.go` deleted). | Q (RawSMLItem) |
| Strict-mode default | **Non-strict** (preserves v1 default; fast ASCII parse, no escape/hex handling). `WithParserStrictMode(true)` opts into escape-sequence + hex-encoded-non-printable-byte handling (no printable-range *rejection* — matches v1's strict path; §5). | Q (default) |
| Construction | **All-at-once** — the recursive-descent parser buffers children locally then calls `secs2.NewListItem(children…)` / typed constructors once; no builder, no post-construction mutation (matches v2 immutable Items). | proposal 211-213; map |
| Message construction | `hsms.NewDataMessage(stream, function, replyExpected, sessionID=0, systemBytes=[4]byte{}, item)` returning `(*DataMessage, error)`; the parser **propagates** that error (folds in `item.Error()` aggregate + W-bit-on-even-function + stream≤127). Replaces the v1 `(…, 0, nil, item)` no-error call. | `hsms/data_msg.go:256-294` |
| Strict-mode plumbing | **Per-instance config**, no package globals. **Both** removed `secs2` SML globals are dropped — `WithASCIIStrictMode` (called at `sml/parser.go:85`) **and** `ASCIIQuote()` (called at `sml/parser.go:433`); strict-mode is `sml`-local state, and strict ASCII parsing reads the opening quote (`'` or `"`) from the input itself rather than from a `secs2` global. | proposal 220-223; `02-…-spec.md:146-148` |
| Config mechanism | **Functional options** (`NewParser(opts…)`, `NewEncoder(opts…)`) — idiomatic, per-instance, immutable config. | v2 ethos |
| Formatting ownership | Configurable rendering lives in **`sml.Encoder`**; `secs2.Item.ToSML()` is the no-arg canonical default. **Invariant:** `sml.NewEncoder().Encode(item) == item.ToSML()` byte-for-byte. | proposal 220-223 |
| Convenience | `sml.Parse`/`sml.ParseStrict`, `sml.Encode`/`sml.EncodeStrict` package funcs (default + strict shortcuts); other options require an explicit `Parser`/`Encoder`. | Q (encode helper) |
| API naming | Drop the `HSMS` prefix (`Parser`/`Parse`/`NewParser`, not `HSMSParser`/`ParseHSMS`); drop the `lazy bool` param (no `RawSMLItem`). | cleanup |

## 4. Package API

```go
package sml

import (
	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// ─── Parser ───────────────────────────────────────────────────────────────
type Parser struct { /* unexported: strict bool (others as needed) */ }

type ParserOption func(*Parser)
func WithParserStrictMode(strict bool) ParserOption // per-instance; replaces v1 global + secs2.WithASCIIStrictMode

func NewParser(opts ...ParserOption) *Parser

func (p *Parser) Parse(input string) ([]*hsms.DataMessage, error)      // one or more messages, fail-fast
func (p *Parser) ParseMessage(input string) (*hsms.DataMessage, error) // exactly one message
func (p *Parser) ParseHeader(input string) (*hsms.DataMessage, error)  // S/F/W only, empty body

// Package convenience (default config — no global state; constructs a default Parser internally):
func Parse(input string) ([]*hsms.DataMessage, error)        // non-strict
func ParseStrict(input string) ([]*hsms.DataMessage, error)  // strict

// ─── Encoder ──────────────────────────────────────────────────────────────
type QuoteStyle int
const (
	QuoteDouble QuoteStyle = iota // "value"  (default)
	QuoteSingle                   // 'value'
	QuoteNone                     // stream/function rendering ONLY (S1F1 with no quotes); invalid for ASCII data
)

type Encoder struct { /* unexported: strict bool, asciiQuote, sfQuote QuoteStyle, indent string */ }

type EncoderOption func(*Encoder)
func WithEncoderStrictMode(strict bool) EncoderOption // escape/hex-encode non-printable ASCII (round-trippable) vs raw
func WithASCIIQuote(q QuoteStyle) EncoderOption        // QuoteDouble | QuoteSingle only; QuoteNone is treated as the default QuoteDouble (ASCII data must be quoted)
func WithSFQuote(q QuoteStyle) EncoderOption           // QuoteDouble | QuoteSingle | QuoteNone
func WithIndent(unit string) EncoderOption             // default "  " (two spaces)

func NewEncoder(opts ...EncoderOption) *Encoder

func (e *Encoder) Encode(item secs2.Item) string
func (e *Encoder) AppendEncode(dst []byte, item secs2.Item) []byte         // alloc-controlled
func (e *Encoder) EncodeMessage(msg *hsms.DataMessage) (string, error)     // S/F/W header line + body; error if a decoded body fails to lazily decode

// Package convenience:
func Encode(item secs2.Item) string        // non-strict; == item.ToSML()
func EncodeStrict(item secs2.Item) string  // strict

// ─── Errors ───────────────────────────────────────────────────────────────
// ParseError carries the position of a syntax error in the input.
type ParseError struct {
	Offset int    // byte offset into input
	Line   int    // 1-based
	Col    int    // 1-based
	Msg    string
}
func (e *ParseError) Error() string
```

`WithParserStrictMode`/`WithEncoderStrictMode` are named distinctly (not a single `WithStrictMode`)
because the option type differs (`ParserOption` vs `EncoderOption`); strict-mode means *parse* handling
on the parser and *render* handling on the encoder.

## 5. Parser behavior

- **Grammar (unchanged from v1):** an SML message is `MsgName 'SnFm' [W] <item> .` where the item is one
  of `L`, `A`/`J`/`W` (string types), `B`, `BOOLEAN`, `I1/I2/I4/I8`, `U1/U2/U4/U8`, `F4/F8`, with
  optional `[size]`/`[min..max]` hints and nested lists. Comments and the three quoting styles per
  `sml/README.md` are accepted.
- **Size hints are non-validating (preserve v1), with one fast-path exception:** for
  list/numeric/binary/boolean items, `[size]`/`[min..max]` are parse/capacity hints — the parser does
  **not** reject a mismatch between the hint and the actual element count (v1 discards `minSize` at
  `sml/parser.go:336` and uses sizes as slice capacities). **Exception:** the non-strict ASCII fast path
  uses the declared `[size]` as a length to slice the string directly (`sml/parser.go:543-548`), so an
  `[size]` larger than the remaining input errors there — this is v1's fast-path length shortcut (a perf
  optimization of the non-strict default), preserved as-is, **not** general count-validation. Turning
  size hints into validated constraints across all types is a possible future enhancement, out of scope
  for this adapt-not-rewrite.
- **Item construction (all-at-once):** `parseItem` dispatches by type; `parseList` buffers child
  `secs2.Item`s into a local slice and calls `secs2.NewListItem(children…)` once; scalar parsers collect
  values then call the typed constructor (`secs2.NewASCIIItem`, `NewBinaryItem`, `NewBooleanItem`,
  `NewIntItem(byteSize, …)`, `NewUintItem`, `NewFloatItem`, `NewJIS8Item`, `NewUTF8StrItem`). No
  post-construction mutation. (This is already the v1 shape, so it carries over unchanged.)
- **Message construction:** `parseMsg` → `hsms.NewDataMessage(stream, function, wbit, 0, [4]byte{},
  item)`; the returned error is wrapped and returned (covers `item.Error()` aggregate, W-bit on even
  function per E37 §8.3.3.3, stream>127). **`ParseHeader`** (header-only, no body) constructs via
  `hsms.NewDataMessage(stream, function, wbit, 0, [4]byte{}, secs2.NewEmptyItem())` — v2 requires a
  **non-nil** item and `[4]byte` system bytes (v1 passed `nil` for both, `sml/parser.go:185`).
- **Strict vs non-strict ASCII parsing:** non-strict (default) is the fast path — assumes the ASCII
  string does not contain the enclosing quote char, no escape sequences. Strict handles escape
  sequences and hex-encoded non-printable bytes (e.g. `0x0A`) — matching v1's strict path exactly (no
  new printable-range *rejection* is added; this is adapt-not-rewrite). The opening quote char (`'` or
  `"`) is read from the input by the parser; the removed `secs2.ASCIIQuote()` global is no longer
  consulted. Mode is read from the `Parser`'s config (no globals).
- **Multiple messages:** `Parse` consumes successive messages until EOF; the first malformed message
  returns its `*ParseError` and stops (fail-fast).

## 6. Encoder behavior

- **Default == canonical (item level).** `sml.NewEncoder().Encode(item)` renders byte-for-byte
  identically to `secs2.Item.ToSML()` — non-strict, double-quoted ASCII, two-space indent, G9/G17 float
  precision. This is a tested invariant; the two renderers must not drift. (It is an *item*-level
  invariant; `ToSML()` has no S/F header, so it does not constrain `EncodeMessage`'s header.)
- **Default message header (a fresh SP3 choice — no `ToSML` to match).** `EncodeMessage` defaults to an
  **unquoted** S/F header (`S1F1`, `S1F1 W` with the wait bit); `WithSFQuote(QuoteDouble|QuoteSingle)`
  opts into `"S1F1"` / `'S1F1'`. Pinned by golden tests (§10).
- **Configurable knobs:** strict-mode (ASCII non-printables escaped/hex-encoded → round-trippable, vs
  raw), ASCII quote style, stream/function quote style, indent unit.
- **Tree walk uses the v2 read API:** `Type`/`Size`, `Items()` (`iter.Seq` over list children),
  `ToASCII`/`IntAt`/`Ints()`/`UintAt`/`Uints()`/`Floats()`/`Bools()`/`ToLocalizedStr`, and for binary
  `ByteAt`/`Size`/`AppendBinaryTo` (there is no `Bytes` accessor on v2 `secs2.Item`) — no mutation, no
  leaked backing slices.
- **Message rendering:** `EncodeMessage(msg) (string, error)` emits the `MsgName? SnFm [W]` header line
  per the configured S/F quote style, then the body item, then the terminating `.`. It obtains the body
  via `msg.Item()`, which for a **decoded** (raw-frame) message lazily decodes and may return an error —
  propagated, not swallowed. For a constructed message `Item()` is error-free.
- **`secs2.Item.ToSML()` is not reused for non-default config** (it is hardcoded canonical, a method on
  the item — there is no package-level `secs2.ToSML`); the encoder re-walks the tree. The
  default-equals-`ToSML` invariant is what keeps the (necessary) duplicated canonical formatting honest.
  `secs2` cannot delegate to `sml` (import cycle), so this small duplication is inherent.

## 7. Error model (D5)

- Idiomatic `(T, error)`. Syntax errors are `*ParseError` (position-carrying); the parser tracks a byte
  offset already, and line/col are derived once on failure (cheap — v1 had no position info).
- Construction/validation errors from `hsms.NewDataMessage` and `item.Error()` are wrapped (`%w`) and
  returned from `Parse`/`ParseMessage`, never swallowed or panicked.
- **Item** encoding cannot fail (`Encode`/`AppendEncode` return `string`/`[]byte`); an item that reaches
  the encoder is valid by construction. **Message** encoding **can** fail: `EncodeMessage` returns
  `(string, error)` because a decoded (raw-frame) `*hsms.DataMessage` decodes its body lazily via
  `Item() (secs2.Item, error)` (`hsms/data_msg.go:119`, lazy-decode error stored at `:313`) — that error
  is wrapped and returned.

## 8. SML ground truth (must satisfy)

- **SEMI E5 SECS-II** item types and the SML text conventions documented in `sml/README.md` (the three
  configurable dimensions: stream/function quote style, ASCII quote style, strict-vs-non-strict ASCII).
- **Round-trip (message-level):** `EncodeMessage(msg)` → `Parse` yields a value-equal
  `*hsms.DataMessage`, and for a canonical message `Parse(text)` → `EncodeMessage` reproduces the text.
  The round-trip is message-level because the parser consumes whole messages (`SnFm [W] <item> .`), not
  bare item fragments; item-level rendering is checked by the `Encode(item) == item.ToSML()` invariant.
  The **strict** encoder paired with `ParseStrict` round-trips even non-printable ASCII bytes; the
  non-strict pair does not (documented).
- The header `SnFm`/W-bit semantics match `hsms` (E37): W-bit ⇒ `replyExpected`; rejected for an even
  function under `NewDataMessage`.

## 9. Migration (v1 → v2)

| v1 | v2 |
|----|----|
| `ParseHSMS(input) ([]*DataMessage, error)`, `HSMSParser`, `NewHSMSParser` | `sml.Parse(input)`, `Parser`, `NewParser(opts…)` |
| `HSMSParser.ParseMessage(input, lazy bool)` | `Parser.ParseMessage(input)` (no `lazy` — `RawSMLItem` gone) |
| `HSMSParser.ParseMessageHeader(input)` | `Parser.ParseHeader(input)` |
| package-global `WithStrictMode(bool)` + `secs2.WithASCIIStrictMode` | per-instance `WithParserStrictMode`/`WithEncoderStrictMode` options |
| `ParseHSMSSlow(input) ([]*DataMessage, []error)` (lexer-backed, report-all) | dropped (additive `ParseAll` later if needed); `parser_slow.go` + `lexer.go` deleted |
| `RawSMLItem` / `NewRawSMLItem` (`raw_sml.go`) | dropped |
| formatting only via `secs2.Item.ToSML()` (hardcoded) | `secs2.Item.ToSML()` unchanged (canonical) **+** configurable `sml.Encoder` |
| `hsms.NewDataMessage(s, f, w, 0, nil, item)` (no error) | `hsms.NewDataMessage(s, f, w, 0, [4]byte{}, item) (*DataMessage, error)` — error propagated |

## 10. Success criteria

- **Round-trip (message-level):** for a corpus covering every item type, deep nesting, empty body, and
  all S/F/W header forms — `EncodeMessage(msg) → Parse` yields a value-equal `*hsms.DataMessage`, and
  `EncodeMessage(default)` reproduces the canonical message text. The item-level invariant
  `sml.NewEncoder().Encode(item) == item.ToSML()` holds byte-for-byte. (The parser consumes whole
  messages; bare item fragments are not separately parseable — round-trips are message-level.)
- **Strict round-trip:** a message whose body is an ASCII item containing control/non-printable bytes →
  `sml.NewEncoder(sml.WithEncoderStrictMode(true)).EncodeMessage(msg)` → **`ParseStrict`** → value-equal
  (the non-strict `Parse` does not read the escapes the strict encoder emits; the non-strict path is
  documented as not guaranteeing round-trip for non-printables).
- **Parser correctness:** golden SML → expected `(stream, function, W-bit, item tree)`; malformed inputs
  (bad `SnFm`, unterminated item/string, invalid numeric token) → `*ParseError` with a correct
  offset/line/col. A W-bit-on-even-function or aggregate `item.Error()` input → returned error (not
  panic, not silent). (`[size]`/`[min..max]` are non-validating hints — see §5 — so a count/hint
  mismatch is **not** an error, with the single exception that the non-strict ASCII fast path errors when
  a declared `[size]` exceeds the remaining input, per v1.)
- **Config / no globals:** strict/quote/indent options take effect per-instance; no package-level mutable
  state remains; distinct `Parser`/`Encoder` instances are race-clean under `-race`.
- **Builds in isolation:** `go build`/`vet`/`go test -race`/scoped `golangci-lint` green on
  `./sml/ ./secs2/ ./hsms/ ./internal/wire/`. (`hsmsss`/connection → SP5, `gem` → SP7; module-wide
  `make ci` stays red for the duration.)

## 11. Open implementation details (resolve in the plan)

- **Default float precision / formatting parity:** confirm the encoder's default exactly matches the
  current `secs2` `ToSML()` (G9 for F4, G17 for F8, 2-space indent, `<A[n] "…">` shape) so the
  byte-for-byte invariant holds; lock a golden-output test.
- **Strict-encode escaping rules:** the exact escape/hex-encoding form for non-printable ASCII under
  `WithEncoderStrictMode(true)` (must be re-parseable by the strict parser) — pin the grammar and a
  round-trip test.
- **`QuoteNone` applicability:** confirm `QuoteNone` is meaningful only for S/F header rendering (not
  ASCII data); the plan locks where each `QuoteStyle` applies.
- **File operations:** enumerate exactly which files are rewritten — `parser.go`, `doc.go` (stale v1
  examples at `sml/doc.go:13,24,26`), `parser_test.go` + `parser_bench_test.go` (hard-reference removed
  `ParseHSMS`/`NewHSMSParser`/lazy `ParseMessage`/`ParseHSMSSlow` at `sml/parser_test.go:24,34,38` and
  `sml/parser_bench_test.go:111`), and `sml/README.md` — added (`encoder.go`, `options.go`/`config.go`,
  `errors.go` + their tests) — and deleted (`parser_slow.go`, `lexer.go`, `raw_sml.go`, plus
  `parser_slow_test.go` and `raw_sml_test.go`) — mirrors the 2a/2b deletion lists.
- **`ParseError` line/col derivation:** compute lazily from the byte offset on error (scan newlines up
  to `Offset`), so the happy path pays nothing.
- **Whether `Parser`/`Encoder` need pooling:** v1 pooled lexer/token objects (deleted with the lexer).
  The fast parser allocates little; defer any pooling unless a benchmark shows a hot path (YAGNI).
