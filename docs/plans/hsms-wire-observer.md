# HSMS wire observer and generation-aware events

Status: draft (2026-09-27) — proposal for go-secs v2.6.0, awaiting the owner.

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
- `Frame` is valid only during the call and must not be retained;
  the observer copies what it keeps.
  Inbound frames are the read buffer with the length prefix.
  Outbound frames are assembled once under the write lock into a per-socket scratch buffer,
  so a recording observer costs no allocation per frame after warm-up.
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
- inbound: `hsmsss/transport_recv.go` `recvLoop`, right after `readFrame` returns and before `dispatchFrame`;
- outbound: the two places the connection hands buffers to the transport byte sink,
  `hsms/connection_send.go` `writeFrame` after a successful `Write`,
  and the courtesy Separate in `hsms/connection_lifecycle.go`;
- the refusal exchange of a second passive connection (`hsmsss/transport_passive.go` `refuseExtraConn`):
  its inbound Select.req and outbound Select.rsp are observed with the refused socket's identity and generation 0.

`At` is `time.Now()` at the call site.
Go's `time.Time` carries a monotonic reading,
so a recorder derives both the wall clock (`UnixNano`) and elapsed monotonic time (`Sub` against its capture origin) from the one value.

### 3.2 Generation and session on existing events

- `LifecycleEvent` gains `Socket uint64` and `Generation uint64`:
  the socket and generation the transition belongs to,
  0 when the transition is not bound to one (`Close` before any socket, a T7 expiry whose generation is already gone).
  The supervisor already carries the generation with every generation-named commit (`commitFrom`),
  so the values are available at the injection sites.
- `TxEvent` gains `Socket uint64`, `Generation uint64` and `SessionID uint16`.
  The generation is the one the send was pinned to (`sendWaitReply` loads it once);
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

Call sites: `hsmsss/transport_active.go` after the dial,
`hsmsss/transport_passive.go` after accept and in `refuseExtraConn`,
and the generation teardown that closes the socket.
`SocketClosed` is emitted exactly once per socket.

### 3.4 Socket identity

`Socket` is a per-connection monotonic counter minted when a socket is dialed or accepted, refused sockets included,
so it never repeats within a process.
It is distinct from `Generation` (`epoch.id`), which only adopted sockets get,
so the FSM's straggler matching is untouched.
A traffic-log recorder uses `Socket` as the tracepack `epoch`,
which is defined per socket used, refused sockets included.
tracepack stores `epoch` as `u32`, and a process cannot open 2^32 sockets on one connection,
so the narrowing is safe.
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
- SECS-I (`secs1`): the same observer types apply to its normalised HSMS framing later;
  this proposal changes only the HSMS-SS transport.
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
- `-race` for everything; `make stress-quick`, because the hooks sit on connection state paths.
- External review rounds until ready, per the repository's review pipeline.

## 8. Phases

| Phase | Content | Status |
|---|---|---|
| 1 | `WithWireObserver`, socket identity, inbound and outbound call sites, refusal exchange | pending |
| 2 | `Socket`, `Generation` and `SessionID` on `LifecycleEvent` and `TxEvent` | pending |
| 3 | `WithSocketObserver` and its call sites | pending |
| 4 | README, `doc.go`, CHANGELOG; release v2.6.0 | pending |

## 9. Open questions for the owner

1. Panic policy: propagate (as `WithTransactionObserver`) or recover and disable the observer with an error log?
   This proposal says propagate.
2. Should the outbound observer fire before or after the write returns?
   After (as proposed) means a frame that failed mid-write is not observed;
   before means a frame that never reached the wire could be.
3. Is the refused-socket exchange worth observing in the first release, or can it wait for a later minor version?
