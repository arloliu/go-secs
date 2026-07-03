package hsmsss

// passive.go — the passive-role connect procedure (spec §6.3, SEMI E37 §7.5 / E37.1). The passive
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
	"fmt"
	"net"
)

// startPassive listens on the configured host:port and spawns the accept goroutine, then returns
// (it never blocks Open on AcceptTCP — see the ASYNC-START CONTRACT above). The listener is created
// synchronously so a listen failure (e.g. port in use) surfaces to the engine's reconnect loop for
// a T5-floored retry, symmetric with an active DialTCP failure. The listener is stored under connMu
// before the accept goroutine is spawned so Stop can close it to unblock a parked AcceptTCP.
func (t *transport) startPassive() error {
	addr := fmt.Sprintf("%s:%d", t.cfg.Host(), t.cfg.Port())

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("hsmsss: resolve %s: %w", addr, err)
	}

	// net.ListenTCP sets SO_REUSEADDR, so re-listening on the same fixed port across reconnect
	// generations succeeds once the prior generation's listener is closed by Stop. The reconnect
	// loop e.wait()s the prior epoch (whose teardown ran tr.Stop → listener closed + accept
	// goroutine joined) BEFORE this next Start, so there is never an "address already in use" race.
	ln, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("hsmsss: listen %s: %w", addr, err)
	}

	// Store the listener before spawning the accept goroutine so Stop can close it to interrupt a
	// parked AcceptTCP (both the first accept and the refuse loop below).
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
// rt.TCPUp + the recv loop), then loops accepting-and-immediately-closing any FURTHER connection to
// REFUSE a 2nd peer while one is live (E37 HSMS-SS single-session, §3 / §6.3). It runs on the accept
// goroutine spawned by startPassive and is joined by Stop via g.accept.
//
// It NEVER initiates Select — a passive side only responds to an inbound Select.req (the shared H2
// responder handleSelectReq, driven by the recv loop's dispatchFrame). Any AcceptTCP error means the
// listener was closed by Stop (the sole closer of ln) — the loop exits cleanly WITHOUT rt.TCPDown:
// a first-accept error is a teardown signal, not a comms failure. Reconnect after an ESTABLISHED
// link drops is driven by the recv loop's rt.TCPDown (as in the active role), not by this loop; a
// listen failure (never reaching here) is retried synchronously by the reconnect loop via startPassive.
func (t *transport) acceptLoop(g *genWG, ln *net.TCPListener) {
	defer g.accept.Done()

	// Accept the FIRST connection — the single live session for this generation.
	conn, err := ln.AcceptTCP()
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

	// Refuse subsequent connections while the accepted one is live: accept-then-immediately-close
	// each 2nd+ dialer so only ONE session exists at a time, WITHOUT disturbing the live link
	// (E37.1 single-session). The loop ends when Stop closes ln (AcceptTCP errors).
	for {
		extra, err := ln.AcceptTCP()
		if err != nil {
			return // listener closed by Stop — the generation is tearing down
		}

		// A 2nd peer dialed while a session is already up. Refuse it immediately (close its socket)
		// and keep serving the live connection; never tear the live link down for a late dialer.
		_ = extra.Close()
	}
}
