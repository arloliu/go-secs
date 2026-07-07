# GEM Message Code Generation — Design

Date: 2026-07-07
Status: Phase 1 design, revision 2 (incorporates Opus + Copilot review findings,
independently fact-checked against the local E5 spec)

## Goal

Replace hand-written GEM message builders (`gem/s1.go`, `s2.go`, `s5.go`, `s6.go`,
`s9.go`, `report.go`) with generated code, driven by a YAML DSL that describes
SECS-II data items and messages. Godoc for every generated message must state
name, direction, description, and exception behavior — short and simple.

## Background / source material

- Local SEMI E5 spec: `/home/arlo/semi_standards/markdowns/e005-00-0813/e005-00-0813.md`
  (authoritative, Table 1 Item Format Codes, Table 3 Data Item Dictionary,
  §10 Message Detail).
- `tmp/secs-gem/items.html`, `tmp/secs-gem/msgs.html`: fetched from
  `https://hume.com/secs/{items,msgs}.html`, a third-party SECS-II code
  generation tool. Covers streams S1–S21 across multiple SEMI standards, but
  the author explicitly documents deviations from the official standards
  (renamed/invented items, no octal type codes, custom TSN notation).
- `secsgem` (Python, open source, actively maintained) independently
  implements S0, S1, S2, S5, S6, S7, S9, S10 — used only as a secondary
  cross-check for the E5-covered streams.

## Scope

SECS-II defines streams S1–S10 directly in E5. Streams S12–S21 are defined by
separate "GEM300" standards (E39, E40, E42, E87, E90, E94, E116, …) that are
not available locally or freely online. Decision: cover **all of S1–S21**, but
track provenance per message:

- **S1–S10**: sourced from the local E5 spec (`source: e5`, high confidence).
- **S12–S21**: each stream's owning standard is cited when we have verified
  text (`source: e39`, `e40`, `e42`, `e87`, `e90`, `e94`, `e116`); where we
  don't have the actual standard locally and reconstructed structure from a
  third-party reference instead, `source: external` flags it as lower
  confidence for later spot-checking against real equipment traffic or a
  purchased standard. This does not block authoring or generation.

**Item catalog exception:** E5's Table 3 Data Item Dictionary (lines 398–966
of the local markdown) cross-references items used by S12–S21 messages too
(verified: 93 distinct S12–S21 message references appear in its "Where Used"
column), even though the *message structures* for those streams live
elsewhere. So the full 336-item catalog (336 unique entries, verified by
row count minus repeated page-break header rows) is E5-sourced regardless of
stream — only message-level structure / description / exception for S12–S21
carries the lower-confidence tag.

## Phasing

This document specifies **Phase 1** only:

1. Design and implement the DSL schema (items + messages).
2. Build `tools/gemgen`, a Go code generator invoked via `go generate`.
3. Bulk-extract the **full item catalog** (336 items) from E5 Table 3 into
   `items.yaml` — mechanical, scriptable, fully E5-grounded regardless of
   phase.
4. Author `messages/s1.yaml` (24 stream-specific functions: F1–F24 —
   E5 also defines the universal `S,F0` abort, but that's handled generically
   at the protocol layer, not per-stream, so it's out of scope for `gem/`)
   as the pilot stream — chosen because it exercises the broadest set of DSL
   constructs found anywhere in E5: header-only body (S1F1), per-actor body
   variants (S1F2, S1F14), enum-valued items (COMMACK), open-binding
   (equipment-defined) items, an opaque/form-dependent body (S1F6, S1F8 —
   see `structure: opaque` below), and nested + sibling repeating groups
   (S1F20, S1F24 — see worked example below).

**S1F12 provenance exception:** our local E5 markdown's §10 Message Detail
section skips from F11 to F13 — a page was dropped during PDF→markdown
conversion, not the standard omitting F12. F12 ("Status Variable Namelist
Reply") genuinely exists: Table 3 cross-references it for both `SVNAME` and
`UNITS` (`e005-00-0813.md:862,927`), and it follows the exact same
request/reply-namelist pattern as S1F21/22 and S1F23/24. Its structure
(`L,n{ L,3{ SVID SVNAME UNITS } }`, equipment-only, no reply) is reconstructed
from a third-party reference and entered with `source: external` plus an
inline comment noting *why* — this is a gap in our local copy, not a genuine
discrepancy with the standard, and should be re-verified against a complete
E5 copy if one becomes available.
5. Generate `gem/s1.go`, `gem/s1_test.go`, `gem/items.go`, replacing the
   hand-written `s1.go` / `s1_test.go`.
6. Acceptance: `make lint` and `make test` pass; a sample of generated godoc
   is manually reviewed for readability.

**Non-goal within Phase 1:** E5 defines legacy compatibility structure
variants for a few S1 functions — e.g. S1F3 and S1F10 allow an alternate
"packed single item" form (`<svid1,,svidn>`) for formats `3()`/`5()`,
explicitly retained "for compatibility with previous implementations." E5
itself states new implementations should use the list form
(`L,n{<svid1>...<svidn>}`). The DSL and generator target only the
approved/current structure; the deprecated packed-item form is not
represented and is not a target for codegen.

**Worked example — sibling + nested repeats (S1F20):** proves the `structure`
tree (below) handles E5's hardest S1 shape without a dedicated "sibling
repeat" construct — a list's `items:` array can itself hold nested `list`/
`repeat` nodes alongside plain `item` refs:

```yaml
- function: 20
  name: Attribute Data
  mnemonic: AD
  direction: equipment-to-host
  description: Transfers the requested set of object attributes, in request order.
  exception: >-
    m = 0 means the OBJTYPE is unknown. n = 0 means the object wasn't found.
    A zero-length ATTRDATA means the attribute doesn't exist. p = 0 means no errors.
  source: e5
  bodies:
    - actor: both
      replyExpected: false
      structure:
        type: list
        items:
          - type: list                     # objects: L,m { L,n { ATTRDATA... } }
            repeat: objects
            of: {type: list, repeat: attrs, of: {item: ATTRDATA}}
          - type: list                     # errors: L,p { L,2 { ERRCODE ERRTEXT } }
            repeat: errors
            of: {type: list, items: [{item: ERRCODE}, {item: ERRTEXT}]}
```

**Phase 2** (separate future design/plan, not detailed here): author
`messages/s2.yaml` … `s21.yaml` stream by stream and regenerate/replace the
rest of `gem/`. S1–S10 content comes from E5 §10; S12–S21 comes from each
stream's owning GEM300 standard where verified, or is tagged
`source: external` where reconstructed from a third-party reference instead.

Existing hand-written function signatures are **not** preserved as a
constraint — the v2 branch is pre-1.0 (rc phase), so the generator is free to
pick whatever signature is most consistent across the full generated set.

## DSL schema

### `tools/gemgen/data/items.yaml`

One file, all items, keyed by item name:

```yaml
MDLN:
  formats: [A]            # SECS-II type shortcuts this item may take (secs2.A/B/U1/...)
  binding: fixed           # fixed | open
  goType: string           # required when binding: fixed
  description: Equipment model number, ID length limited to 20 characters.
  source: e5

ALED:
  formats: [B]
  binding: fixed
  goType: byte
  description: Enable/disable alarm, 128 means enable, 0 disable.
  source: e5
  values:                  # optional: generates named Go constants
    - {name: Disable, value: 0}
    - {name: Enable, value: 128}

CEID:
  formats: [A, U1, U2, U4, U8, I1, I2, I4, I8]   # E5 leaves format equipment-defined
  binding: open            # generated parameter stays secs2.Item — caller picks the wire type
  description: Collection event ID.
  source: e5
```

Fields:

- `formats`: list of secs2 shortcut constructors (`A`, `J`, `W`, `B`,
  `BOOLEAN`, `I1`, `I2`, `I4`, `I8`, `U1`, `U2`, `U4`, `U8`, `F4`, `F8`) this
  item's format code (E5 Table 1) maps to (`J` = JIS-8 string, format code
  21; `W` = UTF-8 localized string, format code 22 — see
  `secs2/shortcut.go:11-17`). Multiple entries mean the standard allows more
  than one representation.
- `binding`: `fixed` when E5 mandates exactly one concrete format
  (`len(formats) == 1`, no wildcard) — generated function parameter gets a
  concrete Go type per `goType`. `open` for every other case: format is
  equipment-defined, wildcarded (`3()`, `4()`, `5()`, or `0` for list), *or*
  the standard allows more than one concrete format with no single canonical
  choice. `open` items always get a `secs2.Item` parameter (or
  `...secs2.Item` when repeated), matching the existing hand-written
  convention for CEID, RPTID, ALID, V, etc. Every item referenced by a
  message's `structure` must have an entry in `items.yaml` regardless of
  binding — `binding` only controls the generated parameter type, not
  whether validation requires the item to be defined.
- `goType`: required iff `binding: fixed`.
- `values`: optional enum table. When present (only valid on `binding: fixed`
  items), codegen emits a **named defined type** on top of `goType`, not bare
  constants on the primitive: `type ALED byte` plus
  `const (ALEDDisable ALED = 0; ALEDEnable ALED = 128)`. The generated
  function's parameter for that item takes the named type (e.g.
  `func S5F3(alid ALID, aled ALED) secs2.SECS2Message`), giving autocomplete
  grouping and a mild type-safety boundary exactly where a real enumeration
  exists — items without `values:` keep the bare primitive (`byte`, `string`,
  …) as their parameter type, unchanged.

  **Constructor-call implication (verified against `secs2`):** `secs2.B`,
  `secs2.I1`–`secs2.I8`, and `secs2.U1`–`secs2.U8` accept `...any` but their
  internal value combiners (`secs2/binary.go`'s `combineBinaryValues`,
  `secs2/int.go`'s `combineIntValues`/`combineIntValuesSlow`) type-switch on
  the *exact* primitive type (`case byte:`, `case uint8:`, …) — a named type
  like `ALED` does not match `case byte:` in a Go type switch even though its
  underlying type is `byte`, and falls through to the "invalid type" deferred
  error. So the generated body expression for an enum-typed parameter must
  convert back to the underlying primitive at the call site:
  `secs2.B(byte(aled))`, not `secs2.B(aled)`. This conversion is generated
  automatically — callers never write it themselves.
- `source`: `e5` for every entry in Phase 1 (all items come from Table 3).
- "Used by" cross-references are **not** hand-authored — they're derived at
  doc-generation time from which messages actually reference the item, to
  avoid drift between item docs and message content.

### `tools/gemgen/data/messages/s1.yaml`

```yaml
stream: 1
messages:
  - function: 1
    name: Are You There Request
    mnemonic: R
    direction: bidirectional       # bidirectional | host-to-equipment | equipment-to-host
    description: Establishes if the equipment is on-line.
    exception: "None"
    source: e5
    bodies:
      - actor: both                # both | equipment | host
        replyExpected: true
        structure: null            # header-only body

  - function: 2
    name: On Line Data
    mnemonic: D
    direction: bidirectional
    description: Data signifying that the equipment is alive.
    exception: The host sends a zero-length list to the equipment.
    source: e5
    bodies:
      - actor: equipment            # generates S1F2(mdln, softrev string)
        replyExpected: false
        structure: {type: list, items: [{item: MDLN}, {item: SOFTREV}]}
      - actor: host                 # generates S1F2Host()
        replyExpected: false
        structure: {type: list, items: []}
```

Fields:

- `function`, `name`, `mnemonic`: from E5 §10 (mnemonic is the parenthesized
  abbreviation, e.g. "(R)", "(D)", "(SSR)").
- `direction`: for godoc only; derived from E5's Direction column
  (`S,H<->E` → `bidirectional`, `S,H->E,reply` → `host-to-equipment`, etc.).
- `description`, `exception`: short prose, copied/condensed from E5's
  *Description* / *Exception* sections (or a third-party reference for
  S12–S21 where the owning standard isn't available locally).
- `source`: the owning standard code when verified against real standard
  text (`e5`, `e39`, `e40`, `e42`, `e87`, `e90`, `e94`, `e116`), or
  `external` when reconstructed from a non-standard reference instead.
  `external`-sourced messages get an extra `confidence: low` field and
  generated godoc carries a disclaimer line.
- `bodies`: one entry per distinct wire shape. `actor: both` when sender
  doesn't change the structure (the common case). Two entries
  (`equipment` + `host`) when E5 defines a per-sender difference (S1F2,
  S1F14 today). The `equipment` (or sole) actor generates the bare `SxFy`
  function name; a second `host` actor generates `SxFyHost`, matching
  current naming.
- `structure`: `null` for header-only bodies, otherwise a tree of:
  - `{item: NAME}` — a leaf item reference (must exist in `items.yaml`).
  - `{type: list, items: [...]}` — a nested list. Each element of `items` is
    itself a structure node — a leaf `item` ref, another nested `list`, or a
    `repeat` node — so sibling repeating groups at the same list level (e.g.
    S1F20's parallel attribute-data and error lists) fall out naturally
    without a dedicated construct. See the S1F20 example above.
  - `{type: list, minItems: N, maxItems: M, items: [...]}` — a bounded-arity
    list where the standard allows a small fixed set of lengths rather than
    a single fixed count or an unbounded repeat (e.g. S2F48's `L,p {p = 0,4}`
    sub-list). Phase 1 ships this node type in the schema and generator even
    though the S1 pilot has no message that needs it, since it's a small,
    self-contained addition and closes a known Phase 2 (S2) blocker before
    Phase 2 starts.
  - `{type: list, repeat: <name>, of: <node>}` — a variable-length list of
    repeated shape; codegen emits a variadic parameter named `<name>...`
    (generalizes the existing hand-written `gem.Report()` helper for
    S6F11-style repeating groups, planned for Phase 2).
  - `{type: opaque}` — a body whose shape E5 declares as form-/context-
    dependent rather than statically specifiable (e.g. S1F6 "Depends upon
    the structure specified by the status form," S1F8 "Depends upon the
    form being specified"). Codegen emits a single `secs2.Item` parameter,
    e.g. `func S1F6(body secs2.Item) secs2.SECS2Message`, and the caller
    assembles the form-specific structure manually via `secs2` primitives.

## Generator architecture

```
tools/gemgen/
  main.go          # CLI: go run ./tools/gemgen -items data/items.yaml -messages data/messages -out ./gem
  schema.go        # Go structs mirroring the YAML (Item, Message, Body, StructureNode)
  load.go          # parse + validate
  gen_messages.go  # renders gem/sN.go per stream via text/template
  gen_items.go     # renders gem/items.go: Go types + enum consts for items with `values:`
  gen_tests.go     # renders gem/sN_test.go: header + full body-tree assertion per function
  templates/*.tmpl # go:embed'd templates

tools/gemgen/data/
  items.yaml
  messages/
    s1.yaml         # Phase 1
    s2.yaml … s21.yaml   # Phase 2, added incrementally
```

Validation (fail generation, no partial/silent output, on any of):

- a `{item: X}` reference (any node type, any binding) where `X` is not in
  `items.yaml`
- duplicate `(stream, function)` pair across the loaded message files
- `binding: fixed` item with `len(formats) != 1`, or missing `goType`
- a `bodies` entry with an unrecognized `actor` value
- a `list` node with both `repeat` and `minItems`/`maxItems` set (mutually
  exclusive — unbounded-repeat vs. bounded-arity are different node shapes)

Wired into the `gem` package via a generate directive (e.g. `gem/generate.go`
containing `//go:generate go run ../tools/gemgen ...`), so `go generate
./...` regenerates everything. `tools/gemgen` is its own Go module (its own
`go.mod`, per the sibling `.linter.go.mod` pattern already used for the
linter toolchain) so its `gopkg.in/yaml.v3` dependency never enters the
`gem`-consuming module graph — the root `go.mod`'s "runtime deps intentionally
minimal" rule (100-overview.md) stays intact.

## Godoc format

Matches `.agents/rules/400-documentation.md` (name-first summary line, short
sentences), extended with a body line and an exception line — each its own
paragraph — and, for `external`-sourced entries only, a source disclaimer.
The first line states only the message name and direction; it does not cite
a section number (S12–S21 messages may not have one to cite, and Phase 1
keeps the citation out of the first line for every stream for consistency).
Every paragraph break is a genuine blank `//` line — without it, godoc
renderers join adjacent comment lines into one paragraph, which would merge
Body and Exception into a single run-on line:

```go
// S1F2 creates an S1F2 (On Line Data) message for equipment, direction: bidirectional.
//
// Data signifying that the equipment is alive.
//
// Body: L[2]{ A[mdln] A[softrev] }.
//
// Exception: the host sends a zero-length list to the equipment.
func S1F2(mdln, softrev string) secs2.SECS2Message { ... }
```

The `Body: L[2]{...}` line is informal human-readable shorthand for the
generated godoc comment — not a formal grammar the generator parses or
validates. It's derived directly from the `structure:` tree for display only.

For a `source: external` message, an additional trailing paragraph:

```go
//
// Source: reconstructed from an external reference, not verified against
// the purchased SEMI standard.
```

## Testing strategy

One generated `<stream>_test.go` per stream, table-driven, generalizing the
existing hand-written `assertS1Header` helper: for every function, assert
stream code, function code, wait bit, and a full recursive walk of the body
tree comparing each node's secs2 type *and* value against the value the test
passed into the builder — not just item count. For example, calling
`S1F2("MODEL-X", "2.3.1")` asserts the returned message is
`ListItem[ASCIIItem("MODEL-X"), ASCIIItem("2.3.1")]` in that exact order.
Because the assertion walks the same `structure:` tree the generator used to
build the function, it's fully mechanical (no hand-authored expectations)
while still catching authoring mistakes a type/count-only check would miss —
e.g. two same-typed sibling items swapped, or an item bound to the wrong
DSL entry. Existing repo-wide gates (`make lint`, `make test`) apply
unchanged; no new test category is introduced (`.agents/rules/300-testing.md`).

## Phase 1 acceptance criteria

- `tools/gemgen` (its own Go module) builds and runs via `go generate ./...`.
- `tools/gemgen/data/items.yaml` fully populated (336 items) from E5 Table 3.
- `tools/gemgen/data/messages/s1.yaml` authored (24 stream-specific
  functions: F1–F24) from E5 §10, with F12 tagged `source: external` per the
  provenance exception above.
- The generator correctly renders at least one message of each: header-only
  (S1F1), per-actor variant (S1F2/S1F2Host), enum-valued item (via COMMACK
  in S1F14), open-binding item, opaque body (S1F6, S1F8), and sibling+nested
  repeat (S1F20 or S1F24) — i.e. every `structure` node type is exercised by
  at least one generated function.
- `gem/s1.go`, `gem/s1_test.go`, `gem/items.go` generated, replacing the
  hand-written `s1.go` / `s1_test.go`.
- `make lint` and `make test` pass.
- A sample of generated godoc manually reviewed for readability.

## Non-goals (this document)

- Authoring message content for S2–S21 (Phase 2).
- Preserving today's hand-written function signatures as a compatibility
  constraint.
- Resolving every third-party-reference vs. E5 discrepancy — only flagging
  confidence per message.
- Representing E5's deprecated/legacy compatibility structure variants (e.g.
  S1F3's and S1F10's packed-single-item alternate forms) — the generator
  targets only the structure E5 recommends for new implementations.
- Discriminated/conditional structure where a value depends on a sibling
  item's value (e.g. S2F49/S2F50's parameter-type-dependent shape). This is
  a real gap the S2 stream will require; it's called out here as an
  explicit Phase 2 schema-design task, not silently deferred.
