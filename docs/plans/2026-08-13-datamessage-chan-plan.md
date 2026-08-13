# Implementation plan: channel-based message delivery (v2.4)

Export the session's existing channel fan-out as `SECS2Endpoint.AddDataMessageChan`,
and export the router's primary/secondary classification as `DataMessage.IsPrimary`.

The mechanism already exists and is teeth-tested:
`chans []chan *DataMessage` (`hsms/session.go`), registered by the unexported `addChanHandler`,
delivered by `recvDataMsg` via a `select` that includes `rt.Done()`
(guard test: `TestSession_J5_BlockedChannelDoesNotWedgeFanOut`).
This plan is therefore mostly contract, godoc, and edge-case enforcement — not plumbing.

## API

```go
// On SECS2Endpoint (interface addition — see the compatibility section):
AddDataMessageChan(ch chan *DataMessage)

// On *DataMessage:
func (msg *DataMessage) IsPrimary() bool
```

No registration-time filtering.
Consumers split the stream themselves with `IsPrimary`;
filter sugar can be added later without breaking anything.

## Design decisions (settled 2026-08-13)

1. **Delivery set = func-handler parity**: primaries plus orphan secondaries
   (late replies after T3 expiry, and replies to `ForwardDataMessage`).
   Nothing is silently dropped — the gateway correlation pattern depends on orphans arriving.
2. **`IsPrimary` is the exact negation of the router's `isSecondaryReply`**
   (`hsms/connection_runtime.go`): `WaitBit() || Function()%2 == 1`.
   Refactor `isSecondaryReply` to call it, so there is one source of truth.
   SxF0 (transaction abort) classifies as a secondary;
   a spec-violating even-function frame with the W-bit set classifies as a primary.
3. **Placement on `SECS2Endpoint`**, symmetric with `AddDataMessageHandler`,
   so the future dispatch router can accept the narrow endpoint interface.

## Edge cases the implementation must handle

Each of these was verified against source during the design review.

1. **nil channel**: a nil send case makes every delivery `select` wait only on `Done()`,
   wedging the connection from the first message.
   `AddDataMessageChan(nil)` must panic at registration (programming error, fail loud).
2. **Consumer closes a registered channel**: send-on-closed-channel panics on the receive goroutine,
   which has no `recover` (verified: `recvLoop` is bare-spawned).
   Same exposure as a panicking func handler — no new guard;
   the godoc must forbid closing a registered channel and prescribe ending consumer loops with the consumer's own quit signal.
3. **Full channel stalls the receive loop**, including control-frame processing,
   so a persistently stalled consumer surfaces as linktest/T6 failure → connection drop → teardown unblocks the fan-out → reconnect.
   Document: keep draining; size the buffer for burst depth.
4. **`Close` under a stalled consumer** burns the full close timeout and returns
   `ErrCloseTimeout` (the epoch closes `done` only after its bounded joins).
   Same contract as a blocked func handler; document the parity.
5. **Sync send from the chan-consumer goroutine** while its own channel is full self-deadlocks until T3 expires
   (the reply cannot route while the receive loop is blocked on that channel).
   Document as part of the must-not-block discipline.
6. **Duplicate registration** delivers the message once per registration (parity with func handlers).
   Document, do not dedupe.
7. **Teardown mid-fan-out**: channels later in the slice miss the message
   (delivery is at-most-once, no teardown flush).
   Document.
8. **Registration is permanent** for the connection's lifetime (no removal API),
   and survives Open/Close cycles like func handlers.
   Document.

## Compatibility

First interface addition since v2.0.0 GA — compile-breaking for hand-rolled
`SECS2Endpoint` implementations.

- Update `hsmstest.FakeEndpoint` in the same commit:
  implement `AddDataMessageChan`,
  and deliver injected inbound messages to registered channels with the same select-on-done semantics as the real fan-out.
- CHANGELOG entry under a `### Changed` (breaking-flagged) note:
  name the added method, state the compile-break for hand-rolled fakes,
  and recommend embedding `hsmstest.FakeEndpoint`.

## Steps

1. Read `hsms/session.go` (fan-out), `hsms/connection_runtime.go` (classification),
   `hsms/hsmstest/endpoint.go` (fake delivery paths) before editing.
2. Add `DataMessage.IsPrimary` (`hsms/data_msg.go`);
   refactor `isSecondaryReply` to `!dm.IsPrimary()`.
3. Rename/wrap `addChanHandler` into the exported method on the session
   (the session already implements the rest of `SECS2Endpoint`);
   add the nil-channel panic.
4. Add the method to the `SECS2Endpoint` interface with the full godoc contract
   (edge cases 2–8 above; public wording, no internal invariant codes).
5. Update `hsmstest.FakeEndpoint`.
6. Tests (see below), lint, CHANGELOG.

## Tests

- `IsPrimary` truth table: odd/W-set/W-clear × even/F0 —
  pinned against `isSecondaryReply` by calling both on the same messages.
- Delivery: registered channel receives primaries and orphan secondaries in arrival order
  (FIFO), interleaved correctly with func handlers.
- nil registration panics.
- Duplicate registration delivers twice.
- Re-Open: channel registered before Close still receives after the next Open.
- `hsmstest.FakeEndpoint` delivers to registered channels.
- Existing `TestSession_J5_BlockedChannelDoesNotWedgeFanOut` already guards the blocked-channel teardown invariant —
  verify it exercises the exported path after the rename.

## Acceptance

- `make test-all` green, `-race` clean, lint clean.
- No allocation change on the fan-out hot path for connections with no registered channels
  (benchmark before/after if the fan-out shows in existing benchmarks).
- Godoc renders the full consumer contract on pkg.go.dev.
