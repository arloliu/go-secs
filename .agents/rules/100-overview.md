# 100 — Overview & Prime Directives

## Packages

| Package  | Scope |
|----------|-------|
| `hsms`   | HSMS message types (control / data), encode / decode, connection-state machine, `Connection` / `SECS2Endpoint` interfaces |
| `hsmsss` | HSMS-SS single-session transport: active / passive, host / equipment, linktest, reconnect |
| `secs1`  | SECS-I over TCP/IP: block transport, ENQ/EOT/ACK/NAK, T1–T4, 244-byte block split/reassembly, Master/Slave contention, S9Fx |
| `secs2`  | SECS-II data items + shortcut constructors (`A`, `B`, `BOOLEAN`, `F4/F8`, `I1–I8`, `U1–U8`, `L`) |
| `sml`    | SML parser / formatter (strict and non-strict modes) |
| `gem`    | GEM (SEMI E30) helpers |
| `logger` | Logger adapter (slog default) |

Private: `internal/framecodec` (frame-codec capability tokens), `internal/gencap` (generation-aware runtime capability shared by `hsms` and `hsmsss`), `internal/pool` (timer pool), `internal/throttle` (time-window gate), `internal/wire` (zero-copy message-body bridge).
Do not expose in public signatures or docs.

Integration: `integration/` drives the real `hsmsss` and `secs1` connections over in-memory pipes against scripted peers.

Also on disk: `tracepack/` and `tools/gemgen/` (nested modules; see `500-workflow.md`);
`docs/secs1/` (SECS-I design notes) and `docs/specs/` (subject directories under `450-doc-lifecycle.md`).

## Architecture

- **Transport-agnostic connection.**
  `hsmsss.New` and `secs1.New` each return their package's `Connection`, which embeds `hsms.Connection`;
  each transport plugs into the shared engine in `hsms` through its unexported `transport` interface.
  Keep that substitution property intact.
- **Connection state machine.**
  The `hsms` engine drives the `hsms.ConnState` transitions and reports them through `Connection.SubscribeLifecycle` and `AddConnStateChangeHandler`.
  Consumers subscribe; tests must subscribe, not poll.
  Race-sensitive.
- **Immutable messages.**
  `hsms` messages and `secs2` items are GC-owned immutable values that goroutines share safely;
  there is no `Free` and no message pooling (`hsms/doc.go`).
  Zero-copy discipline lives on `internal/wire`, not on any public API.
- **SML modes are per instance.**
  Strictness and quoting are `sml.ParserOption` / `sml.EncoderOption` values (`WithParserStrictMode`, `WithEncoderStrictMode`, `WithASCIIQuote`, …),
  fixed when the `Parser` or `Encoder` is built.

## Toolchain

- The module's minimum Go version is the `go` directive in `go.mod`.
  `mise.toml` pins the development toolchain (Go and golangci-lint); keep them separate.
- Runtime deps are intentionally minimal — check `go.mod` before adding any. Prefer stdlib.
- `.golangci.yaml` blocks: `github.com/golang/protobuf`, `github.com/satori/go.uuid`, `github.com/gofrs/uuid`.

## Prime Directives

1. Small diffs. Don't rewrite files unnecessarily.
2. Public API stability. Breaking changes to exported symbols in public packages need clear justification.
3. No `internal/` types in public signatures, examples, or READMEs.
4. `hsmsss.New` and `secs1.New` must both keep returning a `Connection` that satisfies `hsms.Connection`.
   Change `hsms.Connection` or the transport seam → update both transports + `integration/` tests.
