# Implementation plan: gate async sends while NotSelected

- **Status:** Ready for review (root cause approved; see `00-root-cause-report.md`).
- **Date:** 2026-06-02
- **Component:** `github.com/arloliu/go-secs` — `hsmsss` (primary), `secs1` (parity).
- **Depends on:** `00-root-cause-report.md` (root cause + fix direction, externally reviewed to consensus).

## 1. Objective & success criteria

Stop an active `hsmsss` session from looping `not-selected → not-connected`
when the application keeps sending non-wait-bit / response messages via
`SendMessageAsync` while the link is down.

Success criteria (all verifiable):

1. `SendMessageAsync` (and `SendDataMessageAsync`) with a **data** message while
   `!IsSelected()` returns `hsms.ErrNotSelectedState`, does **not** enqueue, and
   does **not** drop the connection — identical to `sendMsg` / `sendMsgSync`.
2. A queued data message that becomes un-sendable because the link left Selected
   mid-flight (the residual check-then-enqueue race) is dropped without tearing
   the connection down.
3. Control messages (`Select.req`, `Linktest`, etc.) still enqueue and send while
   NotSelected — unchanged.
4. The reconnect loop no longer reproduces: an active host flooded with async
   data sends across a disconnect still completes Select on reconnect.
5. `secs1` no longer exhibits the same async-gate asymmetry.
6. `go test ./hsmsss/ ./secs1/ -race` green; `make lint` 0 issues; full suite
   (skip `^Fuzz`) green.

## 2. Design decisions & invariants

- **Primary fix = the gate** (`sendMsgAsync`). It is what prevents the queue from
  filling with data while NotSelected, closing both §4 routes of the report
  (sender-choke and Select.req starvation) at the source.
- **Defense-in-depth = senderTask hardening.** Narrowly scoped: only
  `ErrNotSelectedState` becomes non-fatal; every existing fatal path (real
  network / connection errors) is preserved unchanged.
- **Gate scope = data messages only.** Mirror the existing `sendMsg`
  (`conn.go:750`) and `sendMsgSync` (`conn.go:896`) guard exactly
  (`msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected()`). Control
  messages must remain queueable while NotSelected — the Select handshake itself
  depends on it.
- **Ownership.** On gate rejection the message must be `Free()`d before
  returning, exactly as the existing `queueSendRequest`-error path in
  `sendMsgAsync` already does. `sendMsgAsync` owns the message until
  `queueSendRequest` accepts it, so a single `Free()` on the reject path is
  correct and cannot double-free.
- **Behavior change (intended, must be documented).** Callers of
  `SendMessageAsync` now receive `ErrNotSelectedState` while not Selected, where
  previously the message was silently enqueued (and later dropped or fatal).
  This matches the contract already honored by `sendMsg`/`sendMsgSync` and is the
  fix the consumer applied at their own layer.
- **Metrics (as-built decision).** Not-Selected drops are **expected
  backpressure, not faults**, so they are counted by a dedicated counter
  `DataMsgDropNotSelectedCount` (added to both `hsmsss` and `secs1`
  `ConnectionMetrics`), incremented in **both** the gate rejection and the
  Phase-2 drop. They are **no longer** counted under `DataMsgErrCount` — reusing
  the error counter would pollute error-rate alerts with high-volume benign
  events during outages and make it non-actionable. New exported field =
  additive, backward-compatible. (Considered and rejected: reuse
  `DataMsgErrCount`; silent/no-count.)

## 3. Phase 1 — hsmsss primary fix: gate `sendMsgAsync`

`hsmsss/conn.go:953` — add the data-message selected-state guard before enqueue:

```go
func (c *Connection) sendMsgAsync(msg hsms.HSMSMessage) error {
	// Mirror sendMsg (conn.go:750) and sendMsgSync (conn.go:896): a data
	// message cannot be sent while not Selected. Gating here (rather than only
	// at dequeue) keeps senderMsgChan from filling with undeliverable data
	// while NotSelected, which would both choke senderTask and starve Select.req.
	if msg.Type() == hsms.DataMsgType && !c.stateMgr.IsSelected() {
		msg.Free()
		return hsms.ErrNotSelectedState
	}

	if err := c.queueSendRequest(&sendRequest{msg: msg}); err != nil {
		msg.Free()
		return err
	}

	return nil
}
```

No other callers change: `SendMessageAsync` (`hsmsss/session.go:61`) and the
non-wait-bit branch of `sendMsg` already funnel through here. `sendMsgAsync` is
only reachable for fire-and-forget sends, so there is no waiting caller whose
contract changes beyond the documented error return.

## 4. Phase 2 — hsmsss defense-in-depth: non-fatal `ErrNotSelectedState` in `processSendRequest`

`hsmsss/conn.go:1148` — when `sendMsgSync` returns `ErrNotSelectedState` for a
queued message (only possible via the residual check-then-enqueue race now that
Phase 1 gates the common path), signal the caller as today but keep the
senderTask alive:

As built (note `incDataMsgErrCount` is moved below the `ErrNotSelectedState`
branch so a drop is not counted as an error — see the Metrics decision in §2):

```go
	err := c.sendMsgSync(msg)
	if err != nil {
		// Signal the caller that send failed.
		if req.sentChan != nil {
			req.sentChan <- err
		} else {
			c.replyErrToSender(msg, err)
		}

		// A queued data message that became un-sendable because the link left
		// Selected mid-flight is expected backpressure, not a fault — count it
		// as a drop, keep the sender task alive, do NOT escalate to teardown.
		// Phase 1 makes this path rare (race-only); this is the backstop.
		if errors.Is(err, hsms.ErrNotSelectedState) {
			c.metrics.incDataMsgDropNotSelectedCount()
			c.logger.Debug("dropping queued message: not selected",
				hsms.MsgInfo(msg, "method", "senderTask")...)
			return true
		}

		c.metrics.incDataMsgErrCount()
		if !isNetOpError(err) {
			c.logger.Error("failed to send message", "method", "senderTask", "error", err)
		} else {
			c.logger.Error("network error sending message", "method", "senderTask", "error", err)
		}

		return false
	}
```

**Resolved decision (was open):** for the async case (`req.sentChan == nil`)
`replyErrToSender(msg, err)` synthesizes a `NewErrorDataMessage` delivered to the
session handler. **Option (A) was chosen** — keep `replyErrToSender` as-is for
minimal blast radius (preserves existing error-reporting behavior). Revisit only
if the synthesized error proves noisy in practice.

## 5. Phase 3 — secs1 parity

`secs1/conn.go:935` (`sendMsgAsync`) has the same asymmetry: `sendMsg`
(`:821`, guard `:822`) and `sendMsgSync` (`:983`, guard `:984`) gate on
`!c.stateMgr.IsSelected()` (returning the secs1-local `ErrNotSelectedState`,
`secs1/conn.go:56`), but `sendMsgAsync` does not.

- Add the analogous guard to `secs1.sendMsgAsync`, matching the **existing secs1
  sync-path condition** (`!c.stateMgr.IsSelected()`; note secs1 gates all message
  types, not only `DataMsgType` — preserve that local convention rather than
  importing the hsmsss data-only shape).
- Investigate whether secs1's send-side error handling has the same
  "escalate `ErrNotSelectedState` to teardown" behavior as hsmsss
  `processSendRequest`. secs1 uses a different (SECS-I block) protocol loop, so
  the Defect-2 backstop may be unnecessary or located elsewhere. If an
  equivalent escalation exists, apply the same non-fatal handling; otherwise
  document that the gate alone suffices for secs1.
- secs1 lock-order rule applies — see [[secs1-lock-order-rule]]: the
  `IsSelected()` read must not be taken inside `createContext()` or violate
  `sendMu > ctxMutex`. Verify the guard sits before any such acquisition (it
  mirrors the existing sync-path guards, which already satisfy this).

## 6. Test plan

### 6.1 Update existing repro tests (they assert the bug; invert to guard the fix)

`hsmsss/recv_stall_repro_test.go`:
- **`TestRecvStall_AsyncDataSendDuringNotSelectedKillsConn`** → rename/retarget to
  assert the **fixed** behavior: `SendMessageAsync(dataMsg)` while NotSelected
  returns `hsms.ErrNotSelectedState`, `len(senderMsgChan) == 0`, and the host
  stays NotSelected (no drop). Keep the `SendDataMessage` contrast.
- **`TestRecvStall_OutboundQueueFullOnHalfOpen`** is unaffected (it sends while
  *Selected* during a half-open window, so the new gate passes). Keep as a
  characterization of the still-open Candidate B; confirm it still passes.
- Update the file header comment block: the async-gate defect is now fixed; the
  test for it becomes a regression guard.

### 6.2 New regression tests (from report §7)

1. **Async gate:** async data send while NotSelected → `ErrNotSelectedState`,
   not enqueued, no drop. (May be the retargeted test above.)
2. **No Select starvation:** small-`senderQueueSize` active host; flood
   `SendMessageAsync` across a disconnect; assert reconnect reaches `Selected`
   (the loop is gone). Use a peer that completes Select on reconnect.
3. **Sender backstop:** force a queued data message to be dequeued while
   NotSelected (exercise the residual race, e.g. via a test seam or by enqueuing
   a control msg + data and dropping Selected) and assert the connection is
   **not** torn down (`processSendRequest` returns true). If the race is hard to
   force deterministically, drive `processSendRequest` directly in a unit test
   with a NotSelected connection and a data `sendRequest`.
4. **Control still flows:** while NotSelected, the Select handshake still
   completes against a cooperative peer (covered implicitly by test 2; add an
   explicit Linktest-while-NotSelected assertion if cheap).

### 6.3 Teeth

Per [[regression-guard-teeth-check]]: confirm each new guard fails if the fix is
reverted (temporarily remove the gate / restore the fatal return and re-run).

### 6.4 secs1

Add the parity equivalents of tests 1 and 2 to the secs1 suite (adjusting for
SECS-I connection setup). Check the [[hsmsss-secs1-parity-pattern]] note.

## 7. Validation steps

```
# Targeted repro / regression
go test ./hsmsss/ -run 'TestRecvStall' -race -count=3

# Package-local
go test ./hsmsss/ -count=1 -race -skip '^Fuzz'
go test ./secs1/  -count=1 -race -skip '^Fuzz'

# Full suite (criterion 6). Fuzz tests are excluded because
# FuzzConnectionLifecycle hangs under repeated/-count runs (project policy;
# see .agents/rules/300-testing.md). No -race here to avoid that hang.
go test ./... -count=1 -skip '^Fuzz'

make lint            # expect 0 issues
```

This set proves every §1 success criterion: criteria 1–4 by the hsmsss tests,
criterion 5 by the secs1 tests, and criterion 6 by the full-suite +
`make lint` runs. The behavior change is visible through the public async
helpers (`hsms/base_session.go` `SendDataMessageAsync`, `hsmsss/session.go`
`SendMessageAsync`, and the secs1 equivalents), so the full-suite run is the gate
that catches any consumer-package regression — it is not optional.

Plus a teeth pass (6.3) and `make stress-test`-style `-count` on the new
reconnect test if it has any timing component (always `-skip '^Fuzz'` per
[[stress-test-fuzz-flake]]).

## 8. Risks & open questions

- **Async error visibility (§4 open decision):** option A vs B for
  `replyErrToSender` on dropped async sends. Defaulting to A.
- **secs1 divergence:** the Defect-2 backstop may not map onto secs1's block
  protocol loop; Phase 3 includes an investigation step rather than a fixed diff.
- **Consumer-visible contract change:** `SendMessageAsync` now returns
  `ErrNotSelectedState` while not Selected. Document in `CHANGELOG.md`. This
  aligns the async path with the already-documented sync paths.
- **Out of scope (unchanged):** Candidate A (receiver stall on a full data
  channel), Candidate B (half-open outbound `ErrSendMsgTimeout`), and the
  parked-receiver FIN-miss remain tracked by their characterization tests.

## 9. Rollout

1. Implement Phases 1–3 on a feature branch.
2. Update/inverts tests (§6); teeth-check.
3. Validate (§7).
4. `CHANGELOG.md` entry under the appropriate version (fix: hsmsss/secs1 —
   gate async data sends while not Selected; stop ungated async sends from
   tearing down the reconnect loop).
5. Post-implementation review (`post-impl-review`) against this plan before merge.
6. Merge per the local workflow ([[git-local-workflow]], [[repo-merge-strategy]]).

## 10. References

See `00-root-cause-report.md` §8 for the full source map.
