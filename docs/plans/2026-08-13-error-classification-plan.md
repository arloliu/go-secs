# Implementation plan: error retryability classification (v2.4)

Move the retry decision from consumer guesswork into the packages that know the answer.
Today every consumer writes their own `errors.Is` chain over bare sentinels (`hsms/errors.go`, `secs1/errors.go`),
and each classifies `ErrNotSelectedState` vs. a T3 timeout vs. `ErrMessageTooLarge` differently.

## API

```go
// In hsms — works for errors from BOTH transports (secs1 returns hsms.Connection).
func IsTransient(err error) bool // safe to retry the same call later
func IsTimeout(err error) bool   // a protocol or context timer expired
```

Classification rules, in order:

1. `errors.As` to the marker interfaces below — the extensible path.
2. `errors.Is` against the known `hsms` sentinels (the built-in table).
3. `context.DeadlineExceeded` → timeout; `net.Error.Timeout()` → timeout.
4. Otherwise false — **unknown errors are not transient**;
   misclassifying a permanent error as retryable causes silent retry storms,
   the reverse merely fails a job early.

## The import-cycle constraint and the marker interfaces

`secs1` imports `hsms`, so `hsms.IsTransient` cannot enumerate `secs1` sentinels directly.
Solution — small marker interfaces in `hsms`:

```go
type TransientError interface{ error; Transient() bool }
type TimeoutError   interface{ error; Timeout() bool }   // shape-compatible with net.Error
```

`secs1` (and any future transport) marks its own errors by giving the relevant sentinels a small concrete type implementing the marker,
**preserving variable identity** so existing `errors.Is(err, secs1.ErrSendFailed)` comparisons keep working —
the sentinel var keeps its name and value; only its dynamic type gains a method.
Wrap sites are unaffected because `errors.Is` matches on the var, not the type.

## Classification table (draft — finalized by the inventory step)

| Sentinel | Transient | Timeout | Rationale |
|---|---|---|---|
| `hsms.ErrNotSelectedState` | yes | no | reconnect/select in progress; later send can succeed |
| T3 reply expiry (exact sentinel per inventory) | yes | yes | peer may answer the next attempt |
| `hsms.ErrMessageTooLarge` | no | no | same payload fails forever |
| `hsms.ErrEvenFunctionPrimary`, `ErrAsyncReplyExpected` | no | no | caller bugs |
| `secs1.ErrSendFailed` (retries exhausted) | yes | no | line-level failure; line may recover |
| `secs1.ErrChecksumMismatch`, `ErrDeviceIDMismatch`, … | no | no | surface via receive path/metrics, not send returns — confirm in inventory |
| decode/validation sentinels | no | no | malformed data does not improve on retry |

The **inventory step is the real work**:
walk every error-returning public path (`SendDataMessage`, `SendSECS2Message`,
`Forward*`, `Reply*`, `Open`, `Close`) and record what each can actually return,
including wrapped context errors —
the table above is a hypothesis until then, and the PR description carries the verified version.
Do not classify a sentinel that cannot reach a public return; note it as unreachable instead.

## Steps

1. Inventory (above) — produce the verified sentinel → classification table.
2. Add the marker interfaces and the two functions to `hsms`; mark `secs1` sentinels.
3. Godoc: each classified sentinel's comment states its classification,
   so the table lives next to the errors, not only in the functions.
4. Tests, lint, CHANGELOG (`### Added`).

## Tests

- Table-driven: every classified sentinel through both functions, plus wrapped forms
  (`fmt.Errorf("…: %w", sentinel)`) and `errors.Join` combinations.
- `context.DeadlineExceeded` and a real `net.Error` timeout classify as timeout.
- Unknown error → false/false.
- Identity guard:
  `errors.Is(err, secs1.ErrSendFailed)` still holds after the sentinel gains its marker type (protects the retype).
- Integration: a send into a not-selected connection returns an error for which
  `IsTransient` is true — end-to-end, not just table.

## Acceptance

- Verified inventory table in the PR; every public error path accounted for.
- `make test-all` green, lint clean.
- No sentinel identity broken (the identity-guard test is the teeth).
