# Log

## 2026-09-26
* **Update**: [Where a TransitionCause is chosen, and why one transition can swallow another's cause](/hsms/transition-cause-injection-sites.md) — confirmation-pass correction:
  the shipped transports name a cause through the optional `TCPDownWithCause` capability; a runtime without it reports `CauseUnknown`.
  Promoted to stable.
* **Update**: [Reconnect backoff scope — what resets it, and what doesn't](/hsms/reconnect-backoff-scope.md) — confirmation pass found no fault.
  Promoted to stable.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) — verify-pass corrections:
  the `hsms/epoch.go` source revision now matches its digest;
  an absorbed Close still tears down via `closeEpoch`, only an absorbed involuntary drop loses teardown and reconnect;
  a resurrected NotSelected loses only the successor's TCP-up CAS and report, not necessarily its Select;
  shared-core write failures are also generation-named for `secs1`;
  a stale Select.rsp can commit the successor even when the waiting caller returns cancellation.
  Still draft.
* **Update**: [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md) — verify-pass corrections:
  the `hsms/epoch.go` source revision now matches its digest;
  `prev.wait()` signals teardown completion, not a full join;
  loop lifetimes can overlap even with shipped transports, only the retry/publication sequences are serialized;
  secs1's passive commits can land before or after `Start` returns;
  the reset-check coverage claim now cites the tests rather than a mutation run.
  Still draft.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) — `cc82a06`:
  a real drop into NotConnected now fires even when `lastReacted` already reads NotConnected, reporting the state the `Swap` replaced;
  the coalesced bring-up now fires on the `evTCPUp` with `CauseSelectAccepted` and the later `evSelectAccepted` is the no-op;
  a late `evTCPUp` that finds NotConnected is ignored; records how `react`'s farewell, reconnect, and teardown hang off the reported `prev`.
  Demoted to draft.
* **Update**: [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md) — `cc82a06`: the disconnect's store is now a `Swap` and the drop reports `prev == Selected`,
  though still no entering-Selected reaction fires. Demoted to draft.
* **Update**: digest-only refresh to `cc82a06` for [selected gates](/hsms/selected-gates.md), [E37.1 narrows E37 generic](/hsmsss/e37-1-narrows-e37-generic.md),
  [supervisor join](/hsms/supervisor-join.md), [callback panic/Goexit isolation](/hsms/handler-panic-goexit-isolation.md),
  [inbound generation fence](/hsms/inbound-generation-fence.md), [Close interrupts a blocked Open](/hsms/open-close-abort.md),
  and [block send detect/act split](/secs1/block-send-detect-act-split.md).

## 2026-09-25
* **Update**: [How Close interrupts a blocked Open](/hsms/open-close-abort.md) — confirmation-pass correction:
  the opening summary no longer says a concurrent Close always makes Open return `ErrConnClosed`.
  Promoted to stable.
* **Update**: [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) — confirmation-pass correction:
  a Close from any goroutine can time out on a blocked callback; only a callback's own-cycle Close joins itself.
  Promoted to stable.
* **Update**: [How Close interrupts a blocked Open](/hsms/open-close-abort.md) — verify-pass corrections:
  a pre-fired token's Selected wait can still return nil or `ctx.Err()`, and pre-firing does not make Open nonblocking;
  registration ordering and the positive-count invariant allow a descheduled Close;
  reconnect dials can run before Open returns;
  Start-error mapping requires `callerCut`;
  the bound's cancel runs at `startActive` exit and joins neither bridge;
  the interrupting Close returns the cached rollback result only while that supervisor is current.
  Still draft.
* **Update**: [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) — verify-pass corrections:
  `mapAbortedStartErr` credits the caller only on `callerCut` plus `context.Canceled`;
  the cached rollback result applies only while that supervisor stays current;
  the callback-Close timeout covers only a callback closing its own current cycle,
  and the notifier may be abandoned rather than joined before the reconnect-loop join.
  Still draft.
* **Creation**: [How Close interrupts a blocked Open](/hsms/open-close-abort.md) — the pending-close registration, one-shot abort token, first-dial bound, and Start-error mapping introduced in `c00e1b5`; draft.
* **Update**: [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) — Open's rollback now also covers an aborted or caller-cancelled first dial and returns the mapped Start error, not the raw one; the interrupting Close returns the rollback's `shutdownErr`. Demoted to draft.
* **Update**: digest-only refresh to `c00e1b5` for [transition-cause injection sites](/hsms/transition-cause-injection-sites.md), [reconnect backoff scope](/hsms/reconnect-backoff-scope.md),
  [send error accounting](/hsms/send-error-accounting.md), [inflight gauge](/hsms/inflight-gauge.md), [inbound generation fence](/hsms/inbound-generation-fence.md),
  [callback panic/Goexit isolation](/hsms/handler-panic-goexit-isolation.md), [transaction observer chokepoints](/hsms/transaction-observer-chokepoints.md),
  [passive refusal exchange](/hsmsss/passive-refusal-exchange.md), [activity stamps](/hsmsss/activity-stamps.md), [linktest teardown exemption](/hsmsss/linktest-teardown-exemption.md),
  [E37.1 narrows E37 generic](/hsmsss/e37-1-narrows-e37-generic.md), and [block send detect/act split](/secs1/block-send-detect-act-split.md)
  (Close/Open abort protocol, dial bounding, accept-retry diagnostic, and caller-obligation docs changed; no cited claim touched).
* **Update**: [callback panic/Goexit isolation](/hsms/handler-panic-goexit-isolation.md), [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md),
  [the inbound generation fence](/hsms/inbound-generation-fence.md), and [supervisor join](/hsms/supervisor-join.md) promoted to stable against `d244104`
  after a verify pass (openai/gpt-6-astra) and confirmation passes (openai/gpt-5.6-terra) that corrected Goexit-resumption semantics, reporter-exit cases,
  identity-versus-liveness checks, and notifier-side Goexit; supervisor join now also cites `hsms/endpoint.go`.
* **Update**: digest-only refresh to `752a39a` for entries citing `hsms/connection_config.go` or `secs1/config.go`
  (keep-alive and linktest option docs changed; no cited claim touched).
* **Update** (independent verify pass against `d244104`): [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) —
  scoped the "far more likely"/"before any timeout is at stake" promptness claim: `notifierDone` only closes once Goexit unwinding
  reaches `notifier()`'s own completion defer, and the reporting chain in between (`reportGoexit`, a user-supplied logger,
  `disconnectHandlerGeneration`'s possibly-blocking `injectFrom` enqueue) can still delay it past the close deadline; added
  `hsms/handler_panic.go` (`runCallback`) as a cited source, which the body already named but the frontmatter did not.
  `verified` still absent, status draft.
* **Update** (independent verify pass against `d244104`): [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) —
  dropped the "CauseHandlerExit is the one injection site inside hsms" framing (the entry's own table already lists several
  hsms-internal sites: Close, Open rollback, writeFrame, T7Expired, TCPUp, CommitSelected, SelectLost); corrected "the SAME
  generation-named entry point hsmsss/secs1 use" — secs1 does not name a generation for its own disconnects; corrected "not a
  separate check disconnectHandlerGeneration invents" — it explicitly rejects gen==0 itself, before injectDisconnect/step's
  identity-only match ever runs; corrected "a gen of 0 skips the match everywhere" — selectCommitGate still liveness-checks a
  gen of 0, and disconnectHandlerGeneration rejects it outright rather than treating it as a wildcard; added the missing
  `hsmsss/integration_lifecycle_cause_test.go` source for `(*causeLog).waitBringUp`.
  `verified` dropped, status draft.
* **Update** (independent verify pass against `d244104`): [The inbound generation fence](/hsms/inbound-generation-fence.md) —
  corrected the claim that a Goexit-forced handler disconnect is "a no-op ... the same generation-identity match cutoff #1
  and #2 already rely on" — cutoff #1 (`liveEpoch`) checks identity AND `!ended`, cutoff #2 checks epoch cancellation, and
  `disconnectHandlerGeneration`/`injectDisconnect` check identity only (zero rejected separately), three different conditions;
  corrected "SECS-I (gen == 0) never reaches liveEpoch ... explicit, permanent bypass" — SECS-I's assembler is wired directly
  to the plain `DeliverOwnedFrame` and never calls `DeliverOwnedFrameFromGeneration` at all (so it never takes that function's
  own `if gen == 0` branch), while a stamped SECS-I reply's nonzero `originGen` still reaches `liveEpoch` through
  `SendAsyncFromGeneration` on the outbound leg.
  `verified` dropped, status draft.
* **Update** (independent verify pass against `d244104`): [How a user callback's panic or Goexit is contained](/hsms/handler-panic-goexit-isolation.md) —
  corrected the Goexit-resumption description against the actual runtime source (`runtime.recovery`'s `if p.goexit` branch):
  recovery resumes the pending Goexit loop itself, not the recovering frame, and can revisit already-processed stack frames
  while never re-invoking an already-consumed defer; scoped the "runCallback itself never panics"/"only a panic during Goexit
  reports both" invariants to reporters (`onPanic`/`onGoexit`) that themselves return normally — a panicking reporter escapes
  uncaught, and a Goexiting reporter also trips the outer detector; fixed the disconnect-before-log claim — a disconnect action
  only precedes the site's own log call for a MATCHING generation, a stale-generation report never disconnects at all (pure
  Debug log); reframed the five call sites as three execution roles rather than three fixed goroutines, and noted
  `callDataHandler`/`callDecodeErrorHandler` call the handler unrecovered when `s.rt` does not satisfy `handlerHost`; scoped
  the recv-goroutine-join claim — `g.recv.Done()` fires only after every intervening defer, and `tr.Stop`'s bounded join also
  sequentially waits on `g.proc`/`g.linktest`/`g.t7`, not `g.recv` alone; fixed the "no log line" failure-mode overclaim — the
  one-frame version's surviving `recover()` still reports the nested panic, only the Goexit-specific report and its disconnect
  are lost, and differently by site (receiver lost vs. sender lost); corrected "deliberately NOT recovered" for the
  transaction observer — `WriteMessage`/`WriteMessageNoReply` give it no dedicated recovery boundary, but a synchronous send
  made from inside an already-isolated handler has the observer's panic caught incidentally by that handler's own
  `runCallback`; added `hsmsss/transport.go` (`(*transport).Stop`) as a cited source.
  `verified` still absent, status draft.
* **Creation**: [How a user callback's panic or Goexit is contained](/hsms/handler-panic-goexit-isolation.md) —
  `runCallback`'s two-frame detection (why one frame drops `onGoexit` for a nested panic that interrupts a Goexit,
  pinned by `TestRunCallback_GoexitWithDeferredPanic`), the five call sites across three goroutines and what each
  does on a Goexit, the disconnect-before-log ordering and its logger guard, and what stays outside this mechanism
  (the transaction observer, `WithDialer`/`WithListener`, a closed registered channel, `hsmstest.FakeEndpoint`).
  Sourced against `d244104`; status draft.
* **Update** (sync against `d244104`, `feat(hsms): recover callback panics and detect Goexit`):
  [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) — added the new
  `CauseHandlerExit` injection site (a user callback's `runtime.Goexit` disconnecting the generation it ran for,
  via `disconnectHandlerGeneration` → `TCPDownFromGeneration`), the one injection site that originates inside
  `hsms` rather than a transport goroutine, and the fact that a `secs1`-hosted handler still resolves a nonzero
  generation despite `secs1`'s own disconnects never naming one.
  Digests refreshed; `verified` dropped; status draft.
* **Update** (sync against `d244104`): [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md) —
  noted that `callHandler`/`callSub` now route a `StateChangeHandler`/subscriber callback through `runCallback`
  (counted+logged panic, logged-only Goexit) instead of a bare silent `recover()`, why a Goexit-driven
  `notifierDone` close is what usually lets `Close` "still complete" rather than the close timeout, and the
  hook-install-before-`go` ordering that lets `callHandler`/`callSub` read `reportPanic`/`reportGoexit`
  lock-free.
  Digests refreshed; `verified` dropped; status draft.
* **Update** (sync against `d244104`): [The inbound generation fence](/hsms/inbound-generation-fence.md) —
  noted that cutoff #2 now threads `e`'s resolved generation one parameter deeper, into
  `callDataHandler`/`callDecodeErrorHandler`'s panic/Goexit isolation, and that `callDataHandler` previously ran
  a `DataMessageHandler` with no recover at all (a panic there used to crash the process). Digests refreshed;
  `verified` dropped; status draft.
* **Update** (sync against `d244104`, cosmetic — digest refresh only, no mechanic cited by these entries changed):
  [The B1/B2 Selected gates](/hsms/selected-gates.md), [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md),
  [Linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md),
  [The transaction observer's two chokepoints](/hsms/transaction-observer-chokepoints.md),
  [The W-bit inflight gauge](/hsms/inflight-gauge.md), [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md),
  [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md), [The MaxMessageSize ceiling](/hsms/max-message-size-ceiling.md),
  [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md),
  [Send error accounting](/hsms/send-error-accounting.md), [Activity stamps](/hsmsss/activity-stamps.md), and
  [The block-send transaction's detect/act split](/secs1/block-send-detect-act-split.md) — `hsms/connection_send.go`
  changed only inside `callAsyncSendErrorHandler`'s Goexit handling, and `hsms/connection_lifecycle.go`,
  `supervisor.go`, `connection_config.go`, `connection_metrics.go`, `state.go`, `lifecycle.go`, and `secs1/line.go`,
  `secs1/transport.go` changed only in regions none of these entries' claims rest on; `status: stable` and `verified` kept.
* **Update**: [Send error accounting](/hsms/send-error-accounting.md) and [linktest teardown exemption](/hsmsss/linktest-teardown-exemption.md)
  promoted to stable against `a1cdb0e` after a verify pass (openai/gpt-6-astra) and confirmation passes (openai/gpt-5.6-terra)
  that corrected the custom-connection caveat, the reply/caller-cancellation exceptions, the W-bit-independent counting guard,
  and scoped the `ErrConnClosed` origin list to the synchronous request path; the final confirmation found no fault.
* **Update** (independent verify pass against `a1cdb0e`): [Send error accounting](/hsms/send-error-accounting.md) —
  scoped the "a control T6 expiry is counted nowhere" claim to `DataMsgErrCount` and cross-referenced that an auto-linktest T6 failure still increments hsmsss's `LinktestErrCount`;
  fixed the "never reaches the wire" overstatement for a failed !W write (a deadline can truncate a frame mid-writev, so a write can fail after partial transmission);
  replaced the "Both T3 and T6 return their timeout error" oversimplification with the accurate priority-recheck description (reply, then teardown, then the genuine timeout);
  dropped the unverifiable "teeth-check ... reverting restored a green suite" execution-history claim, keeping only what `hsms/connection_send_metrics_test.go` itself shows, now added as a cited source.
  `verified` still absent, status draft.
* **Update** (independent verify pass against `a1cdb0e`): [Linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md) —
  scoped the "no code path returns `ErrConnClosed` for a live link" guarantee to CORE-generated errors, noting a custom `WithDialer` `net.Conn` that itself returns or wraps a value matching `hsms.ErrConnClosed` would defeat it (`hsmsss/transport.go`'s `Write` passes such an error through unexamined);
  clarified that a matching `ErrConnClosed` on auto-linktest's own path is a lifecycle classification, not proof the underlying write failure was not ALSO a genuine live-link problem racing with teardown.
  status draft.
* **Update** (sync against `a1cdb0e`): [Send error accounting](/hsms/send-error-accounting.md) round 5 —
  `writeFrame` now checks `e.ctx.Err()` after a write fails and classifies a post-teardown failure as `ErrConnClosed` (wrapping the raw error, or passing one through) before `isCountedSendErr` ever sees it, reversing rounds 1-4's "a teardown-interrupted write still counts" finding by fixing the race those rounds only documented.
  `sendWaitReplyOn`'s timer and teardown arms also gained a priority re-check (`tryReply`, then — timer arm only — `e.ctx.Err()`): reply beats teardown beats a genuine T3/T6 timeout, among those three arms only (`callerCtx.Done()` has no re-check).
  Corrected the frontmatter `description` (a concurrent Close is now effectively a blanket exclusion for a sync data send) and the stale "unwrapped" claim about `writeFrame`'s own error handling.
  `verified` dropped, status draft.
* **Update** (sync against `a1cdb0e`): [Linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md) adds the two new `ErrConnClosed` origins `a1cdb0e` introduced — `writeFrame`'s post-write teardown reclassification, and `sendWaitReplyOn`'s timer-arm `e.ctx.Err()` re-check — to the "every origin of the sentinel" trace, which the diff made incomplete.
  Both new origins are teardown-exclusive by the same `e.ctx.Err() != nil` construction as the existing four, so the guard's safety argument is unchanged, only its origin count.
  `verified` dropped, status draft.
* Digest/revision-only refresh to `a1cdb0e` (prose unaffected — the cited symbols each entry names were untouched by the `a1cdb0e` diff, judged cosmetic against a hint list that expected some of these material):
  [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md),
  [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md),
  [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md),
  [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md),
  [The inbound generation fence](/hsms/inbound-generation-fence.md),
  [The W-bit inflight gauge](/hsms/inflight-gauge.md),
  [The send-side MaxMessageSize ceiling's enforcement topology](/hsms/max-message-size-ceiling.md),
  [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md),
  [The transaction observer's two chokepoints](/hsms/transaction-observer-chokepoints.md) — already stated classification-by-error-value before `a1cdb0e`'s doc comment caught up to it, not the other way around,
  [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) — `writeFrame`'s `TCPDownFromGeneration(CauseIOError)` call was already gated on a live (non-cancelled) generation before this commit, so the injection-site table is unaffected.
* **Creation**: [The inbound generation fence](/hsms/inbound-generation-fence.md) records the R10 inbound-fence mechanic `6c257b6` introduced:
  the two distinct cutoffs (one-shot `liveEpoch` admission under `genGate`, vs. the ongoing per-handler `epochDone`/`e.ctx.Done()` recheck in fan-out),
  the window between them and the three test hooks (`testHookAfterReadFrame`, `testHookBeforeFanout`, `testHookBeforeS9F1Enqueue`) that mark it,
  that `lastRecvStamp` is written before either cutoff runs,
  the `connIdentity` reply-origin token and its retention rationale,
  and the SECS-I asymmetry (origin stamp yes, admission cutoff no).
  Notes `staleRecv` has no `ConnectionMetrics` accessor — Debug log only.
  Unit hsms; born draft.
* **Update**: [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) resyncs to `6c257b6`:
  the `hsmsss.recvLoop nil conn / read error` injection site is now `read error` only (the nil-conn branch was deleted — `recvLoop` now takes its socket as a spawn-time parameter);
  the Select-lost section is rewritten — `stopLinktest`/`armT7` are no longer transport-wide, they now touch only the caller's `genWG` bundle (R10 / D4), so a refused Select-lost commit merely skips needless work rather than fencing a straggler from a live successor's timers;
  `genRuntime`/`genCapability` now name ten methods (not eight) after `DeliverOwnedFrameFromGeneration`/`RouteReplyFromGeneration` were added;
  `internal/gencap/gencap.go`'s recorded revision is corrected (it postdates `020da48`, which predates the file's creation);
  adds a trail to the new inbound-generation-fence entry.
  `verified` dropped, status draft.
* **Update**: [Activity stamps](/hsmsss/activity-stamps.md) corrects the FALSE invariant flagged during triage:
  since `6c257b6`, `recvLoop` takes its socket as a spawn-time parameter rather than re-reading `t.conn`,
  so a straggler abandoned by a bounded `Stop` can stamp at most once after its own generation ends —
  the `transport.go` field comment this entry previously said not to trust is now accurate, and the entry is corrected to agree with it.
  Renames `t.genCtx` to `g.ctx` (`genWG.ctx`) throughout, matching the R10 rework that moved the ctx off the transport and onto the per-generation bundle;
  updates the `Where to look` pointer for `g.ctx`'s origin to name `startActive`/`startPassive` rather than `(*transport).Start`, which no longer stores it.
  `verified` dropped, status draft.
* **Update**: [Linktest teardown exclusion](/hsmsss/linktest-teardown-exemption.md) renames `t.genCtx` to `g.ctx` (`genWG.ctx`) throughout — the field moved off the transport onto the per-generation bundle in `6c257b6` — and repoints the `Where to look` entry for its origin to `startActive`/`startPassive`.
  The described mechanism (the two-part `ErrConnClosed` guard, the one-context-tree argument) is unchanged; `verified` dropped, status draft pending re-verification of the rename.
* **Update**: [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) repoints the field-less-Reject-result citation from `RouteReply` to `routeReplyOn`, the function the reject-handling logic actually lives in after `6c257b6` split `RouteReply` into a thin wrapper over `routeReplyOn`/`RouteReplyFromGeneration`.
  The two-legged gate mechanism itself is unchanged; `verified` dropped, status draft.
* **Update**: [The transaction observer's two chokepoints](/hsms/transaction-observer-chokepoints.md) corrects the "`ReplyDataMessage` is built on `rt.SendAsync`" claim:
  since `6c257b6` it is `rt.SendAsync` OR `rt.SendAsyncFromGeneration` (when the primary was admitted by this connection's still-live generation), both equally enqueue-only and equally excluded from the two `TxEvent` chokepoints.
  `verified` dropped, status draft.
* **Update**: [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) corrects the `handleSeparateReq` section:
  `genCtx` is no longer threaded as a separate parameter alongside `g` — `recvLoop` stamps `g.ctx` once (from `genWG.ctx`) and `dispatchFrame` passes `g.ctx` straight through.
  `verified` dropped, status draft.
* Digest/revision-only refresh to `6c257b6` (prose unaffected — cited symbols untouched by the R10 diff): [Send error accounting](/hsms/send-error-accounting.md), [The W-bit inflight gauge](/hsms/inflight-gauge.md), [Trailing-byte counting](/crosscutting/trailing-bytes-counting.md), [How a shutdown joins the per-Open supervisor](/hsms/supervisor-join.md), [Reconnect backoff scope](/hsms/reconnect-backoff-scope.md), [The passive refusal exchange](/hsmsss/passive-refusal-exchange.md).
* **Update** (independent verify pass against `6c257b6`): [The inbound generation fence](/hsms/inbound-generation-fence.md) corrected four claims:
  the origin stamp is set only when the resolved epoch is non-nil,
  not unconditionally;
  fan-out's per-step cancellation check does not deterministically stop every later handler/channel send
  (a channel send can win its select against concurrent cancellation);
  selecting on `e.done` stalls until the bounded teardown join times out,
  rather than deadlocking outright;
  and a stale-admission drop has no exported counter of its own,
  but a stale Reject.req already bumps the public `RejectRecvCount` before the refusal runs.
  Digests and revision stay at `6c257b6`;
  `verified` not touched (never set); status remains draft.
* **Update** (independent verify pass against `6c257b6`): [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md) corrected four claims:
  `Stop` releases `startGate` before socket closure and every join,
  so it never holds that lock during a join `step` could be waiting behind;
  a live current epoch CAN be `NotConnected` during `Open`'s own startup window,
  so identity+state cannot replace the `ended` check for `CommitConnected` either;
  a refused Select-lost commit leaves state unchanged,
  which may be `NotConnected`, `NotSelected`, or `Selected` depending on why it was refused, not always Selected;
  and gen-0 Select-lost bypasses `commitGate` for a bare CAS,
  never reaching `selectCommitGate` (which applies only to Select-accepted).
  Digests and revision stay at `6c257b6`; status remains draft.
* **Update** (independent verify pass against `6c257b6`): [The reply registry's two-legged control exemption](/hsms/reply-matching-control-exemption.md) fixed an internal contradiction:
  the "removing the DATA-registration `*ControlMessage`-miss gate alone is undetected by every test above" claim directly conflicted with the same bullet's own "exercised by ..." list;
  reworded to name the two control-round-trip tests that miss it
  and the data-registration/control-response tests that catch it,
  and to note a genuine reply can be consumed or discarded by the still-registered waiter (not only become unsolicited),
  depending on whether it arrives before or after the sender's deferred deregistration.
  Digests and revision stay at `6c257b6`; status remains draft.
* **Update** (independent verify pass against `6c257b6`): [The transaction observer's two chokepoints](/hsms/transaction-observer-chokepoints.md) corrected three claims:
  `ReplyDataMessage` selects the generation-bound send path by origin-token identity alone,
  not by a still-live-generation check —
  `SendAsyncFromGeneration` checks liveness itself
  and refuses a stale generation rather than falling back to unbound sending;
  deleting the `isData` gate only panics when a `txObserver` is installed,
  since both `WriteMessage`/`WriteMessageNoReply` return before the gate when `txObserver == nil`;
  and `classifyTxOutcome` does not check `replyWaited`,
  so a transport `Write` returning `ErrT3Timeout`/`*RejectError` directly can still produce `TxT3Timeout`/`TxRejected` through the no-reply path.
  Digests and revision stay at `6c257b6`; status remains draft.
* **Update** (independent verify pass against `6c257b6`): [Where the HSMS-SS profile overrides the generic core](/hsmsss/e37-1-narrows-e37-generic.md) corrected two claims:
  `g.ctx` is stamped once by `startActive`/`startPassive` before `recvLoop` is spawned,
  not by `recvLoop` itself, which only reads it;
  and the "only when a nondefault device ID was configured" / "endless reconnect loop" failure-mode framing held only for the Select/Separate call sites —
  the three Reject senders never read the configured device ID at all
  (their SessionID risk comes from the offending frame, independent of local config),
  and only a rejected Select handshake produces the reconnect-loop symptom.
  Digests and revision stay at `6c257b6`; status remains draft.
* **Update**: promoted to stable after an independent confirmation pass (openai/gpt-5.6-terra) against `6c257b6` found no fault:
  [The inbound generation fence](/hsms/inbound-generation-fence.md), [Activity stamps](/hsmsss/activity-stamps.md),
  [Where a TransitionCause is chosen](/hsms/transition-cause-injection-sites.md), [linktest teardown exemption](/hsmsss/linktest-teardown-exemption.md),
  [reply-matching control exemption](/hsms/reply-matching-control-exemption.md), [transaction-observer chokepoints](/hsms/transaction-observer-chokepoints.md),
  and [E37.1 narrows generic E37](/hsmsss/e37-1-narrows-e37-generic.md).
  Activity stamps and the linktest teardown exemption had also passed the preceding verify pass (openai/gpt-6-astra) unchanged.

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
* **Update**: [block-send detect/act split](/secs1/block-send-detect-act-split.md) and [assembler accept algorithm](/secs1/assembler-accept-algorithm.md) drop their notes on `docs/secs1/04-*` and `05-*`, superseded design docs now deleted; re-confirmed by openai/gpt-5.6-terra, stable.

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
