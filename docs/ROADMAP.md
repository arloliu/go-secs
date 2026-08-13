# go-secs v2 Roadmap

Current release: **v2.3.1** (2026-08-12).
This roadmap plans the next two minor releases from a consumer-wishlist review
(two independent codebase passes, consolidated 2026-08-13).

The guiding observation:
the transport layer is complete and hardened,
so the remaining leverage is the layer *above* the wire —
where the library currently stops and every consumer starts repeating each other.

## v2.4 — consumer ergonomics & observability

Additive API only; no breaking changes to concrete types.
Two published interfaces gain a method each (see the compatibility note below).

| Feature | Package | Plan |
|---|---|---|
| Channel-based message delivery (`AddDataMessageChan` + `DataMessage.IsPrimary`) | `hsms` | [plan](plans/2026-08-13-datamessage-chan-plan.md) |
| Typed path extraction (`secs2.Cursor`) | `secs2` | [plan](plans/2026-08-13-secs2-cursor-plan.md) |
| Generated GEM reply decoders (`gem.DecodeS1F14`, …) | `gem`, `tools/gemgen` | [plan](plans/2026-08-13-gem-decoders-plan.md) |
| Transaction observer (`hsms.WithTransactionObserver`) | `hsms` | [plan](plans/2026-08-13-transaction-observer-plan.md) |
| Causal, cancellable lifecycle subscriptions (`SubscribeLifecycle`) | `hsms` | [plan](plans/2026-08-13-lifecycle-events-plan.md) |
| Error retryability classification (`hsms.IsTransient` / `hsms.IsTimeout`) | `hsms`, `secs1` | [plan](plans/2026-08-13-error-classification-plan.md) |

Suggested implementation order:
cursor → GEM decoders (decoders may consume the cursor),
channel delivery → (unblocks nothing in v2.4 but is the foundation for the deferred dispatch router),
observer, lifecycle, classification in any order.

**Compatibility note.**
`AddDataMessageChan` is added to the `SECS2Endpoint` interface, and `SubscribeLifecycle` is added to the `Connection` interface —
the first interface additions since v2.0.0 GA.
This is compile-breaking for hand-rolled implementations of either interface;
fakes that embed `hsmstest.FakeEndpoint` (updated in the same commit) pick up `AddDataMessageChan` for free,
but `FakeEndpoint` does not implement `Connection`,
so a hand-rolled `Connection` still needs its own `SubscribeLifecycle` stub.
The CHANGELOG entry must state this explicitly
and recommend embedding `hsmstest.FakeEndpoint` to be insulated from future additions.

## v2.5 — testing & examples

Theme release: everything a consumer needs to test a service without borrowing equipment.
Plans are written when v2.4 ships.

| Feature | Package |
|---|---|
| Live scriptable protocol peer (`hsmstest.NewPair`, SML-scripted replies) | `hsms/hsmstest` |
| SML assertions (`hsmstest.RequireSML`) | `hsms/hsmstest` |
| End-to-end runnable examples (`examples/host`, `examples/equipment`, `ExampleConnection`) | `examples/` |

## Deferred

Recorded so the reasoning is not re-derived; none of these are scheduled.

| Item | Rationale |
|---|---|
| Dispatch router (`dispatch.New`) | Design is settled (below) but the package waits until the channel foundation has real consumers. |
| Metrics snapshot / name-value iteration | The observer covers the pressing operability need; snapshot follows demand. |
| GEM communications establishment (`gemcomm`) | Buildable from the standard alone, but sequenced behind v2.4/v2.5. |
| Async-send delivery receipts | `WithAsyncSendErrorHandler` already reports failures per-message; positive receipts serve one pattern (durable outbox) and cost an allocation on a zero-alloc path. |
| Multi-connection fleet | Identity, partial startup, and restart policy are product choices; premature as library API. |
| Gateway relay with owned correlation | Wait for a real gateway integration to drive the correlation-table design. |
| GEM event-report registry | Largest item; equipment-specific mutable state should be driven by a real integration, not built from spec. |
| Struct ↔ SECS-II reflection mapping | Cursor + generated decoders cover most of the value without reflection; reassess after v2.4. |

### Recorded design: the dispatch router

Settled during the v2.4 review so the future package starts from a decided contract:

- Built on `AddDataMessageChan`: one small buffered channel as hand-off,
  one always-draining puller goroutine, its own queue where `Queue(n)` and
  `OnFull(Block|Drop|DropOldest)` policies live.
  The raw channel cannot be the policy queue —
  a full channel blocks the receive loop, which is only acceptable as an explicit `Block`.
- The handler table is **primaries only**, enforced at registration:
  `Handle(6, 12, …)` fails fast with an even-function error.
  A matched secondary never reaches the fan-out (the reply registry consumes it),
  so what remains is split with `DataMessage.IsPrimary`.
- Orphan secondaries (late replies after T3, replies to forwarded messages) go to a single
  `OnOrphanReply` hook — never S/F-routed, never S9-answered.
  Default when unset: count and drop.
- `Unhandled(AutoS9)` answers unhandled **primaries** with S9F3 (unknown stream) / S9F5
  (unknown function).
  A spec-violating even-function frame with W-bit set classifies as a primary,
  never matches an odd-function handler, and correctly receives S9F5.
- `Close` keeps the puller draining until connection teardown,
  because channel registration cannot be removed.
