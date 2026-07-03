package hsmsss

// active.go — the active-role Select procedure (spec §6.3, SEMI E37 §7.4). The active side,
// once its dial has advanced the FSM to NotSelected, sends a Select.req (carrying the configured
// SessionID — see the SessionID note at the WriteMessage call below; bounded by T6) over the core
// send path and reaches Selected when the peer answers Select.rsp with select-status 0. On any
// Select failure it drives the FSM back to NotConnected so the engine's reconnect loop re-selects.
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

// runSelectProcedure is the active-role Select procedure goroutine (spec §6.3). It is spawned
// by startActive (tracked by g.proc so Stop joins it) and receives the generation ctx so a
// teardown cancels its pending Select wait. It never blocks Start: Open calls Start
// synchronously, so the handshake must run in the background.
func (t *transport) runSelectProcedure(ctx context.Context) {
	// The FSM is already at NotSelected: startActive calls t.rt.TCPUp BEFORE spawning this
	// goroutine, and TCPUp commits NotConnected -> NotSelected SYNCHRONOUSLY via a guarded CAS
	// (§7.D), so no wait is needed here. That synchronous commit also guarantees the recv loop's
	// status-0 Select.rsp commit (CAS NotSelected -> Selected) finds NotSelected when the peer's
	// reply arrives. A teardown that cancels ctx before/while WriteMessage runs is still handled by
	// the ctx.Err() branch below.
	// SessionID for Select.req: E37 §8.2.6.1 makes the Session ID an association BY REFERENCE between
	// the Select/Deselect control messages and subsequent DATA messages, and §8.3.7.1 / §8.3.22 have
	// Select.rsp / Separate.req carry that same session id — so Select.req carries the CONFIGURED
	// SessionID (WithSessionID; default 0xFFFF), NOT a hard-coded 0xFFFF. Only Linktest.req/.rsp are
	// fixed at 0xFFFF (§8.3.14–19), which NewLinktestReq encodes. HSMS-SS being single-session, the
	// default 0xFFFF is the usual on-wire value; a device that sets WithSessionID selects with it.
	sb := t.rt.NextSystemBytes()
	rsp, err := t.rt.WriteMessage(ctx, hsms.NewSelectReq(t.rt.SessionID(), sb))
	if err != nil {
		// A cancelled generation ctx (teardown / involuntary drop) is NOT a Select failure — the
		// recv loop already reported the drop via TCPDown; just exit so Stop can join us.
		if ctx.Err() != nil {
			return
		}

		// T6 timeout / write error: the peer never completed the Select transaction. Drive the
		// FSM to NotConnected so the reconnect loop re-dials and re-selects (§6.3). T7 (Task 24)
		// is the belt-and-suspenders NotSelected dwell timer; here the failure is explicit.
		t.rt.TCPDown(fmt.Errorf("hsmsss: active Select procedure failed: %w", err))

		return
	}

	// A well-formed Select.rsp with select-status 0 is success. The recv loop has ALREADY
	// committed Selected (H2, see the file header); nothing more to do.
	if rsp == nil || rsp.Type() != hsms.SelectRspType {
		t.rt.TCPDown(errSelectRejected)

		return
	}

	// Select-status 1 (SelectStatusAlreadyActive, "Communication Already Active", E37 Table 7) is ALSO a
	// success, NOT a rejection: it means the peer already considers the link established. This is
	// reachable in a simultaneous-select race where the peer's Select.req arrived on OUR recv loop
	// first — we committed Selected via the responder path (H2, M5) and our own later Select.req is
	// then answered status 1. Tearing down here would drop a validly-Selected link; per E37 §9.4 a
	// non-zero status yields no state transition, and we are already Selected. Any OTHER non-zero
	// status (2 Not Ready, 3 Exhaust, 4+ reserved) IS a genuine Select failure → drop + reconnect.
	if s := selectStatus(rsp); s != hsms.SelectStatusSuccess && s != hsms.SelectStatusAlreadyActive {
		t.rt.TCPDown(errSelectRejected)
	}
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
	conn, err := t.cfg.dial(ctx, "tcp", addr)
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

	t.connMu.Lock()
	t.conn = conn
	t.procCancel = procCancel
	t.connMu.Unlock()

	t.rt.TCPUp(conn)

	g.recv.Add(1)
	go t.recvLoop(g)

	g.proc.Go(func() {
		t.runSelectProcedure(procCtx)
	})
	t.startGate.RUnlock()

	return nil
}
