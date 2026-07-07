# Implementation plan: GEM message code-generation, Phase 1

- **Status:** Ready for implementation (design approved; see `docs/specs/2026-07-07-gem-codegen-design.md`, revision 2).
- **Date:** 2026-07-07
- **Component:** `github.com/arloliu/go-secs/v2` — new `tools/gemgen` module (own `go.mod`) + generated `gem/` output.
- **Depends on:** `docs/specs/2026-07-07-gem-codegen-design.md` (authoritative design, twice-reviewed, fact-checked against local SEMI E5).

---

## 1. Goal

Replace the hand-written `gem/s1.go` and `gem/s1_test.go` with **generated** code, produced by a
new YAML-driven generator `tools/gemgen`, invoked through `go generate`. Phase 1 delivers the
generator, the full 335-item catalog (`items.yaml`), the complete 24-function S1 message schema
(`s1.yaml`), and the generated `gem/s1.go`, `gem/items.go`, `gem/s1_test.go`. Every `structure`
node type in the DSL must be exercised by at least one generated S1 function, and `make lint` +
`make test` must pass.

This document specifies **Phase 1 only**. Authoring S2–S21 is Phase 2 (separate plan) and is out of
scope here, except that the schema/generator must already *support* the two node types S1 does not
use (bounded-arity list; and repeat groups — repeat is used, bounded-arity is not), verified at the
generator unit level.

## 2. Architecture summary

```
tools/gemgen/                      # OWN Go module (own go.mod), never imported by the root module
  main.go        # CLI: flags -items -messages -out; orchestrates load -> generate -> write
  schema.go      # Go structs mirroring the YAML DSL (Item, ItemValue, MessageFile, Message, Body, StructureNode)
  load.go        # LoadItems / LoadMessageFile + all validation rules + format-code derivation maps
  params.go      # StructureNode -> ordered []Param + body-builder expression + godoc body shorthand
  gen_items.go   # renders gem/items.go  (enum consts for items with `values:`)
  gen_messages.go# renders gem/sN.go     (one builder func per (message,body))
  gen_tests.go   # renders gem/sN_test.go(header + recursive body-tree assertion per func)
  templates/
    items.go.tmpl
    messages.go.tmpl
    tests.go.tmpl
  data/
    extract_items.py   # ONE-OFF reproducibility script (NOT part of the Go build); parses E5 Table 3
    items.yaml         # 335 items (script output + curated COMMACK `values:` overlay)
    messages/
      s1.yaml          # 24 stream-specific functions (F1-F24)

gem/
  generate.go    # //go:generate directive wiring tools/gemgen into the gem package
  items.go       # GENERATED (new)
  s1.go          # GENERATED (replaces hand-written)
  s1_test.go     # GENERATED (replaces hand-written)
  s2.go, report.go, doc.go   # untouched in Phase 1 (Phase 2)
```

`params.go` is an addition to the design's illustrative file list (the design writes
"e.g. gen_messages.go"): the structure-tree → parameter/expression logic is shared by
`gen_messages.go` and `gen_tests.go`, so it lives in one file both consume. This keeps the two
generators from drifting.

## 3. Tech stack

- Go **1.26.0** (matches root `go.mod`; `tools/gemgen/go.mod` pins the same floor).
- `gopkg.in/yaml.v3` — YAML parsing, **only** in `tools/gemgen/go.mod`. Never added to the root
  `go.mod` (it is currently an indirect test-only dep; Phase 1 must not promote it to a direct
  runtime dep of the consuming module).
- `text/template` with `//go:embed templates/*.tmpl`.
- Standard `testing` + `testify` (`require`/`assert`) for `tools/gemgen`'s own tests, matching repo
  convention. `testify` is available to the gemgen module via its own `require`.
- Python 3 for the one-off Table-3 extraction (`extract_items.py`), not part of any build target.

## 4. Global constraints (verbatim from the design spec + repo rules)

Copy these into every implementer's context. They are non-negotiable acceptance conditions.

1. **Own module.** `tools/gemgen` has its own `go.mod` (module path
   `github.com/arloliu/go-secs/v2/tools/gemgen`), per the sibling `.linter.go.mod` pattern. Its
   `gopkg.in/yaml.v3` dependency MUST NOT enter the `gem`-consuming module graph. Verify with
   `go mod graph` in the root module showing no *direct* `yaml.v3` runtime edge introduced by this
   work.
2. **Go floor 1.26.** `tools/gemgen/go.mod` says `go 1.26.0`.
3. **decorder file-layout order** (enforced by golangci `decorder`) for every generated and
   hand-written `.go` file: Package → Imports (stdlib, external, internal) → Constants (exported
   first) → Variables (exported first) → Types (exported first) + interface assertions → Factory
   functions → Exported functions → Unexported functions → Exported methods → Unexported methods.
4. **Godoc template** (`.agents/rules/400-documentation.md`): first line starts with the symbol
   name and is a one-sentence summary; do not wrap by hard character count; never put two
   independent sentences on one line; break long sentences at syntactic boundaries. Generated godoc
   for each message: name-first summary line stating the message name and direction (NO section
   citation on the first line); blank `//` line; description; blank `//` line; `Body:` shorthand
   line; blank `//` line; `Exception:` line. Every paragraph is separated by a genuine blank `//`
   line — without it, godoc renderers join adjacent comment lines into one run-on paragraph (Body
   and Exception would merge). For any message with `source: external`, append one further trailing
   paragraph (blank `//` line then the disclaimer) — the design's source disclaimer (design "Godoc
   format"): `Source: reconstructed from an external reference, not verified against the purchased
   SEMI standard.` No internal jargon in godoc (memory: `godoc-no-internal-jargon`).
5. **No internal codes in godoc.** Never emit `D5b`/`SP5`/`§4a`-style internal identifiers. Public
   SEMI/SECS/HSMS references (e.g. "SEMI E5") are allowed.
6. **Repeated groups always take `secs2.Item` elements — `repeat` only, not `packed`.** A `repeat`
   node produces a parameter of type `[]secs2.Item` (promoted to variadic `...secs2.Item` when it is
   the final parameter), regardless of whether the repeated leaf is fixed or open, or is itself a
   list. Body expression is always `secs2.L(<name>...)`. This is the uniform rule for `repeat`;
   **non-repeated fixed leaves**, and now **`packed` groups** (constraint 13), get a concrete Go
   type instead. (Rationale for `repeat`: matches existing `gem.Report(...)` /
   `gem.S2F37(..., ceids ...secs2.Item)` convention and avoids per-element-type conversion helpers
   for the *list-of-separate-items* case — this rationale does not extend to `packed`, a materially
   different wire shape; see constraint 13.)
7. **Deterministic output.** Messages emitted in ascending function order; each body's actor order
   is equipment/both first, then host. Item constants emitted in item-name order, then value order.
   Generated files carry a `// Code generated by gemgen; DO NOT EDIT.` first line.
8. **Gates:** `make lint` = 0 issues; `make test` (race, `-short`) green. After any `.go` write, run
   `go fix ./...` then `make lint` until clean (`.agents/rules/700-lint-after-write.md`).
9. **TDD.** Every layer: write the failing test, run it and confirm the exact failure, write minimal
   code, run it green, commit. No production code before a red test.
10. **Git:** never add `Co-Authored-By` or any attribution trailer. Do not commit the plan file.
11. **Deprecated E5 forms are out of scope, EXCEPT S1F10.** S1F3 has a genuine
    deprecated-packed-vs-approved-list duality — do not represent its packed-single-item legacy
    variant; target only the list form E5 recommends for new implementations. **S1F10 is different:
    corrected during Task 18 authoring** — E5 documents ONLY a packed structure for S1F10
    (`L,2{ <tsip1,,tsipn> <tsop1,,tsopn> }`), no list-form alternative at all, so it is not a
    deprecated form to avoid. S1F10 must use the new `packed` structure-node kind (§6), which emits
    `secs2.B(tsips)` directly — no `secs2.L` wrap, and NO `...` spread (`tsips` is `[]byte`
    internally, and `secs2.B` takes `...any`; spreading `[]byte` into `...any` is a Go compile
    error, caught by an actual `go build`, not the earlier gofmt/parser-only checks — see §4.13) —
    NOT the `repeat` kind, which would generate the wrong (list) wire encoding.
12. **Enum items get a named Go type + a call-site conversion.** An item with a non-empty `values:`
    table (only COMMACK in Phase 1; always `binding: fixed`) generates a named defined type
    `type <ITEM> <goType>` (e.g. `type COMMACK byte`) plus its constants, and the generated function
    parameter for that item takes the named type (`commack COMMACK`), NOT the bare primitive
    (`commack byte`). Because `secs2.B`, `secs2.I1`–`secs2.I8`, and `secs2.U1`–`secs2.U8` accept
    `...any` but their internal value combiners (`secs2/binary.go` `combineBinaryValues`,
    `secs2/int.go` `combineIntValues`/`combineIntValuesSlow`) type-switch on the *exact* primitive
    type (`case byte:`, `case uint8:`, …), a named type like `COMMACK` does NOT match `case byte:`
    even though its underlying type is `byte` — it falls through to the combiner's `default:` and the
    constructor returns a silent deferred-error item, not a working encoded value. So the generated
    body expression for an enum-typed parameter MUST convert back to the underlying primitive at the
    constructor call site: `secs2.B(byte(commack))`, NEVER `secs2.B(commack)`. The generator inserts
    this `<goType>(...)` conversion automatically; callers never write it. Items without a `values:`
    table keep the bare primitive parameter type and take no conversion, unchanged.
13. **`packed` groups take concrete-typed elements, not `secs2.Item`, and never wrap in `secs2.L`.**
    `{type: list, packed: <name>, of: {item: X}}` represents E5's packed multi-value single item
    notation (`<x1,,xn>`): all values share ONE item header, format = X's format, unlike `repeat`
    (constraint 6), which is a list of separate items. `of.item` MUST resolve to a `binding: fixed`
    item — packing needs one shared format for every value, and an `open`-binding item has no single
    format to pack under (`ValidateMessages` rejects `packed` on an open item). The generated
    parameter is typed as the item's concrete `goType` (`[]byte`/`...byte` for TSIP, following the
    same slice-vs-final-variadic position rule as `repeat`, constraint 6 — non-final `packed` group
    is `[]<goType>`, final is `...<goType>`). Body expression is `secs2.<Ctor>(<name>)` directly —
    critically, NEVER wrapped in `secs2.L(...)` (that would silently produce E5's OTHER, rejected
    structure — a list of separate items, not the packed single item the standard actually
    specifies) AND NEVER with a `...` spread on `<name>` (`<name>` is `[]<goType>` inside the
    function body regardless of slice-vs-variadic declaration, and `secs2.<Ctor>` takes `...any`;
    spreading a concrete slice type like `[]byte` into `...any` is a Go compile error — caught by an
    actual `go build`, not syntax-only checks. Passing the slice as one argument works because
    `secs2`'s combiners unpack a single slice argument themselves).
    `packed`, `repeat`, and `minItems`/`maxItems` are mutually exclusive on one `StructureNode`
    (`ValidateMessages` rejects more than one set). Phase 1's only consumer is S1F10
    (`tsips []byte, tsops ...byte`) — see constraint 11's correction.

## 5. Format-code → constructor / goType mapping (source of truth)

Used by `load.go` (Go, authoritative + tested) and mirrored by `extract_items.py`. E5 Table 1
octal format codes:

| E5 format | secs2 ctor | goType (fixed) | | E5 wildcard | expands to | binding |
|-----------|-----------|----------------|-|-------------|-----------|---------|
| `0`  | `L`       | (none, always open) | | `3()` | `I1 I2 I4 I8` | open |
| `10` | `B`       | `byte`         | | `4()` | `F4 F8`       | open |
| `11` | `BOOLEAN` | `bool`         | | `5()` | `U1 U2 U4 U8` | open |
| `20` | `A`       | `string`       | |
| `21` | `J`       | `string`       | |
| `22` | `W`       | `string`       | |
| `30` | `I8`      | `int64`        | |
| `31` | `I1`      | `int8`         | |
| `32` | `I2`      | `int16`        | |
| `34` | `I4`      | `int32`        | |
| `40` | `F8`      | `float64`      | |
| `44` | `F4`      | `float32`      | |
| `50` | `U8`      | `uint64`       | |
| `51` | `U1`      | `uint8`        | |
| `52` | `U2`      | `uint16`       | |
| `54` | `U4`      | `uint32`       | |

**Binding rule:** `fixed` iff the expanded `formats` list has exactly one entry AND that entry is
not `L`; otherwise `open`. Fixed items get `goType` from the single constructor; open items get a
`secs2.Item` parameter. Note: E5 format `10` (Binary) → `byte` is correct for every S1 item (all S1
binary items are documented 1-byte codes: COMMACK, SFCD, OFLACK, ONLACK, TSIP, TSOP). A handful of
non-S1 byte-string items (e.g. ABS, BPD) would want `[]byte`; that override is a Phase-2 concern and
out of scope here.

## 6. DSL structure-node kinds and their codegen (the complete contract)

`params.go` implements exactly these cases. `S` below = the top-level `structure` of a body.

| Node | YAML | Params contributed | Body expression | Body godoc shorthand |
|------|------|--------------------|-----------------|----------------------|
| header-only | `structure: null` | none | `secs2.NewEmptyItem()` | `Header only.` |
| fixed leaf (no `values:`) | `{item: X}`, X fixed | `x <goType>` | `secs2.<Ctor>(x)` | `<Ctor>[x]` |
| fixed enum leaf (has `values:`) | `{item: X}`, X fixed + `values:` | `x <X>` (named type) | `secs2.<Ctor>(<goType>(x))` | `<Ctor>[x]` |
| open leaf | `{item: X}`, X open | `x secs2.Item` | `x` | `<x>` |
| opaque | `{type: opaque}` | `body secs2.Item` | `body` | `form-dependent (see description)` |
| list (items) | `{type: list, items: [...]}` | children in order | `secs2.L(c1, c2, ...)` | `L[k]{ ... }` |
| bounded list | `{type: list, minItems: a, maxItems: b, items: [...]}` | children in order | `secs2.L(c1, ...)` | `L[a,b]{ ... }` |
| repeat | `{type: list, repeat: NAME, of: <node>}` | `NAME []secs2.Item` (variadic if final param) | `secs2.L(NAME...)` | `L[n]{ <NAME>... }` |
| packed | `{type: list, packed: NAME, of: {item: X}}`, X fixed | `NAME []<X.goType>` (variadic if final param) | `secs2.<X.Ctor>(NAME)` — **no `...` spread** | `<X.Ctor>[n]{ <NAME>... }` |

Rules:
- The **top-level** node's body expression is used directly as the `secs2.NewMessage` item
  argument. A bare-leaf or opaque top level is NOT wrapped in a list (e.g. S1F5 body is
  `secs2.B(sfcd)`, not `secs2.L(...)`).
- A `repeat` node is a **single param slot**: the generator does NOT recurse into `of` for
  parameter extraction (repeat elements are caller-supplied `secs2.Item`). `of` informs only the
  godoc shorthand and the schema validation of the item references it contains.
- **Variadic promotion:** exactly one parameter may be variadic — the final parameter, and only if
  it comes from a `repeat` node. All earlier repeat params are `[]secs2.Item`.
- Parameter name = lowercased item name for leaves (`MDLN`→`mdln`), the `repeat:` name for repeats,
  `body` for opaque.
- A **fixed leaf whose item carries a non-empty `values:` table** is the enum case: its parameter
  type is the item's own name used as a Go type identifier (`COMMACK`, not `byte`) and its body
  expression wraps the parameter in a conversion back to the underlying `goType` —
  `secs2.<Ctor>(<goType>(x))`, e.g. `secs2.B(byte(commack))`. This conversion is mandatory (see §4
  constraint 12): the named type does not satisfy the exact-type switch inside the `secs2`
  constructor combiners, so `secs2.B(commack)` would silently produce a deferred-error item.
  Item names in Table 3 are already valid Go identifiers, so no name sanitization is needed. A fixed
  leaf with no `values:` table is unaffected: bare `goType` parameter, `secs2.<Ctor>(x)` body.
- A **`packed` node** is E5's packed multi-value single item (`<x1,,xn>`) — ALL values share one
  item header, unlike `repeat`'s list of separate items. Like `repeat`, it is a single param slot
  (the generator does not recurse into `of` for parameter extraction) and follows the same
  slice-vs-final-variadic position rule — but the parameter type is `X`'s concrete `goType`
  (`[]byte`/`...byte`), never `secs2.Item`, and the body expression calls `X`'s own constructor
  (`secs2.B(NAME)`) directly with NO `secs2.L(...)` wrap AND NO `...` spread — `NAME` is `[]byte`
  inside the function body whether it was declared as a slice or as the final variadic parameter
  (Go represents both the same way internally), and `secs2.B` takes `...any`; spreading a `[]byte`
  into a `...any` parameter is a Go compile error (`[]byte` is not assignable to `[]any`). Passing
  the slice as a single argument works because `secs2`'s own combiners (`combineBinaryValues` et
  al.) unpack a single slice argument via a `case []byte:` branch. `of.item` must be `binding: fixed`
  (packing needs one shared format for every value); a `packed` node whose `of.item` is `open` is a
  validation error (§4 constraint 13). Phase 1's only `packed` consumer is S1F10.

## 7. Generated function signatures for S1 (the full target set)

Derived mechanically from §6 applied to §10 (s1.yaml). Listed here so implementers can eyeball the
golden output. `secs2.SECS2Message` return type omitted for brevity.

```
S1F1()                                               // header-only, W=1
S1F2(mdln string, softrev string)                    // L[2]{A A}, W=0     (actor: equipment)
S1F2Host()                                            // L[0], W=0          (actor: host)
S1F3(svids ...secs2.Item)                             // L[n]{<SVID>...}, W=1
S1F4(svs ...secs2.Item)                               // L[n]{<SV>...}, W=0
S1F5(sfcd byte)                                       // B[sfcd], W=1
S1F6(body secs2.Item)                                 // opaque, W=0
S1F7(sfcd byte)                                       // B[sfcd], W=1
S1F8(body secs2.Item)                                 // opaque, W=0
S1F9()                                               // header-only, W=1
S1F10(tsips []byte, tsops ...byte)                    // L[2]{ B[n]{<tsips>...} B[n]{<tsops>...} }, W=0 (packed, not repeat -- see §4.11)
S1F11(svids ...secs2.Item)                            // L[n]{<SVID>...}, W=1
S1F12(svids ...secs2.Item)                            // L[n]{ L[3]{<SVID> A A} ... }, W=0  (source: external)
S1F13(mdln string, softrev string)                    // L[2]{A A}, W=1     (actor: equipment)
S1F13Host()                                           // L[0], W=1          (actor: host)
S1F14(commack COMMACK, mdln string, softrev string)   // L[2]{B L[2]{A A}}, W=0 (actor: equipment)
S1F14Host(commack COMMACK)                            // L[2]{B L[0]}, W=0  (actor: host)
S1F15()                                              // header-only, W=1
S1F16(oflack byte)                                    // B[oflack], W=0
S1F17()                                              // header-only, W=1
S1F18(onlack byte)                                    // B[onlack], W=0
S1F19(objtype secs2.Item, objids []secs2.Item, attrids ...secs2.Item) // L[3]{<OBJTYPE> L{<OBJID>...} L{<ATTRID>...}}, W=1
S1F20(objects []secs2.Item, errors ...secs2.Item)     // L[2]{ L{obj...} L{err...} }, W=0
S1F21(vids ...secs2.Item)                             // L[n]{<VID>...}, W=1
S1F22(vids ...secs2.Item)                             // L[n]{ L[3]{<VID> A A} ... }, W=0
S1F23(ceids ...secs2.Item)                            // L[n]{<CEID>...}, W=1
S1F24(ceids ...secs2.Item)                            // L[n]{ L[3]{<CEID> A L{<VID>...}} ... }, W=0
```

Coverage against acceptance criteria: header-only (F1/F9/F15/F17), per-actor variant
(F2/F2Host, F13/F13Host, F14/F14Host), enum-valued item (COMMACK in F14 → named type `COMMACK` +
consts in `gem/items.go`; F14 param is `commack COMMACK`, body converts via `secs2.B(byte(commack))`),
open-binding item (SVID/SV/VID/CEID/OBJTYPE/ATTRID/…), opaque body (F6/F8), nested list (F14),
sibling repeat (F19, F20), nested repeat/repeat-of-list (F12, F20, F22, F24), **packed multi-value
single item (F10 — `tsips`/`tsops`, a new node kind added during Task 18 authoring; NOT a repeat,
see §4.11)**. Bounded-arity list is NOT in S1 — covered by a generator unit test only (Task 15).

> **S1F12 provenance exception — carry this note into the code.** Our local E5 markdown's §10
> Message Detail section skips from F11 to F13: a page was dropped during PDF→markdown conversion,
> not the standard omitting F12. **S1F12 (Status Variable Namelist Reply, SVN)** genuinely exists —
> Table 3 in the same local copy cross-references it for both `SVNAME` and `UNITS`
> (`e005-00-0813.md:862,927`; `SVID` "Where Used: S1F3, F11, F12"), and it follows the exact
> request/reply-namelist pattern as S1F21/22 and S1F23/24. Its structure (`L,n{ L,3{ SVID SVNAME
> UNITS } }`, equipment-only, no reply) is reconstructed from an external reference and entered in
> `s1.yaml` with `source: external` plus an inline comment noting *why* — this is a gap in our local
> copy, not a genuine discrepancy with the standard, and should be re-verified against a complete E5
> copy if one becomes available. The authoritative design's Phase-1 scope is **24** functions = F1-F24 (design
> `docs/specs/2026-07-07-gem-codegen-design.md`, "S1F12 provenance exception").

---

## 8. Task list

Tasks are ordered so the pipeline is proven end-to-end on the simplest case (S1F1, header-only)
before scaling. Each task lists **Consumes / Produces** (exact Go identifiers) and bite-sized steps.
"RED" = run the new test and confirm it fails for the stated reason; "GREEN" = run and confirm pass.

**Model / Effort per task.** Each task header states a suggested model tier and reasoning effort
for whoever executes it (subagent-driven-development dispatch, or a Workflow `agent()` call).
Rubric: **opus / high** reserved for tasks that are architecturally load-bearing (the structure-tree
walker in Task 6, the hardest structural shape in Task 13) or where a domain-transcription error is
costly and easy to miss (bulk E5 extraction in Task 16; hand-authoring all 24 message definitions in
Task 18 — this is exactly where the S1F12 gap surfaced during design review, so this task gets extra
scrutiny). **opus / medium** for judgment-heavy but narrow-scope review (Task 23's godoc read-through).
**sonnet / high** for correctness-sensitive validation or multi-case logic with a fully specified
contract (Tasks 4, 5, 11, 14). **sonnet / medium** for standard implementation work against a clear
spec. **sonnet / low** for mechanical scaffolding, wiring, or gate-running with no design judgment.
Nothing in this plan needs haiku — every task involves either non-trivial Go logic or
correctness-critical content transcription. If dispatching via the `Agent` tool, pass `model: "opus"`
explicitly for the five opus tasks — effort itself isn't a parameter on that tool, so treat "effort"
as a signal for how much the executor should slow down and double-check, not a literal flag. If
dispatching via `Workflow`'s `agent()`, both `model` and `effort` are real per-call parameters.

### Milestone A — module bootstrap

#### Task 1 — Create the `tools/gemgen` module

**Model/Effort:** sonnet / low — pure scaffolding, no design judgment.

**Consumes:** nothing. **Produces:** `tools/gemgen/go.mod`, a buildable empty `main.go`.

Steps:
1. Write `tools/gemgen/go.mod`:
   ```
   module github.com/arloliu/go-secs/v2/tools/gemgen

   go 1.26.0

   require (
       github.com/stretchr/testify v1.9.0
       gopkg.in/yaml.v3 v3.0.1
   )
   ```
2. Write a minimal `tools/gemgen/main.go`:
   ```go
   // Command gemgen generates gem/ SECS-II message builders from a YAML DSL.
   package main

   func main() {}
   ```
3. Run `cd tools/gemgen && go mod tidy`. Expected: resolves `testify` + `yaml.v3` + indirects,
   writes `go.sum`.
4. Run `cd tools/gemgen && go build ./...`. Expected: no output, exit 0.
5. Run `go mod graph | grep yaml` from the **root** module dir. Expected: `yaml.v3` still appears
   only as it did before (indirect, test path) — no new direct root-module edge.
6. Commit: `chore(gemgen): scaffold tools/gemgen module`.

### Milestone B — schema + loading (TDD)

#### Task 2 — DSL structs in `schema.go`

**Model/Effort:** sonnet / medium — struct definitions against an already-fully-specified schema.

**Consumes:** Task 1 module. **Produces:** types `Item`, `ItemValue`, `MessageFile`, `Message`,
`Body`, `StructureNode`; method `(*StructureNode).Kind() string`.

Steps:
1. Write failing test `tools/gemgen/schema_test.go`:
   ```go
   package main

   import (
       "testing"

       "github.com/stretchr/testify/require"
       "gopkg.in/yaml.v3"
   )

   func TestStructureNodeKind(t *testing.T) {
       cases := []struct {
           name string
           yml  string
           want string
       }{
           {"leaf", "{item: MDLN}", "leaf"},
           {"opaque", "{type: opaque}", "opaque"},
           {"list", "{type: list, items: []}", "list"},
           {"repeat", "{type: list, repeat: svids, of: {item: SVID}}", "list"},
       }
       for _, tc := range cases {
           t.Run(tc.name, func(t *testing.T) {
               var n StructureNode
               require.NoError(t, yaml.Unmarshal([]byte(tc.yml), &n))
               require.Equal(t, tc.want, n.Kind())
           })
       }
   }
   ```
2. RED: `go test ./...` → fails to compile (`StructureNode` undefined).
3. Write `tools/gemgen/schema.go` (respect decorder: Types section only):
   ```go
   package main

   // Binding controls the generated parameter type for an item.
   type Binding string

   // Binding values.
   const (
       BindingFixed Binding = "fixed"
       BindingOpen  Binding = "open"
   )

   // Item is one entry of the Data Item Dictionary (E5 Table 3).
   type Item struct {
       Formats     []string    `yaml:"formats"`
       Binding     Binding     `yaml:"binding"`
       GoType      string      `yaml:"goType"`
       Description string      `yaml:"description"`
       Source      string      `yaml:"source"`
       Values      []ItemValue `yaml:"values"`
   }

   // ItemValue is one named enumerated value for an item with a `values:` table.
   type ItemValue struct {
       Name  string `yaml:"name"`
       Value int64  `yaml:"value"`
   }

   // MessageFile is one stream's YAML file (e.g. messages/s1.yaml).
   type MessageFile struct {
       Stream   int       `yaml:"stream"`
       Messages []Message `yaml:"messages"`
   }

   // Message is one stream/function definition.
   type Message struct {
       Function    int    `yaml:"function"`
       Name        string `yaml:"name"`
       Mnemonic    string `yaml:"mnemonic"`
       Direction   string `yaml:"direction"`
       Description string `yaml:"description"`
       Exception   string `yaml:"exception"`
       Source      string `yaml:"source"`
       Confidence  string `yaml:"confidence"`
       Bodies      []Body `yaml:"bodies"`
   }

   // Body is one wire shape of a message (one per distinct actor variant).
   type Body struct {
       Actor         string         `yaml:"actor"`
       ReplyExpected bool           `yaml:"replyExpected"`
       Structure     *StructureNode `yaml:"structure"`
   }

   // StructureNode is one node of a message body tree.
   //
   // A leaf sets Item. A list sets Type "list" with either Items, or Repeat+Of.
   // An opaque body sets Type "opaque". A nil *StructureNode is a header-only body.
   type StructureNode struct {
       Type     string          `yaml:"type"`
       Item     string          `yaml:"item"`
       Items    []StructureNode `yaml:"items"`
       Repeat   string          `yaml:"repeat"`
       Packed   string          `yaml:"packed"` // added post-Task-6-review: E5's packed multi-value
                                                  // single item (S1F10) -- see §4 constraint 13
       Of       *StructureNode  `yaml:"of"`
       MinItems *int            `yaml:"minItems"`
       MaxItems *int            `yaml:"maxItems"`
   }

   // Kind returns "leaf", "opaque", or "list" for the node.
   func (n *StructureNode) Kind() string {
       switch {
       case n.Item != "":
           return "leaf"
       case n.Type == "opaque":
           return "opaque"
       default:
           return "list"
       }
   }
   ```
4. GREEN: `go test ./...` passes.
5. Commit: `feat(gemgen): DSL schema structs`.

> **No new field for the named-type signal.** The existing `Values []ItemValue` is the sole signal
> for "this item generates a named Go type" (§4 constraint 12): a fixed-binding item with
> `len(Values) > 0` gets a named type + typed parameter + call-site conversion. Do NOT add a
> redundant boolean — Tasks 4, 6, and 11 all derive the enum case from `len(it.Values) > 0` (and
> `it.Binding == BindingFixed`) directly.

#### Task 3 — Format-code derivation maps in `load.go`

**Model/Effort:** sonnet / medium — lookup-table logic, but feeds correctness for all 335 items.

**Consumes:** Task 2. **Produces:** `deriveBinding([]string) Binding`, `deriveGoType([]string)
(string, bool)`, `singleFormatGoType map[string]string`.

Steps:
1. Write failing `tools/gemgen/load_derive_test.go`:
   ```go
   package main

   import (
       "testing"

       "github.com/stretchr/testify/require"
   )

   func TestDeriveBindingAndGoType(t *testing.T) {
       cases := []struct {
           name     string
           formats  []string
           binding  Binding
           goType   string
           hasGoTyp bool
       }{
           {"fixed ascii", []string{"A"}, BindingFixed, "string", true},
           {"fixed binary", []string{"B"}, BindingFixed, "byte", true},
           {"fixed bool", []string{"BOOLEAN"}, BindingFixed, "bool", true},
           {"list always open", []string{"L"}, BindingOpen, "", false},
           {"multi open", []string{"A", "U1", "U2", "U4", "U8"}, BindingOpen, "", false},
           {"signed wildcard open", []string{"I1", "I2", "I4", "I8"}, BindingOpen, "", false},
       }
       for _, tc := range cases {
           t.Run(tc.name, func(t *testing.T) {
               require.Equal(t, tc.binding, deriveBinding(tc.formats))
               gt, ok := deriveGoType(tc.formats)
               require.Equal(t, tc.hasGoTyp, ok)
               require.Equal(t, tc.goType, gt)
           })
       }
   }
   ```
2. RED: compile failure (undefined `deriveBinding`).
3. Write `tools/gemgen/load.go` Variables + unexported funcs sections:
   ```go
   package main

   // singleFormatGoType maps a single secs2 constructor to its concrete Go type
   // for fixed-binding items. `L` is intentionally absent (lists are always open).
   var singleFormatGoType = map[string]string{
       "A": "string", "J": "string", "W": "string",
       "B": "byte", "BOOLEAN": "bool",
       "I1": "int8", "I2": "int16", "I4": "int32", "I8": "int64",
       "U1": "uint8", "U2": "uint16", "U4": "uint32", "U8": "uint64",
       "F4": "float32", "F8": "float64",
   }

   func deriveBinding(formats []string) Binding {
       if len(formats) == 1 {
           if _, ok := singleFormatGoType[formats[0]]; ok {
               return BindingFixed
           }
       }
       return BindingOpen
   }

   func deriveGoType(formats []string) (string, bool) {
       if deriveBinding(formats) != BindingFixed {
           return "", false
       }
       return singleFormatGoType[formats[0]], true
   }
   ```
4. GREEN. Run `go fix ./...` then `make lint` (from repo root — note: `make lint` lints all
   modules? No: golangci runs on the root module. See Task 22 for the gemgen lint decision.) For
   now, `cd tools/gemgen && go vet ./...` clean.
5. Commit: `feat(gemgen): format-code derivation`.

#### Task 4 — `LoadItems` + `ValidateItems`

**Model/Effort:** sonnet / high — multi-rule validation logic; this is the gate that catches bad DSL content before it reaches codegen.

**Consumes:** Task 3. **Produces:** `LoadItems([]byte) (map[string]Item, error)`,
`ValidateItems(map[string]Item) error`.

Validation rules (design "Validation" list, item portion):
- `binding: fixed` with `len(formats) != 1` → error.
- `binding: fixed` with missing `goType` → error.
- stored `binding` must equal `deriveBinding(formats)` (cross-checks the Python extraction).
- if fixed, stored `goType` must equal the derived goType.
- non-empty `values:` on a non-`fixed` (i.e. `open`) item → error. The `values:` table generates a
  named defined type over the item's `goType` (§4 constraint 12); an `open` item has no concrete
  `goType` to define the type over, so an enum table on it is nonsensical. Failing here (the earliest
  gate) rather than only in `renderItems` (Task 11) surfaces the mistake at validation time. (The
  design states `values:` is "only valid on `binding: fixed` items".)

Steps:
1. Failing `tools/gemgen/load_items_test.go`:
   ```go
   package main

   import (
       "testing"

       "github.com/stretchr/testify/require"
   )

   const goodItems = `
   MDLN:
     formats: [A]
     binding: fixed
     goType: string
     description: Equipment model type.
     source: e5
   CEID:
     formats: [A, U1, U2, U4, U8, I1, I2, I4, I8]
     binding: open
     description: Collected event ID.
     source: e5
   `

   func TestLoadItemsValid(t *testing.T) {
       items, err := LoadItems([]byte(goodItems))
       require.NoError(t, err)
       require.NoError(t, ValidateItems(items))
       require.Equal(t, BindingFixed, items["MDLN"].Binding)
       require.Equal(t, "string", items["MDLN"].GoType)
       require.Equal(t, BindingOpen, items["CEID"].Binding)
   }

   func TestValidateItemsRejectsBadFixed(t *testing.T) {
       cases := map[string]string{
           "fixed multi format": "X:\n  formats: [A, B]\n  binding: fixed\n  goType: string\n  source: e5\n",
           "fixed no goType":     "X:\n  formats: [A]\n  binding: fixed\n  source: e5\n",
           "binding mismatch":    "X:\n  formats: [A]\n  binding: open\n  source: e5\n",
           "goType mismatch":     "X:\n  formats: [A]\n  binding: fixed\n  goType: int8\n  source: e5\n",
           "values on open item": "X:\n  formats: [A, B]\n  binding: open\n  source: e5\n  values:\n    - {name: Foo, value: 0}\n",
       }
       for name, yml := range cases {
           t.Run(name, func(t *testing.T) {
               items, err := LoadItems([]byte(yml))
               require.NoError(t, err)
               require.Error(t, ValidateItems(items))
           })
       }
   }
   ```
2. RED: undefined `LoadItems`/`ValidateItems`.
3. Add to `load.go` (Factory funcs + Exported funcs sections):
   ```go
   func LoadItems(data []byte) (map[string]Item, error) {
       items := map[string]Item{}
       if err := yaml.Unmarshal(data, &items); err != nil {
           return nil, fmt.Errorf("parse items: %w", err)
       }
       return items, nil
   }

   func ValidateItems(items map[string]Item) error {
       for name, it := range items {
           want := deriveBinding(it.Formats)
           if it.Binding != want {
               return fmt.Errorf("item %s: binding %q, want %q for formats %v", name, it.Binding, want, it.Formats)
           }
           if it.Binding == BindingFixed {
               if len(it.Formats) != 1 {
                   return fmt.Errorf("item %s: fixed binding needs exactly one format, got %v", name, it.Formats)
               }
               gt, _ := deriveGoType(it.Formats)
               if it.GoType == "" {
                   return fmt.Errorf("item %s: fixed binding needs goType", name)
               }
               if it.GoType != gt {
                   return fmt.Errorf("item %s: goType %q, want %q", name, it.GoType, gt)
               }
           }
           if len(it.Values) > 0 && it.Binding != BindingFixed {
               return fmt.Errorf("item %s: values table requires binding fixed (a named enum type needs a concrete goType)", name)
           }
       }
       return nil
   }
   ```
   Add `import ("fmt"; "gopkg.in/yaml.v3")` to `load.go`.
4. GREEN.
5. Commit: `feat(gemgen): load + validate items`.

#### Task 5 — `LoadMessageFile` + `ValidateMessages`

**Model/Effort:** sonnet / high — recursive item-ref validation through the structure tree; same correctness stakes as Task 4.

**Consumes:** Task 4. **Produces:** `LoadMessageFile([]byte) (MessageFile, error)`,
`ValidateMessages([]MessageFile, map[string]Item) error`.

Validation rules (design "Validation" list, message portion):
- any `{item: X}` reference (any node depth, any binding) with `X` not in `items` → error.
- duplicate `(stream, function)` across all loaded files → error.
- a body `actor` not in {both, equipment, host} → error.
- a list node with both `repeat` and (`minItems` or `maxItems`) set → error.

Steps:
1. Failing `tools/gemgen/load_messages_test.go` with sub-cases for each rule (one valid file; four
   invalid files). Example valid fragment:
   ```go
   const goodMsgs = `
   stream: 1
   messages:
     - function: 2
       name: On Line Data
       mnemonic: D
       direction: bidirectional
       description: Data signifying that the equipment is alive.
       exception: The host sends a zero-length list to the equipment.
       source: e5
       bodies:
         - actor: equipment
           replyExpected: false
           structure: {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
         - actor: host
           replyExpected: false
           structure: {type: list, items: []}
   `
   ```
   Assert `ValidateMessages` returns nil against an `items` map containing `MDLN`, `SOFTREV`; then
   assert errors for: unknown item ref (`{item: NOPE}`), duplicate `(1,2)` across two files, bad
   actor `sideways`, and `{type: list, repeat: x, minItems: 0, of: {item: MDLN}}`.
2. RED.
3. Implement in `load.go`. `ValidateMessages` iterates files → messages → bodies; walks each
   `Structure` recursively (`walkNode`), collecting item refs and enforcing the repeat/minItems rule
   and actor set; tracks a `seen[[2]int]bool` for duplicate `(stream, function)`. Recursion covers
   `Items[]` and `Of`. Full walk:
   ```go
   func ValidateMessages(files []MessageFile, items map[string]Item) error {
       seen := map[[2]int]bool{}
       for _, f := range files {
           for _, m := range f.Messages {
               key := [2]int{f.Stream, m.Function}
               if seen[key] {
                   return fmt.Errorf("duplicate S%dF%d", f.Stream, m.Function)
               }
               seen[key] = true
               for _, b := range m.Bodies {
                   switch b.Actor {
                   case "both", "equipment", "host":
                   default:
                       return fmt.Errorf("S%dF%d: bad actor %q", f.Stream, m.Function, b.Actor)
                   }
                   if err := walkNode(b.Structure, items, f.Stream, m.Function); err != nil {
                       return err
                   }
               }
           }
       }
       return nil
   }

   func walkNode(n *StructureNode, items map[string]Item, s, fn int) error {
       if n == nil {
           return nil
       }
       switch n.Kind() {
       case "leaf":
           if _, ok := items[n.Item]; !ok {
               return fmt.Errorf("S%dF%d: unknown item %q", s, fn, n.Item)
           }
       case "list":
           exclusive := 0
           if n.Repeat != "" {
               exclusive++
           }
           if n.Packed != "" {
               exclusive++
           }
           if n.MinItems != nil || n.MaxItems != nil {
               exclusive++
           }
           if exclusive > 1 {
               return fmt.Errorf("S%dF%d: repeat, packed, and minItems/maxItems are mutually exclusive", s, fn)
           }
           if n.Packed != "" {
               if n.Of == nil || n.Of.Item == "" {
                   return fmt.Errorf("S%dF%d: packed %q needs an of.item leaf", s, fn, n.Packed)
               }
               it, ok := items[n.Of.Item]
               if !ok {
                   return fmt.Errorf("S%dF%d: unknown item %q", s, fn, n.Of.Item)
               }
               if it.Binding != BindingFixed {
                   return fmt.Errorf("S%dF%d: packed %q's item %q must be binding: fixed (packing needs one shared format)", s, fn, n.Packed, n.Of.Item)
               }
           }
           for i := range n.Items {
               if err := walkNode(&n.Items[i], items, s, fn); err != nil {
                   return err
               }
           }
           if err := walkNode(n.Of, items, s, fn); err != nil {
               return err
           }
       }
       return nil
   }

   func LoadMessageFile(data []byte) (MessageFile, error) {
       var mf MessageFile
       if err := yaml.Unmarshal(data, &mf); err != nil {
           return mf, fmt.Errorf("parse messages: %w", err)
       }
       return mf, nil
   }
   ```
4. GREEN.
5. Commit: `feat(gemgen): load + validate messages`.

### Milestone C — parameter/expression model (TDD)

#### Task 6 — `params.go`: structure → params + body expression + godoc shorthand

**Model/Effort:** opus / high — this is the architectural linchpin: the recursive tree-walker that must correctly handle all 7 node kinds, sibling/nested repeats, variadic promotion, and opaque bodies. Both external design reviews' P0 findings centered on exactly this logic.

**Consumes:** Task 5 (`StructureNode`, `Item`). **Produces:** type `Param{Name, Type string; Repeat,
Variadic bool}`; `BuildParams(*StructureNode, map[string]Item) []Param`; `BodyExpr(*StructureNode,
map[string]Item) string`; `BodyDoc(*StructureNode, map[string]Item) string`.

Steps:
1. Failing `tools/gemgen/params_test.go` — one table covering **every** §6 row. Key expectations
   (items map supplies MDLN=fixed string, COMMACK=fixed byte **with a non-empty `values:` table**
   (so it generates the named type `COMMACK`), SFCD=fixed byte (no `values:`), SVID/SV/OBJTYPE/
   OBJID/ATTRID/ATTRDATA/ERRCODE/ERRTEXT/VID/CEID/DVVALNAME/UNITS/CENAME/TSIP/TSOP as needed):
   Param literal shorthand below is `{name, type, repeat, variadic}` (matching the four-field
   `Param` struct in step 3): `repeat` is true for any param originating from a `repeat` node,
   `variadic` only for the final one when it is itself a repeat.
   ```go
   // header-only
   BuildParams(nil, items)                          -> [] (empty)
   BodyExpr(nil, items)                             -> "secs2.NewEmptyItem()"
   BodyDoc(nil, items)                              -> "Header only."
   // fixed list (S1F2)
   node = {list, items:[{item:MDLN},{item:SOFTREV}]}
   BuildParams(node)  -> [{mdln,string,false,false},{softrev,string,false,false}]
   BodyExpr(node)     -> "secs2.L(secs2.A(mdln), secs2.A(softrev))"
   // nested list + enum leaf (S1F14): COMMACK has a values: table -> named type + byte() conversion
   node = {list, items:[{item:COMMACK},{list, items:[{item:MDLN},{item:SOFTREV}]}]}
   BuildParams -> [{commack,COMMACK,false,false},{mdln,string,false,false},{softrev,string,false,false}]
   BodyExpr    -> "secs2.L(secs2.B(byte(commack)), secs2.L(secs2.A(mdln), secs2.A(softrev)))"
   // bare fixed leaf (S1F5)
   BuildParams({item:SFCD}) -> [{sfcd,byte,false,false}]
   BodyExpr({item:SFCD})    -> "secs2.B(sfcd)"
   // opaque (S1F6)
   BuildParams({opaque})    -> [{body,secs2.Item,false,false}]
   BodyExpr({opaque})       -> "body"
   // open repeat, final -> variadic (S1F3)
   node = {list, repeat:svids, of:{item:SVID}}
   BuildParams -> [{svids,secs2.Item,true,true}]
   BodyExpr    -> "secs2.L(svids...)"
   // sibling packed groups (S1F10): non-final packed is a []goType slice, final is variadic ...goType
   // -- NOT secs2.Item, and NOT wrapped in secs2.L (that's the repeat case, a different node kind
   // representing E5's OTHER structure -- a list of separate items, not what S1F10 actually is).
   // NO ... spread either: tsips/tsops are []byte inside the function body regardless of
   // slice-vs-variadic declaration, and secs2.B takes ...any -- spreading []byte into ...any is a
   // Go compile error. secs2.B(tsips) (one slice argument, no spread) is what actually compiles.
   node = {list, items:[{list,packed:tsips,of:{item:TSIP}},{list,packed:tsops,of:{item:TSOP}}]}
   BuildParams -> [{tsips,byte,true,false},{tsops,byte,true,true}]
   BodyExpr    -> "secs2.L(secs2.B(tsips), secs2.B(tsops))"
   // fixed leaf + sibling repeats (S1F19): leaf, non-final repeat slice, final repeat variadic
   node = {list, items:[{item:OBJTYPE},{list,repeat:objids,of:{item:OBJID}},{list,repeat:attrids,of:{item:ATTRID}}]}
   BuildParams -> [{objtype,secs2.Item,false,false},{objids,secs2.Item,true,false},{attrids,secs2.Item,true,true}]
   BodyExpr    -> "secs2.L(objtype, secs2.L(objids...), secs2.L(attrids...))"
   // sibling repeats of lists (S1F20): non-final repeat slice, final repeat variadic
   node = {list, items:[{list,repeat:objects,of:{...}},{list,repeat:errors,of:{...}}]}
   BuildParams -> [{objects,secs2.Item,true,false},{errors,secs2.Item,true,true}]
   BodyExpr    -> "secs2.L(secs2.L(objects...), secs2.L(errors...))"
   // bounded-arity list (synthetic; no S1 msg)
   node = {list, minItems:0, maxItems:4, items:[{item:MDLN}]}
   BodyDoc     -> "L[0,4]{ A[mdln] }"
   ```
2. RED.
3. Implement `params.go`. Repeat origin is a first-class field of `Param` (`Repeat`), set the moment
   a repeat node contributes its param — it does NOT depend on position, so a non-final repeat
   keeps `Repeat: true` even though it is not variadic. The variadic promotion is a separate
   post-pass: after collecting params, if the last param has `Repeat: true`, also set
   `Variadic = true`. No scratch/origin slice is needed because `Repeat` already lives on `Param`.
   ```go
   package main

   import (
       "fmt"
       "strings"
   )

   // Param is one generated function parameter.
   //
   // Repeat is true when the parameter originates from a repeat node. Such a
   // parameter renders as name []secs2.Item, or name ...secs2.Item when it is
   // also the final parameter (Variadic). A non-repeat leaf renders as name <Type>.
   // Repeat is independent of position: a non-final repeat is still Repeat: true
   // (a slice) without being Variadic.
   type Param struct {
       Name     string
       Type     string
       Repeat   bool
       Variadic bool
   }

   func BuildParams(n *StructureNode, items map[string]Item) []Param {
       var out []Param
       var walk func(*StructureNode)
       walk = func(n *StructureNode) {
           if n == nil {
               return
           }
           switch n.Kind() {
           case "leaf":
               it := items[n.Item]
               typ := "secs2.Item"
               if it.Binding == BindingFixed {
                   typ = it.GoType
                   if len(it.Values) > 0 {
                       typ = n.Item // enum item: named defined type, e.g. COMMACK (over goType byte)
                   }
               }
               out = append(out, Param{Name: lower(n.Item), Type: typ})
           case "opaque":
               out = append(out, Param{Name: "body", Type: "secs2.Item"})
           case "list":
               if n.Repeat != "" {
                   out = append(out, Param{Name: n.Repeat, Type: "secs2.Item", Repeat: true})
                   return // do NOT recurse into Of
               }
               if n.Packed != "" {
                   // Packed group: concrete goType, never secs2.Item -- the caller supplies raw
                   // primitive values, not pre-built Items, since packing into one item header is
                   // a generator-owned concern (§4 constraint 13).
                   it := items[n.Of.Item]
                   out = append(out, Param{Name: n.Packed, Type: it.GoType, Repeat: true})
                   return // do NOT recurse into Of
               }
               for i := range n.Items {
                   walk(&n.Items[i])
               }
           }
       }
       walk(n)
       if len(out) > 0 && out[len(out)-1].Repeat {
           out[len(out)-1].Variadic = true
       }
       return out
   }

   func BodyExpr(n *StructureNode, items map[string]Item) string {
       if n == nil {
           return "secs2.NewEmptyItem()"
       }
       switch n.Kind() {
       case "leaf":
           it := items[n.Item]
           if it.Binding == BindingFixed {
               arg := lower(n.Item)
               if len(it.Values) > 0 {
                   // Enum item: the parameter is the named type (e.g. COMMACK), but the secs2
                   // constructor combiners type-switch on the exact primitive (case byte:), which a
                   // named type does not satisfy. Convert back to goType at the call site so the
                   // value encodes instead of becoming a deferred-error item (§4 constraint 12).
                   arg = fmt.Sprintf("%s(%s)", it.GoType, arg) // e.g. byte(commack)
               }
               return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
           }
           return lower(n.Item)
       case "opaque":
           return "body"
       default: // list
           if n.Repeat != "" {
               return fmt.Sprintf("secs2.L(%s...)", n.Repeat)
           }
           if n.Packed != "" {
               // Packed group: call the underlying item's own constructor directly -- NEVER
               // secs2.L(...), which would produce the wrong (list-of-separate-items) wire shape.
               it := items[n.Of.Item]
               return fmt.Sprintf("secs2.%s(%s...)", it.Formats[0], n.Packed)
           }
           parts := make([]string, len(n.Items))
           for i := range n.Items {
               parts[i] = BodyExpr(&n.Items[i], items)
           }
           return "secs2.L(" + strings.Join(parts, ", ") + ")"
       }
   }

   func lower(name string) string { return strings.ToLower(name) }
   ```
   `BodyDoc` mirrors §6's shorthand column (list `L[k]{ ... }`; bounded `L[a,b]{ ... }`; repeat
   `L[n]{ <NAME>... }`; **packed `<Ctor>[n]{ <NAME>... }`** — e.g. `B[n]{ <tsips>... }`, using the
   packed item's own constructor letter instead of `L`, so a reader can tell packed and repeat
   apart at a glance; fixed leaf `<Ctor>[name]`; open leaf `<name>`; opaque
   `form-dependent (see description)`; nil `Header only.`). Implement recursively, same switch.
4. GREEN.
5. Commit: `feat(gemgen): structure -> params/expr/doc`.

### Milestone D — one template end-to-end (prove the pipeline on S1F1)

#### Task 7 — `gen_messages.go` + `templates/messages.go.tmpl`, header-only only

**Model/Effort:** sonnet / medium — first template proving the pipeline, but scoped to a single case.

**Consumes:** Task 6. **Produces:** `renderMessages(MessageFile, map[string]Item) ([]byte, error)`;
embedded `messages.go.tmpl`; a per-function view model `funcView`.

Scope this task to a single header-only function so the template + view model are proven before
adding cases. Build the `funcView` for a body:
```go
type funcView struct {
    Name    string   // "S1F1" or "S1F2Host"
    Params  []Param
    Body    string   // BodyExpr output
    Stream  int
    Func    int
    WaitBit bool
    Doc     []string // godoc lines (without leading "// ")
}
```
Function name rule: `fmt.Sprintf("S%dF%d", stream, function)` for actor `both`/`equipment`; append
`Host` for actor `host`.

Steps:
1. Failing `tools/gemgen/gen_messages_test.go`:
   ```go
   func TestRenderMessagesHeaderOnly(t *testing.T) {
       items := map[string]Item{}
       mf := MessageFile{Stream: 1, Messages: []Message{{
           Function: 1, Name: "Are You There Request", Mnemonic: "R",
           Direction: "bidirectional", Description: "Establishes if the equipment is on-line.",
           Exception: "None", Source: "e5",
           Bodies: []Body{{Actor: "both", ReplyExpected: true, Structure: nil}},
       }}}
       out, err := renderMessages(mf, items)
       require.NoError(t, err)
       src := string(out)
       require.Contains(t, src, "// Code generated by gemgen; DO NOT EDIT.")
       require.Contains(t, src, "package gem")
       require.Contains(t, src, "func S1F1() secs2.SECS2Message {")
       require.Contains(t, src, "return secs2.NewMessage(1, 1, true, secs2.NewEmptyItem())")
   }
   ```
2. RED.
3. Write `templates/messages.go.tmpl`:
   ```
   // Code generated by gemgen; DO NOT EDIT.

   package gem

   import "github.com/arloliu/go-secs/v2/secs2"
   {{range .Funcs}}
   {{range .Doc}}// {{.}}
   {{end}}func {{.Name}}({{params .Params}}) secs2.SECS2Message {
       return secs2.NewMessage({{.Stream}}, {{.Func}}, {{.WaitBit}}, {{.Body}})
   }
   {{end}}
   ```
   `params` is a template FuncMap helper rendering each parameter joined by `, `, with a three-way
   branch per param, using the param's own `Type` field throughout (NOT a hardcoded `secs2.Item` —
   `Type` is `secs2.Item` for a `repeat`-origin param, but a concrete goType like `byte` for a
   `packed`-origin param; the branch structure is identical, only the wrapped type differs):
   - `Variadic` (final repeat/packed) → `name ...<Type>`;
   - else `Repeat` (non-final repeat/packed) → `name []<Type>`;
   - else (fixed/open leaf or opaque) → `name <Type>`.

   The `Repeat`-but-not-`Variadic` branch is what makes a non-final repeat such as S1F19's `objids`
   or S1F20's `objects` render as `[]secs2.Item`, and a non-final **packed** group such as S1F10's
   `tsips` render as `[]byte` (not `[]secs2.Item` — packed groups carry a concrete goType `Type`).
4. Write `gen_messages.go`: `//go:embed templates/messages.go.tmpl`, parse with the FuncMap, build
   `[]funcView` from the file, execute into a buffer, then `format.Source` the result before
   returning (gofmt normalization so golden text is stable). Godoc lines from a `messageDoc(m,
   body)` helper producing the §4 shape as a `[]string` of comment lines (without the leading
   `// `): name-first summary line (message name + direction, NO section citation), an empty-string
   entry, description, an empty-string entry, `Body:` line, an empty-string entry, `Exception:`
   line. Each empty-string entry becomes a blank `//` line when the template renders it (see the
   `{{range .Doc}}// {{.}}` loop), which is what makes godoc render description, Body, and Exception
   as three distinct paragraphs rather than one merged run-on — do NOT concatenate the sections with
   a single `\n`; a genuine blank comment line must sit between them. `messageDoc` also inspects
   `m.Source`: when it equals `"external"`, it appends an empty-string entry then one trailing
   disclaimer line —
   `Source: reconstructed from an external reference, not verified against the purchased SEMI standard.` — so the
   provenance travels into the generated godoc (design "Godoc format"; the S1F12 exception is the
   only Phase 1 message that triggers it). Add a `messageDoc` unit test here that feeds a synthetic
   `source: external` message and asserts the disclaimer line is present (and that a blank `//` line
   separates Body from Exception), and a non-external message and asserts the disclaimer is absent;
   the full S1F12 render assertion lives in Task 18's data test.
5. GREEN.
6. Commit: `feat(gemgen): render header-only messages`.

#### Task 8 — `main.go` wiring + write files; smoke-generate S1F1 into a temp dir

**Model/Effort:** sonnet / medium — CLI wiring, mostly mechanical given Tasks 4/5/7 already built.

**Consumes:** Task 7 + Tasks 4/5. **Produces:** working CLI: `-items`, `-messages`, `-out`; reads
all `*.yaml` under `-messages`, loads+validates, writes `<out>/items.go` and `<out>/sN.go` +
`<out>/sN_test.go` per stream.

Steps:
1. Failing `tools/gemgen/main_smoke_test.go` that writes a tiny `items.yaml` (empty map ok) and a
   `messages/s1.yaml` containing only S1F1 into `t.TempDir()`, calls `run(itemsPath, msgsDir,
   outDir)` (extract the orchestration into a testable `run(...)` func, `main` just calls it and
   `os.Exit`s on error), and asserts `outDir/s1.go` exists, contains `func S1F1()`, and that
   `go/parser.ParseFile` accepts it (valid Go).
2. RED.
3. Implement `run`: `LoadItems` → `ValidateItems` → glob message files → `LoadMessageFile` each →
   `ValidateMessages(all, items)` → `renderItems` (Task 11, stub returns minimal valid file for now:
   `// Code generated...\npackage gem\n`) → `renderMessages` per file → `renderTests` per file
   (stub for now) → `os.WriteFile`. `main` parses flags and calls `run`.
4. GREEN.
5. Commit: `feat(gemgen): CLI orchestration + file writing`.

### Milestone E — scale generation to all node types (TDD, golden per group)

Each task below adds cases to `renderMessages` view-model building (via `params.go`, already
complete) and asserts golden output. Because `params.go`/`BodyExpr` already handle every case,
these tasks are mostly **golden-output assertions** that lock the exact generated text; add code
only where a case reveals a gap.

#### Task 9 — fixed list + host variant (S1F2 / S1F2Host)

**Model/Effort:** sonnet / medium — golden-test expansion onto an already-proven template.

**Consumes:** Task 8. **Produces:** golden coverage for actor variants + fixed leaves.

Steps:
1. Failing golden test: render a MessageFile with S1F2 (equipment + host bodies), assert output
   contains exactly:
   ```
   func S1F2(mdln string, softrev string) secs2.SECS2Message {
       return secs2.NewMessage(1, 2, false, secs2.L(secs2.A(mdln), secs2.A(softrev)))
   }
   ```
   and
   ```
   func S1F2Host() secs2.SECS2Message {
       return secs2.NewMessage(1, 2, false, secs2.L())
   }
   ```
2. RED (host-suffix naming or empty-list `secs2.L()` may be missing) → implement/fix → GREEN.
3. Commit: `test(gemgen): actor variants golden`.

#### Task 10 — single leaf + open repeat (S1F5, S1F3)

**Model/Effort:** sonnet / low — small, single golden case on an already-proven path.

1. Golden test asserting `func S1F5(sfcd byte)` body `secs2.B(sfcd)` and `func S1F3(svids
   ...secs2.Item)` body `secs2.L(svids...)`. RED→GREEN→commit.

#### Task 11 — nested list + enum leaf (S1F14 / S1F14Host) and `gen_items.go`

**Model/Effort:** sonnet / high — introduces enum constant generation, a new template with more moving parts than Task 7/9.

**Consumes:** Task 6 + Task 8 stub. **Produces:** real `renderItems(map[string]Item) ([]byte,
error)` + `templates/items.go.tmpl`; enum consts.

Steps:
1. Failing `tools/gemgen/gen_items_test.go`:
   ```go
   func TestRenderItemsEnum(t *testing.T) {
       items := map[string]Item{
           "COMMACK": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "byte",
               Description: "Establish Communications Acknowledge Code, 1 byte.", Source: "e5",
               Values: []ItemValue{{"Accepted", 0}, {"Denied", 1}}},
       }
       out, err := renderItems(items)
       require.NoError(t, err)
       src := string(out)
       require.Contains(t, src, "type COMMACK byte")
       require.Contains(t, src, "COMMACKAccepted COMMACK = 0")
       require.Contains(t, src, "COMMACKDenied COMMACK = 1")
   }
   ```
2. RED.
3. `renderItems`: only items with non-empty `Values` emit output (must be `fixed`, so a concrete
   `goType` exists — enforce: if an item has `Values` but is `open`, return an error, since there is
   no Go type to define the named type over; `ValidateItems` (Task 4) already rejects this earlier,
   but keep the guard here as defense-in-depth). For each such item emit, in item-name order:
   - a **named defined type declaration** `type <ITEM> <goType>` (e.g. `type COMMACK byte`), then
   - a `const (...)` block of its values: const name = `<ITEM><ValueName>`, **type = the named type
     `<ITEM>` itself (NOT the bare `goType`)**, value = decimal, in value order.

   So COMMACK yields `type COMMACK byte` plus `COMMACKAccepted COMMACK = 0` /
   `COMMACKDenied COMMACK = 1`. Typing the consts as `<ITEM>` (not `byte`) is what makes them
   assignable to the generated `commack COMMACK` parameter without a cast. `format.Source` the result.
   The view model carries per-item `{Name, GoType string; Consts []constView}` where each `constView`
   is `{Name string; Value int64}`. Template `templates/items.go.tmpl` (Types section before the
   Constants section per decorder — the type decls precede the const block, both grouped by item):
   ```
   // Code generated by gemgen; DO NOT EDIT.

   package gem
   {{range .Types}}
   // {{.Name}} enumerates the defined values of the {{.Name}} item.
   type {{.Name}} {{.GoType}}

   const (
   {{$typeName := .Name}}{{range .Consts}}    {{.Name}} {{$typeName}} = {{.Value}}
   {{end}})
   {{end}}
   ```
   (Render each item's type+const pair together; `{{$typeName}}` on the const line is the item's
   named type. If decorder requires *all* type decls before *any* const block across items, group all
   `type` decls first, then all `const` blocks — but for Phase 1 only COMMACK exists, so a single
   type+const pair is emitted and the grouping question is moot; note it for Phase 2.)
4. Wire `renderItems` into `run` (replace the Task 8 stub). GREEN.
5. Add S1F14 golden to `gen_messages_test.go`: assert
   ```
   func S1F14(commack COMMACK, mdln string, softrev string) secs2.SECS2Message {
       return secs2.NewMessage(1, 14, false, secs2.L(secs2.B(byte(commack)), secs2.L(secs2.A(mdln), secs2.A(softrev))))
   }
   func S1F14Host(commack COMMACK) secs2.SECS2Message {
       return secs2.NewMessage(1, 14, false, secs2.L(secs2.B(byte(commack)), secs2.L()))
   }
   ```
   This golden locks the enum behavior end-to-end: the parameter is the named type `COMMACK` (not
   `byte`) and the body wraps it in `byte(commack)` before `secs2.B` (§4 constraint 12). Together
   with `TestRenderItemsEnum` (step 1, which asserts `type COMMACK byte` in `items.go`), the two
   assertions cover both halves of the mechanism: the type declaration/consts in `items.go` and the
   typed-param + call-site conversion in `s1.go`. RED→GREEN.
6. Commit: `feat(gemgen): item enum consts + nested-list golden`.

#### Task 12 — opaque body (S1F6 / S1F8)

**Model/Effort:** sonnet / low — single small golden case.

1. Golden: `func S1F6(body secs2.Item)` body `body`, i.e.
   `return secs2.NewMessage(1, 6, false, body)`. RED→GREEN→commit.

#### Task 13 — sibling + nested repeats (S1F19, S1F20, S1F22, S1F24)

**Model/Effort:** opus / high — E5's hardest S1 structural shape; the exact construct both external design reviews flagged as a risk area before it was proven out.

1. Golden assertions for the four signatures/bodies in §7. These are the exact strings the fixed
   three-field `Param` model (Task 6) plus the three-way `params` helper (Task 7) must produce — the
   non-final repeats render as `[]secs2.Item` slices, only the final repeat is variadic:
   - S1F19 → `func S1F19(objtype secs2.Item, objids []secs2.Item, attrids ...secs2.Item)` body
     `secs2.NewMessage(1, 19, true, secs2.L(objtype, secs2.L(objids...), secs2.L(attrids...)))`.
   - S1F20 → `func S1F20(objects []secs2.Item, errors ...secs2.Item)` body
     `secs2.NewMessage(1, 20, false, secs2.L(secs2.L(objects...), secs2.L(errors...)))`.
   - S1F22, S1F24 → single final variadic repeat (`vids ...secs2.Item` / `ceids ...secs2.Item`)
     per §7. RED→GREEN.
2. Commit: `test(gemgen): sibling+nested repeat golden`.

#### Task 14 — `gen_tests.go` + `templates/tests.go.tmpl`

**Model/Effort:** sonnet / high — mirrors Task 6's tree-walk logic for test assertions; correctness-critical since this is what's supposed to catch content-authoring bugs, but lower net-new design risk since Task 6 already proved the walker.

**Consumes:** Task 6. **Produces:** `renderTests(MessageFile, map[string]Item) ([]byte, error)`
emitting `gem/sN_test.go` — a per-stream `assertS<N>Header` helper plus one `TestS<N>F<M>[Host]` per
body that (a) builds the message with deterministic sample inputs, (b) asserts stream/function/wait
bit, (c) decodes the body and walks the `structure` tree asserting each node's secs2 type and value.

**Sample-input rule (revised — the original single-shared-literal-per-type rule was found during
implementation to be a real coverage gap and must NOT be used):** the original design called for
one fixed literal per Go type (`fixed string → "X"`, `fixed byte → 0x01`), but two sibling items of
the *same Go type* (e.g. S1F2's `MDLN`/`SOFTREV`, both `string`) would then receive the identical
sample value, so a generator regression that swapped their order in the body expression would
produce identical test output and pass undetected — directly defeating this task's stated purpose
("catches two same-typed sibling items swapped," design doc Testing Strategy section).

Corrected rule: derive the sample **from the item's own name**, not merely its type, so any two
distinct items — same Go type or not — always get distinguishable samples:
- Fixed `string` → the lowercased item name itself, e.g. `MDLN` → `"mdln"`, `SOFTREV` → `"softrev"`.
  Doubles as a self-documenting test-failure message ("mdln: got X, want mdln").
- Fixed `byte` (including enum-typed leaves like `COMMACK`) → a deterministic byte derived from the
  item name (e.g. a stable hash of the name truncated to one byte, or an FNV-1a-style fold) — NOT a
  shared literal `0x01` for every byte item. An untyped derived constant is still assignable to a
  named enum parameter type (`COMMACK`) with no cast needed, per the original rule's reasoning.
- Open leaf / repeat element / opaque → `secs2.A(itemName)` (the lowercased item/repeat name itself,
  same as the fixed-string rule — already per-parameter-labeled in the original rule for these
  kinds; the shipped implementation uses the plain name with no extra prefix).

Steps:
1. Failing `tools/gemgen/gen_tests_test.go`: render the S1F2 message, assert the output contains
   `func TestS1F2(t *testing.T) {`, a call `gem.S1F2("mdln", "softrev")` (distinguishable per-item
   samples, not the flawed shared `"X"`), and `assertS1Header(t, msg, 2, false)`, and that
   `go/parser.ParseFile` accepts the whole file. Add a second assertion proving the fix has teeth:
   swapping the two sample values in the generated call would fail the corresponding decode
   assertions (document this as a code comment referencing the swap-detection purpose, since the
   test itself can't literally test its own regression-catching power without a mutation).
2. RED.
3. Implement `renderTests`. The tree-walk assertion is generated recursively from the same
   `StructureNode` the builder used, producing testify `require` lines that index into decoded
   lists. For header-only, assert `msg.Item().IsEmpty()`. For a fixed leaf under a list, assert the
   decoded child `.ToASCII()`/`.ToBinary()` equals the sample. For repeats/opaque (caller-supplied
   `secs2.Item`), pass a labeled sample item and assert it appears at that index by comparing
   `ToBytes()`. Keep the generated assertions mechanical: reuse `params.go` to get the sample call
   arguments and a parallel `walkAssert(node, pathExpr)` emitter.
4. Wire `renderTests` into `run` (replace stub). GREEN.
5. Commit: `feat(gemgen): render body-tree tests`.

#### Task 15 — bounded-arity list: generator unit test (schema/validator/params coverage)

**Model/Effort:** sonnet / medium — narrow, single new case testing infrastructure Task 6 already built.

**Consumes:** Tasks 5, 6. **Produces:** proof the bounded node type is parsed, validated, and
rendered even though no S1 message uses it.

Steps:
1. Add `tools/gemgen/bounded_test.go`:
   - `LoadMessageFile` a synthetic S99F1 body `{type: list, minItems: 0, maxItems: 4, items:
     [{item: MDLN}]}` and assert `ValidateMessages` passes (MDLN present) — teeth: also assert that
     adding `repeat: x` alongside `minItems` makes it fail.
   - `BuildParams`/`BodyExpr`/`BodyDoc` on that node → `[{mdln,string,false,false}]`,
     `secs2.L(secs2.A(mdln))`, `L[0,4]{ A[mdln] }`.
2. RED→GREEN→commit `test(gemgen): bounded-arity node type`.

### Milestone F — content authoring (E5-sourced)

#### Task 16 — `extract_items.py` and generate `items.yaml`

**Model/Effort:** opus / high — bulk extraction of 335 items where precision matters most: this exact data (Table 3 row/reference counts) produced conflicting miscounts from two independent reviewers earlier in this project's design phase, and a third miscount (336 vs. the correct 335) in the design's own original target — so treat volume + accuracy here as high-stakes, not routine scripting.

**Consumes:** §5 mapping. **Produces:** `tools/gemgen/data/extract_items.py` and
`tools/gemgen/data/items.yaml` (exactly 335 entries).

Steps:
1. Write `tools/gemgen/data/extract_items.py` exactly (mirrors §5; Go re-validates via Task 4):
   ```python
   #!/usr/bin/env python3
   """One-off: extract SEMI E5 Table 3 (Data Item Dictionary) into items.yaml.

   Not part of the Go build. Regenerate items.yaml only when Table 3 changes.
   Mirrors the format-code mapping in tools/gemgen/load.go (deriveBinding/deriveGoType);
   Go's ValidateItems re-derives and cross-checks, so any drift here fails generation.
   """
   import re
   import sys

   SRC = "/home/arlo/semi_standards/markdowns/e005-00-0813/e005-00-0813.md"
   START, END = 400, 966  # Table 3 body rows (inclusive) in the local markdown

   FORMAT_MAP = {
       "0": ["L"], "10": ["B"], "11": ["BOOLEAN"], "20": ["A"], "21": ["J"], "22": ["W"],
       "30": ["I8"], "31": ["I1"], "32": ["I2"], "34": ["I4"],
       "40": ["F8"], "44": ["F4"],
       "50": ["U8"], "51": ["U1"], "52": ["U2"], "54": ["U4"],
       "3()": ["I1", "I2", "I4", "I8"], "4()": ["F4", "F8"], "5()": ["U1", "U2", "U4", "U8"],
   }
   GOTYPE = {"A": "string", "J": "string", "W": "string", "B": "byte", "BOOLEAN": "bool",
             "I1": "int8", "I2": "int16", "I4": "int32", "I8": "int64",
             "U1": "uint8", "U2": "uint16", "U4": "uint32", "U8": "uint64",
             "F4": "float32", "F8": "float64"}

   # Curated overrides for named Table 3 rows whose Format cell is blank in the
   # local markdown. A named row with an empty/unparseable Format cell that is NOT
   # listed here is a hard extraction failure (see main), so no named item is ever
   # silently dropped. Each value is the format-token list the item should carry.
   #
   # RPMSOURLOC's Format cell is blank in the local copy, but its description is
   # "The LocationID ... Conforms to OBJID" — identical semantics to its adjacent
   # sibling RPMDESTLOC (e005-00-0813.md:787), which the same table gives explicit
   # format 20 (ASCII). Both are LocationID strings, so RPMSOURLOC is A (fixed
   # string). This is grounded in the sibling row, not guessed.
   #
   # PARAMVAL (e005-00-0813.md:660), PDEATTRIBUTEVALUE (:668), and PRPAUSEEVENT
   # (:704) each have a corrupted list-format token ("1" or "00") that isn't a
   # real E5 format code — Table 1 only defines list as format 0. Each row's own
   # description confirms list semantics: PARAMVAL's "Values that are lists are
   # restricted to lists of single items of the same format type", PDEATTRIBUTE-
   # VALUE's Values cell literally says "00 used for list of strings", and
   # PRPAUSEEVENT's description opens with "The list of event identifiers". All
   # three corrupted tokens are corrected to "0" (list) alongside their other,
   # correctly-parsed format tokens.
   #
   # SPR (:836) has no format code at all — its Format cell reads "Device
   # Dependent" and its Values cell repeats "Device dependent" — a genuinely
   # equipment-defined item with no fixed SECS-II representation. Given an empty
   # formats list, so binding derives to open (never "exactly one known format",
   # the fixed-binding precondition) with no fabricated format token.
   OVERRIDES = {
       "RPMSOURLOC": ["20"],  # e005-00-0813.md:788, blank cell; inherits RPMDESTLOC's format 20 (A)
       "PARAMVAL": ["0", "10", "11", "20", "3()", "4()", "5()"],  # :660, "1" -> "0" (list)
       "PDEATTRIBUTEVALUE": ["0", "11", "20", "21", "51"],  # :668, "00" -> "0" (list)
       "PRPAUSEEVENT": ["0"],  # :704, "00" -> "0" (list)
       "SPR": [],  # :836, "Device Dependent" has no format code; open binding, no formats
   }

   def clean(cell):
       cell = cell.replace("<br>", " ")
       cell = re.sub(r"<[^>]+>", " ", cell)
       return re.sub(r"\s+", " ", cell).strip()

   def expand_formats(cell):
       out = []
       for tok in re.split(r"[,\s]+", clean(cell)):
           if not tok:
               continue
           if tok not in FORMAT_MAP:
               sys.exit(f"unknown format token {tok!r}")
           for c in FORMAT_MAP[tok]:
               if c not in out:
                   out.append(c)
       return out

   def yaml_str(s):
       if s != s.strip() or re.search(r"""[:#\[\]{},&*!|>'"%@`]""", s):
           return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'
       return s

   def main():
       rows, order = {}, []
       with open(SRC) as f:
           lines = f.readlines()
       for lineno, ln in enumerate(lines[START - 1:END], start=START):
           if not ln.lstrip().startswith("|"):
               continue
           cells = [c.strip() for c in ln.strip().strip("|").split("|")]
           if len(cells) < 3:
               continue
           name = re.sub(r"\s*\(cont\.\)$", "", clean(cells[0]))
           if name in ("", "Name") or not re.match(r"^[A-Za-z]", name):
               continue  # header, separator, or non-named continuation artifact
           if name in rows:
               continue  # first occurrence wins (merges page-break repeats)
           formats = expand_formats(cells[1])
           if not formats:
               # This is a NAMED row (passed the name filter, first occurrence) whose
               # Format cell is blank/unparseable. Never drop it silently: use a curated
               # override if one exists, else fail loudly with the source line + name.
               if name in OVERRIDES:
                   formats = expand_formats(" ".join(OVERRIDES[name]))
               else:
                   sys.exit(f"{SRC}:{lineno}: named item {name!r} has an empty or unparseable "
                            f"Format cell; add an OVERRIDES[{name!r}] entry (its format tokens, "
                            f"grounded in E5) — refusing to silently drop a named row")
           rows[name] = (formats, clean(cells[2]))
           order.append(name)
       for name in sorted(order):
           formats, desc = rows[name]
           fixed = len(formats) == 1 and formats[0] != "L"
           print(f"{name}:")
           print(f"  formats: [{', '.join(formats)}]")
           print(f"  binding: {'fixed' if fixed else 'open'}")
           if fixed:
               print(f"  goType: {GOTYPE[formats[0]]}")
           if desc:
               print(f"  description: {yaml_str(desc)}")
           print("  source: e5")

   if __name__ == "__main__":
       main()
   ```
2. Run `python3 tools/gemgen/data/extract_items.py > tools/gemgen/data/items.yaml`. The script now
   **hard-fails** (non-zero exit, source line + item name printed) on any named Table 3 row whose
   Format cell is empty/unparseable and is not in `OVERRIDES`, so a dropped named item can no longer
   pass silently. `RPMSOURLOC` is pre-seeded in `OVERRIDES` (grounded in its sibling `RPMDESTLOC`);
   if extraction hard-fails on a different named row, add a grounded `OVERRIDES` entry for it (do not
   loosen the check) and re-run.
3. Verify count: `grep -cE '^[A-Za-z][A-Za-z0-9_]*:$' tools/gemgen/data/items.yaml`. This MUST be
   exactly `335` (assert it — fail loudly if it differs; **the target itself was corrected from an
   original 336 to 335 during implementation** — the original count double-counted `ERRCODE`'s
   split-across-page-break `ERRCODE (cont.)` continuation rows as a distinct 336th entry instead of
   merging them into the single `ERRCODE` item they continue; see the design doc's corrected item
   catalog note). The exactness is deliberate now that named-row skipping is no longer silent: with
   hard-fail + curated overrides, every named Table 3 body row is accounted for, so an off-by-N count
   is a real content discrepancy (wrong `START`/`END` boundary, a missed override, or an unexpected
   extra/duplicate row) to investigate — NOT something to paper over by nudging `START`/`END` until
   the number happens to land on 335. Record the exact count in the commit message.
4. Spot-check by hand against E5 for 10 items covering variants: `MDLN` (fixed string),
   `SOFTREV` (fixed string), `COMMACK` (fixed byte), `SFCD` (fixed byte), `SVID` (open, multi),
   `SV` (open, includes `L`), `ATTRDATA` (open, includes `L`), `CEID` (open), `ERRCODE` (open,
   `5()`), `VID` (open). Confirm formats/binding/goType match §5. Worked expected entries:
   ```yaml
   MDLN:
     formats: [A]
     binding: fixed
     goType: string
     description: Equipment Model Type, 20 bytes max.
     source: e5
   SFCD:
     formats: [B]
     binding: fixed
     goType: byte
     description: Status form code, 1 byte.
     source: e5
   SVID:
     formats: [A, I1, I2, I4, I8, U1, U2, U4, U8]
     binding: open
     description: Status variable ID.
     source: e5
   SV:
     formats: [L, B, BOOLEAN, A, J, I1, I2, I4, I8, F4, F8, U1, U2, U4, U8]
     binding: open
     description: Status variable value.
     source: e5
   ERRCODE:
     formats: [U1, U2, U4, U8]
     binding: open
     description: Code identifying an error.
     source: e5
   ```
   (Note ordering within a formats list follows §5 expansion order: `20`→A, then `3()`→I1..I8, then
   `5()`→U1..U8, so `SVID` is `[A, I1, I2, I4, I8, U1, U2, U4, U8]`. If a hand spot-check disagrees
   with a generated line, fix `extract_items.py`, not `items.yaml`.)
5. Run `cd tools/gemgen && go test -run TestLoad` (Task 4 tests) against the real file by adding a
   temporary test that loads `data/items.yaml` and calls `ValidateItems` — assert no error. Keep
   this as a permanent regression test `tools/gemgen/data_items_test.go`.
6. Commit: `feat(gemgen): extract E5 Table 3 items (N=<count>)`.

#### Task 17 — curated `COMMACK` values overlay

**Model/Effort:** sonnet / low — one small hand-curated enum table.

**Consumes:** Task 16. **Produces:** `values:` block on `COMMACK` in `items.yaml`.

Rationale: the extraction script does not parse the messy free-text Values column into clean enum
tables (E5 value cells mix `N = Label`, ranges, and bitfield prose). COMMACK is the only enum S1
acceptance requires; add it by hand.

Steps:
1. Edit `COMMACK` in `items.yaml` to:
   ```yaml
   COMMACK:
     formats: [B]
     binding: fixed
     goType: byte
     description: Establish Communications Acknowledge Code, 1 byte.
     source: e5
     values:
       - {name: Accepted, value: 0}
       - {name: Denied, value: 1}
   ```
   (E5 Table 3: `0 = Accepted`, `1 = Denied, Try Again`. Use `Denied` for the identifier; `2-63`
   are Reserved and not emitted.)
2. Re-run the Task 16 permanent load test — still valid.
3. Commit: `feat(gemgen): COMMACK enum values`.

#### Task 18 — author `messages/s1.yaml` (all 24 functions)

**Model/Effort:** opus / high — Phase 1's actual content deliverable: faithfully transcribing E5 §10 semantics for 24 functions. This is exactly the task where the S1F12 provenance gap surfaced during design review — a domain-transcription error here is easy to miss and costly, since every downstream generated function depends on it being right.

**Consumes:** Tasks 16-17 (item names must exist). **Produces:** complete
`tools/gemgen/data/messages/s1.yaml`.

Steps:
1. Write the file verbatim (this is Phase 1's message content deliverable — complete, not a sample):
   ```yaml
   stream: 1
   messages:
     - function: 1
       name: Are You There Request
       mnemonic: R
       direction: bidirectional
       description: Establishes if the equipment is on-line.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: null

     - function: 2
       name: On Line Data
       mnemonic: D
       direction: bidirectional
       description: Data signifying that the equipment is alive.
       exception: The host sends a zero-length list to the equipment.
       source: e5
       bodies:
         - actor: equipment
           replyExpected: false
           structure: {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
         - actor: host
           replyExpected: false
           structure: {type: list, items: []}

     - function: 3
       name: Selected Equipment Status Request
       mnemonic: SSR
       direction: host-to-equipment
       description: A request to the equipment to report selected values of its status.
       exception: A zero-length list means report all SVIDs.
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {type: list, repeat: svids, of: {item: SVID}}

     - function: 4
       name: Selected Equipment Status Data
       mnemonic: SSD
       direction: equipment-to-host
       description: The equipment reports the value of each SVID requested, in the order requested.
       exception: A zero-length list item for an SV means the corresponding SVID does not exist.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure: {type: list, repeat: svs, of: {item: SV}}

     - function: 5
       name: Formatted Status Request
       mnemonic: FSR
       direction: host-to-equipment
       description: A request for the equipment to report status according to a predefined fixed format.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {item: SFCD}

     - function: 6
       name: Formatted Status Data
       mnemonic: FSD
       direction: equipment-to-host
       description: The equipment reports the value of status variables according to the SFCD.
       exception: A zero-length item means that no report can be made.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure: {type: opaque}

     - function: 7
       name: Fixed Form Request
       mnemonic: FFR
       direction: host-to-equipment
       description: A request for the form used in S1F6.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {item: SFCD}

     - function: 8
       name: Fixed Form Data
       mnemonic: FFD
       direction: equipment-to-host
       description: The equipment returns the fixed form specified for S1F6.
       exception: A zero-length item means the form is unavailable.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure: {type: opaque}

     - function: 9
       name: Material Transfer Status Request
       mnemonic: TSR
       direction: host-to-equipment
       description: A request to report the status of all material ports to the host.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: null

     - function: 10
       name: Material Transfer Status Data
       mnemonic: TSD
       direction: equipment-to-host
       description: The equipment reports the transfer status of all material ports.
       exception: >-
         A zero-length input or output port list means there are no such ports.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure:
             type: list
             items:
               # TSIP/TSOP are E5's packed multi-value single item (<tsip1,,tsipn>), NOT a list of
               # separate items -- packed, not repeat. See design doc's S1F10 correction and §4.13.
               - {type: list, packed: tsips, of: {item: TSIP}}
               - {type: list, packed: tsops, of: {item: TSOP}}

     - function: 11
       name: Status Variable Namelist Request
       mnemonic: SVNR
       direction: host-to-equipment
       description: A request to the equipment to identify certain status variables.
       exception: A zero-length list means report all SVIDs.
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {type: list, repeat: svids, of: {item: SVID}}

     # S1F12 is reconstructed from an external reference, NOT our local E5 markdown, whose §10
     # Message Detail section drops the page between F11 and F13 during PDF-to-markdown conversion.
     # Table 3 in the same local copy still cross-references S1F12 for SVNAME (e005-00-0813.md:862)
     # and UNITS (:927), confirming the function is real: this is a local-copy gap, not a genuine
     # discrepancy with the standard. Re-verify against a complete E5 copy if one becomes available.
     - function: 12
       name: Status Variable Namelist Reply
       mnemonic: SVN
       direction: equipment-to-host
       description: The equipment reports the name and units for each status variable requested in S1F11.
       exception: Zero-length SVNAME and UNITS items indicate that the SVID does not exist.
       source: external
       confidence: low
       bodies:
         - actor: both
           replyExpected: false
           structure:
             type: list
             repeat: svids
             of: {type: list, items: [{item: SVID}, {item: SVNAME}, {item: UNITS}]}

     - function: 13
       name: Establish Communications Request
       mnemonic: CR
       direction: bidirectional
       description: Provides a formal means of initializing communications at the application level.
       exception: The host sends a zero-length list to the equipment.
       source: e5
       bodies:
         - actor: equipment
           replyExpected: true
           structure: {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
         - actor: host
           replyExpected: true
           structure: {type: list, items: []}

     - function: 14
       name: Establish Communications Request Acknowledge
       mnemonic: CRA
       direction: bidirectional
       description: Accepts or denies an Establish Communications Request. MDLN and SOFTREV are valid only if COMMACK is 0.
       exception: The host sends a zero-length list for the second item to the equipment.
       source: e5
       bodies:
         - actor: equipment
           replyExpected: false
           structure:
             type: list
             items:
               - {item: COMMACK}
               - {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
         - actor: host
           replyExpected: false
           structure:
             type: list
             items:
               - {item: COMMACK}
               - {type: list, items: []}

     - function: 15
       name: Request OFF-LINE
       mnemonic: ROFL
       direction: host-to-equipment
       description: The host requests that the equipment transition to the OFF-LINE state.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: null

     - function: 16
       name: OFF-LINE Acknowledge
       mnemonic: OFLA
       direction: equipment-to-host
       description: Acknowledge or error for the OFF-LINE request.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure: {item: OFLACK}

     - function: 17
       name: Request ON-LINE
       mnemonic: RONL
       direction: host-to-equipment
       description: The host requests that the equipment transition to the ON-LINE state.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: null

     - function: 18
       name: ON-LINE Acknowledge
       mnemonic: ONLA
       direction: equipment-to-host
       description: Acknowledge or error for the ON-LINE request.
       exception: None
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure: {item: ONLACK}

     - function: 19
       name: Get Attribute
       mnemonic: GA
       direction: bidirectional
       description: Request for attribute data relating to the specified object or entity within the equipment.
       exception: >-
         A zero-length object list (m = 0) requests all objects of the type.
         A zero-length attribute list (n = 0) requests all attributes.
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure:
             type: list
             items:
               - {item: OBJTYPE}
               - {type: list, repeat: objids, of: {item: OBJID}}
               - {type: list, repeat: attrids, of: {item: ATTRID}}

     - function: 20
       name: Attribute Data
       mnemonic: AD
       direction: bidirectional
       description: Transfers the requested set of object attributes, in request order.
       exception: >-
         m = 0 means the OBJTYPE is unknown. n = 0 means the object was not found.
         A zero-length ATTRDATA means the attribute does not exist. p = 0 means no errors.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure:
             type: list
             items:
               - {type: list, repeat: objects, of: {type: list, repeat: attrs, of: {item: ATTRDATA}}}
               - {type: list, repeat: errors, of: {type: list, items: [{item: ERRCODE}, {item: ERRTEXT}]}}

     - function: 21
       name: Data Variable Namelist Request
       mnemonic: DVNR
       direction: host-to-equipment
       description: Allows the host to request basic information about the data variables available in the equipment.
       exception: A zero-length list means send information for all data variables.
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {type: list, repeat: vids, of: {item: VID}}

     - function: 22
       name: Data Variable Namelist
       mnemonic: DVN
       direction: equipment-to-host
       description: The equipment reports the information for the VIDs requested in S1F21.
       exception: Zero-length DVVALNAME and UNITS items indicate that the VID does not exist or is not a DVVAL-class variable.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure:
             type: list
             repeat: vids
             of: {type: list, items: [{item: VID}, {item: DVVALNAME}, {item: UNITS}]}

     - function: 23
       name: Collection Event Namelist Request
       mnemonic: CENR
       direction: host-to-equipment
       description: Allows the host to retrieve which collection event IDs are available and which DVVALs are valid for each.
       exception: A zero-length list means send information for all CEIDs.
       source: e5
       bodies:
         - actor: both
           replyExpected: true
           structure: {type: list, repeat: ceids, of: {item: CEID}}

     - function: 24
       name: Collection Event Namelist
       mnemonic: CEN
       direction: equipment-to-host
       description: The equipment reports the collection events and associated VIDs for the CEIDs requested in S1F23.
       exception: Zero-length CENAME and associated-VID list indicate that the CEID does not exist.
       source: e5
       bodies:
         - actor: both
           replyExpected: false
           structure:
             type: list
             repeat: ceids
             of:
               type: list
               items:
                 - {item: CEID}
                 - {item: CENAME}
                 - {type: list, repeat: vids, of: {item: VID}}
   ```
2. Verify every item name referenced above exists in `items.yaml`:
   `MDLN SOFTREV SVID SVNAME SV SFCD OFLACK ONLACK TSIP TSOP OBJTYPE OBJID ATTRID ATTRDATA ERRCODE
   ERRTEXT VID DVVALNAME UNITS CEID CENAME COMMACK`. Quick check: for each, `grep -c '^<NAME>:'
   items.yaml` == 1. (`SVNAME` is Table-3 row `e005-00-0813.md:862`, format `20` → fixed `string`;
   the Task 16 extraction captures it automatically.)
3. Add a permanent test `tools/gemgen/data_messages_test.go` that loads `data/items.yaml` +
   `data/messages/s1.yaml`, runs `ValidateMessages`, and asserts 24 messages and no error. In the
   same test, assert the S1F12 entry carries both `source: external` and `confidence: low`, then
   `renderMessages` the file and assert the generated S1F12 godoc contains the exact disclaimer line
   `Source: reconstructed from an external reference, not verified against the purchased SEMI standard.` and that no
   other (e5-sourced) function's godoc contains it. This is the "render S1F12 specifically" gate for
   the external-provenance disclaimer.
4. RED (if any item name is wrong or count != 24) → fix → GREEN.
5. Commit: `feat(gemgen): author S1 message schema (24 functions)`.

### Milestone G — wire, generate, gate

#### Task 19 — `gem/generate.go` directive

**Model/Effort:** sonnet / low — one file, one directive.

**Consumes:** Task 8 CLI. **Produces:** `gem/generate.go`.

**Resolve before committing (do this FIRST, not as an afterthought):** the whole directive hinges on
what working directory the *gemgen binary* runs with, because its flags are relative paths
(`-items data/items.yaml`, `-messages data/messages`, `-out ../../gem`) resolved by the binary
itself, not by `go`. Those paths are written assuming the binary's cwd is `tools/gemgen/`. The
`go run -C dir` form is *expected* to satisfy that: cmd/go's `-C` performs a real process `chdir`
into `dir` early in the command, so the child binary `go run` spawns inherits `dir` as its cwd —
making `data/items.yaml` resolve under `tools/gemgen/` and `../../gem` resolve to the repo-root
`gem/`. This is not to be assumed on faith, though — the implementer MUST confirm which directive
actually generates the three files into `gem/` (not into `gem/data/...`) before committing
`gem/generate.go`. Do not commit a directive that has not been observed to work.

Steps:
1. Write `gem/generate.go` with the `-C` form (expected-correct per the cwd analysis above):
   ```go
   package gem

   //go:generate go run -C ../tools/gemgen . -items data/items.yaml -messages data/messages -out ../../gem
   ```
2. From `gem/`, run `go generate .`. Expected: writes `gem/items.go`, `gem/s1.go`,
   `gem/s1_test.go` (verify they land in `gem/`, and that no stray `gem/data/` tree is created,
   which would indicate the binary ran with the wrong cwd). If `-C` does not place the child
   process cwd at `tools/gemgen/` on this toolchain, use the explicit form
   `//go:generate bash -c 'cd ../tools/gemgen && go run . -items data/items.yaml -messages data/messages -out ../../gem'`.
   Commit only the form that was observed to produce the files in `gem/`; note which one in the
   commit message.
3. Commit: `feat(gem): wire go:generate for gemgen`.

#### Task 20 — regenerate and remove hand-written S1

**Model/Effort:** sonnet / low — mechanical execution + diff review.

**Consumes:** Task 19. **Produces:** generated `gem/s1.go`, `gem/items.go`, `gem/s1_test.go`
replacing the hand-written pair.

Steps:
1. `go generate ./...` from repo root (or `cd gem && go generate .`). The generated `s1.go` and
   `s1_test.go` overwrite the hand-written ones at the same paths (generator writes
   `<out>/s1.go`, `<out>/s1_test.go`). Confirm the hand-written `assertS1Header` + `TestS1F*`
   are gone and replaced by generated equivalents.
2. `git status` — expect modified `gem/s1.go`, `gem/s1_test.go`; new `gem/items.go`,
   `gem/generate.go`.
3. `cd gem && go build ./...` — compiles.
4. Commit: `feat(gem): regenerate S1 from gemgen`.

#### Task 21 — gate: `make test`

**Model/Effort:** sonnet / low — run the gate; escalate only if it fails and the fix needs Task 4-6-level judgment.

**Decision to make explicit for the implementer:** root `make test` runs `go test ./...` from the
**root** module and does NOT descend into `tools/gemgen` (a sibling module with its own `go.mod`).
The generator's own tests must be run explicitly from inside that module. Both are required gates.

Steps:
1. `make test`. Expected: all packages green under `-race`, including `gem` (generated
   `s1_test.go`). If the generated tests fail, fix the **generator/schema**, regenerate, re-run —
   never hand-edit generated files.
2. `cd tools/gemgen && go test ./...` — REQUIRED, in addition to root `make test`. This is the only
   invocation that exercises the generator's own unit tests (schema, load/validate, params, render,
   data tests), since root `make test` cannot reach the nested module. Must be green (add `-race`
   to match repo convention). If it fails, fix the generator and re-run.
3. `cd tools/gemgen && go test -tags integration ./...` — REQUIRED, separately from step 2.
   `packed_compile_integration_test.go` (added during the S1F10 `packed`-node fix chain) is
   `//go:build integration`-gated because it shells out to a real `go build` against the actual
   `secs2` package to catch compile errors in generated body expressions — the exact bug class two
   earlier fix attempts missed by only checking `gofmt`/`go/parser` syntax validity. Neither step 1
   nor step 2 runs build-tagged tests, so this step is the only thing that exercises it; skipping it
   silently disables the one regression guard added specifically because syntax-only checks weren't
   enough. Must be green.
4. Commit only if any generator fix was needed: `fix(gemgen): <specific>`.

#### Task 22 — gate: `make lint` (root) + gemgen lint

**Model/Effort:** sonnet / low — run the gates; the module-boundary decision is already made below.

**Decision to make explicit for the implementer:** `make lint` runs golangci on the root module and
does **not** descend into `tools/gemgen` (separate module). Two sub-steps:

Steps:
1. `make lint` — 0 issues on the root module (this covers generated `gem/*.go`). Fix any decorder /
   godoc / import-order issues by adjusting the **templates**, then regenerate (Task 20) and re-lint.
   Iterate until clean.
2. Lint `tools/gemgen` itself with the **same pinned toolchain**, run from inside the module so
   golangci-lint's build list is the gemgen module while the tool binary and config come from the
   root pin. The root `GOLANGCI` recipe is `go tool -modfile=.linter.go.mod golangci-lint` (Makefile
   `GOLANGCI`/`LINTER_MOD`); from inside `tools/gemgen` the pinned modfile is two levels up, so:
   ```
   cd tools/gemgen && go tool -modfile=../../.linter.go.mod golangci-lint run ./...
   ```
   This reuses the repo's exact pinned linter version and the root `.golangci.yaml` (discovered by
   walking up from the working directory), so `tools/gemgen` is held to the identical standard as the
   root module. It must report 0 issues.

   `go vet` + `gofmt -l` is **NOT** an acceptable substitute — it is weaker than the repo's
   pre-commit lint requirement (`.agents/rules/500-workflow.md`) and misses the decorder/godoc/import
   checks that gate every other `.go` file. If the pinned golangci-lint genuinely cannot be made to
   run against the nested module with the command above, then **Phase 1 is blocked** until a real
   nested-module lint target exists (e.g. a `lint-gemgen` Makefile target wrapping the pinned
   invocation) — do NOT proceed on a downgraded check.
3. `go fix ./...` per `.agents/rules/700-lint-after-write.md`; re-lint until clean.
4. Commit any template/format fixes: `style(gemgen): lint-clean generated output`.

#### Task 23 — godoc readability review + final verification

**Model/Effort:** opus / medium — qualitative judgment call on generated prose (tone, clarity, no internal jargon), not large code-writing volume.

Steps:
1. `cd gem && go doc S1F1 S1F2 S1F6 S1F14 S1F20` (or `go doc ./... | less`). Manually confirm a
   sample reads well: name-first summary line states the message name + direction (NO section
   citation); description present; `Body:` shorthand present; `Exception:` line present; a blank
   `//` line separates every paragraph (summary / description / Body / Exception) so none merge; no
   internal jargon; no two sentences on one line. Expected sample for S1F2 (equipment body):
   ```
   // S1F2 creates an S1F2 (On Line Data) message for equipment, direction: bidirectional.
   //
   // Data signifying that the equipment is alive.
   //
   // Body: L[2]{ A[mdln] A[softrev] }.
   //
   // Exception: the host sends a zero-length list to the equipment.
   func S1F2(mdln string, softrev string) secs2.SECS2Message
   ```
   If wording is awkward, fix the godoc-emitting helper in `gen_messages.go` (or the template),
   regenerate, re-verify.
2. Re-run the **full gate**: root `make lint` + root `make test`, AND the nested-module gates
   `cd tools/gemgen && go test ./...` (Task 21 step 2) and `cd tools/gemgen && go tool
   -modfile=../../.linter.go.mod golangci-lint run ./...` (Task 22 step 2). All four green.
3. Final acceptance checklist (all must be true):
   - [ ] `tools/gemgen` builds and runs via `go generate ./...`.
   - [ ] `items.yaml` populated (exactly 335 items) from E5 Table 3; permanent load test passes.
   - [ ] `s1.yaml` authored (24 functions F1-F24, F12 with `source: external` + `confidence: low`);
         permanent validate test asserts 24, and the S1F12 godoc carries the external source disclaimer.
   - [ ] Each node type exercised by ≥1 generated S1 function (header-only, per-actor, enum
         (COMMACK), open, opaque, sibling+nested repeat, packed (S1F10)), bounded-arity covered by
         generator unit test.
   - [ ] `gem/s1.go`, `gem/items.go`, `gem/s1_test.go` generated; hand-written S1 removed.
   - [ ] Root `make lint` + root `make test` green.
   - [ ] Nested module gated: `cd tools/gemgen && go test ./...` green AND the pinned golangci-lint
         run from inside `tools/gemgen` reports 0 issues (NOT a `go vet`/`gofmt` substitute).
   - [ ] `cd tools/gemgen && go test -tags integration ./...` green — the real-`go build` compile
         guard for generated body expressions (added during the S1F10 `packed`-node fix) is
         build-tag-gated and invisible to every other gate; this is the only step that runs it.
   - [ ] Sample godoc reviewed.
   - [ ] Root `go.mod` unchanged (no new runtime dep); `tools/gemgen/go.mod` owns `yaml.v3`.
4. Commit: `chore(gem): Phase 1 GEM codegen complete`.

---

## 9. Risks & notes

- **`go generate -C` cwd semantics** (Task 19): the `go run -C` form is expected-correct (cmd/go's
  `-C` performs a real process `chdir`, which the spawned binary inherits), but Task 19 makes
  confirming the generated-file location a blocking pre-commit step, with the `bash -c 'cd ...'`
  explicit form as the fallback if the observed cwd differs. Resolved in Task 19 before commit, not
  assumed.
- **Table 3 row boundaries** (Task 16): `START`/`END` line numbers are for the current local
  markdown. The exact `335` count-verification step is the real gate (corrected from an original
  target of 336 during implementation — see the design doc's item-catalog note on the `ERRCODE`
  continuation-row merge). Because named-row skipping now hard-fails and empty-format rows are
  covered by curated `OVERRIDES`, an off-count is a real content discrepancy to diagnose (wrong
  boundary, missed override, unexpected extra/duplicate) — do NOT nudge `START`/`END` until the
  number lands on 335, and never edit `items.yaml` by hand.
- **Enum extraction is deliberately manual** (Task 17): only COMMACK is curated in Phase 1. Other
  items' `values:` are a Phase-2 enhancement; do not auto-parse the messy E5 Values column now.
- **S1F12 provenance** (§7 note): included with `source: external` because our local E5 markdown
  drops the F11→F13 page during PDF→markdown conversion; Table 3 confirms F12 is real. Re-verify
  against a complete E5 copy if one becomes available.
- **`gem/s2.go`, `report.go`, `doc.go`** are untouched; Phase 2 replaces them. No collisions:
  `items.go` holds only the enum named-type declarations plus their consts (just `COMMACK` in
  Phase 1); generated `s1.go` occupies the same symbols the hand-written file did.
- **Enum named type + call-site conversion** (§4 constraint 12; Tasks 4, 6, 11): an item with a
  `values:` table generates `type <ITEM> <goType>` and a typed parameter, and the generated body
  MUST wrap the parameter in `<goType>(...)` (e.g. `secs2.B(byte(commack))`) because the `secs2`
  constructor combiners type-switch on the exact primitive type. Verified against `secs2/binary.go`
  (`combineBinaryValues`) and `secs2/int.go` (`combineIntValues`/`combineIntValuesSlow`): a named
  type falls through to the `default:` "invalid type" branch, so omitting the conversion would
  silently yield a deferred-error item. `ValidateItems` rejects a `values:` table on an `open` item.

## 10. Self-review (performed on this plan)

- **Acceptance-criterion coverage** (design §"Phase 1 acceptance criteria"): own module + go
  generate → Tasks 1, 19; items.yaml 335 → Task 16; s1.yaml 24 functions → Task 18; every node
  type → Tasks 7-15 (+ §7 coverage map); generated s1.go/items.go/s1_test.go replacing hand-written
  → Task 20; make lint + make test → Tasks 21-22; godoc sample → Task 23. All mapped.
- **Node-type coverage**: header-only, fixed leaf (no `values:`), fixed enum leaf (has `values:`),
  open leaf, opaque, list(items), bounded list, repeat, **packed** — all appear in the §6 table, the
  params_test (Task 6), and are exercised by a concrete S1 function (fixed enum leaf via COMMACK in
  S1F14; packed via S1F10's `tsips`/`tsops`, added during Task 18 authoring after S1F10 was found to
  be a packed multi-value item, not a repeat; or, for bounded-arity, the Task 15 unit test). No node
  type is defined but untested.
- **Type/function-name consistency** (checked across tasks): `StructureNode` fields (Task 2) are
  consumed unchanged by `walkNode` (Task 5), `BuildParams`/`BodyExpr`/`BodyDoc` (Task 6),
  `funcView` (Task 7). `Item.Binding`/`GoType`/`Formats`/`Values` (Task 2) used consistently in
  Tasks 3-6, 11 — `len(Values) > 0` on a fixed item is the sole named-type signal (no redundant
  field), driving the `COMMACK`-typed parameter (Task 6 `BuildParams`), the `byte(commack)`
  conversion (Task 6 `BodyExpr`, §7/Task 11 goldens), and the `type COMMACK byte` + typed consts
  (Task 11 `renderItems`). `Param{Name,Type,Repeat,Variadic}` (Task 6) consumed by the `params` template
  helper (Task 7), whose three-way branch renders non-final repeat/packed params as `[]<Type>`, the
  final one as `...<Type>` (where `<Type>` is `secs2.Item` for `repeat`, a concrete goType like
  `byte` for `packed` — same branch, different `Type` value), and everything else as `name <Type>`.
  `deriveBinding`/`deriveGoType`
  (Task 3) reused by `ValidateItems` (Task 4). Actor→name
  suffix rule (`Host`) consistent between Tasks 7, 9, 11 and §7 signatures. `renderItems`/
  `renderMessages`/`renderTests`/`run` names consistent across Tasks 7, 8, 11, 14.
- **Repeat rule consistency**: §4 constraint 6 (repeats always `secs2.Item`), §6 table, §7
  signatures, and `BuildParams`/`BodyExpr` (Task 6) all agree — no `[]byte` element typing anywhere.
- **Placeholder scan**: no "TODO", "similar to Task N", "add error handling", or
  description-without-code steps remain; every code/YAML/step is concrete. (Stub `renderTests`/
  `renderItems` in Task 8 are explicitly replaced in Tasks 11/14 — flagged, not hidden.)
- **Fixes applied during review**: unified repeat element typing to `secs2.Item` for genuine
  list-of-separate-items repeats (an earlier `[]byte` idea for TSIP/TSOP was rejected at the time for
  diverging from gem's `...secs2.Item` convention — **later reversed**: Task 18 content authoring
  found S1F10's real E5 structure is a packed multi-value single item, not a list, so the rejected
  `[]byte`-style typing was actually correct for this one case; added a dedicated `packed` node kind,
  §4 constraint 13, distinct from `repeat`, rather than reopening the `repeat` rule itself); added
  `params.go` to the file map with justification;
  added the explicit gemgen-lint decision (Task 22) since `make lint` does not cross module
  boundaries; added the S1F12 provenance-exception note so its `source: external` inclusion is deliberate;
  made `ValidateItems` cross-check the Python extraction's binding/goType (gives the extraction teeth
  via the Go loader); made enum items (`values:` table) generate a named Go type (`type COMMACK
  byte`) with a typed parameter, and required the generated body to convert back to the primitive at
  the constructor call site (`secs2.B(byte(commack))`) — verified against the `secs2` combiners'
  exact-type switch, so an un-converted named type would silently produce a deferred-error item; also
  added a `ValidateItems` rule rejecting a `values:` table on an `open`-binding item.
