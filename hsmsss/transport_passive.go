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
// accept/refuse goroutine outlives the generation (Codex round-7 join discipline).

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/arloliu/go-secs/v2/hsms"
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
	g.accept.Add(1)
	go t.acceptLoop(g, ln)
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
// responder handleSelectReq, driven by the recv loop's dispatchFrame). Any Accept error means the
// listener was closed by Stop (the sole closer of ln) — the loop exits cleanly WITHOUT rt.TCPDown:
// a first-accept error is a teardown signal, not a comms failure. Reconnect after an ESTABLISHED
// link drops is driven by the recv loop's rt.TCPDown (as in the active role), not by this loop; a
// listen failure (never reaching here) is retried synchronously by the reconnect loop via startPassive.
func (t *transport) acceptLoop(g *genWG, ln net.Listener) {
	defer g.accept.Done()

	// Accept the FIRST connection — the single live session for this generation.
	conn, err := ln.Accept()
	if err != nil {
		// Listener closed by Stop (teardown / Close / reconnect) before a peer connected. No peer
		// was adopted and no recv loop was spawned; exit cleanly so g.accept.Wait unblocks.
		return
	}

	t.applyKeepAlive(conn)

	// Publish the socket before rt.TCPUp / spawning the recv loop. The g.recv.Add(1) below is
	// issued BEFORE this goroutine can return (the refuse loop keeps it alive until Stop closes
	// ln), so Stop's g.accept.Wait (which precedes g.recv.Wait) makes the Add happen-before that
	// Wait — the §7.B Add-vs-Wait guarantee for this generation's recv loop.
	t.connMu.Lock()
	t.conn = conn
	t.resetActivityStamps()
	t.connMu.Unlock()

	t.rt.TCPUp(conn)

	// TCPUp commits NotConnected → NotSelected SYNCHRONOUSLY via a guarded CAS (§7.D), so the FSM
	// is already at NotSelected the instant TCPUp returns. The shared H2 responder's CommitSelected
	// CAS (NotSelected → Selected) is therefore guaranteed to find NotSelected when it dispatches
	// the peer's Select.req — no fence required. (The old waitPassiveSelectable poll-fence escaped
	// only on rt.Done(), which closes only AFTER tr.Stop's g.accept.Wait joins THIS goroutine: a
	// rare unbounded Close-during-accept-window deadlock, now structurally impossible.)
	g.recv.Add(1)
	go t.recvLoop(g)

	// Refuse subsequent connections while the accepted one is live, per E37 §9.2.4.1.1 option 1
	// (see refuseExtraConn): accept, but answer any Select.req with Communication Already
	// Active, then close — so only ONE session exists at a time, WITHOUT disturbing the live
	// link (E37.1 single-session). refuseExtraConn is called SERIALLY (never as a goroutine per
	// dialer, see its doc comment); a slow dialer delays refusing the next one but that is
	// acceptable, since only one session is ever served.
	// The loop ends when Stop closes ln (Accept errors).
	for {
		extra, err := ln.Accept()
		if err != nil {
			return // listener closed by Stop — the generation is tearing down
		}

		t.refuseExtraConn(extra)
	}
}

// refuseExtraConn refuses ONE extra dialer while a session is already live, per E37 §9.2.4.1.1 option 1 (the standard's OWN "preferred option"):
// accept, but answer any subsequent Select with Communication Already Active.
// It is called SERIALLY, once per extra dialer, from acceptLoop's refuse loop above — never as a goroutine per dialer.
// A goroutine per dialer would be an unbounded resource under a connect flood, each holding a read deadline and each needing its own registration on the generation WaitGroup for Stop to join;
// serial refusal needs neither.
//
// It owns extra's entire lifecycle, including the close, via its own defer:
// a defer in acceptLoop's loop body would accumulate across iterations instead of closing per iteration.
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
	defer func() { _ = extra.Close() }()

	if err := extra.SetDeadline(t.clock()().Add(t.rt.Timers().T7)); err != nil {
		// Without an armed absolute deadline the promised bound does not exist: close without
		// reading rather than risk an unbounded read on this socket.
		return
	}

	// Test-only seam (nil in production, style of the injectable clock/clock()): invoked
	// between Accept returning (acceptLoop's ln.Accept() above) and publishing extra into
	// refuseConn below.
	// It exists ONLY so a test can deterministically drive the Stop-races-prepublication race —
	// Stop closing the listener and setting refuseStopped while THIS extra socket has been
	// accepted but not yet published.
	if t.refusePrePublishHook != nil {
		t.refusePrePublishHook()
	}

	// Two-sided handoff (not a bare field): publish extra into the slot ONLY after checking
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
	t.refuseConn = extra
	t.refuseMu.Unlock()

	// Clear the slot by TOKEN, not by "t.refuseConn == extra":
	// extra's dynamic type comes from WithListener's caller-supplied ListenFunc,
	// which may be non-comparable (e.g. a struct embedding net.Conn plus a slice field),
	// and interface equality panics on a non-comparable dynamic type.
	// See refuseToken's doc comment for why the token still matches under the documented serial-refusal / Stop-joins-acceptLoop-before-ArmStart invariant.
	defer func() {
		t.refuseMu.Lock()
		if t.refuseToken == token {
			t.refuseConn = nil
		}
		t.refuseMu.Unlock()
	}()

	// Never allocate from the untrusted length (the validate-before-allocating discipline of
	// .agents/rules/600-perf-sec.md and readFrame's own J2 guard): read the 4-byte length
	// prefix into a fixed buffer first.
	// A bare Select.req is EXACTLY a 10-byte header with no body (E37 §9.3.3.1 — control frames
	// are header-only); any other length is a malformed or non-Select first message and is
	// closed unanswered.
	var lenBuf [4]byte
	if _, err := io.ReadFull(extra, lenBuf[:]); err != nil {
		return
	}

	if binary.BigEndian.Uint32(lenBuf[:]) != 10 {
		return
	}

	var hdr [10]byte
	if _, err := io.ReadFull(extra, hdr[:]); err != nil {
		return
	}

	msg, err := decodeControlFrame(hdr[:]) // hdr is a [10]byte; decodeControlFrame takes []byte and returns hsms.Message
	cm, ok := msg.(*hsms.ControlMessage)   // the responder pattern of handleSelectReq (transport_control.go)
	if err != nil || !ok || cm.Type() != hsms.SelectReqType {
		return // close without a response
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
		return
	}

	_, _ = extra.Write(rsp.ToBytes()) // one bounded 14-byte write; the deadline is already set
}

// haltRefusal is the Stop side of the two-sided refusal handoff (E37 §9.2.4.1.1 option 1,
// refuseExtraConn above): it sets refuseStopped, under the SAME mutex refuseExtraConn checks
// and publishes under, so a helper that has not yet published closes without reading instead
// of parking for the full T7 deadline, and it closes whatever socket IS already published.
// Whichever side "wins" the race, the extra socket gets closed and Stop never waits out the
// refusal deadline.
// Called by Stop before g.accept.Wait (transport.go); a no-op (refuseConn nil) when no extra
// dialer is in flight.
func (t *transport) haltRefusal() {
	t.refuseMu.Lock()
	t.refuseStopped = true
	conn := t.refuseConn
	t.refuseConn = nil
	t.refuseMu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}
