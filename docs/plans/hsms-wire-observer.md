# HSMS wire observer and generation-aware events

Status: active (2026-09-28) — phases 1–3 merged; phases 5 and 6 done on branch feat/hsms/observer-timing; the v2.6.0 release is pending.

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
    At         time.Time // inbound: when the frame was fully read; outbound: when its write was issued (§3.5); carries a monotonic reading
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
  The seam has two directions, and neither widens an exported interface.
  `WireEvent` and `WithWireObserver` live in `hsms`, and the option stores the observer in the shared core.
  Inbound and refusal events are raised by the HSMS-SS transport, which owns those bytes,
  through an optional runtime capability reached by type assertion, exactly as `causeRuntime` and the generation capability are today:
  `hsmsss` declares `type wireRuntime interface { ObserveWire(hsms.WireEvent) }`, the core connection implements it
  (returning at once when no observer is installed), and a runtime without it receives no wire events
  (`hsmsss` imports `hsms`, so it constructs the event without a cycle; the existing generation capability is not touched).
  Outbound events are raised by the core at its two write sites, but only when the transport asserts an optional capability
  `WireReportScope(socket uint64) func()` (a method the HSMS-SS transport implements; discovered by type assertion on the transport):
  the SECS-I transport reports success for an HSMS control frame without any wire I/O and converts data frames to SECS-I blocks,
  so an unconditional hook in the shared core would report frames that never crossed a SECS-I socket.
  The capability also opens the socket's report scope (§3.3) around the write and returns its release,
  a function the transport holds per socket so the call allocates nothing, or nil once the socket's close is pending:
  the core holds the scope from just before the write until after the frame's report,
  and releases it at once when the write fails, and on a panic in the observer.
  An inbound event is raised with the receive goroutine's generation before dispatch, so a frame the admission check later drops is still reported as read.
- the refusal exchange of a second passive connection (`hsmsss/transport_passive.go` `refuseExtraConn`),
  which reads and writes the extra socket directly:
  the inbound Select.req is observed as the full 14-byte frame after the header read completes, before it is interpreted,
  invalid headers included;
  the outbound Select.rsp is observed only when the write returned the full frame length without error;
  both carry the refused socket's identity and generation 0.
  The exchange runs on the accept goroutine, which `Stop` joins without a bound, so it never calls the observer:
  it captures each frame as it crosses, the inbound Select.req with its `At` taken when its header read completed
  and the outbound Select.rsp with its `At` taken immediately before its write (§3.5), kept only when the write returned the full frame length,
  and once it has ended hands both frames, still under the socket's report scope, to one goroutine
  that reports them in wire order and then releases the scope.
- the courtesy Separate runs on the supervisor goroutine, whose teardown `Close` waits for, so it never calls the observer either:
  it copies the frame before the write (14 bytes, one allocation per graceful close, never the shared scratch buffer),
  takes `At` immediately before the write (§3.5),
  and after a successful write hands the report to a goroutine of its own that holds the socket's report scope until it has reported.
  Nothing joins either delivery goroutine; an observer that never returns leaks it and delays only that socket's close event.

`At` is `time.Now()` at the call site, or at the capture for a frame reported from a delivery goroutine;
for an outbound frame it is taken when the write is issued, not when it returns (§3.5).
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
  a transition of an epoch that never acquired a socket carries its generation and socket 0.
  The epoch keeps its socket id after teardown clears its connection, so an event built late still names the socket.
  A stale queued report is discarded by the supervisor and fires no transition;
  a notification already fired keeps its original generation even when the notifier delivers it after that generation tore down.
- `TxEvent` gains `Socket uint64`, `Generation uint64` and `SessionID uint16`.
  `sendWaitReply` and `sendNoReply` load the pinned epoch internally and the event is built after they return,
  so a fresh read there could name a successor after a reconnect;
  the send path returns the epoch it pinned, and the event is built from that.
  A send that found no epoch (`ErrNotOpen`) reports generation 0 and socket 0;
  every return after an epoch was selected, including the pre-write refusals, write errors, timer, teardown and cancellation returns, carries that epoch's generation
  and the socket id it retains after its connection is closed.
  `SessionID` is the primary's session id, which a T3 record needs and the event does not carry today.
  A late reply arriving after a T3 timeout needs no event:
  it is a frame, and the wire observer reports it.

### 3.3 Socket events

```go
type SocketEventKind uint8

const (
    SocketConnected SocketEventKind = iota + 1 // active role: dial succeeded
    SocketAccepted                             // passive role: accept returned
    SocketRefused                              // passive role: accepted, then rejected by the extra-connection policy, whether or not the response reached the peer
    SocketClosed                               // the socket is closed, by either side or by teardown
)

type SocketEvent struct {
    Kind       SocketEventKind
    Socket     uint64
    Generation uint64      // 0 until adopted, and always 0 for a refused socket
    At         time.Time
    Local      net.Addr
    Remote     net.Addr
    Err        error       // for SocketClosed: the first I/O or protocol failure that initiated the close; nil for a local or peer-requested close
    Cause      TransitionCause // for SocketClosed: the cause half of the winning close reason (§3.5), which may differ from the lifecycle cause; CauseUnknown for the other kinds
}

func WithSocketObserver(fn func(SocketEvent)) ConnOption
```

The socket record lives in the HSMS-SS transport, at the socket boundary,
because sealed dials, TCP-ups the core refuses and extra passive sockets close without ever being adopted by an epoch.
It is minted at every successful dial or accept, refused sockets included, and owns one close gate:
an idempotent close that records the first initiating error it is given, closes the connection, and emits `SocketClosed` once.
Socket events reach the core-held observer through a second optional runtime capability,
`type socketRuntime interface { ObserveSocket(hsms.SocketEvent) }`, asserted and called by the transport like `wireRuntime`.

Three interface paths, all optional and additive:
- *Adoption.* The transport reports an adopted socket through a new optional runtime capability
  `AdoptSocketFromGeneration(gen uint64, conn net.Conn, socket uint64) bool`, which the core implements next to `TCPUpFromGeneration`
  and which stores the socket id on the epoch; `TCPUp` and `TCPUpFromGeneration` keep their signatures, so the SECS-I transport,
  which only calls `TCPUp`, and any external caller are unaffected.
  The transport uses the new method when the runtime has it and falls back to the existing one otherwise.
- *Epoch close.* The epoch's socket close, which today closes the connection directly before the transport is stopped,
  first asserts an optional transport capability `CloseSocket(conn net.Conn, err error, cause TransitionCause)`;
  when present it calls the gate with the close reason the epoch recorded (below and §3.5), otherwise it closes the connection directly as it does now.
  Transport `Stop`, a sealed dial, a refused TCP-up, and the refusal cleanup and `haltRefusal` paths call the same gate,
  so every socket closes through it and `SocketClosed` is emitted once.
- *Failure first.* Every failure that initiates a socket's closure reaches the gate with its error before teardown can reach it with nil,
  through three routes rather than per-site calls:
  every involuntary disconnect the HSMS-SS transport reports goes through its `tcpDown` helper with an error and a cause
  (read errors, a failed Select on the active side, a linktest failure), and that helper calls the gate with the error before reporting the down,
  except for a peer Separate, where the helper passes nil to the gate while keeping the sentinel it reports to the core, because that close is peer-requested;
  the core's write-error path records the write error on the epoch before it initiates teardown, and the epoch's close passes it to the gate;
  and a T7 expiry, which does not go through `tcpDown` and which the FSM may discard when a Select wins the race,
  closes nothing at report time: the teardown that the winning T7 transition triggers records the T7 error on the epoch, and the epoch's close passes it to the gate.
  The refusal exchange closes through one deferred gate call that takes the error retained by whichever branch returned:
  the absolute deadline, a short read, a malformed length or header, or a failed response write; a completed exchange retains nil.
  A local close with no recorded failure passes nil.
  A race between a read error and a local close is decided by the first caller; the outcome is one close event with that caller's error.
- *Frames before the close.* Every frame reported for a socket is reported before that socket's `SocketClosed`,
  and no frame is reported after it; nobody waits to make that so.
  The record counts its open report scopes under a mutex held only for that bookkeeping:
  a goroutine that moves a frame across the socket
  (the receive loop around its read, the core's write sites around their write, the refusal exchange as a whole)
  opens a scope before the I/O and releases it after the frame's report, on a panic in the observer too.
  The gate's winner marks the record closing as it is decided, which refuses every later scope,
  then closes the connection, which fails any I/O in progress, and takes the close time,
  then arms the close report and emits `SocketClosed` itself only when no scope is open;
  otherwise the goroutine that releases the last open scope emits it, once its observer call has returned.
  A scope requested after the mark returns nothing and the frame is moved without being reported.
  The closer never blocks on a frame report: no caller of the gate ever waits on the wire observer,
  so the supervisor's teardown stays a non-blocking initiator, and `Close` and `Stop` never wait on the wire observer either.
  The socket observer is different by design: its events are delivered synchronously on the goroutine
  that dialed, accepted, refused or closed the socket, the connection's own lifecycle goroutine included,
  and `Close` and `Stop` wait for a call in progress on those goroutines to return;
  a `SocketClosed` held back behind a frame report is delivered by the reporting goroutine instead,
  and `Close` waits for it only through that goroutine's join: bounded for the receive goroutine and a sender,
  not at all for the detached goroutine delivering a courtesy Separate or a refused peer's frames.
  A socket has two or three such events, none on the traffic path,
  so the contract stays "cheap and returning" rather than adding an ordered delivery goroutine per socket;
  a socket observer that never returns on one of those goroutines holds `Close`.
  The two frames whose goroutines `Close` joins without a bound, the courtesy Separate and a refused socket's exchange,
  are delivered from a goroutine of their own that holds the scope (§3.1), so those goroutines never call the observer.
  An observer that never returns delays only that socket's `SocketClosed`;
  when it is parked on the receive goroutine or a sender it also delays the bounded teardown join,
  which `Close` reports as `ErrCloseTimeout`, never `Close` itself;
  when it is parked on a delivery goroutine it leaks that goroutine and nothing else.
  The close is delivered by the gate's winner when no scope is open, otherwise by the goroutine releasing the last scope,
  and its `At` is the time the winner closed the connection, whichever goroutine delivers it and however late.
  An observer must still not send on the connection from inside a call in either direction:
  an outbound call holds the write lock, and an inbound call parks the receive goroutine that would read the reply.

The raw socket's buffered `WriteTo` fast path is untouched, because the gate wraps closing, not writing.
`SocketRefused` is emitted as soon as the extra-connection policy rejects the socket, before the exchange begins,
so a recorder sees accepted, refused, the exchange's frames, closed;
the `SocketClosed` that follows carries the retained failure (deadline, short read, malformed data or failed write) and nil for a completed exchange or one halted by a local `Stop` (`haltRefusal`),
which is the `Err` contract of §3.3 applied to that socket.
The knowledge note on the passive refusal exchange (`haltRefusal` closes the socket directly) is updated with the mechanic.
Call sites: `hsmsss/transport_active.go` after the dial, `hsmsss/transport_passive.go` after accept and in `refuseExtraConn`, and the gate.

### 3.4 Socket identity

`Socket` is a per-connection monotonic counter minted when a socket is dialed or accepted, refused sockets included.
It is unique within one connection, not across connections of a process;
a recorder that serves several connections keys on the connection as well.
It is distinct from `Generation` (`epoch.id`), which only adopted sockets get, so the FSM's straggler matching is untouched;
the generation-scoped refusal handoff token is not reused for it, because that token resets with every generation.
A traffic-log recorder derives the tracepack `epoch`, which is defined per socket used, refused sockets included, from `Socket`:
it keeps a per-capture `u32` counter keyed by the connection and `Socket` values it has seen, starting at 1,
so the width of this counter never reaches the archive.
A capture whose counter would exceed 2^32 − 1 ends with a `stop` boundary and a recorder error, and the recorder starts a new capture with a fresh counter;
the mapping is per capture, so the new capture's epochs are unrelated to the old one's, as the archive already expects across captures.
Both counters restart with the process, as tracepack expects: a recorder restart is a new capture.

### 3.5 Event times and the close cause

A recorder prototype built on the merged API (phases 1–3) recorded every scenario of §7 into tracepack packs that validated,
but it could not put records in causal order, and it had to match error text to name a socket's close cause.
Three changes close that, all inside the unreleased v2.6.0 surface.

**Outbound `At` is taken when the write is issued.**
The wire hooks run on different goroutines, so a recorder orders records by `At`, not by arrival.
Today an outbound `At` is taken after `Write` returns, after the send metrics and the `WithTraceTraffic` dump;
under load the peer's reply was fully read before the primary's `Write` returned,
and the prototype saw a reply's `At` up to 170 µs before its primary's.
Taking `At` when the write is issued makes the order causal:
a frame the peer sends in response is read only after the frame it responds to was handed to the socket.
The rule applies at every outbound site:
`writeFrame` takes it after the write lock and the report scope are held, immediately before `Write`;
the courtesy Separate and the refusal exchange's Select.rsp take it immediately before their writes.
The observer still fires only after the write succeeded (owner decision 2);
only the time moves, and a frame whose write fails is still not reported.
Inbound `At` is unchanged: when the frame was fully read.
The time rule of `tracepack-semantics.md` §4 ("when the complete frame was observed") gains the matching clarification,
that a frame the vantage sends itself is observed complete when it is handed to the socket (phase 6).

**`LifecycleEvent` gains `At time.Time`.**
It is the time the supervisor fired the transition,
taken with the time's monotonic reading when `Socket` and `Generation` are snapshotted into `stateChange` (§3.2),
never on the notifier goroutine.
The notifier delivers events late whenever a state-change handler or subscriber ahead of it is slow,
and eqp-hub's own state handler sends several messages synchronously there;
a recorder that stamped at receipt placed transitions up to 90 ms late in the prototype.
The stamp is taken where the supervisor's `step` builds the `stateChange` it fires, the coalesced bring-up reported as a Select included,
so a coalesced notification carries the `At` of the transition it reports;
a stale report the supervisor discards fires nothing and has no time,
and a `Close` that tears an epoch down without a transition emits no event.

**`SocketEvent` gains `Cause TransitionCause`.**
On `SocketClosed` it names why the socket closed;
on the other kinds it is `CauseUnknown`.
`Cause` and `Err` are one pair, the **close reason**, and they always come from the same initiator:
the gate records the first close reason it is given, and a later caller changes neither half.
The recorder maps `Cause` to the `cause` of a `socket-close` record without matching error text,
since the linktest-failure and select-rejected errors are unexported.

Where a close reason comes from:
- *The transport's own reports.* `tcpDown` already holds the error and the transition cause it reports to the core, and passes both to the gate
  (a peer Separate keeps its nil error, §3.3).
- *The epoch.* The epoch holds one close reason: an error, a cause and a set flag, written once under the epoch's lock,
  so a later record changes nothing, a `(nil, CauseLocalClose)` pair included.
  Every path that tears an epoch down records its reason immediately before its `teardown` call, and the first record wins:
  - the core's write-error path records (the write error, `CauseIOError`) before it initiates teardown, as it records the error today;
  - the drop reaction (`react`) records (the T7 error, `CauseT7Timeout`) for a winning T7 transition, and (nil, the transition's cause) for any other, for example `CauseHandlerExit`;
    a write that fails between the T7 transition and this record has already set the reason,
    so the socket then reports `CauseIOError` while the lifecycle event reports `CauseT7Timeout`;
  - the supervisor's `evClose` handler in `step` records (nil, `CauseLocalClose`) before its teardown;
    `requestClose` only pins the epoch and enqueues, and records nothing;
  - a failed `Start` after the TCP-up committed on the reconnect loop (`connectLoopStartFailure`),
    and `Open`'s direct teardown of an epoch its rollback did not pin,
    record (the `Start` error, `CauseUnknown`), the cause the reconnect loop reports for that failure;
    the epoch an initial `Open` rollback pinned is closed through `requestClose` and the `evClose` handler,
    so it records (nil, `CauseLocalClose`), matching the lifecycle cause that rollback reports;
  - an epoch that never adopted a socket (a dial that failed, `Open`'s cold background branch) has no socket to report, so its reason is never used.
  The epoch's close passes the pair to the gate through the `socketCloser` capability,
  now `CloseSocket(conn net.Conn, err error, cause TransitionCause)`;
  the capability is unexported and unreleased, so its signature changes in place.
- *Sockets no epoch adopted.* The transport closes them itself with a reason of its own (table below).

Because the gate and the epoch keep the first reason, a socket's `Cause` can differ from the `Cause` of the lifecycle transition that ended its generation:
a write error recorded just before a concurrent `Close` wins the transition yields a `SocketClosed` with `CauseIOError` and the write error,
while the lifecycle event reports `CauseLocalClose`.
The socket event says what closed the socket first; the lifecycle event says what drove the state machine.

| How the socket closed | `Cause` | `Err` |
|---|---|---|
| read error on an adopted socket, or the peer closed its side without Separate, at a frame boundary or mid-frame | `CauseIOError` | the read error; `errors.Is(Err, io.EOF)` for a peer close |
| peer Separate | `CausePeerSeparate` | nil |
| failed active Select | the cause the transport reports (`CauseSelectRejected`, `CauseT6Timeout` or `CauseIOError`) | the failure |
| linktest failure | `CauseLinktestFail` | the failure |
| write error on the core's send path | `CauseIOError` | the write error |
| T7 expiry that won against a Select, when no earlier reason was recorded | `CauseT7Timeout` | the T7 error |
| a reconnect `Start` that failed after the TCP-up committed, when no earlier reason was recorded | `CauseUnknown` | the `Start` error |
| an initial `Open` whose `Start` failed after the TCP-up, rolled back, when no earlier reason was recorded | `CauseLocalClose` | nil |
| another teardown with a transition cause of its own, such as a handler that exited | that transition's cause (`CauseHandlerExit`) | nil |
| local `Close` or `Stop`, with or without a transition, and a dial the transport sealed because it was stopping | `CauseLocalClose` | nil |
| a TCP-up the core refused to adopt (a report from an ended generation, or a second TCP-up for a live one) | `CauseUnknown`: the core gives the transport no reason | nil |
| an extra passive peer refused by policy, exchange completed, or halted by `Stop` (`haltRefusal`) | `CauseLocalClose`: this side decided to close it; the policy itself is reported by `SocketRefused` | nil |
| an extra passive peer refused by policy, exchange failed (a read or write failure, the exchange deadline, a malformed length or header) | `CauseIOError`: the refusal exchange treats a malformed length or header as a failed exchange, as the adopted receive path treats an invalid frame length (other malformed frames there get a Reject and keep the link) | the retained failure; a peer that closed before sending its complete Select.req gives `io.EOF` or `io.ErrUnexpectedEOF` |

`SocketEvent.Err`'s documentation gains the peer-close sentence of the first row,
which holds on an adopted socket's receive path, whose reads return the socket's error unchanged;
tests pin it for a close at a frame boundary and a close mid-frame.
It does not extend to the refusal exchange, whose reads use `io.ReadFull`.

Not in this change, candidates for a later minor release:
`TxEvent.At`, since the transaction observer already runs synchronously when the outcome is known,
and an exported count of coalesced lifecycle notifications.

## 4. What the recorder does with it

For the eqp-hub device (or any recorder), with `epoch` always the per-capture value §3.4 maps from `Socket`:
- one `WireEvent` becomes one data or control record:
  `payload` = `Frame`, `ts_utc_ns` = `At.UnixNano()`, `mono_ns` = `At.Sub(origin)`,
  `dir` from `Direction` and the application's role, `fidelity` = `wire-exact`,
  copy fields and `decode_status` computed from the frame by the tracepack classifier;
- one `LifecycleEvent` becomes a `state-transition` transport event with `cause` mapped per `tracepack-go.md` §5;
- one `TxEvent` with outcome T3 becomes a `timer-expiry` event with the primary's identifiers;
- `SocketConnected` / `SocketAccepted` / `SocketClosed` become `socket-connect` / `socket-accept` / `socket-close` events;
  a refused socket yields, in its own epoch, `socket-accept`, whichever of its two frames were actually observed (none, one or both),
  a `state-transition` record from `not-connected` to `not-connected` with cause `select-rejected` and `cause_raw` `go-secs:SocketRefused`,
  and `socket-close`.
  The transition record's source is the `SocketRefused` notification.
  The tracepack semantics today admit only a state-change notification as the source of a `state-transition` record,
  while the format requires a transition record for an accepted-then-refused socket without naming its source;
  this proposal therefore commits to the wording change in `tracepack-semantics.md` §5 that also permits an implementation's socket-refusal notification,
  carrying the socket's identity, to source the `not-connected` to `not-connected` record,
  and the Go mapping's cause table (`tracepack-go.md` §5) gains the `SocketRefused` row when this proposal is implemented;
- the device's own start and stop become the capture boundaries:
  the recorder keeps one `recorder_instance_id` for its deployment across restarts and mints a new `capture_id` per process start,
  as the durable-bus deployment requires (`tracepack-storage.md` §4);
- `ts_utc_ns` of a `state-transition` record comes from `LifecycleEvent.At`, and `socket-close` takes its `cause` from `SocketEvent.Cause`,
  with `peer-close` for a `CauseIOError` close whose `Err` satisfies `errors.Is(Err, io.EOF)`
  (or `io.ErrUnexpectedEOF` on a refused socket) and `transport-error` for any other `CauseIOError`;
- go-secs makes the times causal (§3.5), not the delivery:
  the hooks run on different goroutines, an outbound report follows the write, and the courtesy Separate and the refusal frames are reported from goroutines of their own,
  so an event can reach the recorder after events that happened later, with no bound on the delay.
  A recorder that sorts by `At` within a window before it assigns `seq` gets causal order for every event that arrives within the window;
  one that arrives later is recorded where it arrives,
  and a reply recorded before its primary is reported by transaction matching as an anomaly (`tracepack-semantics.md` §7.2).
  How such a late record is marked is a tracepack question, recorded in the Go mapping's open questions (phase 6).

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
- Refusal recorder cases with zero and one observed frame, next to the completed two-frame exchange.
- The T7 error is attached to the epoch only after the T7 transition wins, never at report time; a test races a Select against T7 and checks the close error.
- Event times and the close cause (§3.5):
  an outbound frame's `At` precedes the return of a `Write` held open by a test transport,
  at `writeFrame`, the courtesy Separate and the refusal Select.rsp;
  a request/reply burst over loopback never reports a reply's `At` before its primary's;
  a `LifecycleEvent.At` precedes the delivery delay a slow state-change handler registered ahead of the subscriber adds,
  and a coalesced bring-up reported as a Select carries the time its transition fired;
  one test per row of the §3.5 table checks `SocketClosed`'s `Cause` and `Err`,
  including a peer close at a frame boundary and mid-frame (`errors.Is(Err, io.EOF)`), a reset (not `io.EOF`),
  a `Close` that tears an epoch down with no transition (`CauseLocalClose`),
  and a TCP-up the core refuses (`CauseUnknown`);
  a read error racing a local close, a write error racing a `Close` that wins the transition, and a write error racing a winning T7,
  each yield one close event whose error and cause come from the same initiator,
  the last two with the socket's `CauseIOError` differing from the lifecycle event's `CauseLocalClose` or `CauseT7Timeout`;
  a reconnect whose `Start` fails after the TCP-up committed closes its socket with `CauseUnknown` and the `Start` error,
  and an initial `Open` rolled back after the same failure closes it with `CauseLocalClose`,
  each unless the transport reported the failure first.
- External review rounds until ready, per the repository's review pipeline.

## 8. Phases

| Phase | Content | Status |
|---|---|---|
| 1 | `WithWireObserver`, socket identity, inbound and outbound call sites, refusal exchange | done |
| 2 | `Socket`, `Generation` and `SessionID` on `LifecycleEvent` and `TxEvent` | done |
| 3 | `WithSocketObserver` and its call sites | done |
| 4 | README, `doc.go`, CHANGELOG; release v2.6.0, after phases 5 and 6 | in-progress |
| 5 | Outbound `At` at write issue, `LifecycleEvent.At`, `SocketEvent.Cause` (§3.5) and their tests (§7); Godoc, `README.md`, `hsms/doc.go`, `hsmsss/doc.go`, CHANGELOG, and the knowledge notes on transition-cause injection sites and the passive refusal exchange | done |
| 6 | `tracepack-semantics.md` §4: a frame the vantage sends itself is observed complete when it is handed to the socket, with a spec changelog entry; `tracepack-go.md` brought up to the v2.6.0 API: socket and generation identity, the `SocketRefused` row, the close-cause mapping of §4, the answered open questions, and a new open question on marking a record that reached the recorder after its window | done |

## 9. Owner decisions

Decided 2026-09-27:
1. A panic in an observer propagates, as with `WithTransactionObserver`.
2. The outbound observer fires after the write returns successfully;
   a frame that failed mid-write is not observed, and the socket close event carries the error.
3. The refused-socket exchange is observed in the first release.

Decided 2026-09-28:
4. The socket observer stays a synchronous hook on the dialing, accepting, refusing and closing goroutines,
   and `Close` and `Stop` wait for a call in progress on those goroutines;
   a `SocketClosed` delivered by the goroutine that ended the last frame report follows that goroutine's join instead,
   bounded for the receive goroutine and a sender, none for a detached delivery goroutine.
   The wire observer never holds `Close` or `Stop`.
   No ordered delivery goroutine per socket.

Decided 2026-09-28, after the recorder prototype:
5. Three changes land before the v2.6.0 tag (§3.5):
   an outbound frame's `At` is taken when its write is issued, while the observer still fires only after the write succeeded;
   `LifecycleEvent` carries `At`, the time the transition fired;
   `SocketEvent` carries `Cause`, the disconnect cause of a `SocketClosed`.
   `TxEvent.At` and an exported count of coalesced lifecycle notifications wait for a later minor release.
