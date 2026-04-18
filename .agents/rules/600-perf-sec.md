# 600 — Performance & Security

## Performance hot paths

- HSMS message encode / decode.
- Per-item SECS-II encode (one file per type in `secs2/`).
- SML parsing (lexer + parser).
- HSMS-SS message read loop.
- SECS-I block transport (split / reassembly, timers).

In those paths:

- Pre-allocate slices (`make([]T, 0, cap)`) and maps (`make(map[K]V, size)`). The `prealloc` linter flags growable slices.
- Avoid `append` in tight per-message loops when the size is predictable.
- Reuse the existing item / message pools. Do not allocate timers per message; use `internal/pool.TimerPool` for T1–T4, T3 replies, and linktest.
- Pass small headers by value; avoid deep-copying `DataMessage`.
- Don't add indirection on top of `secs2.Item` / `hsms.HSMSMessage` dispatch.
- `sync/atomic` for flags / counters, `sync.Mutex` for complex state, `puzpuzpuz/xsync/v3` for concurrent maps with heavy read/write mix.
- No unbounded goroutines — all loops are scoped to the connection context.

Benchmarks live in `*_bench_test.go`. Stress / soak via `make stress-test` and `tests/bench_timer_pool_test.sh`.

## Security

go-secs decodes bytes from untrusted TCP peers. Treat input as adversarial.

- Validate length / message type / session ID / format codes **before** allocating body buffers.
- Enforce a max message size; never allocate based on a raw attacker-controlled length field.
- SECS-I block reassembly must bound in-flight blocks and discard duplicates / stale blocks per SEMI E4 §9.4.2.
- SML parser must bound nesting / token count; no unbounded recursion on untrusted input.
- Every blocking network read respects a configured timeout (T1–T8 as applicable) and the connection context.
- Return wrapped errors on malformed input. Do not panic.
- Don't log message bodies above DEBUG — SECS-II payloads can be sensitive.
- Extend `hsms/fuzz_test.go` / `hsmsss/fuzz_*_test.go` when adding a decoder entry point.
- `gosec` is enabled; suppress only with a specific `//nolint:gosec // reason`.
