package hsms

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
)

// epoch owns everything scoped to ONE TCP-connection generation: the raw socket,
// the per-generation context, and the join group for every task goroutine spawned
// during this generation. It is created on (re)connect, published via
// Connection.cur (an atomic.Pointer, landed by a later task), and torn down exactly
// once.
//
// Stored-context discipline (spec §5.1): ctx/cancel are the GENERATION-LIFETIME
// context, set once at construction (WithCancel(parent)) and never mutated, so an
// atomic epoch swap can never observe a torn read on them. e.ctx carries no
// per-operation deadlines and no WithValue payloads, and there is no broad Context()
// accessor — goroutines receive it as an explicit parameter through spawn
// (guardrail 4). cancel is invoked only by teardown ownership (Task 6), never by
// arbitrary helpers (guardrail 5).
//
// Teardown ownership (Task 6, spec §5.1/§7.C/E): closeOnce admits exactly one
// teardown owner (F4, replaces the v1 ToClosing CAS); closeErr is written inside the
// once and read after done closes; stopTransport is a hook bound at epoch creation by a
// later task (nil until then) that teardown calls to join the transport recv loop.
// log is the stored generation logger.
//
// Send-path state (Task 12, spec §5.5/§6.2): sendCh is the PER-GENERATION async send
// channel (fire-and-forget frames drained by the sender goroutine; GC'd with the epoch, so
// a frame stranded by a send racing teardown can never be flushed as the next generation's
// first frame — C1 dissolves structurally); replies is the per-generation sender-owned reply
// registry (the sender owns each reply channel's whole lifecycle — F5/F6 dissolve); writeMu
// serializes the writev over conn so concurrent senders never interleave frames on the wire.
type epoch struct {
	ctx    context.Context    // generation-lifetime ctx only; set once at construction
	cancel context.CancelFunc // called by teardown ONLY (Task 6), under closeOnce

	// id is this generation's identity token.
	// It is minted from the connection's monotonic counter before the epoch is published on connection.cur,
	// and never mutated afterwards.
	// A transport goroutine carries it,
	// so an involuntary disconnect it reports is matched against the generation current when the event is PROCESSED (see supervisor.step).
	// Zero means "no identity" — an epoch built outside a connection, as unit tests do — and disables the match.
	id uint64

	// ended latches true the moment this generation's teardown begins, and is never cleared.
	// It separates a generation that is still published on connection.cur but already over from one that is genuinely live.
	// The identity alone cannot make that distinction,
	// because cur keeps pointing at a dead generation for the whole reconnect backoff that follows it.
	// A generation-guarded synchronous commit reads it, and teardown writes it, under connection.genGate.
	ended atomic.Bool

	// genGate is the connection's generation gate, borrowed so teardown can latch ended under it.
	// Nil for an epoch built outside a connection, as unit tests do; markEnded then latches unfenced,
	// which is correct because such an epoch has no id and so is never generation-matched.
	genGate *sync.RWMutex

	// reachedSelected latches true when a Select commit succeeds for this generation.
	// selectCommitGate sets it under genGate on the epoch it validated;
	// connectLoop reads it after this generation is joined,
	// to decide whether the next reconnect wait restarts from the initial delay (connection.reconnectDelay).
	// It is read only once, for that one generation, and never cleared — a fresh epoch starts false.
	reachedSelected atomic.Bool

	// tcpUpAdmitted latches true the instant connection.tcpUpCommitGate admits a TCP-up report for this generation —
	// the FIRST one, whether or not its state CAS goes on to succeed.
	// It is never cleared.
	// A second report on the same generation is refused at admission, even when the first one's CAS failed (at most one TCP-up per epoch).
	tcpUpAdmitted atomic.Bool

	// tcpUpCommitted latches true the instant an admitted TCP-up report wins its state CAS (NotConnected -> NotSelected) on this generation.
	// It is never cleared.
	// connectLoop reads it, after this generation's failed Start has been torn down and joined,
	// to tell a plain dial/listen failure (the FSM never left NotConnected; the loop keeps retrying)
	// from a Start that failed AFTER the link actually came up (the drop reaction owns the retry instead — see connection.connectLoop).
	tcpUpCommitted atomic.Bool

	// startReturned is closed exactly once by the goroutine that called tr.Start for this generation.
	// On a successful Start it closes immediately (connectLoop, Open).
	// connectLoop's own failure branch, and Open's cold-branch retry,
	// each close it only after this generation has been torn down and that teardown joined
	// (see connectLoopStartFailure and connection.Open).
	// Open's OWN fatal-failure rollback is the one path that does not wait for that join first:
	// it closes this barrier right after latching the generation ended — the publication fence, not the
	// full teardown — strictly BEFORE that generation's real teardown runs (see connection.rollbackFailedOpen).
	// A successor generation's connectLoop invocation waits on it, right after prev.wait(),
	// before it may claim the reconnect gauge or dial —
	// so a Start still in flight for this generation always finishes, or is torn down,
	// before any successor for it is dialed or published.
	startReturned chan struct{}

	log logger.Logger // generation logger; used by teardown for close-timeout reporting

	connMu sync.RWMutex // guards conn and reason
	conn   net.Conn     // raw *net.TCPConn (no bufio wrapper — defeats writev, §6.2)
	// nil ONLY after closeSocket(), which runs inside teardown's closeOnce.

	// socket is the identity the transport minted for conn, stored with it by adoptConn and 0 when the transport named none.
	// Unlike conn it is never cleared, so a report built after teardown still names the socket this generation used.
	socket atomic.Uint64

	// reason is why this generation's socket is closed, guarded by connMu.
	// The first reason recorded (recordCloseReason) is kept and a later one changes nothing,
	// and closeSocket hands it to the transport's close (closeConn).
	reason closeReason

	// closeConn is the transport's socket close capability, bound at epoch creation (see connection.bindSocketCloser),
	// or nil, in which case closeSocket closes the conn directly.
	closeConn func(net.Conn, error, TransitionCause)

	wg        sync.WaitGroup // joins every per-generation TASK goroutine (the epoch join)
	liveTasks atomic.Int64   // count of live task goroutines; reported on a close-timeout
	spawnMu   sync.RWMutex   // Add-vs-Wait happen-before guard (§7.B)
	closing   bool           // set under spawnMu.Lock at teardown; post-teardown spawns are no-ops

	closeOnce sync.Once // admits exactly ONE teardown owner (F4, replaces the ToClosing CAS)
	closeErr  error     // written inside closeOnce; read only after <-done

	// stopTransport joins the per-generation transport recv loop.
	// It is bound to the transport's Stop at epoch creation by a later task; nil until then, so
	// teardown MUST nil-guard it.
	stopTransport func(context.Context) error

	done chan struct{} // created here; closed by teardown when the bounded join completes

	sendCh  chan *sendRequest // PER-GENERATION async send channel; GC'd with the epoch (dissolves C1)
	replies replyRegistry     // per-generation sender-owned reply channels (dissolves F5/F6)
	writeMu sync.Mutex        // serializes the writev over conn (§6.2)

	// wireScratch is the reusable buffer an outbound frame is copied into for the wire observer, guarded by writeMu.
	// It stays nil unless an observer is installed on a transport that reports its wire (see connection.outboundWireObserver).
	wireScratch []byte

	// commsFailure records the teardown CAUSE for this generation (E37 §9.1.1, spec §5.2/§9.1.1).
	// TCPDown sets it (an involuntary drop / read-write failure / peer Separate), so the
	// NotConnected reaction sends NO courtesy farewell Separate. It stays false for a graceful
	// voluntary Close, which does attempt the bounded best-effort farewell from a live Selected
	// link. It is per-generation (a fresh epoch starts graceful) so no reset is needed.
	commsFailure atomic.Bool
}

// closeReason is why a generation's socket is closed:
// the failure that initiated the close, nil when none did,
// and the TransitionCause that names it.
// set tells a recorded reason from none, so a first reason that carries no failure still wins.
type closeReason struct {
	err   error
	cause TransitionCause
	set   bool
}

// newEpoch creates a fresh generation rooted at parent: ctx/cancel derive from
// context.WithCancel(parent), log is stored for teardown reporting, done is an open channel
// that teardown closes, and the send-path state is initialised — sendCh is buffered to
// sendQueue (the configured senderQueueSize) and replies is a fresh per-generation registry.
func newEpoch(parent context.Context, log logger.Logger, sendQueue int) *epoch {
	ctx, cancel := context.WithCancel(parent)

	return &epoch{
		ctx:           ctx,
		cancel:        cancel,
		log:           log,
		done:          make(chan struct{}),
		sendCh:        make(chan *sendRequest, sendQueue),
		replies:       newReplyRegistry(),
		startReturned: make(chan struct{}),
	}
}

// liveConn returns the current socket under connMu.RLock — the "socket live?" fact.
func (e *epoch) liveConn() net.Conn {
	e.connMu.RLock()
	defer e.connMu.RUnlock()

	return e.conn
}

// markEnded latches this generation as over, under the connection's generation gate.
//
// Taking the gate is what makes the latch a barrier rather than a flag:
// a generation-guarded commit reads ended and performs its CAS inside one RLock section,
// so that section is ordered wholly before or wholly after this Lock section.
// A commit that observed ended false therefore completed its CAS before this generation ended,
// which is before any successor generation could be published.
//
// The gate is held for a single atomic store, so it can never be the inner lock of a cycle.
func (e *epoch) markEnded() {
	if e.genGate == nil {
		e.ended.Store(true)

		return
	}

	e.genGate.Lock()
	e.ended.Store(true)
	e.genGate.Unlock()
}

// setConn publishes the socket for this generation under connMu.Lock.
func (e *epoch) setConn(c net.Conn) {
	e.adoptConn(c, 0)
}

// adoptConn publishes the socket for this generation together with the identity the transport minted for it,
// under connMu.Lock.
// A socket of 0 means the transport named none.
func (e *epoch) adoptConn(c net.Conn, socket uint64) {
	e.connMu.Lock()
	defer e.connMu.Unlock()
	e.conn = c
	e.socket.Store(socket)
}

// socketID returns the identity of the socket this generation adopted, or 0 when it adopted none or the transport named none.
// It keeps its value after closeSocket clears the conn.
func (e *epoch) socketID() uint64 {
	return e.socket.Load()
}

// recordCloseReason records err and cause as the reason this generation's socket is closed,
// unless a reason is already recorded, in which case it changes nothing, even when that reason carries no error.
// Every path that tears a generation down records its reason immediately before its teardown call,
// so the close that teardown performs hands the transport the reason of whichever path got there first.
// A generation that never adopted a socket never uses its reason.
func (e *epoch) recordCloseReason(err error, cause TransitionCause) {
	e.connMu.Lock()
	defer e.connMu.Unlock()

	if !e.reason.set {
		e.reason = closeReason{err: err, cause: cause, set: true}
	}
}

// closeSocket idempotently closes the generation socket.
// It detaches the conn under connMu.Lock (NOT the RLock path liveConn uses — the detach must be exclusive),
// so a nil conn (never set) and a second call (conn already detached) are safe no-ops.
// teardown calls it exactly once (inside closeOnce) and UNCONDITIONALLY before the join:
// closing the socket unblocks any task parked in conn.Read (which does not watch ctx), so the bounded join can complete.
//
// The close itself goes through the transport's closeConn when one is bound, with the recorded reason
// (nil and CauseUnknown when none was recorded),
// and runs after connMu is released:
// the transport takes its own locks there,
// and a transport goroutine can already hold one of them while it publishes a socket onto this epoch under connMu.
// Without closeConn the conn is closed directly.
func (e *epoch) closeSocket() {
	e.connMu.Lock()
	conn := e.conn
	e.conn = nil
	reason := e.reason
	e.connMu.Unlock()

	if conn == nil {
		return
	}

	if e.closeConn != nil {
		e.closeConn(conn, reason.err, reason.cause)

		return
	}

	_ = conn.Close()
}

// spawn is the ONE sanctioned launch path for a per-generation task goroutine.
//
// It takes spawnMu.RLock and, if the epoch is already closing, releases and returns
// false — post-teardown spawns are no-ops. Otherwise it does wg.Add(1) UNDER the
// RLock (the §7.B Add-vs-Wait happen-before guard: teardown's wg.Wait is reached only
// after spawnMu.Lock, which cannot be acquired until every in-flight RLock — and thus
// every 0->1 Add — has been released, so an Add can never race a Wait), then launches
// a panic-guarded goroutine running fn(e.ctx). The RLock is released after launch.
//
// fn receives e.ctx as a parameter (guardrail 4); cancellation arrives via that ctx.
func (e *epoch) spawn(log logger.Logger, name string, fn func(ctx context.Context)) bool {
	e.spawnMu.RLock()
	if e.closing {
		e.spawnMu.RUnlock()
		return false
	}

	// The Add(1) MUST stay explicit and UNDER the RLock — this is the §7.B guard.
	// (go fix suggests wg.Go here; wg.Go would also be correct because its internal
	// Add runs synchronously under this RLock, but the explicit Add keeps the
	// happen-before guard visible at the call site for the concurrency review.)
	e.wg.Add(1)
	e.liveTasks.Add(1)
	go func() {
		// wg.Done runs LAST so that once wg.Wait returns liveTasks has already been
		// decremented; the count is read only on a close-timeout, where the tasks are
		// by definition still live.
		defer e.wg.Done()
		defer e.liveTasks.Add(-1)
		defer func() {
			if r := recover(); r != nil {
				log.Error("epoch task panicked", "task", name, "panic", r)
			}
		}()
		fn(e.ctx)
	}()
	e.spawnMu.RUnlock()

	return true
}

// teardown is the NON-BLOCKING initiator of this generation's shutdown (spec §5.1/§5.3):
// it runs its body exactly once (closeOnce, single-owner F4) and RETURNS IMMEDIATELY —
// it never blocks on the join, so a supervisor reaction may call it without stalling.
// Callers that need the teardown result call wait() instead.
//
// The body: cancel the generation ctx; closeSocket() UNCONDITIONALLY and BEFORE the join
// (so a parked conn.Read unblocks); seal closing under spawnMu.Lock (§7.B); then spawn
// a SEPARATE goroutine (§7.C — the trigger must never be a member of the set it joins) that
// runs join(timeout) and closes done.
func (e *epoch) teardown(timeout time.Duration) {
	e.closeOnce.Do(func() {
		// Latch the generation as over.
		// The binding constraint is ONLY that this runs before `go e.join(timeout)` below, not that it runs first:
		// everything that publishes a successor generation waits on the join's completion,
		// so latching anywhere ahead of the join puts "this generation ended" strictly before "a successor exists"
		// for every generation-guarded synchronous commit (see connection.commitGate).
		// It sits first because there is no reason to widen the interval in which a straggler's commit is still admitted,
		// but moving it later — ahead of the join — would not break the ordering argument.
		e.markEnded()

		// Single-owner (F4): cancel the generation ctx so ctx-aware tasks unwind.
		e.cancel()

		// Close the socket BEFORE the join so a task parked in conn.Read (which
		// does NOT watch ctx) is unblocked and can exit — otherwise the bounded join
		// would always hit its deadline.
		e.closeSocket()

		// Seal the epoch (§7.B): post-teardown spawns become no-ops, and this Lock
		// establishes the happen-before that makes wg.Wait safe — no 0->1 Add can still
		// be in flight once Lock returns.
		e.spawnMu.Lock()
		e.closing = true
		e.spawnMu.Unlock()

		// §7.C: the join runs on a SEPARATE goroutine so the goroutine that triggered
		// teardown is never a member of the set it waits on, and teardown() itself
		// returns immediately (§5.3 non-blocking initiator).
		go e.join(timeout)
	})
}

// join, run on teardown's separate goroutine, performs the bounded shutdown join and then closes done.
// It (a) joins the transport recv loop via stopTransport
// (nil-guarded until a later task binds it), (b) does the BOUNDED join of
// the per-generation task goroutines — never a bare wg.Wait() — recording ErrCloseTimeout
// with the live-task count on expiry, then (c) closes done to publish the result to wait().
func (e *epoch) join(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// (a) join the transport recv loop, if a transport was bound.
	// Runs HERE, on the teardown goroutine, NOT the supervisor.
	// Nil until a later task sets it.
	if e.stopTransport != nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		if err := e.stopTransport(ctx); err != nil {
			e.log.Warn("epoch: stopTransport returned error during teardown", "error", err)
			// C1: a bounded-Stop timeout (the recv loop wedged in a blocking app handler) is a
			// close-timeout — surface it to wait() so Close reports ErrCloseTimeout rather than nil.
			// The task join (b) below replaces it only when a task is still live.
			e.closeErr = err
		}
		cancel()
	}

	// (b) bounded join: wait for every task goroutine, but never unbounded.
	// The helper goroutine that runs wg.Wait may outlive this select if a task is stuck —
	// that is the stuck task's own leak, which the bounded join deliberately survives.
	joined := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(joined)
	}()

	select {
	case <-joined:
	case <-time.After(time.Until(deadline)):
		// When the transport's Stop used up the deadline, this timer is already expired
		// and can win against tasks that have all exited;
		// keep the transport's error then, rather than a misleading "0 tasks live".
		live := e.liveTasks.Load()
		if live > 0 || e.closeErr == nil {
			e.closeErr = fmt.Errorf("%w: %d tasks live", ErrCloseTimeout, live)
			e.log.Warn("epoch: bounded teardown join timed out", "live_tasks", live, "timeout", timeout)
		}
	}

	// (c) publish the result; wait() reads closeErr after this close (happens-before).
	close(e.done)
}

// wait blocks until teardown's bounded join has completed and returns its result. It is
// the ONLY blocking result-read (Close blocks on it, never the supervisor). Every caller
// observes the same closeErr — nil on a clean join, ErrCloseTimeout on a bounded expiry.
func (e *epoch) wait() error {
	<-e.done

	return e.closeErr
}
