# Implementation plan: transaction observer (v2.4)

One structured hook that reports each completed send transaction —
the seam that lets a consumer bridge to OpenTelemetry or Prometheus without the library importing either.
Today the only sub-counter observability is `WithTraceTraffic` hex dumps through the logger.

## API

```go
func WithTransactionObserver(fn func(TxEvent)) ConnOption

// TxEvent is a value; the observer must not retain pointers into it beyond the call.
type TxEvent struct {
    Stream      uint8
    Function    uint8
    ID          uint32        // System Bytes as uint32 (matches DataMessage.ID)
    ReplyWaited bool          // true when the send opened a reply-wait transaction (W-bit)
    Duration    time.Duration // enqueue-to-outcome
    Outcome     TxOutcome
    Err         error         // nil on TxReplied / TxSent
}

type TxOutcome uint8

const (
    TxReplied  TxOutcome = iota // reply received (W-bit sends)
    TxSent                      // frame written, no reply expected
    TxT3Timeout                 // reply-wait expired
    TxRejected                  // peer Reject.req (RejectError)
    TxCanceled                  // caller context canceled or deadline exceeded
    TxSendError                 // transport write / not-selected / too-large failure
)
```

## Scope (deliberate)

- **In**: the synchronous send paths — `SendDataMessage`, `SendSECS2Message`,
  `ForwardDataMessage`, `ReplyDataMessage`.
  One event per call, fired on the **caller's goroutine** at completion,
  so the hook can never block a protocol goroutine.
- **Out (v2.4)**: async sends.
  Failures are already observable per-message via `WithAsyncSendErrorHandler`;
  positive async receipts are a separate deferred wishlist item.
  Inbound-handler latency is also out — it is the consumer's own code being measured.
- Config is read via the existing `cfg` atomic pointer;
  a nil observer costs one nil check on a pointer load that already happens.
  Zero cost when unset.

## Design notes

1. Instrument at the single chokepoint in `hsms/connection_send.go`
   where the sync paths converge on wait/return, so every outcome branch is covered once.
   Read that file first and map each return path to exactly one `TxOutcome`.
2. `TxEvent` is passed by value and contains no pointers except `Err` —
   safe to hand to another goroutine, cheap to construct, no allocation beyond what the error path already allocates.
3. The observer runs synchronously on the caller's goroutine.
   Document: a slow observer slows that caller's send call and nothing else.
   No panic isolation needed beyond what the caller already owns —
   but confirm this against the existing async-handler panic-isolation precedent
   (`connection_send.go` isolates the async error handler; the sync path is caller-owned).
4. Naming: verify `TxEvent` / `TxOutcome` collide with nothing in `hsms` before committing to the names.

## Steps

1. Read `hsms/connection_send.go` end to end;
   enumerate every sync-path return with its outcome classification in the PR description.
2. Add the config option, the types, and the chokepoint instrumentation.
3. Godoc with a Prometheus-histogram-shaped example (labels from Stream/Function/Outcome,
   observe Duration).
4. Tests, lint, CHANGELOG (`### Added`).

## Tests

Use the in-repo pair/loopback harness (integration tests already establish real connections):

- One test per outcome: replied, sent (W=0 and Forward), T3 timeout, peer reject,
  context cancel, send error (not-selected).
- Duration is positive and sane (≥ actual round-trip on the replied case).
- No event fires when no observer is configured
  (and benchmark: no measurable hot-path regression for the unset case).
- Observer panics propagate to the caller of the send (documenting, not swallowing).

## Acceptance

- Every sync return path maps to exactly one event — verified by the outcome test matrix.
- `make test-all` green, `-race` clean, lint clean, no unset-path benchmark regression.
