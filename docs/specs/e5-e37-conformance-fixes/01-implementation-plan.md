# SEMI E5 / E37 Conformance Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:**
Close the deviations recorded in `docs/specs/secs2-hsms-conformance-audit.md` that the project elected to fix,
document the ones it elected to keep,
and reclassify two misfiled entries in `docs/specs/e37-1-hsms-ss-conformance-audit.md` —
shipping as **v2.3.0**, a minor release that deliberately breaks behavior which was itself a specification violation.

**Architecture:**
Nine changes across four seams, largely independent —
the exceptions are Tasks 1 and 6, which share `connection_send.go` (sequenced: Task 6 runs last),
and Task 7, which adds one piece of shutdown-coordinated state to the passive transport.
*Validation seams* (Tasks 1–4) reject input the standards forbid at the exported boundary:
a send-side frame-size ceiling (`hsms.MaxMessageSize`), Reject reason-code bounds in both directions,
an odd-function requirement on the three primary-send entry points, an async W-bit refusal,
and the control-frame body-length check relocated from the transport into the decoder.
*Receive seams* (Tasks 5–6) convert two silent leniencies into **observed** ones — observation, not rejection,
because the nonconformant party on a receive path is deployed equipment that cannot be patched:
the SECS-II message-body path surfaces bytes trailing the first complete item via a new `DataMessage.TrailingBytes` accessor
(delivery unchanged everywhere, the public parser stays lenient by design),
and reply correlation gains the SEMI E37 §9.4.1 stream/function comparison it never had —
counted by default, enforced only under a new `WithStrictReplyMatching` option,
with SessionID left entirely to the existing `WithSessionIDValidation` policy,
which already drops mismatched inbound data messages before reply correlation runs.
*Connection establishment* (Task 7) replaces the passive accept-then-close refusal —
which matches none of E37 §9.2.4.1.1's three permitted options —
with option 1, a serial one-shot `Select.rsp` carrying status 1,
via a dedicated absolute-deadline `refuseExtraConn` helper
(NOT `readFrame`, whose idle path clears read deadlines — see the Design Reference).
*Documentation* (Tasks 8–10) states the deviations kept by choice,
discharges E37 §10.1's unmet items, and reclassifies the E37.1 entries.

**Tech Stack:**
Go 1.26, stdlib only (no new dependencies), `testify` for tests, existing in-package harnesses (`newEndpoint`, `echoHandler`, `ChaosProxy`, `newTestTransport`).

## Global Constraints

- Module: `github.com/arloliu/go-secs/v2`; no new dependencies.
- All tests must pass under `-race` (`make test` enables it).
  Never use `time.Sleep` to wait for state — subscribe to state events or `require.Eventually` on metrics (`.agents/rules/300-testing.md`).
  `time.Sleep` is allowed only to *inject* a delay into a scenario.
- Use `t.Context()` for test contexts; `testify` `require`/`assert`; cleanup via `t.Cleanup`/`defer`.
- Godoc: public comments may reference SEMI standards (E5 §9.2, E37 §9.4.1, T3/T6) but NEVER internal codes (D1, D7a, R2 etc.).
  Internal comments may keep such codes, matching surrounding style.
- After modifying any `.go` file: run `go fix ./...`, review its diff, then `make lint`, fix all issues, re-run until clean (`.agents/rules/700-lint-after-write.md`).
  Every commit step below implies this sequence.
- Commit messages: conventional-commit style (`fix(hsms): …`, `test(secs2): …`, `docs: …`).
  **No `Co-Authored-By` or any attribution trailer.**
- **Every regression test must be teeth-checked**: temporarily revert the fix it guards, confirm the test fails, restore.
  A test that passes against the unfixed code guards nothing.
  Record the teeth-check in the task's commit body.
- **Do NOT add methods to the exported `hsms.TransportRuntime` interface** — a compile-time break for external implementers and the in-repo mocks.

## Design Reference (read before any task)

### Release posture

v2.3.0, minor, breaking behavior freely.
The precedent is v2.2.0, released 2026-08-11, whose changelog reads
"Two of the changes are visible on the wire; read the `Reject.req` and `Separate.req` entries before upgrading."
Breaking freely applies to the **local-facing** seams (Tasks 1–4).
Tasks 1 and 3 reject only what the local caller constructs, so nothing a peer does can trip them;
Task 4 tightens the standalone decode APIs
while the live transport path keeps its existing Reject-and-hold-the-link answer;
and Task 2's inbound direction *widens* acceptance —
so none of the four changes what a connected peer can get away with.
Any caller depending on the removed behavior has a latent bug it can fix in its own code.
A blanket opt-in strictness flag for those seams was considered and rejected,
on the same reasoning the E37.1 audit used to reject `WithStrictLinktestState`:
it enlarges an already broad public surface for a behavior almost no caller wants.

The **receive-side** seams (Tasks 5–6) follow the *opposite* default,
for the same reason `WithSessionIDValidation` defaults to off:
the nonconformant party there is field equipment the caller cannot patch,
so the library is the only place tolerance can live.
Receive leniency stays the default; observation (counters, accessors) is always on;
conformant strictness is opt-in (`WithStrictReplyMatching`).
Sloppy peers keep working, and the sloppiness stops being invisible —
that inversion is deliberate, not an inconsistency with the send-side posture.

### Deviations deliberately NOT fixed

These are decisions, not oversights.
Do not "fix" them while implementing an adjacent task.

| Audit ref | Behavior | Why it stays | Where it gets documented |
|---|---|---|---|
| D5 | Reconnect backoff seeds at 100 ms, so six attempts land inside the first T5 window (cumulative 0.1, 0.3, 0.7, 1.5, 3.1, 6.3 s) where E37 §9.2.1.1 wants none | Fast reconnect is a deliberate product choice; defaulting the seed to T5 would slow recovery from ~100 ms to 10 s for every existing deployment | Task 8 — `WithReconnectBackoff` and `WithT5` godoc, naming `WithReconnectBackoff(t5, 1.0)` as the conformant setting |
| §8.1 echo | A `Select.rsp` answering a non-`0xFFFF` `Select.req` echoes the nonconformant session ID, satisfying E37 §8.3.7.1 and violating E37.1 §8.1 | The two clauses cannot both be satisfied; E37 binds the response to the request, and `TestPassive_SelectRspMirrorsRequestSessionID` already pins the echo | Task 9 — accepted deviation named in the E37.1 audit |
| Max-item limitation | Neither v1 nor v2 can carry a maximum-size SECS-II item | Raising the frame cap to fit one would loosen an inbound DoS bound chosen to reject an attacker-controlled length before allocating | Task 8 — stated as a known limit in the E37 §10.1 section |

### Size-cap arithmetic (Task 1)

`secs2.MaxByteSize` (2²⁴−1) is an **item payload** bound — the ceiling of an item's 3-byte length field.
`maxHSMSMsgLen` reuses that number as a **frame** bound compared against `msgLen` (10-byte header + body).
The two differ by the item header plus the HSMS header, so the largest legal single item does not fit:

```
max item payload   = 16,777,215   (2²⁴−1)
encoded item       = 16,777,219   (+4-byte item header)
msgLen (hdr+body)  = 16,777,229   (+10-byte HSMS header)
v2 receive cap     = 16,777,215   → does not fit
```

This is inherited, not introduced.
v1 capped `msgLen` at `MaxByteSize − MinHSMSSize` where `MinHSMSSize = LengthFieldSize + HeaderSize = 14`,
so v1's ceiling was 14 bytes *lower* still — and v1 was internally inconsistent,
with `v1:hsmsss/message_reader.go` using plain `MaxByteSize` on its other receive path.
v2 unified on `MaxByteSize`, which relaxed the stricter v1 path by 14 bytes.

**Decision:** the new send cap equals the existing receive cap, `MaxByteSize`.
Send and receive stay symmetric, no receive behavior changes, and the max-item limitation is documented rather than fixed.

**v1 parity note for the audit and changelog:** v1 had **no** outbound cap either,
and v1's list check has the identical hole — `getDataByteLength(ListType, len(values))` passes the child *count*, not the byte total,
exactly as v2's `appendHeaderBytesFC` does.
Both gaps are inherited from v1; neither is a v2 regression.

### Reply-matching semantics (Task 6)

E37 §9.4.1 requires a reply to match its primary on four fields: SessionID, Stream, Function (primary + 1, or 0), and System Bytes.
Only System Bytes is checked today (`hsms/reply_registry.go`, keyed `[4]byte`).

System Bytes correlate strongly but not perfectly:
the registry is keyed on them, and `DeliverOwnedFrame` never offers a peer's primary to the registry,
so a secondary that echoes our System Bytes but carries a wrong stream or function
is usually the *right* reply from a sloppy peer.
The documented exception is relay traffic:
`ForwardDataMessage` registers no waiter itself (it routes through `sendNoReply`),
but it sends caller-supplied System Bytes verbatim,
and those can collide with an ordinary in-flight transaction —
the peer's reply to the forwarded primary is then stolen by the wrong local waiter (the audit's D1 discussion).
A mismatched candidate CAN therefore be a genuinely wrong reply, and stream/function matching is what detects it.
Still, turning every mismatch into a default miss would convert working sloppy-peer transactions into T3 stalls:
a production-line hang caused by a library upgrade, against equipment nobody can patch.
Therefore the default **observes** and the conformant enforcement is **opt-in**:

| Field | Compared | Default (observe) on mismatch | `WithStrictReplyMatching` on mismatch |
|---|---|---|---|
| System Bytes | Always (unchanged) — the registry key; no key match, no comparison | — | — |
| Stream | Every compared candidate | `ReplyMismatchCount`++, reply still delivered | Miss |
| Function | Every compared candidate — must equal primary + 1, **or** 0 (§9.4.1 admits F0 explicitly; `isSecondaryReply` already treats SxF0 as the abort secondary, E5 §7.2, §10.4.1). The F0 exception is function-only: a wrong-stream F0 is still a mismatch | `ReplyMismatchCount`++, reply still delivered | Miss |
| SessionID | **Never in the registry.** `WithSessionIDValidation` already owns inbound session-ID policy at an earlier seam: `DeliverOwnedFrame` runs `checkSessionID`, which drops a mismatched data message and answers S9F1 *before* the secondary is offered to the registry (`hsms/connection_runtime.go`), so with validation on, a session-mismatched reply never reaches the registry and its sender already runs to T3 today. A registry rule for the same field would be dead code behind that drop | — (existing drop + S9F1, unchanged) | — (same) |

A **compared candidate** is a W-clear, even-function `*DataMessage` secondary
that hits an open System-Bytes entry registered for a data primary.
Everything else delivers uncompared:
control transactions (Select/Deselect/Linktest — `*ControlMessage` has no stream or function),
and an inbound `Reject.req` answering a data primary,
which `RouteReply` converts to a field-less `*RejectError` result (`hsms/connection_runtime.go:189`) —
a terminal answer per E37 §8.3.11, not a reply to validate.
A combined stream + function mismatch counts **once per candidate**, not once per field.

`WithStrictReplyMatching` lives in `hsms/connection_config.go`.
Its godoc names it the stream/function half of E37 §9.4.1
and states that full §9.4.1 enforcement is `WithStrictReplyMatching` **plus** `WithSessionIDValidation`,
in the same shape as the D5 treatment naming `WithReconnectBackoff(t5, 1.0)`.

**Under strict mode a mismatch is a MISS**, not an error.
The message falls through to `RouteData` and reaches the session's data handlers as unsolicited,
exactly as an orphan secondary does today.
A miss does NOT consume the registration —
it stays until `sendWaitReply` returns (`hsms/connection_send.go:227`),
so a later conforming reply still completes the transaction before the deadline.
The sender reaches **T3** and returns `ErrT3Timeout` only if no conforming reply ever arrives.
`ReplyMismatchCount` therefore counts mismatched candidates, not transactions known to have stalled.

**State this plainly in the changelog.**
Under the default, nothing changes on the wire or in delivery —
`ReplyMismatchCount` simply starts counting sloppy peers.
Under `WithStrictReplyMatching`, a peer that is sloppy about echoing stream or function
converts a transaction that previously "worked" (by receiving the wrong reply quickly)
into a stall that runs to T3 unless a conforming reply follows;
the first field symptom is a hang, not an error.
The recommended path is a soak under the default: enable strict only after the counter stays at 0.

### Primary-send paths (Task 3)

`SendDataMessage`, `SendDataMessageAsync`, and `SendSECS2Message` all mint fresh System Bytes via `s.sysGen.next()`
(`hsms/session.go:73`, `:103`, `:118`).
A message carrying fresh System Bytes always opens a **new transaction**, so it is always a primary,
so E5 §7.2's odd-function rule always applies.
This is a fact, not a policy choice.

`ReplyDataMessage` is the sole reply path and derives `primary.Function()+1` and `primary.SystemBytes()` itself (`hsms/session.go:175`–`181`).
`ForwardDataMessage` bypasses construction entirely.

**Do NOT put the odd-function check in `NewDataMessage`** — `ReplyDataMessage` and `ForwardDataMessage` share it
and legitimately carry even functions.

### Passive refusal (Task 7)

E37 §9.2.4.1.1 says a passive entity unable to service more than one connection
"will follow **one of these three** procedures":

1. Accept, but answer any subsequent Select with **Communication Already Active** — E37 calls this "the preferred option".
2. **Actively reject the connection request** (`t_snddis`), so the remote's connect procedure terminates unsuccessfully.
3. Refuse to listen or accept at all, so the remote times out.

Today's `acceptLoop` accepts the extra socket then immediately closes it,
which is **none of the three**: by the time `Accept` returns, the peer's connect procedure has already completed successfully.

**Implement option 1 — but do NOT reuse `readFrame` on the extra socket.**
`readFrame`'s `readN` explicitly **clears** any read deadline while waiting for a frame's first byte
(the idle-link policy, `hsmsss/transport_recv.go:262`),
so a dialer that connects and sends nothing would block the serial refusal loop forever,
and T8 bounds only inter-byte gaps — a slow trickle holds the socket indefinitely.
Refusal needs an **absolute** bound on the whole one-shot exchange.
Write a small per-connection helper (e.g. `refuseExtraConn(extra net.Conn)`) that:

- owns exactly one socket and closes it via its own `defer` —
  a `defer` in `acceptLoop`'s loop body would accumulate until the loop exits, not close per iteration;
- sets ONE absolute deadline covering read and write before the first read and never clears or extends it —
  `extra.SetDeadline(t.clock()().Add(t.rt.Timers().T7))` (`t.clock()` returns the injectable clock *function*,
  `hsmsss/transport.go:158`, hence the double call), returning immediately (close, no read) if `SetDeadline` errors,
  since without it the promised absolute bound does not exist; read with `io.ReadFull`, not `readN`;
- never allocates from the untrusted length: read the 4-byte prefix into a `[4]byte`,
  require `msgLen == 10` (a `Select.req` is exactly a bare header; anything else is closed unanswered),
  then `io.ReadFull` into a fixed `[10]byte` —
  the validate-before-allocating discipline of `.agents/rules/600-perf-sec.md` and `readFrame` itself;
- decodes with `decodeControlFrame` (a free function) and answers only a `Select.req`,
  writing all 14 response bytes in one bounded `Write` under the same absolute deadline;
- is interruptible at shutdown via a **two-sided handoff**, not a bare mutex-guarded field.
  A field alone has a race: `Accept` has returned but the helper has not yet published the socket;
  `Stop` closes the listener, sees no in-flight socket, and then blocks in the unbounded `g.accept.Wait()`
  (`hsmsss/transport.go:272`) while the helper settles in for the full refusal deadline.
  Both sides must act under ONE mutex guarding a `refuseStopped bool` plus the socket slot:
  the helper publishes only after checking the flag — if `Stop` already ran, it closes the socket and returns
  without reading; `Stop` sets the flag and closes whatever is in the slot.
  Whichever side wins, the socket gets closed and `Stop` never waits out the deadline.
  The flag and slot are **generation-scoped state on a transport that outlives generations**:
  reset both in `ArmStart` under the same mutex, before the successor generation can accept
  (`hsmsss/transport.go:203` already resets the other generation-scoped fields there) —
  implemented literally as never-reset transport fields, the first `Stop` leaves `refuseStopped == true`
  and poisons every later generation's refusal into close-without-response.
  Guard the `Accept`-returned/not-yet-published window deterministically with a test-only hook
  (a nil-by-default `func()` invoked between `Accept` returning and publication, in the style of the injectable clock) —
  a test that only exercises the published state cannot catch this race.

**Status code: 1 (`hsms.SelectStatusAlreadyActive`), not 3.**
E37 §9.2.4.1.1 names "Communication Already Active" literally, and Table 7 assigns that label to 1.
Status 3 ("Connect Exhaust") has the better-fitting *description* —
"the entity is already servicing a separate TCP/IP connection and is unable to service more than one at any given time" —
and was passed over deliberately, because answering 3 would mean we chose a reasoned alternative rather than implementing option 1.
**Leave a comment saying so**, or someone will "improve" it to 3 and silently cost the conformance claim.

**Refuse serially inside `acceptLoop`, not in a goroutine per dialer.**
A goroutine per dialer is an unbounded resource under a connect flood, each holding a read deadline,
and each would need registering on the generation `WaitGroup` so `Stop` joins it.
Serial refusal needs neither.
A slow dialer delays refusing the next one, bounded by the read deadline — acceptable, since only one session is ever served.

## File Structure

| File | Change |
|------|--------|
| `hsms/decode.go` | `MaxMessageSize` exported; `maxHSMSMsgLen` becomes an alias or is replaced; control-frame body-length check added to `decodeOwnedFrame` |
| `hsms/connection_send.go` | `buildFrameBuffers` returns `(net.Buffers, error)`; size ceiling enforced; reply-match fields threaded into registration |
| `hsms/connection_lifecycle.go` | `buildFrameBuffers` call site updated (farewell Separate; error impossible, control frames are 14 bytes) |
| `hsms/control_msg.go` | `GetRejectReasonCode` range widened to `[1,255]`; both Reject constructors reject reason 0 |
| `hsms/errors.go` | `ErrInvalidRejectReason` text corrected; new `ErrMessageTooLarge`, `ErrEvenFunctionPrimary`, `ErrAsyncReplyExpected` |
| `hsms/session.go` | Odd-function check on the three primary-send paths; `SendDataMessageAsync` rejects `replyExpected: true` |
| `hsms/reply_registry.go` | Value type gains stream/function + `isData` (no SessionID); `route` compares data-on-data only, counts, and misses only under strict mode |
| `hsms/connection_runtime.go` | `RouteReply` passes the reply's fields; mismatch counted |
| `hsms/connection_metrics.go` | `ReplyMismatchCount` counter |
| `hsms/connection_config.go` | New `WithStrictReplyMatching` option (default off); `WithReconnectBackoff`/`WithT5` godoc: D5 deviation + conformant setting |
| `hsms/doc.go` | New "E37 §10.1 implementation documentation" section (items 2, 4, 5, 6) |
| `hsmsss/transport_passive.go` | `acceptLoop` refuses via the serial one-shot `refuseExtraConn` helper (`Select.rsp` status 1) |
| `hsmsss/transport.go` | `Stop` participates in the two-sided refusal-socket handoff (closes the in-flight extra socket) |
| `hsms/data_msg_codec.go` | `DataMessageCodec.TrailingBytes` delegation |
| `hsmsss/transport_control.go` | `handleDeselectReq` rationale comment (E37.1 §7.3 reclassification) |
| `hsmsss/doc.go` | §10.1 item 3 rewritten to describe the now-conformant option-1 refusal |
| `secs2/decode.go` | Unexported end-position-capturing decode used by `DecodeOwnedFrame` only, reporting the trailing byte count; `Decode`/`DecodeOwned` godoc states first-item semantics |
| `hsms/data_msg.go` | Copy-fallback path routed through `DecodeOwnedFrame` so one counting entry covers both body paths; new `TrailingBytes` accessor |
| `docs/specs/secs2-hsms-conformance-audit.md` | Actions ticked, fixed deviations marked, v1 parity provenance added |
| `docs/specs/e37-1-hsms-ss-conformance-audit.md` | §7.3 and control-SessionID reclassified as accepted deviations |
| `CHANGELOG.md` | v2.3.0 section |

## Task 1 — Export `MaxMessageSize` and enforce it on the send path

Closes the outbound half of the audit's D4 item 5 and discharges E37 §10.1 item 4 programmatically.

- [ ] **Step 1: Write the failing tests** — `hsms/decode_test.go` and a new `hsms/connection_send_size_test.go`:
  - `MaxMessageSize` equals `secs2.MaxByteSize` and is exported.
  - `buildFrameBuffers` returns `ErrMessageTooLarge` for a data message whose body exceeds `MaxMessageSize - 10`.
  - `buildFrameBuffers` succeeds at exactly the boundary.
  - A control message never trips the check (always 14 bytes on the wire).
  - Construct the oversized body without allocating 16 MB where possible — a stub `Item` whose `EncodedLen`/`AppendTo` report an oversized length is preferable to a real one.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement**
  - Export `MaxMessageSize` in `hsms/decode.go` with godoc naming E37 §10.1 item 4 and stating the max-item limitation from the Design Reference.
  - Change `buildFrameBuffers` to `(net.Buffers, error)`; update ALL call sites —
    the two production ones (`connection_send.go:91`, `connection_lifecycle.go:348`)
    plus the four benchmark calls in `hsms/connection_send_bench_test.go`.
  - Remove the now-false `//nolint:gosec // n is a bounded body length` comment — the bound is real only after this change.
- [ ] **Step 4: Run tests** (`make test`)
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `fix(hsms): enforce a send-side message size ceiling`

## Task 2 — Reject reason-code bounds, both directions

Closes D2 and the D7b half of D7.

- [ ] **Step 1: Write the failing tests** — `hsms/control_msg_test.go`:
  - `GetRejectReasonCode` accepts 5, 127, 128, and 255 (E37 Table 9 reserves 4–127 for subsidiary standards and 128–255 for local entities).
  - `GetRejectReasonCode` still rejects 0.
  - `NewRejectReq(rejected, 0)` and `NewRejectReqRaw(..., 0)` return an error or a nil message — pick one shape and pin it.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement** — widen the range check in `control_msg.go`; correct `ErrInvalidRejectReason`'s text in `errors.go`, which currently states the wrong range as fact.
  Both constructors currently return `*ControlMessage` with no error; adding an error changes their signature.
  **Prefer returning `(*ControlMessage, error)`** for symmetry with `NewSelectRsp`/`NewDeselectRsp`, and update all call sites.
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `fix(hsms): correct Reject reason-code validation in both directions`

## Task 3 — Odd-function primaries and the async W-bit refusal

Closes D7a and D8.
Both live in `hsms/session.go`; do them in one task so no two workers touch that file concurrently.

- [ ] **Step 1: Write the failing tests** — `hsms/session_test.go`:
  - `SendDataMessage(ctx, 1, 2, false, item)` returns `ErrEvenFunctionPrimary`; same for `SendDataMessageAsync` and `SendSECS2Message`.
  - `ReplyDataMessage` still succeeds with its derived even function — the counter-assertion that catches an over-fix placing the check in `NewDataMessage`.
  - `ForwardDataMessage` still forwards an even-function message verbatim — same counter-assertion.
  - `SendDataMessageAsync(ctx, 1, 1, true, item)` returns `ErrAsyncReplyExpected`.
  - `SendDataMessageAsync(ctx, 1, 1, false, item)` still succeeds.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement** — guard at the three entry points in `session.go`, never in `NewDataMessage`.
  Godoc on `SendDataMessageAsync` must say why `replyExpected: true` is refused, citing E37 §9.4.1.2's "must begin a reply timer"
  and pointing at `SendDataMessage` for a reply-bearing transaction.
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `fix(hsms): reject even-function primaries and async reply-expected sends`

## Task 4 — Move the control-frame body check into the decoder

Closes D3b.

- [ ] **Step 1: Write the failing test** — `hsms/decode_test.go`: a control frame (any SType 1–7, 9) whose `msgLen` exceeds 10 is rejected by `DecodeHSMSMessage`, `DecodeHSMSPayload`, and `DecodeOwnedHSMSPayload`.
  Data messages with bodies still decode.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement** — add the length check to `decodeOwnedFrame` in `hsms/decode.go`, citing E37 §9.3.3.1.
  **Leave `hsmsss/transport_recv.go`'s check in place** — the transport must answer with a `Reject.req` and keep the link,
  which it cannot do from a decode error.
  Add a comment at each site naming the other.
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `fix(hsms): reject control frames carrying a body at the decoder`

## Task 5 — Surface trailing bytes on the message-body path, without rejecting

Addresses D3 (marked **OBSERVED** in the audit, not FIXED), **narrowed twice**:
first from the public parser to the body seam, then from rejection to observation.

**Read this before implementing — the audit's framing of D3 was wrong and the plan corrects it.**
The audit cited E5 §9.2, which defines the *item encoding* and never says a buffer holds exactly one item.
The clause that actually forbids extra items is §10.3.1.3(2)
("The message shall not contain any Lists or Data Items not shown in the Message Definition"),
and that is a **message-definition** rule which this audit places outside `secs2`'s scope.
Enforcing it in the general-purpose `secs2.Decode` would apply a §10 message rule at the item-codec layer, where it does not belong.

There is also **no safety motive**: `DecodeHSMSMessage` enforces `len(data) == 4+msgLen`,
so the body is exactly bounded and the decoder cannot over-read.
Nothing here is a crash, a panic, or a DoS vector.

What remains is a genuine but narrow defect: **silent data loss**.
A peer whose length field is self-consistent but whose body carries two items has item 2 dropped
with no error, no metric, and no way for the application to know it acted on partial data.
(A peer that is merely misframed usually trips the `len(data) != 4+msgLen` check first,
so this needs correct framing around an incorrect body.)

**But in the field, trailing bytes are overwhelmingly padding, not lost payload.**
Fixed-buffer equipment firmware that frames `msgLen` around its buffer rather than around the encoded item
produces exactly this shape — a self-consistent length field with dead bytes after one complete item —
and such equipment interoperates today precisely because the decoder ignores the excess.
Rejecting would break it on every path at once:
an inbound primary would be diverted away from the normal handlers (or fail `Item()`),
and `SendDataMessage`/`SendSECS2Message` — which force the reply's body decode before returning —
would hand every caller a non-nil error for a transaction that succeeded.
A second complete item, the case the data-loss argument actually worries about,
is illegal SECS-II (E5 §10.3.1.3(2)) that no mainstream implementation emits.

**Therefore: observe at the body seam; reject nowhere.**
The body decode records how many bytes trail the first complete item,
a new `DataMessage.TrailingBytes() int` accessor exposes the count (0 = clean),
and delivery is unchanged on every path — `DecodeErr()` stays nil for a decodable first item.
`secs2.DecodeOwnedFrame` is the right seam to count at — its argument type lives in an internal package,
so external code cannot call it, and its godoc states it exists "solely for the in-repo transport layer that frames SECS-II message bodies."
`secs2.Decode` and `DecodeOwned` stay **lenient**, with godoc stating they return the first item and ignore trailing bytes —
documented rather than silent, and no caller of any kind breaks.
A `ConnectionMetrics` counter was considered and dropped:
the body decode is lazy, so a connection-level increment would either force an eager decode on the hot path
or fire only when something else happens to trigger the decode — the accessor is the observation point.

- [ ] **Step 1: Write the failing tests**
  - `hsms/data_msg_test.go`: a data frame whose body is two concatenated items decodes to the **first** item,
    `DecodeErr()` stays nil, and `TrailingBytes()` reports the excess byte count —
    unchanged delivery is the point of the task; assert `Item()` returns item 1.
  - A body of one valid item followed by **arbitrary garbage bytes** (not a decodable second item)
    reports the garbage length — padding, not a second item, is the field case,
    and the count must never attempt to decode the excess.
  - The same body through the copy fallback path reports the same count —
    both `data_msg.go:393` and `:399` must be covered.
  - A synchronous `SendDataMessage` whose reply carries padding returns **success**,
    with the count exposed on the returned message —
    the counter-assertion that padding never fails a transaction.
  - With a decode-error handler registered, a padded-but-decodable inbound primary
    still follows **normal** delivery (`DecodeErr()` nil, not diverted).
  - When the **first item itself** fails to decode, `DecodeErr()` is non-nil and `TrailingBytes()` reports 0 —
    the count is meaningful only when `DecodeErr()` is nil; the godoc must say so.
  - A single-item body: `TrailingBytes()` is 0 and `DecodeErr()` nil.
  - An empty body still yields `NewEmptyItem()` with no error and `TrailingBytes()` 0.
  - `TrailingBytes()` called before anything else forces the lazy decode, like `DecodeErr()` does —
    pin that shape so it never silently reports 0 on a not-yet-decoded body.
  - `secs2/decode_test.go`: `Decode` and `DecodeOwned` **still accept** trailing bytes and return the first item —
    the counter-assertion that catches strictness leaking into the public parser.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement**
  - Add an unexported decode variant in `secs2` that captures `decodeItem`'s end position
    (today `Decode` discards it: `item, _, err := ...`) and returns the item plus the trailing byte count.
  - `DecodeOwnedFrame` changes shape to carry the count (e.g. `(Item, int, error)`) —
    update its callers and its tests (`secs2/decode.go:70`).
  - Thread the count into the lazy-decode state read by `TrailingBytes()`;
    `DataMessageCodec` delegates `TrailingBytes` the same way it delegates `DecodeErr`
    (`hsms/data_msg_codec.go:143`), and the accessor list in `hsms/doc.go:27` gains the new method.
  - Route `hsms/data_msg.go:399`'s copy fallback through `DecodeOwnedFrame` via `framecodec.AdoptSECS2Body(msg.body.AppendTo(nil))`
    so one counting entry covers both body paths.
    The buffer there is freshly allocated and owned, so the ownership contract holds.
  - Godoc on `Decode`/`DecodeOwned`: state the first-item-and-ignore-the-rest contract explicitly.
  - Godoc on `TrailingBytes`: name the padding-equipment diagnostic
    and cite E5 §10.3.1.3(2) as the clause the count observes.
- [ ] **Step 4: Run tests** — no fixture fallout is expected anywhere, because delivery is unchanged by design.
  If any existing test breaks, the change leaked past observation.
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `feat(hsms): surface trailing bytes in SECS-II message bodies`

## Task 6 — E37 §9.4.1 reply matching, observed by default, enforced on opt-in

Addresses D1, the audit's highest-severity finding (marked **OBSERVED** in the audit, not FIXED) —
as always-on observation plus opt-in enforcement, per the receive-side posture in the Design Reference.
Do this task **after** Tasks 1–5: it touches `connection_send.go`, which Task 1 also modifies.

- [ ] **Step 1: Write the failing tests** — `hsms/reply_registry_test.go` and an integration test:
  - **Default mode:** a reply with matching System Bytes but a different **stream** is still delivered to the waiting sender
    (no T3 stall, no fall-through to the data handlers), and `ReplyMismatchCount` increments.
  - Same for a **function** that is neither primary + 1 nor 0.
  - A combined stream + function mismatch increments the counter **once**, not twice.
  - **Strict mode** (`WithStrictReplyMatching`): the same wrong-stream reply is **not** delivered —
    it reaches the data handlers as unsolicited, `ReplyMismatchCount` increments,
    and with no conforming reply following, the sender times out on T3.
  - Strict mode with a wrong **function** (even, W-clear, not primary + 1 and not 0) — same outcome;
    wrong-stream alone must not be the only strict case pinned.
  - Strict mode, then a **conforming reply arrives after the mismatch but before T3**:
    the sender completes successfully — the miss must not consume the registration.
  - **Same-stream F0 is accepted** in both modes with no counter increment (E5 §7.2 / §10.4.1 abort secondary) —
    the counter-assertion that catches an over-strict `function == primary+1`.
  - A **wrong-stream F0** still counts (and misses under strict) — the F0 exception is function-only.
  - A correct reply is delivered in both modes, and `ReplyMismatchCount` stays 0 —
    the counter-assertion that catches a check flagging everything.
  - **`Reject.req` answering a W-bit data primary, under strict mode:** the sender returns `*RejectError` immediately,
    `ReplyMismatchCount` stays 0, and nothing waits for T3 —
    the registry entry is data-registered but the routed result is field-less (`hsms/connection_runtime.go:189`),
    so a comparison keyed on the *registration* alone would misclassify a legitimate rejection.
  - With `WithSessionIDValidation` **off**, a reply whose SessionID differs is delivered with **no** counter increment —
    SessionID is never compared in the registry.
  - With it **on**, that reply is dropped and answered with S9F1 *before* reply correlation
    (the existing pinned behavior), and `ReplyMismatchCount` stays 0 — the registry never sees it.
  - **Forwarding collision** (the audit's D1 reply-theft case): a forwarded primary whose caller-supplied System Bytes
    collide with a local in-flight transaction, answered by the peer with the forwarded message's stream/function.
    Default mode: the stolen delivery still happens (current behavior) and the counter increments.
    Strict mode: the reply routes to the data handlers, and a later conforming reply still satisfies the local waiter.
  - **REQUIRED — the control-transaction guard.** An end-to-end test that establishes a real connection
    **with `WithStrictReplyMatching` enabled** and asserts it reaches **Selected**,
    plus one that completes a linktest round trip without incrementing the linktest failure count
    and asserts `ReplyMismatchCount` stays 0 — control responses must never feed the counter either.
    No unit test of the registry can catch the control-exemption bug:
    a registry test written with data messages passes happily while every control transaction is broken.
    The guard has to live where control transactions actually run, which is a different file from the one being edited.
    Teeth-check it by removing the `isData` branch — the strict-mode connection must fail to establish,
    and the default-mode counter must wrongly increment on a `Linktest.rsp`.
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement**
  - Change `replyRegistry`'s value type from `chan replyResult` to a struct carrying the channel
    plus the primary's stream and function (no SessionID — see the Design Reference matrix).
    The new `register` parameters ripple beyond the registry's own test:
    `hsms/connection_runtime_test.go:38`, `hsms/connection_runtime_test.go:81`,
    and `hsms/harness_assert_shutdown_test.go:94` also call it and must be updated.
  - `register` takes those fields; `route` compares them before the non-blocking send,
    and only for a compared candidate: the entry was registered for a data primary
    **and** the routed result carries a `*DataMessage` —
    a `*RejectError` result (field-less, `hsms/connection_runtime.go:189`) always delivers.
    On mismatch it always increments `ReplyMismatchCount`; it returns a miss **only under strict mode**,
    otherwise it delivers as if the fields had matched.
  - **Control responses MUST be exempt. This is the highest-risk edit in the plan — read the failure trace below before writing the check.**

    `Select.rsp`, `Deselect.rsp`, `Linktest.rsp`, and `Reject.req` share this registry
    (`hsmsss/transport_recv.go:156` calls `RouteReply` for all four),
    and `sendWaitReply` registers control requests by System Bytes exactly like data (`hsms/connection_send.go:229`).
    But `*ControlMessage` has **no `Stream()` or `Function()` methods at all** — those exist only on `*DataMessage`.
    A Select transaction therefore has nothing to register and nothing to compare.

    Observe mode softens the blast radius but does not remove the requirement:
    a check that runs on control responses pollutes `ReplyMismatchCount` on every Select and Linktest round trip,
    burying the sloppy-peer signal the counter exists to surface.
    Under **strict mode** the same mistake makes every control response a **miss**, and a miss on `Select.rsp` does four things at once:
    `CommitSelected()` never runs so the connection never reaches Selected;
    we answer a valid `Select.rsp` with `Reject(TransactionNotOpen)`;
    the Select initiator's channel is never filled, so T6 expires into a communications failure and `TCPDown`;
    and T7 is never cancelled.
    Then reconnect, and repeat.

    **That is the exact shape of the v2.0.1 field bug that started this audit chain** —
    the endless `NotSelected` → `NotConnected` reconnect loop, reached through a different door.
    A `Linktest.rsp` miss independently counts toward the fail threshold and drops the link on its own.

    **The exemption has two legs, and both are required.**
    On the *registration* side, do not infer the kind inside `route` from the value type —
    the registration site already knows which it is
    (`connection_send.go:225` computes `dm, isData := msg.(*DataMessage)`);
    store that flag in the registry value and branch on it explicitly, so the intent is greppable.
    On the *result* side, a data-registered entry can still be answered by a field-less result:
    an inbound `Reject.req` arrives as `replyResult{err: &RejectError{…}}` with no message at all
    (`hsms/connection_runtime.go:189`), and comparing would misclassify a legitimate rejection
    and strand its strict-mode sender until T3.
    Compare only when both legs hold:

    ```go
    if want.isData {
        if dm, ok := result.msg.(*DataMessage); ok {
            // compare stream and function; count, and miss only under strict mode
        }
        // field-less results (RejectError) and non-data results always deliver
    }
    ```
  - Add `WithStrictReplyMatching` to `hsms/connection_config.go`, default off.
    Godoc names it the stream/function half of E37 §9.4.1
    (full §9.4.1 enforcement is this option **plus** `WithSessionIDValidation`),
    states the stall consequence against a sloppy peer — the sender runs to T3 unless a conforming reply follows —
    and recommends enabling it only after `ReplyMismatchCount` stays 0 under the default.
  - Add `ReplyMismatchCount` to `hsms/connection_metrics.go` with godoc explaining both readings:
    it counts mismatched candidates — under the default each was still delivered;
    under strict mode each was diverted to the data handlers,
    and its sender stalled to T3 only if no conforming reply followed.
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `feat(hsms): observe E37 §9.4.1 reply mismatches, enforce on opt-in`

## Task 7 — Conformant passive refusal (E37 option 1)

Closes D6.

- [ ] **Step 1: Write the failing tests** — `hsmsss/transport_passive_test.go`:
  - With a session already established, a second dialer that sends a `Select.req` receives a `Select.rsp` with status **1** and is then closed.
  - The live session is undisturbed throughout — the counter-assertion that catches a refusal path that tears down the wrong connection.
  - A second dialer that connects and sends **nothing** is closed when the absolute deadline expires,
    without leaking a goroutine — this is the teeth-check against reusing `readFrame`,
    whose idle path clears the deadline and would block forever.
  - A second dialer sending a non-`Select.req` frame is closed without a response.
  - `Stop` during an in-flight refusal returns promptly — it must not wait out the refusal deadline.
  - `Stop` racing the `Accept`-returned/not-yet-published window (driven via the test-only pre-publication hook)
    still closes the extra socket promptly — the two-sided handoff's losing side must clean up.
  - After a `Stop` and a successor-generation restart, a new extra dialer still receives the status-1 `Select.rsp` —
    the reconnect test proving a prior `Stop` does not poison later generations' refusal
    (`refuseStopped` must be reset by `ArmStart`).
- [ ] **Step 2: Run to verify failure**
- [ ] **Step 3: Implement** — the `refuseExtraConn` helper from the Design Reference,
  called serially from `acceptLoop`'s refuse loop:
  one absolute deadline for the whole exchange (exact expression and its error path per the Design Reference),
  fixed-size reads (`[4]byte` prefix, `msgLen == 10` required, `[10]byte` header — never allocate from the raw length),
  `decodeControlFrame`, and on a `Select.req`:

  ```go
  msg, err := decodeControlFrame(hdr[:])     // hdr is a [10]byte and the helper takes []byte, returning hsms.Message
  cm, ok := msg.(*hsms.ControlMessage)       // the responder pattern of transport_control.go:63
  if err != nil || !ok || cm.Type() != hsms.SelectReqType {
      return // close without a response
  }
  rsp, err := hsms.NewSelectRsp(cm, hsms.SelectStatusAlreadyActive) // (*ControlMessage, error)
  if err != nil {
      return
  }
  _, _ = extra.Write(rsp.ToBytes()) // one bounded 14-byte write; the deadline is already set
  ```

  then close.
  The helper owns the socket's `defer Close`;
  the in-flight socket participates in the two-sided `Stop` handoff from the Design Reference.
  Include the comment recording why status 3 was passed over.
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + teeth-check + commit** — `fix(hsmsss): refuse extra connections per E37 §9.2.4.1.1 option 1`

## Task 8 — Documentation for deviations kept, and E37 §10.1

Documentation only; no behavior change.

- [ ] **Step 1: Write** — no tests; this is prose.
  - `hsms/connection_config.go`: `WithReconnectBackoff` and `WithT5` godoc state that the default seed (100 ms) is non-conformant with E37 §9.2.1.1, give the cumulative attempt offsets, and name `WithReconnectBackoff(t5, 1.0)` as the conformant setting.
  - `hsms/doc.go`: new "E37 §10.1 implementation documentation" section covering item 2 (any positive `time.Duration`, nanosecond resolution — a superset of Table 10), item 4 (`MaxMessageSize`, plus the max-item limitation), item 5 (same ceiling), and item 6 (no cap on concurrent open transactions; synchronous sends are bounded only by caller goroutines).
  - `hsmsss/doc.go`: rewrite §10.1 item 3 to describe the now-conformant option-1 refusal from Task 7.
- [ ] **Step 2: Verify** — `go doc` renders each section; no internal codes leaked (`.agents/rules/400-documentation.md`).
- [ ] **Step 3: go fix + lint + commit** — `docs(hsms): document E37 §10.1 items and the T5 backoff deviation`

## Task 9 — Reclassify the E37.1 audit entries

The reclassification the project asked for, matching Gap 3's treatment exactly.

- [ ] **Step 1: Write the failing tests** — `hsmsss/transport_control_test.go`:
  - `TestDeselect_AnsweredDespiteE371Prohibition` — an inbound `Deselect.req` while Selected is answered with status 0 and transitions to NotSelected, pinning the deviation as deliberate.
  - `TestSelectRsp_EchoesNonConformantSessionID` — extend or complement the existing `TestPassive_SelectRspMirrorsRequestSessionID`
    so the echo is pinned as a *decision*,
    with a comment naming the E37 §8.3.7.1 versus E37.1 §8.1 conflict.
    The pin covers **both halves** of the deviation: accepting the nonconformant inbound session ID *and* echoing it back.
- [ ] **Step 2: Run to verify failure** (or, if they pass immediately, teeth-check by reverting the behavior they pin)
- [ ] **Step 3: Implement**
  - Rationale comment at `handleDeselectReq` naming E37.1 §7.3, §7.7 and E37 §9.1.1, in the shape of `handleLinktestReq`'s Gap 3 comment.
    **Do not reuse the T6-stranding justification** — the audit established it is not universal, since E37 §9.1.1's close would release the peer sooner.
    State the interoperability reason instead: a peer implemented against generic E37 legitimately sends Deselect,
    and answering it keeps an otherwise usable peer selected-capable
    without forcing a TCP teardown and reconnect cycle —
    do NOT claim that refusing would strand the peer; the audit disproves that
    (E37 §9.1.1's close releases it).
  - In `docs/specs/e37-1-hsms-ss-conformance-audit.md`:
    move §7.3 out of "Clauses verified conformant" into "Deviations reviewed and accepted";
    add the inbound control-SessionID leniency as a new **named** entry
    (do not rely on an ordinal — the section's entry count has already drifted once).
- [ ] **Step 4: Run tests**
- [ ] **Step 5: go fix + lint + commit** — `docs(hsmsss): reclassify the Deselect responder and control SessionID as accepted deviations`

## Task 10 — Update the audit and write the changelog

- [ ] **Step 1: Update `docs/specs/secs2-hsms-conformance-audit.md`**
  - Tick the Actions checkboxes for D1, D2, D3, D3b, D6, D7a, D7b, D8, and the outbound size bound.
  - Mark each fixed deviation **FIXED** inline with its commit, in the shape the E37.1 audit uses.
    D1 and D3 are marked **OBSERVED** instead — delivery unchanged by default, conformant enforcement opt-in (D1) or accessor-only (D3) —
    with a pointer to the receive-side posture rationale.
    For D1 and D3, revise the audit's summary lines, body text, and action recommendations to the observed posture —
    ticking the checkboxes alone would leave the bodies recommending the rejection this plan explicitly declined.
  - D5 and the §8.1 echo stay **open**, now marked documented.
  - Add the v1 parity provenance from the Design Reference: the missing outbound cap and the list hole are inherited from v1, and v2 relaxed one v1 receive cap by 14 bytes.
    The audit currently reads as though these are v2's.
- [ ] **Step 2: Write the v2.3.0 CHANGELOG section** — must state plainly:
  - This is a conformance release that **still ships a known non-conformant reconnect default** (D5, deliberate).
    Do not imply full conformance.
  - The receive-side posture: reply matching (D1) and trailing bytes (D3)
    ship as **observation with unchanged delivery**,
    because the nonconformant party is equipment the caller cannot patch.
    `ReplyMismatchCount` and `DataMessage.TrailingBytes` are the new diagnostics.
  - `WithStrictReplyMatching` is the E37 §9.4.1-conformant opt-in;
    under it a sloppy peer's fast-wrong-reply becomes a **T3 stall** — the first symptom is a hang, not an error.
    Recommend a soak under the default and enabling strict only once the counter stays 0.
  - The behavior breaks, each with the call that changes: even-function primaries,
    `SendDataMessageAsync(replyExpected: true)`, oversized sends, control frames with bodies on the decode APIs,
    and the `NewRejectReq`/`NewRejectReqRaw` signature change (a compile-time break, named as such).
- [ ] **Step 3: lint + commit** — `docs: record the E5/E37 conformance fixes and v2.3.0 changelog`

## Verification gate (before release)

- [ ] `make test` green under `-race`.
- [ ] `make lint` clean.
- [ ] Every regression test teeth-checked, each recorded in its commit body.
- [ ] `integration/` suite green — it exercises the peer simulator, which uses `NewDeselectReq` and even-function paths that Tasks 2 and 3 touch.
- [ ] No internal design codes in any public godoc.
