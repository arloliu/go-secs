# 300 — Testing

## Organization

- **Unit**: `*_test.go` beside the code (same package or `_test` suffix).
- **Benchmarks**: `*_bench_test.go`.
- **Fuzz**: `fuzz_test.go` (in `hsms/` and `hsmsss/`).
- **Integration**: `tests/hsmsss_integration/`, `tests/secs1_integration/`, driven by helper binaries under `tests/active_host/`, `tests/passive_host/`, `tests/passive_eqp/` and the shell harnesses (`tests/*.sh`).

## Rules

- No emojis in test output.
- `t.Context()` for contexts, `t.Setenv()` for env (enforced by `tenv`).
- `for b.Loop()` for benchmarks.
- Assertions: `testify` (`require`, `assert`).
- Cleanup via `t.Cleanup` or `defer` (listeners, connections, sessions, goroutines).
- `t.Parallel()` where safe; `tparallel` flags misuse.
- Reuse the existing in-package mock patterns in `hsms/mock_*_test.go` — do not introduce a mocking framework.

## Async (CRITICAL)

- Never use `time.Sleep` to wait for state. Subscribe to events before triggering the action, collect transitions, then assert.
- `hsms.ConnStateMgr` emits state events — subscribe, don't poll.
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
make test          # all packages, race, -short, logs to test.log
make build-tests   # compile tests without running
make coverage      # per-dir coverage profile
make stress-test   # repeat timing-sensitive packages (STRESS_COUNT, default 50)
make stress-quick  # narrow set of flake-prone tests
make fuzz-test     # all fuzz targets for FUZZ_TIME (default 30s)
make clean         # rm test.log + clear test cache
```
