# 100 — Overview & Prime Directives

## Packages

| Package  | Scope |
|----------|-------|
| `hsms`   | HSMS message types (control / data), encode / decode, connection-state machine, `Connection` / `Session` interfaces |
| `hsmsss` | HSMS-SS single-session transport: active / passive, host / equipment, linktest, reconnect |
| `secs1`  | SECS-I over TCP/IP: block transport, ENQ/EOT/ACK/NAK, T1–T4, 244-byte block split/reassembly, Master/Slave contention, S9Fx |
| `secs2`  | SECS-II data items + shortcut constructors (`A`, `B`, `BOOLEAN`, `F4/F8`, `I1–I8`, `U1–U8`, `L`) |
| `sml`    | SML parser / formatter (strict and non-strict modes) |
| `gem`    | GEM (SEMI E30) helpers |
| `logger` | Logger adapter (slog default) |

Private: `internal/pool` (timer pool), `internal/queue`, `internal/util`. Do not expose in public signatures or docs.

Integration: `tests/hsmsss_integration/`, `tests/secs1_integration/`, with helper binaries `tests/active_host/`, `tests/passive_host/`, `tests/passive_eqp/`, and shell harnesses (`tests/*.sh`).

Also on disk: `examples/device/` and `examples/secs1_device/` (library-usage examples); `docs/secs1/` (SECS-I design notes) and `docs/specs/` (SEMI standard excerpts — read-only reference).

## Architecture

- **Transport-agnostic sessions.** Both `hsmsss` and `secs1` satisfy `hsms.Connection` and `hsms.Session`. Keep that substitution property intact.
- **Connection state machine.** `hsms.ConnStateMgr` drives NOT-CONNECTED → CONNECTED → NOT-SELECTED → SELECTED transitions and exposes an event notifier. Consumers subscribe; tests must subscribe, not poll. Race-sensitive.
- **Data-message lifecycle.** `hsms.DataMessage.Free` returns pooled items and MUST be idempotent. Do not hold references past the reply.
- **SML mode is global.** Mode setters (`sml.WithASCIIStrictMode`, `hsms.UseStreamFunctionSingleQuote`, etc.) configure the package; treat them as startup configuration, not per-message knobs.

## Toolchain

- Go version is pinned in `go.mod`. The linter module (`.linter.go.mod`) pins its own; keep them separate.
- Runtime deps are intentionally minimal — check `go.mod` before adding any. Prefer stdlib.
- `.golangci.yaml` blocks: `github.com/golang/protobuf`, `github.com/satori/go.uuid`, `github.com/gofrs/uuid`.

## Prime Directives

1. Small diffs. Don't rewrite files unnecessarily.
2. Public API stability. Breaking changes to exported symbols in public packages need clear justification.
3. No `internal/` types in public signatures, examples, or READMEs.
4. `hsmsss` and `secs1` must both continue to satisfy `hsms.Connection` / `hsms.Session`. Change the interface → update both transports + integration tests.
