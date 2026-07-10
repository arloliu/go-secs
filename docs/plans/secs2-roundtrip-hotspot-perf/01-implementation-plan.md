# secs2 Round-Trip Hotspot Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut per-round-trip allocation count and CPU on the profiled hotspots — per-leaf decode allocation, the recursive `ListItem.Error()` walk, and `NewIntItem`'s scalar-path temporary slice — with no public API change and zero observable behavior change.

**Architecture:** Three independent changes inside `secs2`. (1) `ListItem` gains an unexported known-clean flag: set by the decoder literal (decoded trees are error-free) and computed in `NewListItem` via a type-switch over built-in concrete types reading stored error state directly (never calling public `Error()`); `Error()` returns nil for clean trees and runs the existing recursive walk byte-for-byte unchanged otherwise. (2) A per-decode-call slab allocator carves **scalar numeric** item structs (`IntItem`/`UintItem`/`FloatItem`, count==1 paths only — their pointer-bearing fields are nil on that branch except `rawPtr` into the already-pinned wire buffer, so no transitive retention) from geometrically growing chunks (1→4→16→64→128). (3) `NewIntItem` gets a scalar fast path that skips the temporary `combineIntValues` slice for the single-plain-integer call shape.

**Tech Stack:** Go, `go test -benchmem`, `go tool pprof`, `benchstat`, existing `make -C benchmarks profile-v2` (the target lives in the nested `benchmarks/` module's Makefile) and root `make lint` targets.

Design reference: `docs/specs/2026-07-10-secs2-roundtrip-hotspot-perf-design.md`.
Profiling baseline: `tmp/hsmsss-e2e-pprof-profiling_results-2026-07-10.md` (3324878 ns/op, 35255148 B/op, 400040 allocs/op).

## Sub-Agent Assignments

The orchestrating session coordinates and never implements directly. Per-task implementer and reviewer agents use these models/efforts (implementation agents deliberately do not use the orchestrator's model tier):

| Task | Implementer | Spec-compliance reviewer | Code-quality reviewer |
|------|-------------|--------------------------|-----------------------|
| 1 — known-clean flag | sonnet / medium | sonnet / high | sonnet / medium |
| 2 — decode slab | sonnet / high | sonnet / high | sonnet / medium |
| 3 — NewIntItem fast path | sonnet / medium | sonnet / high | sonnet / medium |
| 4 — final validation & write-up | sonnet / medium | — (external post-implementation review instead) | — |

Rationale: Task 2 carries the most intricate pointer/lifetime reasoning, so its implementer runs at high effort. Tasks 1 and 3 are small, tightly specified edits with exhaustive behavior-preservation test lists — medium-effort implementers suffice, with the spec-compliance reviewer at high effort as the safety net. Task 4 executes prescribed commands and writes a report; no per-task reviewers because the whole branch then goes through the external post-implementation review pipeline.

## Global Constraints

- No public API signature changes; no new exported identifiers in `secs2`.
- **Zero observable behavior change** is the bar for Task 1, stronger than `errors.Is`/`errors.As` equivalence: identical error identity per call, identical recursive `Unwrap() []error` topology, identical external-implementation `Error()` call timing, identical typed-nil panic site (`Error()`, not `NewListItem`). Not-clean trees must execute the current walk unchanged.
- The clean-flag computation must never invoke the public `Error()` method on any child, and must treat any non-built-in dynamic type or typed-nil built-in pointer as not-clean.
- Q3 validation in `hsms.NewDataMessage` is untouched — only the cost of `item.Error()` changes.
- Slab scope is exactly the scalar (count==1) numeric decode paths; no `ListItem`, multi-value, string, boolean, or binary slabs in this plan.
- Slab state is strictly per-decode-call (created in `Decode`/`DecodeOwned`, passed down); chunks are never grown in place — exhaustion allocates a fresh chunk.
- Small-message no-regression gates are mandatory (Task 2 Step 4). The gating benchmarks must include shapes that actually hit the slab: new one-scalar-leaf decode benchmarks for Int/Uint/Float. The existing `secs2` `BenchmarkDecode` small cases and `benchmarks/hsmsssdata/v2` small-item round trip contain **no count==1 numeric item** and serve only as non-slab controls. Protocol: fixed `-count` (≥10) before/after into named result files, compared with `benchstat`. Gates: zero allocs/op or B/op increase anywhere, and no statistically significant ns/op regression on the e2e small-item control. Bare-decoder micro-benchmark ns/op is reported as evidence, not a gate: the fixed ~1ns per-`Decode`-call cost of threading the per-call slab state (+2–5% on nanosecond-scale synthetic decodes, invisible at e2e scale) was measured, root-caused, and **accepted as a trade-off by the project owner (2026-07-10)** in exchange for the ~50% allocs/op reduction on large scalar lists.
- No production-code changes outside `secs2` — encode paths, `internal/wire`, `hsms`, and `hsmsss` sources are untouched. Test-only additions under `hsms` (the raw-frame-path test in Task 2 Step 3) are allowed.
- Godoc: no internal work-item codes; public SEMI references only.
- Run `make lint` before every commit.

---

## Task 1: Known-clean fast path for `ListItem.Error()`

**Files:**
- Modify: `secs2/list.go` (`ListItem` struct, `NewListItem`, `ListItem.Error`)
- Modify: `secs2/decode.go` (the `&ListItem{values: children}` literal gains `clean: true`)
- Modify/create: `secs2/list_test.go` additions, list-error micro-benchmark in the package's bench file

**Steps:**

- [ ] **Step 1: Add the flag and compute it in `NewListItem`**
  - Add unexported `clean bool` to `ListItem`.
  - In `NewListItem`'s existing child loop (`secs2/list.go:46-52`), compute cleanliness per the design's type-switch: built-in non-list child → clean iff non-nil pointer and stored `itemErr == nil`; `*ListItem` child → clean iff non-nil, own `itemErr == nil`, and child `clean` set; nil built-in pointer, external dynamic type, or own `itemErr` (size-limit early return) → not clean.
  - `ListItem.Error()`: `if item.clean { return nil }`; the existing walk below stays textually unchanged.
  - Decoder literal (`secs2/decode.go:170`) becomes `&ListItem{values: children, clean: true}` (adjust for actual field layout; `setRaw` unchanged).
- [ ] **Step 2: Behavior-preservation tests**
  - Repeated-call identity (`err1`, `err2` from two `Error()` calls on the same invalid list: assert the *current* relationship, established first against unmodified v2.0.0 behavior) and full recursive `Unwrap() []error` topology for: one invalid child among valid siblings, multiple invalid children, invalid nested grandchild, own-`itemErr` size-limit path.
  - Table test driving **every built-in concrete type** (including `NewEmptyItem()`) through `NewListItem` as a valid child → list is clean, `Error() == nil`.
  - Empty list, nil-child-skipped list, decoded (`Decode` and `DecodeOwned`) tree → `Error() == nil`.
  - Typed-nil built-in child for **every built-in concrete type** (`(*IntItem)(nil)`, `(*UintItem)(nil)`, `(*FloatItem)(nil)`, `(*ASCIIItem)(nil)`, `(*JIS8Item)(nil)`, `(*BinaryItem)(nil)`, `(*BooleanItem)(nil)`, `(*LocalizedStrItem)(nil)`, `(*ListItem)(nil)`, `(*EmptyItem)(nil)`): `NewListItem` must not panic (proving each type-switch arm nil-checks before reading `itemErr`); `Error()` must still panic (assert with recover).
  - External `Item` implementation (test-local type wrapping a call counter): `NewListItem` must not call its `Error()`; each `ListItem.Error()` call must invoke it exactly as today (counter increments per call).
- [ ] **Step 3: Micro-benchmark + guard teeth**
  - `BenchmarkListItemError_Flat100k`: `Error()` on a pre-built valid 100K-child list — expect ~ns and 0 allocs/op. Also benchmark `NewListItem` construction of the same list to quantify the added type-switch cost (this measures where the work *moved*).
  - Teeth check (manual, not committed): force `clean=false` and confirm the `Error()` benchmark regresses by orders of magnitude; revert.
- [ ] **Step 4: Package tests + lint, commit**
  - `go test ./secs2/...`, `make lint`, commit (`perf(secs2): O(1) list error check for known-clean trees`).

## Task 2: Slab-allocate decoded scalar numeric items

**Files:**
- Modify: `secs2/decode.go` (`Decode`, `DecodeOwned`, `decodeItem` signature threading, scalar branches of `decodeIntItem`/`decodeUintItem`/`decodeFloatItem`)
- Create: slab type + helpers (new `secs2/decode_slab.go` preferred)
- Modify/create: decode tests/benchmarks as below
- Test-only addition under `hsms` for the raw-frame-path test (Step 3; allowed by the Global Constraints carve-out)

**Steps:**

- [ ] **Step 0: Capture the pre-change baselines (before any production edit in this task)**
  - New one-scalar-leaf decode benchmarks `BenchmarkDecodeScalarInt`/`BenchmarkDecodeScalarUint`/`BenchmarkDecodeScalarFloat` in `secs2/bench_test.go` (each decodes a single scalar I8/U8/F8 item; these are the shapes that will allocate a slab chunk). Commit the benchmarks first, then capture:
    - `go test -run='^$' -bench='BenchmarkDecode' -benchmem -count=10 ./secs2 | tee tmp/bench-secs2-decode-before.txt`
    - `make -C benchmarks bench-v2 COUNT=10` (nested module's own harness, fixed `-benchtime=20x` per its convention); copy `benchmarks/results/hsmsssdata_v2.txt` to `tmp/bench-hsms-before.txt`
    - `make -C benchmarks profile-v2` once; copy `benchmarks/results/profile_v2.txt` to `tmp/profile-v2-before.txt`
- [ ] **Step 1: Introduce the slab**
  - Unexported struct holding, per type (`IntItem`, `UintItem`, `FloatItem`), the current chunk and cursor. Geometric chunk sizes 1, 4, 16, 64, then 128 repeated (cumulative capacities 1, 5, 21, 85, 213, 341, …); first chunk allocated lazily on first use per type. `nextInt()`/`nextUint()`/`nextFloat()` return a pointer into the current chunk, allocating a fresh chunk on exhaustion (never copy-grow — handed-out pointers must stay valid).
  - Code comment documenting the retention model precisely: a retained scalar item pins its GC-scanned chunk (≤128 structs) plus the owned wire buffer the tree already pins via `rawPtr`. The structs' pointer-bearing fields (`values`, `itemErr`) are nil on the scalar decode branch, so no sibling value slices or subtrees are retained.
- [ ] **Step 2: Thread the slab through the decode recursion**
  - `Decode`/`DecodeOwned` create one slab state per call (per-call local, passed by pointer down `decodeItem`).
  - Replace `&IntItem{...}`/`&UintItem{...}`/`&FloatItem{...}` **only in the count==1 scalar branches** (`secs2/decode.go:294-312`, `353-371`, `412-423`) with slab-carved structs; field assignments and `setRaw` calls preserved exactly. Multi-value branches untouched.
- [ ] **Step 3: Correctness verification**
  - Chunk-boundary tests for each of the three types: an outer list holding scalar-leaf counts 0 (empty list — exercises list decode only), 1, 2, 5, 6, 21, 22, 85, 86, 213, and 214 — each pair straddling a cumulative chunk capacity (fills at 1, 5, 21, 85, 213; fresh chunk first needed at 2, 6, 22, 86, 214) — under **both** `Decode` and `DecodeOwned`: every child retained and re-read after the full decode with distinct values at the last slot of one chunk and first slot of the next, `Error() == nil`, byte-for-byte `ToBytes()` round-trip against the input.
  - Zero-payload numeric items are a **separate** case from the empty list: an encoded I/U/F header with zero payload bytes takes the existing multi-value path (`count == 0` bypasses the scalar branch), and empty *input* returns `EmptyItem` before `decodeItem` runs (`secs2/decode.go:31-39`, `secs2/decode.go:60-67`). Test each of Int/Uint/Float with a zero-payload item under both `Decode` and `DecodeOwned`: assert the concrete numeric type (not `EmptyItem`), `Size() == 0`, nil error, byte-for-byte round-trip. This guards against an accidental `count <= 1` slab condition.
  - Production raw-frame path test in `hsms/decode_owned_test.go` (test-only addition; the existing fixture there is ASCII-only and never hits a numeric slab): a frame whose body contains at least one scalar Int, one scalar Uint, and one scalar Float value, decoded via `DataMessage.Item()` (`hsms/data_msg.go:382-402`, reaches `DecodeOwnedFrame`), asserting the decoded values.
  - Full `go test ./secs2/...`; fuzz the decoder explicitly: `go test -run='^$' -fuzz=FuzzDecode_OwnsBytes -fuzztime=60s ./secs2` (note: `make stress-test`/`make fuzz-test` do not fuzz `secs2`).
- [ ] **Step 4: Measure against the Step 0 baselines**
  - `go test -run='^$' -bench='BenchmarkDecode' -benchmem -count=10 ./secs2 | tee tmp/bench-secs2-decode-after.txt`, then `benchstat tmp/bench-secs2-decode-before.txt tmp/bench-secs2-decode-after.txt`. Gate (scalar benchmarks): allocs/op strictly decreases for the multi-scalar shapes and does not increase for the one-leaf shapes; B/op does not increase; no statistically significant ns/op regression. Gate (non-slab `BenchmarkDecode` cases, as controls): no change beyond benchstat significance.
  - `make -C benchmarks bench-v2 COUNT=10`; copy `benchmarks/results/hsmsssdata_v2.txt` to `tmp/bench-hsms-after.txt`; `benchstat tmp/bench-hsms-before.txt tmp/bench-hsms-after.txt`. Gate: no allocs/op or B/op increase and no statistically significant ns/op regression on `BenchmarkConnection_SmallItem_RoundTrip`.
  - `make -C benchmarks profile-v2`; compare against `tmp/profile-v2-before.txt`. Gate: `allocs/op` ≤ 240K (from 400040; the slab replaces ~200K decode-side item allocations with ~1,570 chunk allocations per op — 785 per decode, two decodes per round trip). Evidence (not a hard gate): visibly smaller `mallocgc`/GC share in the CPU top table.
- [ ] **Step 5: Lint, commit**
  - `make lint`, commit (`perf(secs2): slab-allocate decoded scalar numeric items`).

## Task 3: `NewIntItem` scalar fast path

**Files:**
- Modify: `secs2/int.go` (`NewIntItem`; new unexported helper next to `combineIntValues`)
- Modify: `secs2/int_test.go` additions

**Steps:**

- [ ] **Step 0: Capture the pre-change constructor baseline (after Task 2 landed, before this task's edits)**
  - `go test -run='^$' -bench='BenchmarkNewIntItem' -benchmem -count=10 ./secs2 | tee tmp/bench-newintitem-before.txt` (add the constructor benchmark first if none exists, commit it, then capture).
- [ ] **Step 1: Implement the fast path**
  - The existing byte-size validation early return (`secs2/int.go:47-51`) stays **before** the fast path — invalid widths must keep today's deferred-error behavior untouched.
  - Detection is an exact ten-case type switch on the single argument (`len(values)==1`): `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`, `uint16`, `uint32`, `uint64` — the exact set the current conversion switches accept (`secs2/int.go:301-371`, `374-475`). **Every other dynamic type** (slices, strings, `uintptr`, user-defined integer types, etc.) falls through to `combineIntValues` unchanged. No reflection-based integer classification.
  - The matched case stores the clamped value directly into `item.scalar`/`size=1` without building `item.values` (`secs2/int.go:59-63`). Clamping must be byte-identical in outcome, including comparing high `uint`/`uint64` values before conversion exactly as today (`secs2/int.go:413-447`). The size-limit outcome is necessarily unchanged for scalars (≤ 8 payload bytes).
- [ ] **Step 2: Equivalence tests**
  - Table test over the ten exact scalar types, with in-range and **every applicable** clamp case: signed types get low and high clamps; unsigned types have no low-clamp case; high `uint`/`uint64` compared before conversion. Fast path result (`ToInt`, `Size`, `Error`, `ToBytes`) identical to a slice-shaped call (`NewIntItem(sz, []T{v})`) that takes the slow path.
  - Negative classification tests asserting the slow path still handles them identically to current behavior: slices, strings, multi-argument calls, `uintptr(1)`, a test-local `type myInt int` value, and every invalid byte size (0, 3, 5, 7, 9).
- [ ] **Step 3: Measure against the Step 0 baseline**
  - `go test -run='^$' -bench='BenchmarkNewIntItem' -benchmem -count=10 ./secs2 | tee tmp/bench-newintitem-after.txt`; `benchstat tmp/bench-newintitem-before.txt tmp/bench-newintitem-after.txt`. Gate: allocs/op decreases for the scalar shape and no shape regresses significantly; if the gate fails, drop this task rather than tune further.
  - `make -C benchmarks profile-v2` once to record this task's separate contribution vs Task 2's `tmp/profile-v2-before.txt`/post-Task-2 numbers (evidence, not a gate).
- [ ] **Step 4: Lint, commit**
  - `make lint`, commit (`perf(secs2): skip temp slice for scalar int construction`).

## Task 4: Final validation and results write-up

**Files:**
- Create: `tmp/secs2-roundtrip-hotspot-perf_results-<date>.md`

**Steps:**

- [ ] **Step 1: Full-suite pass** — module tests, `make lint`, `make stress-test`, plus these exact fuzz commands (note `make stress-test`/`make fuzz-test` cover none of the first and not `secs2`):
  - `go test -run='^$' -fuzz=FuzzDecode_OwnsBytes -fuzztime=60s ./secs2`
  - `go test -run='^$' -fuzz=FuzzMessageReader -fuzztime=60s ./hsmsss`
  - `go test -run='^$' -fuzz=FuzzConnectionLifecycle -fuzztime=60s ./hsmsss`
  - `go test -run='^$' -fuzz=FuzzParityRoundTrip -fuzztime=60s ./integration`
- [ ] **Step 2: Before/after profile comparison** — re-run `make -C benchmarks profile-v2` (same machine, same `PROFILE_ITERS`); capture ns/op, B/op, allocs/op deltas and the new CPU top table against `tmp/profile-v2-before.txt`; confirm the design's success criteria: allocs/op ≤ 240K after Task 2, `secs2.(*baseItem).Error` gone from the top table with the moved `NewListItem` check cost visible and acceptable, small-message gates clean.
- [ ] **Step 3: Write results report** in `tmp/`, mirroring the baseline report's structure; note deferred leads (slab coverage for multi-value/pointer-rich types with a retention study, typed no-boxing constructors) as future work.
