package hsms

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
)

// Default channel capacities for a supervisor created via newSupervisor. events is the
// GUARANTEED command queue (inject blocks, never drops); notify is the best-effort,
// drop-OLDEST state-notification buffer.
const (
	supervisorEventsCap = 16
	supervisorNotifyCap = 16
)

// supervisorFallbackCloseTimeout bounds the evClose ensure-teardown when no closeTimeout provider
// is installed (unit tests that never set s.closeTimeout). Production always installs a live
// provider via the connection (M7), so this fallback is a test-only safety net.
const supervisorFallbackCloseTimeout = 10 * time.Second

// fsmEvent is the unexported internal event that drives the E37 logical FSM. The transport
// never names a raw event constant — transport-detected transitions enter through the named
// TransportRuntime methods (TCPUp/TCPDown/CommitSelected/SelectLost) — so this type stays
// unexported (spec §5.3/§5.4).
type fsmEvent uint8

const (
	evTCPUp          fsmEvent = iota // TCP came up: NotConnected -> NotSelected
	evSelectAccepted                 // Select accepted: NotSelected -> Selected (and Selected -> Selected, H2)
	evSelectLost                     // Select lost: Selected -> NotSelected
	evDisconnect                     // TCP dropped: Selected/NotSelected -> NotConnected
	evClose                          // voluntary Close: any state -> NotConnected
	evT7Timeout                      // T7 NOT-SELECTED dwell expired: NotSelected -> NotConnected (no-op otherwise)
)

// stateChange is one logical E37 transition, reported to the notifier as (prev -> next).
type stateChange struct {
	prev ConnState
	next ConnState
}

// supervisor is the single E37 logical FSM (SEMI E37 §5.4–§5.6). It is created FRESH per
// Open and stopped in Close (no channel reuse); its lifetime spans the whole Open/Close
// cycle — including reconnect generations — so its run()/notifier() goroutines are gated by
// the connection-owned stopCh, NOT an epoch ctx (an involuntary disconnect cancels an epoch
// but MUST NOT kill the supervisor — spec §5.3).
//
// run() is the SOLE writer of state for every transition EXCEPT TWO synchronous-CAS commits:
// CommitConnected (NotConnected -> NotSelected, the TCP-up commit) and CommitSelected (NotSelected
// -> Selected, the H2/§7.D Select-responder commit). Each CAS-stores its target state directly and
// then enqueues its event (evTCPUp / evSelectAccepted) for the supervisor's dedup'd reaction/notify.
//
// The supervisor has NO indefinite blocking point: every send it makes is non-blocking
// (notify uses drop-OLDEST coalescing), so a concurrent Close()'s inject(evClose) always
// makes progress (spec §5.3, Codex rounds 4-5).
type supervisor struct {
	state         atomic.Uint32              // stores a ConnState; lock-free hot-path reads + State()
	lastReacted   ConnState                  // run-owned; dedups reactions/notify (H3; tolerates the H2 pre-commit)
	closed        bool                       // run-owned; LATCHED true once evClose is processed (I2) — later events ignored
	events        chan fsmEvent              // SOLE reader is run(); GUARANTEED command queue (inject blocks, never drops)
	notify        chan stateChange           // SOLE sender is run(); NON-BLOCKING drop-OLDEST coalescing
	droppedNotify atomic.Uint64              // count of coalesced/dropped notifications; surfaced via a rate-limited Warn (M4)
	react         func(prev, next ConnState) // for a transition INTO NotConnected: farewell decision + teardown init
	closeEpoch    atomic.Pointer[epoch]      // set by requestClose(e) BEFORE evClose; the epoch to ensure-tear-down
	stopCh        chan struct{}              // closed by stop() (from Close, AFTER e.wait()) -> run() exits
	runDone       chan struct{}              // closed when run() returns; makes inject a safe no-op after stop
	stopOnce      sync.Once                  // guards close(stopCh) so stop() is idempotent
	// closeTimeout bounds the evClose ensure-teardown of the pinned epoch (§5.2/§7.A). It is a
	// provider (not a captured value) so the teardown reads the LIVE config: a mid-session
	// UpdateConfigOptions(WithCloseTimeout) is honored, matching the connection's other teardown
	// sites which all read c.cfg.Load().closeTimeout (M7). Nil defaults to supervisorFallbackCloseTimeout.
	closeTimeout func() time.Duration
	// logger surfaces the Warn when notify coalesces (drops) an intermediate state change under a
	// stalled handler (M4). Set post-construction by the connection; nil in unit tests that do not
	// assert on the Warn (reportDrops nil-guards it). The Warn is emitted from the NOTIFIER
	// goroutine (reportDrops), never the run() goroutine, so a blocking logger cannot stall the FSM.
	logger logger.Logger
	// lastLoggedDropped is owned SOLELY by the notifier goroutine (reportDrops); it edge-triggers
	// the drop Warn so a single stall-burst logs once, not once per coalesced notification.
	lastLoggedDropped uint64

	// testHookAfterStateLoad, when non-nil, is invoked by step() immediately after it loads state and
	// before the transition/store — a test seam to deterministically interpose a concurrent
	// CommitSelected and exercise the evT7Timeout CAS tie. Always nil in production.
	testHookAfterStateLoad func(ev fsmEvent)

	// handlers are NOT owned by the supervisor: they live on the Connection (an atomic.Pointer
	// to an immutable slice) so they persist across Open/Close cycles while the supervisor is
	// recreated per Open. The per-Open supervisor only reads this pointer.
	handlers *atomic.Pointer[[]StateChangeHandler]
}

// newSupervisorWithEventsCap builds a supervisor with an explicit events-queue capacity
// (used by tests to make the guaranteed-command-queue behavior deterministic); the notify
// buffer keeps the default capacity. The caller installs the live closeTimeout provider (M7)
// and logger (M4) as post-construction fields; both are optional (safe defaults / nil-guard).
func newSupervisorWithEventsCap(react func(prev, next ConnState), handlers *atomic.Pointer[[]StateChangeHandler], eventsCap int) *supervisor {
	return &supervisor{
		state:         atomic.Uint32{},
		lastReacted:   NotConnectedState,
		events:        make(chan fsmEvent, eventsCap),
		notify:        make(chan stateChange, supervisorNotifyCap),
		droppedNotify: atomic.Uint64{},
		react:         react,
		closeEpoch:    atomic.Pointer[epoch]{},
		stopCh:        make(chan struct{}),
		runDone:       make(chan struct{}),
		stopOnce:      sync.Once{},
		handlers:      handlers,
	}
}

// newSupervisor builds a fresh supervisor for one Open/Close cycle. react is invoked for
// each deduped logical transition (non-blocking — it only SCHEDULES teardown, never Waits);
// handlers points at the Connection's persistent StateChangeHandler slice. The caller sets the
// live closeTimeout provider (M7) and logger (M4) on the returned supervisor.
func newSupervisor(react func(prev, next ConnState), handlers *atomic.Pointer[[]StateChangeHandler]) *supervisor {
	return newSupervisorWithEventsCap(react, handlers, supervisorEventsCap)
}

// transition is the pure E37 §5.4–§5.6 state table. It returns the next state and whether
// the (cur, ev) pair is a legal transition; an illegal pair yields (cur, false) so the run
// loop can treat it as a safe no-op.
//
// evTCPUp is legal from BOTH NotConnected AND NotSelected (the latter tolerates the CommitConnected
// pre-commit — spec §7.D), mirroring evSelectAccepted. evSelectAccepted is legal from BOTH
// NotSelected AND Selected (the latter tolerates the H2 pre-commit in CommitSelected — spec §7.D).
// evClose is legal from ALL THREE states, all -> NotConnected, so Close cannot hang while dialing /
// waiting for a passive peer.
//
// evT7Timeout (the T7 NOT-SELECTED dwell expiry, §9.2.2) is legal ONLY from NotSelected
// (-> NotConnected) and is a NO-OP from Selected/NotConnected — so a session that reached Selected
// before T7 expiry is NEVER torn down by a stale T7. This no-op-from-Selected clause is the
// load-bearing guard (see TestSupervisor_T7TimeoutFromSelectedIsNoOp); unlike evDisconnect, it does
// NOT fire from Selected. The entering-NotConnected store for this event is additionally CAS-guarded
// in step() (not a plain Store), so a concurrent CommitSelected that wins the nanosecond tie between
// step()'s state.Load() and its store is honored — the stale T7 disconnect is abandoned and the
// committed Selected session survives (see TestSupervisor_T7TimeoutLosesTieToCommitSelected).
func transition(cur ConnState, ev fsmEvent) (ConnState, bool) {
	switch ev {
	case evTCPUp:
		if cur == NotConnectedState || cur == NotSelectedState {
			return NotSelectedState, true
		}
	case evSelectAccepted:
		if cur == NotSelectedState || cur == SelectedState {
			return SelectedState, true
		}
	case evSelectLost:
		// NotSelected is a legal (no-op-store) entry — symmetric with evTCPUp/evSelectAccepted — so
		// the synchronous CommitSelectLost pre-commit (Selected->NotSelected) still fires the deduped
		// entering-NotSelected reaction when step() later processes the injected evSelectLost (I3).
		if cur == SelectedState || cur == NotSelectedState {
			return NotSelectedState, true
		}
	case evDisconnect:
		if cur == SelectedState || cur == NotSelectedState {
			return NotConnectedState, true
		}
	case evT7Timeout:
		if cur == NotSelectedState {
			return NotConnectedState, true
		}
	case evClose:
		return NotConnectedState, true
	default:
		// unknown event: no transition
	}

	return cur, false
}

// State returns the current logical E37 state via a lock-free atomic read.
func (s *supervisor) State() ConnState {
	return ConnState(s.state.Load())
}

// CommitConnected performs the synchronous TCP-up commit (symmetric with CommitSelected / §7.D):
// a guarded CAS NotConnected -> NotSelected directly on state, making State()==NotSelected
// immediately (before the recv loop / active Select procedure runs) so a Select.req dispatched
// right after TCP-up finds NotSelected and CommitSelected's CAS succeeds — with NO async poll-fence.
// On a successful commit it enqueues evTCPUp so the supervisor fires the entering-NotSelected
// reaction/notify EXACTLY ONCE (deduped on lastReacted, tolerating the pre-committed state via the
// evTCPUp-from-NotSelected table entry). It returns whether THIS call performed the commit; a call
// when not NotConnected is a no-op returning false (TCPUp is driven once per generation, and the
// only transition out of NotConnected is evTCPUp itself, so the CAS always succeeds in practice).
func (s *supervisor) CommitConnected() (committed bool) {
	if s.state.CompareAndSwap(uint32(NotConnectedState), uint32(NotSelectedState)) {
		s.inject(evTCPUp)

		return true
	}

	return false
}

// CommitSelected performs the H2 §7.D synchronous responder commit: a guarded CAS
// NotSelected -> Selected directly on state, making IsSelected() true immediately (before
// the responder writes Select.rsp) so data pipelined right after Select.rsp is not spuriously
// Rejected. On a successful commit it enqueues evSelectAccepted so the supervisor fires the
// entering-Selected reaction/notify EXACTLY ONCE (deduped on lastReacted, tolerating the
// pre-committed state). It returns whether THIS call performed the commit; a call when already
// Selected is a no-op returning false.
func (s *supervisor) CommitSelected() (committed bool) {
	if s.state.CompareAndSwap(uint32(NotSelectedState), uint32(SelectedState)) {
		s.inject(evSelectAccepted)

		return true
	}

	return false
}

// CommitSelectLost performs the synchronous Selected -> NotSelected commit (symmetric with
// CommitSelected / §7.D): a guarded CAS Selected -> NotSelected directly on state, making
// State()==NotSelected IMMEDIATELY. Without it, SelectLost was an async inject: after a Deselect.req
// the state stayed Selected until run() processed the event, so a peer that pipelined a re-Select.req
// had its CommitSelected CAS fail (still Selected) yet was still answered Select.rsp status-0 — told
// "selected" without a real commit (I3, the efb220b class via the Deselect door). Committing here on
// the recv goroutine closes that window. On a successful commit it enqueues evSelectLost so the
// supervisor fires the entering-NotSelected reaction/notify EXACTLY ONCE (deduped on lastReacted,
// tolerating the pre-committed state via the evSelectLost-from-NotSelected table entry). It returns
// whether THIS call performed the commit; a call when not Selected is a no-op returning false.
func (s *supervisor) CommitSelectLost() (committed bool) {
	if s.state.CompareAndSwap(uint32(SelectedState), uint32(NotSelectedState)) {
		s.inject(evSelectLost)

		return true
	}

	return false
}

// run is the single writer for async transitions. Its lifetime is the whole Open/Close cycle
// (NOT an epoch): it selects on stopCh (closed by stop()) and events. There is deliberately
// NO ctx.Done()->evClose synthesis (Codex round-6) — evClose is ONLY ever the pinned one from
// requestClose(e), so the evClose handler's closeEpoch.Load().teardown() is never a nil deref
// and an involuntary epoch-ctx cancellation never kills the supervisor. Closing notify and
// runDone on return drains the notifier (H4/H5) and makes inject a safe no-op after stop.
func (s *supervisor) run() {
	defer close(s.notify)
	defer close(s.runDone)

	for {
		select {
		case <-s.stopCh:
			return
		case ev := <-s.events:
			s.step(ev)
		}
	}
}

// step applies one event: it reads the current state, applies the pure transition (illegal
// pairs are safe no-ops), stores state only when it actually changed (a no-op when
// CommitSelected already pre-stored — H2), and fires the deduped reaction/notify keyed on
// lastReacted (H3). Two events are guarded against a concurrent synchronous CommitSelected: the
// evT7Timeout store is a CAS(cur -> next) (not a plain Store), and evSelectLost is abandoned when the
// state is observed Selected (a pipelined re-Select re-committed after CommitSelectLost's CAS) — in
// both cases step early-returns and leaves the committed Selected session intact rather than tearing
// it down / flapping it (§9.2.2). Regardless of the
// transition, evClose additionally and unconditionally
// initiates teardown of the PINNED epoch (idempotent closeOnce) so a Close while already
// NotConnected — where no transition fires — still initiates teardown and Close's e.wait()
// cannot hang (spec §5.3). closeEpoch is nil-guarded so a raw evClose (no requestClose) is a
// safe no-op.
func (s *supervisor) step(ev fsmEvent) {
	// I2: once evClose has been processed the supervisor is LATCHED closed — every later event is a
	// no-op. This closes the Close-vs-reconnect-Start race where an evTCPUp queued behind evClose
	// (NotConnected -> NotSelected is a legal table entry) would resurrect NotSelected AFTER Close,
	// leaving State() misreporting and suppressing the terminal NotConnected. requestClose is only
	// ever terminal (Close / failed-Open rollback, both under lifeMu), so latching cannot drop a
	// legitimate later transition — the generation is ending.
	if s.closed {
		return
	}

	cur := ConnState(s.state.Load())

	// Test seam (T24b): lets a test deterministically interpose a concurrent CommitSelected between
	// the state.Load() above and the evT7Timeout CAS below, exercising the tie the CAS closes. nil in
	// production (one nil-checked call per event step).
	if s.testHookAfterStateLoad != nil {
		s.testHookAfterStateLoad(ev)
	}

	// NEW-2 (I3 supersession): evSelectLost is enqueued ONLY after CommitSelectLost has already CAS'd
	// Selected -> NotSelected. If step now observes Selected, a concurrent CommitSelected (a peer that
	// pipelined Deselect.req -> Select.req) re-committed AFTER that CAS — the SelectLost is stale and
	// superseded. ABANDON it: a plain Store of NotSelected here would clobber the valid Selected and
	// flap the FSM (spuriously Rejecting a legitimately-selected peer's next frame, the efb220b class).
	// Same supersession rationale as the evT7Timeout CAS below.
	if ev == evSelectLost && cur == SelectedState {
		return
	}

	if next, ok := transition(cur, ev); ok {
		if next != cur {
			// evT7Timeout is the ONLY transition-store that can race a concurrent SYNCHRONOUS
			// CommitSelected CAS: all other events are the supervisor's own serial transitions, and
			// evClose SHOULD win. A plain Store here would clobber a Select that committed between
			// our state.Load() above and this store (a nanosecond tie), tearing down a validly-
			// Selected session — the invariant §9.2.2 forbids. Guard it with CAS(cur -> next): if a
			// CommitSelected won the tie (state is no longer `cur`), the CAS fails and we ABANDON the
			// stale T7 disconnect; the session stays Selected and its evSelectAccepted fires the
			// entering-Selected reaction. This makes "never torn down by a stale T7" hold BY
			// CONSTRUCTION, with no TOCTOU.
			if ev == evT7Timeout {
				if !s.state.CompareAndSwap(uint32(cur), uint32(next)) {
					return // concurrent commit changed state; the T7 disconnect is stale — abandon it
				}
			} else {
				s.state.Store(uint32(next))
			}
		}

		if next != s.lastReacted {
			s.fireTransition(s.lastReacted, next)
			s.lastReacted = next
		}
	}

	if ev == evClose {
		// Latch closed (I2) BEFORE teardown: no event queued behind this evClose may move state again.
		s.closed = true
		if e := s.closeEpoch.Load(); e != nil {
			e.teardown(s.resolveCloseTimeout())
		}
	}
}

// fireTransition emits the notification and calls react for one deduped transition. The
// reported prev is lastReacted (NOT the atomic's current value — H2/H3), so a pre-committed
// entering-Selected is still reported as (NotSelected -> Selected). For a terminal
// NotConnected transition the notify is EMITTED BEFORE react (the F1 ordering guarantee: the
// terminal state is enqueued before react may initiate teardown that stops the notifier);
// for any other transition react runs first, then emit.
func (s *supervisor) fireTransition(prev, next ConnState) {
	if next == NotConnectedState {
		s.emit(stateChange{prev: prev, next: next})
		s.react(prev, next)

		return
	}

	s.react(prev, next)
	s.emit(stateChange{prev: prev, next: next})
}

// emit is a NON-BLOCKING drop-OLDEST send onto notify. The supervisor is the SOLE sender, so
// on a full buffer it drops the oldest buffered notification to make room and then sends —
// after dropping one (or observing the consumer drained), a free slot is guaranteed, so the
// final send never blocks. Drop-oldest guarantees the LATEST state always reaches the handler
// eventually (only intermediate transitions may coalesce under a stalled consumer). The
// supervisor NEVER blocks here, so a concurrent Close()'s inject(evClose) always makes
// progress (spec §5.3, Codex rounds 4-5).
func (s *supervisor) emit(sc stateChange) {
	select {
	case s.notify <- sc:
	default:
		select {
		case <-s.notify:
		default:
		}
		// Count the coalesced (dropped) intermediate state change; the diagnostic Warn is emitted
		// by the notifier goroutine (reportDrops), NOT here — logging on the supervisor run()
		// goroutine could block on a user-supplied logger and stall a concurrent Close's evClose,
		// reintroducing the C1 hang this emit is contractually forbidden from causing (M4/P1-B).
		s.droppedNotify.Add(1)
		s.notify <- sc
	}
}

// resolveCloseTimeout returns the LIVE evClose teardown bound from the installed provider (M7),
// falling back to supervisorFallbackCloseTimeout when no provider is set (unit tests).
func (s *supervisor) resolveCloseTimeout() time.Duration {
	if s.closeTimeout != nil {
		return s.closeTimeout()
	}

	return supervisorFallbackCloseTimeout
}

// inject enqueues a command onto events. It is a GUARANTEED (blocking-bounded) send while the
// supervisor runs — a command is NEVER dropped (dropping evSelectAccepted would lose the
// entering-Selected reaction; dropping evClose would hang Close) — and a safe NO-OP once
// run() has returned (runDone closed), so a re-Close after stop() cannot deadlock on the
// unread events channel. Drop coalescing applies only to notify, never to events (spec §5.3).
func (s *supervisor) inject(ev fsmEvent) {
	select {
	case s.events <- ev:
	case <-s.runDone:
	}
}

// requestClose pins the exact epoch the supervisor must ensure-tear-down (so Close and the
// supervisor agree on WHICH generation) and then injects evClose (spec §5.3).
func (s *supervisor) requestClose(e *epoch) {
	s.closeEpoch.Store(e)
	s.inject(evClose)
}

// stop closes stopCh exactly once (via stopOnce), causing run() to return. Close calls it
// AFTER e.wait(); the run() defers then close notify (stopping the notifier) and runDone.
func (s *supervisor) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// notifier ranges the single notify channel and delivers each transition to every registered
// StateChangeHandler (read from the Connection's persistent atomic.Pointer slice). Each
// handler call is individually panic-isolated (H4) so one panicking handler cannot stop the
// rest or crash the notifier. It exits when run() closes notify. A slow/blocked handler delays
// delivery but — because every supervisor send is non-blocking — never blocks the supervisor.
func (s *supervisor) notifier() {
	for sc := range s.notify {
		// Surface any notifications coalesced (dropped) since the last pickup. Emitted here, on the
		// consumer goroutine — NEVER the supervisor run() goroutine — so a blocking user logger can
		// delay delivery but can never stall the FSM or a concurrent Close (M4/P1-B).
		s.reportDrops()

		hs := s.handlers.Load()
		if hs == nil {
			continue
		}

		for _, h := range *hs {
			s.callHandler(h, sc)
		}
	}
}

// reportDrops logs a Warn if notify has coalesced (dropped) intermediate state changes since the
// last report (M4). It is edge-triggered on droppedNotify so a stall-burst logs once (with the
// running total), not once per drop; the latest state is always delivered regardless. Called ONLY
// from the notifier goroutine, so lastLoggedDropped needs no synchronization and a slow logger
// cannot block the supervisor. A nil logger (unit tests) is a no-op.
func (s *supervisor) reportDrops() {
	if s.logger == nil {
		return
	}

	if dropped := s.droppedNotify.Load(); dropped > s.lastLoggedDropped {
		s.lastLoggedDropped = dropped
		s.logger.Warn("hsms: state-change notification(s) coalesced (handler not draining); latest state still delivered",
			"dropped_total", dropped)
	}
}

// callHandler invokes one StateChangeHandler under a recover guard (H4 panic isolation).
func (s *supervisor) callHandler(h StateChangeHandler, sc stateChange) {
	defer func() {
		_ = recover() // isolate a panicking handler; one bad handler must not stop the rest
	}()

	h(sc.prev, sc.next)
}
