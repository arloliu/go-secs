# HSMS-SS concurrent e2e profiling design

## Context

`go-secs` v2 has micro-benchmarks for encode/decode paths in the main module
(`secs2`, `hsms`, `hsmsss`, `secs1`, `sml`) and real end-to-end round-trip
benchmarks in the separate `benchmarks/` module
(`benchmarks/hsmsssdata/{v1,v2}`), which bring up a real active/passive
HSMS-SS pair over loopback TCP. Those e2e benchmarks exist to compare v1 vs
v2 timing/allocs via `benchstat` — they run one synchronous
send-and-wait-for-reply round trip per `b.N` iteration, on a single
connection pair.

Nothing in the repo captures CPU or memory profiles (`pprof`) of v2 under a
realistic, concurrent, sustained e2e workload. This is a gap for ongoing v2
performance work: as v2 evolves (e.g. `gem-codegen`), there's no repeatable
way to see where CPU time or allocations actually go under load resembling
multiple equipment connections each with several transactions in flight.

## Goal

Add a repeatable, low-maintenance way to capture CPU, memory, and contention
(block/mutex) profiles of v2's HSMS-SS connection path under concurrent,
pipelined load, as ongoing perf tooling — not a one-off report, and not a
v1-vs-v2 comparison (v1 is out of scope here; the existing
`bench-v1`/`bench-v2`/`compare` flow already covers that comparison for
timing/allocs).

## Non-goals

- SECS-I profiling (out of scope for this round; HSMS-SS only).
- Automated summary generation from profiles. The Makefile target produces
  raw `.prof` files; analysis is done interactively via `go tool pprof`.
- Runtime-configurable connection/worker counts. Counts are hardcoded
  constants, matching this module's existing style (fixed payload shapes,
  fixed benchmark names) and keeping the tool simple to reason about.
- CI integration or regression-alerting. This is a manually-run developer
  tool for now.
- Automated wall-clock-adequacy tuning. Whether a given `PROFILE_ITERS` run
  lasted long enough for a trustworthy CPU-profile sample count is checked
  manually once during implementation, not enforced by the Makefile target
  itself.

## Design

### Location

New file `benchmarks/hsmsssdata/v2/bench_concurrent_test.go`, in the
existing standalone `benchmarks` Go module (own `go.mod`, `v2` `replace`d to
`../` per the module's existing setup). It is a new benchmark function
alongside the existing four in `bench_test.go`, reusing that file's harness
helpers (`newBenchConn`, `echoHandler`, `waitSelected`, `noopLogger`,
`freeLoopbackPort`) rather than duplicating them.

There is no v1 counterpart for this benchmark — this tooling is v2-only.

### Benchmark: `BenchmarkConnectionPool_ConcurrentRoundTrip`

- Constants: `numConns = 4` connection pairs, `workersPerConn = 8` goroutines
  per connection — 32 total concurrent `SendDataMessage` callers. This
  exercises both connection-scaling (accept path, per-connection state) and
  pipelining (concurrent outstanding transactions on one connection) in a
  single blended workload, avoiding the need for separate profile runs per
  axis.
- Payload: `structuredListItem` (existing fixture from `shapes_test.go`) — a
  mid-complexity shape representative of typical GEM traffic. A single
  representative shape is enough for profiling purposes (unlike the
  benchstat comparison benchmarks, which sweep all four shapes because they
  are diffing behavior *change*, not hunting for hotspots).
- Setup (bind `numConns` fresh loopback ports, bring up each active/passive
  pair, register `echoHandler` on the passive side, wait for
  `SelectedState` on both sides) happens once, outside the timed section,
  matching the existing `benchConnection` pattern.
- Timed section: `b.ReportAllocs()`, `b.ResetTimer()`, then `b.N` total
  round trips are divided evenly across the 32 workers
  (`iterationsPerWorker := b.N / (numConns*workersPerConn)`, minimum 1;
  remainder iterations are dropped). `b.N` (i.e. every `-benchtime=Nx` value
  used to run this benchmark) must be an exact multiple of 32 so this
  division is exact and the benchmark's reported op count matches the
  actual number of round trips performed — the minimum-1 clamp exists only
  to keep ad-hoc runs below `32x` from doing zero work, not to give exact
  accounting in that regime. Each worker is a goroutine that loops
  `iterationsPerWorker` times calling `SendDataMessage` on its assigned
  connection with a fresh `structuredListItem()`.
- Concurrency safety: `SendDataMessage` is safe for concurrent callers on
  the same connection today because of three independent invariants in the
  connection implementation — not because of any locking added in this
  benchmark: System Bytes are drawn from an atomic counter
  (`hsms/sysbytes.go`'s `sysBytesGen.next`, doc'd safe for concurrent use),
  the reply registry is a concurrent `xsync.MapOf` whose `route` delivery is
  a non-blocking send (`hsms/reply_registry.go`), and the actual socket
  write is serialized under the epoch's `writeMu` inside `writeFrame`
  (`hsms/connection_send.go`) — so concurrent callers never interleave
  frames on the wire and never race on reply routing.
- Error handling: `(*testing.B).Fatal` is only safe to call from the
  goroutine running the benchmark function, not from spawned workers. Each
  worker goroutine that hits an error does a non-blocking send of that error
  on a `chan error` buffered to `numConns*workersPerConn` (so no worker ever
  blocks trying to report); after `wg.Wait()`, the main goroutine does a
  non-blocking drain of that channel and calls `b.Fatal` with the first
  error found, if any.
- Teardown: each connection registers its own `b.Cleanup(func() { _ =
  conn.Close() })` immediately after it is successfully created — not a
  single combined `defer` installed after all `numConns` pairs finish
  setup. This bounds the blast radius of a partial setup failure: if pair 2
  of 4 fails to reach `SelectedState` and the benchmark calls `b.Fatal`,
  the connections already created for pairs 0-2 still get closed during
  goroutine unwind, rather than leaking past the failed benchmark.
- Like the existing `hsmsssdata` benchmarks, this must be run with a fixed
  `-benchtime=Nx` rather than the time-based default — Go's calibration
  re-invokes the whole benchmark function (including the out-of-band setup
  of 4 fresh connection pairs) on each calibration step, so a fixed count
  keeps run time predictable. This is documented in `benchmarks/README.md`
  already for the existing benchmarks and the new one follows the same
  rule.

### Profiling workflow

**`bench-v2`/`compare` exclusion.** `bench-v2`'s existing `hsmsssdata/v2`
line uses `-bench=.`, an unanchored regex matching every benchmark in the
package. Adding `BenchmarkConnectionPool_ConcurrentRoundTrip` to that same
package would otherwise get it swept into `make bench-v2` at its
`-benchtime=20x` (not a multiple of 32, reintroducing the op-accounting
mismatch) and into `make compare`'s `benchstat` diff against `v1.txt`,
where no such benchmark exists — directly contradicting the "no v1
counterpart, not a benchstat comparison target" decision above. Fix:
rescope that one line's `-bench=.` to `-bench=BenchmarkConnection_`, a
substring of all four existing benchmark names but not of
`BenchmarkConnectionPool_ConcurrentRoundTrip` (no `_` immediately follows
`Connection` there), so `bench-v2`/`compare` stay limited to exactly the
original four single-connection benchmarks.

New Makefile target in `benchmarks/Makefile`, alongside `bench-v1`/
`bench-v2`/`compare`:

```makefile
PROFILE_ITERS ?= 2048

profile-v2: results
	go test -run=^$$ -bench=BenchmarkConnectionPool_ConcurrentRoundTrip -benchmem \
	  -benchtime=$(PROFILE_ITERS)x -cpuprofile=results/cpu_v2.prof -memprofile=results/mem_v2.prof \
	  -blockprofile=results/block_v2.prof -mutexprofile=results/mutex_v2.prof \
	  ./hsmsssdata/v2/... > results/profile_v2.txt
```

`-blockprofile`/`-mutexprofile` capture contention on the shared
`writeMu`/reply registry under concurrent load — the specific thing this
benchmark's concurrency (vs. the existing single-connection benchmarks) is
positioned to reveal. Neither needs extra runtime setup: passing the flag
alone defaults to full-detail recording (equivalent to
`-blockprofilerate=1`/`-mutexprofilefraction=1`, per `go help testflag`).
The `> results/profile_v2.txt` redirect captures the benchmark's
`ns/op`/`B/op`/`allocs/op` line and the final `ok ... <N>s` wall-clock
summary, matching the existing `bench-v1`/`bench-v2` convention of
redirecting to a results file — giving a lightweight, human-diffable trend
record across runs without building real regression-tracking machinery
(still out of scope, per Non-goals).

`PROFILE_ITERS` is overridable the same way `COUNT` is for `bench-v1`/
`bench-v2` (e.g. `make profile-v2 PROFILE_ITERS=4096`) — keep it a multiple
of 32 (`numConns*workersPerConn`) so the reported op count stays exact.
`results/` is already git-ignored (`benchmarks/.gitignore`), so none of the
`.prof`/`.txt` output ever gets committed — same handling as the existing
text benchmark output.

`benchmarks/README.md` gets a new short section documenting `make
profile-v2` and how to read the output:

```sh
go tool pprof -top results/cpu_v2.prof
go tool pprof -http=:0 results/mem_v2.prof   # browsable flame graph / graph view
go tool pprof -top results/block_v2.prof     # goroutine blocking
go tool pprof -top results/mutex_v2.prof     # contended-mutex stacks
```

No automated summary extraction — raw profiles only, analyzed interactively
per the scope decision above.

### Validation plan (before considering this done)

1. Compile and smoke-test with `-benchtime=32x` (the smallest multiple of
   32, so every worker performs exactly one round trip and the reported op
   count is exact) to confirm the harness wires up correctly (4 connections
   reach `SelectedState`, workers complete, no panics).
2. Run once manually with `-race -benchtime=320x` (10 iterations per
   worker — enough repeated register/write/reply/deregister cycles per
   worker for the race detector to have real signal, not a single-shot
   burst). Not part of the Makefile target — the race detector distorts
   profiling numbers, so this is a one-off correctness check of the new
   concurrent harness code, not a repeatable step.
3. Verify the `bench-v2` regex fix with `go test -list='BenchmarkConnection_'
   ./hsmsssdata/v2/...` (no execution — just lists matching names): must
   show exactly the four existing single-connection benchmarks, never
   `BenchmarkConnectionPool_ConcurrentRoundTrip`.
4. Run `make profile-v2` for real and check the final `ok ... <N>s` line in
   `results/profile_v2.txt` for total wall-clock time — if `<N>` is under
   roughly 1 second, the run was too short for a trustworthy CPU profile at
   Go's default 100 Hz sampling rate; bump `PROFILE_ITERS` to the next
   multiple of 32 large enough to clear ~1s and rerun. One-time manual
   check, not automated Makefile logic (per Non-goals).
5. Spot-check with `go tool pprof -top results/cpu_v2.prof` that the top
   entries are real HSMS/SECS-II hotspots (e.g. encode/decode, frame
   building, reply routing) rather than pure `runtime`/network-syscall
   noise, confirming the profile actually captures useful signal before
   calling the tooling complete.
6. Spot-check `results/block_v2.prof` and `results/mutex_v2.prof` the same
   way with `go tool pprof -top` — a valid, parseable table with near-zero
   samples is an acceptable outcome (low contention at this concurrency
   level is a real finding); only an unparseable/empty-from-wiring-mistake
   file is a failure.
7. `go vet ./hsmsssdata/v2/...` from within `benchmarks/` before committing
   — this standalone module has no `golangci-lint` wiring (same situation
   as `tools/gemgen` before its dedicated `lint-gemgen` target existed), so
   `go vet` is the available pre-commit check here.

## Open questions

None — all scope decisions (protocol, location, load shape, concurrency
model, deliverable format, benchmark structure) were settled during
brainstorming.
