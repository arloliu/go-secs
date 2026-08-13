package hsms

import (
	"context"
	"errors"
	"net"
	"time"
)

// farewellWriteTimeout bounds the courtesy farewell Separate write (§7.E). The farewell is a
// bounded best-effort courtesy: it acquires e.writeMu with TryLock (never blocks teardown) and
// writes under a short deadline. On any failure it is skipped and teardown proceeds — the
// unconditional closeSocket() in e.teardown is what guarantees progress (J5).
const farewellWriteTimeout = 500 * time.Millisecond

// selectPollInterval is the internal poll cadence used by OpenWaitSelected to observe the
// Selected state via the lock-free FSM atomic (a require.Eventually-style bounded wait — never
// a time.Sleep-poll). The wait also unblocks on ctx cancellation and on the generation being
// torn down (a connect-fatal drop closes e.done), so it never spins unbounded.
const selectPollInterval = 2 * time.Millisecond

// Open starts the connection lifecycle (spec §5.2).
//
// It is serialized with Close by lifeMu (one opener), and it creates a fresh per-generation epoch plus a FRESH per-Open supervisor.
//
// Invariants (spec §5.2):
//   - tr must be non-nil (a connection built without a transport cannot open).
//   - Double-open H6: if the supervisor is alive and shutdown==false (connection logically open, including during the reconnect inter-generation window) return ErrAlreadyOpen as a no-op. If shutdown==true (prior Close completed, supWg drained) the connection is legally reopened.
//   - G1: connectLoopWg.Wait() (join a dying reconnect loop) runs BEFORE creating fresh contexts,
//     so a stale reconnect loop cannot publish over the new generation.
//   - The supervisor's run()/notifier() are CONNECTION-owned goroutines joined by supWg (NOT epoch.spawn)
//     so the supervisor OUTLIVES any single epoch (Codex round-6) — an involuntary disconnect cancels the epoch ctx
//     but must not kill the supervisor (reconnect needs it).
//   - The async sender is a PER-GENERATION goroutine via epoch.spawn (dies with the epoch).
//
// mode selects the return behavior: OpenWaitSelected blocks on {Selected | conn-drop | ctx};
// OpenBackground returns after kickoff (passive HSMS-SS in particular needs background open).
//
// Active first-connect contract: an active transport dials SYNCHRONOUSLY during Open (via transport.Start),
// so a failure of the very FIRST dial (e.g. connection refused) is handled differently depending on mode.
// Under OpenWaitSelected (or for an inactive/passive transport under either mode) it is still returned synchronously by Open itself —
// auto-reconnect is not engaged for that initial connect.
// Under OpenBackground, for an active transport whose FSM never left NotConnectedState (TCPUp was never driven), the failure is instead a cold/unreachable peer:
// Open tears the failed attempt down, starts the background reconnect loop, and returns nil (Gap 1, rc3) —
// v1 parity ("start the device; it connects whenever the equipment appears").
// The reconnect loop (exponential backoff capped at T5) otherwise takes over only AFTER a generation has been established
// and later drops.
// A caller on OpenWaitSelected that wants to keep retrying an initially-unreachable active peer should retry Open. (A passive Open never blocks on a peer regardless of mode:
// it returns after ListenTCP + spawning the accept goroutine.)
//
// OpenWaitSelected wait-failure (I7): if the wait fails AFTER the generation was established —
// the caller ctx expired, or a first-generation select rejection tripped the always-on reconnect loop — Open returns an error
// but the connection lifecycle KEEPS RUNNING (goroutines, socket, reconnect loop); only a tr.Start failure rolls back.
// A caller that gets a wait error must Close() to release the lifecycle — a bare retry-Open returns ErrAlreadyOpen.
func (c *connection) Open(ctx context.Context, mode OpenMode) error {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()

	if c.tr == nil {
		return errors.New("hsms: Open requires a non-nil transport")
	}

	// Double-open guard (H6): key on the SUPERVISOR, the per-Open-cycle liveness token, not
	// on the current epoch's done channel. During the reconnect inter-generation window, cur
	// points at a done-closed epoch (just torn down) while the supervisor is still alive
	// (shutdown==false). The old cur-done check incorrectly passed in that window, allowing
	// a second Open to add two goroutines to supWg and store a second supervisor — Close's
	// supWg.Wait() would then wait forever for the first supervisor's goroutines (which nobody
	// stopped): a deterministic deadlock. Three cases (under lifeMu, which Open/Close hold):
	//   - s == nil:                  never opened               → proceed
	//   - s != nil && !shutdown:     logically open (incl. mid-reconnect) → ErrAlreadyOpen
	//   - s != nil && shutdown.Load: prior Close completed (supWg drained) → reopen, proceed
	if s := c.sup.Load(); s != nil && !c.shutdown.Load() {
		return ErrAlreadyOpen
	}

	// Fence any in-flight reconnect loop from a prior cycle: bump reconnectGen so the loop's
	// G2 fence (atomics-only) observes the advance and abandons instead of publishing over the
	// new generation. Open resets shutdown to false for the new cycle, so reconnectGen — NOT
	// shutdown — is the fence that catches a reopen while a stale loop is still in flight.
	c.reconnectGen.Add(1)
	c.shutdown.Store(false)

	// G1: join a dying reconnect loop BEFORE creating fresh contexts, so the loop cannot
	// cur.Store a stale epoch after we publish the new one. The reconnectGen bump above makes
	// any such loop abandon at its fence; this join then reaps it deterministically. The
	// loop's fence is atomics-only, so this lifeMu-held Wait cannot deadlock against it (round-8).
	c.connectLoopWg.Wait()

	// Fresh reconnect-cancel channel for this Open cycle (closed by Close to interrupt a T5
	// backoff). Stored BEFORE the epoch/supervisor exist, so any reconnect loop later spawned by
	// an involuntary drop observes this cycle's channel.
	cancel := make(chan struct{})
	c.reconnectCancel.Store(&cancel)

	cfg := c.cfg.Load()

	// Fresh per-generation epoch. The parent is context.Background(): e.ctx is
	// generation-lifetime and is cancelled ONLY by teardown (never by the caller's Open ctx,
	// which bounds only the OpenWaitSelected wait — D5a-7). stopTransport lets teardown join
	// the transport recv loop (Codex round-7).
	e := newEpoch(context.Background(), cfg.logger, cfg.senderQueueSize)
	e.stopTransport = c.tr.Stop
	// Mint the generation identity BEFORE publishing, so no reader can ever observe a live epoch with id 0.
	// cur is published here and in the reconnect loop, and NOWHERE else;
	// the loop only ever runs after the FSM entered NotConnected,
	// so cur advances only after the previous generation ended.
	// That is what makes the generation match in supervisor.step safe:
	// a mismatch means the reporting generation is already over,
	// so its disconnect cannot be the one that ends a live link.
	e.id = c.genSeq.Add(1)
	c.cur.Store(e)

	// FRESH per-Open supervisor (no channel reuse — round-5). Its run()/notifier() are
	// connection-owned (supWg), NOT epoch-spawned, so the supervisor spans reconnect
	// generations (round-6). It reads user handlers from the Connection's persistent pointer.
	s := newSupervisor(c.react, &c.handlers, &c.lifecycleSubs)
	// Install the LIVE closeTimeout provider (M7 — the evClose teardown reads current config, so a
	// mid-session UpdateConfigOptions(WithCloseTimeout) is honored) and the logger for the
	// notify-coalesce Warn (M4).
	s.closeTimeout = func() time.Duration { return c.cfg.Load().closeTimeout }
	s.logger = cfg.logger
	// The live-generation provider behind step's generation match.
	// It is a single atomic load and takes no locks: step runs on the FSM goroutine,
	// which an epoch teardown join can be waiting behind, so any lock the teardown path holds would close a cycle here.
	s.curGen = c.CurrentGeneration
	c.sup.Store(s)
	c.supWg.Add(2)
	go func() { defer c.supWg.Done(); s.run() }()
	go func() { defer c.supWg.Done(); s.notifier() }()

	// Per-generation async sender (epoch.spawn passes e.ctx; it exits when teardown cancels it).
	e.spawn(cfg.logger, "sender", func(ctx context.Context) { c.drainSendCh(ctx, e) })

	// Clear any transport Stop-seal left by a prior Close so this generation's tr.Start may register
	// its transport goroutines (I1). Safe under lifeMu: the prior Close's e.wait() already joined
	// every tr.Stop, so no straggler Stop is in flight whose seal this could undo.
	c.tr.ArmStart()

	// Dial (active) or listen (passive) + spawn the recv loop, driving rt (TCPUp/CommitSelected/
	// TCPDown). The recv loop is generation-scoped (e.ctx) and joined by teardown via tr.Stop.
	if err := c.tr.Start(e.ctx, c); err != nil {
		// Gap 1 (rc3): an ACTIVE connection under OpenBackground whose FIRST dial fails while the
		// FSM never left NotConnectedState (TCPUp was never driven) is a cold peer, not a fatal
		// error — v1 parity ("start the device; it connects whenever the equipment appears"). Tear
		// this failed epoch down WITHOUT s.requestClose/evClose: step()'s evClose handler
		// unconditionally latches s.closed=true, and that latch would permanently stop this SAME
		// persistent supervisor from ever processing another evTCPUp/evSelectAccepted — but the
		// reconnect loop needs this exact supervisor alive for every future generation (round-6:
		// the supervisor spans reconnect generations). e.teardown() is the same direct,
		// non-latching teardown call connectLoop's own dial-failure-retry branch already uses.
		//
		// This does NOT apply to the "TCPUp already fired, then Start errored" case below (E2
		// reconciliation #2) — that stays fatal exactly as before; s.State() reads NotSelected (or
		// further) there, not NotConnectedState, so this branch's condition is false and control
		// falls through to the existing fatal path.
		if mode == OpenBackground && c.tr.IsActive() && s.State() == NotConnectedState {
			c.cfg.Load().logger.Debug("hsms: initial connect failed, retrying in background", "error", err)

			e.teardown(c.cfg.Load().closeTimeout)
			_ = e.wait()

			// false: this is the initial connect, not a post-Selected reconnect — it must NOT
			// increment the cumulative Reconnects() metric (see its doc: "never for the very
			// first Open()").
			c.startConnectLoop(e, false)

			return nil
		}

		// E2 reconciliation #2: fence reconnect BEFORE the rollback teardown. A tr.Start that
		// failed AFTER driving evTCPUp leaves the FSM at NotSelected, so requestClose(e) drives
		// NotSelected->NotConnected and FIRES the reaction; with shutdown still false that
		// reaction would start a spurious reconnect loop for a connection whose Open is failing.
		// Setting shutdown makes the reaction's !shutdown check skip startConnectLoop.
		c.shutdown.Store(true)

		// Roll back the freshly created generation + supervisor so lifeMu is not released over
		// a half-open connection (a later Open would then see a torn-down cur and reopen).
		// CauseLocalClose: this rollback is a locally initiated teardown of a generation this same
		// Open just built — the same class of transition a user Close drives, reached by a different door.
		s.requestClose(e, CauseLocalClose)
		_ = e.wait()
		s.stop()
		c.supWg.Wait()

		return err
	}

	if mode == OpenWaitSelected {
		return c.waitSelected(ctx, e, s)
	}

	return nil
}

// waitSelected blocks until the FSM reaches Selected, the caller's ctx is cancelled, or the
// generation is torn down (a connect-fatal drop). It observes Selected via the lock-free FSM
// atomic on a bounded poll (require.Eventually-style — never a time.Sleep-poll) and also
// unblocks immediately on ctx.Done()/e.done via the select.
func (c *connection) waitSelected(ctx context.Context, e *epoch, s *supervisor) error {
	ticker := time.NewTicker(selectPollInterval)
	defer ticker.Stop()

	for {
		if s.State() == SelectedState {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.done:
			// The generation was torn down before reaching Selected (connect-fatal / drop).
			return ErrConnClosed
		case <-ticker.C:
		}
	}
}

// Close tears down the connection (spec §5.2, ctx-free — D5a-7).
//
// It is serialized with Open by lifeMu.
// Ordering is binding: requestClose(e) (pin + inject evClose) -> e.wait() (the pinned epoch's teardown completes while the supervisor is still alive
// and draining events) -> sup.stop() (only NOW tear down the supervisor).
// The supervisor outlives the epoch, never the reverse (round-6).
//
// Entry guards (round-7/8):
//   - NEVER-OPENED: cur == nil => ErrNotOpen (no requestClose/e.wait on a nil epoch/supervisor).
//   - IDEMPOTENT RE-CLOSE: cur is NOT cleared on Close, so a re-Close sees a non-nil but torn-down epoch.
//     If the supervisor already stopped (runDone closed) it returns the retained closeErr WITHOUT a second requestClose (which would deadlock on the now-unread events channel —
//     inject's runDone select is the backstop, but short-circuiting is the primary guard).
func (c *connection) Close() error {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()

	e := c.cur.Load()
	if e == nil {
		return ErrNotOpen // never opened
	}

	s := c.sup.Load()

	// Idempotent re-Close: the supervisor already stopped => return the prior result WITHOUT a
	// second requestClose. runDone closed implies (via the Close ordering below) e.done is also
	// closed, so e.wait() returns the retained closeErr immediately.
	if s != nil {
		select {
		case <-s.runDone:
			return e.wait()
		default:
		}
	}

	// Voluntary close: fence out reconnect (bump reconnectGen + set shutdown, re-checked by the
	// G2 fence / reconnect reactions), then funnel teardown through the supervisor's evClose. The
	// fence + successor RE-PIN run under publishMu, linearized against the reconnect loop's publish
	// (I1): re-loading cur here catches a successor generation the loop published just before we set
	// shutdown, so Close tears THAT generation down (no orphan) rather than the stale one loaded
	// above. publishMu is held ONLY for this atomic fence/re-pin — never across e.wait()/Stop below.
	c.publishMu.Lock()
	c.reconnectGen.Add(1)
	c.shutdown.Store(true)
	e = c.cur.Load() // re-pin under publishMu: a just-published reconnect successor, else unchanged
	c.publishMu.Unlock()

	// Interrupt a reconnect loop parked in its T5 backoff so Close stays bounded (the loop then
	// re-checks its F3/G2 fence and returns). Closed exactly once — a re-Close short-circuits
	// above, and a fresh Open installs a new channel.
	if p := c.reconnectCancel.Load(); p != nil {
		close(*p)
	}

	s.requestClose(e, CauseLocalClose) // pins e + injects evClose (initiates teardown of e from EVERY state)
	err := e.wait()                    // <-e.done; return closeErr — no poll, no F2 hang

	s.stop()       // close stopCh -> run() exits (closing notify/runDone -> notifier exits)
	c.supWg.Wait() // join run() + notifier()

	// Join any reconnect loop spawned by an earlier involuntary drop in this cycle. shutdown
	// (set above) + reconnectGen (bumped above) + the closed reconnectCancel make the loop
	// abandon at its F3/G2 fence and return; this join reaps it so no reconnect goroutine
	// outlives Close. It is SEPARATE from epoch.wg (§7.C), and the loop's fence is atomics-only,
	// so this lifeMu-held Wait cannot deadlock against the loop (round-8).
	c.connectLoopWg.Wait()

	return err
}

// react is the supervisor's transition reaction (spec §5.2/§7.E). It runs on the supervisor
// goroutine and is the SOLE initiator of teardown. It MUST NOT block the supervisor: the
// farewell uses TryLock + a short write deadline, and teardown is the non-blocking initiator
// (it kicks the bounded join on a separate goroutine and returns immediately).
//
// It acts only on transitions INTO NotConnected (fired by both a voluntary Close's
// evClose Selected->NotConnected and an involuntary evDisconnect). Ordering: (1) bounded
// best-effort farewell Separate, (2) if !shutdown start the reconnect loop, (3) e.teardown.
func (c *connection) react(prev, next ConnState) {
	if next != NotConnectedState {
		return // reactions only matter for transitions INTO NotConnected
	}

	e := c.cur.Load()
	if e == nil {
		return
	}

	// (1) Cause-aware farewell (§9.1.1): only on a GRACEFUL teardown from a live Selected link.
	// A comms-failure (TCPDown set commsFailure) sends NO Separate — the link is already gone.
	if prev == SelectedState && !e.commsFailure.Load() && e.liveConn() != nil {
		c.writeFarewellSeparate(e)
	}

	// (2) Reconnect on an INVOLUNTARY drop (not a voluntary Close / failing Open — both set
	// shutdown). startConnectLoop issues connectLoopWg.Add(1) SYNCHRONOUSLY here — BEFORE the
	// teardown below initiates the async close of e.done — so the Add strictly happens-before
	// e.done closes, and thus before any Open/Close that gates on e.done and then joins
	// connectLoopWg (a zero->1 Add can never race connectLoopWg.Wait — WaitGroup discipline,
	// §7.C). The spawned loop's first act is e.wait() on the epoch torn down just below, so it
	// never races teardown.
	if !c.shutdown.Load() {
		c.startConnectLoop(e, true) // a post-Selected involuntary drop always counts as a reconnect
	}

	// (3) Non-blocking teardown initiator (idempotent closeOnce — the supervisor's evClose
	// ensure-teardown and this both funnel here). closeSocket() runs unconditionally inside.
	e.teardown(c.cfg.Load().closeTimeout)
}

// writeFarewellSeparate attempts the bounded best-effort courtesy Separate.req on a graceful
// teardown from Selected (§7.E). It acquires e.writeMu with TryLock (the SAME lock a W-bit
// writer holds) — on contention it SKIPS and returns, so it can NEVER block teardown behind a
// wedged writer. On success it writes under a short deadline; any error is ignored (courtesy,
// not correctness). closeSocket() (unconditional, inside e.teardown) is what actually unblocks
// a wedged writer (J5), so skipping the farewell never stalls progress.
func (c *connection) writeFarewellSeparate(e *epoch) {
	if !e.writeMu.TryLock() {
		return // a writer holds writeMu — skip the courtesy Separate, never block teardown
	}
	defer e.writeMu.Unlock()

	// Bind the farewell write to THIS epoch's socket (I1 full-fix), like writeFrame. react only
	// calls this while e.liveConn() != nil, but re-capture under writeMu and nil-guard against the
	// TOCTOU where teardown closed the socket between that check and the TryLock above.
	conn := e.liveConn()
	if conn == nil {
		return
	}

	// Separate.req carries ControlSessionID (0xFFFF), NOT cfg.sessionID:
	// E37.1 §7.6 — "Separate shall always use SessionID 0xFFFF (binary, all ones)".
	// cfg.sessionID is the device ID and belongs in data messages only (E37.1 §8.1).
	//
	// This core is shared: BOTH hsmsss and secs1 build on it, and secs1 DOES reach this function.
	// secs1.New wraps NewConnection, its transport commits the FSM to Selected,
	// and a graceful Close from Selected lands here.
	// No HSMS frame reaches a SECS-I peer only because secs1's Write drops every non-zero SType before the wire,
	// so the value below is inert there rather than correct there.
	// The profile constant is therefore hard-coded for the HSMS-SS case it actually serves.
	// A future HSMS-GS transport, or any profile needing a different value, must make this transport-supplied rather than inherit it.
	sep := NewSeparateReq(ControlSessionID, c.sysGen.next())

	bufs, err := buildFrameBuffers(sep)
	if err != nil {
		// Unreachable in practice: a control frame is always the fixed 14 bytes and never
		// trips buildFrameBuffers' data-message size cap.
		// Skip the courtesy Separate rather than write onto a malformed frame — the same
		// fail-safe posture as the writeMu contention and nil-conn guards above.
		return
	}

	_ = c.tr.SetWriteDeadline(conn, time.Now().Add(farewellWriteTimeout))
	_ = c.tr.Write(e.ctx, conn, bufs)
	_ = c.tr.SetWriteDeadline(conn, time.Time{}) // clear the deadline for any later (teardown) use
}

// startConnectLoop launches the reconnect loop after an involuntary drop (spec §5.2/§7.C). It is
// called from the NotConnected reaction (on the supervisor goroutine) with the just-dropped epoch
// as prev. The loop runs under connectLoopWg — SEPARATE from epoch.wg (§7.C) — so a reconnect in
// flight during a teardown is never a member of the epoch join set that teardown waits on (which
// would deadlock). The connectLoopWg.Add(1) is issued HERE, synchronously on the supervisor
// goroutine and BEFORE react initiates the prev-epoch teardown, so the Add happens-before
// prev.done closes (WaitGroup discipline versus the Open/Close joins that gate on done).
//
// countReconnect selects whether a successful dial at the end of this loop increments the
// cumulative Reconnects() metric — true for a post-Selected involuntary drop, false for Task 4's
// initial-cold-connect retry (which must NOT count as a "reconnect": see
// ConnectionMetrics.Reconnects doc, "never for the very first Open()").
func (c *connection) startConnectLoop(prev *epoch, countReconnect bool) {
	gen := c.reconnectGen.Load()       // the generation THIS retry is scheduled at (the G2 fence base)
	cancel := c.reconnectCancel.Load() // this cycle's cancel channel (closed by Close to interrupt backoff)

	c.connectLoopWg.Go(func() {
		c.connectLoop(prev, gen, cancel, countReconnect)
	})
}

// connectLoop is the reconnect state machine (spec §5.2). It reconnects after a single involuntary
// drop: it first waits for the prior generation to be FULLY torn down (generation-serialization),
// then dials with an exponential backoff (WithReconnectBackoff) capped at T5, re-checking
// shutdown/reconnectGen on every attempt (F3) and once more via the atomics-only G2 fence
// immediately before publishing the fresh generation. The supervisor is NOT recreated here
// (round-6) — only a fresh epoch per generation; the loop hands each generation to the persistent
// supervisor via tr.Start (which drives evTCPUp -> CommitSelected -> Selected).
func (c *connection) connectLoop(prev *epoch, gen uint64, cancel *chan struct{}, countReconnect bool) {
	c.metrics.incConnRetry()
	defer c.metrics.decConnRetry()

	// Generation-serialization (E2 reconciliation #1, round-7): wait for the just-torn-down
	// prior epoch to be FULLY joined — its transport recv loop (via tr.Stop inside teardown) and
	// every per-generation goroutine — BEFORE dialing the next generation. Generations normally do
	// not overlap: a gen-N recv loop is gone before gen N+1 exists, so a stale gen-N evDisconnect
	// can never disconnect gen N+1. EXCEPTION (C1): a bounded tr.Stop that hits ErrCloseTimeout — a
	// data handler wedged the recv goroutine past the close timeout — ABANDONS that straggler, which
	// may outlive gen N. recvLoop's captured-genCtx guard fences it: the straggler observes its own
	// cancelled generation ctx and exits WITHOUT driving TCPDown, so it still cannot disconnect gen N+1.
	if prev != nil {
		_ = prev.wait()
	}

	// stop is closed by Close (reconnectCancel) — it lets a backoff be interrupted promptly so
	// a Close during reconnect does not wait out the whole backoff. It is NOT the supervisor
	// stopCh: the failed-Open rollback stops the supervisor but must not cancel reconnect here.
	var stop <-chan struct{}
	if cancel != nil {
		stop = *cancel
	}

	delay := c.cfg.Load().reconnectBackoffInitial

	for {
		cfg := c.cfg.Load()

		sleepFor := delay
		if ceil := cfg.timers.T5; sleepFor > ceil {
			sleepFor = ceil
		}

		// Exponential-backoff connect separation between attempts (active dial; a passive
		// re-listen is the same tr.Start call), capped at T5. Interruptible by a Close via the
		// reconnectCancel channel.
		if !c.reconnectSleep(sleepFor, stop) {
			return
		}

		delay = nextBackoffDelay(delay, cfg.reconnectBackoffMultiplier, cfg.timers.T5)

		// Optional test hook (nil in production, zero cost). The gen-fence / no-deadlock teeth
		// tests use it to pause the loop between the backoff and the fence.
		if hook := c.testHookConnectLoop; hook != nil {
			hook()
		}

		// F3: a Close (shutdown) or a fresh Open (reconnectGen advanced) during reconnect stops
		// the loop before it spends work building a generation.
		if c.shutdown.Load() || c.reconnectGen.Load() != gen {
			return
		}

		// Build the next-generation epoch (NOT yet published). stopTransport lets the epoch's
		// teardown join this generation's transport recv loop (round-7).
		e := newEpoch(context.Background(), cfg.logger, cfg.senderQueueSize)
		e.stopTransport = c.tr.Stop
		// The second and last cur publisher; see the identity note at the Open site.
		// This loop is reached only from the entering-NotConnected reaction (or Open's cold-connect retry),
		// so the generation it replaces has already ended.
		e.id = c.genSeq.Add(1)

		// G2 fence + publish, LINEARIZED against a voluntary Close under publishMu (I1). Re-check
		// shutdown + the captured reconnectGen and, if still current, arm the transport for this
		// fresh generation and publish it — atomically w.r.t. Close's {set shutdown + re-pin cur}.
		// Either Close observes this just-published successor and tears it down (no orphan), or this
		// loop observes shutdown first and abandons before publishing (and is joined by
		// connectLoopWg.Wait()). NEVER take lifeMu here — Open/Close hold lifeMu across
		// connectLoopWg.Wait(), so a lifeMu acquisition at this fence would deadlock against them.
		// publishMu is held ONLY for this atomic pin/publish, NEVER across the dial below.
		c.publishMu.Lock()
		if c.shutdown.Load() || c.reconnectGen.Load() != gen {
			c.publishMu.Unlock()
			return
		}
		c.tr.ArmStart() // clear the transport Stop-seal for this generation BEFORE Close can seal it (I1)
		c.cur.Store(e)
		c.publishMu.Unlock()

		// Per-generation async sender (dies with the epoch, via epoch.spawn / e.ctx).
		e.spawn(cfg.logger, "sender", func(ctx context.Context) { c.drainSendCh(ctx, e) })

		// Dial (active) / re-listen (passive) + spawn the recv loop; the persistent supervisor
		// drives evTCPUp -> CommitSelected -> Selected across this new generation (round-6). On
		// success the generation is live and the loop's job is done.
		if err := c.tr.Start(e.ctx, c); err != nil {
			// Dial failed: tear this generation down, join it, and retry after the next backoff
			// (still F3/G2-gated). Joining here keeps the next attempt from overlapping this one.
			cfg.logger.Debug("hsms: reconnect dial failed, retrying", "error", err)
			e.teardown(c.cfg.Load().closeTimeout) // LIVE closeTimeout (M7) — consistent with the react teardown
			_ = e.wait()

			continue
		}

		if countReconnect {
			c.metrics.incReconnects()
		}

		return
	}
}

// nextBackoffDelay advances the exponential reconnect backoff: cur scaled by multiplier, capped
// at ceil (T5). The next<=0 check is what makes this safe against float64 overflow/anomalies
// (Inf/NaN convert to a huge or zero/negative time.Duration; a huge value already fails the
// next>ceil check, and a non-positive one is caught explicitly) — WithReconnectBackoff's own
// validation (multiplier >= 1.0) keeps this out of reach in practice, but this clamp is the
// actual safety net, not the option validation alone.
func nextBackoffDelay(cur time.Duration, multiplier float64, ceil time.Duration) time.Duration {
	next := time.Duration(float64(cur) * multiplier)
	if next <= 0 || next > ceil {
		return ceil
	}

	return next
}

// reconnectSleep waits d (the current backoff delay, already capped at the T5 ceiling by the
// caller) before the next dial attempt. It returns true when the wait elapsed and false when it
// was interrupted by stop (a Close). A non-positive d still honors stop without blocking. It
// never time.Sleep-polls state — it is a single timer selected against the stop channel.
func (c *connection) reconnectSleep(d time.Duration, stop <-chan struct{}) bool {
	if d <= 0 {
		select {
		case <-stop:
			return false
		default:
			return true
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-stop:
		return false
	}
}

// TCPUp is called by the transport when a TCP connection is established (TransportRuntime).
//
// It publishes the socket on the current epoch and then advances the FSM NotConnected -> NotSelected SYNCHRONOUSLY via a guarded CAS (CommitConnected, symmetric with CommitSelected),
// which also enqueues evTCPUp for the deduped entering-NotSelected reaction/notify.
// The socket is published BEFORE the supervisor call so it is visible before State() flips to NotSelected.
func (c *connection) TCPUp(conn net.Conn) {
	if e := c.cur.Load(); e != nil {
		e.setConn(conn)
	}

	if s := c.sup.Load(); s != nil {
		// CauseLocalOpen: reaching NotSelected means OUR Open (or the reconnect loop it owns)
		// established the link, whether by dialing out or by accepting the peer we were listening for.
		s.CommitConnected(CauseLocalOpen)
	}
}

// TCPDown is called by the transport when the TCP connection is lost (TransportRuntime).
//
// Any TCPDown is an INVOLUNTARY drop, so it marks the current generation commsFailure=true (the NotConnected reaction then sends NO farewell Separate —
// §9.1.1) and injects evDisconnect.
// A graceful voluntary Close never routes through TCPDown; it funnels through evClose and leaves commsFailure false.
//
// CauseUnknown, not CauseIOError:
// this entry point carries no classification, and a transport that reaches it has declined to name one.
// Guessing "I/O error" here would put a wrong cause on a linktest-failure or select-rejection drop.
// In-module transports call TCPDownWithCause instead.
func (c *connection) TCPDown(cause error) {
	c.TCPDownWithCause(cause, CauseUnknown)
}

// TCPDownWithCause is TCPDown with the TransitionCause named by the transport.
//
// It is the cause-carrying half of the TCP-down back-channel, reached by the in-module transports through a package-local capability
// interface rather than through [TransportRuntime] — widening that exported interface would break external implementers,
// the same reason LinktestSuppression is reached that way.
//
// cause is the transport's error for the drop (used for the farewell decision, unchanged);
// transitionCause is the closed-set classification carried to the lifecycle subscribers.
//
// It carries NO generation identity, so it resolves the current generation at call time.
// A transport that can outlive the generation it belongs to should report through
// [connection.TCPDownFromGeneration] instead, which is generation-isolated.
func (c *connection) TCPDownWithCause(cause error, transitionCause TransitionCause) {
	c.injectDisconnect(0, cause, transitionCause)
}

// CurrentGeneration returns the identity of the generation that is live right now, or 0 when none is.
//
// A transport reads it once per Start and hands the value back with every disconnect it reports,
// which is how the core tells a report from the LIVE generation apart from one arriving late out of a generation that has already ended.
// It is reached by type assertion rather than through [TransportRuntime]:
// widening that exported interface would break external implementers,
// the same reason TCPDownWithCause is reached that way.
func (c *connection) CurrentGeneration() uint64 {
	if e := c.cur.Load(); e != nil {
		return e.id
	}

	return 0
}

// TCPDownFromGeneration is TCPDownWithCause reported ON BEHALF OF a specific generation —
// the one the caller belongs to, as returned by [connection.CurrentGeneration] when that generation started.
//
// A transport goroutine can outlive its own generation.
// The shutdown join is bounded,
// so a goroutine wedged past the close timeout is abandoned and may resume at any later time,
// by which point a successor generation can already be selected.
// Because the plain TCPDown entry points resolve the current generation at CALL time,
// such a straggler would otherwise mark the successor's socket as failed and drop a link it knows nothing about.
//
// Reporting the generation makes that impossible in two places.
// Here, a report whose generation is no longer live is a counted no-op.
// In particular it does not touch the successor's comms-failure flag,
// which decides whether that generation still sends a courtesy Separate when it closes.
// And on the event itself: the generation travels with the queued event and is re-checked when the FSM PROCESSES it,
// which is what closes the window between this check and the injection (see supervisor.step).
//
// A gen of 0 means "unidentified" and skips the match, behaving exactly like TCPDownWithCause.
func (c *connection) TCPDownFromGeneration(gen uint64, cause error, transitionCause TransitionCause) {
	c.injectDisconnect(gen, cause, transitionCause)
}

// injectDisconnect is the shared body of the TCP-down back-channel: mark the generation as a comms failure
// (so the entering-NotConnected reaction sends no farewell Separate) and inject evDisconnect.
//
// gen is the reporting generation's identity, or 0 when the caller did not name one.
func (c *connection) injectDisconnect(gen uint64, cause error, transitionCause TransitionCause) {
	e := c.cur.Load()
	if e == nil {
		return
	}

	if gen != 0 && e.id != gen {
		// The reporting generation is over and a successor is live.
		// Dropping here is safe because cur advances only after the FSM entered NotConnected,
		// so this generation's link was already torn down and this report cannot be the one that ends a live link.
		if s := c.sup.Load(); s != nil {
			s.staleGen.Add(1)
		}

		c.cfg.Load().logger.Debug("hsms: dropped a disconnect reported by a generation that has ended",
			"reported_generation", gen, "current_generation", e.id, "error", cause)

		return
	}

	e.commsFailure.Store(true)

	if s := c.sup.Load(); s != nil {
		s.injectFrom(gen, evDisconnect, transitionCause)
	}
}
