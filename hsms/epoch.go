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
// Teardown ownership (Task 6, spec §5.1/§7.A/C/E): closeOnce admits exactly one
// teardown owner (F4, replaces the v1 ToClosing CAS); closeErr is written inside the
// once and read after done closes; stopTransport is a hook bound at epoch creation by a
// later task (nil until then) that teardown calls to join the transport recv loop (Codex
// round-7). log is the stored generation logger.
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

	log logger.Logger // generation logger; used by teardown for close-timeout reporting

	connMu sync.RWMutex // guards conn
	conn   net.Conn     // raw *net.TCPConn (no bufio wrapper — defeats writev, §6.2)
	// nil ONLY after closeSocket(), which runs inside teardown's closeOnce.

	wg        sync.WaitGroup // joins every per-generation TASK goroutine (G3)
	liveTasks atomic.Int64   // count of live task goroutines; reported on a close-timeout (§7.A)
	spawnMu   sync.RWMutex   // Add-vs-Wait happen-before guard (§7.B)
	closing   bool           // set under spawnMu.Lock at teardown; post-teardown spawns are no-ops

	closeOnce sync.Once // admits exactly ONE teardown owner (F4, replaces the ToClosing CAS)
	closeErr  error     // written inside closeOnce; read only after <-done

	// stopTransport joins the per-generation transport recv loop (Codex round-7). It is
	// bound to the transport's Stop at epoch creation by a later task; nil until then, so
	// teardown MUST nil-guard it.
	stopTransport func(context.Context) error

	done chan struct{} // created here; closed by teardown when the bounded join completes

	sendCh  chan *sendRequest // PER-GENERATION async send channel; GC'd with the epoch (dissolves C1)
	replies replyRegistry     // per-generation sender-owned reply channels (dissolves F5/F6)
	writeMu sync.Mutex        // serializes the writev over conn (§6.2)

	// commsFailure records the teardown CAUSE for this generation (E37 §9.1.1, spec §5.2/§9.1.1).
	// TCPDown sets it (an involuntary drop / read-write failure / peer Separate), so the
	// NotConnected reaction sends NO courtesy farewell Separate. It stays false for a graceful
	// voluntary Close, which does attempt the bounded best-effort farewell from a live Selected
	// link. It is per-generation (a fresh epoch starts graceful) so no reset is needed.
	commsFailure atomic.Bool
}

// newEpoch creates a fresh generation rooted at parent: ctx/cancel derive from
// context.WithCancel(parent), log is stored for teardown reporting, done is an open channel
// that teardown closes, and the send-path state is initialised — sendCh is buffered to
// sendQueue (the configured senderQueueSize) and replies is a fresh per-generation registry.
func newEpoch(parent context.Context, log logger.Logger, sendQueue int) *epoch {
	ctx, cancel := context.WithCancel(parent)

	return &epoch{
		ctx:     ctx,
		cancel:  cancel,
		log:     log,
		done:    make(chan struct{}),
		sendCh:  make(chan *sendRequest, sendQueue),
		replies: newReplyRegistry(),
	}
}

// liveConn returns the current socket under connMu.RLock — the "socket live?" fact.
func (e *epoch) liveConn() net.Conn {
	e.connMu.RLock()
	defer e.connMu.RUnlock()

	return e.conn
}

// setConn publishes the socket for this generation under connMu.Lock.
func (e *epoch) setConn(c net.Conn) {
	e.connMu.Lock()
	defer e.connMu.Unlock()
	e.conn = c
}

// closeSocket idempotently closes the generation socket under connMu.Lock (NOT the
// RLock path liveConn uses — the close must be exclusive). It guards against a nil
// conn (never set) and against a double close (conn is niled after the first close),
// so repeated calls are safe no-ops. teardown calls it exactly once (inside closeOnce)
// and UNCONDITIONALLY before the join (J5): closing the socket unblocks any task parked
// in conn.Read (which does not watch ctx), so the bounded join can complete.
func (e *epoch) closeSocket() {
	e.connMu.Lock()
	defer e.connMu.Unlock()

	if e.conn != nil {
		_ = e.conn.Close()
		e.conn = nil
	}
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
// (J5, so a parked conn.Read unblocks); seal closing under spawnMu.Lock (§7.B); then spawn
// a SEPARATE goroutine (§7.C — the trigger must never be a member of the set it joins) that
// runs join(timeout) and closes done.
func (e *epoch) teardown(timeout time.Duration) {
	e.closeOnce.Do(func() {
		// Single-owner (F4): cancel the generation ctx so ctx-aware tasks unwind.
		e.cancel()

		// J5: close the socket BEFORE the join so a task parked in conn.Read (which
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

// join, run on teardown's separate goroutine, performs the bounded shutdown join and
// then closes done. It (a) joins the transport recv loop via stopTransport (Codex
// round-7; nil-guarded until a later task binds it), (b) does the §7.A BOUNDED join of
// the per-generation task goroutines — never a bare wg.Wait() — recording ErrCloseTimeout
// with the live-task count on expiry, then (c) closes done to publish the result to wait().
func (e *epoch) join(timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	// (a) round-7: join the transport recv loop, if a transport was bound. Runs HERE,
	// on the teardown goroutine, NOT the supervisor. Nil until a later task sets it.
	if e.stopTransport != nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		if err := e.stopTransport(ctx); err != nil {
			e.log.Warn("epoch: stopTransport returned error during teardown", "error", err)
			// C1: a bounded-Stop timeout (the recv loop wedged in a blocking app handler) is a
			// close-timeout — surface it to wait() so Close reports ErrCloseTimeout rather than nil.
			// The §7.A task join (b) below may overwrite this with its own timeout; both wrap
			// ErrCloseTimeout, so either faithfully reports that teardown could not fully join.
			e.closeErr = err
		}
		cancel()
	}

	// (b) §7.A bounded join: wait for every task goroutine, but never unbounded. The
	// helper goroutine that runs wg.Wait may outlive this select if a task is stuck —
	// that is the stuck task's own leak, which the bounded join deliberately survives.
	joined := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(joined)
	}()

	select {
	case <-joined:
	case <-time.After(time.Until(deadline)):
		live := e.liveTasks.Load()
		e.closeErr = fmt.Errorf("%w: %d tasks live", ErrCloseTimeout, live)
		e.log.Warn("epoch: bounded teardown join timed out", "live_tasks", live, "timeout", timeout)
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
