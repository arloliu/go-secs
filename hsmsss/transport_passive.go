package hsmsss

// passive.go — the passive-role connect procedure (spec §6.3, SEMI E37 §6.3.4 / E37.1). The passive
// side LISTENS and ACCEPTS a TCP connection, REFUSES a 2nd connection while one is live (HSMS-SS is
// single-session, §3 / §7.4.3), and does NOT initiate Select — it only RESPONDS to an inbound
// Select.req via the shared H2 responder (handleSelectReq / dispatchFrame in transport.go, §7.D).
// It reconnects through the FSM: a drop of the established link fires the recv loop's rt.TCPDown,
// the engine's reconnect loop then calls tr.Start again, and startPassive re-listens on a fresh
// generation.
//
// ASYNC-START CONTRACT (§6.3 — passive REQUIRES OpenBackground). The engine's Open (and the
// reconnect loop) call tr.Start SYNCHRONOUSLY. Active returns after DialTCP; passive MUST NOT block
// Open waiting for a peer (selection depends on the peer connecting first). So startPassive listens
// synchronously (a listen failure is returned to the caller, retried by the reconnect loop exactly
// like an active dial failure) and then spawns the accept goroutine, returning immediately. The
// accept + rt.TCPUp happen on that goroutine; it is tracked by g.accept and joined by Stop, so no
// accept/refuse goroutine outlives the generation.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/internal/pool"
	"github.com/arloliu/go-secs/v2/logger"
)

// Retry pacing for a transient Accept failure (acceptConn): the delay starts at acceptRetryInitial,
// doubles on each consecutive failure, and is capped at acceptRetryMax — the same shape net/http's
// Server.Serve uses for the same condition.
const (
	acceptRetryInitial = 5 * time.Millisecond
	acceptRetryMax     = time.Second
)

// startPassive listens on the configured host:port and spawns the accept goroutine, then returns
// (it never blocks Open on Accept — see the ASYNC-START CONTRACT above). The listener is created
// synchronously so a listen failure (e.g. port in use) surfaces to the engine's reconnect loop for
// a retry (exponential backoff capped at T5), symmetric with an active DialTCP failure. The
// listener is stored under connMu before the accept goroutine is spawned so Stop can close it to
// unblock a parked Accept.
func (t *transport) startPassive(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", t.cfg.Host(), t.cfg.Port())

	// t.cfg.listen owns address resolution the same way t.cfg.dial already does for the active
	// side. The DEFAULT ListenFunc ((&net.ListenConfig{}).Listen) sets SO_REUSEADDR, so
	// re-listening on the same fixed port across reconnect generations succeeds once the prior
	// generation's listener is closed by Stop. The reconnect loop e.wait()s the prior epoch
	// (whose teardown ran tr.Stop → listener closed + accept goroutine joined) BEFORE this next
	// Start, so there is never an "address already in use" race for the default listener. A
	// custom ListenFunc supplied via WithListener is responsible for its own re-listen semantics
	// if it needs real-port reuse across generations — an in-memory/pipe-backed listener used in
	// tests does not hit this concern at all.
	ln, err := t.cfg.listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("hsmsss: listen %s: %w", addr, err)
	}

	// Store the listener before spawning the accept goroutine so Stop can close it to interrupt a
	// parked Accept (both the first accept and the refuse loop below).
	t.connMu.Lock()
	t.listener = ln
	t.connMu.Unlock()

	// I1 Add-vs-Wait guard (mirrors startActive): register the accept goroutine ONLY while no Stop
	// is sealing. The LISTEN above is outside the guard (never hold startGate across it). If a
	// voluntary Close sealed this transport after the core published this generation, abort the
	// just-created listener instead of racing Stop's g.accept.Wait. acceptLoop's own g.recv.Add
	// (issued before it returns) stays ordered before Stop's g.recv.Wait by Stop joining g.accept
	// first — unchanged — so only this outer g.accept.Add needs the guard.
	t.startGate.RLock()
	if t.stopping {
		t.startGate.RUnlock()
		t.connMu.Lock()
		t.listener = nil
		t.connMu.Unlock()
		_ = ln.Close()

		return errStartSealed
	}
	// Capture THIS generation's WaitGroup bundle under the RLock that gates the Add (NEW-1); the
	// accept goroutine (and the recv loop it spawns) join on this captured bundle, never t.wg.
	g := t.wg
	g.gen = t.currentGeneration() // see the identical stamp in startActive
	g.ctx = ctx                   // see the identical stamp in startActive
	g.accept.Add(1)
	go t.acceptLoop(ctx, g, ln)
	t.startGate.RUnlock()

	return nil
}

// acceptLoop accepts the FIRST peer connection (adopting it as the single session and driving
// rt.TCPUp + the recv loop), then loops accepting and REFUSING any FURTHER connection — per E37
// §9.2.4.1.1 option 1, via refuseExtraConn — while one is live (E37 HSMS-SS single-session, §3 /
// §6.3). It runs on the accept goroutine spawned by startPassive and is joined by Stop via
// g.accept.
//
// It NEVER initiates Select — a passive side only responds to an inbound Select.req (the shared H2
// responder handleSelectReq, driven by the recv loop's dispatchFrame). Both Accept sites go through
// acceptConn, which retries a transient Accept failure and returns an error only once Stop is
// tearing the generation down — so the loop exits cleanly WITHOUT rt.TCPDown: an error here is a
// teardown signal, not a comms failure. Reconnect after an ESTABLISHED link drops is driven by the
// recv loop's rt.TCPDown (as in the active role), not by this loop; a listen failure (never
// reaching here) is retried synchronously by the reconnect loop via startPassive.
func (t *transport) acceptLoop(ctx context.Context, g *genWG, ln net.Listener) {
	defer g.accept.Done()

	// Accept the FIRST connection — the single live session for this generation.
	conn, err := t.acceptConn(ctx, ln)
	if err != nil {
		// Listener closed by Stop (teardown / Close / reconnect) before a peer connected. No peer
		// was adopted and no recv loop was spawned; exit cleanly so g.accept.Wait unblocks.
		return
	}

	t.applyKeepAlive(conn)

	// Record the socket before anything decides its fate, so a refused accept is still reported, and closed through its gate.
	// The record goes on the bundle before the core or the recv loop can reach it.
	rec := t.mintSocket(conn, hsms.SocketAccepted)
	g.sock = rec

	// Report TCP-up BEFORE touching any transport-level bookkeeping (t.conn, the activity stamps).
	// Reported for THIS generation (g.gen), not whichever is current when the report lands.
	// Stop's join of this goroutine is bounded,
	// so an accept that returned just before a teardown can be abandoned and resume after the generation is over —
	// at which point publishing this dead socket, and committing NotSelected,
	// would land on a connection that has already gone down or been closed.
	// The core can refuse it for exactly that reason, and a refusal means no epoch will ever own conn:
	// close it here and return without touching t.conn or spawning the recv loop,
	// so a dead generation's socket can never clobber a live successor's bookkeeping or join set.
	// This reordering is safe on the ACCEPTED path too:
	// nothing between here and the connMu block below ever reads t.conn
	// (a write only reads e.liveConn(), already set by the core's publishSocket during adoptSocket).
	// A concurrent Stop cannot race this store either way:
	// its g.accept.Wait() (transport.go) is unbounded and joins THIS goroutine —
	// refused-and-returned or accepted-and-published — before Stop ever reads t.conn.
	if !t.adoptSocket(g, rec) {
		rec.close(nil)

		return
	}

	// Publish the socket before spawning the recv loop.
	// The g.recv.Add(1) below is issued BEFORE this goroutine can return
	// (the refuse loop keeps it alive until Stop closes ln),
	// so Stop's g.accept.Wait (which precedes g.recv.Wait) makes the Add happen-before that Wait —
	// the §7.B Add-vs-Wait guarantee for this generation's recv loop.
	t.connMu.Lock()
	t.conn = conn
	t.connSock = rec
	t.resetActivityStamps()
	t.connMu.Unlock()

	// TCPUp commits NotConnected → NotSelected SYNCHRONOUSLY via a guarded CAS (§7.D), so the FSM
	// is already at NotSelected the instant TCPUp returns. The shared H2 responder's CommitSelected
	// CAS (NotSelected → Selected) is therefore guaranteed to find NotSelected when it dispatches
	// the peer's Select.req — no fence required. (The old waitPassiveSelectable poll-fence escaped
	// only on rt.Done(), which closes only AFTER tr.Stop's g.accept.Wait joins THIS goroutine: a
	// rare unbounded Close-during-accept-window deadlock, now structurally impossible.)
	g.recv.Add(1)
	go t.recvLoop(g, conn)

	// Refuse subsequent connections while the accepted one is live, per E37 §9.2.4.1.1 option 1
	// (see refuseExtraConn): accept, but answer any Select.req with Communication Already
	// Active, then close — so only ONE session exists at a time, WITHOUT disturbing the live
	// link (E37.1 single-session). refuseExtraConn is called SERIALLY (never as a goroutine per
	// dialer, see its doc comment); a slow dialer delays refusing the next one but that is
	// acceptable, since only one session is ever served.
	// The loop ends when Stop closes ln (Accept errors).
	for {
		extra, err := t.acceptConn(ctx, ln)
		if err != nil {
			return // listener closed by Stop — the generation is tearing down
		}

		t.refuseExtraConn(extra)
	}
}

// acceptConn accepts one connection from ln, retrying an Accept failure that is not a teardown.
//
// An Accept error does NOT by itself mean Stop closed the listener.
// The runtime retries ECONNABORTED internally, but it hands EMFILE / ENFILE (descriptor exhaustion) back to the caller,
// and a ListenFunc supplied via WithListener may return any error at all.
// Treating such an error as teardown would leave the generation stranded:
// the listener stays open, so a peer's connect still lands in the backlog,
// yet nothing accepts it, the FSM stays NotConnected, and no event ever arrives to start a reconnect.
//
// An error is therefore terminal only when the generation is going away:
// the listener reports net.ErrClosed, the generation ctx is cancelled, or Stop has sealed the transport.
// Stop seals (startGate) BEFORE it closes ln, so an Accept that fails because Stop closed a custom listener
// always observes the seal, whatever error that listener returns —
// which is what keeps Stop's unbounded g.accept.Wait from waiting on a retry loop.
// Any other error is logged and retried after a doubling delay capped at acceptRetryMax;
// the wait ends early on ctx cancellation.
func (t *transport) acceptConn(ctx context.Context, ln net.Listener) (net.Conn, error) {
	var delay time.Duration

	for {
		conn, err := ln.Accept()
		if err == nil {
			return conn, nil
		}

		if errors.Is(err, net.ErrClosed) || ctx.Err() != nil || t.isStopping() {
			return nil, err
		}

		if delay == 0 {
			delay = acceptRetryInitial
		} else {
			delay = min(2*delay, acceptRetryMax)
		}

		// Emit the diagnostic off the accept goroutine, on a goroutine nothing joins,
		// so a blocking logger can never stall Stop's unbounded g.accept.Wait (this loop's own
		// caller). At most one diagnostic is outstanding per transport; while one is in flight,
		// further warnings from this loop are silently dropped rather than queued.
		if t.acceptWarnInFlight.CompareAndSwap(false, true) {
			log := t.cfg.Logger()
			if rt, ok := t.rt.(traceConfigRuntime); ok {
				_, log = rt.TraceConfig()
			}

			go t.reportAcceptRetry(log, err, delay)
		}

		timer := pool.GetTimer(delay)
		select {
		case <-ctx.Done():
			pool.PutTimer(timer)

			return nil, err
		case <-timer.C:
			pool.PutTimer(timer)
		}
	}
}

// reportAcceptRetry logs one accept-retry diagnostic.
//
// It runs on a goroutine nothing joins, so a logger that blocks stalls only this one diagnostic,
// never Stop's unbounded g.accept.Wait. log/err/delay are snapshotted at the call site rather than
// captured, so a later accept attempt's values can never leak into an already-running call, and a
// panicking logger is recovered rather than crashing the process.
func (t *transport) reportAcceptRetry(log logger.Logger, err error, delay time.Duration) {
	defer t.acceptWarnInFlight.Store(false)
	defer func() { _ = recover() }()

	log.Warn("hsmsss: accept failed, retrying", "error", err, "retry_in", delay)
}

// isStopping reports whether Stop has sealed the transport for the current generation.
func (t *transport) isStopping() bool {
	t.startGate.RLock()
	defer t.startGate.RUnlock()

	return t.stopping
}

// refuseExtraConn refuses ONE extra dialer while a session is already live, per E37 §9.2.4.1.1 option 1 (the standard's OWN "preferred option"):
// accept, but answer any subsequent Select with Communication Already Active.
// It is called SERIALLY, once per extra dialer, from acceptLoop's refuse loop above — never as a goroutine per dialer.
// A goroutine per dialer would be an unbounded resource under a connect flood, each holding a read deadline and each needing its own registration on the generation WaitGroup for Stop to join;
// serial refusal needs neither.
//
// It owns extra's entire lifecycle, including the close, via its own defer:
// a defer in acceptLoop's loop body would accumulate across iterations instead of closing per iteration.
// The socket is reported refused as soon as it is recorded, which is when this policy decided to refuse it:
// before its exchange, and before the record is published to haltRefusal, the only other side that can close it,
// so the refusal is reported exactly once and always ahead of the close.
// The close goes through the socket's record, once, with the failure retained by whichever branch returned:
// the absolute deadline or another read error, a short read, a malformed length or header, or a failed response write.
// A completed exchange, and a helper that finds Stop already ran, retain nil.
// The record's gate reports the socket closed exactly once, whichever side closes it first.
// The two frames of the exchange are reported to the wire observer with the socket's identity and generation 0:
// the peer's first frame, whole, as soon as its header is read and before it is interpreted,
// and the Select.rsp only once its write returned the full frame without error.
// They are not reported from this goroutine, which Stop joins without a bound:
// the exchange captures each frame as it crosses (refusalCapture),
// the peer's with its At taken when its read completed and the Select.rsp with its At taken when its write was issued,
// and once the exchange has ended hands the capture, still holding the socket's report scope, to a goroutine of its own,
// which reports the frames in wire order and then releases the scope.
// That handoff runs before the deferred close, on a panic too,
// so a haltRefusal close that lands mid-exchange closes the socket at once
// and the close is reported after the frames, by whichever of the gate and that release finds the other done.
//
// Deliberately NOT readFrame/readN.
// readN's idle-first-byte policy explicitly CLEARS the read deadline while waiting for the first byte (transport_recv.go's idle-link policy — correct for an adopted LIVE session, wrong here),
// so an extra dialer that connects and sends nothing would park this serial refusal loop forever.
// T8 (readN's inter-byte bound) would not save it either:
// T8 bounds only gaps BETWEEN bytes, not a total connect-to-close budget,
// so a slow trickle could still hold the socket open indefinitely.
// A single ABSOLUTE deadline covering the whole one-shot exchange — read and write — is the only bound that actually terminates it:
// set once, before the first read, and never cleared or extended.
func (t *transport) refuseExtraConn(extra net.Conn) {
	rec := t.mintSocket(extra, hsms.SocketAccepted)
	t.observeSocket(rec.event(hsms.SocketRefused, nil, time.Now()))

	var closeErr error
	defer func() { rec.close(closeErr) }()

	if err := extra.SetDeadline(t.clock()().Add(t.rt.Timers().T7)); err != nil {
		// Without an armed absolute deadline the promised bound does not exist: close without
		// reading rather than risk an unbounded read on this socket.
		closeErr = err

		return
	}

	// Test-only seam (nil in production, style of the injectable clock/clock()): invoked
	// between Accept returning (acceptLoop's ln.Accept() above) and publishing extra into
	// refuseSock below.
	// It exists ONLY so a test can deterministically drive the Stop-races-prepublication race —
	// Stop closing the listener and setting refuseStopped while THIS extra socket has been
	// accepted but not yet published.
	if t.refusePrePublishHook != nil {
		t.refusePrePublishHook()
	}

	// Two-sided handoff (not a bare field): publish extra's record into the slot ONLY after checking
	// refuseStopped under the SAME mutex Stop's haltRefusal uses.
	// If Stop already ran, it saw no in-flight socket here and is (or is about to be) parked in
	// its g.accept.Wait(); closing via the defer above and returning without reading is what
	// lets that Wait proceed instead of stalling for the full T7 deadline.
	t.refuseMu.Lock()
	if t.refuseStopped {
		t.refuseMu.Unlock()
		return
	}
	t.refuseToken++
	token := t.refuseToken
	t.refuseSock = rec
	t.refuseMu.Unlock()

	// Clear the slot by TOKEN, never by comparing conns:
	// extra's dynamic type comes from WithListener's caller-supplied ListenFunc,
	// which may be non-comparable (e.g. a struct embedding net.Conn plus a slice field),
	// and interface equality panics on a non-comparable dynamic type.
	// See refuseToken's doc comment for why the token still matches under the documented serial-refusal / Stop-joins-acceptLoop-before-ArmStart invariant.
	// Registered after the close defer, so it runs first: the slot is cleared before the socket closes.
	defer func() {
		t.refuseMu.Lock()
		if t.refuseToken == token {
			t.refuseSock = nil
		}
		t.refuseMu.Unlock()
	}()

	// Registered after the close defer, so it runs first, on a panic too:
	// the frames' report and the scope's release are handed off before the deferred close runs the gate,
	// which then leaves the close report to that release, or takes it itself when the release already ran.
	capture := t.captureRefusal(rec)
	if capture != nil {
		defer capture.finish()
	}

	closeErr = t.refusalExchange(extra, capture)
}

// refusalExchange runs the one-shot refusal exchange on extra, whose absolute deadline refuseExtraConn has armed,
// and returns the failure that cut it short, or nil when it completed.
// It records the exchange's frames into capture, when the caller passes one, and never calls the wire observer itself.
//
// Never allocate from the untrusted length
// (the validate-before-allocating discipline of .agents/rules/600-perf-sec.md and readFrame's own J2 guard):
// the frame is read into a fixed 14-byte buffer.
// A bare Select.req is EXACTLY a 10-byte header with no body (E37 §9.3.3.1 — control frames are header-only);
// any other length is a malformed or non-Select first message and is closed unanswered.
func (t *transport) refusalExchange(extra net.Conn, capture *refusalCapture) error {
	var frame [14]byte // [4-byte length prefix || 10-byte header]
	if _, err := io.ReadFull(extra, frame[:4]); err != nil {
		return err
	}

	if n := binary.BigEndian.Uint32(frame[:4]); n != 10 {
		return fmt.Errorf("%w: length %d", errRefusalBadLength, n)
	}

	if _, err := io.ReadFull(extra, frame[4:]); err != nil {
		return err
	}

	if capture != nil {
		capture.inbound(frame[:])
	}

	msg, err := decodeControlFrame(frame[4:]) // decodeControlFrame takes []byte and returns hsms.Message
	if err != nil {
		return err // close without a response
	}

	cm, ok := msg.(*hsms.ControlMessage) // the responder pattern of handleSelectReq (transport_control.go)
	if !ok || cm.Type() != hsms.SelectReqType {
		return errRefusalNotSelectReq // close without a response
	}

	// Status 1 (SelectStatusAlreadyActive / "Communication Already Active"), deliberately NOT
	// status 3 ("Connect Exhaust"). E37 §9.2.4.1.1 names "Communication Already Active"
	// literally as this option's answer, and Table 7 assigns that label to status 1. Status 3's
	// DESCRIPTION reads like the better fit ("the entity is already servicing a separate
	// TCP/IP connection and is unable to service more than one at any given time"), but
	// answering 3 would mean choosing a reasoned alternative instead of implementing option 1
	// as the standard actually specifies it — do not "improve" this to 3.
	rsp, err := hsms.NewSelectRsp(cm, hsms.SelectStatusAlreadyActive)
	if err != nil {
		return err
	}

	out := rsp.ToBytes()

	// The response's At is taken immediately before its write, as at the core's own write sites, not when the write returns.
	var issuedAt time.Time
	if capture != nil {
		issuedAt = time.Now()
	}

	n, err := extra.Write(out) // one bounded 14-byte write; the deadline is already set
	if err != nil {
		return err
	}

	if n != len(out) {
		return io.ErrShortWrite
	}

	if capture != nil {
		capture.outbound(out, issuedAt)
	}

	return nil
}

// haltRefusal is the Stop side of the two-sided refusal handoff (E37 §9.2.4.1.1 option 1,
// refuseExtraConn above): it sets refuseStopped, under the SAME mutex refuseExtraConn checks
// and publishes under, so a helper that has not yet published closes without reading instead
// of parking for the full T7 deadline, and it closes whatever socket IS already published.
// Whichever side "wins" the race, the extra socket gets closed and Stop never waits out the
// refusal deadline.
// The published socket is closed through its record's gate with no failure, because Stop's close is a local one;
// the gate closes the socket at once, which fails the exchange's read or write,
// and leaves the close report to the exchange's report scope when one is still open,
// so the goroutine that reports the exchange's frames delivers it after them,
// and the helper's own deferred close then finds the gate closed and reports nothing more.
// Neither Stop nor the accept goroutine it joins ever waits on that report.
// Called by Stop before g.accept.Wait (transport.go); a no-op (refuseSock nil) when no extra
// dialer is in flight.
func (t *transport) haltRefusal() {
	t.refuseMu.Lock()
	t.refuseStopped = true
	rec := t.refuseSock
	t.refuseSock = nil
	t.refuseMu.Unlock()

	if rec != nil {
		rec.close(nil)
	}
}
