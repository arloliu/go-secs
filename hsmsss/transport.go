package hsmsss

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// errStartSealed is returned by Start when the transport's Add-vs-Wait guard is sealed (a Stop is
// tearing this generation down, the I1 race): rather than register this generation's goroutines on
// its WaitGroup bundle concurrently with that generation's Stop Waits, Start rolls back the
// just-dialed/listened socket and returns this. The reconnect loop treats it exactly like a dial
// failure — it tears the (already torn-down) epoch down again idempotently and re-checks its F3/G2
// fence, which observes the concurrent Close's shutdown and returns.
var errStartSealed = errors.New("hsmsss: transport stopping — start aborted (I1 guard)")

// Compile-time assertion that *transport satisfies the unexported hsms.transport seam.
// Expressed as a function literal assigned to the blank identifier so the compiler type-checks
// the body without requiring the function to be called. (hsms.transport is unexported, so the
// cross-package check goes through hsms.NewConnection rather than a var _ hsms.transport = ….)
var _ = func() {
	_, _ = hsms.NewConnection(nil, &transport{})
}

// genWG bundles the five per-generation join WaitGroups (NEW-1). A FRESH genWG is created for
// each generation — in newTransport for the first, in ArmStart for every reconnect successor —
// and every goroutine of that generation captures the SAME bundle at spawn: it Done()s on the
// bundle it captured and Stop Wait()s the bundle it captured. The bundle is therefore NEVER
// shared across generations.
//
// Why per-generation, not per-transport: a bounded tr.Stop can time out and ABANDON a straggler
// recv goroutine (one wedged in a blocking inline data handler) that still holds recv count >= 1.
// With a single per-transport bundle reused every generation that straggler's stale +1 would
// (i) make EVERY later Stop wait it out — each teardown burns the full close-timeout, inflating
// reconnect latency for as long as the straggler lives — and (ii) risk the runtime panic
// "sync: WaitGroup is reused before previous Wait has returned" when the straggler finally Done()s
// (count->0 wakes the leaked Stop waiter) while the next generation's Start is issuing an Add on
// the same WaitGroup. A fresh bundle per generation isolates each generation's counts: the
// abandoned Stop waiter parks on the OLD bundle (which the straggler eventually drains) while the
// next generation Adds to and Waits on a bundle no leaked waiter ever touches.
type genWG struct {
	recv     sync.WaitGroup // the one recv-loop goroutine per Start call
	proc     sync.WaitGroup // the active Select-procedure goroutine (active only) per Start call
	accept   sync.WaitGroup // the passive accept goroutine (passive only) per Start call
	linktest sync.WaitGroup // auto-linktest goroutines spawned while Selected
	t7       sync.WaitGroup // the T7 NOT-SELECTED dwell goroutine
}

// transport is the HSMS-SS concrete implementation of the unexported hsms.transport seam
// (spec §5.4). It dials (active) or accepts (passive) a net.Conn (typically a *net.TCPConn; a custom
// WithDialer may supply another) — NO bufio.Writer,
// which would defeat writev (§6.2) — and tracks each generation's goroutines via a per-generation
// genWG bundle so Stop can join them (Codex round-7: no recv goroutine outlives its generation).
type transport struct {
	cfg Config
	// rt is the transport runtime (the connection engine). It is bound ONCE, on the first Start,
	// BEFORE any recv loop is ever spawned, and never re-stored (F7): the core passes the SAME
	// connection singleton to every generation's Start (hsms connection_lifecycle.go tr.Start
	// sites), so a reconnect Start would only re-store the identical value. Writing it write-once
	// makes t.rt effectively immutable — every reader, including a recv straggler abandoned by a
	// bounded Stop (readFrame's live-T8 read, or a dispatch of a peer-pipelined frame), observes a
	// value written before any reader existed, so there is no cross-generation data race on t.rt.
	rt hsms.TransportRuntime

	// metrics holds the HSMS-SS-only control-plane counters (linktest, Select/Separate/Reject).
	// It is allocated once in newTransport and never re-stored, so it is safe to read from any
	// goroutine without synchronization; ConnectionMetrics itself is atomic-backed.
	metrics *ConnectionMetrics

	connMu sync.Mutex
	// conn is the current generation's socket; nil before the first successful Start or after Stop
	// clears it. It is typically a *net.TCPConn (the writev fast path in Write, and the target of
	// applyKeepAlive), but a custom dialer supplied via WithDialer may provide any net.Conn (e.g. an
	// in-memory pipe), so the stored type is generalized to net.Conn.
	conn     net.Conn
	listener *net.TCPListener // passive only; nil for active; nil after AcceptTCP completes

	// procCancel cancels the active Select-procedure goroutine's generation-scoped ctx. It is
	// set (active only) by startActive under connMu and called by Stop so the pending Select
	// wait unwinds even when the ctx handed to Start is not itself cancelled (e.g. a unit test
	// that passes context.Background()). nil for passive / before the first active Start.
	procCancel context.CancelFunc

	// startGate + stopping are the transport-level Add-vs-Wait happen-before guard (I1), mirroring
	// the epoch's §7.B spawnMu/closing at transport scope. A voluntary Close that pins a
	// just-published reconnect successor can drive tr.Stop (WaitGroup Waits) concurrently with that
	// same generation's in-flight tr.Start (WaitGroup Adds). Start registers its generation
	// goroutines' Adds only while !stopping under startGate.RLock; Stop sets stopping under
	// startGate.Lock THEN Waits — so once Stop's Lock returns no Add is in flight and no new Add can
	// begin, and an Add can never race a Wait WITHIN a generation. The seal is sticky; ArmStart
	// clears it for the next generation (called by the core before it publishes, ordered before any
	// Close could seal that generation), so a stale arm never undoes a live Stop's seal.
	//
	// wg is the CURRENT generation's join bundle (NEW-1). It is read and written ONLY under
	// startGate: ArmStart installs a FRESH *genWG under Lock; Start captures it under RLock (after
	// the stopping check) and hands it to every goroutine it spawns; Stop captures it under Lock
	// (with stopping) and Waits on that captured bundle. Because ArmStart swaps in a new bundle for
	// the next generation, a bounded Stop that abandoned a straggler leaves that straggler draining
	// the OLD bundle while the next generation uses a fresh one — CROSS-generation WaitGroup reuse
	// is structurally impossible (see genWG).
	startGate sync.RWMutex
	stopping  bool   // guarded by startGate; a Stop is sealing → Start aborts its Adds (errStartSealed)
	wg        *genWG // current generation's join bundle; read/written ONLY under startGate

	// genCtx is the current generation's ctx (Start's ctx = the epoch ctx). Stored so the recv
	// goroutine can derive the auto-linktest sub-context. Written by Start under connMu before the
	// recv loop spawns; read by startLinktest under connMu. (Cross-generation access is serialized
	// by Stop joining the prior recv loop before the next Start.)
	genCtx context.Context // generation-lifetime ctx only; set once per Start (spec §6.3)

	linktestCancel context.CancelFunc // cancels the CURRENT Selected-session's linktest goroutine; guarded by connMu; nil when not Selected

	t7Cancel context.CancelFunc // cancels the current NotSelected-entry's T7 goroutine; guarded by connMu; nil when not armed

	// allocFrame allocates the GC-owned frame buffer read by readFrame. It defaults to
	// makeFrame; it is a test seam (overridden in reader_test.go) so a test can assert that
	// an oversized, attacker-controlled length is rejected BEFORE any allocation (J2).
	allocFrame func(n int) []byte

	// now is the injectable clock for the T8 recv-idle read deadline (readFrame/readN); it
	// defaults to time.Now. It is set only from in-package tests to drive the T8 deadline
	// deterministically. Read via clock(), which is nil-safe so a bare transport{} literal
	// (used in some tests/fuzz) keeps working without setting now.
	now func() time.Time
}

// newTransport constructs a transport for cfg with an initial per-generation WaitGroup bundle.
// Called by hsmsss.New. ArmStart swaps in a fresh bundle before each subsequent generation. The
// clock defaults to time.Now; tests inject t.now to drive the T8 read deadline deterministically.
func newTransport(cfg Config) *transport {
	return &transport{cfg: cfg, metrics: &ConnectionMetrics{}, wg: &genWG{}, allocFrame: makeFrame, now: time.Now}
}

// clock returns the transport's injectable clock, defaulting to time.Now when now is unset. It is
// nil-safe so a bare transport{} literal (used in some tests/fuzz) does not have to set now.
func (t *transport) clock() func() time.Time {
	if t.now != nil {
		return t.now
	}

	return time.Now
}

// Start dials (active) or accepts (passive) a TCP connection, calls rt.TCPUp on success,
// and spawns the per-generation recv loop tracked by g.recv. rt is the TransportRuntime
// back-channel the recv loop uses to deliver TCP-lifecycle events to the connection core.
//
// On dial/listen failure Start returns the error immediately; the engine's reconnect loop handles
// the retry (exponential backoff capped at T5; spec §6.3). Start does NOT block waiting for a peer
// in either role: active returns after DialTCP + spawning the recv loop / Select procedure; passive
// returns after ListenTCP + spawning the accept goroutine (§6.3 — passive REQUIRES OpenBackground,
// so Start must return before a peer connects). The passive accept + rt.TCPUp happen on the
// accept goroutine (tracked by g.accept, joined by Stop); see startPassive in passive.go.
func (t *transport) Start(ctx context.Context, rt hsms.TransportRuntime) error {
	// Bind rt ONCE, on the first Start, before any recv loop is spawned (F7). The core passes the
	// same connection singleton to every generation's Start, so a reconnect Start only re-presents
	// the identical rt — but re-storing it would be a data race against a still-live straggler recv
	// goroutine reading t.rt. Write-once, ahead of any reader, makes t.rt safe to read unsynchronized.
	if t.rt == nil {
		t.rt = rt
	}

	// Publish the generation ctx before spawning any goroutine: startLinktest (on the recv
	// goroutine, at a Selected-commit) derives its auto-linktest sub-context from it under connMu.
	t.connMu.Lock()
	t.genCtx = ctx
	t.connMu.Unlock()

	if t.cfg.Active() {
		return t.startActive(ctx)
	}

	return t.startPassive()
}

// IsActive reports whether this transport is configured for the active (dialing) role.
func (t *transport) IsActive() bool { return t.cfg.Active() }

// ArmStart clears the Stop-seal (I1) AND installs a fresh per-generation WaitGroup bundle (NEW-1)
// so this generation's Start may register its goroutines on a bundle no prior generation touches.
// The core calls it immediately before it publishes a fresh generation and calls Start (under the
// core's publishMu in the reconnect loop, under lifeMu in Open). Ordering it before the publish
// guarantees it happens-before any voluntary Close that could seal that same generation, so a live
// Stop's seal is never undone by a stale arm. The bundle swap happens under the SAME Lock as the
// seal clear, so a Stop that captured the OLD bundle (under Lock, before this ArmStart) still Waits
// on that OLD bundle — where any abandoned straggler decrements — while this generation gets a
// clean bundle (see genWG for why cross-generation sharing would panic / inflate teardown latency).
func (t *transport) ArmStart() {
	t.startGate.Lock()
	t.stopping = false
	t.wg = &genWG{}
	t.startGate.Unlock()
}

// Stop closes the TCP connection (and any pending listener) to unblock the recv loop's parked Read,
// then joins the per-generation goroutines. The recv/proc/linktest/T7 joins are BOUNDED by ctx (the
// close-timeout deadline epoch.join passes): normally no goroutine outlives Stop (round-7), but if a
// data handler wedges the recv goroutine past the deadline, Stop returns ErrCloseTimeout and ABANDONS
// that straggler (fenced by recvLoop's captured-genCtx guard so it cannot drive a stale TCPDown into a
// later generation — C1). Idempotent: safe when Start never connected (nil conn) or Stop already ran.
//
// The engine's epoch teardown calls tr.Stop after closeSocket has already closed the conn
// (J5), so the recv loop's parked Read is unblocked before this join. Stop's own Close
// calls below are the idempotent belt-and-suspenders guard for the transport-level contract.
func (t *transport) Stop(ctx context.Context) error {
	// I1 Add-vs-Wait guard: seal the transport BEFORE any Wait below. Taking startGate.Lock waits
	// out any in-flight Start RLock section (its WaitGroup Adds), so once it returns no 0->1 Add is
	// in flight; setting stopping then makes every concurrent/subsequent Start abort its Adds. This
	// is the transport-scope mirror of the epoch's §7.B seal — it is what makes the g.recv/g.proc/
	// g.accept/... Waits below safe on THIS generation's captured bundle. The seal is sticky; ArmStart
	// (called by the core before it publishes the next generation) clears it and installs a fresh bundle.
	t.startGate.Lock()
	t.stopping = true
	// NEW-1: capture THIS generation's bundle under the seal. A later ArmStart swaps in a fresh one,
	// so even if this Stop times out and returns, its join helper below waits on this OLD bundle
	// (which an abandoned straggler drains) while the next generation Adds to and Waits on its own.
	g := t.wg
	t.startGate.Unlock()

	t.connMu.Lock()
	conn := t.conn
	ln := t.listener
	procCancel := t.procCancel
	t.procCancel = nil
	// Cancel the current Selected-session's auto-linktest goroutine (mirrors procCancel): in
	// production genCtx cancellation at teardown already unwinds it, but this covers a unit test
	// that passes a non-cancelled ctx. g.linktest.Wait below guarantees it does not outlive Stop.
	if t.linktestCancel != nil {
		t.linktestCancel()
		t.linktestCancel = nil
	}
	// Cancel the current NotSelected-entry's T7 dwell goroutine (mirrors linktestCancel): in
	// production genCtx cancellation at teardown already unwinds it, but this covers a unit test
	// that passes a non-cancelled ctx. g.t7.Wait below guarantees it does not outlive Stop.
	if t.t7Cancel != nil {
		t.t7Cancel()
		t.t7Cancel = nil
	}
	t.connMu.Unlock()

	// Cancel the active Select procedure's ctx so a pending Select WriteMessage unwinds and the
	// goroutine can exit — needed even when the ctx handed to Start is never cancelled by its
	// parent. No-op (nil) for passive / before an active Start.
	if procCancel != nil {
		procCancel()
	}

	// Close the listener first to unblock a parked AcceptTCP in the passive accept + refuse loops
	// (passive.go) so the accept goroutine returns and g.accept.Wait below can join it.
	if ln != nil {
		_ = ln.Close()
	}

	// Close the conn we already know about (active: set synchronously by startActive; passive:
	// set once the accept goroutine adopted a peer) to unblock the recv loop's parked Read (J5).
	// Closing an already-closed net.Conn is safe — the error is intentionally discarded.
	if conn != nil {
		_ = conn.Close()
	}

	// Join the passive accept goroutine BEFORE g.recv. The accept goroutine issues its
	// g.recv.Add(1) for the recv loop BEFORE it can return (g.accept.Done fires via defer only
	// after the refuse loop exits), so joining g.accept first makes that Add happen-before the
	// g.recv.Wait below — the §7.B Add-vs-Wait guarantee for the passive spawn. For active,
	// accept is never Add'd, so this returns immediately.
	g.accept.Wait()

	// Re-read conn under connMu: a peer may have connected CONCURRENTLY with the first read above
	// (passive Stop races the accept goroutine adopting a peer just before the listener closed),
	// so the accept goroutine could have stored a fresh conn — and spawned its recv loop — after
	// we read nil. g.accept.Wait() has now joined the accept goroutine, so t.conn is stable; close
	// it to unblock that recv loop's parked Read. Idempotent if it is the same conn closed above.
	t.connMu.Lock()
	late := t.conn
	t.conn = nil
	t.listener = nil
	t.connMu.Unlock()

	if late != nil {
		_ = late.Close()
	}

	// BOUNDED join (§7.A / C1) of the remaining per-generation goroutines. The recv loop runs app
	// data handlers INLINE, so a handler that blocks would wedge g.recv.Wait — and therefore Close —
	// forever (the exact F2-hang class this redesign dissolves). Bound these joins by ctx (epoch.join
	// passes a close-timeout deadline). On expiry ABANDON the straggler goroutines (they leak until
	// the handler returns; recvLoop's gen-ctx guard prevents a late straggler from driving a stale
	// TCPDown into a LATER generation) and return ErrCloseTimeout so Close reports it. All conn /
	// listener state was already niled synchronously above, so a leaked straggler cannot clobber a
	// later generation's socket. (A ctx with no deadline — e.g. a unit test passing Background —
	// degrades to the prior unbounded join.)
	joined := make(chan struct{})
	go func() {
		defer close(joined)

		// proc + recv first (round-7 extended to the Select procedure), then the linktest and
		// T7 dwell goroutines, which exit promptly on their cancelled sub-contexts. All on the
		// captured generation bundle g (NEW-1), never t.wg (a later ArmStart may have swapped it).
		g.proc.Wait()
		g.recv.Wait()
		g.linktest.Wait()
		g.t7.Wait()
	}()

	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: hsmsss transport teardown join timed out", hsms.ErrCloseTimeout)
	}
}

// Write performs a writev of the pre-framed buffers over conn (spec §6.2). conn is the EPOCH's
// socket, captured and passed by the core (I1 full-fix): the transport writes onto exactly this
// conn and does NOT re-resolve its own current t.conn, so a stale sender pinned to an old
// generation writes that generation's (closed) socket — never a successor's. conn is typically a
// *net.TCPConn, so bufs.WriteTo(conn) hits the writev system-call fast path — a bufio.Writer
// wrapper would defeat this and must NOT be added. A custom dialer supplied via WithDialer may
// provide any net.Conn (e.g. an in-memory pipe), in which case the write is a plain WriteTo
// (correct, just not vectored). The connection core serializes Write calls under epoch.writeMu so
// no additional lock is needed here.
//
// The write deadline is deliberately NOT derived from ctx: a cancelled ctx must not truncate
// a partial frame in flight, which would desync the HSMS stream (E1 review carry). Use
// SetWriteDeadline (on the same conn) for bounded writes.
func (t *transport) Write(_ context.Context, conn net.Conn, bufs net.Buffers) error {
	if conn == nil {
		return errors.New("hsmsss: Write: not connected")
	}

	_, err := bufs.WriteTo(conn)

	return err
}

// SetReadDeadline sets the read deadline on conn — the epoch's socket, passed explicitly for
// the same I1 reason as Write/SetWriteDeadline (never re-resolve t.conn). No core caller today
// (the recv loop arms its read deadline on its own captured conn); conn-bound for symmetry.
// No-op when conn is nil.
func (t *transport) SetReadDeadline(conn net.Conn, deadline time.Time) error {
	if conn == nil {
		return nil
	}

	return conn.SetReadDeadline(deadline)
}

// SetWriteDeadline sets the write deadline on conn — the SAME epoch socket the core hands to Write
// (I1 full-fix), so the bounded-write deadline is armed on the exact conn the frame is written to,
// never a successor generation's socket. No-op when conn is nil.
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
