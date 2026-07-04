package secs1

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errStartSealed is returned by Start when the transport's Add-vs-Wait guard is sealed (a Stop is
// tearing this generation down — the I1 race): rather than register this generation's line engine on
// the WaitGroup bundle concurrently with that generation's Stop Waits, Start rolls back the
// just-dialed/listened socket and returns this. The reconnect loop treats it exactly like a dial
// failure. (Mirrors hsmsss.errStartSealed.)
var errStartSealed = errors.New("secs1: transport stopping — start aborted (I1 guard)")

// Compile-time assertion that *transport satisfies the unexported hsms.transport seam. Expressed as
// a function literal assigned to the blank identifier so the compiler type-checks the body without
// requiring the function to be called. (hsms.transport is unexported, so the cross-package check
// goes THROUGH hsms.NewConnection rather than a var _ hsms.transport = … .)
var _ = func() {
	_, _ = hsms.NewConnection(nil, &transport{})
}

// genWG bundles the per-generation join WaitGroups the SECS-I line engine uses (NEW-1). A FRESH
// genWG is installed for each generation — in newTransport for the first, in ArmStart for every
// reconnect successor — and every goroutine of that generation captures the SAME bundle at spawn: it
// Done()s on the bundle it captured, and Stop Wait()s the bundle it captured. The bundle is
// therefore NEVER shared across generations, so a straggler abandoned by a bounded Stop drains the
// OLD bundle while the next generation Adds to and Waits on a fresh one (no cross-generation
// WaitGroup reuse — which would risk the "reused before previous Wait returned" panic).
type genWG struct {
	line   sync.WaitGroup // the single line-engine goroutine per Start
	accept sync.WaitGroup // the passive accept goroutine per Start; passive only
}

// transport is the SECS-I (SEMI E4) concrete implementation of the unexported hsms.transport seam
// (spec §5.4). It dials (active) or accepts (passive) a net.Conn (typically a *net.TCPConn; a custom
// WithDialer may supply another) and drives the single
// half-duplex line engine (lineEngine) — the ONE goroutine per generation that reads and writes the
// conn (the G-A invariant, see lineIO). Start brings a generation up and auto-commits it to Selected
// (SECS-I has no HSMS-style Select handshake — a live line IS the selected session, D5b-2 / G-B);
// Stop tears it down with a bounded join; ArmStart installs the next generation's fresh bundle and
// channels. Each generation's line engine captures its conn/ctx/channels/bundle as PARAMS at spawn
// and never re-reads the shared fields (NEW-1/I1/C1).
type transport struct {
	cfg Config

	// rt is the transport runtime (the connection core). It is bound ONCE, on the first Start,
	// BEFORE any line engine is spawned, and never re-stored (F7): the core passes the SAME
	// connection singleton to every generation's Start, so a reconnect Start would only re-store the
	// identical value. Writing it write-once makes t.rt effectively immutable — every reader,
	// including a straggler abandoned by a bounded Stop, observes a value written before any reader
	// existed, so there is no cross-generation data race on t.rt.
	rt hsms.TransportRuntime

	connMu   sync.Mutex
	conn     net.Conn         // nil before first successful Start or after Stop clears it
	listener *net.TCPListener // passive only; nil for active; guarded by connMu

	// engineCancel cancels THIS generation's engine ctx. Set (active: in startActive; passive: in
	// startPassive) under connMu and called by Stop (C1) so a Stop-initiated socket close does NOT
	// drive a spurious TCPDown and a straggler cannot TCPDown into a later generation. nil before
	// the first Start / after Stop clears it.
	engineCancel context.CancelFunc

	// startGate + stopping are the transport-level Add-vs-Wait guard (I1): Start registers this
	// generation's goroutines' Adds only while !stopping under RLock; Stop sets stopping under Lock
	// THEN Waits, so no Add races a Wait within a generation.
	startGate sync.RWMutex
	stopping  bool   // guarded by startGate; a Stop is sealing → Start aborts its Adds (errStartSealed)
	wg        *genWG // current generation's join bundle; read/written ONLY under startGate

	// Per-generation send-handoff + teardown plumbing. A FRESH pair is installed by newTransport
	// (first gen) and ArmStart (every reconnect); the engine captures BOTH as params at spawn (it
	// never re-reads these fields — NEW-1/I1). G-C: sendReqCh is NEVER closed (a Write racing a
	// closed channel would panic — the SP5a sender-owned-channel lesson); teardown is the genDone
	// broadcast, closed once by Stop.
	sendReqCh chan *sendReq // per-generation; Write (T4) hands off; the engine consumes; NEVER closed
	genDone   chan struct{} // per-generation broadcast; closed by Stop to unblock a pending Write (T4) + the engine

	// gen publishes the CURRENT generation's (conn, sendReqCh, genDone) as ONE immutable bundle so
	// Write (T4) obtains a self-consistent triple with a SINGLE lock-free atomic Load — and binds the
	// hand-off to exactly this generation's socket (I1). startActive/acceptLoop Store the bundle at the
	// SAME site they publish t.conn (under connMu, so gen's store/clear ordering mirrors t.conn's and
	// inherits the same cross-generation safety — a bounded-Stop straggler's clear can never clobber a
	// successor's publish); Stop clears it (Store(nil)). This is ADDITIVE: the engine still captures
	// sendReqCh/genDone as spawn PARAMS (NEW-1) and NEVER reads gen — gen exists only to give Write an
	// always-consistent snapshot the naive "channels under startGate, then conn under connMu" two-lock
	// read cannot guarantee.
	gen atomic.Pointer[genState]

	// newSink builds a FRESH per-generation inbound sink. The line engine calls it once at spawn so
	// each generation accumulates in isolation — a straggler abandoned by a bounded Stop can never race
	// a successor generation's accumulator (the multi-block assembler holds partial-message state, so a
	// shared sink would be a data race + cross-message corruption). newTransport sets the production
	// default (a fresh assembler that synthesizes HSMS frames and calls rt.DeliverOwnedFrame); tests
	// override it. It is set ONCE and stable for the transport's life (no per-generation swap), so the
	// engine reads the FACTORY without synchronization (the write happens-before the engine spawn); the
	// per-generation isolation comes from CALLING it once per engine, not from swapping the field.
	newSink func() func(block) error

	// metrics holds the SECS-I block/line-level counters (secs1.ConnectionMetrics). It is created once
	// in newTransport and SHARED across every generation's line engine and assembler (unlike newSink's
	// per-generation instances): the counters are a cumulative, connection-lifetime record, and their
	// atomic Add/Load make cross-generation sharing safe. secs1.New hands the same pointer to the
	// consumer via connection.BlockMetrics().
	metrics *ConnectionMetrics
}

// genState bundles ONE generation's live socket and its send/teardown plumbing so Write loads a
// CONSISTENT (conn, sendReqCh, genDone) triple in a SINGLE atomic read — no lock, no multi-field
// window — then verifies its I1 epoch binding (gs.conn == the caller's conn) before handing off. It is
// published as one immutable value behind t.gen at the SAME site startActive/acceptLoop publish
// t.conn, and cleared by Stop. The fields mirror the channel values the engine captured at spawn (so
// Write hands off on exactly this generation's engine channels), never the swappable t.* fields.
type genState struct {
	conn      net.Conn      // this generation's live socket — the I1 binding Write checks (gs.conn == conn)
	sendReqCh chan *sendReq // this generation's hand-off channel (the engine consumes it; NEVER closed)
	genDone   chan struct{} // this generation's teardown broadcast (Stop closes it to release a parked Write)
}

// sendReq is one Write's hand-off to the line engine (T4 produces it; the engine consumes it). done
// is BUFFERED cap 1 (G-C) so the engine's result send never blocks even if the Write goroutine has
// already abandoned the wait via genDone. ALWAYS construct via newSendReq so the cap-1 invariant is
// enforced by construction — the engine's unconditional `req.done <- err` relies on it.
type sendReq struct {
	blocks []block
	done   chan error // buffered cap 1 — see newSendReq
}

// newSendReq builds a sendReq for blocks with the load-bearing cap-1 done channel (G-C). Centralizing
// construction here is what makes the engine's unconditional result send (`req.done <- err`) provably
// non-blocking: even if the Write goroutine has already abandoned the wait via genDone, the buffered
// send succeeds and the value is simply dropped when req is GC'd. T4's Write is the sole producer.
func newSendReq(blocks []block) *sendReq {
	return &sendReq{blocks: blocks, done: make(chan error, 1)}
}

// linePollInterval is the idle "watch both" poll tick (O7 default = short-read-deadline poll): the
// engine arms it as a read deadline to peek for an inbound ENQ, and on timeout re-checks sendReqCh.
// It bounds inbound/outbound wake latency to one tick — negligible vs SECS-I T1 (0.5 s) / T2 (10 s).
// A cross-goroutine SetReadDeadline "poke" from Write (the O7 alternative) is deliberately NOT used:
// it would race the engine's own deadline. Revisit only if a benchmark shows the latency matters.
const linePollInterval = 10 * time.Millisecond

// newTransport constructs a SECS-I transport for cfg with an initial per-generation WaitGroup
// bundle. Called by secs1.New. (T3 will swap in a fresh bundle before each subsequent generation.)
func newTransport(cfg Config) *transport {
	t := &transport{
		cfg:       cfg,
		wg:        &genWG{},
		sendReqCh: make(chan *sendReq), // UNBUFFERED: Write blocks until the engine takes the hand-off
		genDone:   make(chan struct{}),
		metrics:   &ConnectionMetrics{},
	}

	// Production default: each generation's engine builds a FRESH assembler that synthesizes HSMS
	// frames and delivers them to the core via rt.DeliverOwnedFrame. The closure captures t; it is
	// CALLED in lineEngine AFTER t.rt is bound (on the first Start), so t.rt is non-nil at call time.
	// t.cfg is available immediately.
	t.newSink = func() func(block) error {
		return newAssembler(t.cfg, t.rt.DeliverOwnedFrame, t.rt.Timers, t.metrics).accept
	}

	return t
}

// Start binds the transport runtime write-once, derives THIS generation's engine ctx, and dials
// (active) or listens (passive), spawning the single SECS-I line engine on success. rt is the
// TransportRuntime back-channel the engine uses to report TCP lifecycle and deliver blocks.
//
// On dial/listen failure Start returns the error immediately (the core's reconnect loop retries).
// Start does NOT block waiting for a peer: active returns after DialContext + auto-commit + spawning
// the engine; passive returns after ListenTCP + spawning the accept goroutine (the passive peer
// adoption + auto-commit + engine spawn happen on that goroutine).
func (t *transport) Start(ctx context.Context, rt hsms.TransportRuntime) error {
	// Bind rt ONCE, on the first Start, before any line-engine goroutine exists (F7). The core passes
	// the same connection singleton to every generation's Start, so a reconnect Start only re-presents
	// the identical rt — write-once, ahead of any reader, makes t.rt safe to read unsynchronized.
	if t.rt == nil {
		t.rt = rt
	}

	// Derive this generation's engine ctx from ctx (the epoch ctx). engineCancel is Stop's explicit
	// lever: calling it on teardown cancels the engine ctx even when the ctx handed to Start is never
	// cancelled by its parent (e.g. a unit test passing context.Background) — the belt that makes the
	// C1 straggler guard (engineCtx.Err()==nil around every TCPDown) suppress a Stop-initiated TCPDown.
	engineCtx, engineCancel := context.WithCancel(ctx)

	if t.cfg.Active() {
		return t.startActive(engineCtx, engineCancel)
	}

	return t.startPassive(engineCtx, engineCancel)
}

// startActive dials the configured host:port, applies keep-alive, publishes the conn, auto-commits
// the FSM to Selected, and spawns the single line engine. engineCtx/engineCancel scope this
// generation's engine (teardown cancels engineCtx via Stop's engineCancel).
func (t *transport) startActive(engineCtx context.Context, engineCancel context.CancelFunc) error {
	addr := fmt.Sprintf("%s:%d", t.cfg.Host(), t.cfg.Port())

	// Ctx-aware dial (I6): the configured dialer (default (&net.Dialer{}).DialContext, overridable via
	// WithDialer) is ctx-aware, so a teardown that cancels engineCtx aborts an in-flight connect
	// promptly. The DIAL is deliberately OUTSIDE the I1 guard below (never hold startGate across a
	// dial). A custom dialer may return any net.Conn (e.g. an in-memory pipe); keep-alive is applied
	// only to a *net.TCPConn.
	conn, err := t.cfg.dial(engineCtx, "tcp", addr)
	if err != nil {
		engineCancel()

		return fmt.Errorf("secs1: dial %s: %w", addr, err)
	}

	t.applyKeepAlive(conn)

	// I1 Add-vs-Wait guard: register this generation's line engine ONLY while no Stop is sealing. If a
	// voluntary Close sealed this transport after the core published this generation (the I1 race),
	// abort the just-dialed conn instead of racing Stop's Waits.
	t.startGate.RLock()
	if t.stopping {
		t.startGate.RUnlock()
		engineCancel()
		_ = conn.Close()

		return errStartSealed
	}

	// Capture THIS generation's bundle + channels under the SAME RLock that gates the Add (NEW-1): the
	// engine Done()s on — and this generation's Stop Waits — the captured bundle, never t.wg; and it
	// consumes the captured sendReqCh / watches the captured genDone, never the (swappable) fields.
	g := t.wg
	sendReqCh := t.sendReqCh
	genDone := t.genDone

	t.connMu.Lock()
	t.conn = conn
	t.engineCancel = engineCancel
	// Publish this generation's send plumbing as ONE atomic bundle alongside t.conn (G-C/I1): Write
	// loads the CONSISTENT (conn, sendReqCh, genDone) triple with a single atomic Load and binds to this
	// exact socket. Stored under connMu so its store ordering mirrors t.conn's — a Stop clearing an OLD
	// generation cannot clobber this publish (the same guarantee that already protects t.conn).
	t.gen.Store(&genState{conn: conn, sendReqCh: sendReqCh, genDone: genDone})
	t.connMu.Unlock()

	// Auto-commit (D5b-2 / G-B): SECS-I has no Select handshake — a live line IS the selected session.
	// TCPUp (NotConnected->NotSelected) then CommitSelected (NotSelected->Selected) run SYNCHRONOUSLY
	// here, BEFORE the engine spawns, so State()==Selected the instant Start returns and no inbound
	// block is processed before the commit. secs1 is the sole committer, so CommitSelected's bool is
	// ignored.
	t.rt.TCPUp(conn)
	t.rt.CommitSelected()

	g.line.Add(1)
	go t.lineEngine(engineCtx, g, conn, sendReqCh, genDone)
	t.startGate.RUnlock()

	return nil
}

// startPassive listens on the configured host:port and spawns the accept goroutine, then returns (it
// never blocks Open on AcceptTCP). engineCancel is stored under connMu BEFORE the accept goroutine is
// spawned so Stop can always cancel this generation's engine ctx — including in the late-peer race
// where the accept goroutine adopts a peer just as Stop closes the listener (Stop's engineCancel then
// guarantees that late engine's C1 guard suppresses a spurious TCPDown).
func (t *transport) startPassive(engineCtx context.Context, engineCancel context.CancelFunc) error {
	addr := fmt.Sprintf("%s:%d", t.cfg.Host(), t.cfg.Port())

	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		engineCancel()

		return fmt.Errorf("secs1: resolve %s: %w", addr, err)
	}

	// net.ListenTCP sets SO_REUSEADDR, so re-listening on the same fixed port across reconnect
	// generations succeeds once the prior generation's listener is closed by Stop. The LISTEN is
	// outside the I1 guard (never hold startGate across it).
	ln, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		engineCancel()

		return fmt.Errorf("secs1: listen %s: %w", addr, err)
	}

	// Store the listener AND engineCancel before spawning the accept goroutine so Stop can close the
	// listener (to unblock a parked AcceptTCP) and cancel the engine ctx regardless of whether a peer
	// is ever adopted. Storing engineCancel here (not in acceptLoop) means Stop's single connMu read
	// always finds it — closing the late-peer TCPDown window described in the doc above.
	t.connMu.Lock()
	t.listener = ln
	t.engineCancel = engineCancel
	t.connMu.Unlock()

	// I1 Add-vs-Wait guard (mirrors startActive): register the accept goroutine ONLY while no Stop is
	// sealing. If a voluntary Close sealed this transport after the core published this generation,
	// abort the just-created listener instead of racing Stop's g.accept.Wait.
	t.startGate.RLock()
	if t.stopping {
		t.startGate.RUnlock()
		t.connMu.Lock()
		t.listener = nil
		t.connMu.Unlock()
		_ = ln.Close()
		engineCancel()

		return errStartSealed
	}

	// Capture THIS generation's bundle + channels under the RLock that gates the Add (NEW-1); the
	// accept goroutine (and the line engine it spawns) join on this captured bundle and consume/watch
	// these captured channels, never the swappable fields.
	g := t.wg
	sendReqCh := t.sendReqCh
	genDone := t.genDone
	g.accept.Add(1)
	go t.acceptLoop(engineCtx, g, ln, sendReqCh, genDone)
	t.startGate.RUnlock()

	return nil
}

// acceptLoop accepts the FIRST peer connection (adopting it as the single session, auto-committing
// to Selected, and spawning the line engine), then loops accepting-and-immediately-closing any
// FURTHER connection to REFUSE a 2nd peer while one is live (SECS-I is a single point-to-point line).
// It runs on the accept goroutine spawned by startPassive and is joined by Stop via g.accept.
//
// Any AcceptTCP error means the listener was closed by Stop (the sole closer) — the loop exits
// cleanly WITHOUT rt.TCPDown: a first-accept error is a teardown signal, not a comms failure.
// Reconnect after an ESTABLISHED line drops is driven by the line engine's rt.TCPDown, not here.
func (t *transport) acceptLoop(engineCtx context.Context, g *genWG, ln *net.TCPListener, sendReqCh chan *sendReq, genDone chan struct{}) {
	defer g.accept.Done()

	// Accept the FIRST connection — the single live session for this generation.
	conn, err := ln.AcceptTCP()
	if err != nil {
		// Listener closed by Stop before a peer connected. No peer adopted, no engine spawned; exit
		// cleanly so g.accept.Wait unblocks. (engineCancel was stored by startPassive and is called by
		// Stop; it needs no action here.)
		return
	}

	t.applyKeepAlive(conn)

	// Late-accept seal guard (I1 / C1): a peer can be accepted CONCURRENTLY with a Stop that has
	// already sealed this generation — AcceptTCP may dequeue a queued peer even as Stop closes the
	// listener. Re-check the seal under startGate.RLock BEFORE driving ANY runtime callback. The core's
	// CommitConnected/CommitSelected flip the FSM state atomic with an UNGUARDED CAS (only the
	// reaction/notify inject is a no-op after the supervisor stops — hsms/supervisor.go:188/205), so a
	// late TCPUp+CommitSelected here would pulse the state atomic NotConnected->NotSelected->Selected on
	// a generation the supervisor believes is stopped. If a Stop has sealed, this generation is tearing
	// down: close the accepted socket and return WITHOUT TCPUp/CommitSelected/g.line.Add. Mirrors the
	// startActive/startPassive Add-vs-Wait guard; Stop's startGate.Lock serializes with this RLock, so
	// either this publish completes before Stop seals (normal bring-up-then-teardown) or Stop wins and
	// this bails (no spurious pulse, no unaccounted engine).
	t.startGate.RLock()
	if t.stopping {
		t.startGate.RUnlock()
		_ = conn.Close()

		return
	}

	// Publish the socket. The g.line.Add(1) below is issued BEFORE this goroutine can return (the
	// refuse loop keeps it alive until Stop closes ln), so Stop's g.accept.Wait (which precedes the
	// bounded g.line.Wait) makes the Add happen-before that Wait — the Add-vs-Wait guarantee for this
	// generation's engine. All of publish/TCPUp/CommitSelected/Add run under the RLock so a concurrent
	// Stop.Lock cannot seal mid-sequence.
	t.connMu.Lock()
	t.conn = conn
	// Publish this generation's send plumbing as ONE atomic bundle alongside t.conn (see startActive):
	// Write binds the hand-off to this adopted socket via a single atomic Load of a consistent triple.
	t.gen.Store(&genState{conn: conn, sendReqCh: sendReqCh, genDone: genDone})
	t.connMu.Unlock()

	// Auto-commit (D5b-2 / G-B): as on the active path, TCPUp then CommitSelected run SYNCHRONOUSLY
	// here, BEFORE the engine spawns, so no inbound block is processed before the FSM reaches Selected.
	t.rt.TCPUp(conn)
	t.rt.CommitSelected()

	g.line.Add(1)
	go t.lineEngine(engineCtx, g, conn, sendReqCh, genDone)
	t.startGate.RUnlock()

	// Refuse subsequent connections while the accepted one is live: accept-then-immediately-close each
	// 2nd+ dialer so only ONE session exists at a time, WITHOUT disturbing the live line. The loop
	// ends when Stop closes ln (AcceptTCP errors).
	for {
		extra, err := ln.AcceptTCP()
		if err != nil {
			return // listener closed by Stop — the generation is tearing down
		}

		_ = extra.Close()
	}
}

// lineEngine is the single half-duplex SECS-I line goroutine (the G-A load-bearing goroutine): the
// SOLE reader and writer of conn for this generation, driving one lineIO / bufio.Reader (see lineIO's
// G-A invariant). It multiplexes three duties on one goroutine — teardown, outbound send, and inbound
// receive — so there is never a second reader to steal buffered bytes.
//
// It captures conn, engineCtx, sendReqCh, and genDone as PARAMS at spawn and NEVER re-reads the
// transport's shared fields (NEW-1/I1/C1): a straggler abandoned by a bounded Stop therefore acts
// only on ITS generation's socket/ctx/channels, never a successor's.
//
// Loop, each iteration:
//
//	(1) genDone broadcast → exit promptly (teardown).
//	(2) a pending outbound send (Write hands off on sendReqCh) → run it, deliver the result, loop.
//	(3) idle: poll the line for one byte with a short read deadline (linePollInterval) to peek for an
//	    inbound ENQ; a timeout is just an idle tick (loop back to re-check (1)/(2)); a REAL read error
//	    (peer drop / Stop closed the socket) → C1-guarded TCPDown + exit.
//	(4) a byte arrived on the idle line → expect ENQ (§7.8.2): grant the line with EOT, receive the
//	    block, and deliver it to the single inbound sink. A block-level receive error was already
//	    NAK'd by receiveBlock (the peer will RTY) — it is NOT a link teardown, so keep looping.
func (t *transport) lineEngine(engineCtx context.Context, g *genWG, conn net.Conn, sendReqCh chan *sendReq, genDone chan struct{}) {
	defer g.line.Done()

	// ONE lineIO / bufio.Reader for this generation — the G-A single-reader invariant. This goroutine
	// is the only code that ever reads or writes conn bytes.
	line := newLineIO(conn, t.cfg, t.rt.Timers, t.metrics)

	// Build THIS generation's inbound sink once (the per-generation isolation point): the multi-block
	// assembler holds partial-message state, so each engine accumulates in its own instance. A
	// straggler abandoned by a bounded Stop keeps feeding ITS sink, never a successor's (see newSink).
	sink := t.newSink()

	for {
		// (1) Teardown broadcast — exit promptly.
		select {
		case <-genDone:
			return
		default:
		}

		// (2) Pending outbound send? Non-blocking. Write (T4) hands off a *sendReq on sendReqCh.
		select {
		case req := <-sendReqCh:
			err := t.runSend(engineCtx, line, req, sink)
			// req.done is BUFFERED cap 1 (G-C) — this never blocks even if the Write goroutine has
			// already abandoned the wait via genDone.
			req.done <- err

			continue
		case <-genDone:
			return
		default:
		}

		// (3) Idle: poll for an inbound ENQ (O7 short-read-deadline poll).
		b, err := line.readByte(linePollInterval)
		if err != nil {
			if isTimeout(err) {
				continue // idle tick — re-check teardown / a newly-queued send
			}
			// A REAL read error: an involuntary peer drop, or Stop closed the socket. C1 straggler
			// guard — drive TCPDown ONLY for a LIVE generation (engineCtx not cancelled). A Stop
			// closes the socket AFTER calling engineCancel, so a Stop-initiated read error finds
			// engineCtx cancelled and injects no spurious TCPDown.
			if engineCtx.Err() == nil {
				t.rt.TCPDown(fmt.Errorf("secs1: line engine read: %w", err))
			}

			return
		}

		// (4) A byte on the idle line — expect ENQ (§7.8.2): the peer wants to send us a block.
		if b == enq {
			if werr := line.writeByte(eot); werr != nil { // grant the line
				if engineCtx.Err() == nil {
					t.rt.TCPDown(fmt.Errorf("secs1: line engine EOT: %w", werr))
				}

				return
			}

			blk, rerr := line.receiveBlock(engineCtx)
			if rerr != nil {
				// A block-level receive error (T1/T2 timeout, bad length/checksum) was already NAK'd by
				// receiveBlock; the peer will RTY. This is NOT a link teardown — keep looping. Exit
				// promptly, though, if the error is a teardown cancellation (a genuine conn close
				// otherwise surfaces as the next poll's real read error → the (3) TCPDown path).
				if engineCtx.Err() != nil {
					return
				}

				continue
			}

			_ = sink(blk) // this generation's inbound assembler feed (P1 / T5)
		}
		// A non-ENQ byte on an idle line is a protocol violation — ignore it; the line stays up.
	}
}

// runSend transmits every block of req in order over the line, applying the full SEMI E4 §7.8.2
// retry/contention discipline per block (sendBlock). sink is THIS generation's inbound assembler feed,
// passed as sendBlock's deliver callback so a block taken during a slave-contention yield is delivered
// through the same per-generation accumulator, not dropped (P1). It returns the first block's send
// error, or nil once the LAST block ACKs. (Write is a stub in T3, so this has no producer yet; it is
// exercised in T4.)
func (t *transport) runSend(engineCtx context.Context, line *lineIO, req *sendReq, sink func(block) error) error {
	for _, blk := range req.blocks {
		if err := line.sendBlock(engineCtx, blk, t.cfg.RetryLimit(), sink); err != nil {
			return err
		}
	}

	return nil
}

// isTimeout reports whether err is a net.Error deadline expiry (an idle poll tick), distinguishing it
// from a real read error (peer drop / closed socket).
func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// Stop seals the Add-vs-Wait guard, cancels the engine ctx, broadcasts genDone, closes the socket (and
// any pending listener) to unblock the engine's parked poll/receive read, then joins the current
// generation's goroutines BOUNDED by ctx. Normally the engine exits promptly and Stop returns nil; if
// a blocking inbound handler wedges the engine past the deadline, Stop returns ErrCloseTimeout and
// ABANDONS that straggler (engineCancel already fired, so its C1 guard cannot drive a stale TCPDown
// into a later generation). Idempotent for the nil-conn case (Start never connected). Stop is called
// at most once per generation (the core's epoch teardown is closeOnce-guarded), and ArmStart installs
// a fresh genDone before the next generation — so close(genDone) below runs exactly once per channel.
func (t *transport) Stop(ctx context.Context) error {
	// I1 Add-vs-Wait guard: seal the transport BEFORE any Wait below. Taking startGate.Lock waits out
	// any in-flight Start RLock section (its WaitGroup Add), so once it returns no 0->1 Add is in
	// flight; setting stopping then makes every concurrent/subsequent Start abort its Add. Capture THIS
	// generation's bundle + genDone under the seal. genDone is niled here as a consume-once marker: a
	// redundant Stop on the SAME generation (without an intervening ArmStart) captures nil and skips
	// the close below, so the once-per-generation close(genDone) can never double-close and panic.
	t.startGate.Lock()
	t.stopping = true
	g := t.wg
	genDone := t.genDone
	t.genDone = nil
	t.startGate.Unlock()

	t.connMu.Lock()
	conn := t.conn
	ln := t.listener
	engineCancel := t.engineCancel
	t.conn = nil
	t.listener = nil
	t.engineCancel = nil
	// Clear the published generation alongside t.conn (G-C/I1): a Write arriving after this point loads
	// nil and returns ErrConnClosed, while a Write that already loaded the OLD bundle still unblocks via
	// the genDone this Stop closes below.
	t.gen.Store(nil)
	t.connMu.Unlock()

	// C1: cancel the engine ctx FIRST, so the socket-close-driven read error the engine is about to
	// see finds engineCtx cancelled and injects no spurious TCPDown, and a straggler cannot TCPDown
	// into a later generation.
	if engineCancel != nil {
		engineCancel()
	}

	// Broadcast teardown: unblocks a pending Write (T4) and is the engine's prompt-exit signal. Closed
	// exactly once per generation (see the consume-once marker above).
	if genDone != nil {
		close(genDone)
	}

	// Close the listener first (passive) to unblock a parked AcceptTCP in the accept + refuse loops,
	// then the conn (J5) to unblock the engine's parked poll/receive read. Both idempotent.
	if ln != nil {
		_ = ln.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}

	// Join the passive accept goroutine BEFORE the engine. The accept goroutine issues g.line.Add(1)
	// for the engine BEFORE it can return, so joining g.accept first makes that Add happen-before the
	// g.line.Wait below. For active, accept is never Add'd, so this returns immediately.
	g.accept.Wait()

	// Re-read conn under connMu: a passive peer may have connected CONCURRENTLY with the first read
	// above (Stop races the accept goroutine adopting a peer just before the listener closed), so the
	// accept goroutine could have stored a fresh conn — and spawned its engine — after we read nil.
	// g.accept.Wait() has now joined the accept goroutine, so t.conn is stable; close it to unblock
	// that engine's parked read.
	t.connMu.Lock()
	late := t.conn
	t.conn = nil
	// A late passive accept may have re-published gen after the first clear above; clear it again so no
	// live bundle survives Stop (the late generation's genDone was closed above — see genDone capture).
	t.gen.Store(nil)
	t.connMu.Unlock()

	if late != nil {
		_ = late.Close()
	}

	// BOUNDED join of the line engine (§7.A / C1). The engine runs the inbound sink INLINE (T5), so a
	// handler that blocks would wedge g.line.Wait — and therefore Close — forever. Bound it by ctx
	// (epoch.join passes a close-timeout deadline). On expiry ABANDON the straggler (it leaks until the
	// handler returns; engineCancel already fired so it cannot drive a stale TCPDown into a successor)
	// and return ErrCloseTimeout so Close reports it. (A ctx with no deadline — a unit test passing
	// Background — degrades to an unbounded join.)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		g.line.Wait()
	}()

	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: secs1 transport teardown join timed out", hsms.ErrCloseTimeout)
	}
}

// ArmStart clears the Stop-seal (I1) AND installs a FRESH per-generation WaitGroup bundle and
// send-handoff/teardown channels (NEW-1 / G-C) so this generation's Start registers its engine on a
// bundle — and hands off / broadcasts on channels — no prior generation (nor a straggler a bounded
// Stop abandoned) touches. The core calls it immediately before it publishes a fresh generation and
// calls Start, so a live Stop's seal is never undone by a stale arm. The bundle + channel swaps
// happen under the SAME Lock as the seal clear, so a Stop that captured the OLD values still tears
// that generation down while this generation gets clean ones.
func (t *transport) ArmStart() {
	t.startGate.Lock()
	t.stopping = false
	t.wg = &genWG{}
	t.genDone = make(chan struct{})
	t.sendReqCh = make(chan *sendReq) // UNBUFFERED: Write blocks until the engine takes the hand-off
	t.startGate.Unlock()
}

// Write is the SECS-I send half of the §4a seam: it converts one core-framed HSMS DATA frame into
// SECS-I blocks, hands them to THIS generation's line engine, and blocks until the engine reports the
// line-transaction result (or teardown releases it). It is the SOLE producer on sendReqCh.
//
// ctx is intentionally NOT selected on. secs1 forces writeTimeout=0 (D5b-11), so the core arms no
// write deadline: a SECS-I Write may legitimately block for a whole line transaction
// (T2 x (RetryLimit+1)). The engine bounds that transaction, and teardown (genDone) is the release
// for a parked Write — ctx cancellation is not a SECS-I line-abort signal.
//
// Shape + control guard (P0-2): the core supplies bufs whose bufs[0] is a 14-byte prefix
// ([4-byte length][10-byte HSMS header]) and whose bufs[1:] are the body sub-slices. If the HSMS SType
// byte (header[5]) is NON-ZERO the frame is an HSMS control frame (Select/Deselect/Linktest/Separate/
// Reject) — SECS-I has NO control frames, so it is DROPPED with no wire I/O and no hand-off. This is
// what neutralizes the core's graceful writeFarewellSeparate and keeps a v1 SECS-I peer from ever
// seeing HSMS control bytes.
//
// I1 epoch binding + G-C hand-off: t.gen.Load() yields a self-consistent (conn, sendReqCh, genDone)
// snapshot in ONE lock-free atomic read. If it is nil (no live generation) or its conn is not the
// caller's conn (a stale sender pinned to a superseded generation), the write is refused with
// ErrConnClosed — NEVER handed onto a successor generation's channel. Otherwise the blocks are handed
// off and awaited, with genDone releasing BOTH the hand-off and the result wait at teardown
// (sendReqCh is never closed — a send on a closed channel would panic).
func (t *transport) Write(_ context.Context, conn net.Conn, bufs net.Buffers) error {
	// Defensive shape guard (never trust the seam): the core always supplies a >=14-byte prefix.
	if len(bufs) == 0 || len(bufs[0]) < 14 {
		return fmt.Errorf("secs1: Write: malformed frame — need a 14-byte [length][HSMS-header] prefix, got %d buffer(s)", len(bufs))
	}
	header := bufs[0][4:14] // the 10-byte HSMS header

	// Control-frame drop: SType != 0 is an HSMS control frame; SECS-I has none. Drop it silently with
	// no wire I/O and no hand-off (P0-2 — load-bearing for v1 wire-compat).
	if header[5] != 0 {
		return nil
	}

	// Data frame (§4a): map the HSMS header onto the SECS-I message header and split the body.
	blocks, err := t.splitFrame(header, bufs)
	if err != nil {
		return err
	}

	// I1 binding: a SINGLE atomic load yields a consistent (conn, sendReqCh, genDone) triple. Refuse a
	// nil generation (no live line, or post-Stop) or a conn that is not this generation's live socket —
	// never hand off onto a successor's channel.
	gs := t.gen.Load()
	if gs == nil || gs.conn != conn {
		return hsms.ErrConnClosed
	}

	// G-C hand-off: block until the engine takes the request, or teardown (genDone) releases us.
	req := newSendReq(blocks)
	select {
	case gs.sendReqCh <- req:
	case <-gs.genDone:
		return hsms.ErrConnClosed
	}

	// Await the engine's result. req.done is cap-1 buffered (newSendReq), so the engine's unconditional
	// result send never blocks even if teardown wins this select and abandons the wait.
	select {
	case err := <-req.done:
		return err
	case <-gs.genDone:
		return hsms.ErrConnClosed
	}
}

// SetReadDeadline sets the read deadline on conn — the epoch's socket, passed explicitly (never
// re-resolve t.conn). Conn-bound no-op today: the line engine arms its own read deadlines on its
// captured conn (linePollInterval poll, T1/T2 in receiveBlock), so no core caller drives this.
func (t *transport) SetReadDeadline(conn net.Conn, deadline time.Time) error {
	if conn == nil {
		return nil
	}

	return conn.SetReadDeadline(deadline)
}

// SetWriteDeadline sets the write deadline on conn — the same epoch socket handed to Write. Conn-bound
// no-op today: the line engine owns all writes on its captured conn.
func (t *transport) SetWriteDeadline(conn net.Conn, deadline time.Time) error {
	if conn == nil {
		return nil
	}

	return conn.SetWriteDeadline(deadline)
}

// applyKeepAlive enables TCP keep-alive probes on conn when TCPKeepAlive > 0 in the config. conn is
// typically a *net.TCPConn (the passive AcceptTCP result or the default dialer's socket); a custom
// dialer supplied via WithDialer may provide any net.Conn (e.g. an in-memory pipe), for which
// keep-alive does not apply and is silently skipped.
func (t *transport) applyKeepAlive(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return // non-TCP (e.g. net.Pipe in tests): keep-alive is a no-op
	}

	if ka := t.cfg.TCPKeepAlive(); ka > 0 {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(ka)
	}
}
