# Architecture and Coding Rules

## Domain
- SECS/GEM protocols (SECS-I, HSMS, SECS-II, SML, GEM) for semiconductor equipment communication.
- Key specs: SEMI E4 (SECS-I), SEMI E5 (SECS-II), SEMI E37 (HSMS / HSMS-SS).

## Package Layout
| Package | Purpose |
|---------|---------|
| `secs1` | SECS-I over TCP/IP (SEMI E4). Implements `hsms.Connection` / `hsms.Session`. |
| `secs2` | SECS-II data item types and encoding/decoding. |
| `hsms`  | Shared interfaces, control/data message types, state machines, decoding. |
| `hsmsss`| HSMS Single-Session (SEMI E37.1) connection implementation. |
| `gem`   | GEM message helpers (e.g., S9Fx). |
| `sml`   | SML parser/lexer. |
| `logger`| Logging interface + adapters (slog, mock). |
| `internal/pool`, `internal/queue`, `internal/util` | Internal helpers; not part of public API. |

## Error Handling
- Use `errors.New` or `fmt.Errorf` with `%w` for wrapping.
- Package-level sentinel errors start with `Err` and contain the package prefix in the string: `ErrConnClosed = errors.New("secs1: connection closed")`.
- The `errname` linter enforces this naming convention.
- Use `errors.As` / `errors.Is` for checking; never compare error strings.

## Concurrency
- Use `sync`, `sync/atomic`, `context.Context`, and `github.com/puzpuzpuz/xsync/v3` for thread-safe maps.
- **Goroutine discipline**: Every goroutine must be cancellable via context or a shutdown signal. Always use `defer` to clean up (e.g., `defer atomic.Store(false)` for loop guards).
- **Timer pool**: Use `internal/pool.GetTimer` / `pool.PutTimer` instead of `time.NewTimer` or `time.After` on hot paths. `time.After` leaks the timer until it fires and should be avoided in loops.
- **Context mutex pattern**: When a per-connection `ctx`/`ctxCancel` can be recreated (reconnect), protect them with an `RWMutex` and expose via `createContext()` (write) / `getContext()` (read).
- **Atomic config fields**: Runtime-updatable config values (timeouts, feature toggles) use `sync/atomic` typed atomics (`atomic.Int64`, `atomic.Bool`) for lock-free hot-path reads. Structural config (host, port, role, queue sizes) stays behind a `sync.RWMutex` or is immutable after creation.

## Connection Lifecycle
- Both `hsmsss` and `secs1` connections follow the same state model: `Closed → Opening → Opened → (NotConnected ↔ NotSelected ↔ Selected)`.
- Active mode: `openActive()` does a synchronous first dial, then delegates to a background `connectLoop` goroutine with exponential backoff.
- Passive mode: a `taskMgr.Start("acceptConn", ...)` task loops accepting connections.
- `Close()` must: increment `reconnectGen`, cancel `loopCtx`, stop the state manager, force `NotConnected`, then wait for `isClosed()`.
- `Open()` must call `createContext()` and create a fresh `loopCtx` before spawning any goroutines.

## Logging
- Use the `logger.Logger` interface (never `fmt.Print` or `log.*` directly).
- Structured key-value pairs: `c.logger.Debug("message", "key1", val1, "key2", val2)`.
- Include `"method"` key for traceability in state handlers and connection methods.

## Documentation
- All exported functions, methods, types, variables, and constants must have GoDoc comments.
- Comments should reference the relevant SEMI spec section where applicable (e.g., `// per SEMI E37 §7.4.3`).
