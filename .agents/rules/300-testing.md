# 300 — Testing

## Organization

- **Unit**: `*_test.go` beside the code (same package or `_test` suffix).
- **Benchmarks**: `*_bench_test.go`.
- **Fuzz**: `Fuzz*` targets, usually in `fuzz_test.go`.
  `make fuzz-test` runs only the packages in the Makefile's `FUZZ_PKGS`, so a package's first fuzz target also needs an entry there.
- **Integration**: `integration/` — the real `hsmsss` and `secs1` connections over in-memory pipes against scripted peers.

## Rules

- No emojis in test output.
- `t.Context()` for contexts, `t.Setenv()` for env.
- `for b.Loop()` for benchmarks.
- Assertions: `testify` (`require`, `assert`).
- Cleanup via `t.Cleanup` or `defer` (listeners, connections, sessions, goroutines).
- `t.Parallel()` where safe; `tparallel` flags misuse.
- Reuse the existing test doubles (`hsms/harness_mock_*_test.go`, `hsms/hsmstest`, `logger/loggertest`) —
  do not introduce a mocking framework.

## Async

Sleep-based waits are timing-dependent and flake under `-race` and `make stress-test`.

- Never use `time.Sleep` to wait for state. Subscribe to events before triggering the action, collect transitions, then assert.
- `Connection.SubscribeLifecycle` and `AddConnStateChangeHandler` report state transitions — subscribe, don't poll.
- When polling is the only option (metric counters, etc.), use `require.Eventually` with a bounded timeout.
- `time.Sleep` is acceptable only to *inject* a delay into the scenario itself.

## Patterns

Table-driven only when there are multiple cases:

```go
tests := []struct{ name string; in X; want Y }{...}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) { ... })
}
```

Single case: plain function.

## Running

```bash
make test           # all packages, race, -short, streams to stdout + test.log
make test-all       # full suite without -short (integration tests included)
make build-tests    # compile tests without running
make bench          # run benchmarks across all packages
make coverage       # per-dir coverage profile
make stress-test    # repeat timing-sensitive packages (STRESS_COUNT, default 10)
make stress-quick   # narrow set of flake-prone tests
make fuzz-test      # every Fuzz* in FUZZ_PKGS, run for FUZZ_TIME (default 30s)
make clean          # rm test.log + clear test cache
make clean-coverage # rm ./.coverage
```

`make` with no target prints a categorized help list.
