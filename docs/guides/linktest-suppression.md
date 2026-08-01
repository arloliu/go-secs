# Activity-Based Linktest Suppression

`go-secs` v2.1.0 adds activity-based suppression to the HSMS-SS auto-linktest: the connection
probes only links that are actually idle, and a probe timeout no longer counts toward the
disconnect threshold while the link shows signs of life. The feature is **enabled by default**
and exists for one core reason: aged semiconductor equipment can spend a minute or more
processing a single command (a recipe read, a long report), answer nothing during that time,
and still be perfectly healthy — probing it then, and disconnecting it on a missed probe, is
exactly the wrong move.

This guide covers the public API, the behavior contract, how to tune it, and the trade-offs.
For the precise race-window bounds, the `hsms.WithLinktestSuppression` godoc is authoritative.

## Quick start

Nothing is required — suppression is on by default whenever the auto-linktest is enabled:

```go
cfg, err := hsmsss.NewConfig("10.1.2.3", 5000,
    hsmsss.WithActive(),
    hsmsss.WithConnectionOption(hsms.WithLinktestInterval(30*time.Second)),
    hsmsss.WithConnectionOption(hsms.WithT3(2*time.Minute)), // sized for the slowest command
    hsmsss.WithConnectionOption(hsms.WithLinktestFailThreshold(3)),
)
```

To restore the v2.0.x behavior (a probe every interval, unconditionally):

```go
    hsmsss.WithConnectionOption(hsms.WithLinktestSuppression(false)),
```

The flag is also accepted by `Connection.UpdateConfigOptions`. Like `WithLinktestInterval`, a
mid-session change applies at the **next** entry to the Selected state (typically the next
reconnect), never mid-session.

## The three rules

With suppression enabled, the auto-linktest applies three rules, in order:

1. **Activity reset.** A `Linktest.req` is sent only after a full `LinktestInterval` in which
   no HSMS frame was sent *or* received. Traffic in either direction defers the probe — a line
   in active use does not need to be tested, and some older equipment is slow to answer
   `Linktest.req` while busy.
2. **Inflight skip.** No probe is sent while one of your W-bit data messages is awaiting its
   reply. During that window the **T3 reply timeout owns liveness detection**: the equipment
   gets exactly the reply window you configured, unprobed.
3. **Liveness credit.** A probe that times out (T6) is not counted toward the
   `WithLinktestFailThreshold` disconnect when the failure evaluation observes signs of life —
   a frame arrived after the probe went out, or a reply is still outstanding. Only
   *consecutive failures on a silent link* accumulate toward a disconnect, and the disconnect
   decision re-checks for life immediately before dropping the link.

One definition carries the contract: a **"sign of life" is a frame received from the peer, or
an open reply-pending transaction**. Your own successful writes defer probing (rule 1) but
never forgive a probe failure — a write success proves local TCP buffering, not the peer.

The inbound side is unchanged: a peer's `Linktest.req` to you is always answered immediately,
with or without suppression. Suppression governs only the probes this connection initiates.

## Detection bounds — when go-secs still steps in

When the link is silent or a reply is outstanding, a dead or wedged peer is detected within a
bounded time. (The continuous fire-and-forget pattern under
[Trade-offs](#trade-offs-and-limitations) is the documented exception — there, write timeouts
are the detection mechanism.)

| Scenario | Bound |
|----------|-------|
| Silent dead link, nothing outstanding | ≈ `threshold × (interval + T6)` after its last sign of life |
| Wedged mid-transaction | caller gets the T3 reply timeout at ≈ T3, then probing resumes: total ≈ `T3 + threshold × (interval + T6)` |
| Rare probe/frame races | up to one extra probe cycle (`interval + T6`) |
| Immediately after a reconnect | worst case ≈ `2 × threshold × (interval + T6)` (a late frame from the torn-down session can restart the failure run once) |

The practical tuning rule: **T3 is your "step in after N minutes" knob.** Size it for the
slowest command your equipment legitimately runs; the linktest machinery covers the idle case.

## Observability

Two counters on `hsmsss.ConnectionMetrics` (via `Connection.ControlMetrics()`) expose what
suppression is doing:

- `LinktestSuppressedCount()` — probe timer fires skipped because the line was active or a
  reply was outstanding. A steadily climbing value on a busy connection is expected and healthy.
- `LinktestCreditedCount()` — probe failures that were *not* counted toward the disconnect
  threshold because the link showed life. `LinktestErrCount()` still counts every failure.

**Dashboard note (behavior change vs v2.0.x):** on busy links `LinktestSendCount` /
`LinktestRecvCount` stop climbing (probes are skipped), and `LinktestErrCount` can grow without
any disconnect (credited failures). Alerts built on the old always-probing cadence need to
account for this — or disable suppression on that connection.

## Choosing the failure threshold

Use `WithLinktestFailThreshold(2)` or higher (the default is 3). At threshold ≥ 2 no single
race can disconnect a link whose life signals are observed by a failure evaluation (including
the final pre-disconnect re-check) — the next evaluation sees the life signal and restarts the
failure run. At threshold 1, any single *counted* probe timeout disconnects, with or without
suppression, exactly as in v2.0.x.

## Trade-offs and limitations

- **Slower dead-link detection during a long wait.** If the cable is pulled while a reply is
  outstanding, you find out at T3 (plus the resumed-probe window) instead of `interval + T6`.
  This is the feature's purpose — the wait is bounded by a knob you already tune per equipment.
- **Continuous fire-and-forget traffic starves the probe.** An application that streams
  no-reply messages nonstop keeps the line active with nothing inflight, so the linktest never
  runs; an application-level zombie peer is then caught only by write timeouts
  (`WithWriteTimeout`). Relay and forwarding workloads should set
  `WithLinktestSuppression(false)`.
- **Threshold 1 gets no race protection** (see above). Prefer ≥ 2 wherever suppression matters.
- **Small accepted race windows.** The probe decision and the wire write are not atomically
  linked, so a frame racing the probe can add one probe cycle of detection delay, and life
  arriving in the final instants of the disconnect decision can still be missed. These windows
  are bounded and documented precisely in the `hsms.WithLinktestSuppression` godoc. At
  threshold ≥ 2, no single race can disconnect a link whose life signals are *observed* by a
  failure evaluation (including the final pre-disconnect re-check) — the observation condition
  is the guarantee's boundary, not a formality.
- **HSMS-SS only.** SECS-I (SEMI E4) has no linktest, so the `secs1` package is unaffected.

## Relationship to v1

v1's `WithAutoLinktest` was the master on/off switch for the auto-linktest; its v2 equivalent
for disabling probes entirely is `WithLinktestInterval(0)`. v1 also suppressed the linktest
timer on send/receive activity — that behavior is what v2.1.0 restores and extends (v1 had no
inflight skip and no liveness credit, so it still probed into a long silent wait). See the
[migration guide](../migration-v1-to-v2.md) for the mapping table.
