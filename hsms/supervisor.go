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
	evTCPUp          fsmEvent = iota // TCP came up (a same-state no-op here; the NotConnected -> NotSelected CAS is CommitConnected's, see transition)
	evSelectAccepted                 // Select accepted: NotSelected -> Selected (and Selected -> Selected, H2)
	evSelectLost                     // Select lost: Selected -> NotSelected
	evDisconnect                     // TCP dropped: Selected/NotSelected -> NotConnected
	evClose                          // voluntary Close: any state -> NotConnected
	evT7Timeout                      // T7 NOT-SELECTED dwell expired: NotSelected -> NotConnected (no-op otherwise)
)

// fsmCommand is one queued event plus the TransitionCause the INJECTION SITE named for it.
// The cause rides alongside the event value and is never read by the transition table:
// the FSM's states and transitions are exactly what they were before causes existed.
// It exists only so fireTransition can hand the notifier a reason for the transition it reports.
type fsmCommand struct {
	ev    fsmEvent
	cause TransitionCause
	// gen is the identity of the generation on whose behalf the event was injected (epoch.id).
	// For a synchronous commit's follow-up that passed through a real gate,
	// this is the gate's own resolved id, not the caller's own gen (see commitFrom).
	// 0 means the injection site named none and no gate resolved one (evClose, or the nil-gate bypass).
	// It exists so an event injected by a generation that has since ended is discarded,
	// instead of driving a transition on its successor.
	// When the event does drive a transition,
	// it is also the generation that transition is reported for (see transitionIdentity).
	gen uint64
}

// stateChange is one logical E37 transition, reported to the notifier as (prev -> next) plus a cause:
// ordinarily the one carried by the event that drove it,
// but step substitutes CauseSelectAccepted for the coalesced entering-Selected bring-up (see step and fireTransition).
//
// gen and socket name the generation the transition belongs to and that generation's socket.
// step snapshots them when the transition fires (see transitionIdentity),
// and the notifier reports them verbatim:
// a notification the notifier delivers late,
// after its generation tore down or a successor was published,
// still names its own generation.
// at is the time step fired the transition, taken with the identity for the same reason:
// the notifier's delivery can trail it by as long as a slow handler or subscriber holds the notifier.
type stateChange struct {
	prev   ConnState
	next   ConnState
	cause  TransitionCause
	gen    uint64
	socket uint64
	at     time.Time
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
	state         atomic.Uint32                                     // stores a ConnState; lock-free hot-path reads + State()
	lastReacted   ConnState                                         // run-owned; dedups reactions/notify (H3; tolerates the H2 pre-commit)
	closed        bool                                              // run-owned; LATCHED true once evClose is processed (I2) — later events ignored
	events        chan fsmCommand                                   // SOLE reader is run(); GUARANTEED command queue (inject blocks, never drops)
	notify        chan stateChange                                  // SOLE sender is run(); NON-BLOCKING drop-OLDEST coalescing
	droppedNotify atomic.Uint64                                     // count of coalesced/dropped notifications; surfaced via a rate-limited Warn (M4)
	react         func(prev, next ConnState, cause TransitionCause) // for a transition INTO NotConnected: farewell decision + teardown init
	closeEpoch    atomic.Pointer[epoch]                             // set by requestClose(e) BEFORE evClose; the epoch to ensure-tear-down
	stopCh        chan struct{}                                     // closed by stop() (from Close, AFTER e.wait()) -> run() exits
	runDone       chan struct{}                                     // closed when run() returns; makes inject a safe no-op after stop
	notifierDone  chan struct{}                                     // closed when notifier() returns; the per-cycle notifier join signal
	stopOnce      sync.Once                                         // guards close(stopCh) so stop() is idempotent
	// shutdownErr is the result of the shutdown that stopped this supervisor (Close, or a failed Open's rollback),
	// returned again by every later Close of the same cycle.
	// It is written once and read only under connection.lifeMu; runDone and notifierDone do NOT publish it.
	shutdownErr error
	// closeTimeout bounds the evClose ensure-teardown of the pinned epoch (§5.2). It is a
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

	// curGen reports the identity of the generation that is CURRENT (connection.CurrentGeneration),
	// which may be a generation that has already ended, during the reconnect window before its successor publishes.
	// step compares it against fsmCommand.gen and discards an event whose generation is no longer current.
	// It MUST stay lock-free — step calls it on the FSM goroutine, which the epoch teardown join can be waiting behind,
	// so a lock the teardown path holds would close a cycle.
	// Nil in unit tests that build a supervisor without a connection,
	// which SKIPS the match rather than suppressing the event.
	curGen func() uint64

	// curEpoch reports the epoch that is CURRENT (connection.cur), under the same lock-free rule as curGen.
	// step reads it only when a transition fires,
	// to name the socket of the generation the transition belongs to (see transitionIdentity).
	// Nil in unit tests that build a supervisor without a connection, which reports socket 0.
	curEpoch func() *epoch

	// commitGate fences the Select-lost synchronous commit (connection.commitGate).
	// That commit is a CAS operation on state rather than an event on the queue,
	// so step's generation match never sees it and it needs a barrier of its own.
	// The gate runs the supplied CAS only while gen is still the live, un-torn-down generation
	// (a gen of 0 is admitted on liveness alone, skipping only the identity comparison — connection.commitGate),
	// and reports whether the CAS committed, whether the generation was live at all,
	// and the id of the epoch it resolved the decision against (meaningful only when live).
	// commitFrom carries that id onward to the follow-up event (see commitFrom).
	// Nil in unit tests that build a supervisor without a connection, which SKIPS the fence.
	commitGate func(gen uint64, cas func() bool) (committed, live bool, id uint64)

	// selectGate is commitGate's counterpart for the Select-accepted commit (connection.selectCommitGate).
	// It shares commitGate's shape, RLock discipline, and gen-0-admitted-on-liveness rule.
	// On a successful commit it also latches the committing generation's reconnect-backoff reset
	// marker (epoch.reachedSelected) inside the same RLock section.
	// Nil in unit tests that build a supervisor without a connection, which SKIPS the fence exactly as
	// a nil commitGate does.
	selectGate func(gen uint64, cas func() bool) (committed, live bool, id uint64)

	// tcpUpCommitGate is commitGate's counterpart for the TCP-up commit (connection.tcpUpCommitGate).
	// It shares commitGate's shape, RLock discipline, and gen-0-admitted-on-liveness rule:
	// SECS-I, and any transport that uses only TransportRuntime, report TCP-up unnamed,
	// and gating only named generations would leave every one of their reports unfenced.
	// It additionally admits at most one TCP-up per generation BEFORE the CAS runs at all —
	// a second report on the same generation is refused outright, even when the first one's own CAS failed —
	// and, on that admitted commit's success, latches the generation's tcpUpCommitted marker — see connection.tcpUpCommitGate.
	// Nil in unit tests that build a supervisor without a connection, which SKIPS the fence exactly as
	// a nil selectGate does.
	tcpUpCommitGate func(gen uint64, cas func() bool) (committed, live bool, id uint64)

	// staleGen counts events discarded because the generation that injected them had already ended,
	// plus the disconnect reports the injection path itself dropped for the same reason (connection.injectDisconnect),
	// plus the synchronous commits the generation gate refused for the same reason.
	// It is a diagnostic: a non-zero value is normal under reconnect churn with a wedged transport goroutine,
	// and is the signal that the generation match is doing work.
	staleGen atomic.Uint64

	// testHookAfterStateLoad, when non-nil, is invoked by step() immediately after it loads state and
	// before the transition/store — a test seam to deterministically interpose a concurrent
	// CommitSelected and exercise the evT7Timeout CAS tie. Always nil in production.
	testHookAfterStateLoad func(ev fsmEvent)

	// testHookBeforeEnqueue, when non-nil,
	// is invoked by commitFrom immediately after one of the three synchronous commits' CAS succeeds
	// and BEFORE its follow-up event is enqueued.
	// gen is the id that will be queued —
	// the gate's own resolved id for a gated commit, or the caller's own gen for the nil-gate bypass (see commitFrom).
	// A true return withholds that enqueue entirely, leaving the TEST —
	// not commitFrom's caller, which never sees skipEnqueue —
	// to replay the report later (via injectFrom) or drop it:
	// the test seam this repo's synchronous-commit races have no other way to force deterministically,
	// since a report that already landed on state but has not yet reached the FSM queue is exactly the window a disconnect (or a Close) can overtake.
	// A test installs it before the commit it means to intercept can run —
	// before starting any goroutine that could reach it, or behind its own happens-before edge —
	// since a hook installed after that commit's CAS has already applied simply misses it.
	// Always nil in production.
	testHookBeforeEnqueue func(gen uint64, ev fsmEvent, cause TransitionCause) (skipEnqueue bool)

	// handlers are NOT owned by the supervisor: they live on the Connection (an atomic.Pointer
	// to an immutable slice) so they persist across Open/Close cycles while the supervisor is
	// recreated per Open. The per-Open supervisor only reads this pointer.
	handlers *atomic.Pointer[[]StateChangeHandler]

	// subs is the CANCELLABLE lifecycle-subscription slice, held on the Connection for the same reason as handlers and read the same way.
	// It is deliberately SEPARATE storage from handlers:
	// the legacy append-only registration path stays untouched, and a cancel only ever rebuilds this slice.
	// nil in unit tests that construct a supervisor without a connection.
	subs *atomic.Pointer[[]lifecycleSub]

	// reportPanic and reportGoexit are invoked by callHandler / callSub to record a recovered panic or a runtime.Goexit from a StateChangeHandler or a lifecycle-subscriber callback.
	// Both are installed by the connection BEFORE run()/notifier() start (Open), and both load the
	// connection's LIVE config on every call rather than a value snapshotted at install time, so a
	// mid-session UpdateConfigOptions(WithLogger) is honored.
	// Both are nil in a unit-test supervisor built without a connection;
	// callHandler / callSub then fall back to a bare recover with no count and no log —
	// the SAME silent isolation those sites had before this existed.
	// Goexit detection itself is unaffected: runCallback always distinguishes a Goexit from a panic;
	// only the REPORT (the count/log this hook would otherwise make) is a no-op.
	reportPanic  func(kind string, r any)
	reportGoexit func(kind string)
}

// newSupervisorWithEventsCap builds a supervisor with an explicit events-queue capacity
// (used by tests to make the guaranteed-command-queue behavior deterministic); the notify
// buffer keeps the default capacity. The caller installs the live closeTimeout provider (M7)
// and logger (M4) as post-construction fields; both are optional (safe defaults / nil-guard).
func newSupervisorWithEventsCap(
	react func(prev, next ConnState, cause TransitionCause),
	handlers *atomic.Pointer[[]StateChangeHandler],
	subs *atomic.Pointer[[]lifecycleSub],
	eventsCap int,
) *supervisor {
	return &supervisor{
		state:         atomic.Uint32{},
		lastReacted:   NotConnectedState,
		events:        make(chan fsmCommand, eventsCap),
		notify:        make(chan stateChange, supervisorNotifyCap),
		droppedNotify: atomic.Uint64{},
		react:         react,
		closeEpoch:    atomic.Pointer[epoch]{},
		stopCh:        make(chan struct{}),
		runDone:       make(chan struct{}),
		notifierDone:  make(chan struct{}),
		stopOnce:      sync.Once{},
		handlers:      handlers,
		subs:          subs,
	}
}

// newSupervisor builds a fresh supervisor for one Open/Close cycle. react is invoked for
// each deduped logical transition, with the cause step reports for it (non-blocking — it only SCHEDULES teardown, never Waits);
// handlers points at the Connection's persistent StateChangeHandler slice, and subs at its cancellable lifecycle-subscription slice.
// The caller sets the live closeTimeout provider (M7) and logger (M4) on the returned supervisor.
func newSupervisor(
	react func(prev, next ConnState, cause TransitionCause),
	handlers *atomic.Pointer[[]StateChangeHandler],
	subs *atomic.Pointer[[]lifecycleSub],
) *supervisor {
	return newSupervisorWithEventsCap(react, handlers, subs, supervisorEventsCap)
}

// transition is the pure E37 §5.4–§5.6 state table. It returns the next state and whether
// the (cur, ev) pair is a legal transition; an illegal pair yields (cur, false) so the run
// loop can treat it as a safe no-op.
//
// evTCPUp is legal from NotSelected —
// the ordinary case: CommitConnected's own CAS always lands before its event is processed (spec §7.D) —
// and from Selected, tolerating a Select commit's CAS
// that raced ahead of evTCPUp's own processing (step reports that coalesced bring-up with CauseSelectAccepted;
// see step and fireTransition).
// It is NOT legal from NotConnected:
// production enqueues evTCPUp only after its own NotConnected->NotSelected CAS has already succeeded,
// so finding NotConnected at processing time means a disconnect overtook it,
// and treating that as a legal bring-up would resurrect a generation that has already dropped.
// evSelectAccepted is legal from BOTH NotSelected AND Selected
// (the latter tolerates the H2 pre-commit in CommitSelected — spec §7.D).
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
		if cur == NotSelectedState || cur == SelectedState {
			return cur, true
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
// a guarded CAS NotConnected -> NotSelected directly on state, making State()==NotSelected immediately (before the recv loop / active Select procedure runs)
// so a Select.req dispatched right after TCP-up finds NotSelected and CommitSelected's CAS succeeds —
// with NO async poll-fence.
//
// On a successful commit it enqueues evTCPUp
// so the supervisor fires the entering-NotSelected reaction/notify EXACTLY ONCE, deduped on lastReacted:
// tolerant of the pre-committed state via the evTCPUp-from-NotSelected table entry,
// and of a Select commit's CAS that raced ahead of it (see step's coalesced-cause handling).
// It returns whether THIS call performed the commit;
// a call when not NotConnected is a no-op returning false —
// TCPUp is driven once per generation, and this CAS is the only site that ever moves state OUT of
// NotConnected, so it always succeeds in practice.
// cause comes from the caller, so the reason travels with the event from the site that named it.
func (s *supervisor) CommitConnected(cause TransitionCause) (committed bool) {
	return s.commitFrom(0, NotConnectedState, NotSelectedState, evTCPUp, cause)
}

// CommitConnectedFromGeneration is CommitConnected performed ON BEHALF OF gen, the generation whose socket came up.
//
// NotConnected — the state this commit CASes out of — is also the state a generation leaves behind when it ends,
// so identity alone cannot tell a live TCP-up from one an abandoned goroutine is replaying onto a dead generation.
// The generation gate supplies that distinction; see commitFrom.
func (s *supervisor) CommitConnectedFromGeneration(gen uint64, cause TransitionCause) (committed bool) {
	return s.commitFrom(gen, NotConnectedState, NotSelectedState, evTCPUp, cause)
}

// CommitSelected performs the H2 §7.D synchronous responder commit: a guarded CAS NotSelected -> Selected directly on state, making IsSelected() true immediately (before the responder writes Select.rsp)
// so data pipelined right after Select.rsp is not spuriously Rejected.
//
// On a successful commit it enqueues evSelectAccepted so the supervisor fires the entering-Selected reaction/notify EXACTLY ONCE (deduped on lastReacted, tolerating the pre-committed state).
// It returns whether THIS call performed the commit; a call when already Selected is a no-op returning false.
// cause comes from the caller, so the reason travels with the event from the site that named it.
func (s *supervisor) CommitSelected(cause TransitionCause) (committed bool) {
	return s.commitFrom(0, NotSelectedState, SelectedState, evSelectAccepted, cause)
}

// CommitSelectedFromGeneration is CommitSelected performed ON BEHALF OF gen, the generation that ran the handshake.
//
// A correlated Select.rsp, or an inbound Select.req, can be processed by a recv goroutine the bounded teardown join abandoned.
// Committing it at that point would flip the SUCCESSOR generation's FSM to Selected without any handshake on that link,
// and would then make the successor's own commit a no-op —
// so it would never cancel its T7 dwell nor start its auto-linktest, and its bring-up would go unreported.
// The generation gate refuses such a commit; see commitFrom.
func (s *supervisor) CommitSelectedFromGeneration(gen uint64, cause TransitionCause) (committed bool) {
	return s.commitFrom(gen, NotSelectedState, SelectedState, evSelectAccepted, cause)
}

// CommitSelectLost performs the synchronous Selected -> NotSelected commit (symmetric with CommitSelected / §7.D):
// a guarded CAS Selected -> NotSelected directly on state, making State()==NotSelected IMMEDIATELY.
//
// Without it, SelectLost was an async inject: after a Deselect.req the state stayed Selected until run() processed the event,
// so a peer that pipelined a re-Select.req had its CommitSelected CAS fail (still Selected) yet was still answered Select.rsp status-0 —
// told "selected" without a real commit (I3, the efb220b class via the Deselect door).
// Committing here on the recv goroutine closes that window.
// On a successful commit it enqueues evSelectLost so the supervisor fires the entering-NotSelected reaction/notify EXACTLY ONCE (deduped on lastReacted, tolerating the pre-committed state via the evSelectLost-from-NotSelected table entry).
// It returns whether THIS call performed the commit; a call when not Selected is a no-op returning false.
// cause comes from the caller, so the reason travels with the event from the site that named it.
func (s *supervisor) CommitSelectLost(cause TransitionCause) (committed bool) {
	return s.commitFrom(0, SelectedState, NotSelectedState, evSelectLost, cause)
}

// CommitSelectLostFromGeneration is CommitSelectLost performed ON BEHALF OF gen, the generation that answered the Deselect.
//
// A recv goroutine that outlived its own generation would otherwise answer a Deselect.req by deselecting the SUCCESSOR's legitimately selected link.
// The generation gate refuses it; see commitFrom.
func (s *supervisor) CommitSelectLostFromGeneration(gen uint64, cause TransitionCause) (committed bool) {
	return s.commitFrom(gen, SelectedState, NotSelectedState, evSelectLost, cause)
}

// commitFrom is the shared body of the three synchronous §7.D commits:
// a guarded CAS from -> to directly on state,
// followed by ev's injection for the deduped reaction/notify.
//
// gen is the generation the commit is made on behalf of, or 0 when the caller named none.
// Every one of the three gates (commitGate for Select-lost, selectGate for Select-accepted, tcpUpCommitGate for TCP-up) is skipped outright,
// leaving a bare CAS,
// only when it is nil,
// which happens only in a supervisor built without a connection (unit tests).
// A gen of 0 reaches whichever gate is installed, exactly like any other gen:
// it is admitted on liveness alone, skipping only the identity comparison (see commitGate's doc).
//
// A gated commit runs the CAS inside the connection's generation gate,
// which admits it only while the live generation has not begun teardown and (for a named generation)
// matches identity.
// Both halves matter for a named generation:
// identity alone cannot reject a commit replayed onto a generation that has ENDED but is still published,
// because connection.cur keeps pointing at it for the whole reconnect backoff that follows.
//
// The fence has to cover the CAS itself, not just a check before it.
// Unlike an event on the queue, this commit is applied by the caller's own goroutine,
// so there is no later point where the answer can be re-taken.
// A bare check-then-CAS would leave the whole gap between them open,
// and the FSM state cannot close it either:
// NotConnected -> NotSelected -> ... -> NotConnected is a real cycle,
// so the state a stale CAS expects can legitimately reappear underneath it.
//
// It cannot refuse a legitimate commit.
// A generation runs its handshake between its own publish and its own teardown,
// and no successor can be published until this generation's teardown has both begun
// (which latches epoch.ended, under the same gate)
// and completed its join (which is what the reconnect loop waits for).
// So a commit issued by the working generation always finds itself live.
//
// The injection is deliberately issued AFTER the gate is released:
// inject can block on a full event queue, and the gate must never be held across anything that blocks.
// A gated commit carries the gate's own id onward, in place of the caller's gen,
// so step's later generation match is applied against the exact generation this gate resolved the decision against —
// not against whatever generation is current by the time step processes the follow-up.
// The nil-gate bypass has no gate to resolve an id from, so it carries the caller's gen onward unchanged.
func (s *supervisor) commitFrom(gen uint64, from, to ConnState, ev fsmEvent, cause TransitionCause) (committed bool) {
	cas := func() bool { return s.state.CompareAndSwap(uint32(from), uint32(to)) }

	gate := s.commitGate

	if ev == evSelectAccepted { //nolint:staticcheck // if/else, not a switch: an exhaustive switch over every event would need a default with nothing sensible to do here.
		gate = s.selectGate
	} else if ev == evTCPUp {
		gate = s.tcpUpCommitGate
	}
	// Every other event this function can be called with — only evSelectLost, in practice;
	// evDisconnect, evClose, and evT7Timeout are async-only and never reach commitFrom — keeps commitGate.

	if gate == nil {
		if cas() {
			if hook := s.testHookBeforeEnqueue; hook == nil || !hook(gen, ev, cause) {
				s.injectFrom(gen, ev, cause)
			}

			return true
		}

		return false
	}

	committed, live, id := gate(gen, cas)
	if !live {
		s.staleGen.Add(1)

		return false
	}

	if committed {
		if hook := s.testHookBeforeEnqueue; hook == nil || !hook(id, ev, cause) {
			s.injectFrom(id, ev, cause)
		}
	}

	return committed
}

// run is the single writer for async transitions. Its lifetime is the whole Open/Close cycle
// (NOT an epoch): it selects on stopCh (closed by stop()) and events. There is deliberately
// NO ctx.Done()->evClose synthesis — evClose is ONLY ever the pinned one from
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
		case cmd := <-s.events:
			s.step(cmd)
		}
	}
}

// step applies one event.
// It reads the current state.
// Then an event that names the generation it was injected for is matched against the current generation,
// and discarded once its generation is no longer current —
// the barrier that keeps a late transport goroutine from dropping its successor's link
// (reading state before this check is load-bearing; see the inline note below).
// It then applies the pure transition (illegal
// pairs are safe no-ops), stores state only when it actually changed (a no-op when
// CommitSelected already pre-stored — H2), and fires the deduped reaction/notify keyed on
// lastReacted (H3).
// A stored transition that actually drops the link into NotConnected is the one exception to that dedup:
// it always fires, even when lastReacted already reads NotConnected
// because an earlier bring-up into the state the link is leaving was itself never reported —
// one reaction and one notification ENQUEUE per real drop, with the reported prev always the state actually left (cur), never lastReacted,
// since lastReacted may lag behind an unreported bring-up the drop overtook.
// Delivery past that enqueue stays best-effort (notify still coalesces under a stalled consumer — see emit),
// so an unreported predecessor state may still go unobserved even though the drop itself is not.
// Two events are guarded against a concurrent synchronous CommitSelected: the
// evT7Timeout store is a CAS(cur -> next) (not a plain Store), and evSelectLost is abandoned when the
// state is observed Selected (a pipelined re-Select re-committed after CommitSelectLost's CAS) — in
// both cases step early-returns and leaves the committed Selected session intact rather than tearing
// it down / flapping it (§9.2.2). Regardless of the
// transition, evClose additionally and unconditionally
// initiates teardown of the PINNED epoch (idempotent closeOnce) so a Close while already
// NotConnected — where no transition fires — still initiates teardown and Close's e.wait()
// cannot hang (spec §5.3). closeEpoch is nil-guarded so a raw evClose (no requestClose) is a
// safe no-op.
func (s *supervisor) step(cmd fsmCommand) {
	ev := cmd.ev

	// Once evClose has been processed the supervisor is LATCHED closed — every later event is a no-op.
	// This is the general backstop for the Close-vs-reconnect-Start race:
	// a legal event queued behind evClose (evSelectAccepted from NotSelected, for instance) would otherwise still fire after Close,
	// leaving State() misreporting and suppressing the terminal NotConnected.
	// A delayed evTCPUp is additionally closed off at the table level (NotConnected + evTCPUp is illegal — see transition),
	// but this latch is what protects every other event this race can queue.
	// requestClose is only ever terminal (Close / failed-Open rollback, both under lifeMu), so latching cannot drop a legitimate later transition — the generation is ending.
	if s.closed {
		return
	}

	// The epoch a Close tears down, loaded once so the transition it fires names the same epoch its teardown ends.
	var closing *epoch
	if ev == evClose {
		closing = s.closeEpoch.Load()
	}

	cur := ConnState(s.state.Load())

	// Generation match.
	// An event carrying a generation (epoch.id) is honored only while THAT generation is still the live one;
	// otherwise it is discarded and counted.
	//
	// The ORDER here is load-bearing and must not be reversed: the state read above happens BEFORE this check.
	// A state belonging to a successor generation can only have been committed after that generation was published,
	// so reading such a state guarantees this check observes the successor and discards the event.
	// Checking first and reading state second would let a successor's freshly committed state be torn down by a predecessor's event.
	//
	// This is also why an injection-site check alone is not enough.
	// Cancellation, or the epoch swap, can land between any pre-check and the injection,
	// so the only place the answer cannot go stale is the point where the event is applied to the FSM —
	// here, on the single goroutine that owns transitions.
	//
	// It cannot suppress a legitimate disconnect.
	// cur advances only after the FSM entered NotConnected (see the note at the two connection.cur publish sites),
	// so a mismatch means the reporting generation's link was already torn down and its event is redundant.
	if cmd.gen != 0 && s.curGen != nil && s.curGen() != cmd.gen {
		s.staleGen.Add(1)

		return
	}

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
		// left is the state this step actually replaced.
		// It can differ from cur when a synchronous commit CASed the state after the load above,
		// so a real drop reads it back from the store itself rather than trusting cur.
		left := cur
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
				left = ConnState(s.state.Swap(uint32(next)))
			}
		}

		// A transition that really drops the link into NotConnected always fires,
		// even when lastReacted already reads NotConnected.
		// Dedup on lastReacted alone would absorb it whenever the bring-up it ends was never reported
		// (a Select CAS landed while its evSelectAccepted was still queued, for instance),
		// losing the reaction, the teardown and reconnect it drives, and the notification.
		dropped := next == NotConnectedState && left != NotConnectedState

		if next != s.lastReacted || dropped {
			prev := s.lastReacted
			if dropped {
				// lastReacted can lag behind an unreported bring-up the drop overtook,
				// so a drop reports the state the link actually left.
				prev = left
			}

			cause := cmd.cause
			if ev == evTCPUp && next == SelectedState {
				// The Select commit's CAS landed before this evTCPUp was processed,
				// so the coalesced bring-up is reported as an ordinary Select, not with evTCPUp's own cause.
				cause = CauseSelectAccepted
			}

			// The time is taken here, as the transition fires, with its identity,
			// never on the notifier goroutine, whose delivery a slow handler or subscriber delays.
			gen, socket := s.transitionIdentity(cmd, closing)
			s.fireTransition(stateChange{prev: prev, next: next, cause: cause, gen: gen, socket: socket, at: time.Now()})
			s.lastReacted = next
		}
	}

	if ev == evClose {
		// Latch closed (I2) BEFORE teardown: no event queued behind this evClose may move state again.
		s.closed = true
		if closing != nil {
			closing.teardown(s.resolveCloseTimeout())
		}
	}
}

// transitionIdentity returns the generation and socket a transition driven by cmd belongs to,
// read at the moment the transition fires.
//
// A Close belongs to closing, the epoch requestClose pinned, and not to whichever epoch is current;
// it reports 0/0 when nothing was pinned, as for a connection with no epoch at all.
// Any other event belongs to the generation it carries:
// step has just matched that generation against the current one,
// so the current epoch is that generation's own, and its socket is read from it.
// The current epoch cannot have advanced since the match:
// a successor is published only after this generation's link is down,
// and an event of a generation whose link is down fires no transition.
// An epoch that never acquired a socket reports socket 0,
// and so does an event that names no generation, which only a supervisor built without a connection queues.
func (s *supervisor) transitionIdentity(cmd fsmCommand, closing *epoch) (gen, socket uint64) {
	if cmd.ev == evClose {
		if closing == nil {
			return 0, 0
		}

		return closing.id, closing.socketID()
	}

	if cmd.gen == 0 || s.curEpoch == nil {
		return cmd.gen, 0
	}

	if e := s.curEpoch(); e != nil && e.id == cmd.gen {
		return e.id, e.socketID()
	}

	return cmd.gen, 0
}

// fireTransition emits the notification and calls react for one deduped transition, sc, which step builds.
// sc.prev is supplied by step.
// Normally it is lastReacted (NOT the atomic's current value — H2/H3),
// so a pre-committed entering-Selected is still reported as (NotSelected -> Selected).
// For a real drop into NotConnected it is the state the store actually replaced,
// since lastReacted can lag behind an unreported bring-up the drop overtook.
// For a terminal NotConnected transition the notify is EMITTED BEFORE react
// (the F1 ordering guarantee: the terminal state is enqueued before react may initiate teardown that stops the notifier);
// for any other transition react runs first, then emit.
//
// sc.cause is the one step computed for this deduped transition.
// Ordinarily it is the cause carried by the event that drove it;
// for the coalesced entering-Selected bring-up — evTCPUp finding the Select commit already applied —
// it is CauseSelectAccepted, matching an ordinary Select.
// Because the dedup key is the state entered,
// a later event landing on the same state fires nothing and its cause is never reported —
// a Close on an already-dropped link whose drop was itself already reported fires nothing further, not
// CauseLocalClose (documented on SubscribeLifecycle).
//
// sc.gen and sc.socket are the identity step snapshotted for this transition (see transitionIdentity);
// react takes no identity, since it resolves the epoch it acts on itself.
func (s *supervisor) fireTransition(sc stateChange) {
	if sc.next == NotConnectedState {
		s.emit(sc)
		s.react(sc.prev, sc.next, sc.cause)

		return
	}

	s.react(sc.prev, sc.next, sc.cause)
	s.emit(sc)
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
// cause travels with the event to step,
// which ordinarily reports it verbatim for whichever transition this event ends up driving —
// except the coalesced entering-Selected bring-up,
// where step substitutes CauseSelectAccepted regardless of the cause named here (see step).
func (s *supervisor) inject(ev fsmEvent, cause TransitionCause) {
	s.injectFrom(0, ev, cause)
}

// injectFrom is inject for an event reported ON BEHALF OF a generation.
// gen (epoch.id) rides along on the command,
// and step discards the event if that generation is no longer the current one
// (an ended generation that is still current is admitted).
//
// A gen of 0 means the injection site itself named no generation.
// step then processes the queued event against whatever generation is current when it reaches it,
// skipping the identity match.
// Only two kinds of site still queue an event with gen 0: evClose,
// and the nil-gate bypass a supervisor built without a connection falls back to (unit tests, which have no epoch to name).
// Every unnamed synchronous commit (TCP-up, Select-accepted, or Select-lost) that reaches a real gate is instead queued with the gate's own resolved id (see commitFrom).
// All three gates check liveness at commit time regardless of gen;
// only the identity comparison is skipped for a gen of 0 (see commitGate, tcpUpCommitGate, selectCommitGate).
// An unnamed evDisconnect or evT7Timeout report never carries gen 0 here:
// injectDisconnect and injectT7Expiry each bind it, at report time, to the generation current then,
// before calling this (see injectDisconnect).
func (s *supervisor) injectFrom(gen uint64, ev fsmEvent, cause TransitionCause) {
	select {
	case s.events <- fsmCommand{ev: ev, cause: cause, gen: gen}:
	case <-s.runDone:
	}
}

// requestClose pins the exact epoch the supervisor must ensure-tear-down (so Close and the
// supervisor agree on WHICH generation) and then injects evClose (spec §5.3).
// cause is the reason the caller is closing; every caller today is a locally initiated teardown.
func (s *supervisor) requestClose(e *epoch, cause TransitionCause) {
	s.closeEpoch.Store(e)
	s.inject(evClose, cause)
}

// stop closes stopCh exactly once (via stopOnce), causing run() to return. Close calls it
// AFTER e.wait() (via joinSupervisor); the run() defers then close notify (stopping the notifier) and runDone.
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
//
// Cancellable lifecycle subscriptions ride this SAME goroutine, after the legacy handlers and in registration order, with the same panic isolation.
// One delivery pass, one ordering, no second goroutine.
// Each pass reads the subscription pointer ONCE and iterates that immutable snapshot,
// so a cancel racing the pass can still let one already-dispatching event through (documented on SubscribeLifecycle),
// but can never corrupt the iteration.
func (s *supervisor) notifier() {
	defer close(s.notifierDone)

	for sc := range s.notify {
		// Surface any notifications coalesced (dropped) since the last pickup. Emitted here, on the
		// consumer goroutine — NEVER the supervisor run() goroutine — so a blocking user logger can
		// delay delivery but can never stall the FSM or a concurrent Close (M4/P1-B).
		s.reportDrops()

		if hs := s.handlers.Load(); hs != nil {
			for _, h := range *hs {
				s.callHandler(h, sc)
			}
		}

		s.notifySubs(sc)
	}
}

// notifySubs delivers one transition to every live lifecycle subscription.
// subs is nil for a supervisor built without a connection (unit tests), which makes this a no-op.
func (s *supervisor) notifySubs(sc stateChange) {
	if s.subs == nil {
		return
	}

	subs := s.subs.Load()
	if subs == nil || len(*subs) == 0 {
		return
	}

	ev := LifecycleEvent{Previous: sc.prev, Current: sc.next, Cause: sc.cause, Socket: sc.socket, Generation: sc.gen, At: sc.at}
	for _, sub := range *subs {
		s.callSub(sub.fn, ev)
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

// callHandler invokes one StateChangeHandler under [runCallback]'s panic/Goexit isolation (H4):
// a recovered panic is counted and logged, and a runtime.Goexit is logged —
// there is no per-generation transport to drop for a notifier-side callback, so this site logs only.
// The notifier goroutine ends WITH that Goexit — it runs the callback inline, so the Goexit unwinds the notifier itself —
// so no later notification for this Open is delivered.
// An unrecovered panic, in contrast, would crash the whole process rather than merely end this goroutine,
// which is exactly why runCallback recovers it instead.
func (s *supervisor) callHandler(h StateChangeHandler, sc stateChange) {
	runCallback(
		func() { h(sc.prev, sc.next) },
		func(r any) { s.reportCallbackPanic(kindStateChangeHandler, r) },
		func() { s.reportCallbackGoexit(kindStateChangeHandler) },
	)
}

// callSub invokes one lifecycle subscription callback under the same isolation callHandler gives
// a StateChangeHandler.
func (s *supervisor) callSub(fn func(LifecycleEvent), ev LifecycleEvent) {
	runCallback(
		func() { fn(ev) },
		func(r any) { s.reportCallbackPanic(kindLifecycleSubscriber, r) },
		func() { s.reportCallbackGoexit(kindLifecycleSubscriber) },
	)
}

// reportCallbackPanic forwards to the installed reportPanic hook,
// a no-op when the supervisor was built without a connection (see the reportPanic field doc).
func (s *supervisor) reportCallbackPanic(kind string, r any) {
	if s.reportPanic != nil {
		s.reportPanic(kind, r)
	}
}

// reportCallbackGoexit forwards to the installed reportGoexit hook,
// a no-op when the supervisor was built without a connection (see the reportGoexit field doc).
func (s *supervisor) reportCallbackGoexit(kind string) {
	if s.reportGoexit != nil {
		s.reportGoexit(kind)
	}
}
