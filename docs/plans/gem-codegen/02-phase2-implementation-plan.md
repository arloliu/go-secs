# Implementation plan: GEM message code-generation, Phase 2 (S2, S5, S6, S9)

- **Status:** Ready for implementation (design approved; see `docs/specs/2026-07-07-gem-codegen-design.md`, "Phase 2 design addendum" section).
- **Date:** 2026-07-07
- **Component:** `github.com/arloliu/go-secs/v2` — extends the existing `tools/gemgen` module + generated `gem/` output from Phase 1.
- **Depends on:** Phase 1 shipped on `v2` (commits `4379157`..`1a5d77c`): `tools/gemgen`'s schema/loader/params/renderers/CLI, the 335-item `items.yaml`, and `gem/s1.go`/`s1_test.go`/`items.go` as the generated precedent. Read `docs/plans/gem-codegen/01-phase1-implementation-plan.md` for the generator's original design rationale if any task references a Phase 1 decision.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

---

## 1. Goal

Replace the hand-written `gem/s2.go`, `gem/s5.go`, `gem/s6.go`, `gem/s9.go` (and their `_test.go`
siblings) with **generated** code, extending the same `tools/gemgen` generator Phase 1 built. Phase 2
delivers: one small generator extension (a byte-array/byte-slice leaf variant for format-`B` items),
an `items.yaml` enum backfill for S2/S5/S6/S9's ack/status-code items, four new message-schema files
(`messages/s2.yaml`, `s5.yaml`, `s6.yaml`, `s9.yaml` — 105 functions total), and the four regenerated
`gem/` files. `make ci` must stay green throughout.

**Not in scope:** `gem/report.go` (its hand-written `Report()` helper is reused by generated
`S6F11`/`S6F16`, unchanged). Streams S3, S4, S7, S8, S10, S12–S21 are later phases.

## 2. Architecture summary

No new files in `tools/gemgen/` itself — Phase 2 extends three existing files and adds new data
files:

```
tools/gemgen/
  load.go                    # MODIFIED: ValidateItems accepts byte-array/byte-slice goType for format B
  params.go                  # MODIFIED: BodyExpr slices a fixed-size byte-array leaf before the constructor call
  gen_tests.go                # MODIFIED: fixedSamples / sample generation handles the new goType shapes
  data/
    items.yaml                # MODIFIED: MHEAD/SHEAD -> [10]byte, ABS -> []byte; ~20 ack/status items gain `values:`
    messages/
      s2.yaml                  # NEW: 50 functions (F1-F50)
      s5.yaml                  # NEW: 18 functions (F1-F18)
      s6.yaml                  # NEW: 30 functions (F1-F30, F13 reconstructed)
      s9.yaml                  # NEW: 7 functions (F1,3,5,7,9,11,13)

gem/
  generate.go                 # MODIFIED: directive comment lists s2/s5/s6/s9 as generated
  s2.go, s5.go, s6.go, s9.go   # REGENERATED (replace hand-written)
  s2_test.go, s5_test.go, s6_test.go, s9_test.go   # NEW, generated
  items.go                    # REGENERATED (new enum constants from the backfill)
  report.go, doc.go            # untouched
```

## 3. Tech stack

Unchanged from Phase 1 — same `tools/gemgen` module (`go 1.26.0`, `gopkg.in/yaml.v3`,
`text/template` + `//go:embed`), same root-module test stack (`testing` + `testify`).

## 4. Global constraints

Carry forward **every** Phase 1 global constraint verbatim (`docs/plans/gem-codegen/01-phase1-implementation-plan.md`
§4, items 1–13: own-module boundary, Go 1.26 floor, `decorder` file layout, godoc template + no
internal jargon, `repeat` vs `packed` typing rules, deterministic output order, `make lint`/`make
test` gates, TDD, no `Co-Authored-By`, the S1F3-style deprecated-form exclusion, enum call-site
conversion, packed-group typing). They apply unchanged to every Phase 2 task. In addition:

14. **Byte-array/byte-slice leaf (new).** A format-`B` item's `goType` may be `byte` (scalar,
    unchanged), `[N]byte` for a fixed-size binary blob (`N` a positive integer literal), or `[]byte`
    for a variable-length binary blob. No other format may use this — E5 format code 10 (`B`) is the
    only one E5 documents as covering both a single accept/deny byte and a multi-byte binary string.
    `BodyExpr` slices a fixed-size array before the constructor call (`secs2.B(mhead[:])`); a `[]byte`
    parameter passes straight through (`secs2.B(abs)`) — verified against the existing hand-written
    `gem/s9.go`. **Never spread `[:]`'s result or a `[]byte` parameter with `...`** — same Phase 1
    lesson as `packed` (constraint 13): `secs2.B` takes `...any`, and spreading a concrete `[]byte`
    into `...any` is a Go compile error, not a runtime one, and is invisible to `gofmt`/`go/parser`
    checks. Any task touching this MUST verify with a real `go build` against `secs2`, not unit
    tests alone.
15. **Legacy/deprecated forms excluded in Phase 2 (S1F3-style, constraint 11's pattern extended):**
    **S2F13** (Equipment Constant Request) and **S2F23** (Trace Initialize Send) each document an
    approved list form and a legacy packed-form alternative "for compatibility with previous
    implementations." Only the approved list form is generated for both; do not add a `packed`
    variant for either.
16. **S6F13 provenance exception (S1F12-style, constraint from Phase 1's non-goal correction):** the
    local E5 copy drops S6F13's body text between F12 and F14. Reconstructed from cross-references
    within the same document (S6F16 says "Identical to structure of S6,F11"; S6F18 says "Same as
    S6,F13"; S6F14 is an Ack needing F13 as its paired primary) as F11's shape with the innermost `V`
    replaced by `L,2{VID, V}` (the same substitution pattern visible in S6F22). Enter with
    `source: external`, `confidence: low`, and an inline YAML comment recording the cross-reference
    chain — same convention as S1F12.
17. **Optional-reply (`[reply]`) messages generate `replyExpected: true` with a godoc note.** E5
    marks S5F1/F3/F9/F11 and S6F1/F3/F9/F11/F25/F27 as sender-optional on the W-bit — a distinction
    S1 never needed. Generate these with `replyExpected: true` (the common case) and add one
    godoc sentence: "Callers that require a no-reply variant should use [secs2.NewMessage] directly."
    — matching the wording the existing hand-written `S2F37`/`S5F1`/`S6F11` already use verbatim.
18. **Enum backfill scope:** every ack/status-code item S2/S5/S6/S9 reference that E5's Table 3
    documents a `N = Label` enumeration for gets a `values:` table in `items.yaml`, using the exact
    same mechanism as `COMMACK`/`ALED` (named defined type, generated constants, call-site
    `<goType>(...)` conversion — constraint 12). An item with an enumeration is not skipped because
    it's "obvious" (e.g. `0 = Accepted` acks) — consistency with `COMMACK`'s precedent is the point.
19. **Domain grounding is mandatory, not optional.** Every message-authoring task below includes a
    classification table (function/name/DSL-kind/items) already verified against the local E5
    markdown, plus the exact source line number for each message's header row. This table is a
    **starting scaffold, not a substitute** for reading the actual Structure/Description/Exception
    text at that line — implementers and reviewers must independently confirm each message's
    structure against `/home/arlo/semi_standards/markdowns/e005-00-0813/e005-00-0813.md` directly.
    Two of the source's tables (S6F27, S6F30) are visibly reflowed by the PDF→markdown conversion
    (numbered sub-items scattered across cells out of order) — cross-check cardinality and item
    order against the surrounding Exception/Description prose for those two specifically, not the
    raw table layout.
20. **`gemgen`'s own gates, every task:** `go tool -modfile=../../.linter.go.mod golangci-lint run
    --config ../../.golangci.yaml ./...` (and `--build-tags integration` variant) from
    `tools/gemgen/`, plus `go test ./...` there, must be clean before `make ci` at the repo root.

---

## Task 1 — byte-array/byte-slice leaf: schema + generator support

**Model/Effort:** opus / high — the exact same class of bug Phase 1's `packed` node caused (a new
goType handled in only one of two parallel tree-walkers, invisible until a real `go build`) is the
central risk here; treat it with the same rigor.

**Files:**
- Modify: `tools/gemgen/load.go`
- Modify: `tools/gemgen/params.go`
- Modify: `tools/gemgen/gen_tests.go`
- Modify: `tools/gemgen/data/items.yaml` (`MHEAD`, `SHEAD`, `ABS` entries only)
- Test: `tools/gemgen/load_items_test.go`, `tools/gemgen/params_test.go`, `tools/gemgen/gen_tests_test.go`
- New: `tools/gemgen/byte_array_compile_integration_test.go` (`//go:build integration`, mirrors
  `packed_compile_integration_test.go`'s pattern exactly)

**Consumes:** Phase 1's `Item`/`StructureNode` schema (unchanged shape — `GoType` is already a plain
string field, so no struct change is needed, only validation/rendering logic). **Produces:** the
`[N]byte`/`[]byte` goType support every other Phase 2 task depends on (S9 entirely; S2F25/F26).

- [ ] **Step 1: write failing tests for the validation relaxation**

  Add to `tools/gemgen/load_items_test.go` (new test function, following that file's existing
  style):
  ```go
  func TestValidateItems_ByteArrayGoType(t *testing.T) {
      items := map[string]Item{
          "MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"},
          "ABS":   {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"},
          "BAD":   {Formats: []string{"A"}, Binding: BindingFixed, GoType: "[10]byte"}, // format A may NOT use this
      }
      require.NoError(t, ValidateItems(map[string]Item{"MHEAD": items["MHEAD"]}))
      require.NoError(t, ValidateItems(map[string]Item{"ABS": items["ABS"]}))
      err := ValidateItems(map[string]Item{"BAD": items["BAD"]})
      require.Error(t, err)
  }
  ```
  Run: `cd tools/gemgen && go test ./... -run TestValidateItems_ByteArrayGoType -v`. Expected: FAIL
  (both `[10]byte` cases currently reject with "goType ... want byte").

- [ ] **Step 2: implement the validation relaxation in `load.go`**

  Add a regexp-matched helper next to `singleFormatGoType`, and use it in `ValidateItems`:
  ```go
  import (
      "fmt"
      "regexp"

      "gopkg.in/yaml.v3"
  )

  // byteArrayGoTypeRE matches a fixed-size byte-array Go type, e.g. "[10]byte".
  var byteArrayGoTypeRE = regexp.MustCompile(`^\[[0-9]+\]byte$`)

  // isByteArrayGoType reports whether gt is a valid byte-array/byte-slice
  // alternative to the scalar "byte" goType, allowed only on a format-B item:
  // E5 format code 10 covers both a single accept/deny byte and a multi-byte
  // binary string (e.g. MHEAD/SHEAD's 10-byte header, ABS's variable-length
  // blob), and the DSL needs a way to say which.
  func isByteArrayGoType(gt string) bool {
      return gt == "[]byte" || byteArrayGoTypeRE.MatchString(gt)
  }
  ```
  In `ValidateItems`, change:
  ```go
  if it.GoType != gt {
      return fmt.Errorf("item %s: goType %q, want %q", name, it.GoType, gt)
  }
  ```
  to:
  ```go
  if it.GoType != gt && !(it.Formats[0] == "B" && isByteArrayGoType(it.GoType)) {
      return fmt.Errorf("item %s: goType %q, want %q (or a byte-array/byte-slice variant for format B)", name, it.GoType, gt)
  }
  ```

- [ ] **Step 3: run the test, confirm it passes**

  Run: `cd tools/gemgen && go test ./... -run TestValidateItems_ByteArrayGoType -v`. Expected: PASS.

- [ ] **Step 4: write a failing test for `BodyExpr`'s array-slicing**

  Add to `tools/gemgen/params_test.go`:
  ```go
  func TestBodyExpr_FixedByteArray(t *testing.T) {
      items := map[string]Item{"MHEAD": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[10]byte"}}
      got := BodyExpr(&StructureNode{Item: "MHEAD"}, items)
      require.Equal(t, "secs2.B(mhead[:])", got)
  }

  func TestBodyExpr_ByteSlice(t *testing.T) {
      items := map[string]Item{"ABS": {Formats: []string{"B"}, Binding: BindingFixed, GoType: "[]byte"}}
      got := BodyExpr(&StructureNode{Item: "ABS"}, items)
      require.Equal(t, "secs2.B(abs)", got)
  }
  ```
  Run: `go test ./... -run TestBodyExpr_FixedByteArray -v` and the `ByteSlice` variant. Expected: FAIL
  (`TestBodyExpr_FixedByteArray` currently renders `secs2.B(mhead)`, no `[:]`).

- [ ] **Step 5: implement the slicing in `params.go`'s `BodyExpr`**

  Change the leaf case from:
  ```go
  case "leaf":
      it := items[n.Item]
      if it.Binding == BindingFixed {
          arg := lower(n.Item)
          if len(it.Values) > 0 {
              arg = fmt.Sprintf("%s(%s)", it.GoType, arg)
          }

          return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
      }

      return lower(n.Item)
  ```
  to:
  ```go
  case "leaf":
      it := items[n.Item]
      if it.Binding == BindingFixed {
          arg := lower(n.Item)
          switch {
          case len(it.Values) > 0:
              // Enum item: convert back to the underlying primitive at the call site (constraint 12).
              arg = fmt.Sprintf("%s(%s)", it.GoType, arg)
          case byteArrayGoTypeRE.MatchString(it.GoType):
              // Fixed-size byte array: secs2.B takes ...any, and a [N]byte does not satisfy its
              // []byte type-switch case directly -- slice it. NEVER add a "..." spread here too;
              // spreading a []byte into ...any is a Go compile error (constraint 14).
              arg = arg + "[:]"
          }

          return fmt.Sprintf("secs2.%s(%s)", it.Formats[0], arg)
      }

      return lower(n.Item)
  ```
  (`[]byte`-typed items need no change to the expression — `arg` already resolves to the bare
  parameter name, which is what `secs2.B(abs)` needs.)

- [ ] **Step 6: run the tests, confirm they pass**

  Run: `go test ./... -run 'TestBodyExpr_FixedByteArray|TestBodyExpr_ByteSlice' -v`. Expected: PASS.

- [ ] **Step 7: extend `gen_tests.go`'s sample generation for the new goTypes**

  This is the step Phase 1's `packed` bug came from (a new goType/kind handled in the renderer but
  missed in the *separate* test-sample walker). `fixedSamples` (`gen_tests.go:44`) is a flat
  `map[string]fixedSample` keyed by exact GoType string, used by both `SampleExprs` (call-site
  argument) and `assertFixed` (decode-and-compare). Its current `scalar bool` field only
  distinguishes "compare directly" (`string`) from "decode returns `[]T`, compare its single
  element" (`byte`, `bool`) — neither shape fits a multi-byte blob, where the decoded value must be
  compared as a *whole sequence*, not one element.

  Add a third comparison mode. Concretely:
  1. Add a `blob bool` field to the `fixedSample` struct (default `false`, unchanged for the 3
     existing entries).
  2. Add a helper that generates a deterministic N-byte sample slice from a name, reusing the
     existing `byteSample` per-byte derivation (e.g. `byteSample(name + strconv.Itoa(i))` for each
     index `i`), and renders it as a Go slice/array literal string for use as both the call-site
     argument and the expected-value comparand.
  3. In `SampleExprs` and `assertFixed`/`assertNode`'s lookup path, handle `it.GoType` values
     matching `byteArrayGoTypeRE` or equal to `"[]byte"` by deriving the sample via that helper
     (parsing `N` out of `[N]byte` for the array case) instead of a static `fixedSamples` map
     lookup — a per-size map entry does not scale if a future item needs a different array length.
  4. In `assertFixed`'s emission, when `blob` is set, compare the *entire* decoded byte sequence
     against the expected sample (e.g. via `bytes.Equal`), not the existing `len(v) != 1 || v[0] !=
     literal` single-element check — that check is correct only for genuinely single-byte
     `fixedSamples["byte"]` and must not be reused for a blob.
  5. Set `testCtx.usesBytes = true` when a blob assertion is emitted (mirrors `assertSample`'s
     existing `bytes.Equal` usage, so the generated test file's `bytes` import stays conditional and
     correct).

  Write the failing test first: add a message fixture with an `{item: MHEAD}` (or a synthetic
  `[10]byte` item) leaf to `tools/gemgen/gen_tests_test.go`, following that file's existing
  `TestRenderTests_*` pattern, asserting the rendered test source contains a full-sequence
  comparison (not a single-element one) and compiles/formats cleanly. Run it, confirm it fails
  against the current code, implement, confirm it passes.

- [ ] **Step 8: update `items.yaml`'s `MHEAD`, `SHEAD`, `ABS` entries**

  ```yaml
  MHEAD:
    formats: [B]
    binding: fixed
    goType: "[10]byte"
    description: SECS message block header associated with message block in error.
    source: e5

  SHEAD:
    formats: [B]
    binding: fixed
    goType: "[10]byte"
    description: Stored header related to the transaction timer.
    source: e5

  ABS:
    formats: [B]
    binding: fixed
    goType: "[]byte"
    description: Any binary string.
    source: e5
  ```
  (Quote the `goType` values — `[10]byte` and `[]byte` both start with `[`, which YAML would
  otherwise try to parse as a flow-sequence.)

- [ ] **Step 9: add the real-compile integration guard**

  Create `tools/gemgen/byte_array_compile_integration_test.go` (`//go:build integration`),
  following `packed_compile_integration_test.go`'s exact structure: build a tiny synthetic
  `MessageFile` with one message using an `{item: MHEAD}` leaf (via a temporary items map entry, or
  by using the real `MHEAD` from `data/items.yaml`), render it with `renderMessages`, write the
  output to a temp file with a real `package main` (or reuse the existing test's approach of writing
  into a scratch module that imports `secs2`), and run `go build` against it. This is the guard that
  would have caught Phase 1's spread bug at this stage instead of three tasks later — it must
  exercise the exact `[N]byte` → `[:]`-sliced call and the `[]byte` passthrough call, not just
  `gofmt`/`go/parser`-validate the generated source.

  Run: `cd tools/gemgen && go test -tags integration ./... -run TestByteArray -v`. Expected: PASS.

- [ ] **Step 10: full gemgen gate + commit**

  Run: `cd tools/gemgen && go tool -modfile=../../.linter.go.mod golangci-lint run --config
  ../../.golangci.yaml ./... && go tool -modfile=../../.linter.go.mod golangci-lint run --config
  ../../.golangci.yaml --build-tags integration ./... && go test ./... && go test -tags integration
  ./...`. Fix any issues. Commit: `feat(gemgen): byte-array/byte-slice leaf for format-B items`.

---

## Task 2 — enum `values:` backfill in `items.yaml`

**Model/Effort:** opus / high — domain-transcription work reading E5 Table 3's free-text Values
column for ~20 items; same care level as Phase 1's `COMMACK` addition (Phase 1 Task 17), scaled up.

**Files:**
- Modify: `tools/gemgen/data/items.yaml`

**Consumes:** Task 1 (independent — no code dependency, but keep Task 1 merged first to keep the
branch's history in dependency order). **Produces:** `values:` tables the Task 4–7 message-authoring
tasks reference by item name.

- [ ] **Step 1: identify every ack/status-code item to backfill**

  Read `/home/arlo/semi_standards/markdowns/e005-00-0813/e005-00-0813.md` Table 3 (Data Item
  Dictionary, lines 398–966) Values column for each of these items (all format `B`, all currently
  bare `byte` in `items.yaml`, all referenced by at least one S2/S5/S6/S9 message in Tasks 4–7's
  classification tables below): `GRANT`, `EAC`, `SPAACK`, `CSAACK`, `DRACK`, `LRACK`, `ERACK`,
  `HCACK`, `VLAACK`, `LVACK`, `LIMITACK`, `RSPACK`, `STRACK`, `TIAACK`, `TIACK`, `RAC`, `RIC`,
  `CMDA`, `CPACK`, `RMACK`, `ACKC5`, `ACKC6`, `ALED` (already has `values:` from Phase 1 — skip),
  `ALCD` (bitfield, not a clean enum — see note below). This list is a starting point derived from
  the Task 4–7 classification tables; if authoring those tasks surfaces another ack/status item with
  a clean `N = Label` Table 3 entry not in this list, backfill it too (constraint 18 — consistency,
  not this list's literal completeness, is the requirement).

  **`ALCD` (S5F1's alarm condition code) is a bitfield, not a plain enum** — E5 describes it as bit
  7 set = alarm set / bit 7 clear = alarm cleared, with bits 0-6 carrying a category code. Do not
  force it into a `values:` table; leave it as bare `byte` with its existing description covering
  the bit-7 semantics (this is a judgment call to record in the commit message, not silently skip).

- [ ] **Step 2: for each item, add a `values:` table following `COMMACK`'s exact pattern**

  Example (verify the real value against Table 3 before using — this is illustrative of the
  mechanism, not a pre-verified answer):
  ```yaml
  GRANT:
    formats: [B]
    binding: fixed
    goType: byte
    description: Grant code, permission to send.
    source: e5
    values:
      - {name: Granted, value: 0}
      - {name: Busy, value: 1}
      - {name: NotInterested, value: 2}
  ```
  Use `Reserved`/numeric-gap handling the same way Phase 1's `COMMACK` did (unused codes are not
  emitted as constants). Where Table 3's Values cell is corrupted/blank for one of these items
  (the `extract_items.py` `OVERRIDES` table already lists any items known to have this problem —
  check it first), ground the values in the item's own description/exception text in the relevant
  message(s) instead, same as the original `OVERRIDES` entries did, and note the reasoning in a
  YAML comment.

- [ ] **Step 3: validate**

  Run: `cd tools/gemgen && go test ./... -run TestDataItemsYAML -v` (the Phase 1 permanent load/
  validate/count guard — item count must stay 335, since this step only adds `values:` to existing
  entries, not new items).

- [ ] **Step 4: commit**

  `feat(gemgen): backfill enum values for S2/S5/S6 ack and status items`.

---

## Task 3 — author `messages/s9.yaml` (7 functions)

**Model/Effort:** opus / high — content-authoring task; smallest and simplest stream, good pilot for
the multi-stream pattern before the larger S5/S6/S2 tasks.

**Files:**
- Create: `tools/gemgen/data/messages/s9.yaml`
- Test: `tools/gemgen/data_messages_test.go` (extend, following the existing `TestDataMessagesS1YAML`
  pattern — a new `TestDataMessagesS9YAML`)

**Consumes:** Task 1 (`MHEAD`/`SHEAD` now `[10]byte`). **Produces:** `messages/s9.yaml`, ready for
Task 8's `go generate`.

**Classification** (verified against `e005-00-0813.md`; all direction `equipment-to-host`, all
`replyExpected: false` except F13 which is `replyExpected: true` per its reply direction marker —
confirm against the source at each cited line before authoring):

| F | Name | Line | Kind | Items |
|---|------|------|------|-------|
| 1 | Unrecognized Device ID | 5063 | fixed leaf | MHEAD |
| 3 | Unrecognized Stream Type | 5075 | fixed leaf | MHEAD |
| 5 | Unrecognized Function Type | 5087 | fixed leaf | MHEAD |
| 7 | Illegal Data | 5101 | fixed leaf | MHEAD |
| 9 | Transaction Timer Timeout | 5115 | fixed leaf | SHEAD |
| 11 | Data Too Long | 5127 | fixed leaf | MHEAD |
| 13 | Conversation Timeout | 5141 | list `{MEXP, EDID}` | MEXP (open), EDID (open) |

`MEXP`/`EDID` are both `binding: open` in `items.yaml` already (verify) — F13's structure is
`{type: list, items: [{item: MEXP}, {item: EDID}]}`, matching the existing hand-written
`gem/s9.go`'s `S9F13(mexp string, edid secs2.Item)` (note: `MEXP` is hand-typed as `string` in the
existing code even though it will validate as `open` in the DSL unless `items.yaml` already declares
it `fixed`/`string` — check `items.yaml`'s current `MEXP` entry first and use whatever it already
says; do not change `MEXP`'s existing binding as part of this task).

F1/F3/F5/F7/F11 (`MHEAD`) and F9 (`SHEAD`) are single-leaf bodies: `structure: {item: MHEAD}` (or
`SHEAD`). None of S9's functions have a paired reply message (all are one-way host-notify — E5
Table 3/§10 confirms no `SxF(y+1)` for any of these).

- [ ] **Step 1: write the file**

  Author `tools/gemgen/data/messages/s9.yaml` with `stream: 9` and all 7 messages, following
  `messages/s1.yaml`'s exact field structure (function/name/mnemonic/direction/description/exception/
  source/bodies). Read each message's Description/Exception text directly at the cited line and
  condense it the same way Phase 1's S1 authoring did — do not copy the classification table's
  one-line item description as a substitute for reading the source.

- [ ] **Step 2: write `TestDataMessagesS9YAML`**

  Mirror `TestDataMessagesS1YAML`'s structure exactly (load real `items.yaml` + the new
  `messages/s9.yaml`, validate, assert exactly 7 functions, assert no F0/F2/F4/F6/F8/F10/F12 gap).

- [ ] **Step 3: run it, confirm it passes**

  Run: `cd tools/gemgen && go test ./... -run TestDataMessagesS9YAML -v`.

- [ ] **Step 4: gate + commit**

  `cd tools/gemgen && go tool -modfile=../../.linter.go.mod golangci-lint run --config
  ../../.golangci.yaml ./... && go test ./...`. Commit: `feat(gemgen): author Stream 9 message DSL (7 messages)`.

---

## Task 4 — author `messages/s5.yaml` (18 functions)

**Model/Effort:** opus / high — content-authoring; second-smallest stream, uniform alarm/exception
request-reply shapes, no legacy duality, no missing items.

**Files:**
- Create: `tools/gemgen/data/messages/s5.yaml`
- Test: `tools/gemgen/data_messages_test.go` (add `TestDataMessagesS5YAML`)

**Consumes:** Task 2 (`ACKC5` enum). **Produces:** `messages/s5.yaml`.

**Classification** (verified; direction column per E5's `S,H<->E`/`S,H->E`/`S,H<-E` markers —
translate to `bidirectional`/`host-to-equipment`/`equipment-to-host` as Phase 1's S1 authoring did):

| F | Name | Line | Kind | Items | Note |
|---|------|------|------|-------|------|
| 1 | Alarm Report Send | 3631 | list `{ALCD, ALID, ALTX}` | ALCD(B), ALID(open), ALTX(A) | `[reply]` — constraint 17 |
| 2 | Alarm Report Acknowledge | 3643 | fixed leaf | ACKC5 |  |
| 3 | Enable/Disable Alarm Send | 3653 | list `{ALED, ALID}` | ALED(enum, Phase 1), ALID(open) | `[reply]` |
| 4 | Enable/Disable Alarm Acknowledge | 3667 | fixed leaf | ACKC5 |  |
| 5 | List Alarms Request | 3677 | repeat | ALID | |
| 6 | List Alarm Data | 3687 | repeat-of-list `{ALCD, ALID, ALTX}` | ALCD, ALID, ALTX | |
| 7 | List Enabled Alarm Request | 3697 | header-only | — | |
| 8 | List Enabled Alarm Data | 3709 | repeat-of-list `{ALCD, ALID, ALTX}` | ALCD, ALID, ALTX | same shape as F6 |
| 9 | Exception Post Notify | 3719 | list, 5 fields incl. trailing repeat | TIMESTAMP, EXID, EXTYPE, EXMESSAGE, EXRECVRA (repeat) | `[reply]` |
| 10 | Exception Post Confirm | 3733 | header-only | — | |
| 11 | Exception Clear Notify | 3743 | list, 4 fields | (verify exact fields at line 3743) | `[reply]` |
| 12 | Exception Clear Confirm | 3755 | header-only | — | |
| 13 | Exception Recover Request | 3765 | list `{EXID, EXRECVRA}` | EXID(open), EXRECVRA(open) | |
| 14 | Exception Recover Acknowledge | 3775 | list, nested ack shape | EXID, ACKA, ERRCODE, ERRTEXT (bounded-list-of-list) | |
| 16 | Exception Recovery Complete Confirm | 3807 | header-only | — | note: F15 does not exist in this DSL — verify at line 3785 whether F15 is genuinely absent or (like S6F13) a dropped-text gap before treating it as absent |
| 17 | Exception Recovery Abort Request | 3817 | fixed leaf | EXID | `[reply]` |
| 18 | Exception Recovery Abort Acknowledge | 3827 | same nested-ack shape as F14 | EXID, ACKA, ERRCODE, ERRTEXT | |

**F15 caveat:** the original Phase 2 survey found "S5,F15 Exception Recovery Complete Notify (EXRCN)
exists with a full Structure section (line 3785)" contradicting the initially-assumed gap — confirm
this directly at line 3785 before authoring, and if present, add it as function 15 (verify its
structure and direction from the source; likely a notify paired with F16's confirm, `[reply]`-style
per constraint 17 if E5 marks it so).

For F1/F3/F9/F11/F17 (marked `[reply]` in E5's Direction column): `replyExpected: true` +
constraint 17's godoc sentence.

- [ ] **Step 1: read every cited line directly** and confirm/correct the table above (it is a
  scaffold — constraint 19). In particular resolve the F15 question and confirm F11/F14/F18's exact
  field lists, which the table above leaves partially unconfirmed pending direct source reading.

- [ ] **Step 2: write the file**, following `messages/s1.yaml`'s field structure.

- [ ] **Step 3: write `TestDataMessagesS5YAML`**, mirroring `TestDataMessagesS1YAML`, asserting the
  final function count (17 or 18, depending on the F15 resolution) and no other gaps.

- [ ] **Step 4: run it, confirm it passes.**

- [ ] **Step 5: gate + commit.** `feat(gemgen): author Stream 5 message DSL`.

---

## Task 5 — author `messages/s6.yaml` (30 functions, incl. F13 reconstruction)

**Model/Effort:** opus / high — largest single-file content task before S2; includes the one
reconstructed message (constraint 16) and the two table-mangled messages (constraint 19).

**Files:**
- Create: `tools/gemgen/data/messages/s6.yaml`
- Test: `tools/gemgen/data_messages_test.go` (add `TestDataMessagesS6YAML`)

**Consumes:** Task 2 (`ACKC6` and other enums). **Produces:** `messages/s6.yaml`.

**Classification** (verified; F13 has no source line — reconstructed per constraint 16):

| F | Name | Line | Kind | Items | Note |
|---|------|------|------|-------|------|
| 1 | Trace Data Send | 3851 | list `{TRID, SMPLN, STIME, repeat SV}` | TRID, SMPLN, STIME, SV | `[reply]` |
| 2 | Trace Data Acknowledge | 3877 | fixed leaf | ACKC6 | |
| 3 | Discrete Variable Data Send | 3889 | list, nested repeat-of-repeat | DATAID, CEID, DSID, repeat{DVNAME, repeat{DVVAL}} — verify exact nesting at line | `[reply]` |
| 4 | Discrete Variable Data Acknowledge | 3922 | fixed leaf | ACKC6 | |
| 5 | Multi-block Data Send Inquire | 3932 | list `{DATAID, DATALENGTH}` | both open | |
| 6 | Multi-block Grant | 3946 | fixed leaf | GRANT | |
| 7 | Data Transfer Request | 3956 | open leaf | DATAID | |
| 8 | Data Transfer Data | 3966 | same shape as F3 | DATAID, CEID, DSID, DVNAME, DVVAL | |
| 9 | Formatted Variable Send | 3978 | list, nested | PFCD, DATAID, CEID, repeat{DSID, repeat{DVVAL}} — verify at line | `[reply]` |
| 10 | Formatted Variable Acknowledge | 3987 | fixed leaf | ACKC6 | |
| 11 | Event Report Send | 3999 | list `{DATAID, CEID, repeat{reports}}` | DATAID, CEID, RPTID, V (see `gem.Report()` in `report.go`) | `[reply]`; canonical `repeat` shape |
| 12 | Event Report Acknowledge | 4026 | fixed leaf | ACKC6 | |
| 13 | Annotated Event Report Send | — | **reconstructed** (constraint 16): F11's shape with innermost `V` → `L,2{VID, V}` | DATAID, CEID, RPTID, VID, V | `source: external, confidence: low` |
| 14 | Annotated Event Report Acknowledge | 4042 | fixed leaf | ACKC6 | |
| 15 | Event Report Request | 4052 | fixed leaf (or small list — verify) | RPTID or CEID | |
| 16 | Event Report Data | 4064 | "Identical to structure of S6,F11" per source | same as F11 | |
| 17 | Annotated Event Report Request | 4074 | verify at line | | |
| 18 | Annotated Event Report Data | 4084 | "Same as S6,F13" per source | same as reconstructed F13 | |
| 19 | Individual Report Request | 4094 | fixed leaf | RPTID | |
| 20 | Individual Report Data | 4106 | repeat | V | |
| 21 | Annotated Individual Report Request | 4117 | fixed leaf | RPTID | |
| 22 | Annotated Individual Report Data | 4127 | repeat-of-list `{VID, V}` | VID, V | |
| 23 | Request Spooled Data | 4145 | fixed leaf | RSDC | |
| 24 | Request Spooled Data Acknowledgement Send | 4157 | fixed leaf | RSDA | |
| 25 | Notification Report Send | 4167 | list, 7+ fields, sibling+nested pattern (verify at line — largest S6 body) | DATAID, OPID, LINKID, RCPSPEC, RMCHGSTAT, RCPATTRID, RCPATTRDATA, RMACK, ERRCODE, ERRTEXT | |
| 26 | Notification Report Send Acknowledge | 4212 | fixed leaf | RMACK (or similar — verify) | |
| 27 | Trace Report Send | 4222 | list, deep nesting | DATAID, TRID, RPTID, V | **table-mangled (constraint 19) — cross-check cardinality against Exception/Description text, not the raw table** |
| 28 | Trace Report Send Acknowledge | 4237 | fixed leaf | TRID (or ack code — verify) | |
| 29 | Trace Report Request | 4247 | fixed leaf | TRID | |
| 30 | Trace Report Data | 4257 | list `{TRID, repeat{RPTID, repeat{V}}, ERRCODE}` (ERRCODE as trailing sibling, not nested — verify) | TRID, RPTID, V, ERRCODE | **table-mangled (constraint 19)** |

- [ ] **Step 1: read every cited line directly** and resolve every "verify at line" / "verify" note
  above before writing YAML — this table intentionally leaves several nested-field orderings
  unconfirmed pending direct source reading (constraint 19), and F27/F30 specifically require
  cross-checking against the Exception/Description prose due to table reflow.

- [ ] **Step 2: write F13's reconstruction with its provenance comment**, e.g.:
  ```yaml
  - function: 13
    name: Annotated Event Report Send
    mnemonic: AERS
    direction: equipment-to-host
    description: >-
      Reports collection event data annotated with variable IDs. Reconstructed: the local E5 copy
      drops F13's body text between F12 and F14. S6F16 ("Event Report Data") states its structure is
      "Identical to structure of S6,F11"; S6F18 ("Annotated Event Report Data") states "Same as
      S6,F13"; S6F14 ("Annotated Event Report Acknowledge") needs F13 as its paired primary. Derived
      as F11's shape with the innermost V replaced by L,2{VID, V} (the same substitution visible in
      S6F22).
    exception: "Not verified against a complete E5 copy; re-check if one becomes available."
    source: external
    confidence: low
    bodies:
      - actor: both
        replyExpected: true
        structure: {type: list, items: [{item: DATAID}, {item: CEID}, {type: list, repeat: reports, of: {type: list, items: [{item: RPTID}, {type: list, repeat: attrs, of: {type: list, items: [{item: VID}, {item: V}]}}]}}]}
  ```
  (Adjust the nesting to F11's actual verified shape once Step 1 confirms it — this is illustrative
  of the provenance-comment convention, not a pre-verified structure.)

- [ ] **Step 3: write the rest of the file**, following `messages/s1.yaml`'s field structure.

- [ ] **Step 4: write `TestDataMessagesS6YAML`**, mirroring `TestDataMessagesS1YAML`, asserting 30
  functions and F13's `source: external`/`confidence: low`.

- [ ] **Step 5: run it, confirm it passes.**

- [ ] **Step 6: gate + commit.** `feat(gemgen): author Stream 6 message DSL, incl. reconstructed F13`.

---

## Task 6 — author `messages/s2.yaml` Part A (F1–F30, 30 functions)

**Model/Effort:** opus / high — largest content task in Phase 2; split into two sequential tasks
purely for review-sizing (constraint: same file, Task 7 appends after this task's review passes).

**Files:**
- Create: `tools/gemgen/data/messages/s2.yaml` (this task creates it with F1–F30; Task 7 appends F31–F50)
- Test: `tools/gemgen/data_messages_test.go` — do NOT add `TestDataMessagesS2YAML` yet (it would fail
  with a partial file); add it in Task 7 once the file is complete.

**Consumes:** Task 1 (`ABS` for F25/26), Task 2 (enums). **Produces:** `messages/s2.yaml` (partial —
F1–F30).

**Classification** (verified; F13/F23 exclude their legacy packed-form alternative per constraint 15):

| F | Name | Line | Kind | Items | Note |
|---|------|------|------|-------|------|
| 1 | Service Program Load Inquire | 1783 | list `{SPID, LENGTH}` | both open | |
| 2 | Service Program Load Grant | 1793 | fixed leaf | GRANT | |
| 3 | Service Program Send | 1803 | open leaf | SPD | |
| 4 | Service Program Send Acknowledge | 1815 | fixed leaf | SPAACK | |
| 5 | Service Program Load Request | 1825 | open leaf | SPID | |
| 6 | Service Program Load Data | 1835 | open leaf | SPD | |
| 7 | Service Program Run Send | 1845 | open leaf | SPID | |
| 8 | Service Program Run Acknowledge | 1855 | fixed leaf | CSAACK | |
| 9 | Service Program Results Request | 1867 | open leaf | SPID | |
| 10 | Service Program Results Data | 1877 | open leaf | SPR | |
| 11 | Service Program Directory Request | 1887 | header-only | — | |
| 12 | Service Program Directory Data | 1897 | repeat | SPID | |
| 13 | Equipment Constant Request | 1910 | repeat | ECID | **exclude legacy packed form (constraint 15)** |
| 14 | Equipment Constant Data | 1923 | repeat | ECV | |
| 15 | New Equipment Constant Send | 1935 | repeat-of-list `{ECID, ECV}` | ECID, ECV(open) | |
| 16 | New Equipment Constant Acknowledge | 1945 | fixed leaf | EAC | |
| 17 | Date and Time Request | 1955 | header-only | — | |
| 18 | Date and Time Data | 1965 | fixed leaf | TIME | |
| 19 | Reset/Initialize Send | 1977 | fixed leaf | RIC | |
| 20 | Reset Acknowledge | 1987 | fixed leaf | RAC | |
| 21 | Remote Command Send | 1997 | open leaf | RCMD | |
| 22 | Remote Command Acknowledge | 2007 | fixed leaf | CMDA | |
| 23 | Trace Initialize Send | 2019 | list `{TRID, DSPER, TOTSMP, REPGSZ, repeat SVID}` | TRID, DSPER, TOTSMP, REPGSZ, SVID | **exclude legacy packed form on the SVID sub-list (constraint 15)** |
| 24 | Trace Initialize Acknowledge | 2058 | fixed leaf | TIAACK | |
| 25 | Loopback Diagnostic Request | 2070 | fixed leaf | ABS | uses Task 1's `[]byte` leaf |
| 26 | Loopback Diagnostic Data | 2080 | fixed leaf | ABS | uses Task 1's `[]byte` leaf |
| 27 | Initiate Processing Request | 2090 | list `{LOC, PPID, repeat MID}` | LOC, PPID (open), MID (open) | |
| 28 | Initiate Processing Acknowledge | 2111 | fixed leaf | CMDA | |
| 29 | Equipment Constant Namelist Request | 2123 | repeat | ECID | |
| 30 | Equipment Constant Namelist | 2133 | repeat-of-list, 6 leaves | ECID, ECNAME, ECMIN, ECMAX, ECDEF, UNITS (verify exact field list/order at line 2133) | |

- [ ] **Step 1: read every cited line directly**, confirm the table above (constraint 19),
  resolving F30's exact field order and any items list not fully specified here.

- [ ] **Step 2: write F1–F30**, following `messages/s1.yaml`'s field structure. For F13 and F23,
  write only the approved list-form structure (constraint 15) and add a one-line comment noting the
  excluded legacy alternative, matching how Phase 1 documented the same exclusion for S1F3.

- [ ] **Step 3: validate structurally** — run a throwaway local check (e.g. a temporary Go test or
  `go run . -items data/items.yaml -messages data/messages -out /tmp/gemgen-check` from
  `tools/gemgen/`, since the full `TestDataMessagesS2YAML` guard isn't added until Task 7) confirming
  the partial file parses, validates, and every `{item: X}` reference resolves.

- [ ] **Step 4: gate + commit.** `feat(gemgen): author Stream 2 message DSL, part 1 (F1-F30)`.

---

## Task 7 — author `messages/s2.yaml` Part B (F31–F50, 20 functions)

**Model/Effort:** opus / high — completes S2; ends with the two messages needing the `opaque`
discriminated-union treatment (F49/F50), which needs the most careful godoc since the DSL can't
statically describe the shape.

**Files:**
- Modify: `tools/gemgen/data/messages/s2.yaml` (append F31–F50 to Task 6's file)
- Test: `tools/gemgen/data_messages_test.go` (add `TestDataMessagesS2YAML`, now that the file is
  complete)

**Consumes:** Task 6 (same file), Task 2 (enums). **Produces:** complete `messages/s2.yaml`.

**Classification** (verified; F45/F46 are the deepest nesting in Phase 2 — 4 levels; F49/F50 are
`opaque` per constraint from the design addendum):

| F | Name | Line | Kind | Items | Note |
|---|------|------|------|-------|------|
| 31 | Date and Time Set Request | 2160 | fixed leaf | TIME | |
| 32 | Date and Time Set Acknowledge | 2172 | fixed leaf | TIACK | |
| 33 | Define Report | 2182 | list, sibling+nested (S1F20-style) | DATAID, repeat{RPTID, repeat{VID}} — verify exact nesting at line | |
| 34 | Define Report Acknowledge | 2218 | fixed leaf | DRACK | |
| 35 | Link Event Report | 2230 | same shape as F33 | DATAID, CEID, repeat{RPTID} — verify | |
| 36 | Link Event Report Acknowledge | 2266 | fixed leaf | LRACK | |
| 37 | Enable/Disable Event Report | 2278 | list `{CEED, repeat CEID}` | CEED(bool), CEID(open) | matches existing hand-written `gem/s2.go`'s `S2F37` — verify signature parity |
| 38 | Enable/Disable Event Report Acknowledge | 2289 | fixed leaf | ERACK | |
| 39 | Multi-block Inquire | 2299 | list `{DATAID, DATALENGTH}` | both open | |
| 40 | Multi-block Grant | 2309 | fixed leaf | GRANT | |
| 41 | Host Command Send | 2321 | list `{RCMD, repeat{CPNAME, CPVAL}}` | RCMD(open), CPNAME(open), CPVAL(open) | |
| 42 | Host Command Acknowledge | 2332 | list `{HCACK, repeat{CPNAME, CPACK}}` | HCACK(enum), CPNAME(open), CPACK(open) | |
| 43 | Reset Spooling Streams and Functions | 2346 | repeat-of-list `{STRID, repeat FCNID}` | STRID(open), FCNID(open) | |
| 44 | Reset Spooling Acknowledge | 2388 | list `{RSPACK, repeat{STRID, STRACK, repeat FCNID}}` | RSPACK, STRID, STRACK, FCNID | verify exact nesting |
| 45 | Define Variable Limit Attributes | 2468 | 4-level nested repeat/bounded-list | DATAID, VID, repeat{LIMITID, bounded-list{0,2}{UPPERDB, LOWERDB}} — verify exact nesting/cardinality at line | deepest S2 nesting |
| 46 | Variable Limit Attribute Acknowledge | 2501 | same depth as F45 | VLAACK, VID, repeat{LVACK, LIMITID, bounded-list{0,2}{LIMITACK}} — verify | |
| 47 | Variable Limit Attribute Request | 2518 | repeat | VID | |
| 48 | Variable Limit Attributes Send | 2543 | repeat-of-list with bounded sub-list | VID, UNITS, bounded-list{0,4}{LIMITMIN,LIMITMAX}, repeat{LIMITID, UPPERDB, LOWERDB} — verify exact shape; this is the `minItems`/`maxItems` node Phase 1 built but never exercised | first real exercise of the `bounded list` DSL kind |
| 49 | Enhanced Remote Command | 2582 | **opaque** | DATAID, OBJSPEC, RCMD, CPNAME, CEPVAL — see note | `CEPVAL`'s shape is value-dependent/recursive; model the whole body as `{type: opaque}` |
| 50 | Enhanced Remote Command Acknowledge | (verify — not separately line-numbered above; search near F49) | **opaque** | HCACK, CPNAME, CEPACK | `CEPACK` mirrors F49's discriminant |

For F49/F50: write `structure: {type: opaque}` (a single `secs2.Item` parameter, per the existing
`{type: opaque}` precedent from S1F6/S1F8) and a godoc description that explains *why* — E5 allows
`CEPVAL` to be a scalar, a list of scalars, or a recursively nested list of `CPNAME`/`CEPVAL` pairs
chosen by the caller at the value level, which no static `structure` tree can express; the caller
assembles the body manually via `secs2` primitives, same as any other opaque message.

- [ ] **Step 1: read every cited line directly**, confirm the table above (constraint 19),
  resolving F44/F45/F46/F48's exact nesting and F50's line number before writing.

- [ ] **Step 2: append F31–F50 to `messages/s2.yaml`**, following `messages/s1.yaml`'s field
  structure. Write F49/F50 as `opaque` with the explanatory description above.

- [ ] **Step 3: write `TestDataMessagesS2YAML`**, mirroring `TestDataMessagesS1YAML`, asserting
  exactly 50 functions (F1–F50, no gaps) and that F49/F50 are `opaque`.

- [ ] **Step 4: run it, confirm it passes.**

- [ ] **Step 5: gate + commit.** `feat(gemgen): author Stream 2 message DSL, part 2 (F31-F50)`.

---

## Task 8 — wire `go:generate`, regenerate, remove hand-written S2/S5/S6/S9

**Model/Effort:** sonnet / medium — mechanical execution once Tasks 1–7 are done, but the diff
review (hand-written vs. generated, for every one of 105 functions) needs care.

**Files:**
- Modify: `gem/generate.go`
- Regenerate: `gem/s2.go`, `gem/s5.go`, `gem/s6.go`, `gem/s9.go`, `gem/items.go`
- New (generated): `gem/s2_test.go`, `gem/s5_test.go`, `gem/s6_test.go`, `gem/s9_test.go`
- Delete: none directly — `go generate` overwrites `gem/s2.go` etc. in place; the hand-written
  `_test.go` files for these streams (if any exist beyond what Phase 1 already replaced) are
  overwritten the same way.

**Consumes:** Tasks 1–7 (all message YAML complete and gated). **Produces:** the actual Phase 2
deliverable.

- [ ] **Step 1: extend `gem/generate.go`'s directive comment**

  Update the comment (not the directive line itself — it already generates every file whose data
  exists in `data/messages/`) to name `s2.go`, `s5.go`, `s6.go`, `s9.go` as also generated, alongside
  `items.go`/`s1.go`.

- [ ] **Step 2: run `go generate`**

  Run: `cd gem && go generate .` (verify the `-C ../tools/gemgen` chdir behavior documented in
  `generate.go`'s comment still lands output in `gem/`, exactly as Phase 1 verified).

- [ ] **Step 3: review the diff for every stream**

  Run: `git diff gem/s2.go gem/s5.go gem/s6.go gem/s9.go gem/items.go`. Confirm: the hand-written
  `gem/s9.go`'s existing 7 functions (`S9F1`...`S9F13`) are replaced by generated equivalents with
  matching signatures (`mhead [10]byte` etc. — Task 1's byte-array leaf should reproduce this
  exactly); the hand-written `gem/s2.go`'s existing 6 functions (`S2F17/18/31/32/37/38`) and
  `gem/s5.go`'s 2 functions (`S5F1/F2`) and `gem/s6.go`'s 2 functions (`S6F11/F12`) are each replaced
  by a generated version with the same or a deliberately-changed signature (constraint: Phase 1
  established that hand-written signatures are not preserved as a hard constraint — but any
  divergence here should be because the DSL's uniform rules produce it, not an authoring accident;
  if a signature changes, confirm it's intentional).

- [ ] **Step 4: full repo gate**

  Run: `make ci`. Fix any issues (lint, `go vet`, `go test`, gemgen's own lint/test/integration).

- [ ] **Step 5: commit**

  `feat(gem): wire go:generate and regenerate S2/S5/S6/S9 via gemgen`.

---

## Task 9 — final whole-branch review

**Model/Effort:** opus / high — same bar as Phase 1's final review; dispatch on the most capable
available model, not the session default.

Dispatch the final code-reviewer subagent (per `superpowers:requesting-code-review`'s
`code-reviewer.md` template) against the full Phase 2 diff (merge-base = the commit this phase
branched from). Give it:
- This plan file and the design spec's "Phase 2 design addendum" section as the global-constraints
  source (§4 above, items 14–20 especially).
- A reminder that Phase 1's `packed`-node bug (a new goType/kind proven only by unit tests, broken
  at real `go build` time) is the specific failure mode Task 1's Step 9 integration test exists to
  catch — the reviewer should confirm that guard actually exercises both the `[N]byte` and `[]byte`
  paths, not just parse-check them.
- A reminder to spot-check at least one message per stream against the actual E5 source directly
  (line numbers are in each task's classification table), not just structurally validate the YAML —
  domain-transcription correctness is not something `go test` can catch.

Resolve all Critical/Important findings via a single fix subagent per the skill's guidance (not one
fixer per finding). Record Minor findings; triage before merge same as Phase 1.

Then use `superpowers:finishing-a-development-branch` — same 4-option menu as Phase 1's finish.
