package hsms

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/internal/gencap"
)

// farewellWriteTimeout bounds the courtesy farewell Separate write (§7.E). The farewell is a
// bounded best-effort courtesy: it acquires e.writeMu with TryLock (never blocks teardown) and
// writes under a short deadline. On any failure it is skipped and teardown proceeds — the
// unconditional closeSocket() in e.teardown is what guarantees progress.
const farewellWriteTimeout = 500 * time.Millisecond

// selectPollInterval is the internal poll cadence used by OpenWaitSelected to observe the
// Selected state via the lock-free FSM atomic (a require.Eventually-style bounded wait — never
// a time.Sleep-poll). The wait also unblocks on ctx cancellation and on the generation being
// torn down (a connect-fatal drop closes e.done), so it never spins unbounded.
const selectPollInterval = 2 * time.Millisecond

// errNotifierTimeout is the shutdown result when the state-change notifier outlives the close timeout —
// a StateChangeHandler or lifecycle subscriber that is blocked, or that is itself the Close being joined.
var errNotifierTimeout = fmt.Errorf("%w: state-change handler still running", ErrCloseTimeout)

// abortToken is a one-shot interrupt a Close fires to unblock the Open it finds registered.
//
// Close (pre-firing a registered token, or firing the one an in-flight Open just published)
// and Open (pre-firing its own fresh token when a Close is already pending)
// both reach fire() through the SAME sync.Once,
// so the several racing call sites documented on connection.pendingCloses / connection.openAbort can never double-close ch.
type abortToken struct {
	ch   chan struct{}
	once sync.Once
}

// newAbortToken returns a fresh, unfired abortToken.
func newAbortToken() *abortToken {
	return &abortToken{ch: make(chan struct{})}
}

// fire closes the token's channel exactly once; a later call is a no-op.
func (t *abortToken) fire() {
	t.once.Do(func() { close(t.ch) })
}

// fired reports whether fire has already run, without blocking.
func (t *abortToken) fired() bool {
	select {
	case <-t.ch:
		return true
	default:
		return false
	}
}

// firstDialState publishes what gencap.DialBounder.BoundDial needs to bound the FIRST dial of one Open cycle.
// epochCtx is the original Start ctx — the identity BoundDial matches against —
// callerCtx is the ctx the caller passed to Open, and abort is that Open's abort channel.
// Open stores it immediately before c.tr.Start and clears it immediately after,
// so a reconnect generation's Start (a different epochCtx) never matches.
type firstDialState struct {
	epochCtx  context.Context
	callerCtx context.Context
	abort     <-chan struct{}

	// callerCut is set by BoundDial's caller-ctx bridge the instant it actually cuts the merged dial ctx, before that bridge cancels it —
	// see mapAbortedStartErr, which reads this flag to credit a Start failure to the caller's own ctx only on that provenance, never on a context sentinel alone.
	callerCut atomic.Bool
}

// Open starts the connection lifecycle (spec §5.2).
//
// It is serialized with Close by lifeMu (one opener), and it creates a fresh per-generation epoch plus a FRESH per-Open supervisor.
//
// Invariants (spec §5.2):
//   - tr must be non-nil (a connection built without a transport cannot open).
//   - Double-open H6: if the supervisor is alive and shutdown==false (connection logically open, including during the reconnect inter-generation window) return ErrAlreadyOpen as a no-op. If shutdown==true (prior Close completed, supervisor joined) the connection is legally reopened.
//   - connectLoopWg.Wait() joins a dying reconnect loop first, before creating fresh contexts,
//     so a stale reconnect loop cannot publish over the new generation.
//   - The supervisor's run()/notifier() are per-Open goroutines joined through the supervisor's own runDone/notifierDone (NOT epoch.spawn)
//     so the supervisor OUTLIVES any single epoch — an involuntary disconnect cancels the epoch ctx
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
//
// Close interruption and the first-dial ctx bound: Open publishes a one-shot abort token (under
// abortMu, right after the double-open guard) and clears it on every return path before lifeMu releases —
// see connection.openAbort / connection.pendingCloses for the full registration protocol.
// A concurrent Close fires that token instead of blocking behind lifeMu until this Open returns on its own.
// Open observes the fire in two places: waitSelected's own select, and the active transport's first dial,
// bounded through the firstDialState this method publishes for gencap.DialBounder.BoundDial.
// The caller's own ctx bounds that same first dial, in BOTH OpenWaitSelected and OpenBackground modes:
// a caller ctx that expires while the dial is still in flight rolls back exactly like a fired abort,
// taking the fatal/rollback path below rather than falling into the cold-active background retry.
// Once the dial has returned a connection, neither the caller's ctx nor a Close abort has any further effect on that generation —
// see BoundDial.
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
	// a second Open to store a second supervisor over the first, whose goroutines nobody would
	// ever stop. Three cases (under lifeMu, which Open/Close hold):
	//   - s == nil:                  never opened               → proceed
	//   - s != nil && !shutdown:     logically open (incl. mid-reconnect) → ErrAlreadyOpen
	//   - s != nil && shutdown.Load: prior Close completed (supervisor joined) → reopen, proceed
	//
	// A prior shutdown may have abandoned its notifier (a callback outlived the close timeout).
	// Reopening does not wait for it: the new supervisor has its own completion signals,
	// so the straggler can never delay or corrupt this cycle's joins.
	if s := c.sup.Load(); s != nil && !c.shutdown.Load() {
		return ErrAlreadyOpen
	}

	// Test seam: between the double-open guard above and publishing this Open's abort
	// token below — a test pauses here to register a Close first, exercising the registration
	// ordering between a racing Close and this Open's publish.
	if hook := c.testHookOpenBeforePublish; hook != nil {
		hook()
	}

	// Publish this Open's abort token.
	// A Close that registered (bumped pendingCloses) before this point could not have seen openAbort yet,
	// so it cannot have fired it — pre-fire it here instead.
	// A Close that registers AFTER this point finds openAbort non-nil and fires it itself (see Close, below).
	// Either way no registered Close is ever missed:
	// registration and this publish both happen under abortMu, so the two orderings are exhaustive.
	tok := newAbortToken()
	c.abortMu.Lock()
	if c.pendingCloses > 0 {
		tok.fire()
	}
	c.openAbort = tok
	c.abortMu.Unlock()

	// Cleared on EVERY return path, before lifeMu releases: this defer is registered AFTER the
	// lifeMu-unlock defer at the top of Open, so LIFO runs it BEFORE that unlock actually fires.
	defer func() {
		c.abortMu.Lock()
		c.openAbort = nil
		c.abortMu.Unlock()
	}()

	// Fence any in-flight reconnect loop from a prior cycle:
	// bump reconnectGen so the loop's reconnect fence (atomics-only) observes the advance and abandons instead of publishing over the new generation.
	// Open resets shutdown to false for the new cycle,
	// so reconnectGen — NOT shutdown — is the fence that catches a reopen while a stale loop is still in flight.
	c.reconnectGen.Add(1)
	c.shutdown.Store(false)

	// Join a dying reconnect loop BEFORE creating fresh contexts,
	// so the loop cannot cur.Store a stale epoch after we publish the new one.
	// The reconnectGen bump above makes any such loop abandon at its fence; this join then reaps it deterministically.
	// The loop's fence is atomics-only, so this lifeMu-held Wait cannot deadlock against it.
	c.connectLoopWg.Wait()

	// Fresh reconnect-cancel channel for this Open cycle (closed by Close to interrupt a T5
	// backoff). Stored BEFORE the epoch/supervisor exist, so any reconnect loop later spawned by
	// an involuntary drop observes this cycle's channel.
	cancel := make(chan struct{})
	c.reconnectCancel.Store(&cancel)

	cfg := c.cfg.Load()

	// Seed the reconnect backoff for this Open cycle from the configured initial delay, so every
	// fresh cycle — including the cold-active background retry below — starts the ramp over,
	// regardless of how far a prior cycle's backoff had grown (see connection.reconnectDelay).
	c.reconnectDelay.Store(int64(cfg.reconnectBackoffInitial))

	// Fresh per-generation epoch.
	// The parent is context.Background(): e.ctx is
	// generation-lifetime and is cancelled ONLY by teardown, never directly by the caller's Open ctx
	// (which instead bounds the first dial and the OpenWaitSelected wait through separate,
	// narrower mechanisms — see firstDialState and waitSelected).
	// stopTransport lets teardown join the transport recv loop.
	e := newEpoch(context.Background(), cfg.logger, cfg.senderQueueSize)
	e.stopTransport = c.tr.Stop
	// The gate teardown latches e.ended under,
	// so a generation-guarded synchronous commit cannot straddle the end of its own generation.
	e.genGate = &c.genGate
	// Mint the generation identity BEFORE publishing, so no reader can ever observe a live epoch with id 0.
	// cur is published here and in the reconnect loop, and NOWHERE else;
	// the loop only ever runs after the FSM entered NotConnected,
	// so cur advances only after the previous generation ended.
	// That is what makes the generation match in supervisor.step safe:
	// a mismatch means the reporting generation is already over,
	// so its disconnect cannot be the one that ends a live link.
	e.id = c.genSeq.Add(1)
	c.cur.Store(e)

	// FRESH per-Open supervisor (no channel reuse). Its run()/notifier() are
	// joined through its own runDone/notifierDone, NOT epoch-spawned, so the supervisor spans
	// reconnect generations.
	// It reads user handlers from the Connection's persistent pointer.
	s := newSupervisor(c.react, &c.handlers, &c.lifecycleSubs)
	// Install the LIVE closeTimeout provider (M7 — the evClose teardown reads current config, so a
	// mid-session UpdateConfigOptions(WithCloseTimeout) is honored) and the logger for the
	// notify-coalesce Warn (M4).
	s.closeTimeout = func() time.Duration { return c.cfg.Load().closeTimeout }
	s.logger = cfg.logger
	// Handler-panic/Goexit reporting hooks (installed BEFORE run()/notifier() start below), each reading the connection's LIVE config on every call rather than the cfg snapshot taken here,
	// so a later UpdateConfigOptions(WithLogger) is honored by every callback report this supervisor makes for the rest of its cycle.
	s.reportPanic = func(kind string, r any) { countHandlerPanic(&c.metrics, c.cfg.Load().logger, kind, r) }
	s.reportGoexit = func(kind string) { logHandlerGoexit(c.cfg.Load().logger, kind) }
	// The live-generation provider behind step's generation match.
	// It is a single atomic load and takes no locks: step runs on the FSM goroutine,
	// which an epoch teardown join can be waiting behind, so any lock the teardown path holds would close a cycle here.
	s.curGen = c.CurrentGeneration
	// The fence behind the Select-lost commit, which never reaches step and so is not covered by its match.
	s.commitGate = c.commitGate
	// The Select commit's own fence: the same generation-guarded CAS discipline, but it also gates a
	// gen of 0 and stores the reconnect-backoff reset marker (see connection.selectCommitGate).
	s.selectGate = c.selectCommitGate
	// The TCP-up commit's own fence: gates a gen of 0 too,
	// and admits at most one TCP-up per generation, recording whether it committed (see connection.tcpUpCommitGate).
	s.tcpUpCommitGate = c.tcpUpCommitGate
	c.sup.Store(s)
	go s.run()
	go s.notifier()

	// Per-generation async sender (epoch.spawn passes e.ctx; it exits when teardown cancels it).
	e.spawn(cfg.logger, "sender", func(ctx context.Context) { c.drainSendCh(ctx, e) })

	// Clear any transport Stop-seal left by a prior Close so this generation's tr.Start may register
	// its transport goroutines (I1). Safe under lifeMu: the prior Close's e.wait() already joined
	// every tr.Stop, so no straggler Stop is in flight whose seal this could undo.
	c.tr.ArmStart()

	// Publish the state gencap.DialBounder.BoundDial needs to bound the dial an active transport is about to make,
	// keyed by e.ctx's identity — the EXACT ctx tr.Start receives below.
	// Cleared immediately after Start returns (the deferred Store below runs even if a custom Start panics),
	// so it is live only for this one call —
	// a later reconnect generation's Start carries a different e.ctx and is never bound.
	// Kept locally too, so a failed Start can still read its callerCut flag below after the
	// deferred Store(nil) has already cleared the connection's own copy.
	fd := &firstDialState{epochCtx: e.ctx, callerCtx: ctx, abort: tok.ch}
	c.firstDial.Store(fd)

	// Dial (active) or listen (passive) + spawn the recv loop, driving rt (TCPUp/CommitSelected/
	// TCPDown). The recv loop is generation-scoped (e.ctx) and joined by teardown via tr.Stop.
	err := func() error {
		defer c.firstDial.Store(nil)

		return c.tr.Start(e.ctx, c)
	}()

	if err != nil {
		// Because only the dial is bounded, an abort or an expired caller ctx always surfaces as a Start error BEFORE TCP-up —
		// the FSM is still NotConnected, no recv goroutine exists, no reconnect can have started.
		// Map that Start failure to the caller-visible error and skip the cold-active background retry below entirely
		// (mapAbortedStartErr has the full mapping, including the caller-cancellation provenance it requires).
		ctxErr := ctx.Err()
		aborted := tok.fired()
		callerCut := fd.callerCut.Load()

		// Latch e as ended BEFORE the cold-branch decision below, so e.tcpUpCommitted is final either way:
		// markEnded takes the generation gate's write lock, which waits out any TCP-up commit still inside its own RLock section,
		// and a commit that arrives after this latch is refused by that same gate.
		// This is deliberately NOT the full teardown:
		// the cold branch runs its own full e.teardown()/e.wait() below exactly as before,
		// and the fatal path's requestClose funnels through react —
		// which still finds a live socket to send the courtesy farewell from, since nothing has closed it yet —
		// before ITS teardown call (idempotent alongside this one) runs.
		e.markEnded()

		// Gap 1 (rc3): an ACTIVE connection under OpenBackground whose FIRST dial fails while the FSM never left NotConnectedState
		// AND no TCP-up commit ever succeeded on this generation is a cold peer, not a fatal error —
		// v1 parity ("start the device; it connects whenever the equipment appears").
		//
		// !e.tcpUpCommitted.Load() is what keeps the "TCPUp already fired, then Start errored" case fatal exactly as before (the rollback below):
		// s.State() usually already reads NotSelected (or further) there, so the branch condition is already false;
		// the marker additionally catches a report whose own processing is still pending.
		//
		// ctxErr == nil && !aborted additionally excludes a bounded-dial interruption —
		// an interrupted dial is not a "cold peer that will show up later", it is the caller (or a concurrent Close) asking to stop,
		// so it always takes the fatal/rollback path below instead of the cold-active background retry.
		if mode == OpenBackground && c.tr.IsActive() && s.State() == NotConnectedState &&
			ctxErr == nil && !aborted && !e.tcpUpCommitted.Load() {
			c.cfg.Load().logger.Debug("hsms: initial connect failed, retrying in background", "error", err)

			// e.teardown() is the same direct, non-latching call (idempotent alongside markEnded above)
			// connectLoop's own dial-failure branch uses, WITHOUT s.requestClose/evClose:
			// step()'s evClose handler unconditionally latches s.closed=true,
			// and that latch would permanently stop this SAME persistent supervisor from ever processing another evTCPUp/evSelectAccepted —
			// but the reconnect loop needs this exact supervisor alive for every future generation.
			e.teardown(c.cfg.Load().closeTimeout)
			_ = e.wait()

			close(e.startReturned) // release the barrier for whatever successor this loop starts

			// false: this is the initial cold-connect retry itself, not a recovery from an involuntary drop —
			// its own eventual success must NOT increment the cumulative Reconnects() metric
			// (see ConnectionMetrics.Reconnects: a cold background retry's own success is not counted).
			c.startConnectLoop(e, false)

			return nil
		}

		return c.rollbackFailedOpen(e, s, err, ctxErr, aborted, callerCut)
	}

	// This generation is live: release its barrier so a future successor (spawned whenever it
	// eventually drops) never has to wait on a Start call that has already returned.
	close(e.startReturned)

	if mode == OpenWaitSelected {
		return c.waitSelected(ctx, e, s, tok.ch)
	}

	return nil
}

// rollbackFailedOpen performs Open's fatal-failure rollback for every Start failure
// that does NOT qualify for the cold-active background retry above: a plain dial failure under OpenWaitSelected,
// any passive transport's listen failure, a caller-ctx/Close-aborted dial,
// or a Start that failed AFTER driving TCP-up on this generation.
// The caller has already latched e as ended, so e.tcpUpCommitted is final,
// but e's actual teardown — socket close, task join — has NOT run yet.
//
// It fences reconnect and interrupts any in-flight reconnect loop the same way Close does —
// bumping reconnectGen, latching shutdown, re-pinning cur, and closing reconnectCancel —
// before tearing the pinned generation down through the supervisor's own evClose
// (so react still finds a live socket to send the courtesy farewell from) and joining the supervisor,
// then joins connectLoopWg exactly as Close does,
// so a reconnect loop spawned by a race between the cold-branch check above and this fence cannot outlive this Open call.
func (c *connection) rollbackFailedOpen(e *epoch, s *supervisor, err, ctxErr error, aborted, callerCut bool) error {
	// Fenced like Close: a tr.Start that failed AFTER driving evTCPUp leaves the FSM at NotSelected,
	// so requestClose(pinned) drives NotSelected->NotConnected and FIRES the reaction;
	// with shutdown still false that reaction would start a spurious reconnect loop for a connection whose Open is failing.
	// Setting shutdown makes the reaction's !shutdown check skip startConnectLoop.
	// reconnectGen is bumped and cur re-pinned under publishMu, the SAME fence Close itself uses,
	// so a reconnect loop that raced ahead of this rollback and already published a successor
	// is the one this rollback tears down instead of the stale e (no orphan) —
	// e's own barrier, released below, already keeps that from happening for e itself; the re-pin is defence in depth.
	c.publishMu.Lock()
	c.reconnectGen.Add(1)
	c.shutdown.Store(true)
	pinned := c.cur.Load()
	c.publishMu.Unlock()

	// Interrupt a reconnect loop parked in its backoff, exactly like Close — closed exactly once per Open cycle:
	// a later Close finds runDone already closed (via joinSupervisor below) and returns early instead of closing this same channel again,
	// and the next Open installs a fresh one.
	if p := c.reconnectCancel.Load(); p != nil {
		close(*p)
	}

	close(e.startReturned)

	// Roll back the freshly created generation + supervisor so lifeMu is not released over
	// a half-open connection (a later Open would then see a torn-down cur and reopen).
	// CauseLocalClose: this rollback is a locally initiated teardown of a generation this same
	// Open just built — the same class of transition a user Close drives, reached by a different door.
	// The rollback's join result is recorded for a later Close; Open still reports the Start failure.
	deadline := time.Now().Add(c.cfg.Load().closeTimeout)
	s.requestClose(pinned, CauseLocalClose)

	teardownErr := pinned.wait()
	if pinned != e {
		// e was never the pinned generation, so nothing else tears it down:
		// markEnded above only latched it as ended, it still needs its own real teardown (socket close, task join).
		e.teardown(c.cfg.Load().closeTimeout)
		teardownErr = firstErr(teardownErr, e.wait())
	}

	s.shutdownErr = firstErr(teardownErr, c.joinSupervisor(s, deadline))

	// Join any reconnect loop a race above might have spawned before this rollback closed
	// reconnectCancel — mirrors Close's own join, so no reconnect goroutine outlives a failed Open.
	c.connectLoopWg.Wait()

	return mapAbortedStartErr(err, ctxErr, aborted, callerCut)
}

// waitSelected blocks until the FSM reaches Selected, the caller's ctx is cancelled, the
// generation is torn down (a connect-fatal drop), or a concurrent Close aborts this Open.
// It observes Selected via the lock-free FSM atomic on a bounded poll
// (require.Eventually-style — never a time.Sleep-poll) and also unblocks immediately on
// ctx.Done()/e.done/abort via the select.
func (c *connection) waitSelected(ctx context.Context, e *epoch, s *supervisor, abort <-chan struct{}) error {
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
		case <-abort:
			// A Close registered while this wait was parked here.
			// Under the existing I7 contract a failed OpenWaitSelected wait does NOT roll back —
			// the lifecycle keeps running — and by the pendingCloses invariant a registered Close is already blocked on lifeMu,
			// so it performs the normal, fully fenced teardown the instant this call returns and releases lifeMu.
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
// The supervisor outlives the epoch, never the reverse.
//
// Entry guards:
//   - NEVER-OPENED: cur == nil => ErrNotOpen (no requestClose/e.wait on a nil epoch/supervisor).
//   - IDEMPOTENT RE-CLOSE: cur is NOT cleared on Close, so a re-Close sees a non-nil but torn-down epoch.
//     If the supervisor already stopped (runDone closed) it returns the retained shutdown result WITHOUT a second requestClose (which would deadlock on the now-unread events channel —
//     inject's runDone select is the backstop, but short-circuiting is the primary guard).
//
// The notifier join is bounded by the close timeout, counted from Close's entry:
// the notifier runs user callbacks, and one of them may be this very Close.
// On expiry the notifier is abandoned and Close reports ErrCloseTimeout,
// unless the epoch join already reported its own, more specific error.
//
// Close no longer waits behind an Open that is itself blocked — in OpenWaitSelected's
// wait for Selected, or in an active transport's first dial.
// Close registers as a PENDING close, and fires any in-flight Open's abort token, BEFORE it ever
// tries to acquire lifeMu — see connection.pendingCloses / connection.openAbort for the full
// registration protocol.
func (c *connection) Close() error {
	// Register BEFORE lifeMu: bump pendingCloses and fire any in-flight Open's abort token.
	// A registration that lands before Open publishes its token is caught by Open's own pre-fire check instead (see Open, above) —
	// the two sides agree under the SAME abortMu, so neither ordering ever misses the other.
	c.abortMu.Lock()
	c.pendingCloses++
	if c.openAbort != nil {
		c.openAbort.fire()
	}
	c.abortMu.Unlock()

	c.lifeMu.Lock()

	// Test seam: registered BEFORE the lifeMu-unlock defer below, so LIFO runs it AFTER
	// lifeMu actually releases — a test uses it to observe a queued Open's freshly published
	// token before this Close call returns to its caller.
	if hook := c.testHookCloseAfterUnlock; hook != nil {
		defer hook()
	}
	defer c.lifeMu.Unlock()

	// Unregister IMMEDIATELY after acquiring lifeMu — a plain statement, not a defer, so the
	// window in which a fresh Open could observe "a Close is blocked on lifeMu right now" is as
	// short as possible (see the pendingCloses invariant on the field doc).
	c.abortMu.Lock()
	c.pendingCloses--
	c.abortMu.Unlock()

	e := c.cur.Load()
	if e == nil {
		return ErrNotOpen // never opened
	}

	s := c.sup.Load()

	// Idempotent re-Close: the supervisor already stopped => return the prior result WITHOUT a
	// second requestClose. runDone closes only inside joinSupervisor, and every caller of that
	// (Close below, Open's rollback) records shutdownErr under lifeMu before releasing it.
	if s != nil {
		select {
		case <-s.runDone:
			return s.shutdownErr
		default:
		}
	}

	deadline := time.Now().Add(c.cfg.Load().closeTimeout)

	// Voluntary close: fence out reconnect (bump reconnectGen + set shutdown, re-checked by
	// the reconnect fence), then funnel teardown through the supervisor's evClose. The
	// fence + successor RE-PIN run under publishMu, linearized against the reconnect loop's publish
	// (I1): re-loading cur here catches a successor generation the loop published just before we set
	// shutdown, so Close tears THAT generation down (no orphan) rather than the stale one loaded
	// above. publishMu is held ONLY for this atomic fence/re-pin — never across e.wait()/Stop below.
	c.publishMu.Lock()
	c.reconnectGen.Add(1)
	c.shutdown.Store(true)
	e = c.cur.Load() // re-pin under publishMu: a just-published reconnect successor, else unchanged
	c.publishMu.Unlock()

	// Interrupt a reconnect loop parked in its T5 backoff so it does not add its own wait to Close's
	// real bound (the loop then re-checks its shutdown fence and returns) — one of several joins Close
	// still performs; see [Connection.Close] for what actually bounds those and what does not.
	// Closed exactly once — a re-Close short-circuits above, and a fresh Open installs a new channel.
	if p := c.reconnectCancel.Load(); p != nil {
		close(*p)
	}

	s.requestClose(e, CauseLocalClose) // pins e + injects evClose (initiates teardown of e from EVERY state)
	err := e.wait()                    // <-e.done; return closeErr — no poll, no F2 hang

	err = firstErr(err, c.joinSupervisor(s, deadline))
	s.shutdownErr = err

	// Join any reconnect loop spawned by an earlier involuntary drop in this cycle. shutdown
	// (set above) + reconnectGen (bumped above) + the closed reconnectCancel make the loop
	// abandon at its reconnect fence and return; this join reaps it so no reconnect goroutine
	// outlives Close. It is SEPARATE from epoch.wg (§7.C), and the loop's fence is atomics-only,
	// so this lifeMu-held Wait cannot deadlock against the loop.
	c.connectLoopWg.Wait()

	return err
}

// joinSupervisor stops s and joins its FSM goroutine, then bounds only the notifier join by deadline.
//
// run() does not invoke state-change handlers or lifecycle subscribers, so its join stays unbounded;
// abandoning it would leave a straggler FSM that reacts against whatever epoch cur holds next.
// The notifier runs those callbacks, and a callback may be the very Close waiting here,
// so its join ends at deadline: the notifier is abandoned and errNotifierTimeout is returned.
// An already-finished notifier wins over an expired deadline.
func (c *connection) joinSupervisor(s *supervisor, deadline time.Time) error {
	s.stop() // close stopCh -> run() exits, closing notify (ending the notifier's range) and runDone
	<-s.runDone

	select {
	case <-s.notifierDone:
		return nil
	default:
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errNotifierTimeout
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case <-s.notifierDone:
		return nil
	case <-timer.C:
		return errNotifierTimeout
	}
}

// mapAbortedStartErr maps a tr.Start failure to the caller-visible error when the FIRST dial of an
// Open cycle was bounded: a fired abort reports ErrConnClosed (a Close was requested — checked
// first, the more actionable of the two when both happen to be true).
// The caller's ctx is credited only on provenance, never on a context sentinel matching alone:
// callerCut is true only when BoundDial's own caller-ctx bridge is what cut the merged dial ctx,
// and even then only a Canceled error — the sentinel that bridge's own cancel produces — is mapped to ctxErr.
// A context sentinel that reaches Start some other way — a WithConnectTimeout-style dial deadline expiring on its own,
// for instance, while the caller's ctx happens to be done too for an unrelated reason —
// never sets callerCut and so is returned unchanged.
// A Start failure unrelated to the bound dial —
// a passive listen refused because the port is already in use, for instance,
// or any failure a transport's Start can still report after it has already driven TCP-up —
// is returned unchanged too, even while ctxErr happens to be non-nil for an unrelated reason.
func mapAbortedStartErr(err, ctxErr error, aborted, callerCut bool) error {
	switch {
	case aborted:
		return ErrConnClosed
	case callerCut && errors.Is(err, context.Canceled):
		return ctxErr
	default:
		return err
	}
}

// firstErr returns the first non-nil error, keeping the earlier and more specific cause.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// react is the supervisor's transition reaction (spec §5.2/§7.E).
// It runs on the supervisor goroutine and is the SOLE initiator of teardown.
// It MUST NOT block the supervisor: the farewell uses TryLock + a short write deadline,
// and teardown is the non-blocking initiator (it kicks the bounded join on a separate goroutine and returns immediately).
// This holds for the write itself and for its own control flow; it does NOT hold against a
// pathological custom net.Conn — SetWriteDeadline/Write here, and Close inside e.teardown, are
// synchronous calls into caller-supplied code, and a conn that ignores its deadline or blocks in
// Close can still block this goroutine (see the WithDialer / WithListener caller-obligation docs).
//
// It acts only on transitions INTO NotConnected (fired by both a voluntary Close's
// evClose Selected->NotConnected and an involuntary evDisconnect). Ordering: (1) bounded
// best-effort farewell Separate, (2) if !shutdown start the reconnect loop, (3) e.teardown.
func (c *connection) react(prev, next ConnState) {
	if next != NotConnectedState {
		return // reactions only matter for transitions INTO NotConnected
	}

	// Test seam (nil in production, MAY BLOCK): fires immediately before the generation resolution below.
	// A test holds it to prove that nothing else can have published a successor in the window between the FSM's own store
	// (already applied by the time react runs) and this resolution:
	// the only other publisher of a successor is the reconnect loop THIS SAME call is about to start (step 2 below),
	// so a react call for generation e's drop can only ever resolve e here, never a successor, however long that resolution is held.
	if hook := c.testHookReactBeforeCurLoad; hook != nil {
		hook()
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
	// §7.C).
	// The spawned loop's first act is connectLoopAwaitBarrier's prev.wait() on the epoch torn down just below, so it never races teardown;
	// its NEXT act is waiting for e's own startReturned barrier,
	// so it never dials — or claims the reconnect gauge — while e's own Start call (on whatever goroutine drove it) may still be in flight.
	if !c.shutdown.Load() {
		c.startConnectLoop(e, true) // any involuntary drop always counts as a reconnect, whether or not this generation ever reached Selected
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
// a wedged writer, so skipping the farewell never stalls progress.
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
// countReconnect selects whether a successful dial at the end of this loop increments the cumulative Reconnects() metric —
// true for a recovery after any involuntary drop, whether or not that generation ever reached Selected;
// false for Task 4's initial-cold-connect retry, whose OWN success must NOT count as a "reconnect"
// (see ConnectionMetrics.Reconnects: a cold background retry's own success is not counted,
// but a later recovery after its link came up and dropped is).
func (c *connection) startConnectLoop(prev *epoch, countReconnect bool) {
	gen := c.reconnectGen.Load()       // the generation THIS retry is scheduled at (the reconnect fence base)
	cancel := c.reconnectCancel.Load() // this cycle's cancel channel (closed by Close to interrupt backoff)

	c.connectLoopWg.Go(func() {
		c.connectLoop(prev, gen, cancel, countReconnect)
	})

	// Test seam (nil in production): fires immediately after the goroutine above is counted, so a
	// test can rely on the loop being launched before it releases a held predecessor Start —
	// letting it drive both "drop processed first" and "Start returns first" schedules deterministically.
	if hook := c.testHookLoopSpawned; hook != nil {
		hook()
	}
}

// connectLoopAwaitBarrier waits for prev to be FULLY torn down and joined, then for prev's own
// startReturned barrier to release, before a connectLoop invocation may claim the reconnect gauge or dial.
// It reports false when stop (Close) released the wait before the barrier did —
// in which case the caller must return without ever having claimed anything —
// and true otherwise, including when prev is nil (no predecessor to wait for at all).
func (c *connection) connectLoopAwaitBarrier(prev *epoch, stop <-chan struct{}) bool {
	if prev == nil {
		return true
	}

	_ = prev.wait()

	// Test seam (nil in production): fires immediately before the barrier select below,
	// so a test can rely on this invocation being parked here — neither channel closed yet —
	// before it samples the gauge or otherwise asserts on state this invocation has not reached.
	if hook := c.testHookConnectLoopBarrier; hook != nil {
		hook()
	}

	select {
	case <-prev.startReturned:
		return true
	case <-stop:
		return false
	}
}

// connectLoop is the reconnect state machine (spec §5.2).
// It reconnects after a single involuntary drop:
// it first waits for the prior generation to be FULLY torn down (generation-serialization)
// AND for that generation's OWN tr.Start call to have returned (the startReturned barrier, below),
// then dials with an exponential backoff (WithReconnectBackoff) capped at T5,
// re-checking shutdown and reconnectGen on every attempt and once more, under publishMu, immediately before publishing the fresh generation.
// The supervisor is NOT recreated here — only a fresh epoch per generation;
// the loop hands each generation to the persistent supervisor via tr.Start (which drives evTCPUp -> CommitSelected -> Selected).
//
// The startReturned barrier: a Start that has already reported TCP-up can still fail after react has already spawned this very invocation for the same predecessor
// (a custom transport's Start can outlive the drop it eventually reports).
// Waiting for prev.startReturned before doing anything else — claiming the reconnect gauge, computing a delay, or dialing —
// guarantees that predecessor's own Start call has returned (or been torn down for) before this invocation acts,
// so the two never dial concurrently and never race to publish a successor.
// See connectLoopStartFailure for how a generation this loop itself starts hands off to its own successor the same way.
//
// Gauge ownership: this invocation claims connection.metrics.connRetry only once it is past the barrier,
// and every terminating path from there on releases it —
// inline, before releasing its OWN epoch's startReturned barrier, on every path that reaches a Start (success or hand-off alike),
// or via the deferred release on a path that never reaches one (the shutdown fence, or a Close during the backoff sleep).
// A successor can therefore never observe two owners' shares at once.
//
// The backoff delay itself outlives this one invocation:
// it is read from and written back to connection.reconnectDelay,
// so a generation that comes up and drops again without ever reaching Selected —
// a peer that refuses every Select.req, for instance —
// keeps the ramp growing across the fresh connectLoop invocation react launches for it,
// instead of restarting at the initial delay.
func (c *connection) connectLoop(prev *epoch, gen uint64, cancel *chan struct{}, countReconnect bool) {
	// stop is reconnectCancel —
	// it lets a backoff, or the barrier below, be interrupted promptly so neither a Close nor a failed Open's own rollback has to wait either one out.
	// It is NOT the supervisor stopCh:
	// a failed Open's rollback stops the supervisor too, but through evClose, not through this channel.
	var stop <-chan struct{}
	if cancel != nil {
		stop = *cancel
	}

	// Generation-serialization: wait for the just-torn-down
	// prior epoch to be FULLY joined — its transport recv loop (via tr.Stop inside teardown) and
	// every per-generation goroutine — BEFORE dialing the next generation. Generations normally do
	// not overlap: a gen-N recv loop is gone before gen N+1 exists, so a stale gen-N evDisconnect
	// can never disconnect gen N+1. EXCEPTION (C1): a bounded tr.Stop that hits ErrCloseTimeout — a
	// data handler wedged the recv goroutine past the close timeout — ABANDONS that straggler, which
	// may outlive gen N.
	//
	// recvLoop's captured-genCtx guard usually catches that straggler,
	// which observes its own cancelled generation ctx and exits WITHOUT reporting a disconnect.
	// But that guard is an EARLY EXIT, not a fence:
	// cancellation can land between the check and the call,
	// so a sufficiently descheduled straggler still reaches the report.
	// What actually stops it from disconnecting gen N+1 is the generation identity it carries (see the e.id note below),
	// re-checked when the FSM applies the event (supervisor.step).
	// Serialization keeps generations from overlapping in the normal case;
	// the generation match is what covers this exception.
	//
	// owned tracks whether this invocation currently holds the reconnect gauge share.
	// releaseGauge is idempotent, so an inline release on a terminating Start attempt (below) and the deferred cleanup's own call never double-count.
	// The single deferred cleanup below covers EVERY return path —
	// including abandoning at the barrier, before owned is ever set true —
	// so testHookLoopExit fires regardless of how far this invocation got, always strictly before whatever gauge release applies.
	owned := false
	releaseGauge := func() {
		if owned {
			owned = false
			c.metrics.decConnRetry()
		}
	}

	// Test seam (nil in production, MAY BLOCK): runs as the very last step before this invocation returns,
	// on every path, BEFORE the gauge release below settles.
	// The failed-Open rollback join test holds it to keep a reconnect loop alive
	// while asserting that Open's own connectLoopWg.Wait() has not returned.
	defer func() {
		if hook := c.testHookLoopExit; hook != nil {
			hook()
		}

		releaseGauge()
	}()

	if !c.connectLoopAwaitBarrier(prev, stop) {
		return // Close released the wait before prev's own Start ever returned; nothing was claimed
	}

	c.metrics.incConnRetry()
	owned = true

	// Decide the starting delay now that the prior generation, if any, has fully ended.
	// A predecessor that reached Selected at least once —
	// epoch.reachedSelected, set atomically with its Select commit (see connection.selectCommitGate) —
	// resets the ramp to the configured initial delay;
	// otherwise the loop continues growing from the persisted delay (connection.reconnectDelay),
	// which is what keeps the backoff climbing across reconnects that never select.
	// A persisted value that is not yet valid (<= 0) also falls back to initial, defensively.
	delay := time.Duration(c.reconnectDelay.Load())
	if delay <= 0 || (prev != nil && prev.reachedSelected.Load()) {
		delay = c.cfg.Load().reconnectBackoffInitial
	}

	for {
		cfg := c.cfg.Load()

		sleepFor := delay
		if ceil := cfg.timers.T5; sleepFor > ceil {
			sleepFor = ceil
		}

		// Optional test hook (nil in production, zero cost).
		// Reconnect-backoff persistence tests use it to record the exact delay sequence the loop computes,
		// one call per dial attempt, before this attempt sleeps.
		if hook := c.testHookBackoff; hook != nil {
			hook(sleepFor)
		}

		// Exponential-backoff connect separation between attempts (active dial; a passive
		// re-listen is the same tr.Start call), capped at T5. Interruptible by a Close via the
		// reconnectCancel channel.
		if !c.reconnectSleep(sleepFor, stop) {
			return
		}

		delay = nextBackoffDelay(delay, cfg.reconnectBackoffMultiplier, cfg.timers.T5)
		// Persist the advanced delay so the NEXT connectLoop invocation —
		// whether this same loop's next iteration or a fresh one launched by react after this generation drops —
		// continues the ramp instead of reseeding (see connection.reconnectDelay).
		c.reconnectDelay.Store(int64(delay))

		// Optional test hook (nil in production, zero cost). The gen-fence / no-deadlock teeth
		// tests use it to pause the loop between the backoff and the fence.
		if hook := c.testHookConnectLoop; hook != nil {
			hook()
		}

		// A Close (shutdown) or a fresh Open (reconnectGen advanced) during reconnect stops
		// the loop before it spends work building a generation.
		if c.shutdown.Load() || c.reconnectGen.Load() != gen {
			return
		}

		// Build the next-generation epoch (NOT yet published). stopTransport lets the epoch's
		// teardown join this generation's transport recv loop.
		e := newEpoch(context.Background(), cfg.logger, cfg.senderQueueSize)
		e.stopTransport = c.tr.Stop
		e.genGate = &c.genGate // see the note at the Open site
		// The second and last cur publisher; see the identity note at the Open site.
		// This loop is reached only from the entering-NotConnected reaction (or Open's cold-connect retry),
		// so the generation it replaces has already ended.
		e.id = c.genSeq.Add(1)

		// The reconnect fence + publish, LINEARIZED against a voluntary Close under publishMu (I1). Re-check
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
		// drives evTCPUp -> CommitSelected -> Selected across this new generation.
		// On success the generation is live and the loop's job is done.
		if err := c.tr.Start(e.ctx, c); err != nil {
			if c.connectLoopStartFailure(e, err, cfg, releaseGauge) {
				continue
			}

			return
		}

		// This generation is live and this loop's job is done:
		// release the gauge and this generation's OWN barrier before returning,
		// so a FUTURE successor (spawned whenever this generation eventually drops) never finds both owners' shares held at once.
		releaseGauge()
		close(e.startReturned)

		if countReconnect {
			c.metrics.incReconnects()
		}

		return
	}
}

// connectLoopStartFailure handles a failed tr.Start for the generation e that connectLoop just published:
// tear it down and join that teardown FIRST, with e's own startReturned barrier still closed, so e.tcpUpCommitted is final before deciding what happened.
//
// A generation that never left NotConnected (no TCP-up commit ever succeeded on it) was a plain dial/listen failure:
// releasing e's barrier and reporting true lets connectLoop retry with this same invocation, still the sole owner of the reconnect gauge.
//
// A generation that DID leave NotConnected came up and then failed to start:
// this invocation hands the retry off to the drop reaction's own loop instead of retrying itself.
// It releases the gauge share and e's barrier — in that order, so an already-unblocked successor can never observe both owners' shares held at once —
// then reports the drop through injectDisconnect and returns false.
// If the drop was already stored (a TCPDown processed first), the report is a no-op:
// illegal from NotConnected, or stale once a successor is already current.
// Otherwise it produces the drop, and every real drop into NotConnected is guaranteed a reaction (see supervisor.step's dropped-transition handling).
// injectDisconnect can block — exactly like any other injection (see supervisor.inject),
// it is a bounded-blocking send on the events queue, not a guaranteed-non-blocking one —
// and becomes a safe no-op only once the supervisor has stopped (runDone closed).
// It still cannot deadlock against a concurrent Close:
// Close tears the connection down through requestClose/evClose on that SAME queue,
// and the supervisor's own run() loop is what drains it,
// so the two can never wait on each other.
func (c *connection) connectLoopStartFailure(e *epoch, err error, cfg *ConnectionConfig, releaseGauge func()) (retry bool) {
	// Test seam (nil in production): fires immediately on observing the failure,
	// before the teardown below — see the field doc for what this acknowledgement proves.
	if hook := c.testHookConnectLoopBeforeFailureTeardown; hook != nil {
		hook()
	}

	e.teardown(c.cfg.Load().closeTimeout) // LIVE closeTimeout (M7) — consistent with the react teardown
	_ = e.wait()

	if !e.tcpUpCommitted.Load() {
		cfg.logger.Debug("hsms: reconnect dial failed, retrying", "error", err)
		close(e.startReturned)

		return true
	}

	cfg.logger.Debug("hsms: reconnect start failed after TCP-up; handing the retry off to the drop reaction's own loop", "error", err)

	releaseGauge()
	close(e.startReturned)

	// Test seam (nil in production): lets a test hold this invocation here, after the barrier has released,
	// so an already-unblocked successor can run to publication BEFORE the report below —
	// exercising injectDisconnect's stale-generation no-op path deterministically.
	if hook := c.testHookBeforeHandoffInject; hook != nil {
		hook()
	}

	c.injectDisconnect(e.id, err, CauseUnknown)

	return false
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

// genCapability is the generation-aware back-channel this core offers; see gencap.GenerationRuntime.
// The assertion below makes a method that *connection loses a build failure
// instead of a silent fallback on the hsmsss side.
type genCapability = gencap.GenerationRuntime[Message, TransitionCause]

var _ genCapability = (*connection)(nil)

// dialCapability is the dial-bounding back-channel this core offers; see gencap.DialBounder.
type dialCapability = gencap.DialBounder

var _ dialCapability = (*connection)(nil)

// BoundDial merges dialCtx with the caller ctx and abort channel of the Open whose ORIGINAL Start
// ctx is startCtx.
//
// startCtx is compared against the published firstDialState by IDENTITY, not value: a reconnect
// generation's Start carries a DIFFERENT epoch ctx, so this returns dialCtx unchanged for it —
// even while a firstDial from an unrelated, still-in-flight Open happens to be published, since that
// published firstDial belongs to a different epoch than the one this Start call is dialing for.
// The identity comparison alone is what makes this safe: it holds regardless of any ordering between
// a reconnect generation's dial and any other Open's Start call, or of when that other Open's own
// generation began or ended relative to this one.
//
// The merged ctx is cancelled when dialCtx is, when the caller's ctx is, or when the Open is
// aborted by a concurrent Close. Neither the AfterFunc bridge nor the abort-bridge goroutine is
// joined by the returned cancel func — both are confined to the dial: they exit promptly once the
// merged ctx is cancelled, so nothing here can leak past the one dial call that owns this ctx.
// The AfterFunc bridge also marks fd.callerCut before it cancels, recording that THIS bridge — not dialCtx's own deadline, and not the abort — is what cut the merged ctx;
// mapAbortedStartErr reads that flag once Start returns.
func (c *connection) BoundDial(startCtx, dialCtx context.Context) (context.Context, context.CancelFunc) {
	fd := c.firstDial.Load()
	if fd == nil || fd.epochCtx != startCtx {
		return dialCtx, func() {}
	}

	merged, cancel := context.WithCancel(dialCtx)
	stopCaller := context.AfterFunc(fd.callerCtx, func() {
		fd.callerCut.Store(true)
		cancel()
	})

	// Bridge the abort channel into the merged ctx's cancellation.
	// It exits on its own, without being joined,
	// the instant merged is cancelled by any means (dialCtx, the caller ctx via stopCaller, or this select's own abort case) —
	// see the doc comment above.
	go func() {
		select {
		case <-fd.abort:
			cancel()
		case <-merged.Done():
		}
	}()

	return merged, func() {
		stopCaller()
		cancel()
	}
}

// TCPUp is called by the transport when a TCP connection is established (TransportRuntime).
//
// It publishes the socket on the current epoch and then attempts to advance the FSM NotConnected -> NotSelected SYNCHRONOUSLY via a guarded CAS
// (CommitConnected, symmetric with CommitSelected), which — on a committed CAS — also enqueues evTCPUp for the deduped entering-NotSelected reaction/notify.
// The socket is published BEFORE the supervisor call so it is visible before State() flips to NotSelected.
//
// TCPUp carries no generation identity,
// so publishing the socket is unconditional whenever a current epoch exists at all — the same pre-generation behavior this entry point has always had.
// The FSM commit that follows is different:
// it is refused when the current generation's teardown has already begun,
// or when that generation already admitted a TCP-up report, even at this unnamed call site — see [connection.tcpUpCommitGate].
// A refused commit still costs nothing beyond the diagnostic accounting for the FSM itself:
// publishSocket has already run by then, with no liveness check at gen 0 (see [connection.publishSocket]),
// so a report racing a generation whose own teardown already closed its socket can still re-populate that socket,
// and nothing closes it afterward — a pre-existing limitation of this unnamed path, tracked as a follow-up,
// not something the FSM-commit refusal above fixes.
// [connection.TCPUpFromGeneration] is the generation-aware counterpart an in-module transport uses instead,
// and it DOES report whether conn was accepted at the publish step.
func (c *connection) TCPUp(conn net.Conn) {
	c.commitTCPUp(0, conn)
}

// TCPUpFromGeneration is TCPUp reported ON BEHALF OF gen, the generation whose socket came up.
//
// It reports whether conn was accepted onto a live generation.
// On false, gen has already ended — no epoch will ever take ownership of conn,
// so the caller owns it and must close it;
// there is nothing else for the caller to do
// (no recv loop to spawn, no activity stamps to reset — the generation this socket belonged to is gone).
//
// The passive accept goroutine
// (and, more narrowly, an active dial racing a concurrent teardown to completion)
// are the producers this exists for.
// The generation's Stop is bounded.
// A goroutine descheduled between the accept/dial returning and this call can therefore be abandoned,
// and resume after the generation it belongs to is over.
// Reporting the generation keeps such a straggler from publishing a dead socket on a live epoch
// and from resurrecting NotSelected on a connection that has already gone down or been closed.
//
// A gen of 0 skips the match and behaves exactly like TCPUp — always accepted, so the return is always true.
func (c *connection) TCPUpFromGeneration(gen uint64, conn net.Conn) bool {
	return c.commitTCPUp(gen, conn)
}

// commitTCPUp is the shared body of the TCP-up back-channel:
// publish the socket on the generation that owns it, then commit NotConnected -> NotSelected for that generation.
//
// The socket is published BEFORE the supervisor call so it is visible before State() flips to NotSelected.
//
// It reports publishSocket's accept/refuse decision,
// NOT whether the FSM CAS that follows actually flipped the state —
// those two can diverge
// (a teardown can land between the two calls and refuse the CAS on a socket publishSocket already accepted),
// but for a NAMED generation the divergence is harmless:
// publishSocket's own liveness check (under genGate.RLock) refuses the store once that generation's teardown has begun,
// so the epoch never takes ownership of a socket its own teardown has already closed.
// At gen 0, though, publishSocket has no such check (see [connection.publishSocket]):
// a report landing after this generation's own teardown already ran closeSocket can still re-populate its socket,
// and nothing closes it afterward — the same pre-existing, unnamed-path limitation [connection.TCPUp] documents.
// A refusal at publishSocket itself — the named-generation case — is the only case where no epoch ever takes ownership,
// which is what the return value exists to let the caller detect.
//
// The FSM commit is still attempted UNCONDITIONALLY, even when publishSocket already refused:
// tcpUpCommitGate applies the identical {id, ended} liveness check on its own RLock section and,
// finding the same generation already gone, counts it as a stale commit (staleGen) and logs it —
// the diagnostic this back-channel has always produced for a report naming a dead generation.
// The commit itself is a guaranteed no-op in that case
// (once ended latches true it never reverts,
// so a generation publishSocket already refused can never pass tcpUpCommitGate either),
// so this costs nothing beyond the accounting.
func (c *connection) commitTCPUp(gen uint64, conn net.Conn) bool {
	accepted := c.publishSocket(gen, conn)

	if s := c.sup.Load(); s != nil {
		// CauseLocalOpen: reaching NotSelected means OUR Open (or the reconnect loop it owns)
		// established the link, whether by dialing out or by accepting the peer we were listening for.
		s.CommitConnectedFromGeneration(gen, CauseLocalOpen)
	}

	return accepted
}

// publishSocket publishes conn as the socket of the generation that established it,
// and reports whether it did.
//
// A named generation is resolved by identity and checked for liveness under the generation gate,
// for the same reason the FSM commit that follows is:
// an abandoned accept goroutine must not hang a dead socket on an epoch whose teardown already closed its own.
// Holding the gate across the store is what keeps that from slipping through between the check and the store —
// teardown latches ended before it closes the socket, and it latches it under this same gate.
//
// A gen of 0 publishes unconditionally (true whenever a current epoch exists),
// exactly as this path did before generations were carried.
func (c *connection) publishSocket(gen uint64, conn net.Conn) bool {
	if gen == 0 {
		if e := c.cur.Load(); e != nil {
			e.setConn(conn)

			return true
		}

		return false
	}

	c.genGate.RLock()
	defer c.genGate.RUnlock()

	if e := c.cur.Load(); e != nil && e.id == gen && !e.ended.Load() {
		e.setConn(conn)

		return true
	}

	return false
}

// liveEpoch resolves gen to its epoch, or nil when gen is not the live generation or has already ended.
//
// It is the read-only half of commitGate's decision, for a caller that has no CAS to perform under the gate:
// the generation-bound async send resolves its target epoch here, then enqueues on THAT epoch after the gate is released.
// Returning the epoch rather than a bool is what makes the release safe —
// the caller never re-reads c.cur, so a swap after the unlock cannot redirect its work onto a successor.
//
// Nothing that can block runs under the gate.
func (c *connection) liveEpoch(gen uint64) *epoch {
	c.genGate.RLock()
	defer c.genGate.RUnlock()

	if e := c.cur.Load(); e != nil && e.id == gen && !e.ended.Load() {
		return e
	}

	return nil
}

// commitGate fences one generation-guarded synchronous FSM commit against the end of the generation that asked for it.
//
// It reports whether the CAS committed, and whether gen was live at all.
// The caller needs the two apart:
// "already in the target state" and "your generation is over" are different answers,
// and only the second one is a discarded commit.
//
// The RLock spans the whole decision: resolve the live generation, verify it is gen and that gen has not ended,
// and run the CAS.
// epoch.markEnded takes the same gate for writing at the top of teardown,
// so this section is ordered wholly before or wholly after the end of any generation.
// Observing ended false therefore means the CAS lands while gen is still the working generation —
// no successor can exist yet, because every successor publish is downstream of a teardown that has already latched ended.
//
// Nothing that can block runs under the gate: the log below is deliberately emitted after the unlock.
func (c *connection) commitGate(gen uint64, cas func() bool) (committed, live bool) {
	c.genGate.RLock()

	e := c.cur.Load()
	live = e != nil && e.id == gen && !e.ended.Load()
	if live {
		committed = cas()
	}

	c.genGate.RUnlock()

	if !live {
		var current uint64
		if e != nil {
			current = e.id
		}

		c.cfg.Load().logger.Debug("hsms: dropped a state commit requested by a generation that is no longer live",
			"reported_generation", gen, "current_generation", current)
	}

	return committed, live
}

// selectCommitGate is commitGate's counterpart for the Select-accepted commit only.
//
// It fences the same way —
// one genGate.RLock section spanning {resolve the live generation, verify liveness, run the CAS} —
// but ALSO differs from commitGate in the one place the Select commit needs to: a gen of 0 is NOT bypassed.
// secs1, and any transport that uses only TransportRuntime, never carry a generation identity,
// so a named-generation check alone would leave every one of their Select commits ungated;
// instead, a gen of 0 skips ONLY the identity comparison,
// and the liveness requirement still applies — a non-nil, un-torn-down current generation.
// A named generation (gen != 0, the hsmsss path) keeps requiring an exact identity match, exactly like commitGate.
//
// On a successful commit it also latches epoch.reachedSelected on the SAME generation the RLock section validated,
// before releasing the lock —
// the reconnect loop's backoff-reset read (connectLoop, connection.reconnectDelay)
// depends on the marker landing on the exact generation whose commit set it,
// never on a successor a concurrent teardown-and-republish could otherwise let it fall onto.
//
// Nothing that can block runs under the gate: the log below is deliberately emitted after the unlock,
// exactly like commitGate.
func (c *connection) selectCommitGate(gen uint64, cas func() bool) (committed, live bool) {
	c.genGate.RLock()

	e := c.cur.Load()
	live = e != nil && !e.ended.Load() && (gen == 0 || e.id == gen)
	if live {
		committed = cas()
		if committed {
			e.reachedSelected.Store(true)
		}
	}

	c.genGate.RUnlock()

	if !live {
		var current uint64
		if e != nil {
			current = e.id
		}

		c.cfg.Load().logger.Debug("hsms: dropped a select commit requested by a generation that is no longer live",
			"reported_generation", gen, "current_generation", current)
	}

	return committed, live
}

// tcpUpCommitGate is commitGate's counterpart for the TCP-up commit only.
//
// It fences the same way —
// one genGate.RLock section spanning {resolve the live generation, verify liveness, run the CAS} —
// but ALSO differs from commitGate in the one place the TCP-up commit needs it to: a gen of 0 is NOT bypassed,
// for the same reason selectCommitGate does not bypass it —
// SECS-I, and any transport that uses only TransportRuntime, never carry a generation identity,
// so a named-generation check alone would leave every one of their TCP-up reports ungated.
// A gen of 0 skips ONLY the identity comparison; the liveness requirement — a non-nil,
// un-torn-down current generation — still applies.
//
// It additionally admits AT MOST ONE TCP-up per generation:
// e.tcpUpAdmitted.CompareAndSwap(false, true) inside the same RLock section refuses a second report
// on a generation that already admitted one, even when the first report's own CAS failed.
// The admission CAS and the state CAS both run under the same RLock section, and a live generation cannot end while that section holds,
// so concurrent RLock holders can never both pass the admission CAS.
//
// On a commit that both admission and the CAS accept,
// it latches epoch.tcpUpCommitted on the SAME generation the RLock section validated, before releasing the lock —
// connectLoop's Start-failure branch depends on that marker landing on the exact generation whose commit set it,
// so it can tell a plain dial failure (the FSM never left NotConnected) from a link that came up and then failed to start
// (see connectLoop).
//
// A TCP-up reported after the current generation's teardown has already begun,
// or one reported after the generation already admitted an earlier TCP-up,
// is refused here rather than moving a dying or already-dropped link back into NotSelected —
// see the TransportRuntime.TCPUp doc for the resulting behavior change.
//
// Nothing that can block runs under the gate: the log below is deliberately emitted after the unlock,
// exactly like commitGate and selectCommitGate.
// A refused second report on a still-live generation logs the same way, distinctly from a refusal on a dead one;
// commitFrom counts staleGen only for the latter (live is still true here), so this refusal is a diagnostic only.
func (c *connection) tcpUpCommitGate(gen uint64, cas func() bool) (committed, live bool) {
	c.genGate.RLock()

	e := c.cur.Load()
	live = e != nil && !e.ended.Load() && (gen == 0 || e.id == gen)

	var admitted bool
	if live {
		admitted = e.tcpUpAdmitted.CompareAndSwap(false, true)
		if admitted {
			committed = cas()
			if committed {
				// Test seam (nil in production, MAY BLOCK): fires here, still holding genGate.RLock,
				// after the CAS above and before the marker store below — see the field doc.
				if hook := c.testHookTCPUpCommitBeforeMarker; hook != nil {
					hook()
				}

				e.tcpUpCommitted.Store(true)
			}
		}
	}

	c.genGate.RUnlock()

	if !live {
		var current uint64
		if e != nil {
			current = e.id
		}

		c.cfg.Load().logger.Debug("hsms: dropped a TCP-up commit requested by a generation that is no longer live",
			"reported_generation", gen, "current_generation", current)
	} else if !admitted {
		c.cfg.Load().logger.Debug("hsms: dropped a TCP-up commit because this generation already admitted an earlier one",
			"reported_generation", gen, "current_generation", e.id)
	}

	return committed, live
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
// It carries NO generation identity;
// injectDisconnect binds it, at report time, to whichever generation this call finds current (see injectDisconnect).
// A transport that can outlive the generation it belongs to should report through
// [connection.TCPDownFromGeneration] instead, which is generation-isolated from an earlier point still:
// the generation the transport itself captured at its own Start, not merely whichever one is current when it happens to call.
func (c *connection) TCPDownWithCause(cause error, transitionCause TransitionCause) {
	c.injectDisconnect(0, cause, transitionCause)
}

// CurrentGeneration returns the identity of the generation cur currently points at, or 0 when there is none.
//
// It names the generation that is CURRENT, not one guaranteed still live:
// during the reconnect window between a drop and its successor's publish,
// cur still points at the generation that just ended, and this returns that ended generation's own id, not 0.
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
// The shutdown join is bounded by the close timeout in the ordinary case —
// a straggler task goroutine still running when that deadline passes is abandoned rather than awaited further —
// though a custom net.Conn, DialFunc, ListenFunc, or Logger that itself ignores deadlines or blocks can still delay the join beyond that bound
// (see the WithDialer / WithListener caller-obligation docs).
// Either way, a goroutine wedged past the close timeout is abandoned and may resume at any later time,
// by which point a successor generation can already be selected.
// Because the plain TCPDown entry points resolve the current generation at CALL time,
// such a straggler would otherwise mark the successor's socket as failed and drop a link it knows nothing about.
//
// Reporting the generation makes that impossible in two places.
// Here, a report whose generation is not the CURRENT one is rejected by identity mismatch and counted as stale —
// an ended-but-current generation is still admissible,
// which is what the named hand-off report relies on when it reports after its own teardown.
// A rejected report does not touch the successor's comms-failure flag,
// which decides whether that generation still sends a courtesy Separate when it closes.
// And on the event itself: the generation travels with the queued event and is re-checked when the FSM PROCESSES it,
// which is what closes the window between this check and the injection (see supervisor.step).
//
// A gen of 0 skips this identity check — there is nothing to compare against —
// and is instead bound to the generation current at report time (the single cur.Load() inside injectDisconnect),
// exactly like TCPDownWithCause.
func (c *connection) TCPDownFromGeneration(gen uint64, cause error, transitionCause TransitionCause) {
	c.injectDisconnect(gen, cause, transitionCause)
}

// injectDisconnect is the shared body of the TCP-down back-channel: mark the generation as a comms failure
// (so the entering-NotConnected reaction sends no farewell Separate) and inject evDisconnect.
//
// gen is the reporting generation's identity, or 0 when the caller did not name one.
// An unnamed report is bound here, at report time, to e — the generation this call finds current — before the event is queued.
// That binding is what lets step honor or discard it against THAT generation,
// no matter how long the event then sits queued before the FSM gets to process it (see injectFrom).
// Without it, an unnamed report queued while its own generation is already over, but not yet processed,
// would be matched against whatever generation is current BY THEN instead —
// a successor that generation's own drop had nothing to do with.
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

	if gen == 0 {
		gen = e.id
	}

	if s := c.sup.Load(); s != nil {
		s.injectFrom(gen, evDisconnect, transitionCause)
	}
}
