# HSMS-SS Concurrent e2e Profiling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a repeatable way to capture CPU/memory pprof profiles of v2's HSMS-SS connection path under concurrent, pipelined load, in the standalone `benchmarks/` module.

**Architecture:** One new benchmark function (`BenchmarkConnectionPool_ConcurrentRoundTrip`) in `benchmarks/hsmsssdata/v2` drives 4 connection pairs × 8 concurrent workers each against a `structuredListItem` payload, reusing the existing single-pair benchmark's harness helpers. A new `profile-v2` Makefile target runs it with `-cpuprofile`/`-memprofile`/`-blockprofile`/`-mutexprofile` (the latter two to catch contention on the shared `writeMu`/reply registry under concurrent load), and fixes a latent collision where the existing `bench-v2` target's `-bench=.` wildcard would otherwise sweep this new benchmark into the v1-vs-v2 comparison flow.

**Tech Stack:** Go `testing` package (`testing.B`), `go tool pprof`, the existing `benchmarks` standalone Go module (own `go.mod`, `v2` replaced to `../`).

Design reference: `docs/specs/2026-07-10-hsmsss-e2e-pprof-profiling-design.md`.

## Global Constraints

- HSMS-SS only — no SECS-I in this round.
- Lives entirely in the standalone `benchmarks/` module — no changes to the main module.
- No v1 counterpart for this benchmark — this is v2-only profiling tooling, not a benchstat comparison target.
- Connection count (4) and workers-per-connection (8) are hardcoded constants — not env/flag-configurable.
- Payload is `structuredListItem` only (one blended workload, not a per-shape sweep).
- Must be run with a fixed `-benchtime=Nx`, not the time-based default (existing `hsmsssdata` convention — setup cost sits outside the timed loop and repeats on every calibration step). `Nx` must also be an exact multiple of the 32 concurrent workers (`profileNumConns * profileWorkersPerConn`) so the benchmark's reported op count matches the actual number of round trips performed — see Task 1 Step 1's `iterationsPerWorker` note.
- No automated pprof summary generation — the Makefile target produces raw `.prof` files; analysis is manual via `go tool pprof`.
- `results/` is already git-ignored (`benchmarks/.gitignore`) — no new gitignore entries needed.
- The new benchmark must never be swept into `make bench-v2`/`make compare` — `bench-v2`'s `hsmsssdata` line is rescoped from `-bench=.` to `-bench=BenchmarkConnection_` for exactly this reason (Task 2 Step 1).
- Wall-clock adequacy for profiling (is the run long enough for a trustworthy sample count) is a one-time manual check performed during implementation — not automated Makefile logic.

---

## Task 1: Add the concurrent round-trip benchmark

**Files:**
- Create: `benchmarks/hsmsssdata/v2/bench_concurrent_test.go`

**Interfaces:**
- Consumes (from `benchmarks/hsmsssdata/v2/bench_test.go`, same package `v2`, no import needed): `freeLoopbackPort(b *testing.B) int`, `newBenchConn(port int, active bool) (hsms.Connection, error)`, `echoHandler(msg *hsms.DataMessage, ep hsms.SECS2Endpoint)`, `waitSelected(b *testing.B, conn hsms.Connection)`.
- Consumes (from `benchmarks/hsmsssdata/v2/shapes_test.go`, same package): `structuredListItem() secs2.Item`.
- Produces: `BenchmarkConnectionPool_ConcurrentRoundTrip(b *testing.B)`, runnable via `go test -bench=BenchmarkConnectionPool_ConcurrentRoundTrip` — consumed by Task 2's Makefile target.

- [ ] **Step 1: Write the benchmark file**

```go
// Package v2 — see bench_concurrent_test.go's sibling bench_test.go for the
// single-connection round-trip benchmarks this file's harness helpers come
// from.
package v2

import (
	"context"
	"sync"
	"testing"

	"github.com/arloliu/go-secs/v2/hsms"
)

const (
	profileNumConns       = 4
	profileWorkersPerConn = 8
)

type profileConnPair struct {
	active  hsms.Connection
	passive hsms.Connection
}

// BenchmarkConnectionPool_ConcurrentRoundTrip drives profileNumConns
// active/passive HSMS-SS connection pairs, each with profileWorkersPerConn
// goroutines concurrently issuing synchronous SendDataMessage round trips on
// the SAME connection. This is safe today on three independent invariants in
// the connection implementation, not because of any locking added here:
// System Bytes are drawn from an atomic counter (hsms/sysbytes.go's
// sysBytesGen.next, doc'd safe for concurrent use), the reply registry is a
// concurrent xsync.MapOf whose route delivery is a non-blocking send
// (hsms/reply_registry.go), and the actual socket write is serialized under
// the epoch's writeMu inside writeFrame (hsms/connection_send.go) — so
// concurrent callers never interleave frames on the wire and never race on
// reply routing.
//
// This is profiling tooling (see `make profile-v2` in this module's
// Makefile), not a benchstat comparison target — there is no v1
// counterpart.
func BenchmarkConnectionPool_ConcurrentRoundTrip(b *testing.B) {
	ctx := context.Background()

	pairs := make([]profileConnPair, profileNumConns)
	for i := range pairs {
		port := freeLoopbackPort(b)

		passiveConn, err := newBenchConn(port, false)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = passiveConn.Close() })

		activeConn, err := newBenchConn(port, true)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = activeConn.Close() })

		passiveConn.AddDataMessageHandler(echoHandler)

		if err := passiveConn.Open(ctx, hsms.OpenBackground); err != nil {
			b.Fatal(err)
		}
		if err := activeConn.Open(ctx, hsms.OpenBackground); err != nil {
			b.Fatal(err)
		}

		waitSelected(b, passiveConn)
		waitSelected(b, activeConn)

		pairs[i] = profileConnPair{active: activeConn, passive: passiveConn}
	}

	const totalWorkers = profileNumConns * profileWorkersPerConn

	// b.N is expected to be an exact multiple of totalWorkers (32) — every
	// caller-supplied -benchtime=Nx in this plan is chosen that way — so this
	// division is exact and every worker performs the same number of round
	// trips. The floor-to-1 clamp only guards ad-hoc runs below 32x; it is
	// not meant to produce an exact accounting in that regime.
	iterationsPerWorker := b.N / totalWorkers
	if iterationsPerWorker < 1 {
		iterationsPerWorker = 1
	}

	errCh := make(chan error, totalWorkers)

	b.ReportAllocs()
	b.ResetTimer()

	var wg sync.WaitGroup
	for i := 0; i < totalWorkers; i++ {
		conn := pairs[i/profileWorkersPerConn].active
		wg.Add(1)
		go func(conn hsms.Connection) {
			defer wg.Done()
			for j := 0; j < iterationsPerWorker; j++ {
				item := structuredListItem()
				if _, err := conn.SendDataMessage(ctx, 1, 1, true, item); err != nil {
					select {
					case errCh <- err:
					default:
					}

					return
				}
			}
		}(conn)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		b.Fatal(err)
	default:
	}
}
```

- [ ] **Step 2: Smoke-test with the minimum exact-accounting benchtime**

Run: `cd benchmarks && go test -run=^$ -bench=BenchmarkConnectionPool_ConcurrentRoundTrip -benchtime=32x ./hsmsssdata/v2/...`

`32x` is `totalWorkers` (`profileNumConns * profileWorkersPerConn` = 4*8), the smallest count where every worker performs exactly one round trip and the reported op count (32) exactly matches actual round trips performed — not a smaller count like `1x`, which would still spin up all 32 workers (via the floor-to-1 clamp) but report only 1 op for 32 actual sends.

Expected: `PASS` with one `BenchmarkConnectionPool_ConcurrentRoundTrip` result line, no `b.Fatal` output, no hang (all 4 connection pairs reach `SelectedState` and all 32 workers complete).

- [ ] **Step 3: Race-check the new concurrent harness**

Run: `cd benchmarks && go test -run=^$ -bench=BenchmarkConnectionPool_ConcurrentRoundTrip -race -benchtime=320x ./hsmsssdata/v2/...`

`320x` gives each of the 32 workers 10 iterations (10 full register/write/reply/deregister cycles per worker on its shared connection), rather than the single-shot burst `5x` would give — a stronger signal for the race detector to actually catch a concurrency bug in this new harness if one exists.

Expected: `PASS`, no `WARNING: DATA RACE` output. This is a one-off manual check — the race detector distorts profiling numbers, so it is not part of the `profile-v2` Makefile target added in Task 2.

- [ ] **Step 4: Vet the new file**

Run: `cd benchmarks && go vet ./hsmsssdata/v2/...`

Expected: no output (clean). Note: this standalone `benchmarks` module has no `golangci-lint` wiring today (same situation as `tools/gemgen` before its dedicated `lint-gemgen` target existed) — the root `make lint` only traverses the main module's `go.mod`, so `go vet` is the available pre-commit check here.

- [ ] **Step 5: Commit**

```bash
git add benchmarks/hsmsssdata/v2/bench_concurrent_test.go
git commit -m "test(benchmarks): add concurrent HSMS-SS round-trip profiling benchmark"
```

---

## Task 2: Add the `profile-v2` Makefile target and document it

**Files:**
- Modify: `benchmarks/Makefile`
- Modify: `benchmarks/README.md`

**Interfaces:**
- Consumes: `BenchmarkConnectionPool_ConcurrentRoundTrip` from Task 1, by name, via `-bench=` regex.
- Produces: `make profile-v2` (writes `results/cpu_v2.prof`, `results/mem_v2.prof`, `results/block_v2.prof`, `results/mutex_v2.prof`, `results/profile_v2.txt`); `PROFILE_ITERS` make variable (default `2048`, overridable like the existing `COUNT` variable).

- [ ] **Step 1: Fix the `bench-v2` wildcard collision**

Current `benchmarks/Makefile` `bench-v2` target:

```makefile
bench-v2: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v2/... > results/secs2item_v2.txt
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v2/... > results/hsmsssdata_v2.txt
	cat results/secs2item_v2.txt results/hsmsssdata_v2.txt > results/v2.txt
```

Replace the `hsmsssdata/v2` line's `-bench=.` with `-bench=BenchmarkConnection_`:

```makefile
bench-v2: results
	go test -run=^$$ -bench=. -benchmem -count=$(COUNT) ./secs2item/v2/... > results/secs2item_v2.txt
	go test -run=^$$ -bench=BenchmarkConnection_ -benchmem -count=$(COUNT) -benchtime=20x ./hsmsssdata/v2/... > results/hsmsssdata_v2.txt
	cat results/secs2item_v2.txt results/hsmsssdata_v2.txt > results/v2.txt
```

`-bench=.` is an unanchored regex matching every benchmark in the package. Once Task 1 adds `BenchmarkConnectionPool_ConcurrentRoundTrip` to `benchmarks/hsmsssdata/v2`, plain `-bench=.` would sweep it into `make bench-v2` at `-benchtime=20x` (not a multiple of 32, reintroducing the op-accounting mismatch) and into `make compare`'s `benchstat` diff against `v1.txt`, where no such benchmark exists — contradicting the "no v1 counterpart, not a benchstat comparison target" constraint. `BenchmarkConnection_` is a substring of all 4 existing benchmark names (`BenchmarkConnection_SmallItem_RoundTrip`, etc.) but not of `BenchmarkConnectionPool_ConcurrentRoundTrip` (no `_` immediately after `Connection` there), so the new regex keeps `bench-v2`/`compare` scoped to exactly the original four.

- [ ] **Step 2: Verify the new regex still matches only the original four benchmarks**

Run: `cd benchmarks && go test -list='BenchmarkConnection_' ./hsmsssdata/v2/...`

Expected: exactly `BenchmarkConnection_SmallItem_RoundTrip`, `BenchmarkConnection_StructuredList_RoundTrip`, `BenchmarkConnection_WaferMap_RoundTrip`, `BenchmarkConnection_Recipe_RoundTrip` — and NOT `BenchmarkConnectionPool_ConcurrentRoundTrip`. `-list` only prints matching names; it does not execute anything.

- [ ] **Step 3: Add the `PROFILE_ITERS` variable and `profile-v2` target to the Makefile**

Current `benchmarks/Makefile` top and `.PHONY` line:

```makefile
.PHONY: bench-v1 bench-v2 test-compat compare clean

COUNT ?= 6
```

Replace with:

```makefile
.PHONY: bench-v1 bench-v2 profile-v2 test-compat compare clean

COUNT ?= 6
# Must stay a multiple of 32 (BenchmarkConnectionPool_ConcurrentRoundTrip's
# profileNumConns * profileWorkersPerConn) so the benchmark's reported op
# count matches the actual number of round trips performed exactly.
PROFILE_ITERS ?= 2048
```

Then add the new target after the existing `bench-v2` target (i.e. right before `test-compat:`):

```makefile
profile-v2: results
	go test -run=^$$ -bench=BenchmarkConnectionPool_ConcurrentRoundTrip -benchmem \
	  -benchtime=$(PROFILE_ITERS)x -cpuprofile=results/cpu_v2.prof -memprofile=results/mem_v2.prof \
	  -blockprofile=results/block_v2.prof -mutexprofile=results/mutex_v2.prof \
	  ./hsmsssdata/v2/... > results/profile_v2.txt
```

`-blockprofile`/`-mutexprofile` need no extra runtime setup — passing them alone defaults to recording every blocking event / every contended-mutex stack trace (equivalent to `-blockprofilerate=1`/`-mutexprofilefraction=1`), per `go help testflag`. The trailing `> results/profile_v2.txt` captures the benchmark's `ns/op`/`B/op`/`allocs/op` line and the final `ok ... <N>s` wall-clock summary for manual trend comparison across runs, matching the existing `bench-v1`/`bench-v2` convention of redirecting to a results file.

- [ ] **Step 4: Run it, verify the profiles are produced, and check the run was long enough to profile**

Run: `cd benchmarks && make profile-v2 && cat results/profile_v2.txt`

Expected: exits 0; `results/profile_v2.txt` contains one `BenchmarkConnectionPool_ConcurrentRoundTrip` result line followed by a final `ok  	github.com/arloliu/go-secs/v2/hsmsssdata/v2	<N>s` line; `benchmarks/results/cpu_v2.prof`, `mem_v2.prof`, `block_v2.prof`, and `mutex_v2.prof` all exist (verify with `ls -la benchmarks/results/`).

Check `<N>` in that final `ok` line: if it's under roughly 1 second, the run was too short for a trustworthy CPU profile at Go's default 100 Hz sampling rate (too few samples to trust `go tool pprof -top`). If so, bump `PROFILE_ITERS` to the next multiple of 32 large enough to push `<N>` past ~1s (e.g. `make profile-v2 PROFILE_ITERS=4096`, doubling again if still too short) and re-run before moving to Step 5. This is a one-time manual check for this implementation — not automated Makefile logic.

- [ ] **Step 5: Spot-check the CPU profile has real signal**

Run: `cd benchmarks && go tool pprof -top results/cpu_v2.prof | head -20`

Expected: the top-20 output includes at least one entry whose package path is under `github.com/arloliu/go-secs/v2/` (e.g. a function in `hsms`, `hsmsss`, or `secs2` — such as frame encode/decode, `SendDataMessage`, or the HSMS-SS read loop), confirming the profile captured library code rather than only `runtime`/network-syscall frames. If every top-20 entry is `runtime.*` or syscall frames with no library code, increase `PROFILE_ITERS` to another multiple of 32 (e.g. `make profile-v2 PROFILE_ITERS=4096`) and re-check before proceeding.

- [ ] **Step 6: Spot-check the block/mutex profiles are valid**

Run: `cd benchmarks && go tool pprof -top results/block_v2.prof && go tool pprof -top results/mutex_v2.prof`

Expected: both commands print a valid pprof top table — even if sample counts are near zero, which is a legitimate finding (low lock contention at this concurrency level), not a bug. Only fail this check if a command errors out or reports the profile couldn't be parsed at all; that would indicate a wiring mistake in the `-blockprofile`/`-mutexprofile` flags, not an absence of contention.

- [ ] **Step 7: Vet the Makefile change's target package**

Run: `cd benchmarks && go vet ./hsmsssdata/v2/...`

Expected: no output (clean) — same rationale as Task 1 Step 4: this standalone module has no `golangci-lint` wiring, so `go vet` is the pre-commit check available here.

- [ ] **Step 8: Add the "Profiling v2" section to the README**

In `benchmarks/README.md`, append this new section at the end of the file (after the existing `results/` is git-ignored... line):

```markdown

## Profiling v2

`make profile-v2` runs `BenchmarkConnectionPool_ConcurrentRoundTrip`
(4 active/passive HSMS-SS connection pairs, 8 concurrent workers each,
`structuredListItem` payload) and captures CPU, memory, and contention
(block/mutex) profiles, plus a text summary, instead of a
benchstat-comparable result:

\`\`\`sh
make profile-v2                       # results/{cpu,mem,block,mutex}_v2.prof, results/profile_v2.txt
make profile-v2 PROFILE_ITERS=4096    # override the -benchtime=Nx count (default 2048; keep it a multiple of 32)
\`\`\`

Inspect the profiles with `go tool pprof`:

\`\`\`sh
go tool pprof -top results/cpu_v2.prof
go tool pprof -http=:0 results/mem_v2.prof     # browsable flame graph / graph view
go tool pprof -top results/block_v2.prof       # goroutine blocking (e.g. writeMu contention)
go tool pprof -top results/mutex_v2.prof       # contended-mutex stacks
\`\`\`

`results/profile_v2.txt` keeps the benchmark's `ns/op`/`B/op`/`allocs/op`
line for manual comparison across runs taken at different times.

This is standalone profiling tooling for v2 — there is no v1 counterpart
(the `bench-v2`/`compare` targets deliberately exclude it, see the
`bench-v2` target's `-bench=BenchmarkConnection_` scoping) and no automated
summary; `results/*.prof` are raw pprof output, read interactively.
```

- [ ] **Step 9: Commit**

```bash
git add benchmarks/Makefile benchmarks/README.md
git commit -m "build(benchmarks): add profile-v2 target for HSMS-SS pprof capture"
```
