# secs2 round-trip hotspot performance design

## Context

The first run of the HSMS-SS concurrent e2e profiling tooling
(`make -C benchmarks profile-v2`, `BenchmarkConnectionPool_ConcurrentRoundTrip`, results in
`tmp/hsmsss-e2e-pprof-profiling_results-2026-07-10.md`) measured, for a flat
100,000-leaf SECS-II list of I8 items at 32-way concurrency:

```
3324878 ns/op   35255148 B/op   400040 allocs/op
```

with ~40% of CPU samples inside `runtime.mallocgc`/GC-scan machinery. The
workload is allocation-bound. Lock contention (`writeMu`), blocking, and
frame/encode costs were all measured and are **not** problems.

Allocation attribution (corrected against source):

- **Decode side** (~45% of bytes): `secs2.decodeIntItem` heap-allocates one
  `IntItem` struct per decoded leaf (`secs2/decode.go:309/335`). The decoder
  never calls `NewIntItem`. A 100K-leaf list decodes to 100K individual small
  heap objects, twice per round trip (passive side decodes the primary;
  active side decodes the echoed reply) — ~200K decode-side allocations per
  op. The decode path is already zero-copy for payload bytes
  (`DecodeOwnedFrame` aliases the owned frame buffer) and the scalar fast
  path avoids a per-item values slice.
- **Construction side** (~34% of bytes): the benchmark rebuilds the outgoing
  payload every iteration; `structuredListItem` calls `secs2.I8` 100K times,
  which delegates to `NewIntItem` (`secs2/shortcut.go:45-49`). `NewIntItem`
  allocates the item struct plus a temporary `values` slice via
  `combineIntValues` that the scalar case then discards
  (`secs2/int.go:59-63`, `secs2/int.go:305-337`).

### Hotspot 2 — recursive `ListItem.Error()` walk on every message construction

`secs2.(*baseItem).Error` — a one-line getter (`secs2/item.go:356`) — shows
4.72s *flat* CPU (~5%), purely from call volume. `ListItem.Error()`
(`secs2/list.go:144-159`) re-walks **every child** on **every call**, calling
the interface method `v.Error()` and `errors.Join` per child. The Q3
validation gate in `hsms.NewDataMessage` (`hsms/data_msg.go:317`) invokes it
once per constructed message — for the benchmark that is both the outgoing
primary and the echoed reply: 200K+ dynamic calls per round trip confirming
a negative.

Facts established by code inspection:

- Every built-in `secs2.Item` is **deeply immutable after construction**
  (documented on the `Item` interface, `secs2/item.go:152-154`, and on
  `ListItem`, `secs2/list.go:11-15`). No method on any built-in type mutates
  values or error state post-construction; `setError`/`setErrorMsg` are
  called only inside `New*Item` constructors.
- The decoder never produces an item with a non-nil `Error()`: decode
  failures surface as `(nil, error)` returns; `secs2/decode.go` contains zero
  `setError` calls. Decoded trees are error-free by construction.
- The inbound wire path builds `DataMessage` via `newRawFrameDataMessage`
  (`hsms/decode.go:125-137`) and never runs the Q3 gate; the gate runs only
  for user/application-constructed messages.
- The only production `ListItem` composite literals are the constructor
  allocation (`secs2/list.go:35`) and the decoder literal
  (`secs2/decode.go:170`). The SML parser closes parsed lists through
  `NewListItem` (`sml/parser.go:363-380`); GEM helpers use `secs2.L`
  (`gem/report.go:5-11`). `ToList` clones only the child slice, not the
  item (`secs2/list.go:98`).
- **However**, `secs2.Item` is an open exported interface: external
  implementations exist (the repo itself tests one,
  `secs2/equal_test.go:11-25`) and GEM accepts arbitrary caller-provided
  items. External implementations may observe *when* `Error()` is called,
  and a typed-nil built-in child (e.g. `(*IntItem)(nil)`) passes
  `NewListItem`'s `v == nil` filter today and panics only when `Error()` is
  later invoked. Any redesign must not move those observable behaviors.

## Goal

Reduce per-round-trip allocation count and CPU on the profiled hot paths
with **zero observable behavior change** — same error identities, same
`errors.Join`/`Unwrap() []error` topology, same call and panic timing for
external or typed-nil items — and no public API signature change:

1. Make `ListItem.Error()` O(1) for valid built-in trees via a known-clean
   flag (not a cached error object).
2. Batch the decode-side scalar numeric item allocations with per-decode-call
   slabs.
3. Remove `NewIntItem`'s temporary slice allocation on the scalar path.

## Non-goals

- No change to Q3 validation semantics (SEMI E37 §8.3.3.3) — the gate still
  rejects items with deferred errors; only the cost of answering changes.
- No encode/frame-path work (`makeFrame` + `treeBody.encoded` are ~11% of
  bytes — not the bottleneck).
- No lock/contention work (`writeMu` measured clean at 32-way concurrency).
- No `sync.Pool`/explicit-release item lifecycle — items are user-facing and
  immutable with no release hook; pooling without one is unsound.
- No new public constructors or exported identifiers.
- No slab allocation for `ListItem`, multi-value leaves, or pointer-rich
  text/binary/boolean types in this round — their struct pointer fields make
  chunk retention transitive (one retained descendant would pin sibling
  value slices and subtrees). Extending the slab beyond the scalar numeric
  leaves — whose pointer-bearing fields are all nil on the decode branch
  except `rawPtr` — requires a separate retention analysis with
  heap-profile evidence.
- The decoder's per-list `children` slice allocation stays as-is (one
  allocation per list — negligible for the profiled flat list).
- No SECS-I work this round.

## Design

### Part 1 — known-clean fast path for `ListItem.Error()`

A cached *error object* is ruled out: it would stabilize error identity
across calls, flatten the current `errors.Join` nesting (observable via
`Unwrap() []error`), and move external-`Error()` call timing and typed-nil
panics from `Error()` into `NewListItem`. Instead, cache only the *boolean*
"this tree is known error-free":

- `ListItem` gains an unexported flag (e.g. `clean bool`).
- **Decoder literal** (`secs2/decode.go:170`): set `clean: true` — decoded
  trees are error-free by construction.
- **`NewListItem`**: during the existing child loop, determine cleanliness
  with a type-switch over the built-in concrete types only, reading stored
  state directly — never calling the public `Error()` method:
  - built-in non-list types (`*IntItem`, `*UintItem`, `*FloatItem`,
    `*ASCIIItem`, `*JIS8Item`, `*BinaryItem`, `*BooleanItem`,
    `*LocalizedStrItem`, `*EmptyItem`): clean iff the child pointer is
    non-nil and its stored `itemErr` is nil. A **nil pointer** of a built-in
    type marks the list *not clean* (preserving today's panic-at-`Error()`
    timing).
  - `*ListItem`: clean iff non-nil, its own `itemErr` is nil, and its
    `clean` flag is set.
  - any other dynamic type (external implementation): *not clean* —
    uncacheable, so external `Error()` methods keep their exact current
    call timing.
  - the item's own `itemErr` (size-limit path, `secs2/list.go:38-42`) also
    forces *not clean*.
- **`ListItem.Error()`**: `if item.clean { return nil }` — otherwise run the
  existing recursive walk **byte-for-byte unchanged**. Not-clean does not
  mean has-an-error; it means "answer the slow way", so error topology,
  identity-per-call, nil-child skipping, and panic behavior are preserved
  exactly for every uncacheable tree.

Cost model: construction of an all-built-in list replaces (per message
validation, per child) an interface `Error()` dispatch plus an `errors.Join`
call with (once, per child) a type-switch and a field read. Valid built-in
trees — the only trees that reach `SendDataMessage` in practice — answer the
Q3 gate in O(1), and `baseItem.Error` interface dispatch disappears from the
hot path entirely. Note this *moves* a (much cheaper) check into
`NewListItem` rather than deleting all work: the profiled effect must be
verified with a fresh CPU profile, not assumed.

### Part 2 — slab allocation for decoded scalar numeric items

Introduce a small per-decode-call slab allocator inside `secs2` (unexported),
threaded through the `decodeItem` recursion as per-call local state (a
pointer parameter; whether the struct itself stays off-heap is a compiler
outcome, verified by benchmark allocation counts, not asserted):

- Scope: **only the scalar (count==1) paths** of `decodeIntItem`,
  `decodeUintItem`, and `decodeFloatItem` (`secs2/decode.go:294-312`,
  `353-371`, `412-423`). The numeric structs do contain pointer-bearing
  fields (`values` slice, `itemErr` interface, `rawPtr`), but on the scalar
  decode branch all of them are nil except `rawPtr`, which points into the
  wire buffer the item tree already pins. Retaining one element therefore
  pins a GC-scanned chunk plus that same owned wire buffer — no sibling
  value slices or subtrees. Multi-value paths, strings, booleans, binaries,
  and lists keep individual allocation (see Non-goals).
- Chunk growth is **geometric per type**: first chunk 1, then 4, 16, 64,
  capped at 128 (cumulative capacities 1, 5, 21, 85, 213, 341, …). A message
  with one scalar leaf pays for ~one item; a 100K-leaf flat list pays 785
  chunk allocations per decode (85 items in the four growth chunks, then
  `ceil(99915/128) = 781` full chunks) instead of 100K item allocations.
  Chunks are never grown in place — exhaustion allocates a fresh chunk, so
  handed-out pointers stay valid.
- `Decode`/`DecodeOwned` each create one slab state per call; no shared
  state, so concurrency properties are unchanged.

Expected effect: decode-side allocation count for the profiled payload drops
from ~200K/op to ~1,570/op chunk allocations (785 per decode, two decodes
per round trip). Total B/op moves little (same bytes, fewer and larger
objects) — the win is allocation count and `mallocgc`/`memclr` CPU share.
Small-message decode must not regress; the gates in the plan use dedicated
one-scalar-leaf benchmarks (the existing small benchmark shapes contain no
count==1 numeric item and serve only as non-slab controls).

### Part 3 — `NewIntItem` scalar fast path (construction side)

`NewIntItem` funnels all inputs through `combineIntValues`, which builds an
`item.values` slice that the scalar case then copies into `item.scalar` and
discards (`secs2/int.go:59-63`). Add an internal fast path for the
single-plain-integer call shape (the `I8(v)` pattern — one argument, a
built-in integer type) that validates, clamps, and stores directly into
`item.scalar` without the temporary slice. Behavior (clamping, deferred
errors, size-limit check) must match `combineIntValues` exactly for that
shape; all other shapes keep the existing path. This attacks the
construction-side share of the profile with no API change. Measured
independently from Parts 1–2.

## Verification

- Correctness:
  - Full module tests.
  - Behavior-preservation tests for Part 1 (see plan Task 1): repeated-call
    error identity, full recursive `Unwrap() []error` topology, invalid
    child among valid siblings, own-`itemErr` (size-limit) path, empty list,
    nil-child skipping, typed-nil built-in child (panic still at `Error()`
    time), and an external `Item` implementation with a call counter proving
    `Error()` invocation timing is unchanged.
  - Slab chunk-boundary tests for every enabled type at scalar-item counts
    0, 1, 2, 5, 6, 21, 22, 85, 86, 213, and 214 (each pair straddling a
    cumulative chunk capacity), under both `Decode` and `DecodeOwned`,
    asserting distinct values (every child retained and re-read after the
    full decode), `Error() == nil`, and byte-for-byte `ToBytes()`
    round-trip; plus the production raw-frame path via `DataMessage.Item()`
    (`hsms/data_msg.go:382-402`).
  - Fuzz (real targets): `secs2` `FuzzDecode_OwnsBytes`
    (`secs2/decode_test.go`), `hsmsss` `FuzzMessageReader` (success path
    exercises lazy `DataMessage.Item()`) and `FuzzConnectionLifecycle`,
    `integration` `FuzzParityRoundTrip`. Note `make stress-test`/`make
    fuzz-test` do **not** fuzz `secs2` — run
    `go test -run='^$' -fuzz=FuzzDecode_OwnsBytes -fuzztime=60s ./secs2`
    explicitly.
- Guard-test teeth: temporarily disable the clean flag (force not-clean) and
  confirm the new `ListItem.Error` micro-benchmark regresses, per the
  regression-guard practice.
- Performance (same machine, before/after; exact commands, result-file
  names, and baseline-capture ordering are specified in the plan's Task 2
  Step 0/Step 4 and Task 3 Step 0/Step 3):
  - `make -C benchmarks profile-v2`: `allocs/op` on
    `BenchmarkConnectionPool_ConcurrentRoundTrip` drops ≥ 40% (≈400K →
    ≤240K) after Part 2 — hard gate. Part 3's delta measured separately
    against its own constructor-benchmark gate (allocs/op decrease for the
    scalar shape; drop Part 3 if not met).
  - `secs2.(*baseItem).Error` leaves the CPU top table (evidence, verified
    by profile — CPU-share shifts are not pass/fail thresholds since they
    vary run to run; the allocs/op and benchstat gates above are the hard
    gates). Part 1 moves a cheaper check into `NewListItem` — that shift
    must be visible and explained in the results write-up.
  - Small-message gates: new one-scalar-leaf decode benchmarks for each of
    Int/Uint/Float (the shapes that actually hit the slab), plus the
    existing `secs2` `BenchmarkDecode` small cases and
    `benchmarks/hsmsssdata/v2` small-item round trip as non-slab controls.
    Protocol: fixed `-count` (≥10) before and after into named files,
    compared with `benchstat`; gate is zero allocs/op or B/op increase
    anywhere and no statistically significant ns/op regression on the e2e
    small-item control. Bare-decoder micro-benchmark ns/op is evidence, not
    a gate: the fixed ~1ns per-call slab-threading cost (+2–5% on synthetic
    single-item decodes, invisible at e2e scale) was accepted as a
    trade-off by the project owner (2026-07-10).
- Lint: `make lint` clean.

## Compatibility

No public API signature changes and no observable behavior changes:
`ListItem.Error()` returns the identical error values with identical
join topology on every path that can produce a non-nil error, external
`Item` implementations see identical call timing, and slab allocation is
invisible to callers. Safe for a v2 minor/patch release.
