# Log

## 2026-09-24
* **Update**: [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) narrows the exemption:
  a DATA registration (`isData` true) answered by a `*ControlMessage` result is a miss —
  not delivered, registration not consumed, no mismatch recorded — instead of the old uncompared delivery,
  while a `*RejectError` result and a CONTROL registration's own uncompared delivery are unchanged;
  names the new `TestReplyRegistry_DataRegistration_ControlResultMustMiss`, `TestReplyMatching_DataPrimary_ControlResponseMustMiss`, and `TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes`.
  A follow-up pass corrects the leg-1/leg-2 sections to the three-gate shape:
  leg 1 (`isData`) now also gates the new `*ControlMessage`-miss check,
  so removing it alone breaks `TestReplyMatching_StrictMode_SelectReachesSelected` and `TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch` instead of going undetected;
  leg 2 (the `*DataMessage` assertion) now only screens the field-less `*RejectError` result,
  since a `*ControlMessage` never reaches it on a DATA registration.
  The same pass refreshes the `hsms/connection_send.go` digest.
  It re-reads the file and corrects the entry to attribute the `isData` type assertion to `sendWaitReplyOn` (called by `sendWaitReply`) rather than to `sendWaitReply` itself.
  `verified` stays dropped, status stays draft pending independent verification.
* **Creation**: [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) records the bounded notifier join introduced in `fb8d9cd`;
  draft pending independent verification.
* **Creation**: [Reconnect backoff scope — what resets it, and what doesn't](/hsms/reconnect-backoff-scope.md) records where `connection.reconnectDelay` is persisted across separate `connectLoop` invocations;
  the `epoch.reachedSelected` marker set atomically with the Select CAS under `genGate` and read after `prev.wait()`;
  why the reset is not keyed off `react`'s deduped reaction;
  the gen-0 liveness gate `selectCommitGate` now applies to the Select commit;
  config sampling; and passive re-listen damping.
  Notes the pre-existing overlapping-reconnect-loops residual (unreachable with shipped transports) as a Gotcha;
  born draft.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) now records that the Select-accepted commit gates a gen of 0 too, through a sibling `connection.selectCommitGate` rather than `commitGate`;
  the latter still bypasses gen 0 for the TCP-up and Select-lost commits.
  Also records that a successful commit latches `epoch.reachedSelected`.
  Refreshes digests for `hsms/supervisor.go`, `hsms/connection_lifecycle.go`, `hsms/connection_runtime.go`, `hsms/connection.go`, and `hsms/epoch.go`;
  `verified` stays dropped, entry remains draft.
* **Update**: [Reconnect backoff scope — what resets it, and what doesn't](/hsms/reconnect-backoff-scope.md)
  paraphrases the `WithReconnectBackoff` opener instead of quoting text the godoc no longer carries;
  corrects the missed-reaction window, which had the Select commit's CAS and `step`'s own load/store backwards
  (the commit lands between `step`'s load and its plain Store, not the other way around);
  notes the one-loop-at-a-time invariant now excepts the overlap already described under Gotchas;
  rewraps the whole entry with semantic linefeeds.
  Refreshes digests for `hsms/connection.go`, `hsms/connection_lifecycle.go`, `hsms/connection_config.go`,
  `hsms/connection_runtime.go`, `hsms/supervisor.go`, and `hsms/epoch.go`;
  `verified` stays dropped, entry remains draft.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md)
  corrects "where it used to always succeed" to "where it used to take the bare CAS":
  before `selectCommitGate` existed, a gen-0 `CommitSelected` was never gated at all, not merely "always successful".
  Refreshes digests for `hsms/supervisor.go`, `hsms/connection_lifecycle.go`, `hsms/connection_runtime.go`,
  `hsms/connection.go`, `hsms/epoch.go`, and `secs1/transport.go` (revision advanced to `a9235b4`);
  `verified` stays dropped, entry remains draft.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md)
  qualifies the `runSelectProcedure` non-zero-select-status row:
  a status-1 answer is this cause's genuine refusal only while this generation's `genWG.selectedOnce` latch reads false;
  a status-1 answer while the latch reads true is the legitimate simultaneous-select case and drives no transition at all.
  Adds a `genWG.selectedOnce` pointer under Where to look.
  Refreshes digests for `hsmsss/transport.go`, `hsmsss/transport_control.go`, `hsmsss/transport_active.go`, and `hsmsss/transport_recv.go`
  (revision advanced to `b0c8081`);
  entry remains draft.
* **Update**: [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md) — `revision:` labels only, advanced to `b0c8081`;
  digests were already current.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) — no content change;
  a follow-up pass tightened the `runSelectProcedure`/`genWG.selectedOnce` comments the prior entry above already describes
  (a compact status-1 rewrite, the race-free store-before-read argument on the field comment, and matching test comments),
  which moved the digests of `hsmsss/transport.go`, `hsmsss/transport_active.go`, and `hsmsss/transport_recv.go`;
  `hsmsss/transport_control.go` was unchanged.
  Revision stays `b0c8081`; entry remains draft.
* **Update** (sync against `922feb8`): [Activity stamps](/hsmsss/activity-stamps.md) corrects the `resetActivityStamps` ordering —
  `startActive` and `acceptLoop` now call the generation-gated `t.tcpUp` before the `connMu` section that resets the stamps; `verified` dropped, status draft.
* **Update** (sync): [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) rewrites the `handleSeparateReq` deviation:
  the generation-identity barrier checked at FSM-apply time now fences it; `verified` dropped, status draft.
* **Update** (sync): [LinktestErrCount's teardown exclusion](/hsmsss/linktest-teardown-exemption.md) adds a fourth `ErrConnClosed` origin,
  `WriteMessageFromGeneration`'s stale-generation refusal, reached through `runLinktest`'s `t.writeMessage`; `verified` dropped, status draft.
* **Update** (sync): [The W-bit inflight gauge](/hsms/inflight-gauge.md), [Send error accounting](/hsms/send-error-accounting.md), and [The B1/B2 Selected gates](/hsms/selected-gates.md)
  repoint to `sendWaitReplyOn` and `enqueueAsync`, shared with the generation-bound `WriteMessageFromGeneration` / `SendAsyncFromGeneration`; `verified` dropped, status draft.
* **Update** (sync): [The transaction observer's two chokepoints](/hsms/transaction-observer-chokepoints.md) records that hsmsss Select.req / Linktest.req now reach the core through `WriteMessageFromGeneration` / `SendAsyncFromGeneration`, bypassing `WriteMessage`'s `isData` gate,
  corrects the gate-deletion failure mode, and cites `hsmsss/transport_control.go`; still draft.
* **Update** (sync, digests only): [passive refusal exchange](/hsmsss/passive-refusal-exchange.md), [reply-matching control exemption](/hsms/reply-matching-control-exemption.md), [supervisor join](/hsms/supervisor-join.md), [transition-cause injection sites](/hsms/transition-cause-injection-sites.md),
  [MaxMessageSize ceiling](/hsms/max-message-size-ceiling.md), [stale-epoch write guard](/hsms/stale-epoch-write-guard.md), [trailing-byte counting](/crosscutting/trailing-bytes-counting.md), and [decode aliasing](/secs2/decode-aliasing.md) — cited symbols unchanged.
* **Update** (verify): all eleven Claude-authored drafts checked claim by claim against source by an independent model (openai/gpt-6-astra), corrected where wrong, re-confirmed (openai/gpt-5.6-terra), and promoted to stable:
  [inflight gauge](/hsms/inflight-gauge.md), [Selected gates](/hsms/selected-gates.md), [send error accounting](/hsms/send-error-accounting.md), [transaction observer chokepoints](/hsms/transaction-observer-chokepoints.md),
  [reconnect backoff scope](/hsms/reconnect-backoff-scope.md), [reply-matching control exemption](/hsms/reply-matching-control-exemption.md), [supervisor join](/hsms/supervisor-join.md), [transition-cause injection sites](/hsms/transition-cause-injection-sites.md),
  [activity stamps](/hsmsss/activity-stamps.md), [E37.1 profile overrides](/hsmsss/e37-1-narrows-e37-generic.md), and [linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md) (retitled: one context tree, not two cascades).
  Corrections removed overclaims (B2 closes TOCTOU; Close never counts; stamps never move backward; one-straggler bound) and fixed ordering, ancestry, and pointer errors.
* **Update** (verify): [Parser input bounds](/sml/prealloc-bound.md) verified by claude/opus-5.5; one byte count corrected (the example message is 28 bytes); promoted to stable.
* **Creation**: [The block-send transaction's detect/act split and RTY counting](/secs1/block-send-detect-act-split.md) — the sendBlockOnce/sendBlock detect-vs-act split and anti-starvation retry counting on a failed contention yield;
  first entries in a new `secs1` unit.
* **Creation**: [The inbound assembler's accept order, lazy T4 check, and frame-ownership handoff](/secs1/assembler-accept-algorithm.md) — the fixed five-step per-block check order and T4 as a lazy next-arrival gap check rather than a timer;
  the owned-buffer handoff to `rt.DeliverOwnedFrame`.
* **Update** (resync against `a7ff4a8`): [Send error accounting](/hsms/send-error-accounting.md), [Linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md), and [Transaction observer chokepoints](/hsms/transaction-observer-chokepoints.md)
  record that the source comments they flagged (the Close-mid-transaction claim, runLinktest's "two cascades", ErrConnClosed's godoc, the observer test's rationale) were corrected in `ed4665e` / `a7ff4a8`; `verified` dropped, status draft pending confirmation.
* **Update** (resync, digests only): stale-epoch write guard, inflight gauge, Selected gates, MaxMessageSize ceiling, supervisor join, reconnect backoff scope, transition-cause injection sites, activity stamps, reply-matching control exemption, and E37.1 profile overrides —
  their cited files changed only in comments they do not discuss.
* **Update** (verify): the two secs1 drafts were checked against source by openai/gpt-6-astra and corrected — RTY bounds consecutive failures, not total attempts; `Write` can return `ErrConnClosed` on teardown; T4 can co-fire with a duplicate or invalid-first count; only the returned frame buffer is owned.
* **Update** (verify): the three resynced entries and the two secs1 entries were confirmed by openai/gpt-5.6-terra and promoted to stable; every entry in the bundle is now stable.

## 2026-09-23
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) now records that `genRuntime` and `genCapability` alias one shared `internal/gencap.GenerationRuntime` instantiation, and cites `internal/gencap/gencap.go`;
  `verified` dropped, entry remains draft.
  Digests for `hsms/connection_lifecycle.go` and `hsmsss/transport_control.go` were deliberately NOT refreshed: their drift since `0b40043` predates this change and was not judged.

## 2026-08-14
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) refreshed the `secs1/transport.go` digest and revision after a semantic-linefeed-only comment change;
  mechanic prose, generated metadata, and trust status remain unchanged.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) now cites `secs1/transport.go`, which its `causeRuntime` pointer already named;
  the body remains unchanged.
* **Update**: [Parser input bounds](/sml/prealloc-bound.md) refreshed the parser allocation-test digest and revision after strict ASCII coverage was strengthened;
  prose and generated metadata remain unchanged because the documented mechanic did not change.
* **Update**: [Parser input bounds](/sml/prealloc-bound.md) now records the 64-element initial-capacity ceiling that prevents unrelated trailing input from amplifying parser preallocation;
  digests and revisions were refreshed.
  The entry remains draft pending independent verification.

## 2026-08-13
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) resynced after the Select-failure classification fix (`5a0ec1b`): the active Select procedure is the one site whose cause is not a constant, because its reply registry can return a `*hsms.RejectError` (peer refused — not an I/O fault), a bare `ErrT6Timeout` (peer never answered), or a transport error; a correlated response of the wrong TYPE also reports `CauseSelectRejected`, with the refusal-versus-wrong-frame distinction kept in the error value rather than the cause.
* **Creation**: [Where a TransitionCause is chosen, and why one transition can swallow another's cause](/hsms/transition-cause-injection-sites.md) records the full injection-site to cause map behind `SubscribeLifecycle`; why the cause is data on `fsmCommand` rather than a function of the state pair; why the transports name it through a package-local `causeRuntime` capability instead of a widened `TransportRuntime`, so a bare `TCPDown` reports `CauseUnknown` rather than a guess; and the three ways a cause never reaches a subscriber (dedup on the state entered, drop-oldest coalescing, and the bring-up pair collapsing when the select commit outruns the TCP-up event); born draft.
* **Creation**: [The transaction observer's two chokepoints, its isData gate, and its outcome classifier](/hsms/transaction-observer-chokepoints.md) records why `WithTransactionObserver` instruments two chokepoints (`WriteMessage`/`WriteMessageNoReply`), not one; why `ReplyDataMessage` is structurally excluded (built on the async `SendAsync` enqueue primitive); the teeth-checked finding that deleting the `isData` gate crashes the whole process (nil-pointer panic on Select.req) rather than just leaking noisy events; and how `classifyTxOutcome`'s six-way split relates to `isCountedSendErr`'s binary one; born draft.

## 2026-08-12
* **Update**: [Parser input bounds](/sml/prealloc-bound.md) widened from allocation to input bounds generally: `parseList` now caps nesting at `secs2.MaxListDepth`, the decoder's own constant, closing a fatal (unrecoverable) stack overflow at ~2M levels and the asymmetry where the parser accepted messages its own wire decoder rejected; cites `secs2/decode.go` and the new nesting guard, digests refreshed, stays draft.
* **Creation**: [Parser input bounds](/sml/prealloc-bound.md) records both ways the SML parser keeps attacker text from driving oversized allocations — `capHint` clamping preallocation against the remaining input across the six slice parsers and strict-mode ASCII's `Builder.Grow`, and the strict-mode numeric token being sliced rather than accumulated (which was quadratic) — and notes unbounded list recursion as a separate open concern; born draft.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) independently re-verified after the material coverage-paragraph edit by `codex/gpt-5.6-terra`: `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget` pins the injected-write-error !W `sendWaitReply` case as DataMsgErr+1/DataMsgSend+0, the teeth-check description matches `75e907b`, every existing source claim still holds, and both cited digests match; promoted to stable.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) the flagged test-coverage gap is closed: `TestSendMetrics_DataMsgErr_OnWriteError_FireAndForget` (`hsms/connection_send_metrics_test.go`) now pins a write failure on a !W `sendWaitReply` send, teeth-checked by temporarily gating the increment on `!fireAndForget`; rewrote the gap paragraph to name the new test, demoted to `draft` (material edit, per FORMAT.md), cleared `verified` for an independent re-pass; sources untouched, digests already match, no re-digest needed.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) independently verified claim-by-claim by `codex/gpt-5.6-sol`; both source digests match, the !W write-failure accounting and T3-only short-circuit hold, and the synchronous !W write-failure test gap is confirmed — promoted to stable.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) round 4 corrector fix: rewrote `DataMsgErrCount`'s godoc (`hsms/connection_metrics.go`, dropping "reply-expected" and the false fire-and-forget exclusion, since a write failure on a !W `sendWaitReply` send DOES count and only the T3 branch is W-bit-exclusive) and rescoped `sendWaitReply`'s fire-and-forget short-circuit comment (`connection_send.go`) to the on-wire-success path it actually sits on, reworded the entry to match and re-digested both sources, kept `status: draft` for a separate actor to verify, and flagged — without adding — the missing test for a write failure on a !W send.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) round 3, independent verify by `claude/opus-5` CORRECTED and demoted to draft: the fire-and-forget invariant ("send+1, error+0") is false on the write-failure branch — `sendWaitReply`'s increment is guarded on `isData` alone, never on the W bit — and `DataMsgErrCount`'s godoc carries the same overstatement in its exclusion list, an imprecision round 2 missed while editing that very paragraph; `verified` cleared per FORMAT.md, sources untouched (both digests still match).
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) round 2: `DataMsgErrCount`'s godoc was corrected (`hsms/connection_metrics.go`) to name both `sendWaitReply` and `sendNoReply`, so the round-1 disagreement this entry recorded no longer holds — reworded that clause to reflect the fix, re-digested `connection_metrics.go`, verified every remaining claim against source, and promoted to stable.
* **Update**: [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md) round 2: verified every claim against `hsms/connection_send.go`, `hsms/decode.go`, and `internal/wire/body.go` — all held, digests already fresh, no prose changed — promoted to stable.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) verify pass CORRECTED: the "no package doc explains attribution" opener was false (`DataMsgErrCount`'s godoc lists four exclusions), and that godoc's "counts only sendWaitReply" is contradicted by `sendNoReply`'s own increment; stays draft.
* **Update**: [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md) verify pass CORRECTED: the rejection is not a sum over "already-materialized" buffers — `Buffers()` is what memoizes a tree body's encoding, so the first oversized send pays a full encode; stays draft.
* **Update**: [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) verified by `claude/opus-5` (same actor as `generated.by` — weakest tier) and promoted to stable; non-canonical `# How it fails` / `# Where it lives` headings renamed so the conformance passes stop skipping its pointers.
* **Update**: [Trailing-byte counting](/crosscutting/trailing-bytes-counting.md), [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md), [Activity stamps](/hsmsss/activity-stamps.md), [LinktestErrCount's teardown exclusion](/hsmsss/linktest-teardown-exemption.md), [The passive refusal exchange](/hsmsss/passive-refusal-exchange.md), [What a decoded item aliases](/secs2/decode-aliasing.md), and [Item slab carving and its retention model](/secs2/item-slab-retention.md) verified claim-by-claim against source by `claude/opus-5` and promoted to stable.
* **Update**: [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) the linktest failure reducer's trigger corrected — teardown-race `hsms.ErrConnClosed` is now also filtered, not only `ctx.Err()`; draft pending re-verification.
* **Update**: [Item slab carving and its retention model](/secs2/item-slab-retention.md) the slab-creation pointer repointed to `decodeFirstItem`, the new shared body `Decode`/`DecodeOwned`/`DecodeOwnedFrame` all delegate to; draft pending re-verification.
* **Update**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md), [The W-bit inflight gauge](/hsms/inflight-gauge.md), [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md), and [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) digests and revisions refreshed against v2.3.0 (`3660aa4`) — no mechanic described in any of these changed.
* **Update**: [What a decoded item aliases](/secs2/decode-aliasing.md) corrected the "in no package doc" claim — `decode.go`'s own doc comment already documents `DecodeOwnedFrame`'s capability-token access control, though `doc.go` itself still never names the function; draft pending re-verification.
* **Creation**: [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) the registration-side isData flag and result-side `*DataMessage` assertion, and the v2.0.1 field-bug shape breaking both reproduces.
* **Creation**: [The passive refusal exchange's absolute deadline and two-sided Stop handoff](/hsmsss/passive-refusal-exchange.md) why `readFrame` is forbidden, the `refuseToken` non-comparable-`net.Conn` reasoning, and the `ArmStart` generation reset.
* **Creation**: [LinktestErrCount's teardown exclusion is two independent cancellation cascades, not one](/hsmsss/linktest-teardown-exemption.md) traced every origin of `hsms.ErrConnClosed` in the send path to confirm the guard is teardown-exclusive; approved from the prior sync's Proposed list.
* **Creation**: [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md) the pre-`writeMu` check, the structural control-frame exemption, and why async accounting has no exclusion list at all; approved from the prior sync's Proposed list.
* **Creation**: [Trailing-byte counting — where it's measured and how it reaches TrailingBytes()](/crosscutting/trailing-bytes-counting.md) the shared `decodeFirstItem` site and hsms's copy-fallback routing through the same `DecodeOwnedFrame` entry point.

## 2026-08-05
* **Creation**: bundle bootstrapped at `bc97919` — seven unit skeletons, `hsms` / `hsmsss` / `secs2` seeded.
* **Deprecation**: the first seeding of `hsms` / `hsmsss` / `secs2` was replaced wholesale. Three broad entries (`linktest-suppression`, `send-path-reply-correlation`, `decode-ownership`) failed the novelty gate and the one-mechanic rule; two carried factual errors inherited from the doc comments they restated.
* **Creation**: [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) stamp state, per-generation reset, and the reducer's real trigger.
* **Creation**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) pre-write gate, write-boundary re-check, counted chokepoint.
* **Creation**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) lifecycle events excluded from the transaction error counter.
* **Creation**: [The W-bit inflight gauge](/hsms/inflight-gauge.md) increment discipline and its liveness coupling to `hsmsss`.
* **Creation**: [What a decoded item aliases](/secs2/decode-aliasing.md) per-leaf aliasing, the scalar/values split, the capability-gated entry point.
* **Creation**: [Item slab carving and its retention model](/secs2/item-slab-retention.md) chunk growth rule and the pointer-free property that bounds retention.
* **Creation**: [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md) synchronous-path counterpart to the doc's async stale-frame guarantee.
* **Update**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) corrected — four `dropNotSelected` call sites across three send entry points, not two. Draft pending independent verification.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) corrected — the error increment sits inside an `isData` check, so control T6 timeouts count nowhere. Draft pending independent verification.
* **Update**: [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) corrected — the per-generation reset is a rebaseline, not a fence; a straggler can stamp once after it. Draft pending independent verification.
* **Update**: [What a decoded item aliases](/secs2/decode-aliasing.md) cited `secs2/item.go` for the retention claim.
* **Update**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md), [Send error accounting](/hsms/send-error-accounting.md), and [Activity stamps](/hsmsss/activity-stamps.md) independently verified against source by a second model and promoted to stable.
* **Update**: recorded the independent verification of [The W-bit inflight gauge](/hsms/inflight-gauge.md), [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md), [What a decoded item aliases](/secs2/decode-aliasing.md), and [Item slab carving and its retention model](/secs2/item-slab-retention.md), which had happened but was missing from their frontmatter. Every entry now carries a verification by an actor other than its author.
- 2026-08-12 — **Update** ×6: refreshed hsms/connection_send.go digests (comment-only rescope of the fire-and-forget short-circuit note, 038319b) in inflight-gauge, reply-matching-control-exemption, selected-gates, stale-epoch-write-guard, max-message-size-ceiling (hsms) and linktest-teardown-exemption (hsmsss); prose untouched.
