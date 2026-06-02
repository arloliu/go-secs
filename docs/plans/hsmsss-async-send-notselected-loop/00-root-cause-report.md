# HSMS-SS reconnect loop: ungated `SendMessageAsync` while NotSelected

- **Status:** Root cause confirmed and reproduced. Fix not yet implemented.
- **Date:** 2026-06-02
- **Component:** `github.com/arloliu/go-secs` — package `hsmsss` (check `secs1` for parity)
- **Reproduction branch:** `worktree-hsmsss-recv-stall-repro`
- **Repro test:** `hsmsss/recv_stall_repro_test.go::TestRecvStall_AsyncDataSendDuringNotSelectedKillsConn`

## 1. Reported symptom

A consumer using an `hsmsss` active session reports that, when the target
peer disconnects and the application keeps sending messages, the connection
loops **`not-selected` → `not-connected`** repeatedly and never reaches
`Selected`. The symptom was described as an "internal queue full" and initially
attributed to `session.SendMessageSync`.

## 2. Triggering usage pattern

The relevant consumer pattern is a session wrapper whose send routine routes by
message kind: it sends **responses and all non-wait-bit messages via
`session.SendMessageAsync`**, and reserves `session.SendMessageSync` for
wait-bit (reply-expected) requests:

```go
// responses or non-wait-bit messages -> async (fire-and-forget)
if isResponse || !msg.WaitBit() {
    return session.SendMessageAsync(msg)
}
// wait-bit requests -> synchronous
return session.SendMessageSync(msg)
```

So the `SendMessageAsync` path is the one that matters here; `SendMessageSync`
is a red herring (see §6).

The consumer's effective workaround confirms the mechanism: gating the send
routine on the application's own connection state — refusing to send unless the
application considers the link connected — stops `SendMessageAsync` from being
called while the session is disconnected, which makes the loop disappear. This
is precisely the gate the library should apply internally (see §7).

## 3. Root cause (two coupled library defects)

### Defect 1 — `sendMsgAsync` has no `IsSelected` gate

The three outbound paths are inconsistent. The synchronous and 2-phase paths
reject data messages while the link is not Selected:

- `sendMsg` — `hsmsss/conn.go:750`:
  `if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() { ... return hsms.ErrNotSelectedState }`
- `sendMsgSync` — `hsmsss/conn.go:896`: same guard.

But the async path does **not**:

```go
// hsmsss/conn.go:953
func (c *Connection) sendMsgAsync(msg hsms.HSMSMessage) error {
    if err := c.queueSendRequest(&sendRequest{msg: msg}); err != nil {
        msg.Free()
        return err
    }
    return nil
}
```

`SendMessageAsync` → `sendMsgAsync` → `queueSendRequest` enqueues the data
message into `senderMsgChan` **regardless of connection state**.

### Defect 2 — `senderTask` treats `ErrNotSelectedState` as fatal

When `senderTask` later dequeues that data message while the link is NotSelected,
it calls `sendMsgSync`, which now returns `ErrNotSelectedState` (the §3.1 guard).
`processSendRequest` treats *any* send error as fatal and returns `false`, and
`senderLoop` then tears the connection down:

```go
// hsmsss/conn.go:1136 (senderLoop)
if !c.processSendRequest(req) {
    c.cancelSenderTask() // -> ToNotConnectedAsync()  (conn.go:1114)
    return false
}
```

So an `ErrNotSelectedState` on a *queued* message — an expected, transient
condition — is escalated to a full connection teardown.

## 4. Failure mechanism (the loop)

1. Equipment disconnects. The active host begins its reconnect cycle.
2. The application keeps calling `SendMessageAsync` for responses / non-wait-bit
   messages. Because of **Defect 1**, these enqueue into `senderMsgChan` even
   while the host is NotConnected / NotSelected (no gate).
3. On each reconnect the host enters NotSelected and starts `senderTask` and the
   select handshake (`conn_active.go:48` then `:87`). A queued stale data message
   now drives NotConnected by **either** of two independent routes:
   - **Defect 2 (sender choke):** `senderTask` dequeues the stale data message,
     `sendMsgSync` returns `ErrNotSelectedState`, and `processSendRequest`
     escalates it to `cancelSenderTask` → `ToNotConnected`.
   - **Queue starvation:** if `senderMsgChan` is saturated with stale async data,
     `selectSession`'s own `Select.req` cannot enqueue — `queueSendRequest`'s
     slow path returns `ErrSendMsgTimeout` (`conn.go:1006-1007`), `selectSession`
     returns an error, and the active NotSelected handler drives
     `ToNotConnected` (`conn_active.go:87-90`). This route does **not** require
     Defect 2 to be fatal.
4. The connection drops from NotSelected to NotConnected before (or instead of)
   completing Select. Reconnect repeats → the reported
   **NotSelected → NotConnected loop that never reaches Selected**.

Notes on the loop's persistence:

- `closeConn` drains `senderMsgChan` on every teardown (before and after the
  task-join; `conn.go:595` and `:658`) and resets the send gate, so each
  reconnect starts with an empty queue. The loop is therefore **self-sustaining
  only under the application's continuous send rate**, which refills the queue
  every connection generation — exactly the reported usage pattern.
- The repro does not depend on FIFO ordering of stale-data-vs-`Select.req`. A
  single queued async data message is sufficient: under continuous refill it
  typically precedes `Select.req` (choke route), and even if it follows
  `Select.req` it kills the connection before the (long) Select timeout would
  otherwise complete.

## 5. Reproduction

`hsmsss/recv_stall_repro_test.go::TestRecvStall_AsyncDataSendDuringNotSelectedKillsConn`:

- A raw-socket peer (`newDeafSelectPeer`) accepts and drains but never answers
  `Select.req`, holding the host in NotSelected (T6 and T7 set to 20s).
- **Contrast (teeth):** `SendDataMessage(1,1,false,…)` returns `ErrNotSelectedState`
  and does **not** drop the connection (the gated path behaves correctly).
- **Defect:** a single `SendMessageAsync(dataMsg)` is accepted (enqueued) while
  NotSelected, and the host then transitions NotSelected → NotConnected.
- **Teeth argument (source-grounded):** the drop is asserted to occur within the
  test's 3s `require.Eventually` bound, while T6/T7 are configured to 20s — so the
  transition cannot be attributed to a Select/NotSelected timeout; only the async
  send can have caused it.
- **Verified by a live run on branch `worktree-hsmsss-recv-stall-repro`** (not by
  static reading): the four `TestRecvStall_*` tests pass deterministically under
  `-race` (repeated runs) and `make lint` reports 0 issues; this test completes in
  ~0.2s.

Adjacent failure modes were also reproduced during the investigation and kept as
characterization tests (not the user's incident): a slow-consumer receiver stall,
a half-open outbound `ErrSendMsgTimeout`, and a parked-receiver FIN-miss.

## 6. Why the initial `SendMessageSync` description was a red herring

`SendMessageSync` → `sendMsgSync` writes **directly to the socket** under
`writeMutex`/`resMutex` and never touches `senderMsgChan`; it is also gated on
`IsSelected` (`conn.go:896`). An exploratory reproduction (half-open reconnecting
peer + 8 goroutines hammering `SendMessageSync`) did **not** loop: the host
reached Selected and recovered each cycle, `senderMsgChan` stayed empty, and
~3.3M sends were correctly rejected with `ErrNotSelectedState`. Only the
`SendMessageAsync` path reproduces the incident.

## 7. Proposed fix direction (not yet implemented)

These two changes are **not interchangeable**. The async gate is the required
primary fix; the senderTask hardening is a defense-in-depth backstop. (Hardening
alone is insufficient: even with `ErrNotSelectedState` made nonfatal, a
queue saturated with stale async data still starves `Select.req` via the
queue-starvation route in §4 step 3 — `queueSendRequest` → `ErrSendMsgTimeout`
→ `selectSession` error → `ToNotConnected`.)

1. **(Required, primary) Gate `sendMsgAsync` like the other two paths** — reject
   `DataMsgType` while `!IsSelected()` with `ErrNotSelectedState`, *before*
   enqueuing. This mirrors the application-layer workaround, restores consistency
   across the three send entry points, and prevents the queue from filling with
   data while NotSelected — which closes **both** §4 routes (sender choke and
   queue starvation) at the source.
2. **(Defense-in-depth) Stop treating `ErrNotSelectedState` as a fatal
   `senderTask` error** — a queued data message that becomes un-sendable because
   the link left Selected mid-flight (the residual check-then-enqueue / state-
   change race that the gate cannot fully close) should be failed back / dropped
   without tearing the connection down. Scope this narrowly: only
   `ErrNotSelectedState` becomes nonfatal; all existing fatal handling for real
   network / connection errors must be preserved.

### Open design questions

- **Ownership / error reporting:** an async send has no caller waiting; if it is
  rejected at the new gate it must be `Free()`d before returning the error.
  `sendMsgAsync` owns the message until `queueSendRequest` accepts it (the item
  is not pooled-out until then — `hsms/data_msg.go:452`), so freeing on the
  pre-enqueue rejection path is correct and carries no double-free risk. Ensure
  metrics / logging reflect the drop.
- **Control messages:** the gate must remain data-only — `Select.req`,
  `Linktest`, etc. must still enqueue while NotSelected, exactly as `sendMsg` /
  `sendMsgSync` already scope their guard to `DataMsgType`.
- **Race at the gate:** state can change between the `IsSelected()` check and the
  enqueue. The existing `sendMsg`/`sendMsgSync` guards have the same inherent
  race and are considered acceptable; the senderTask hardening (#2) is the
  backstop for the residual window.
- **`secs1` parity (confirmed):** the sibling package has the same asymmetry —
  the sync paths `sendMsg` (`secs1/conn.go:821`, gated at `:822`) and `sendMsgSync`
  (`:983`, gated at `:984`) reject while not Selected, but the async path
  `sendMsgAsync` (`:935`) does not. Fix in the same pass — see
  [[hsmsss-secs1-parity-pattern]].

### Regression tests to add with the fix

- **Async gate:** `SendMessageAsync` with a data message while NotSelected
  returns `ErrNotSelectedState`, is **not** enqueued, and does **not** drop the
  connection (the inverse of the current repro, which will then need updating).
- **No Select starvation:** a small-`senderQueueSize` active host flooded with
  `SendMessageAsync` before/while NotSelected still completes Select on reconnect
  (proves the queue no longer starves `Select.req`).
- **Sendertask backstop:** a data message that becomes NotSelected mid-flight
  (already queued, link drops before dequeue) is failed/dropped without
  `ToNotConnectedAsync` — exercises defense-in-depth #2 for the residual race.
- **Control still flows:** `Linktest` / `Select.req` still enqueue and send while
  NotSelected after the gate is added.

### Out of scope

This report does not fix the adjacent stall modes (receiver stall on a full data
channel; parked-receiver FIN-miss). They are tracked by their characterization
tests and can be addressed separately.

## 8. References

- Lib: `hsmsss/conn.go` — `sendMsg:743` (gate `:750`), `sendMsgSync:895` (gate
  `:896`), `sendMsgAsync:953` (no gate), `queueSendRequest:979` (slow-path
  `ErrSendMsgTimeout` `:1007`), `senderLoop:1120`, `processSendRequest:1148`,
  `cancelSenderTask:1114`, `closeConn:571` drains `:595`/`:658`
- Lib: `hsmsss/session.go` — `selectSession:267`; `hsmsss/conn_active.go` —
  NotSelected handler `:48`/`:87-90`
- secs1 parity: gated `secs1/conn.go:821` (`sendMsg`), `:983` (`sendMsgSync`);
  ungated `:935` (`sendMsgAsync`)
- Repro: `hsmsss/recv_stall_repro_test.go`
