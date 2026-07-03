# v2 Sub-Project 0 — Bake-Off Results

**Status:** done. Produced by `internal/bakeoff`. Each verdict unlocks a D2
benchmark-gated decision from `docs/v2/00-v2-proposal.md`.

| Gated decision | Verdict | Evidence |
|----------------|---------|----------|
| SnapshotForRelay methodology reproduced | **CONFIRMED** — snapshot 3–63x faster relay; structural loss reproduced | Task 2 |
| Body representation per type (raw-frame-view vs item-tree), relay + structural | **decoded-frame-of-truth wins** — warm relay equals raw-frame; structural parity; no §5.B revisit | Tasks 3–4 |
| Lazy-decode memoization mechanism (incl. cold-concurrent duplicate decode) | **`sync.Once`** — equal cold cost, 1.7 ns warm, 0.11 ns concurrent, no duplicate decodes | Task 5 |
| Value-vs-pointer `DataMessage` envelope (incl. escaping boxing) | **pointer** — value+pointer are a wash (1 alloc) once interface-carried; pointer preferred for shared-body semantics | Task 6 |
| Concurrent fan-out: one shared body → N restamps, race-free, zero clones | **CONFIRMED** — 18 allocs (goroutine overhead), body-size-independent; race test passes | Task 6 |
| Read-buffer vs zero-copy frame (§5.F) | **OUT OF SCOPE — sub-project 4** | n/a |

---

## Environment

| Key | Value |
|-----|-------|
| `go version` | go1.26.1 linux/amd64 |
| `GOARCH` | amd64 |
| `GOMAXPROCS` | 32 (default — all logical cores) |
| CPU | AMD Ryzen 9 9950X3D 16-Core Processor |
| `benchstat` | available (`golang.org/x/perf/cmd/benchstat`) |

Full suite was run with `-count=6 -benchmem`. Due to the combined runtime exceeding 5 min,
the suite was split into three invocations (Baseline+Envelope+Fanout+Memo-first, then
RawFrame+Tree, then Memo-warm+warmconcurrent+coldconcurrent) and merged for benchstat. All
count=6 samples were captured; no arm was omitted.

---

## Raw benchmark output (benchstat summary, count=6)

```
goos: linux
goarch: amd64
pkg: github.com/arloliu/go-secs/internal/bakeoff
cpu: AMD Ryzen 9 9950X3D 16-Core Processor

                                          sec/op          B/op         allocs/op
Baseline/ascii64k/relay/clone-32         18.94µ ± 24%   222.7Ki ± 0%   12.00 ± 0%
Baseline/ascii64k/relay/snapshot-32       4.937µ ±  5%   72.00Ki ± 0%   1.000 ± 0%
Baseline/ascii64k/structural/clone-32     150.8n ±  9%    364.0 ± 0%    5.000 ± 0%
Baseline/ascii64k/structural/snapshot-32  3.571µ ±  3%   72.34Ki ± 0%   6.000 ± 0%
Baseline/list1000/relay/clone-32          68.21µ ±  3%   174.0Ki ± 0%  2.024k ± 0%
Baseline/list1000/relay/snapshot-32       1.082µ ±  9%    10.00Ki ± 0%  1.000 ± 0%
Baseline/list1000/structural/clone-32     33.86µ ± 16%   101.6Ki ± 0%  1.003k ± 0%
Baseline/list1000/structural/snapshot-32  72.15µ ±  9%   143.9Ki ± 0%  1.007k ± 0%
Baseline/int_scalar/relay/clone-32         99.95n ± 10%   194.5 ± 0%    5.000 ± 0%
Baseline/int_scalar/relay/snapshot-32      41.60n ± 58%   32.00 ± 0%    2.000 ± 0%
Baseline/int_scalar/structural/clone-32    88.88n ± 15%   155.5 ± 1%    3.000 ± 0%
Baseline/int_scalar/structural/snapshot-32 123.0n ±  6%   108.5 ± 2%    4.000 ± 0%

RawFrame/ascii64k/relay-32                4.628n ±  3%     0.000 ± 0%   0.000 ± 0%
RawFrame/ascii64k/structural-32           302.4n ±  2%    305.0 ± 0%    5.000 ± 0%
RawFrame/list1000/relay-32                4.428n ±  1%     0.000 ± 0%   0.000 ± 0%
RawFrame/list1000/structural-32           81.64µ ±  0%  95.11Ki ± 0%  1.004k ± 0%
RawFrame/int_scalar/relay-32              4.429n ±  1%     0.000 ± 0%   0.000 ± 0%
RawFrame/int_scalar/structural-32         136.6n ±  1%    136.0 ± 0%    3.000 ± 0%

Tree/ascii64k/relay/warm-32               4.300n ±  1%     0.000 ± 0%   0.000 ± 0%
Tree/ascii64k/relay/cold-32               5.784µ ± 38%  72.73Ki ± 0%   6.000 ± 0%
Tree/ascii64k/structural/cold-32          298.4n ±  2%    305.0 ± 0%    5.000 ± 0%
Tree/list1000/relay/warm-32               4.278n ±  1%     0.000 ± 0%   0.000 ± 0%
Tree/list1000/relay/cold-32              78.60µ ±  1%  105.2Ki ± 0%  1.005k ± 0%
Tree/list1000/structural/cold-32          83.00µ ±  4%  95.15Ki ± 0%  1.004k ± 0%
Tree/int_scalar/relay/warm-32             4.308n ±  1%     0.000 ± 0%   0.000 ± 0%
Tree/int_scalar/relay/cold-32             170.1n ±  2%    168.0 ± 0%    5.000 ± 0%
Tree/int_scalar/structural/cold-32        130.8n ±  0%    136.0 ± 0%    3.000 ± 0%

Memo/first/once-32                        299.5n ±  4%    304.5 ± 0%    5.000 ± 0%
Memo/first/atomic-32                      303.4n ±  6%    288.5 ± 0%    6.000 ± 0%
Memo/first/mutex-32                       294.9n ±  4%    304.5 ± 0%    5.000 ± 0%
Memo/warm/once-32                         1.714n ±  1%     0.000 ± 0%   0.000 ± 0%
Memo/warm/atomic-32                       1.677n ±  7%     0.000 ± 0%   0.000 ± 0%
Memo/warm/mutex-32                        9.599n ±  1%     0.000 ± 0%   0.000 ± 0%
Memo/warmconcurrent/once-32              0.1086n ±  2%     0.000 ± 0%   0.000 ± 0%
Memo/warmconcurrent/atomic-32            0.1114n ±  5%     0.000 ± 0%   0.000 ± 0%
Memo/warmconcurrent/mutex-32              46.53n ±  5%     0.000 ± 0%   0.000 ± 0%
Memo/coldconcurrent/once-32               3.845µ ±  7%    855.0 ± 0%   17.00 ± 0%
Memo/coldconcurrent/atomic-32             4.074µ ±  9%    903.0 ± 2%   19.00 ± 5%
Memo/coldconcurrent/mutex-32              3.893µ ±  8%    855.0 ± 0%   17.00 ± 0%

Envelope/derive/value-32                  3.208n ±  1%     0.000 ± 0%   0.000 ± 0%
Envelope/derive/pointer-32               12.16n ±  3%    16.00 ± 0%    1.000 ± 0%
Envelope/boxescape/value-32              14.87n ±  2%    16.00 ± 0%    1.000 ± 0%
Envelope/boxescape/pointer-32            12.95n ±  1%    16.00 ± 0%    1.000 ± 0%
Envelope/boxslice/value-32               16.34n ± 18%    16.00 ± 0%    1.000 ± 0%
Envelope/boxslice/pointer-32             15.21n ± 15%    16.00 ± 0%    1.000 ± 0%

Fanout/ascii64k/concurrent-32             1.828µ ±  1%    416.0 ± 0%   18.00 ± 0%
Fanout/ascii64k/serial-32                 66.38n ±  1%     0.000 ± 0%   0.000 ± 0%
Fanout/list1000/concurrent-32             1.833µ ±  2%    416.0 ± 0%   18.00 ± 0%
Fanout/list1000/serial-32                 66.04n ±  1%     0.000 ± 0%   0.000 ± 0%
Fanout/int_scalar/concurrent-32           1.858µ ±  2%    416.0 ± 0%   18.00 ± 0%
Fanout/int_scalar/serial-32               66.16n ±  2%     0.000 ± 0%   0.000 ± 0%
```

`TestMemoColdConcurrentDecodeCounts` (64-goroutine barrier, ascii scalar):
- `memoOnce`: **1** decode — asserted ==1, PASS
- `memoMutex`: **1** decode — asserted ==1, PASS
- `memoAtomic`: **9** decodes — logged (≥1), PASS (duplicated work under CAS race confirmed)

`TestFanoutSharedBodyRaceFree` under `-race`: **PASS**

---

## Verdict details

### V1 — SnapshotForRelay reproduced

| payload | relay: snapshot vs clone | structural: snapshot vs clone |
|---------|--------------------------|-------------------------------|
| ascii64k | 4.94µs vs 18.94µs → 3.8x faster | 3.57µs vs 151ns → 23.7x SLOWER |
| list1000 | 1.08µs vs 68.21µs → 63x faster | 72.15µs vs 33.86µs → 2.1x SLOWER |
| int_scalar | 41.6ns vs 100ns → 2.4x faster (high σ) | 123ns vs 89ns → 1.4x SLOWER |

Result matches the Task 2 directional finding and the known v1 result. The relay-win /
structural-loss asymmetry is confirmed at all three payload scales. Harness validated.

---

### V2 — Body representation per type

Key numbers (representative means, count=6):

| arm | ascii64k | list1000 | int_scalar |
|-----|----------|----------|------------|
| RawFrame/relay | 4.6 ns, 0 allocs | 4.4 ns, 0 allocs | 4.4 ns, 0 allocs |
| Tree/relay/warm | 4.3 ns, 0 allocs | 4.3 ns, 0 allocs | 4.3 ns, 0 allocs |
| Tree/relay/cold | 5.8 µs, 6 allocs | 78.6 µs, 1005 allocs | 170 ns, 5 allocs |
| RawFrame/structural | 302 ns, 5 allocs | 81.6 µs, 1004 allocs | 137 ns, 3 allocs |
| Tree/structural/cold | 298 ns, 5 allocs | 83.0 µs, 1004 allocs | 131 ns, 3 allocs |

**Relay verdict:** The decoded-frame-of-truth (raw-frame provenance, `rawFrameMsg`) reaches
the relay-warm floor (4.4 ns, 0 allocs) identical to `Tree/relay/warm` — both restamp a
14-byte prefix and forward the body sub-slice without copying. The tree cold arm pays
decode+re-encode per op (5.8 µs for ascii64k), which is the forward-received cost without
warm memoization; raw-frame relay avoids this entirely.

**Structural verdict:** `Tree/structural/cold` ≈ `RawFrame/structural` across all three
payloads (±2–4%). No structural regression. The §5.B "decoded-raw-frame path regresses
structural-access arm beyond mitigation" clause is **not triggered**.

**Int scalar note:** The cold relay cost for int_scalar (170 ns, 5 allocs) is inexpensive
relative to large types. For scalars, eager decode at message construction is acceptable
and removes the cold relay decode+re-encode step at the cost of one upfront decode. The
warm relay path (4.3 ns) remains optimal regardless; the choice between eager and lazy
decode for scalar leaves is a per-type leaf-strategy decision for sub-project 1.

**Outcome:** decoded-frame-of-truth wins relay without regressing structural. Hybrid
provenance confirmed viable.

---

### V3 — Lazy-decode memoization mechanism

| scenario | once | atomic | mutex |
|----------|------|--------|-------|
| first decode | 299 ns | 303 ns | 295 ns |
| warm / single | 1.71 ns | 1.68 ns | 9.6 ns |
| warm / concurrent | 0.109 ns | 0.111 ns | 46.5 ns |
| cold / concurrent | 3.85 µs, 17 allocs | 4.07 µs, 19 allocs | 3.89 µs, 17 allocs |
| cold duplicate decodes | **1** | **9** (of 64 goroutines) | **1** |

**Verdict: `sync.Once`.** Rationale:
- Cold-decode cost is equal to mutex and 6% faster than atomic.
- Warm single-threaded (1.71 ns) matches atomic (1.68 ns); mutex is 5.6x slower (9.6 ns).
- Warm concurrent (0.109 ns) matches atomic (0.111 ns); mutex collapses ~428x (46.5 ns).
- **No duplicate large-body decodes**: once fires exactly once under any concurrency —
  critical for 64 KiB or list-of-1000 payloads.
- Atomic saves one word of struct space vs once but duplicates decode work under cold
  concurrent access (9 goroutines in the 64-goroutine barrier test). Not acceptable when
  the body is large.
- Mutex warm path is unacceptable (serializes all hot reads at 46.5 ns each).

---

### V4 — Value-vs-pointer DataMessage envelope

| arm | ns/op | allocs/op | notes |
|-----|-------|-----------|-------|
| derive/value | 3.2 ns | 0 | struct copy stays on stack |
| derive/pointer | 12.2 ns | 1 | `&c` in WithSessionID escapes to heap |
| boxescape/value | 14.9 ns | 1 | boxing value into interface |
| boxescape/pointer | 13.0 ns | 1 | derivation `&c` allocates; pointer-boxing free |
| boxslice/value | 16.3 ns | 1 | value boxed into []msgIface element |
| boxslice/pointer | 15.2 ns | 1 | derivation `&c` allocates |

**Key conditional:** when the v2 `DataMessage` is carried as an interface (e.g., an
`HSMSMessage` in channels, handlers, or router tables — the expected v2 shape), value and
pointer are a **wash on allocation**: value pays 1 alloc for boxing; pointer pays 1 alloc
for the heap `&c` in `WithSessionID`. On latency, pointer was slightly faster in this run
(boxescape 13.0 vs 14.9 ns) — a <2 ns absolute gap that does not change the verdict, which
rests on the equal allocation shape and pointer's shared-body / interface-carriage fit.

**Verdict: pointer receiver.** Prefer pointer because:
1. Interface-carried value and pointer have identical alloc cost (1 alloc/op, both paths).
2. Pointer enables shared-body semantics — N downstreams hold `*DataMessage` pointing at
   the same `*sharedBody`; copying the pointer is free.
3. Value wins *only* if the message is carried concrete-typed end-to-end and derivation
   never touches an interface boundary — not the case for the v2 handler/channel model.

**Sub-project 2 carriage model:** use pointer receivers (`*DataMessage`) and pass
`*DataMessage` through channels and handler signatures.

---

### V5 — Concurrent fan-out

`TestFanoutSharedBodyRaceFree` (32 goroutines, `-race`): **PASS** — confirmed race-clean.

`BenchmarkFanout` (16 goroutines per op):

| payload | concurrent | serial |
|---------|-----------|--------|
| ascii64k | 1.83 µs, 416 B, 18 allocs | 66 ns, 0 B, 0 allocs |
| list1000 | 1.83 µs, 416 B, 18 allocs | 66 ns, 0 B, 0 allocs |
| int_scalar | 1.86 µs, 416 B, 18 allocs | 66 ns, 0 B, 0 allocs |

**Verdict: confirmed.** The fixed 18 allocs/op are goroutine/closure/`sync.WaitGroup`
runtime overhead from spawning 16 goroutines per op (`runConcurrent` uses a `WaitGroup`, no
channel; exact per-source attribution not isolated) — and crucially the count is **identical
across all three payloads, so it is independent of body size**.
Zero clones, zero body copies. The body sub-slice (`frame[14:]`) is read-only shared;
each goroutine writes only its own 14-byte prefix (stack-allocated, no heap escape).
Serial path shows 0 B/op, 0 allocs/op — the 14-byte prefix is confirmed stack-allocated.

---

## Decisions unlocked for D2 / sub-projects 1–2

| Decision | Concrete choice |
|----------|----------------|
| **Per-type leaf strategy (received provenance)** | Decoded-frame-of-truth: hold raw `frame []byte`, lazy-decode via `sync.Once`, relay = restamp 14-byte prefix + `frame[14:]` body subslice. No §5.B revisit needed — structural arm at parity with raw-frame decode. |
| **Per-type leaf strategy (constructed provenance)** | Item-tree + memoized `ToBytes` (`sync.Once`): warm relay reaches the same 4.3 ns floor. Scalar leaves (int_scalar-class) may be eagerly decoded at construction (170 ns cold cost acceptable); large leaves (ascii64k, list1000) must remain lazy. |
| **Memoization mechanism** | `sync.Once` — cheapest warm read (1.7 ns single, 0.11 ns concurrent), no duplicate large-body decodes, equal cold cost to mutex. Adopt for both lazy-decode and lazy-encode (`ToBytes`) memo slots. |
| **Envelope type / carriage model** | Pointer receiver (`*DataMessage`). Interface-carried value and pointer are a 1-alloc wash; pointer is preferred for shared-body semantics. Sub-project 2 channels and handler signatures must use `*DataMessage`. |
| **Fan-out body sharing** | Zero-copy pointer share confirmed race-clean. Sub-project 2 should pass `*DataMessage` (holding a `*sharedBody`) to N goroutines without cloning. No additional locking required beyond `sync.Once` on the decode/encode slots. |
| **§5.B revisit** | **Not triggered.** `Tree/structural/cold` ≈ `RawFrame/structural` within ±4% across all payload types. The hybrid-by-provenance model is approved. |
| **§5.F (read-buffer vs zero-copy frame)** | **OUT OF SCOPE — sub-project 4.** |
