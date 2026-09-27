# HSMS wire observer and generation-aware events

Status: draft (2026-09-27) — proposal for go-secs v2.6.0; owner decisions in §9, external review pending.

## 1. Purpose

Let an application that owns a go-secs HSMS connection record the traffic on that connection
with the fidelity and context a traffic-log archive needs:
every frame in both directions as it went on the wire,
which socket it belongs to,
when it was read or written,
and the connection's lifecycle, transaction and socket events with the same identity.

The first consumer is the eqp-hub `eqp_hsms` device,
which will emit records to a log service that stores them in the tracepack format (`docs/specs/tracepack/`).
The requirements below come from that format's capture model:
`tracepack-format.md` §7 (record header), invariant I-7 (epoch per socket),
`tracepack-semantics.md` §2 (fidelity), §4 (time), §5 (transport events).
Any other application with the same need, such as a test harness or a simulator, uses the same API.

## 2. Why the hook lives in go-secs

An eqp-hub review of its three candidate emission points
(the `tap_nats` device, the shadow channel, and the `eqp_hsms` device)
found that only the device holding the go-secs connection can see everything:
auto-replies and hub-originated messages never enter a channel,
invalid messages are dropped before any channel,
T3 timeouts surface only as synthetic replies,
and no path sees control frames, transition causes or sockets.
Instrumenting each of those places separately would still miss the frames go-secs itself sends
(Reject, Linktest, Select, Separate).
One observer at the byte boundary covers all of them.

What go-secs exposes today, and what is missing:

| Need | Today | Gap |
|---|---|---|
| Every frame, both directions, wire bytes | `WithTraceTraffic` writes hex dumps to the logger; inbound tracing covers data frames only, and the outbound dump is a re-encode | no programmatic hook, no control frames inbound, no timestamps |
| Which socket a frame or event belongs to | `epoch.id`, a per-connection monotonic counter (`hsms/epoch.go`), unexported | not visible to any callback |
| Timestamps at read/write | none on messages or events | — |
| Lifecycle with cause | `SubscribeLifecycle` and `LifecycleEvent{Previous, Current, Cause}` since v2.4.0 | no generation |
| Transaction outcomes | `WithTransactionObserver` and `TxEvent` (stream, function, System Bytes, outcome) | no generation, no session id |
| Socket accept / connect / close, including an accepted-then-refused socket | none | — |

## 3. Design

All additions are backward compatible: new options, new types, and new fields on existing event structs.

### 3.1 Wire observer

```go
// WireDirection says which way a frame crossed the socket.
type WireDirection uint8

const (
    WireInbound  WireDirection = iota + 1 // read from the peer
    WireOutbound                          // written to the peer
)

// WireEvent describes one complete HSMS frame as it crossed a socket.
type WireEvent struct {
    Direction  WireDirection
    Socket     uint64    // socket identity within this connection (§3.4); never 0
    Generation uint64    // FSM generation that adopted the socket; 0 for a refused socket
    At         time.Time // taken when the frame was fully read or fully written; carries a monotonic reading
    Frame      []byte    // the wire bytes: 4-byte length prefix, 10-byte header, message text
}

// WithWireObserver installs a hook called once per complete frame in either direction.
func WithWireObserver(fn func(WireEvent)) ConnOption
```

Contract:
- Called for every frame that reached the socket:
  data and control frames,
  frames go-secs sends on its own (Select, Linktest, Reject, Separate),
  and inbound frames go-secs rejects after reading them
  (bad PType or SType, a control frame with a body, a data frame while not selected).
  A frame is observed before it is dispatched,
  so a frame that trips a decode error is still observed as read.
- Not called for a frame the read loop could not complete (a socket that closed mid-frame):
  those bytes never form a frame.
  The socket close event of §3.3 marks the boundary.
- `Frame` is valid only during the call, must not be retained and must not be mutated;
  the observer copies what it keeps.
  This is a caller contract, not a physical guarantee:
  the inbound buffer is the one the core later adopts as the data message's body (`DeliverOwnedFrame`), zero-copy,
  so an observer that wrote into it would corrupt the message.
  Outbound frames are assembled under the write lock into a per-socket scratch buffer, filled before the write consumes the buffer list,
  so the observer adds no allocation of its own per frame after warm-up (the send path's own prefix allocation is unchanged).
- The outbound hook runs under the socket's write lock:
  an observer must not send on the same connection from inside the callback.
- Ordering: within one socket,
  inbound events are delivered in wire order from the receive goroutine
  and outbound events in wire order under the write lock.
  Across the two directions no order is promised beyond the timestamps.
- The hook runs synchronously on the receive or send path.
  It must be cheap: a recorder copies the frame into its own queue and returns.
  Blocking here delays the link, including the T8 read deadline on inbound.
- A panic in the observer is not recovered, the policy `WithTransactionObserver` already has.
- Without an observer the cost is one nil check per frame.

Call sites:
- inbound: `hsmsss/transport_recv.go` `recvLoop`, right after `readFrame` returns and before `dispatchFrame`.
  `readFrame` today reads the length into a separate small buffer and returns only header and body;
  it changes to allocate one owned buffer holding prefix, header and body after the length is validated,
  observes the whole buffer, and hands its `[4:]` slice to dispatch, so the body is still adopted without a copy.
- outbound: the two places the connection hands buffers to the transport byte sink,
  `hsms/connection_send.go` `writeFrame` after a successful `Write`,
  and the courtesy Separate in `hsms/connection_lifecycle.go`.
  Wire reporting is a transport capability the connection discovers by type assertion, as it does for other `hsmsss` capabilities,
  and only the HSMS-SS transport provides it:
  the SECS-I transport reports success for an HSMS control frame without any wire I/O and converts data frames to SECS-I blocks,
  so an unconditional hook in the shared core would report frames that never crossed a SECS-I socket.
- the refusal exchange of a second passive connection (`hsmsss/transport_passive.go` `refuseExtraConn`),
  which reads and writes the extra socket directly:
  the inbound Select.req is observed as the full 14-byte frame after the header read completes, before it is interpreted,
  invalid headers included;
  the outbound Select.rsp is observed only when the write returned the full frame length without error;
  both carry the refused socket's identity and generation 0.

`At` is `time.Now()` at the call site.
Go's `time.Time` carries a monotonic reading,
so a recorder derives both the wall clock (`UnixNano`) and elapsed monotonic time (`Sub` against its capture origin) from the one value.

### 3.2 Generation and session on existing events

- `LifecycleEvent` gains `Socket uint64` and `Generation uint64`, the socket and generation the transition belongs to.
  The supervisor resolves the generation when it fires a transition (a guarded commit carries it; an unnamed T7 or disconnect report is bound at report time),
  but today `stateChange` drops it and the notifier builds the event later.
  The identity is therefore snapshotted into `stateChange` at the moment the transition fires,
  never resolved from the current epoch on the notifier goroutine;
  `Close` takes the epoch `requestClose` pinned.
  Both fields are 0 when the connection has no epoch at all (a `Close` before any `Open`);
  a transition of an epoch that never established a socket has its generation and socket 0.
  A stale report is discarded by the supervisor and produces no event, so no event carries a generation that is already gone.
- `TxEvent` gains `Socket uint64`, `Generation uint64` and `SessionID uint16`.
  `sendWaitReply` and `sendNoReply` load the pinned epoch internally and the event is built after they return,
  so a fresh read there could name a successor after a reconnect;
  the send path returns the identity it pinned, including on its early-failure returns, and the event is built from that.
  `SessionID` is the primary's session id, which a T3 record needs and the event does not carry today.
  A late reply arriving after a T3 timeout needs no event:
  it is a frame, and the wire observer reports it.

### 3.3 Socket events

```go
type SocketEventKind uint8

const (
    SocketConnected SocketEventKind = iota + 1 // active role: dial succeeded
    SocketAccepted                             // passive role: accept returned
    SocketRefused                              // passive role: accepted, answered Communication Already Active, closed
    SocketClosed                               // the socket is closed, by either side or by teardown
)

type SocketEvent struct {
    Kind       SocketEventKind
    Socket     uint64
    Generation uint64      // 0 until adopted, and always 0 for a refused socket
    At         time.Time
    Local      net.Addr
    Remote     net.Addr
    Err        error       // for SocketClosed: the read or write error that ended it, nil for a local close
}

func WithSocketObserver(fn func(SocketEvent)) ConnOption
```

A socket record is minted at every successful dial or accept, refused sockets included,
and owns a single close gate that captures the error which initiated the close.
Every path that closes a socket goes through that gate:
epoch teardown, a dial sealed by a concurrent teardown, a TCP-up the core refuses, transport `Stop`,
and the refusal cleanup and `haltRefusal` paths of the extra socket.
`SocketClosed` is therefore emitted exactly once per socket, with `Err` nil for a local close.
`SocketRefused` is emitted when the exchange completes, and also when the peer's data is malformed or the response write fails;
the close event that follows carries the error in those cases.
Call sites: `hsmsss/transport_active.go` after the dial, `hsmsss/transport_passive.go` after accept and in `refuseExtraConn`, and the close gate.

### 3.4 Socket identity

`Socket` is a per-connection monotonic counter minted when a socket is dialed or accepted, refused sockets included.
It is unique within one connection, not across connections of a process;
a recorder that serves several connections keys on the connection as well.
It is distinct from `Generation` (`epoch.id`), which only adopted sockets get, so the FSM's straggler matching is untouched;
the generation-scoped refusal handoff token is not reused for it, because that token resets with every generation.
A traffic-log recorder uses `Socket` as the tracepack `epoch`, which is defined per socket used, refused sockets included.
tracepack stores `epoch` as `u32`; a recorder that sees a value above 2^32 − 1 stops the capture and starts a new one,
rather than wrapping, so the narrowing has a defined outcome even if it never happens in practice.
Both counters restart with the process, as tracepack expects: a recorder restart is a new capture.

## 4. What the recorder does with it

For the eqp-hub device (or any recorder):
- one `WireEvent` becomes one data or control record:
  `payload` = `Frame`, `ts_utc_ns` = `At.UnixNano()`, `mono_ns` = `At.Sub(origin)`, `epoch` = `Socket`,
  `dir` from `Direction` and the application's role, `fidelity` = `wire-exact`,
  copy fields and `decode_status` computed from the frame by the tracepack classifier;
- one `LifecycleEvent` becomes a `state-transition` transport event
  with `cause` mapped per `tracepack-go.md` §5 and `epoch` = `Socket`;
- one `TxEvent` with outcome T3 becomes a `timer-expiry` event with the primary's identifiers;
- `SocketEvent`s become `socket-connect` / `socket-accept` / `socket-close` events
  (a refused socket: accept then close, in its own epoch);
- the device's own start and stop become the capture boundaries, with a recorder instance id it mints per process.

Buffering, shipping and back-pressure are the recorder's, never go-secs's.

## 5. Not in scope

- Partial frames at disconnect (never a frame; the close event is the evidence).
- SECS-I (`secs1`): the HSMS-SS transport alone provides the wire capability;
  an observer installed on a SECS-I connection receives no wire events until a SECS-I design defines what a frame is there.
- Replacing `WithTraceTraffic`; it stays as the debugging aid it is.
- Any change to message decoding, dispatch or the FSM.

## 6. Compatibility and release

Additive API only; a minor release, go-secs v2.6.0.
eqp-hub runs go-secs v2.3.0 and must move to v2.6.0 to use any of this;
the lifecycle subscription it also needs exists since v2.4.0.
tracepack's writer phase then requires go-secs v2.6.0.

## 7. Verification

- Integration tests in `hsmsss`:
  every frame in both directions is observed in order with the right socket and generation, control frames included;
  an inbound frame with an undefined SType is observed and then rejected;
  a reconnect increments the generation and the socket counter;
  the refusal exchange yields two frames and a refused socket's accept and close under generation 0.
- Lifecycle and transaction events carry the socket, generation and session id;
  a T3 timeout event carries the primary's session id.
- Socket events: connect, accept, refuse, close once each; `Err` set only on an involuntary close.
- Observer cost: a benchmark with and without an observer on the send and receive paths;
  no allocation per outbound frame after warm-up.
- Identity races, following the patterns of `hsmsss/integration_inbound_straggler_test.go` and `integration_lifecycle_cause_test.go`:
  a send pinned to one generation that completes after a reconnect reports the pinned generation;
  a lifecycle notification queued before teardown and delivered after it keeps its generation;
  `haltRefusal` racing the refusal publication on either side still yields one refuse and one close event;
  a TCP-up the core refuses closes its socket through the gate once;
  a read error racing a local close yields one close event with the winning cause.
- `-race` for everything; `make stress-quick`, with the new race tests added to its selection pattern in the Makefile,
  because the hooks sit on connection state paths.
- External review rounds until ready, per the repository's review pipeline.

## 8. Phases

| Phase | Content | Status |
|---|---|---|
| 1 | `WithWireObserver`, socket identity, inbound and outbound call sites, refusal exchange | pending |
| 2 | `Socket`, `Generation` and `SessionID` on `LifecycleEvent` and `TxEvent` | pending |
| 3 | `WithSocketObserver` and its call sites | pending |
| 4 | README, `doc.go`, CHANGELOG; release v2.6.0 | pending |

## 9. Owner decisions

Decided 2026-09-27:
1. A panic in an observer propagates, as with `WithTransactionObserver`.
2. The outbound observer fires after the write returns successfully;
   a frame that failed mid-write is not observed, and the socket close event carries the error.
3. The refused-socket exchange is observed in the first release.
