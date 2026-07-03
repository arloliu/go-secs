# Sub-project 4 — internal-pooling decision (benchmark + contract) (design / spec)

**Status:** design approved 2026-06-30, ready for implementation plan.
**Depends on:** sub-project 0 (`docs/v2/01-bakeoff-results.md`), 1 (`secs2`), 2a/2b (`hsms`/`secs1`) — the
v2 immutable message model whose allocation profile this measures. Proposal D7 + §5.F + the SP4 scope line
(`docs/v2/00-v2-proposal.md:184`, `447-456`, `487`).
**Module:** `github.com/arloliu/go-secs/v2`, branch `v2`, Go floor `1.26.0`.

---

## 1. Goal

Settle, with evidence, **how much internal pooling the v2 message model warrants** — the benchmark-gated
question D7 left to this sub-project. The finding (from an escape analysis of every v2 allocation site):
under the locked rule — *pool only transient objects whose lifecycle the library fully controls internally
and that never escape to the caller* — the message-model layers (`secs2`, `internal/wire`, `hsms`, `secs1`,
`sml`) have **essentially no poolable objects**: nearly every allocation either escapes to the caller (Items,
messages, `ToBytes`/`AppendTo` results) or becomes owned by an escaping object (decode's `bytes.Clone` →
`Item.raw`; `assembleBlocks`' coalesce buffer → `wire.AdoptBody` → `Body` → `DataMessage`). The **one**
genuine internal-transient is the `raw := msg.body.AppendTo(nil)` buffer in `hsms.DataMessage.decode`
(`hsms/data_msg.go:300-313`) — immediately re-cloned by `secs2.Decode` (`secs2/decode.go:22`) and then
discarded, so it never escapes. It is a redundant **double-copy**, flagged by 2a as an SP4 item
(`docs/v2/03-…-2a-spec.md`: "a pure-waste `AppendTo(nil)` pre-copy in raw-frame decode … is a
recv-rewrite/SP4 item"). SP4 **measures** it and decides **not to pool** it — pooling a buffer that exists
only because of a redundant copy is the wrong fix; the copy is eliminated in the SP5 recv rewrite (via the
zero-copy decode-owned-frame entry), not by a pool. So SP4 delivers (a) the **codified internal-pooling
contract**, (b) a **perf-baseline benchmark suite** that measures the hot paths (including that raw-frame
decode path) and confirms the no-pooling decision is evidence-based, and (c) the **decision record** that
keeps the timer pool and defers the connection's I/O read buffer (§5.F) **and** the raw-frame double-copy
elimination to sub-project 5 where the recv loop lives. **No production pooling code is added** (the one
transient is a redundant copy to remove in SP5, not a pool candidate).

## 2. Scope (sub-project 4)

**In scope:**
- The **internal-pooling contract** (binding policy, §4), recorded in this spec.
- A **benchmark suite** (durable `*_bench_test.go` perf guards) over the v2 hot paths: `secs2` decode/encode,
  `hsms` frame decode/encode, `secs1` split/assemble/serialize — with `b.ReportAllocs()`. The measured
  numbers go into the decision record (§6).
- The **decision record** (§6): no pooling in the message-model layers; the existing timer pool kept; the
  read-buffer pooling + timer hit-rate validation deferred to SP5.

**Out of scope (later / explicitly not built):**
- **No new pooling code, no `sync.Pool`, no production change** to the message-model layers — there is
  nothing poolable there (everything escapes). The `AppendTo(dst)` API already gives callers zero-extra-alloc
  encoding via their own reused buffer; no internal encode pool is needed or possible under D7.
- **No public API change** — D7 stands (no `Free`/`usePool`/ownership). This sub-project does not resurrect
  any of it.
- **The connection's 128 KB I/O read-buffer pooling** (§5.F option a pool+copy vs option b zero-copy
  GC-owned) and **timer-pool hit-rate validation** → **sub-project 5** (they need a live recv loop; the
  default per §5.F is already option b = not pooled).
- **`sml` text-path benchmarking** — `sml` parse/encode is a human-readable/debug path, not the wire hot
  path; excluded.

## 3. Decisions adopted

| Topic | Decision | Source |
|-------|----------|--------|
| Pooling rule | **Pool only transient objects whose lifecycle the library fully controls internally and that NEVER escape to the caller.** Objects returned to callers are GC-owned — never pooled, never caller-released. | user (locked); D7 |
| Message-model layers | **No pooling.** Escape analysis: nearly every alloc escapes (Items, messages, `ToBytes`/`AppendTo` results) or is owned by an escaping object (decode clone → `Item.raw`; assemble buf → `Body`). Benchmark-confirmed (§5/§6). | escape analysis |
| Raw-frame decode transient | The one internal-transient — `raw := body.AppendTo(nil)` in `DataMessage.decode`, re-cloned by `secs2.Decode` then discarded — is **measured but NOT pooled**: it is a redundant double-copy (2a SP4 item), and the correct fix is **eliminating** the copy in the SP5 recv rewrite (zero-copy decode-owned-frame), not pooling a symptom. | Codex I1; 2a forward note |
| Read buffer | **Deferred to SP5; SP4 narrows the proposal's §5.F assignment.** §5.F originally tasked SP4 with picking pooled-copy (a) vs zero-copy GC-owned (b). SP4 **cannot** run that benchmark — the recv loop does not exist yet (`hsmsss/message_reader.go` is stale pre-SP5 v1 code calling the removed `hsms.DecodeMessage`). So SP4 keeps **option (b)** (per-message GC-owned, not pooled) as the standing default and reassigns the real recv-loop benchmark/switch to SP5. | §5.F (proposal 447-456); Codex I2 |
| v1 pools | **Stay removed (D7).** v1's `DataMessage` pool, 8 `secs2` item pools, public `Free`, and `usePool` global used the caller-managed-release model — the exact source of the double-free / use-after-free / ownership bugs (e.g. commits `6fd2af3`, `9118a9c`; `SystemBytes()` aliasing; `ReplyDataMessage` item-sharing). Not revived. | D7; v1 history |
| Encode buffer | **No internal encode pool.** `Item.AppendTo(dst)` / `DataMessage.ToBytes` already let a caller reuse a GC-owned `dst` for zero-extra-alloc encoding (benchmark §5). The buffer is caller-owned, not a library pool. | v2 API; §5.F |
| Timer pool | **Keep `internal/pool/timer_pool.go`** (`GetTimer`/`PutTimer` over a `sync.Pool` of `*time.Timer`). Timers are created, used, and returned entirely inside the connection and never escape — the canonical sanctioned internal pool. Its hit-rate validation under load is SP5 (needs the recv/timeout loops). | §5.F; existing |
| New internal pools | Any future internal pool requires BOTH a benchmark showing a win AND a proof the pooled object never escapes; otherwise GC owns it. | contract |

## 4. The internal-pooling contract (binding policy)

This is the policy every present and future v2 contributor follows:

1. **Default: GC owns everything.** Immutable objects returned to callers — `secs2.Item`, `hsms.DataMessage`
   / `hsms.ControlMessage`, `internal/wire.Body` / `wire.Chunk`, and any `[]byte` from `ToBytes` /
   `AppendTo` — are **GC-owned**. They are never pooled and never returned to a pool by anyone (no public
   `Free`, no `usePool`, no ownership-transfer contract — D7).
2. **Internal pooling is allowed only when ALL hold:** (a) the object is a *transient* whose entire
   lifecycle (acquire → use → release) happens **inside one library operation or subsystem** the library
   fully controls; (b) the object — and any buffer it backs — **never escapes** to the caller or to a
   longer-lived object that escapes; (c) a benchmark shows a measurable win.
3. **Caller-managed release is forbidden.** The library never asks the caller to release an object back to a
   pool. (That model is the documented source of v1's double-free / use-after-free / ownership bugs.)
4. **Canonical example:** `internal/pool/timer_pool.go` — timers are acquired, armed, drained, and returned
   wholly inside the connection's timeout/retry loops; they never escape. New internal pools are held to the
   same bar.
5. **Caller-controlled buffers are not pooling.** `AppendTo(dst []byte)` lets a caller reuse its **own**
   GC-owned buffer across calls for zero-extra-alloc encoding. That is the v2 answer to "encode buffer
   reuse" — caller-owned, not a library pool.

## 5. Benchmark suite (methodology)

Durable `*_bench_test.go` files, each benchmark calling `b.ReportAllocs()` and resetting the timer after
setup. They establish the alloc/ns baseline AND demonstrate the alloc profile is inherent/escaping (so no
pool applies). Representative, not exhaustive.

- **`secs2`** (`secs2/bench_test.go`):
  - `BenchmarkDecode` for a scalar (`U4` few values), an `ASCII` string, and a nested `L` (list of mixed
    items) — building the wire bytes once in setup, then `secs2.Decode(b)` per iteration. Documents that
    decode allocs are the **returned Item tree** (escape) + the owned `bytes.Clone` held by `Item.raw`.
  - `BenchmarkToBytes` (allocates the result each call) **vs** `BenchmarkAppendTo_ReusedBuffer`
    (`item.AppendTo(buf[:0])` with a reused `buf`) — the latter must show **~0 allocs/op**, proving the
    encode path needs no internal pool (the caller reuses a GC-owned buffer).
- **`hsms`** (`hsms/bench_test.go`):
  - `BenchmarkDecodeHSMSMessage` (decode a full frame) and `BenchmarkDataMessage_ToBytes` /
    `BenchmarkControlMessage_ToBytes` (one inherent frame alloc each, escapes to caller).
  - `BenchmarkDataMessage_Item_RawFrame` — `DecodeHSMSMessage(frame)` then `Item()` (the lazy raw-frame
    decode): documents the **redundant double-copy** (`body.AppendTo(nil)` → `secs2.Decode` clone). This is
    the one internal-transient; the benchmark records its cost as evidence that the right fix is removing the
    copy in SP5, not pooling it.
- **`secs1`** (`secs1/bench_test.go`, **`package secs1`** — `splitBody`/`assembleBlocks`/`block.appendTo`
  are unexported, so the benchmarks are in-package):
  - `BenchmarkSplitBody` (zero-copy iterator — expect ~O(1) allocs, independent of block count),
    `BenchmarkAssembleBlocks` (one inherent coalesce buffer → `Body`), and `BenchmarkBlockAppendTo`
    (serialize a block into a reused `dst` — ~0 extra allocs).

All three bench files are **in-package** (`package secs2`/`hsms`/`secs1`) so they can reach unexported
helpers and pre-sized buffers without widening the public surface.

These run once in the gate (`-benchtime=1x`) to confirm they execute; the implementer captures the actual
`-benchtime` numbers (on this machine) and records them in §6 of this spec / the decision record.

## 6. Decision record (measured 2026-06-30, AMD Ryzen 9 9950X3D, linux/amd64, Go 1.26, -benchtime=5s)

### Measured numbers

| Benchmark | ns/op | B/op | allocs/op |
|-----------|------:|-----:|----------:|
| `secs2.BenchmarkDecode/U4x4` | 41.66 | 136 | 3 |
| `secs2.BenchmarkDecode/ASCII32` | 37.17 | 144 | 3 |
| `secs2.BenchmarkDecode/NestedL` | 175.5 | 608 | 13 |
| `secs2.BenchmarkToBytes` (nested L) | 122.3 | 48 | 1 |
| **`secs2.BenchmarkAppendTo_ReusedBuffer`** | 99.09 | **0** | **0** |
| `hsms.BenchmarkDecodeHSMSMessage` | 50.61 | 168 | 4 |
| `hsms.BenchmarkDataMessage_ToBytes` | 17.61 | 48 | 1 |
| `hsms.BenchmarkControlMessage_ToBytes` | 8.07 | 16 | 1 |
| `hsms.BenchmarkDataMessage_Item_RawFrame` | 173.7 | 512 | 12 |
| `secs1.BenchmarkSplitBody` (3 blocks) | 66.95 | 72 | 3 |
| `secs1.BenchmarkAssembleBlocks` (3 blocks) | 91.83 | 536 | 2 |
| **`secs1.BenchmarkBlockAppendTo`** (reused dst) | 53.56 | **0** | **0** |

### Findings

- **Decode** allocs = the returned Item tree + one owned `bytes.Clone` of the input (the decoder's backing
  buffer, a sub-slice of which each decoded item retains zero-copy). All inherent escaping allocs; no transient
  scratch to pool. U4x4: 3 allocs (UintItem + `[]uint64` values + clone). NestedL: 13 allocs (ListItem + 2
  child ListItems + leaves + clone). Nothing poolable.
- **Raw-frame `DataMessage.Item()`** (`BenchmarkDataMessage_Item_RawFrame`): 12 allocs vs 4 allocs for decode
  alone. The extra allocs come from (a) the redundant `body.AppendTo(nil)` intermediate buffer in
  `DataMessage.decode` (`hsms/data_msg.go:311`) and (b) `secs2.Decode`'s `bytes.Clone` of that buffer.
  This is the **single internal-transient** identified by 2a: a redundant double-copy. Decision: **not pooled**
  — pooling a buffer that only exists because of a redundant copy is the wrong fix; the copy is eliminated in
  the SP5 recv rewrite (zero-copy decode-owned-frame entry). Deferred SP5 item.
- **Encode via `AppendTo(reused dst)`** = **0 allocs/op** (confirmed). `ToBytes` = 1 alloc (result escapes to
  caller). No internal encode pool is needed: the `AppendTo(dst []byte)` API already gives callers
  zero-extra-alloc encoding via their own GC-owned buffer.
- **`secs1` split** = 3 allocs (one closure allocation for the iterator + two block values on heap from the
  range-func mechanism), independent of block count — confirms the zero-copy character. **assemble** = 2 allocs
  (coalesce `[]byte` buf that escapes into the returned `wire.AdoptBody` + the `rawFrameBody` wrapper). Both
  inherent; the result is caller-owned. **block serialize into reused `dst`** = **0 allocs/op** (confirmed).
  Nothing poolable.
- **Conclusion:** **no internal pooling is added to the message-model layers** — every allocation is either
  inherent to a returned immutable object (GC-owned by design, D7) or already avoidable by the caller via
  `AppendTo` buffer reuse. The `internal/pool` timer pool (`internal/pool/timer_pool.go`) is the single
  sanctioned internal pool and is **kept** unchanged. `secs2/pool.go` and `hsms/pool.go` do not exist on v2
  (verified: `find secs2/ hsms/ -name "pool*.go"` returns nothing; `grep -r sync.Pool secs2/ hsms/ secs1/`
  returns nothing). The connection's read-buffer pooling (§5.F option a-vs-b) and timer-pool hit-rate
  validation are **deferred to SP5**, where the recv/timeout loops exist; the standing default is §5.F option
  (b) (per-message GC-owned read buffer, not pooled).

## 7. Success criteria

- **Contract recorded** (§4) — the binding internal-pooling policy is written and unambiguous.
- **Benchmark suite present and green:** the `secs2`/`hsms`/`secs1` `*_bench_test.go` files compile and run
  (`go test -run='^$' -bench=. -benchtime=1x ./secs2/ ./hsms/ ./secs1/` exits 0), with `b.ReportAllocs()` on
  each. The `AppendTo`-into-reused-buffer benchmarks demonstrate **0 allocs/op** (the key evidence that the
  encode path needs no pool).
- **Decision record filled** (§6) with the actual measured numbers and the no-pooling conclusion.
- **No production code / API change:** no `sync.Pool` added to the message-model layers; no public
  `Free`/`usePool`/ownership; the existing `internal/pool/timer_pool.go` is unchanged (kept). `git diff`
  touches only the new `*_bench_test.go` files and docs.
- **Builds in isolation:** `go build`/`vet`/`go test -race`/scoped `golangci-lint` green on
  `./secs2/ ./hsms/ ./secs1/ ./internal/wire/` (+ the new bench files). Module-wide `make ci` stays red until
  SP5/SP7.

## 8. Open implementation details (resolve in the plan)

- **Bench representativeness:** lock the exact item shapes (e.g. `U4[4]`, `A[32]`, `L{A, U4[8], L{...}}`) and
  frame sizes so the baseline is meaningful and stable; keep them small/deterministic so `-benchtime=1x` is a
  valid smoke run in the gate.
- **Where the numbers live:** decide whether the measured figures are embedded in this spec's §6, in a short
  `docs/v2/06-…-results` note, or in benchmark doc-comments — pick one and keep it the single source.
- **`go test -bench` in the gate:** confirm `-benchtime=1x` (one iteration) is the right smoke invocation so
  CI builds+runs the benchmarks without a long benchmarking run; the real measurement is a manual/local
  `-benchtime` capture recorded once.
- **Verify no stale v1 pool code on v2:** confirm the v2 rewrite already removed `secs2/pool.go` and
  `hsms/pool.go` (D7); if any dead pool code lingers in a message-model package, note it (removal is a
  one-liner, but flag rather than silently expand scope).
