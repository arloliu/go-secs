# Activity-Based Linktest Suppression Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore (and extend) v1's activity-based linktest suppression in the v2 `hsmsss` transport so go-secs does not linktest-probe a line with recent traffic or an outstanding reply, and does not linktest-disconnect a link that shows signs of life (frames received from the peer, or an open T3-guarded transaction) — with all residual race windows explicitly bounded in the Design Reference.

**Architecture:** Three suppression rules, all evaluated inside the existing `runLinktest` goroutine (`hsmsss/transport_procedures.go`), gated by one new config knob `hsms.WithLinktestSuppression(bool)` (default **ON**): (1) *activity reset* — a linktest fires only after a full `LinktestInterval` of line silence, tracked by two monotonic atomic stamps written at the transport's single send chokepoint (`transport.Write`) and single receive chokepoint (`recvLoop`), reset at each generation's `Start`; (2) *inflight skip* — no linktest is sent while a W-bit data reply is outstanding (`DataMsgInflight() > 0`), because T3 already bounds liveness there; (3) *liveness credit* — a linktest that T6-times-out is not counted toward the fail-threshold disconnect when the link showed life: a frame arrived after the probe went out, **or** a data reply is outstanding at failure time (closes the pre-send check race), **or** a frame arrived since the last counted failure (failures only count when consecutive-in-silence). The `hsmsss` package reaches the two new runtime getters through a package-local capability interface asserted on `hsms.TransportRuntime` — the exported interface itself is NOT widened (that would compile-break external implementers and the in-repo mocks). No timer-sharing across goroutines: the linktest timer is lazily re-armed inside its own goroutine (`timer.Reset(interval - idle)`).

**Tech Stack:** Go 1.26, stdlib `sync/atomic` + `time` only (no new dependencies), `testify` for tests, existing in-package test harness (`newEndpoint`, `echoHandler`, `ChaosProxy`, `assertStaysSelected`, `waitNotConnectedEvent`).

## Global Constraints

- Module: `github.com/arloliu/go-secs/v2`; no new dependencies.
- All tests must pass under `-race` (`make test` enables it). Never use `time.Sleep` to wait for state — subscribe to state events or `require.Eventually` on metrics (`.agents/rules/300-testing.md`). `time.Sleep` is allowed only to *inject* a delay into a scenario.
- Use `t.Context()` for test contexts; `testify` `require`/`assert`; cleanup via `t.Cleanup`/`defer`.
- Godoc: public comments may reference SEMI standards (E37 §x.y, T3/T6 timers) but NEVER internal design codes (D5a-5, NEW-1, I1 etc.). Internal (unexported) comments may keep such codes, matching surrounding style.
- After modifying any `.go` file: run `go fix ./...`, review its diff (every change must be behavior-preserving), then `make lint`, fix all issues, re-run until clean (`.agents/rules/700-lint-after-write.md`). Every commit step below implies this sequence.
- Commit messages: conventional-commit style (`feat(hsmsss): …`, `test(hsmsss): …`, `docs: …`). **No `Co-Authored-By` or any attribution trailer.**
- **Do NOT add methods to the exported `hsms.TransportRuntime` interface** (`hsms/transport.go:81`) — that is a compile-time break for external implementers and for the in-repo mocks (`hsms/harness_mock_runtime_test.go`, `hsmsss/transport_test.go` `mockRT`, `hsmsss/transport_recv_test.go` `recRT`). The new getters live only on the unexported `connection` type; `hsmsss` reaches them via a package-local capability interface + type assertion (Task 4). Do not touch the unexported `transport` interface either.
- Default for the new knob is **true** (suppression ON). The default lives in `hsms.DefaultConnectionConfig()` (explicit-defaults pattern) — a plain `bool` field is correct here; no pointer needed, because config values always originate from `DefaultConnectionConfig`, never from a zero-valued struct literal.

## Design Reference (read before any task)

Current v2 behavior (`hsmsss/transport_procedures.go:59` `runLinktest`): a fixed `pool.GetTimer(interval)` loop sends a T6-bounded `Linktest.req` every `interval` while Selected, unconditionally. `fails++` on each round-trip error; `fails >= threshold` → `t.rt.TCPDown(errLinktestFailed)`. Note `incLinktestSend` fires BEFORE `WriteMessage` (`hsmsss/transport_procedures.go:81`) — `LinktestSendCount` is an *attempt* signal, not an on-wire barrier; tests that need on-wire causality must observe the frame at the ChaosProxy instead.

New behavior when suppression is enabled (captured ONCE per Selected entry in `startLinktest`, like `interval` — a live reconfig via `UpdateConfigOptions` applies on the NEXT Selected entry):

| # | Rule | Trigger point | Effect |
|---|------|--------------|--------|
| 1 | Activity reset | timer fire | if `now − max(lastSend, lastRecv) < interval`: don't send, `timer.Reset(interval − idle)`, count `linktestSuppressed` |
| 2 | Inflight skip | timer fire (after rule 1 passes) | if `sr.DataMsgInflight() > 0`: don't send, `timer.Reset(interval)`, count `linktestSuppressed` |
| 3 | Liveness credit | linktest round-trip error | credit (don't count toward threshold, DO count `linktestErr` + `linktestCredited`) when `lastRecv > sentAt` **or** `sr.DataMsgInflight() > 0` at failure time; additionally, an uncredited failure resets the consecutive counter first if any frame arrived since the last *counted* failure |

**Failure counting semantics (rule 3, precise).** `fails` counts *consecutive probe failures with no intervening sign of life*. A probe failure is COUNTED only when, at failure-evaluation time: no frame was received after the probe was approved (`lastRecv <= sentAt`), AND no data reply is outstanding (`DataMsgInflight() == 0`), AND — if `fails > 0` — no frame was received since the previous counted failure (else `fails` resets to 0 before counting). The inflight condition closes the race where a data send wins the write path between the pre-send inflight check and the probe's write: without it, a probe raced by a long-running transaction could count a failure while T3 was already guarding that window. A dead link produces no frames and no new inflight transactions, earns no credit, and still trips the threshold. The classification lives in a pure function (`linktestFailureStep`, Task 6) so every branch is exhaustively unit-testable without scheduling real races.

**Detection bounds (state these numbers everywhere; do not promise tighter).** Probe cadence on failure keeps the existing shape (one `interval` re-arm after every attempt, matching v1 and shipped v2 — see the existing threshold test's `T6+interval` per dropped cycle, `hsmsss/integration_linktest_threshold_test.go:40-48`). Therefore a genuinely dead idle link is dropped within ≈ `threshold × (interval + T6)` of its last sign of life; when a transaction was outstanding, add the caller's `T3` in front: ≈ `T3 + threshold × (interval + T6)`. After a reconnect, a straggler stamp from the torn-down generation can restart the failure run once, so the post-reconnect worst case is one additional threshold-length run: ≤ `2 × threshold × (interval + T6)`. The operator's primary "step in" knob is T3.

**Accepted, documented races (bounded — deliberately NOT fixed with a new core write seam). This is the plan's honest contract; do not restate stronger claims anywhere.** The suppression checks and `sentAt` are captured before the probe's frame reaches the core's serialized write boundary (`epoch.writeMu`, `hsms/connection_send.go`), and there is no shared lock between the failure evaluation and `TCPDown`. Consequences, each bounded:

- (a) *Pre-wire false credit:* a frame received while the probe is queued-but-not-yet-written earns credit though it preceded the probe on the wire. On a dead link the next probe (≤ `interval + T6` later) fails uncredited — ≤ one extra probe cycle of detection delay.
- (b) *Micro-race spurious count.* First, the definition the whole contract hangs on — **"sign of life" means frames RECEIVED from the peer, or an open W-bit transaction (T3-guarded)**. Our own successful writes suppress probes (rule 1: don't add load to a line we're using) but are deliberately NOT life for failure forgiveness — a write success proves local TCP buffering, not the peer. With that definition, the three interleavings at the check-to-write boundary resolve as:
  - *Inbound frame between the rule-1 check and `sentAt`:* the counted failure is spurious, but absorbed at `threshold >= 2` even from `fails == threshold-1`: the reducer's `recvNow > recvAtLastFail` restart fires on that same evaluation (the frame advanced `recvNow` past the previous counted failure's memory), so the run restarts at 1 instead of disconnecting.
  - *Fire-and-forget write in that window:* counted, and contractually CORRECT — the peer has, by construction, sent nothing and owes nothing across the entire failure run; only our own writes occurred. A peer silent through `threshold` probe windows is dead by this contract, whatever we were writing at it.
  - *W-bit send becoming inflight between the failure-time `DataMsgInflight()` load and the threshold decision:* narrowed by a mandatory FINAL re-check — at `fails >= threshold`, immediately before `TCPDown`, the loop re-loads `DataMsgInflight()` and `lastRecvStamp` and converts to a credit if either shows life. The residual window is the handful of instructions between that re-check and `TCPDown`; a transaction landing inside it is torn down with the standard connection-drop error and retried after reconnect. This residue is accepted (a true zero-window close needs a core-serialized disconnect seam, deliberately declined).

  At `threshold == 1` any counted timeout disconnects — exactly the pre-existing v2.0.x semantic, where ANY single T6 timeout disconnects regardless of traffic; suppression strictly narrows that window, never widens it. The Task-1 godoc therefore recommends a threshold of at least 2 when suppression matters.
- (c) *Straggler stamp after reconnect:* a receive-goroutine straggler from a torn-down generation can stamp `lastRecvStamp` once after the successor starts, suppressing/crediting one probe or restarting an accumulated failure run — worst case one additional threshold-length run (the bound stated above). The stamp resets at both conn-publish sites (Task 2) shrink the window; the stamps stay transport-global (not generation-fenced) by deliberate choice, trading a rare bounded delay for zero added lifecycle machinery.

A cancelled-but-queued probe writing after teardown begins is pre-existing core behavior (`writeFrame` checks the generation ctx, not the caller ctx) and is out of scope. **The feature's guarantee, stated precisely:** with `threshold >= 2`, a link that showed a sign of life (as defined in (b): received frames or an open transaction) OBSERVED by a failure evaluation — including the final pre-disconnect re-check — is not disconnected by probes; the sole residue is the instruction window between that re-check and `TCPDown`. A genuinely dead link is always detected within the bounds above. There is no "zero false disconnects at any threshold" claim, and prose elsewhere (Goal, README, godoc) must not restate anything stronger than this paragraph.

Timestamps: `transport.clockBase time.Time` (set once in `newTransport`, immutable) + `lastSendStamp`/`lastRecvStamp atomic.Int64` holding `int64(time.Since(clockBase))` — monotonic, wall-clock-jump immune, nanosecond resolution (a same-nanosecond tie on the strict `>` credit comparison is accepted as astronomically unlikely). Send stamp: in `(t *transport) Write` after a successful `bufs.WriteTo` (every outbound frame passes here — sync, async drain, and control paths all funnel through the core's serialized write; includes our own Linktest.req, which is harmless: the idle-link cadence stays ≈ `interval` because the loop re-arms after the round trip resolves). Recv stamp: in `recvLoop` immediately after a successful `readFrame` (every inbound frame, including control frames — a peer's `Linktest.req` IS proof of link liveness). Both stamps reset to "now" in `Start` when the generation's conn is published, so a fresh session begins with a coherent baseline.

Known, accepted trade-offs (documented in godoc, Task 8):
- During a long silent wait on a reply, dead-link detection falls to T3 (+ TCP write errors) instead of `interval + T6`. This is the feature's purpose: the operator sizes T3 for their slowest equipment.
- An application that continuously streams fire-and-forget sends (`SendAsync`/`WriteMessageNoReply`, inflight always 0 but line never silent) suppresses linktest indefinitely; an application-level zombie peer is then only detected by write timeouts. That traffic pattern is the documented reason to set `WithLinktestSuppression(false)`.

## File Structure

| File | Change |
|------|--------|
| `hsms/connection_config.go` | `linktestSuppression bool` field, default `true`, `WithLinktestSuppression` option |
| `hsms/connection.go` | `LinktestSuppression() bool` + `DataMsgInflight() int64` methods on the unexported `connection` (NOT on any interface) |
| `hsms/connection_config_test.go` | default + option tests |
| `hsmsss/transport.go` | `clockBase` + stamp fields on `transport`, `monoNanos`/`sinceLastActivity`/`resetActivityStamps` helpers, send stamp in `Write` |
| `hsmsss/transport_active.go` | stamp reset at the active conn-publish site in `startActive` |
| `hsmsss/transport_passive.go` | stamp reset at the passive conn-publish site in `acceptLoop` |
| `hsmsss/transport_recv.go` | recv stamp in `recvLoop` |
| `hsmsss/transport_activity_test.go` | new: unit tests for stamps/idle math/reset |
| `hsmsss/metrics.go` | `linktestSuppressed` + `linktestCredited` counters |
| `hsmsss/metrics_internal_test.go` | counter tests |
| `hsmsss/transport_procedures.go` | `suppressionRuntime` capability interface, suppression logic in `startLinktest`/`runLinktest` |
| `hsmsss/integration_linktest_suppression_test.go` | new: the strict behavior suite (Tasks 4–7) |
| `README.md`, `CHANGELOG.md`, `docs/migration-v1-to-v2.md`, `hsmsss/doc.go` | docs (Task 8) |

---

### Task 1: Config knob + connection getters (hsms package)

**Files:**
- Modify: `hsms/connection_config.go` (field ~line 46, default ~line 77, option after `WithLinktestFailThreshold` ~line 300)
- Modify: `hsms/connection.go` (methods after `LinktestFailThreshold()` ~line 204)
- Test: `hsms/connection_config_test.go`

**Interfaces:**
- Consumes: existing `ConnectionConfig`, `ConnOption`, `DefaultConnectionConfig`, `connection.cfg` atomic, `connection.metrics`.
- Produces: `hsms.WithLinktestSuppression(enabled bool) ConnOption`; methods `LinktestSuppression() bool` and `DataMsgInflight() int64` on the unexported `connection` type. **No interface is modified** — Task 4's capability interface in `hsmsss` discovers these via type assertion.

- [ ] **Step 1: Write the failing tests** — add to `hsms/connection_config_test.go` (follow the file's existing style):

```go
func TestConnectionConfig_LinktestSuppressionDefault(t *testing.T) {
	cfg := DefaultConnectionConfig()
	require.True(t, cfg.linktestSuppression, "linktest suppression must default to enabled")
}

func TestWithLinktestSuppression(t *testing.T) {
	cfg := DefaultConnectionConfig()

	require.NoError(t, cfg.apply(WithLinktestSuppression(false)))
	require.False(t, cfg.linktestSuppression)

	require.NoError(t, cfg.apply(WithLinktestSuppression(true)))
	require.True(t, cfg.linktestSuppression)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./hsms/ -run 'LinktestSuppression' -v`
Expected: FAIL — `cfg.linktestSuppression` undefined.

- [ ] **Step 3: Implement**

`hsms/connection_config.go` — add the field next to `linktestFailThreshold` in the struct:

```go
	linktestSuppression        bool
```

In `DefaultConnectionConfig()` next to `linktestFailThreshold: 3,`:

```go
		linktestSuppression:        true,
```

Option (place after `WithLinktestFailThreshold`; godoc explains all three rules — public wording, SEMI refs only):

```go
// WithLinktestSuppression enables or disables activity-based suppression of the
// automatic linktest (enabled by default).
//
// When enabled, the auto-linktest (see [WithLinktestInterval]) only probes a link
// that is actually idle:
//
//   - A Linktest.req is sent only after a full linktest interval with no HSMS
//     frame sent or received on the connection. A line carrying traffic is in
//     active use, so probing it adds redundant load — some older equipment is
//     slow to answer Linktest.req while busy. Note the asymmetry: outbound
//     traffic suppresses probing, but only received frames or an outstanding
//     reply count as proof of peer liveness for the failure rules below (a
//     successful write proves local buffering, not the peer).
//   - No Linktest.req is sent while a sent data message is awaiting its reply.
//     During that window the T3 reply timeout already bounds failure detection,
//     and probing equipment that is busy processing a long-running command
//     (a recipe transfer can take minutes) risks a spurious T6 timeout.
//   - A linktest failure (T6 timeout) is not counted toward the
//     linktest-failure disconnect threshold (see [WithLinktestFailThreshold])
//     while the link shows other signs of life: a frame arrived after the
//     Linktest.req went out, a data reply is still outstanding, or a frame
//     arrived since the previous counted failure. Only consecutive failures on
//     a silent link accumulate toward the disconnect.
//
// A truly dead link is still detected: a silent link with nothing outstanding is
// probed every interval exactly as before, so it is dropped within roughly
// threshold x (interval + T6) of its last sign of life; an unanswered reply is
// bounded by T3 first. Use a linktest failure threshold of at least 2 (the
// default is 3): a single probe timeout that races a lone received frame is then
// absorbed instead of disconnecting, and the disconnect decision re-checks for
// signs of life (received frames, or a reply still outstanding) immediately
// before dropping the link — though life arriving in the final instants of that
// decision can still be missed. With a threshold of 1, any single probe timeout
// disconnects, with or without suppression. Disable suppression to restore
// unconditional periodic linktests — for example when the application streams
// fire-and-forget messages continuously (the line is never silent, so a
// suppressed linktest would never run).
func WithLinktestSuppression(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.linktestSuppression = enabled
		return nil
	}
}
```

`hsms/connection.go` — after `LinktestFailThreshold()`. These are methods on the unexported `connection`; they do not extend any interface, and `hsmsss` discovers them via a type assertion (see Task 4):

```go
// LinktestSuppression reports whether activity-based linktest suppression is
// enabled (see WithLinktestSuppression). Lock-free atomic read: the transport
// reads it once per entry to Selected, so a reconfig applies on the NEXT
// Selected-entry, never mid-session. Reached by the hsmsss transport through a
// package-local capability interface, deliberately NOT via TransportRuntime
// (widening that exported interface would break external implementers).
func (c *connection) LinktestSuppression() bool {
	return c.cfg.Load().linktestSuppression
}

// DataMsgInflight returns the current number of sent data messages still
// awaiting a reply — the same gauge as ConnectionMetrics.DataMsgInflightCount.
// Reached by the hsmsss transport through the same capability interface as
// LinktestSuppression, for the linktest inflight-skip and liveness-credit rules.
func (c *connection) DataMsgInflight() int64 {
	return c.metrics.DataMsgInflightCount()
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./hsms/ -run 'LinktestSuppression' -v && go build ./... && go vet ./hsms/`
Expected: PASS, clean build. Also run `go test ./hsms/ ./hsmsss/ -count=1` — the existing mocks (`mockRuntime`, `mockRT`, `recRT`) must still compile unchanged, which is the point of not widening the interface.

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsms/connection_config.go hsms/connection.go hsms/connection_config_test.go
git commit -m "feat(hsms): add WithLinktestSuppression option and connection getters"
```

---

### Task 2: Activity stamps in the hsmsss transport

**Files:**
- Modify: `hsmsss/transport.go` (struct ~line 59, `newTransport`, `Write` ~line 331)
- Modify: `hsmsss/transport_active.go` (`startActive`, the `t.conn = conn` publish ~lines 150-164)
- Modify: `hsmsss/transport_passive.go` (`acceptLoop`, the conn publish ~lines 94-121)
- Modify: `hsmsss/transport_recv.go` (`recvLoop` ~line 57)
- Test: create `hsmsss/transport_activity_test.go`

**Interfaces:**
- Consumes: `transport` struct, `newTransport`, `startActive`/`acceptLoop` conn-publish sites, `(t *transport) Write`, `recvLoop`.
- Produces: `t.monoNanos() int64`, `t.sinceLastActivity() time.Duration`, `t.resetActivityStamps()`, `t.lastSendStamp`/`t.lastRecvStamp atomic.Int64`. Tasks 4/6 read these from `runLinktest`.

> **Wiring note (review round 2):** `Start` itself never stores the socket — it binds the runtime and delegates to role-specific paths. The active conn is published in `startActive` (`hsmsss/transport_active.go`) and the passive conn asynchronously in `acceptLoop` (`hsmsss/transport_passive.go`). The reset MUST sit beside both actual `t.conn = conn` publications (under `connMu`), not anywhere in `Start` — a passive listener may wait arbitrarily long before accepting, and resetting at listen time would mis-baseline the eventual session.

- [ ] **Step 1: Write the failing unit tests** — `hsmsss/transport_activity_test.go` (package `hsmsss`, white-box; construct a bare `*transport` the same way the existing `transport_test.go` does — if no reusable constructor helper exists there, add a minimal `newTestTransport(t *testing.T) *transport` to this file via the same config + `newTransport` path, no `Start`):

```go
package hsmsss

// transport_activity_test.go — unit tests for the monotonic activity stamps that feed
// activity-based linktest suppression: the send stamp written by (t *transport).Write,
// the recv stamp written by recvLoop (covered end-to-end by the integration suite; here
// the field is driven directly), the sinceLastActivity idle computation, and the
// per-generation stamp reset.

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTransport_WriteStampsSendActivity(t *testing.T) {
	tr := newTestTransport(t)

	require.Zero(t, tr.lastSendStamp.Load(), "no send stamp before any Write")

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() { buf := make([]byte, 16); _, _ = server.Read(buf) }()

	require.NoError(t, tr.Write(t.Context(), client, net.Buffers{[]byte("x")}))
	require.Positive(t, tr.lastSendStamp.Load(), "successful Write must stamp send activity")
}

func TestTransport_WriteErrorDoesNotStamp(t *testing.T) {
	tr := newTestTransport(t)

	require.Error(t, tr.Write(t.Context(), nil, net.Buffers{[]byte("x")}))
	require.Zero(t, tr.lastSendStamp.Load(), "failed Write must not stamp send activity")
}

func TestTransport_SinceLastActivity(t *testing.T) {
	tr := newTestTransport(t)

	// No stamps at all: idle since clockBase — a large duration, never negative.
	require.GreaterOrEqual(t, tr.sinceLastActivity(), time.Duration(0))

	// The most recent of the two stamps wins.
	tr.lastSendStamp.Store(tr.monoNanos() - int64(time.Hour))
	tr.lastRecvStamp.Store(tr.monoNanos() - int64(10*time.Millisecond))
	require.Less(t, tr.sinceLastActivity(), time.Second,
		"recv stamp 10ms ago must dominate the 1h-old send stamp")

	tr.lastSendStamp.Store(tr.monoNanos())
	require.Less(t, tr.sinceLastActivity(), 100*time.Millisecond, "fresh send stamp must reset idle")
}

func TestTransport_ResetActivityStamps(t *testing.T) {
	tr := newTestTransport(t)

	tr.lastSendStamp.Store(tr.monoNanos() - int64(time.Hour))
	tr.lastRecvStamp.Store(tr.monoNanos() - int64(time.Hour))

	tr.resetActivityStamps()

	require.Less(t, tr.sinceLastActivity(), time.Second,
		"reset must re-baseline both stamps to now so a new generation starts coherent")
}
```

Production-path wiring tests — a direct `resetActivityStamps()` call cannot catch missing wiring in either role, so both connection-establishment paths must prove a fresh baseline through the real code. Two red-capability requirements (review round 3): (i) seed stamps **hour-old** (`tr.monoNanos() - int64(time.Hour)`) — seeding `1` is useless because `clockBase` is fresh, so an unwired reset would still pass a "recent" assertion; (ii) assert on **`lastRecvStamp` specifically against a peer that sends nothing** — the active role's Select.req write freshens `lastSendStamp` through `Write` regardless of the reset, so only the recv stamp distinguishes "reset ran" from "handshake traffic". With a silent peer, `lastRecvStamp` can ONLY become fresh via `resetActivityStamps()` at conn-publish: remove either production call and its test goes red.

Scaffolding (verified locations): the active direct-`Start` pattern (bare `newTransport` + mock runtime + `Start`) is in `hsmsss/transport_test.go:208-229` — NOT in `transport_dialer_test.go`, whose tests drive the exported surface; the passive side combines the same direct pattern with `newPipeListener` (`hsmsss/transport_listener_test.go:123-179`) so the test can dial in on demand.

```go
// TestTransport_ActiveStartResetsActivityBaseline: the ACTIVE conn-publish path
// (startActive) must re-baseline the stamps through production wiring. The peer is a
// silent listener: it accepts and sends nothing, so lastRecvStamp can only be freshened
// by resetActivityStamps() at the t.conn publish — not by handshake traffic.
func TestTransport_ActiveStartResetsActivityBaseline(t *testing.T) {
	// Arrange per transport_test.go:208-229: net.Listen on loopback (accept and hold the
	// conn open, never write), active-config bare transport via newTransport, the same
	// mock runtime that file passes to Start, t.Cleanup(Stop).
	stale := tr.monoNanos() - int64(time.Hour)
	tr.lastSendStamp.Store(stale)
	tr.lastRecvStamp.Store(stale)

	// ... Start(ctx, rt) per the reference pattern ...

	require.Eventually(t, func() bool {
		return tr.lastRecvStamp.Load() > stale+int64(30*time.Minute)
	}, 5*time.Second, 10*time.Millisecond,
		"startActive must re-baseline lastRecvStamp at conn-publish: the silent peer sent nothing, so only resetActivityStamps can freshen it")
}

// TestTransport_PassiveAcceptResetsActivityBaseline: the PASSIVE conn-publish path
// (acceptLoop) must re-baseline the stamps when a peer connects — not at listen time.
func TestTransport_PassiveAcceptResetsActivityBaseline(t *testing.T) {
	// Arrange per transport_listener_test.go:123-179: passive-config bare transport with
	// newPipeListener, mock runtime, Start (listener armed, no conn yet), t.Cleanup(Stop).
	stale := tr.monoNanos() - int64(time.Hour)
	// Seed AFTER Start returns and BEFORE dialing in — proves the reset happens at
	// conn-publish inside acceptLoop, not anywhere in Start.
	tr.lastSendStamp.Store(stale)
	tr.lastRecvStamp.Store(stale)

	// ... dial the pipe listener (silent client: connect, never write) ...

	require.Eventually(t, func() bool {
		return tr.lastRecvStamp.Load() > stale+int64(30*time.Minute)
	}, 5*time.Second, 10*time.Millisecond,
		"acceptLoop must re-baseline lastRecvStamp when it publishes the accepted conn")
}
```

The `// ... arrange ...` markers defer only the boilerplate that the two cited reference tests already contain verbatim (listener/pipe setup, mock runtime construction, Stop cleanup); the seed values, the silent-peer requirement, and the `lastRecvStamp`-vs-`stale` assertions above are the deliverable and must be kept exactly as shown.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./hsmsss/ -run 'TestTransport_WriteStamps|TestTransport_WriteError|TestTransport_SinceLastActivity|TestTransport_ResetActivityStamps' -v`
Expected: FAIL — `lastSendStamp` / `monoNanos` / `sinceLastActivity` / `resetActivityStamps` undefined.

- [ ] **Step 3: Implement**

`hsmsss/transport.go` struct — add fields (near `metrics`, doc comment in the file's internal style):

```go
	// clockBase is the immutable monotonic reference for the activity stamps below; set once in
	// newTransport, never re-stored, so monoNanos() is race-free and wall-clock-jump immune.
	clockBase time.Time

	// lastSendStamp / lastRecvStamp hold monoNanos() at the last successful frame write /
	// frame read. They feed the activity-based linktest suppression in runLinktest: the
	// send stamp is written under the core's write serialization (single writer), the recv
	// stamp by the recv goroutine (single reader) — both read lock-free by the linktest
	// goroutine, so plain atomics suffice. Start re-baselines both (resetActivityStamps)
	// when it publishes a new generation's conn; a torn-down generation's straggler can
	// stamp at most once after that, which delays/credits at most one probe (bounded,
	// documented in the plan's accepted-races note).
	lastSendStamp atomic.Int64
	lastRecvStamp atomic.Int64
```

In `newTransport` (wherever the struct literal is built): `clockBase: time.Now(),`

Helpers (place near `Write`):

```go
// monoNanos returns nanoseconds elapsed since this transport's immutable clockBase.
// time.Since uses the monotonic clock, so stamps never move backward on wall-clock changes.
func (t *transport) monoNanos() int64 {
	return int64(time.Since(t.clockBase))
}

// sinceLastActivity returns how long the line has been silent: elapsed time since the most
// recent successful frame write or read. With no stamps yet it is the age of the transport,
// which correctly reads as "long idle".
func (t *transport) sinceLastActivity() time.Duration {
	last := max(t.lastSendStamp.Load(), t.lastRecvStamp.Load())
	return time.Duration(t.monoNanos() - last)
}

// resetActivityStamps re-baselines both activity stamps to "now". Start calls it when it
// publishes a fresh generation's conn, so suppression decisions never consult a prior
// generation's traffic.
func (t *transport) resetActivityStamps() {
	now := t.monoNanos()
	t.lastSendStamp.Store(now)
	t.lastRecvStamp.Store(now)
}
```

In `Write`, stamp only on success:

```go
	_, err := bufs.WriteTo(conn)
	if err == nil {
		t.lastSendStamp.Store(t.monoNanos())
	}

	return err
```

Wire the reset at BOTH real conn-publish sites (see the Wiring note above — NOT in `Start`):
- `hsmsss/transport_active.go`, `startActive`: add `t.resetActivityStamps()` immediately beside the `t.conn = conn` store under `connMu`.
- `hsmsss/transport_passive.go`, `acceptLoop`: add `t.resetActivityStamps()` immediately beside its conn store under `connMu`.

`hsmsss/transport_recv.go` — in `recvLoop`, immediately after the `readFrame` error check (i.e. once `frame` is known good, before `dispatchFrame`):

```go
		t.lastRecvStamp.Store(t.monoNanos()) // any complete inbound frame is proof of link liveness
```

- [ ] **Step 4: Run tests**

Run: `go test ./hsmsss/ -run 'TestTransport_' -race -v && go test ./hsmsss/ -race -count=1`
Expected: PASS; the full existing suite stays green (stamps are pure additions).

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/transport.go hsmsss/transport_active.go hsmsss/transport_passive.go hsmsss/transport_recv.go hsmsss/transport_activity_test.go
git commit -m "feat(hsmsss): track monotonic send/recv activity stamps on the transport"
```

---

### Task 3: Suppression metrics

**Files:**
- Modify: `hsmsss/metrics.go`
- Test: `hsmsss/metrics_internal_test.go`

**Interfaces:**
- Produces: `ConnectionMetrics.LinktestSuppressedCount() uint64`, `ConnectionMetrics.LinktestCreditedCount() uint64`, unexported `incLinktestSuppressed()`, `incLinktestCredited()`. Tasks 4–6 call the inc helpers; the integration tests assert the public counters.

- [ ] **Step 1: Write the failing test** — add to `hsmsss/metrics_internal_test.go`, following its existing inc-then-read pattern:

```go
func TestConnectionMetrics_LinktestSuppressionCounters(t *testing.T) {
	var m ConnectionMetrics

	require.Zero(t, m.LinktestSuppressedCount())
	require.Zero(t, m.LinktestCreditedCount())

	m.incLinktestSuppressed()
	m.incLinktestSuppressed()
	m.incLinktestCredited()

	require.Equal(t, uint64(2), m.LinktestSuppressedCount())
	require.Equal(t, uint64(1), m.LinktestCreditedCount())
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./hsmsss/ -run 'TestConnectionMetrics_LinktestSuppression' -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Implement** — in `hsmsss/metrics.go` add struct fields next to the linktest counters:

```go
	linktestSuppressed atomic.Uint64 // auto-linktest fire skipped: line active or a data reply outstanding
	linktestCredited   atomic.Uint64 // linktest failure forgiven: the link showed life (frame after the probe, or a reply outstanding)
```

Public getters (match the file's godoc voice) + unexported incs (place near the other `inc*` helpers):

```go
// LinktestSuppressedCount returns the number of auto-linktest probe opportunities that were
// skipped by activity-based suppression (see hsms.WithLinktestSuppression): the line had
// traffic within the last linktest interval, or a sent data message was still awaiting its
// reply. It only ever grows; a steadily climbing value on a busy connection is expected and
// healthy.
func (m *ConnectionMetrics) LinktestSuppressedCount() uint64 {
	return m.linktestSuppressed.Load()
}

// LinktestCreditedCount returns the number of failed linktest round-trips that were NOT
// counted toward the linktest-failure disconnect threshold because the link showed other
// signs of life — a frame arrived after the Linktest.req went out, or a data reply was
// still outstanding when the probe timed out (see hsms.WithLinktestSuppression).
// LinktestErrCount still counts these failures.
func (m *ConnectionMetrics) LinktestCreditedCount() uint64 {
	return m.linktestCredited.Load()
}

func (m *ConnectionMetrics) incLinktestSuppressed() { m.linktestSuppressed.Add(1) }

func (m *ConnectionMetrics) incLinktestCredited() { m.linktestCredited.Add(1) }
```

- [ ] **Step 4: Run tests**

Run: `go test ./hsmsss/ -run 'TestConnectionMetrics' -v`
Expected: PASS.

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/metrics.go hsmsss/metrics_internal_test.go
git commit -m "feat(hsmsss): add linktest suppression and liveness-credit metrics"
```

---

### Task 4: Capability interface + rule 1 (activity reset) in runLinktest

**Files:**
- Modify: `hsmsss/transport_procedures.go` (`startLinktest` ~line 22, `runLinktest` ~line 59)
- Test: create `hsmsss/integration_linktest_suppression_test.go`

**Interfaces:**
- Consumes: `connection.LinktestSuppression()`/`DataMsgInflight()` (Task 1, via assertion), `t.sinceLastActivity()` (Task 2), `t.metrics.incLinktestSuppressed()` (Task 3).
- Produces: `suppressionRuntime` interface; `runLinktest(ctx, g, interval, sr suppressionRuntime)` where `sr == nil` means suppression OFF — Tasks 5–6 extend this same body.

- [ ] **Step 1: Write the failing tests** — create `hsmsss/integration_linktest_suppression_test.go`. Notes baked into the tests: the harness already defines `echoHandler` (`hsmsss/harness_endpoint_test.go:138`) — REUSE it, do not redeclare. All "no probe" assertions are **baseline-delta**, never lifetime-zero: an idle probe is legal between Selected-entry and the first controlled action, so each test first performs one controlled round trip (or gates on inflight), snapshots the counters, and asserts no growth *after* that point.

```go
package hsmsss

// integration_linktest_suppression_test.go — behavior suite for activity-based linktest
// suppression (hsms.WithLinktestSuppression, default ON). Rules under test:
//   1. activity reset  — no Linktest.req while the line saw a frame within the last interval
//   2. inflight skip   — no Linktest.req while a sent data message awaits its reply
//   3. liveness credit — a T6-failed probe is not counted toward the disconnect threshold
//                        while the link shows other signs of life
// All negative ("no probe") assertions are baseline-delta: a legal idle probe may fire
// between Selected-entry and the test's first controlled action, so each test establishes
// its precondition, snapshots LinktestSendCount, and asserts no growth afterwards.
// LinktestSendCount is an attempt counter incremented before the frame is written
// (transport_procedures.go), so causal on-wire ordering is asserted via the ChaosProxy
// filter where it matters (rule 3). Uses the shared harness (newEndpoint / echoHandler /
// waitSelected / controlMetrics / assertStaysSelected / ChaosProxy / waitNotConnectedEvent).
// Not t.Parallel: interval/T6 timing runs cleaner sequentially.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

const (
	// suppInterval is deliberately generous relative to the traffic cadence used below
	// (interval/5) so scheduler jitter under -race cannot open a full-interval silence
	// gap between two pings and let a legitimate probe through mid-test.
	suppInterval = 300 * time.Millisecond
	suppT6       = 500 * time.Millisecond
)

// TestHSMS_LinktestSuppression_BusyLinkSendsNoLinktest: rule 1. After one controlled
// round trip establishes the activity baseline, continuous request/reply traffic at
// interval/5 keeps the line busy: not a single NEW Linktest.req attempt may be made.
func TestHSMS_LinktestSuppression_BusyLinkSendsNoLinktest(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// Prime the activity window, then baseline the attempt counter.
	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()
	baseSupp := m.LinktestSuppressedCount()

	// Drive request/reply pings every interval/5 for 10 intervals: the line is never idle
	// for a full interval, so rule 1 must suppress every probe opportunity.
	deadline := time.Now().Add(10 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing, not state-waiting
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no new Linktest.req attempt may be made once traffic keeps the line busy")
	require.Greater(t, m.LinktestSuppressedCount(), baseSupp,
		"the suppressed-probe counter must record the skipped opportunities")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_IdleLinkStillProbes: the over-suppression guard. A fully
// idle link must be probed at the configured cadence exactly as without suppression —
// this preserves dead-link detection on quiet links.
func TestHSMS_LinktestSuppression_IdleLinkStillProbes(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// No traffic at all after Select: at least 3 successful probes within a generous window.
	require.Eventually(t, func() bool {
		return m.LinktestRecvCount() >= 3
	}, 20*suppInterval, 10*time.Millisecond,
		"an idle link must still be probed every interval (no over-suppression)")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestHSMS_LinktestSuppression_DisabledRestoresUnconditionalProbing: the knob. With
// WithLinktestSuppression(false), probes fire despite continuous traffic (v2.0.x behavior).
func TestHSMS_LinktestSuppression_DisabledRestoresUnconditionalProbing(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestSuppression(false)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
				time.Sleep(suppInterval / 5) // scenario pacing
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	require.Eventually(t, func() bool {
		return m.LinktestSendCount() >= 2
	}, 20*suppInterval, 10*time.Millisecond,
		"with suppression disabled, probes must fire despite continuous traffic")
	require.Zero(t, m.LinktestSuppressedCount(),
		"the suppression counter must stay zero when the feature is off")
}
```

- [ ] **Step 2: Run to verify the teeth**

Run: `go test ./hsmsss/ -run 'TestHSMS_LinktestSuppression' -race -v`
Expected: `BusyLinkSendsNoLinktest` FAILS on current code (probe attempts continue on the busy link — the regression the feature fixes). `IdleLinkStillProbes` PASSES now and must keep passing after (the over-suppression guard). `DisabledRestoresUnconditionalProbing` may pass trivially pre-implementation (current code never suppresses); its teeth are the knob wiring once suppression exists.

- [ ] **Step 3: Implement** — `hsmsss/transport_procedures.go`.

Add the capability interface near the top of the file:

```go
// suppressionRuntime is the optional capability that activity-based linktest suppression
// needs beyond hsms.TransportRuntime. The hsms connection core implements both methods on
// its (unexported) connection type; startLinktest discovers them via a type assertion.
// Kept package-local ON PURPOSE: widening the exported TransportRuntime interface would
// compile-break external implementers and the in-repo mocks (mockRuntime/mockRT/recRT).
// A runtime that lacks the capability simply runs with suppression off — the mocks in the
// transport unit tests therefore exercise the pre-suppression cadence unchanged.
type suppressionRuntime interface {
	LinktestSuppression() bool
	DataMsgInflight() int64
}
```

In `startLinktest`, resolve the capability once per Selected entry (extend the doc comment's "read ONCE here" sentence to cover it):

```go
	interval := t.rt.LinktestInterval()
	if interval <= 0 {
		return
	}

	// Suppression capability + flag, resolved once per Selected entry (like interval): a
	// live reconfig applies on the NEXT entry. sr == nil (capability missing or knob off)
	// runs the loop with suppression disabled.
	var sr suppressionRuntime
	if s, ok := t.rt.(suppressionRuntime); ok && s.LinktestSuppression() {
		sr = s
	}
```

…and pass it: `go t.runLinktest(ctx, g, interval, sr)`.

In `runLinktest`, change the signature to `func (t *transport) runLinktest(ctx context.Context, g *genWG, interval time.Duration, sr suppressionRuntime)` and insert rule 1 after the Selected-state check, before the T6 send:

```go
		if sr != nil {
			// Activity suppression rule 1: probe only a line that has been silent for a full
			// interval. A frame in either direction within the window already proves the link,
			// so re-arm for the remainder and skip — the timer converges on (lastActivity +
			// interval) without any cross-goroutine timer sharing.
			if idle := t.sinceLastActivity(); idle < interval {
				t.metrics.incLinktestSuppressed()
				timer.Reset(interval - idle)

				continue
			}
		}
```

- [ ] **Step 4: Run tests**

Run: `go test ./hsmsss/ -run 'TestHSMS_LinktestSuppression|TestHSMS_LinktestFailThreshold' -race -v && go test ./hsmsss/ -race -count=1`
Expected: all PASS, including the pre-existing threshold test (its link is idle — only linktest frames flow, and each probe's own send stamp still leaves idle ≥ interval at the next fire) and the transport unit tests (their mocks lack the capability, so `sr == nil` and behavior is unchanged).

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/transport_procedures.go hsmsss/integration_linktest_suppression_test.go
git commit -m "feat(hsmsss): suppress auto-linktest while the line is active"
```

---

### Task 5: Rule 2 — inflight skip

**Files:**
- Modify: `hsmsss/transport_procedures.go` (`runLinktest`)
- Test: `hsmsss/integration_linktest_suppression_test.go` (append)

**Interfaces:**
- Consumes: `sr.DataMsgInflight()` (Task 4's `sr`), rule-1 block from Task 4 (the new branch goes directly below it, inside the same `if sr != nil`).

- [ ] **Step 1: Write the failing test** — append:

```go
// slowEchoHandler replies after a deliberate processing delay, simulating aged equipment
// grinding through a long command (e.g. a recipe read). The delay is scenario injection.
func slowEchoHandler(delay time.Duration) hsms.DataMessageHandler {
	return func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if !msg.WaitBit() {
			return
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		go func() { // off the recv goroutine: handlers must not block it
			time.Sleep(delay) // the simulated processing time itself
			_ = ep.ReplyDataMessage(context.Background(), msg, item)
		}()
	}
}

// TestHSMS_LinktestSuppression_NoProbeWhileAwaitingReply: rule 2. During a long silent
// wait for a data reply (equipment busy processing), no NEW probe attempt may be made
// even though the line is idle far beyond the linktest interval — T3 owns that window.
// The baseline is taken only after the transaction is confirmed inflight, so a legal
// idle probe from the pre-send window cannot fail the assertion.
func TestHSMS_LinktestSuppression_NoProbeWhileAwaitingReply(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	const processing = 4 * suppInterval // 1.2s of line silence while "processing" (T3 base is 3s)

	passive := newEndpoint(t, port, false, nil, slowEchoHandler(processing))
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)
	coreMetrics := active.conn.Metrics()

	done := make(chan error, 1)
	go func() {
		_, err := active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("recipe?"))
		done <- err
	}()

	// Gate: the transaction is on the wire and awaiting its reply.
	require.Eventually(t, func() bool {
		return coreMetrics.DataMsgInflightCount() == 1
	}, 5*time.Second, 5*time.Millisecond, "the slow transaction must become inflight")

	baseSend := m.LinktestSendCount()

	select {
	case err := <-done:
		require.NoError(t, err, "the slow transaction must complete within T3")
	case <-time.After(10 * time.Second):
		t.Fatal("slow transaction did not complete")
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no probe attempt may be made while the reply was outstanding, despite >interval of silence")
	require.Positive(t, m.LinktestSuppressedCount())
	require.Equal(t, hsms.SelectedState, active.conn.State())
}
```

Add `"context"` to the file's imports for `slowEchoHandler`.

- [ ] **Step 2: Run to verify the teeth**

Run: `go test ./hsmsss/ -run 'NoProbeWhileAwaitingReply' -race -v`
Expected: FAIL on the Task-4 code — the line is silent during the 1.2s wait, so rule 1 alone lets probe attempts through (`LinktestSendCount` grows past the baseline). This failure is exactly the gap rule 2 closes.

- [ ] **Step 3: Implement** — inside the `if sr != nil` block from Task 4, directly after the rule-1 `if`:

```go
			// Activity suppression rule 2: a sent data message awaiting its reply means the
			// peer may be busy processing a long-running command; the T3 reply timer already
			// bounds that wait, so probing now only risks a spurious T6 on slow equipment.
			// Re-check at full-interval cadence.
			if sr.DataMsgInflight() > 0 {
				t.metrics.incLinktestSuppressed()
				timer.Reset(interval)

				continue
			}
```

- [ ] **Step 4: Run tests**

Run: `go test ./hsmsss/ -run 'TestHSMS_LinktestSuppression|TestHSMS_LinktestFailThreshold' -race -v`
Expected: all PASS.

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/transport_procedures.go hsmsss/integration_linktest_suppression_test.go
git commit -m "feat(hsmsss): skip auto-linktest while a data reply is outstanding"
```

---

### Task 6: Rule 3 — liveness credit

**Files:**
- Modify: `hsmsss/transport_procedures.go` (`linktestFailureStep` + `runLinktest` failure branch)
- Test: `hsmsss/integration_linktest_suppression_test.go` (append), `hsmsss/transport_activity_test.go` (append reducer tables)

**Interfaces:**
- Consumes: `t.lastRecvStamp`/`t.monoNanos()` (Task 2), `sr.DataMsgInflight()` (Task 4), `t.metrics.incLinktestCredited()` (Task 3), `ChaosProxy` + `waitNotConnectedEvent` (existing harness).
- Produces: `linktestFailureStep(suppress bool, recvNow, sentAt, inflight int64, fails int, recvAtLastFail int64) (newFails int, newRecvAtLastFail int64, credited bool)` — the pure rule-3 STATE REDUCER. A reducer (not just a classifier) so the full count → restart → disconnect sequence, including the evolving `fails`/`recvAtLastFail` state, is unit-testable end to end (review round 3).

- [ ] **Step 1: Write the failing test** — append. Causality is anchored at the ChaosProxy: the proxy filter signals on a channel when it actually OBSERVES the outbound Linktest.req on the wire, and only then does the test inject the proof-of-life frame — so the frame provably arrives after the probe, closing the "attempt counter is not an on-wire barrier" gap. Two phases: (A) linktest responses dropped while the peer keeps showing life → failures credited, no disconnect; (B) the life signal stops → consecutive uncredited failures reach the threshold → disconnect. Phase B proves credit cannot mask a dead link.

```go
// TestHSMS_LinktestSuppression_LivenessCreditForgivesFailures: rule 3 (phase A), then
// proves the disconnect threshold still has teeth once all signs of life stop (phase B).
func TestHSMS_LinktestSuppression_LivenessCreditForgivesFailures(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	// probeSeen fires when the proxy observes an outbound Linktest.req ON THE WIRE —
	// the causal anchor for "this frame arrived after that probe".
	probeSeen := make(chan struct{}, 16)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if len(header) < 10 {
			return ProxyActionForward, 0
		}
		if isClientToTarget && header[5] == byte(hsms.LinktestReqType) {
			select {
			case probeSeen <- struct{}{}:
			default:
			}
			return ProxyActionForward, 0
		}
		if !isClientToTarget && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0 // every probe of this test times out on T6
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(3)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Phase A: for each of 4 rounds (> threshold), wait until a probe is on the wire,
	// then deliver proof of life inside its T6 window. Every timeout must be credited.
	for round := 1; round <= 4; round++ {
		select {
		case <-probeSeen:
		case <-time.After(30 * suppInterval):
			t.Fatalf("round %d: no Linktest.req observed on the wire", round)
		}

		_, sendErr := passive.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("alive"))
		require.NoError(t, sendErr)

		errsWant := uint64(round)
		require.Eventually(t, func() bool {
			return m.LinktestErrCount() >= errsWant && m.LinktestCreditedCount() >= errsWant
		}, 30*suppInterval, 5*time.Millisecond, "round %d: the T6 failure must be credited", round)
	}
	require.GreaterOrEqual(t, m.LinktestCreditedCount(), uint64(3),
		"at least threshold-many failures were credited")
	assertStaysSelected(t, active, suppT6+2*suppInterval)

	// Phase B: stop injecting life. Consecutive uncredited failures must now reach the
	// threshold and drop the link — credit must never mask a link that stopped showing life.
	waitNotConnectedEvent(t, active)
}
```

- [ ] **Step 2: Run to verify the teeth**

Run: `go test ./hsmsss/ -run 'LivenessCreditForgivesFailures' -race -v`
Expected: FAIL on Task-5 code — without credit, the dropped responses accumulate `fails` and the link drops during phase A (threshold 3 < 4 rounds), so `assertStaysSelected` or an earlier round's Eventually sees the disconnect. Note phase-A pacing: each proof-of-life primary resets the activity window, so consecutive probes are naturally ≥ interval apart — the per-round Eventually windows absorb that; what cannot happen pre-implementation is surviving 4 dropped probes with threshold 3.

- [ ] **Step 3: Implement** — the failure handling is a PURE STATE REDUCER so every branch AND the evolving state (including sequences that cannot be scheduled deterministically in integration tests) is exhaustively unit-testable. Add to `hsmsss/transport_procedures.go`:

```go
// linktestFailureStep is the pure state reducer for a failed probe round-trip (D5a-5
// suppression rule 3). Inputs are the loop's snapshot at failure-evaluation time; outputs
// are the new consecutive-failure run state and whether the failure was credited.
//
//   - suppress off: always counts (pre-suppression v2 semantics).
//   - recvNow > sentAt: a frame arrived after the probe went out — credit, run resets.
//   - inflight > 0: a data send won the write path between the probe's pre-send check and
//     its write; the peer sits inside a T3-guarded transaction — credit (race closure).
//   - fails > 0 && recvNow > recvAtLastFail: life between counted failures — run restarts
//     at 1 with this failure.
//   - otherwise: consecutive-in-silence — increment the run.
//
// Note "life" is receive-side only plus inflight: our own send stamps never forgive a
// failure (a write success proves local TCP buffering, not the peer).
func linktestFailureStep(suppress bool, recvNow, sentAt, inflight int64, fails int, recvAtLastFail int64) (newFails int, newRecvAtLastFail int64, credited bool) {
	if suppress && (recvNow > sentAt || inflight > 0) {
		return 0, recvAtLastFail, true
	}
	if suppress && fails > 0 && recvNow > recvAtLastFail {
		return 1, recvNow, false
	}

	return fails + 1, recvNow, false
}
```

In `runLinktest`, capture the send instant, add the failure-memory local (`recvAtLastFail := int64(0)` declared next to `fails`), and replace the failure branch (success branch unchanged). The threshold decision includes the FINAL pre-disconnect re-check from the Design Reference's accepted-races contract:

```go
		sentAt := t.monoNanos()
		t6 := t.rt.Timers().T6
		lctx, cancel := context.WithTimeout(ctx, t6)
		t.metrics.incLinktestSend()
		_, err := t.rt.WriteMessage(lctx, hsms.NewLinktestReq(t.rt.NextSystemBytes()))
		cancel()

		if err != nil {
			// A cancelled PARENT ctx (teardown / Deselect / drop) is not a linktest failure.
			if ctx.Err() != nil {
				return
			}
			t.metrics.incLinktestErr()

			recvNow := t.lastRecvStamp.Load()
			var inflight int64
			if sr != nil {
				inflight = sr.DataMsgInflight()
			}

			var credited bool
			fails, recvAtLastFail, credited = linktestFailureStep(sr != nil, recvNow, sentAt, inflight, fails, recvAtLastFail)
			if credited {
				// The cumulative err metric above still counts the failure; only the
				// CONSECUTIVE counter driving the disconnect is spared.
				t.metrics.incLinktestCredited()
			}

			if fails >= threshold {
				// Final pre-disconnect re-check (accepted-races (b)): a W-bit send may have
				// become inflight — or a frame arrived — after the snapshot above. Convert
				// to a credit rather than dropping a link that just showed life. The
				// remaining unguarded window is the instructions between this check and
				// TCPDown (documented, accepted).
				if sr != nil && (sr.DataMsgInflight() > 0 || t.lastRecvStamp.Load() > sentAt) {
					t.metrics.incLinktestCredited()
					fails = 0
				} else {
					t.rt.TCPDown(errLinktestFailed)
					return
				}
			}
		} else {
			t.metrics.incLinktestRecv()
			fails = 0
		}
```

A dead link delivers no frames and opens no transactions: `recvNow` stops advancing, no branch credits it, and the threshold trips within ≈ `threshold × (interval + T6)`.

- [ ] **Step 3b: Write the exhaustive reducer tests** — append to `hsmsss/transport_activity_test.go`. Two layers: a branch table, then a SEQUENCE test that threads the reducer's outputs back as inputs — this is the deterministic proof of the count → restart → disconnect state path that no socket test can force (review round 3):

```go
func TestLinktestFailureStep_Branches(t *testing.T) {
	tests := []struct {
		name           string
		suppress       bool
		recvNow        int64
		sentAt         int64
		inflight       int64
		fails          int
		recvAtLastFail int64
		wantFails      int
		wantMem        int64
		wantCredited   bool
	}{
		{"suppression off always counts", false, 100, 50, 5, 2, 0, 3, 100, false},
		{"frame after probe credits, run resets", true, 100, 50, 0, 2, 10, 0, 10, true},
		{"inflight at failure credits (race closure)", true, 40, 50, 1, 2, 40, 0, 40, true},
		{"silence with no history counts", true, 40, 50, 0, 0, 0, 1, 40, false},
		{"consecutive silence increments", true, 40, 50, 0, 2, 40, 3, 40, false},
		{"life since last counted failure restarts run at 1", true, 45, 50, 0, 2, 40, 1, 45, false},
		{"restart needs prior fails", true, 45, 50, 0, 0, 40, 1, 45, false},
		{"equal stamps do not credit (strict >)", true, 50, 50, 0, 0, 0, 1, 50, false},
		{"equal recvAtLastFail does not restart (strict >)", true, 40, 50, 0, 1, 40, 2, 40, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFails, gotMem, gotCredited := linktestFailureStep(tt.suppress, tt.recvNow, tt.sentAt, tt.inflight, tt.fails, tt.recvAtLastFail)
			require.Equal(t, tt.wantFails, gotFails)
			require.Equal(t, tt.wantMem, gotMem)
			require.Equal(t, tt.wantCredited, gotCredited)
		})
	}
}

// TestLinktestFailureStep_Sequence: the stateful proof the integration suite cannot force
// deterministically. Threshold 2. Silent failure counts (run=1); a frame between probes
// restarts the run at 1 instead of disconnecting at 2; two further silent failures then
// reach the threshold exactly.
func TestLinktestFailureStep_Sequence(t *testing.T) {
	const threshold = 2
	fails, mem := 0, int64(0)
	var credited bool

	// Probe 1 at sentAt=100, total silence (recvNow=90 predates it): counted, run=1.
	fails, mem, credited = linktestFailureStep(true, 90, 100, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 1, fails)
	require.Less(t, fails, threshold)

	// A frame arrives at t=150, BETWEEN probes. Probe 2 at sentAt=200 fails in silence:
	// recvNow(150) predates sentAt (no credit) but postdates mem(90) — run RESTARTS at 1.
	// A build missing the recvAtLastFail state would count to 2 and disconnect here.
	fails, mem, credited = linktestFailureStep(true, 150, 200, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 1, fails, "life between counted failures must restart the run, not extend it")
	require.Less(t, fails, threshold)

	// Probe 3 in total silence: the restarted run increments to 2 -> threshold reached.
	fails, mem, credited = linktestFailureStep(true, 150, 300, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 2, fails)
	require.GreaterOrEqual(t, fails, threshold, "an uninterrupted silent run must still disconnect exactly at threshold")
	_ = mem
}
```

- [ ] **Step 3c: Write the recvAtLastFail smoke test** — append to the integration suite. SMOKE, not proof (review round 3): `LinktestErrCount` increments before classification, so the injected frame may land as a direct credit on failure 1 or a pending failure 2 — under those interleavings the `>= 3` oracle passes even without the failure-memory wiring. The deterministic proof of the restart branch is `TestLinktestFailureStep_Sequence` (Step 3b); this test only smoke-checks the wiring against real sockets:

```go
// TestHSMS_LinktestSuppression_LifeBetweenFailuresRestartsRun: SMOKE for the failure-memory
// wiring (the deterministic proof is TestLinktestFailureStep_Sequence). With threshold 2
// and every Linktest.rsp dropped, one failure is recorded, a data frame is injected, and
// the disconnect must not arrive before cumulative LinktestErrCount >= 3 under any of the
// admitted interleavings (between-probes restart, or direct credit on a pending probe).
func TestHSMS_LinktestSuppression_LifeBetweenFailuresRestartsRun(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(2)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Failure #1 is counted (no life anywhere near it).
	require.Eventually(t, func() bool {
		return m.LinktestErrCount() >= 1
	}, 30*suppInterval, 5*time.Millisecond, "first probe failure must be recorded")

	// Life BETWEEN probes: the failed probe has resolved (err counted); the next probe
	// needs a fresh interval of silence after this frame, so it provably lands between.
	_, sendErr := passive.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("alive"))
	require.NoError(t, sendErr)

	// No further life: the run must restart at 1 (errs=2), then reach 2 (errs=3) -> drop.
	waitNotConnectedEvent(t, active)
	require.GreaterOrEqual(t, m.LinktestErrCount(), uint64(3),
		"the run must restart after mid-run life: disconnect at >= 3 cumulative errors, not 2")
}
```

Interleaving note: the injected frame may land between probes (restart branch), during a pending probe (direct credit), or even credit failure 1 itself — the `>= 3` assertion holds under all of them, which is precisely why this test is smoke rather than the branch proof; the reducer sequence test carries that burden.

- [ ] **Step 4: Run tests**

Run: `go test ./hsmsss/ -run 'TestLinktestFailureStep|TestHSMS_LinktestSuppression|TestHSMS_LinktestFailThreshold' -race -v && go test ./hsmsss/ -race -count=1`
Expected: all PASS. The pre-existing threshold test carries no non-linktest traffic and no inflight transactions, so no credit fires there and its phase structure is unchanged.

- [ ] **Step 5: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/transport_procedures.go hsmsss/integration_linktest_suppression_test.go hsmsss/transport_activity_test.go
git commit -m "feat(hsmsss): credit linktest failures when the link shows other signs of life"
```

---

### Task 7: End-to-end "step in" proof, reconfig semantics, async traffic, audit

**Files:**
- Test: `hsmsss/integration_linktest_suppression_test.go` (append)
- Possibly modify: any existing test that assumed unconditional linktest cadence (audit below)

**Interfaces:** consumes everything from Tasks 1–6; no new production code.

- [ ] **Step 1: Write the end-to-end wedged-peer test** — the user story: equipment accepts a command and wedges; go-secs must NOT probe during the configured wait, must fail the transaction at T3, and must then detect the dead peer via resumed probes and disconnect within ≈ `T3 + threshold × (interval + T6)`.

```go
// TestHSMS_LinktestSuppression_WedgedPeerStepsInAfterT3: the end-to-end guarantee. A peer
// that accepts a command and then goes fully silent (no reply, no linktest answers) is
// (1) left unprobed while the reply is outstanding, (2) surfaced to the caller as a T3
// reply timeout at the caller's configured deadline, and (3) disconnected after by the
// resumed auto-linktest. Total detection ~= T3 + threshold*(interval + T6).
func TestHSMS_LinktestSuppression_WedgedPeerStepsInAfterT3(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// The passive never replies to data (no handler) — a wedged command processor.
	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0 // linktests are never answered either
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	const shortT3 = 1 * time.Second

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT3(shortT3)),
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(2)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)
	coreMetrics := active.conn.Metrics()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("recipe?"))
		done <- err
	}()

	require.Eventually(t, func() bool {
		return coreMetrics.DataMsgInflightCount() == 1
	}, 5*time.Second, 5*time.Millisecond, "the doomed transaction must become inflight")

	baseSend := m.LinktestSendCount()

	// (1)+(2): the transaction fails at the T3 deadline, and no probe attempt was made
	// while it was outstanding.
	var sendErr error
	select {
	case sendErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wedged transaction never resolved")
	}
	elapsed := time.Since(start)
	require.Error(t, sendErr, "the wedged transaction must fail")
	require.GreaterOrEqual(t, elapsed, shortT3-50*time.Millisecond,
		"the caller must get the full configured T3 window")
	require.Equal(t, baseSend, m.LinktestSendCount(),
		"no probe attempt may be made while the reply was outstanding")

	// (3): with nothing inflight and the line silent, probing resumes; threshold
	// unanswered probes later the dead link is dropped. waitNotConnectedEvent observes
	// the state EVENT, so the auto-reconnect that follows cannot hide the disconnect.
	waitNotConnectedEvent(t, active)
	require.GreaterOrEqual(t, m.LinktestSendCount(), baseSend+2,
		"the disconnect must have been driven by resumed, unanswered probes")
}
```

Before finalizing, check the exact T3-timeout error identity: `grep -n "ErrT3Timeout" hsms/*.go`. If `hsms.ErrT3Timeout` is exported, strengthen `require.Error(t, sendErr)` to `require.ErrorIs(t, sendErr, hsms.ErrT3Timeout)`.

- [ ] **Step 2: Write the continuous fire-and-forget test** — the documented starvation trade-off, asserted intentionally:

```go
// TestHSMS_LinktestSuppression_AsyncTrafficSuppresses: continuous fire-and-forget sends
// (no W-bit, so nothing inflight) still count as line activity — probes stay suppressed.
// This is the documented trade-off for streaming/relay workloads; the knob-off test above
// proves the escape hatch.
func TestHSMS_LinktestSuppression_AsyncTrafficSuppresses(t *testing.T) {
	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)

	// Prime with one no-reply send, then baseline.
	_, err := active.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()

	deadline := time.Now().Add(8 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 6, 11, false, secs2.NewASCIIItem("stream"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing
	}

	require.Equal(t, baseSend, m.LinktestSendCount(),
		"fire-and-forget traffic must suppress probes (documented starvation trade-off)")
	require.Equal(t, hsms.SelectedState, active.conn.State())
}
```

- [ ] **Step 3: Reconfig semantics (MANDATORY — both halves)** — prove `WithLinktestSuppression` follows the same next-Selected-entry rule as `WithLinktestInterval`. Config-rail half: add to `hsms/connection_config_test.go` an assertion that `UpdateConfigOptions(WithLinktestSuppression(false))` flips the live config value (reuse the file's existing pattern). Behavioral half: the reconnect-forcing mechanism already exists — `ChaosProxy.CloseConnections` (`hsmsss/harness_chaos_proxy_test.go:147-160`) plus the NotConnected-event / re-Selected wait pattern in `hsmsss/integration_connect_reconnect_test.go:75-105`. Write:

```go
// TestHSMS_LinktestSuppression_ReconfigAppliesNextSelected: a mid-session
// UpdateConfigOptions(WithLinktestSuppression(false)) does NOT change the current
// Selected session (its goroutine captured the flag at entry), but DOES apply to the
// re-Selected successor after a forced reconnect.
func TestHSMS_LinktestSuppression_ReconfigAppliesNextSelected(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP) // no filter: pure pass-through + kill switch
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	m := controlMetrics(t, active)

	// Toggle mid-session: current session must keep suppressing.
	require.NoError(t, active.conn.UpdateConfigOptions(hsms.WithLinktestSuppression(false)))

	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("prime"))
	require.NoError(t, err)
	baseSend := m.LinktestSendCount()

	deadline := time.Now().Add(5 * suppInterval)
	for time.Now().Before(deadline) {
		_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
		require.NoError(t, err)
		time.Sleep(suppInterval / 5) // scenario pacing
	}
	require.Equal(t, baseSend, m.LinktestSendCount(),
		"the current session must retain its captured suppression=on")

	// Force a reconnect; the successor session captures suppression=off at entry.
	proxy.CloseConnections()
	waitNotConnectedEvent(t, active)
	waitSelected(t, active)
	drainStateCh(active.states)

	baseSend = m.LinktestSendCount()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ping"))
				time.Sleep(suppInterval / 5)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	require.Eventually(t, func() bool {
		return m.LinktestSendCount() > baseSend
	}, 20*suppInterval, 10*time.Millisecond,
		"the re-Selected session must probe despite traffic: suppression=off applied at entry")
}
```

Verify `UpdateConfigOptions` is reachable from the harness's `hsms.Connection` value (it is on the endpoint surface, `hsms/endpoint.go:183`); adjust the call site if the concrete accessor differs.

- [ ] **Step 3b: Inflight terminal-outcome matrix** — a leaked inflight gauge would suppress probing forever, so all four reply outcomes must provably return it to zero with probing resumed. Append:

```go
// TestHSMS_LinktestSuppression_InflightGaugeTerminalOutcomes: reply, T3 timeout, caller
// cancellation, and connection drop each return DataMsgInflightCount to zero — the gauge
// the inflight-skip rule depends on. A leak would disable dead-link detection forever.
func TestHSMS_LinktestSuppression_InflightGaugeTerminalOutcomes(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	// Passive replies only to S99 (echoHandler ignores nothing, so use a selective handler):
	// S99Fx gets an echo; S7Fx gets silence (drives T3/cancel/drop cases).
	selective := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Stream() != 99 || !msg.WaitBit() {
			return
		}
		item, err := msg.Item()
		if err != nil {
			return
		}
		_ = ep.ReplyDataMessage(context.Background(), msg, item)
	}

	passive := newEndpoint(t, portP, false, nil, selective)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT3(1 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(suppInterval)),
		WithConnectionOption(hsms.WithT6(suppT6)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	gauge := active.conn.Metrics()
	zero := func(label string) {
		require.Eventually(t, func() bool {
			return gauge.DataMsgInflightCount() == 0
		}, 5*time.Second, 5*time.Millisecond, "gauge must return to zero after %s", label)
	}

	// 1. Reply.
	_, err := active.conn.SendDataMessage(ctx, 99, 1, true, secs2.NewASCIIItem("ok"))
	require.NoError(t, err)
	zero("reply")

	// 2. T3 timeout (S7 gets no reply). After the terminal, prove probing RESUMES —
	// the gauge returning to zero is only half the promise; a wedged suppression
	// state would still never probe again.
	_, err = active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("t3"))
	require.Error(t, err)
	zero("T3 timeout")

	probeBase := controlMetrics(t, active).LinktestSendCount()
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestSendCount() > probeBase
	}, 20*suppInterval, 10*time.Millisecond,
		"idle probing must resume once the T3 terminal cleared the gauge")

	// 3. Caller cancellation.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := active.conn.SendDataMessage(cctx, 7, 5, true, secs2.NewASCIIItem("cancel"))
		done <- err
	}()
	require.Eventually(t, func() bool { return gauge.DataMsgInflightCount() == 1 },
		5*time.Second, 5*time.Millisecond)
	cancel()
	require.Error(t, <-done)
	zero("caller cancellation")

	// 4. Connection drop mid-wait. After the drop, wait for the auto-reconnect to
	// re-Select, then prove idle probing resumes in the successor session too.
	drainStateCh(active.states)
	go func() {
		_, _ = active.conn.SendDataMessage(ctx, 7, 5, true, secs2.NewASCIIItem("drop"))
	}()
	require.Eventually(t, func() bool { return gauge.DataMsgInflightCount() == 1 },
		5*time.Second, 5*time.Millisecond)
	proxy.CloseConnections()
	waitNotConnectedEvent(t, active)
	zero("connection drop")

	waitSelected(t, active)
	probeBase = controlMetrics(t, active).LinktestSendCount()
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestSendCount() > probeBase
	}, 20*suppInterval, 10*time.Millisecond,
		"the re-Selected session must resume idle probing after the drop terminal")
}
```

Add `"context"` to imports if not already present from Task 5.

- [ ] **Step 4: Audit existing tests for cadence assumptions**

Run: `grep -ln "inktest" hsmsss/*_test.go hsms/*_test.go` and inspect each hit for assumptions that linktests fire *despite traffic* or that failures always increment the consecutive counter. Expected: the threshold test (idle link — unaffected), metrics tests (linktest traffic on otherwise-idle links — unaffected), chaos tests (inspect each). For any test that intends v2.0.x unconditional-cadence semantics, add `WithConnectionOption(hsms.WithLinktestSuppression(false))` to ITS options with a one-line comment saying why, rather than weakening its assertions.

- [ ] **Step 5: Full verification**

```bash
make test            # full suite, race, -short
make stress-quick    # flake-prone narrow set
go test ./hsmsss/ -run 'TestHSMS_LinktestSuppression' -race -count=10   # stability of the new suite
```
Expected: all green, 10/10 stable. Flake posture (review round 2): the `TestLinktestFailureStep` table (Task 6) is the deterministic regression oracle for every rule-3 branch; the real-socket integration tests are smoke coverage of the wiring on top of it. If a timing-sensitive integration test flakes under CI scheduling pauses (a pause > the remaining idle budget legitimately opens a full-interval silence gap), fix its timing structure (bigger multiples of `suppInterval`, proxy-anchored causality) — do not loosen assertions to "usually zero", and never delete the classifier tables to compensate.

- [ ] **Step 6: go fix + lint + commit**

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add hsmsss/integration_linktest_suppression_test.go hsms/connection_config_test.go   # plus any audited test files
git commit -m "test(hsmsss): end-to-end wedged-peer step-in proof, reconfig semantics, cadence audit"
```

---

### Task 8: Documentation

**Files:**
- Modify: `README.md` (Resilience bullet, ~line 46)
- Modify: `CHANGELOG.md` (new release section at top)
- Modify: `docs/migration-v1-to-v2.md` (the `WithAutoLinktest` removal row ~line 1032 and the linktest note ~line 423)
- Modify: `hsmsss/doc.go` (feature mention near the linktest text)

**Interfaces:** none — prose only. Public wording: SEMI terms OK, internal codes forbidden.

- [ ] **Step 1: README** — extend the HSMS Resilience bullet:

```markdown
* **Resilience:** automatic reconnection, and an auto-linktest with a configurable failure threshold
  for tolerating transient T6 timeouts. Activity-based linktest suppression (on by default) probes
  only idle links: each probe opportunity is skipped when it observes recent traffic or an
  outstanding data reply, and a probe timeout is not counted toward the disconnect threshold when
  the failure evaluation observes signs of life (received frames, or a reply still outstanding) —
  protecting slow, aged equipment busy with a long command from probe-induced disconnects. A silent
  dead link is still dropped within about threshold × (interval + T6). Disable with
  `hsms.WithLinktestSuppression(false)`.
```

- [ ] **Step 2: CHANGELOG** — inspect the file's existing format, then add a new `## v2.1.0` section above the `v2.0.1` entry (this is a minor feature release: new option, new metrics, default behavior change). Content to convey:
  - **Added:** `hsms.WithLinktestSuppression` (default enabled): activity-based auto-linktest suppression — each probe opportunity is skipped when it observes recent line traffic or an outstanding data reply, and a probe timeout is not counted toward the fail threshold when the failure evaluation observes signs of life (liveness credit). `hsmsss.ConnectionMetrics` gains `LinktestSuppressedCount` / `LinktestCreditedCount`. Restores and extends v1's linktest suppression (v1 commit `77262a6`). See the `WithLinktestSuppression` godoc for the precise guarantees and bounds.
  - **Changed (behavior, on by default):** on busy connections `LinktestSendCount` / `LinktestRecvCount` stop climbing (probes are suppressed while traffic flows), and `LinktestErrCount` can grow without a disconnect when failures are credited — dashboards or alerts built on the old always-probing cadence must account for this or set `WithLinktestSuppression(false)`.
  - Detection bound statement: a silent dead link is dropped within ≈ `threshold × (interval + T6)`; with a transaction outstanding, T3 applies first.

- [ ] **Step 3: Migration doc** — update the removed-options table row for `WithAutoLinktest` to note that v1's send/receive-activity linktest suppression is restored in v2 as `hsms.WithLinktestSuppression` (default on, extended with the outstanding-reply skip and liveness credit), and adjust the prose note near line 423 accordingly.

- [ ] **Step 4: hsmsss/doc.go** — one sentence where linktest is described, pointing at `hsms.WithLinktestSuppression`.

- [ ] **Step 5: go fix + lint + commit** — `hsmsss/doc.go` is a `.go` file, so the full rule-700 sequence applies here too:

```bash
go fix ./... && git diff        # review the full diff: behavior-preserving only
make lint
git add README.md CHANGELOG.md docs/migration-v1-to-v2.md hsmsss/doc.go
git commit -m "docs: document activity-based linktest suppression"
```

---

## Review Round 1 Resolutions (tmp/linktest-suppression_initial_review.md)

- **P0 (check-to-wire linearization):** resolved without a new core write seam — rule 3 now also credits a failure when `DataMsgInflight() > 0` at failure time (closes the disconnect-mid-transaction race, including threshold 1), and failures only count when consecutive-in-silence (`recvAtLastFail` memory). The residual pre-wire false-credit window is bounded to ≤ one probe cycle of detection delay and is documented in "Accepted, documented races" — a deliberate contract, not an oversight.
- **P0 (detection bound):** prose corrected everywhere to `threshold × (interval + T6)` (+ T3 when a transaction was outstanding); the probe cadence itself intentionally keeps the v1/v2 shape.
- **P1 (TransportRuntime break):** no interface widening; getters live on the unexported `connection`, consumed via the package-local `suppressionRuntime` assertion. In-repo mocks compile unchanged and exercise the suppression-off path.
- **P1 (test compile/causality):** existing `echoHandler` reused (no redeclaration); all "no probe" assertions are baseline-delta gated on a controlled precondition (round trip or inflight==1); rule-3 causality anchored at the ChaosProxy's on-wire observation of the Linktest.req.
- **P2 (release/workflow):** CHANGELOG targets `v2.1.0` with an explicit dashboards/alerts note; every commit step runs `go fix ./...` + diff review before `make lint` per `.agents/rules/700-lint-after-write.md`.
- **Reviewer's additional tests:** generation-stamp reset (Task 2 unit + `Start` re-baseline), reconfig next-Selected semantics (Task 7 step 3), continuous fire-and-forget suppression (Task 7 step 2), inflight terminal matrix (existing single-defer structure in `hsms/connection_send.go` already covers the four outcomes; the wedged-peer test exercises the T3-timeout path end-to-end and probing provably resumes after it).

## Review Round 2 Resolutions (tmp/linktest-suppression_round2_review.md)

- **P0 (contract overclaim):** the plan no longer claims "zero false disconnects" or a universal one-cycle stale-credit bound. The "Accepted, documented races" section now states the precise guarantee — with `threshold >= 2` no single race can disconnect a link that showed life; `threshold == 1` retains pre-existing v2.0.x single-timeout-disconnect semantics (suppression strictly narrows that window); a reconnect straggler costs at most one additional threshold-length run (`<= 2 x threshold x (interval + T6)`). The Task-1 godoc recommends `threshold >= 2`. The reviewer's causal core seam remains deliberately declined: every residual race is now bounded and stated, per the reviewer's own "weaken the contract honestly" alternative.
- **P1 (publish sites):** Task 2 now wires `resetActivityStamps()` beside both real conn publications — `startActive` (`hsmsss/transport_active.go`) and `acceptLoop` (`hsmsss/transport_passive.go`) — with an explicit wiring note that `Start` has no such site, plus production-path baseline tests for both roles modeled on `transport_dialer_test.go`/`transport_listener_test.go`.
- **P1 (regression-proof coverage):** rule-3 classification extracted into a pure function with an exhaustive table test (renamed to the `linktestFailureStep` reducer in round 3), a dedicated `LifeBetweenFailuresRestartsRun` test for the `recvAtLastFail` path (reclassified as smoke in round 3), the reconfig next-Selected test made MANDATORY via `ChaosProxy.CloseConnections`, and an inflight terminal-outcome matrix (reply / T3 / caller-cancel / conn-drop all return the gauge to zero).
- **P2 (Task 8 workflow):** Task 8's commit step now runs `go fix ./...` + full-diff review before `make lint`; all diff reviews upgraded from `--stat` to full diffs.

## Review Round 5 Resolution (tmp/linktest-suppression_round5_review.md)

Round 5 confirmed the Task-1 godoc fix and flagged the same absolute-claim pattern in Task 8's README/CHANGELOG prose. Fixed — both now use observation-conditioned wording ("each probe opportunity is skipped when it observes …", "when the failure evaluation observes signs of life", "protecting … from probe-induced disconnects") and the CHANGELOG defers precise guarantees to the godoc. No public prose states an unqualified no-probe/no-disconnect absolute.

## Review Round 4 Resolution (tmp/linktest-suppression_round4_review.md)

Round 4 confirmed every round-3 resolution (red-capable reset tests, reducer sequence oracle, full diffs, terminal-matrix probing-resumes) and left ONE P0: the Task-1 godoc overpromised relative to the contract. Fixed — the godoc now states the outbound-traffic asymmetry explicitly ("outbound traffic suppresses probing, but only received frames or an outstanding reply count as proof of peer liveness … a successful write proves local buffering, not the peer") and qualifies the threshold guidance with the final re-check plus its residual window ("life arriving in the final instants of that decision can still be missed"). No public prose now exceeds the accepted-races contract.

## Review Round 3 Resolutions (tmp/linktest-suppression_round3_review.md)

- **P0 (threshold>=2 guarantee counterexamples):** resolved by contract precision + one code addition, not a core seam. "Sign of life" is now DEFINED (received frames or an open T3-guarded transaction; our own writes are not life), which makes the fire-and-forget counterexample a contractually correct disconnect of a peer silent through the whole failure run. The W-bit TOCTOU is closed to an instruction-sized window by a mandatory final pre-disconnect re-check (`DataMsgInflight() > 0 || lastRecvStamp > sentAt` → credit instead of `TCPDown`), stated in accepted-races (b) and implemented in Task 6's loop. The Goal line, README bullet, and guarantee paragraph were all reworded to exactly this contract — no stronger prose remains.
- **P1 (reset tests not red-capable):** tests rewritten — stamps seeded hour-old (`monoNanos() - 1h`, not `1`), assertions target `lastRecvStamp` against a SILENT peer (handshake writes freshen the send stamp regardless, so only the recv stamp distinguishes the reset from traffic), and the scaffolding references corrected to `transport_test.go:208-229` (active direct-Start) and `newPipeListener` (`transport_listener_test.go:123-179`) for passive. Removing either production reset call now turns its test red.
- **P1 (failure-memory oracle):** the classifier became the `linktestFailureStep` REDUCER returning `(newFails, newRecvAtLastFail, credited)`; `TestLinktestFailureStep_Sequence` threads state through count → restart-on-life → disconnect-at-threshold deterministically. `LifeBetweenFailuresRestartsRun` is explicitly demoted to real-socket smoke with its interleavings documented.
- **P2 (--stat diffs):** every task's commit step now reviews the full `go fix` diff (`git diff`), matching rule 700.
- **Terminal matrix extension (reviewer suggestion):** after the T3 terminal the test now proves idle probing RESUMES (send count grows), and after the drop terminal it waits for re-Selected and proves the successor session probes — the gauge reaching zero is no longer the only oracle.

## Self-Review Notes

- Spec coverage: rule 1 → Task 4, rule 2 → Task 5, rule 3 (extended) → Task 6, knob+default → Task 1 (+Task 4 knob-off test), stamps+reset → Task 2, observability → Task 3, end-to-end story + reconfig + async + audit → Task 7, docs → Task 8.
- Teeth: Tasks 4, 5, 6 each begin with a test that FAILS on the preceding task's code; Task 7's audit protects the pre-existing suite; the idle-probe test guards against over-suppression.
- Type consistency: `runLinktest(ctx, g, interval, sr suppressionRuntime)` is introduced in Task 4 and only extended (never re-signed) in Tasks 5–6; metric names `LinktestSuppressedCount`/`LinktestCreditedCount` are identical in Tasks 3–7; `recvAtLastFail` is declared in Task 6 where it is used.
- Known verification points for the implementer: bare-transport constructor for `newTestTransport` (Task 2), the exact `t.conn` publish lines in `startActive`/`acceptLoop` (Task 2), exact `ErrT3Timeout` name (Task 7), CHANGELOG format (Task 8).
