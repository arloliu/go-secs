# GEM Message Code Generation — Design

Date: 2026-07-07
Status: Phase 1 design (approved for write-up; pending external review)

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
- **S12–S21**: sourced from hume.com (`source: hume`), since there is no other
  available reference; flagged as lower confidence for later spot-checking
  against real equipment traffic or purchased standards. This does not block
  authoring or generation.

**Item catalog exception:** E5's Table 3 Data Item Dictionary cross-references
items used by S12–S21 messages too (verified: Table 3 contains 93 distinct
S12–S21 message references in its "Where Used" column), even though the
*message structures* for those streams live elsewhere. So the full 376-item
catalog is E5-sourced regardless of stream — only message-level structure /
description / exception for S12–S21 carries the lower-confidence tag.

## Phasing

This document specifies **Phase 1** only:

1. Design and implement the DSL schema (items + messages).
2. Build `tools/gemgen`, a Go code generator invoked via `go generate`.
3. Bulk-extract the **full item catalog** (376 items) from E5 Table 3 into
   `items.yaml` — mechanical, scriptable, fully E5-grounded regardless of
   phase.
4. Author `messages/s1.yaml` (24 messages) as the pilot stream — chosen
   because it exercises every DSL construct: header-only body, per-actor body
   variants (S1F2, S1F14), enum-valued items (COMMACK), and open-binding
   (equipment-defined) items.
5. Generate `gem/s1.go`, `gem/s1_test.go`, `gem/items.go`, replacing the
   hand-written `s1.go` / `s1_test.go`.
6. Acceptance: `make lint` and `make test` pass; a sample of generated godoc
   is manually reviewed for readability.

**Phase 2** (separate future design/plan, not detailed here): author
`messages/s2.yaml` … `s21.yaml` stream by stream and regenerate/replace the
rest of `gem/`. S1–S10 content comes from E5 §10; S12–S21 comes from
hume.com, tagged `source: hume`.

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

- `formats`: list of secs2 shortcut constructors (`A`, `B`, `BOOLEAN`, `I1`,
  `I2`, `I4`, `I8`, `U1`, `U2`, `U4`, `U8`, `F4`, `F8`) this item's format code
  (E5 Table 1) maps to. Multiple entries mean the standard allows more than
  one representation.
- `binding`: `fixed` when E5 mandates one exact format (generated function
  parameter gets a concrete Go type per `goType`); `open` when E5 leaves the
  format equipment-defined or wildcarded (`3()`, `4()`, `5()`, or `0` for
  list) — generated parameter stays `secs2.Item` (or `...secs2.Item` when
  repeated), matching the existing hand-written convention for CEID, RPTID,
  ALID, V, etc.
- `goType`: required iff `binding: fixed`.
- `values`: optional enum table. When present, codegen emits named Go
  constants (e.g. `ALEDDisable byte = 0`, `ALEDEnable byte = 128`).
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
  *Description* / *Exception* sections (or hume.com for S12–S21).
- `source`: `e5` or `hume`. `hume`-sourced messages get an extra
  `confidence: low` field and generated godoc carries a disclaimer line.
- `bodies`: one entry per distinct wire shape. `actor: both` when sender
  doesn't change the structure (the common case). Two entries
  (`equipment` + `host`) when E5 defines a per-sender difference (S1F2,
  S1F14 today). The `equipment` (or sole) actor generates the bare `SxFy`
  function name; a second `host` actor generates `SxFyHost`, matching
  current naming.
- `structure`: `null` for header-only bodies, otherwise a tree of:
  - `{item: NAME}` — a leaf item reference (must exist in `items.yaml`).
  - `{type: list, items: [...]}` — a fixed-shape nested list.
  - `{type: list, repeat: <name>, of: <node>}` — a variable-length list of
    repeated shape; codegen emits a variadic parameter named `<name>...`
    (generalizes the existing hand-written `gem.Report()` helper for
    S6F11-style repeating groups, planned for Phase 2).

## Generator architecture

```
tools/gemgen/
  main.go          # CLI: go run ./tools/gemgen -items data/items.yaml -messages data/messages -out ./gem
  schema.go        # Go structs mirroring the YAML (Item, Message, Body, StructureNode)
  load.go          # parse + validate
  gen_messages.go  # renders gem/sN.go per stream via text/template
  gen_items.go     # renders gem/items.go: Go types + enum consts for items with `values:`
  gen_tests.go     # renders gem/sN_test.go: smoke test per function
  templates/*.tmpl # go:embed'd templates

tools/gemgen/data/
  items.yaml
  messages/
    s1.yaml         # Phase 1
    s2.yaml … s21.yaml   # Phase 2, added incrementally
```

Validation (fail generation, no partial/silent output, on any of):

- a `{item: X}` reference where `X` is not in `items.yaml`
- duplicate `(stream, function)` pair across the loaded message files
- `binding: fixed` item missing `goType`
- a `bodies` entry with an unrecognized `actor` value

Wired into the `gem` package via a generate directive (e.g. `gem/generate.go`
containing `//go:generate go run ../tools/gemgen ...`), so `go generate
./...` regenerates everything. `gopkg.in/yaml.v3` is already an indirect
module dependency (pulled in transitively); Phase 1 promotes it to a direct
dependency for `tools/gemgen`.

## Godoc format

Matches `.agents/rules/400-documentation.md` (name-first summary line, short
sentences), extended with a body/exception line and — for `hume`-sourced
entries only — a source disclaimer:

```go
// S1F2 creates an S1F2 (On Line Data) message for equipment (SEMI E5 §10, direction: H<->E).
//
// Data signifying that the equipment is alive.
//
// Body: L[2]{ A[mdln] A[softrev] }.
// Exception: the host sends a zero-length list to the equipment.
func S1F2(mdln, softrev string) secs2.SECS2Message { ... }
```

For a `source: hume` message (Phase 2 only), an additional trailing line:

```go
// Source: hume.com structure dump, not verified against the purchased SEMI standard.
```

## Testing strategy

One generated `<stream>_test.go` per stream, table-driven, generalizing the
existing hand-written `assertS1Header` helper: for every function, assert
stream code, function code, wait bit, and body shape (item type + child
count/format). This gives smoke coverage across the full generated surface
without hand-authoring per-message tests. Existing repo-wide gates
(`make lint`, `make test`) apply unchanged; no new test category is
introduced (`.agents/rules/300-testing.md`).

## Phase 1 acceptance criteria

- `tools/gemgen` builds and runs via `go generate ./...`.
- `tools/gemgen/data/items.yaml` fully populated (376 items) from E5 Table 3.
- `tools/gemgen/data/messages/s1.yaml` authored (24 messages) from E5 §10.
- `gem/s1.go`, `gem/s1_test.go`, `gem/items.go` generated, replacing the
  hand-written `s1.go` / `s1_test.go`.
- `make lint` and `make test` pass.
- A sample of generated godoc manually reviewed for readability.

## Non-goals (this document)

- Authoring message content for S2–S21 (Phase 2).
- Preserving today's hand-written function signatures as a compatibility
  constraint.
- Resolving every hume.com vs. E5 discrepancy — only flagging confidence per
  message.
