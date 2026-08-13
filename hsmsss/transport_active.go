package hsmsss

// active.go — the active-role Select procedure (spec §6.3, SEMI E37 §7.4).
// The active side, once its dial has advanced the FSM to NotSelected, sends a Select.req over the core send path,
// and reaches Selected when the peer answers Select.rsp with select-status 0.
// That Select.req carries the HSMS-SS control session ID 0xFFFF (E37.1 §7.1.1) and is bounded by T6;
// see the SessionID note at the WriteMessage call below.
// On any Select failure it drives the FSM back to NotConnected so the engine's reconnect loop re-selects.
//
// H2 note (§7.D): the FSM commit to Selected for the initiator path happens on the RECV
// LOOP the instant it routes the status-0 Select.rsp (see dispatchFrame), NOT here on the
// Select goroutine. That is deliberate and load-bearing: the recv loop is the single
// sequential reader, so committing there guarantees IsSelected() is true BEFORE the recv loop
// dispatches any data frame the peer pipelined right after Select.rsp — closing the efb220b
// spurious-Reject race on the initiator side too. A commit on this (separate) goroutine would
// race the recv loop and lose. This goroutine therefore only drives the request and handles
// the FAILURE path.

import (
	"context"
	"errors"
	"fmt"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errSelectRejected is the TCPDown cause when the peer answers our Select.req with a non-zero
// (failure) select-status: the session cannot be selected on this generation, so the link is
// dropped and the engine reconnects (§6.3 / E37 §7.4).
var errSelectRejected = errors.New("hsmsss: peer rejected Select.req (non-zero select-status)")

// errSelectBadResponse is the TCPDown cause when the frame correlated to our Select.req is not a Select.rsp at all,
// a peer protocol violation (E37 §7.10 requires a response to answer its own transaction).
// It is kept distinct from errSelectRejected so a log reader can tell a peer that declined the select from a peer that answered with the wrong frame.
var errSelectBadResponse = errors.New("hsmsss: peer answered Select.req with a frame that is not a Select.rsp")

// runSelectProcedure is the active-role Select procedure goroutine (spec §6.3). It is spawned
// by startActive (tracked by g.proc so Stop joins it) and receives the generation ctx so a
// teardown cancels its pending Select wait. It never blocks Start: Open calls Start
// synchronously, so the handshake must run in the background.
func (t *transport) runSelectProcedure(ctx context.Context, g *genWG) {
	// The FSM is already at NotSelected: startActive calls t.rt.TCPUp BEFORE spawning this
	// goroutine, and TCPUp commits NotConnected -> NotSelected SYNCHRONOUSLY via a guarded CAS
	// (§7.D), so no wait is needed here. That synchronous commit also guarantees the recv loop's
	// status-0 Select.rsp commit (CAS NotSelected -> Selected) finds NotSelected when the peer's
	// reply arrives. A teardown that cancels ctx before/while WriteMessage runs is still handled by
	// the ctx.Err() branch below.
	// SessionID for Select.req: ALWAYS hsms.ControlSessionID (0xFFFF), never the configured one.
	// E37 §8.2.6.1 makes the Session ID an association by reference between the Select/Deselect control messages and subsequent data messages,
	// but §8.3.4.3 (Select.req) explicitly defers the value to the subsidiary standard.
	// E37.1 §7.1.1 fixes it:
	// "It uses a SessionID value of 0xFFFF and implies that all device IDs are available for communication."
	// §8.1 generalizes that to EVERY HSMS-SS control message.
	// The configured SessionID (WithSessionID) is the DEVICE ID and belongs in data messages only (E37.1 §7.2 / §8.1).
	//
	// Regression note: v2.0.1 sent t.rt.SessionID() here, reading E37 generic without E37.1.
	// Equipment that (correctly) requires 0xFFFF rejected the Select.req and closed the socket whenever the host configured a device ID other than 0xFFFF,
	// producing an endless NotSelected -> NotConnected reconnect loop.
	// v1 hard-coded 0xFFFF; see TestActive_SelectAndSeparateUseControlSessionID.
	sb := t.rt.NextSystemBytes()
	// Named for THIS generation: the transaction is opened on g's own epoch — its reply registry and its socket —
	// so a procedure goroutine abandoned by a bounded Stop can never open a Select transaction on the successor's link.
	rsp, err := t.writeMessage(ctx, g.gen, hsms.NewSelectReq(hsms.ControlSessionID, sb))
	if err != nil {
		// A cancelled generation ctx (teardown / involuntary drop) is NOT a Select failure — the
		// recv loop already reported the drop via TCPDown; just exit so Stop can join us.
		//
		// hsms.ErrConnClosed is the same thing said by the core rather than by our own ctx:
		// either this generation's epoch ctx fired mid-wait, or the send was refused because the generation is over
		// (the two cancellation cascades have no ordering guarantee, so ctx.Err() can still read nil here).
		// Nothing was sent in the refused case, so there is nothing to report and nothing to drop.
		if ctx.Err() != nil || errors.Is(err, hsms.ErrConnClosed) {
			return
		}

		// The Select transaction did not complete.
		// Drive the FSM to NotConnected so the reconnect loop re-dials and re-selects (§6.3).
		// T7 (Task 24) is the belt-and-suspenders NotSelected dwell timer; here the failure is explicit.
		// selectFailureCause splits the outcomes rather than reporting one cause for all of them.
		t.tcpDown(g.gen, fmt.Errorf("hsmsss: active Select procedure failed: %w", err), selectFailureCause(err))

		return
	}

	// A well-formed Select.rsp with select-status 0 is success.
	// The recv loop has ALREADY committed Selected (H2, see the file header); nothing more to do.
	//
	// Anything else correlated to our System Bytes — a Deselect.rsp, a Linktest.rsp — is a peer protocol violation,
	// and it leaves the select ungranted exactly as a failure status would.
	// CauseSelectRejected covers "the peer answered but did not grant the select", which is what a subscriber needs;
	// the error value distinguishes the two shapes for a reader of the log.
	if rsp == nil || rsp.Type() != hsms.SelectRspType {
		t.tcpDown(g.gen, errSelectBadResponse, hsms.CauseSelectRejected)

		return
	}

	// Select-status 1 (SelectStatusAlreadyActive, "Communication Already Active", E37 Table 7) is ALSO a
	// success, NOT a rejection: it means the peer already considers the link established. This is
	// reachable in a simultaneous-select race where the peer's Select.req arrived on OUR recv loop
	// first — we committed Selected via the responder path (H2, M5) and our own later Select.req is
	// then answered status 1. Tearing down here would drop a validly-Selected link; per E37 §7.4.1.3 a
	// non-zero status yields no state transition, and we are already Selected. Any OTHER non-zero
	// status (2 Not Ready, 3 Exhaust, 4+ reserved) IS a genuine Select failure → drop + reconnect.
	if s := selectStatus(rsp); s != hsms.SelectStatusSuccess && s != hsms.SelectStatusAlreadyActive {
		t.tcpDown(g.gen, errSelectRejected, hsms.CauseSelectRejected)
	}
}

// selectFailureCause classifies an active Select transaction that ended in an error,
// so a lifecycle subscriber is told what actually went wrong rather than being handed one catch-all cause.
//
// Three outcomes are distinguishable, and they mean different things to an operator.
// A peer Reject.req correlated to our Select.req is delivered to the waiting sender as a *hsms.RejectError (E37 §7.10):
// the peer answered and refused, nothing on the link failed, so this is a select rejection and NOT an I/O error.
// A T6 expiry means the peer never answered at all.
// Anything left is the transport itself failing — a write error, or a teardown that raced this goroutine's own ctx check.
func selectFailureCause(err error) hsms.TransitionCause {
	var rejectErr *hsms.RejectError
	if errors.As(err, &rejectErr) {
		return hsms.CauseSelectRejected
	}

	if errors.Is(err, hsms.ErrT6Timeout) {
		return hsms.CauseT6Timeout
	}

	return hsms.CauseIOError
}

// selectStatus returns the select-status byte (HSMS header byte 3) of a Select.rsp control
// message. Status 0 (SelectStatusSuccess) means communication is established (E37 §7.4).
func selectStatus(msg hsms.Message) byte {
	h := msg.HeaderBytes()

	return h[3]
}

// startActive dials the configured host:port (via the configured dialer), applies TCP keep-alive if
// set (a no-op for a non-TCP conn), stores the net.Conn, calls rt.TCPUp, and spawns BOTH the recv
// loop and the active Select
// procedure (§6.3). ctx is the generation-lifetime ctx (teardown cancels it); it scopes the
// Select procedure's pending Select wait so a teardown unwinds it. Start MUST NOT block on the
// Select handshake (Open calls Start synchronously), so the procedure runs on its own
// goroutine, joined by Stop via g.proc.
func (t *transport) startActive(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", t.cfg.Host(), t.cfg.Port())

	// Ctx-aware dial (I6): the configured dialer (default (&net.Dialer{}).DialContext, overridable
	// via WithDialer) is ctx-aware, so a teardown that cancels this generation's ctx (the ctx passed
	// to Start) aborts an in-flight connect promptly. net.DialTCP ignored ctx, so Close could block on
	// the reconnect loop's connectLoopWg.Wait for up to the OS connect timeout (~2 min) behind a dial
	// to an unreachable peer. The default dialer resolves the address internally. A custom dialer may
	// return any net.Conn (e.g. an in-memory pipe); keep-alive is applied only to a *net.TCPConn.
	//
	// WithConnectTimeout (Gap 2) additionally bounds THIS attempt: when configured (>0) it derives a
	// per-attempt deadline off ctx so an unreachable/black-holed peer fails fast rather than riding out
	// the OS connect timeout. startActive is called fresh per generation (never in an inner retry loop
	// — the engine's reconnect loop re-invokes Start for each attempt), so a single deferred cancel
	// here is leak-free: it fires when this call returns, never accumulating across attempts.
	dialCtx := ctx
	if t.cfg.connectTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, t.cfg.connectTimeout)
		defer cancel()
	}

	conn, err := t.cfg.dial(dialCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("hsmsss: dial %s: %w", addr, err)
	}

	t.applyKeepAlive(conn)

	// Derive the Select procedure's ctx from the generation ctx so a teardown (which cancels the
	// generation ctx) also cancels a pending Select wait. procCancel is Stop's explicit lever for
	// the same, needed when Start's ctx is never cancelled by its parent (unit tests).
	procCtx, procCancel := context.WithCancel(ctx)

	// I1 Add-vs-Wait guard: register this generation's goroutines (recv loop + Select procedure)
	// ONLY while no Stop is sealing. The DIAL above is deliberately OUTSIDE the guard (never hold
	// startGate across a dial). If a voluntary Close sealed this transport after the core published
	// this generation (the I1 race), abort the just-dialed conn instead of racing Stop's Waits: the
	// reconnect loop then observes shutdown at its F3/G2 fence and returns. rt.TCPUp is kept inside
	// the guard (before the recv-loop spawn) both to satisfy the FSM ordering (NotSelected committed
	// before frames dispatch) and so a sealed generation drives no state.
	t.startGate.RLock()
	if t.stopping {
		t.startGate.RUnlock()
		procCancel()
		_ = conn.Close()

		return errStartSealed
	}

	// Capture THIS generation's WaitGroup bundle under the same RLock that gates the Adds (NEW-1).
	// The recv loop (and the Select procedure) Done() on — and this generation's Stop Waits — this
	// captured bundle, never t.wg, so a straggler abandoned by a bounded Stop drains g while the
	// next generation uses a fresh bundle installed by ArmStart.
	g := t.wg
	// Stamp this generation's identity before anything is spawned.
	// Every goroutine below then reports under the generation it belongs to,
	// rather than whichever is current when it happens to report.
	// The core published the generation before calling Start, so this reads THIS generation's identity.
	g.gen = t.currentGeneration()

	// Report TCP-up BEFORE touching any transport-level bookkeeping
	// (t.conn, the activity stamps, t.procCancel).
	// The core can refuse this generation here —
	// e.ended was already latched by a concurrent teardown that raced this dial to completion,
	// ahead of that same teardown's tr.Stop call —
	// and a refusal means no epoch will ever own conn.
	// Checking first keeps a refused straggler's conn out of t.conn entirely,
	// so it can never clobber a live successor generation's socket or activity stamps,
	// and keeps its dead work out of the join set Stop waits on: no recv loop, no Select procedure.
	// This reordering is safe on the ACCEPTED path too:
	// nothing between here and the connMu block below ever reads t.conn
	// (a write only reads e.liveConn(), already set by tcpUp's own publishSocket call),
	// and no concurrent Stop can race this store either way —
	// the held startGate.RLock fences one out for as long as this whole function runs.
	if !t.tcpUp(g.gen, conn) {
		t.startGate.RUnlock()

		procCancel() // release procCtx; nothing will ever run on it
		_ = conn.Close()

		return errStartSealed
	}

	t.connMu.Lock()
	t.conn = conn
	t.procCancel = procCancel
	t.resetActivityStamps()
	t.connMu.Unlock()

	g.recv.Add(1)
	go t.recvLoop(g)

	g.proc.Go(func() {
		t.runSelectProcedure(procCtx, g)
	})
	t.startGate.RUnlock()

	return nil
}
