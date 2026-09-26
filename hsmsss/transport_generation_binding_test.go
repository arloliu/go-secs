package hsmsss

// transport_generation_binding_test.go — unit coverage for the generation-bound recv loop and per-generation timer ownership:
// the recv loop must bind to the socket its OWN generation published, never the transport's current (possibly successor) t.conn,
// and the per-generation timer helpers (armT7/cancelT7/startLinktest/stopLinktest) must touch only the generation bundle they were called with,
// never a shared transport-wide handle a straggler can reach into a live successor through.
//
// The file lives in package hsmsss (not hsmsss_test) for the same reason every other
// transport-internal test file does: only in-package tests can construct *transport values and drive
// armT7/cancelT7/startLinktest/stopLinktest/recvLoop/the wrapper methods directly.
// It reuses newLinktestTransport (transport_procedures_test.go),
// waitT7Exit (transport_procedures_timers_test.go),
// waitLinktestExit (transport_procedures_test.go) and recRT (transport_recv_test.go).
//
// The production code derives from a bundle's own g.ctx (never a transport-wide field), so a
// test that hand-builds a *genWG bare (rather than going through Start/newLinktestTransport) must
// set its ctx field itself wherever that bundle is armed/started — see the individual literals below.

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// ── legacy capability fallback ──────────────────────────────────────

// TestWrappers_FallBackToPlainWithoutGenerationCapability verifies that a runtime that does not
// implement genRuntime (every existing mock, including recRT and
// runtimeWithoutTraceConfig-wrapped runtimes) must still be reachable through the hsmsss
// wrappers — deliverOwnedFrame/routeReply must fall back to the plain
// hsms.TransportRuntime.DeliverOwnedFrame/RouteReply, exactly as dispatchFrame called them before
// the inbound generation fence existed.
// This is the legacy/back-compat half of that fence, not the new capability itself:
// it is regression coverage that the fallback path stays correct now that a capability-offering
// runtime (the real hsms core) exists alongside it, not a test of the generation fence itself.
func TestWrappers_FallBackToPlainWithoutGenerationCapability(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	_, ok := any(rt).(genRuntime)
	require.False(t, ok, "recRT must NOT satisfy genRuntime — this test is only meaningful on the fallback path")

	tr := newLinktestTransport(t, rt, t.Context())

	frame := []byte{0, 0, byte(0), byte(hsms.DataMsgType), 0, 0, 0, 0, 0, 0}
	require.NoError(t, tr.deliverOwnedFrame(42, frame), "deliverOwnedFrame must fall back to plain DeliverOwnedFrame")
	require.Equal(t, 1, rt.deliveredCount(), "the plain rt.DeliverOwnedFrame must have been called exactly once")

	rsp := hsms.NewLinktestReq([4]byte{0, 0, 0, 7})
	require.False(t, tr.routeReply(42, rsp), "routeReply must fall back to plain RouteReply (recRT.RouteReply always misses)")
	require.Len(t, rt.routed, 1, "the plain rt.RouteReply must have been called exactly once")
	require.Same(t, rsp, rt.routed[0], "the exact message given to routeReply must reach the plain RouteReply call")
}

// TestWrappers_FallBackHoldsForTraceStrippedRuntime re-asserts runtimeWithoutTraceConfig
// (transport_recv_test.go) against genRuntime: it embeds only hsms.TransportRuntime,
// so it cannot satisfy genRuntime either.
// Used to confirm the fallback also holds for the trace-capability-stripped double
// other tests in this package already rely on.
func TestWrappers_FallBackHoldsForTraceStrippedRuntime(t *testing.T) {
	t.Parallel()

	base := newRecRT()
	rt := runtimeWithoutTraceConfig{TransportRuntime: base}
	_, ok := any(rt).(genRuntime)
	require.False(t, ok)

	tr := newLinktestTransport(t, rt, t.Context())

	frame := []byte{0, 0, byte(0), byte(hsms.DataMsgType), 0, 0, 0, 0, 0, 0}
	require.NoError(t, tr.deliverOwnedFrame(7, frame))
	require.Equal(t, 1, base.deliveredCount())
}

// ── recvLoop must bind to the socket it was GIVEN ─────────────

// trapConn is a net.Conn whose Read records that it was called and then blocks until Close
// unblocks it (backed by a net.Pipe half). It stands in for "the successor's socket": trapConn's
// whole point is that recvLoop(g, startConn) must NEVER touch it.
type trapConn struct {
	net.Conn
	readCalled atomic.Bool
}

func newTrapConn() (*trapConn, net.Conn) {
	server, client := net.Pipe()

	return &trapConn{Conn: server}, client
}

func (c *trapConn) Read(p []byte) (int, error) {
	c.readCalled.Store(true)

	return c.Conn.Read(p)
}

// TestRecvLoop_BindsToPassedConnNotCurrentConn verifies that recvLoop(g, oldConn)
// must read ONLY oldConn — the socket its own Start call published — never t.conn, even when
// t.conn already points at a "successor" socket by the time the goroutine runs.
//
// Teeth: revert recvLoop to resolve conn := t.conn under connMu (ignoring its startConn/conn
// parameter) and this test fails fast — the trap conn's Read is called almost immediately instead
// of never.
func TestRecvLoop_BindsToPassedConnNotCurrentConn(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	tr := newLinktestTransport(t, rt, t.Context())

	trap, trapPeer := newTrapConn()
	t.Cleanup(func() { _ = trap.Close(); _ = trapPeer.Close() })

	tr.connMu.Lock()
	tr.conn = trap // simulate: by the time this goroutine runs, t.conn is the SUCCESSOR's socket
	tr.connMu.Unlock()

	oldConn, oldPeer := net.Pipe()
	t.Cleanup(func() { _ = oldConn.Close(); _ = oldPeer.Close() })

	// ctx is set (unlike gN/gSucc below) because recvLoop calls armT7(g) unconditionally on entry,
	// which derives a child ctx from g.ctx — a nil g.ctx would panic context.WithCancel.
	g := &genWG{gen: 1, ctx: t.Context()}
	g.recv.Add(1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.recvLoop(g, oldConn)
	}()

	require.Never(t, trap.readCalled.Load, 300*time.Millisecond, 20*time.Millisecond,
		"recvLoop(g, oldConn) must read the conn it was GIVEN, never the transport's current (successor) conn")

	require.NoError(t, oldConn.Close())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recvLoop must exit once its OWN conn (oldConn) is closed")
	}
}

// ── per-generation timer ownership ────────────────────────────

// boundedStop calls tr.Stop bounded by a generous fixed timeout, for tests that only need Stop to
// complete and are not themselves exercising Stop's own bound.
func boundedStop(t *testing.T, tr *transport) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return tr.Stop(ctx)
}

// t7HandleArmed / linktestHandleArmed snapshot a bundle's handle under timerMu and release the
// lock before returning, so a caller can assert on the result WITHOUT holding timerMu — asserting
// inside a locked section is a trap: a failed assertion unwinds via runtime.Goexit and never
// reaches an inline Unlock, wedging any cleanup that later takes the same lock.
func t7HandleArmed(g *genWG) bool {
	g.timerMu.Lock()
	defer g.timerMu.Unlock()

	return g.t7Cancel != nil
}

func linktestHandleArmed(g *genWG) bool {
	g.timerMu.Lock()
	defer g.timerMu.Unlock()

	return g.linktestCancel != nil
}

// TestGenTimers_StragglerCannotTouchSuccessorT7 verifies the successor-isolation guarantee for the T7
// handle: a gen-N straggler's armT7(gN)/cancelT7(gN) calls must install/clear ONLY gN's own
// handle, never reach into the LIVE successor's.
//
// The check is the handle itself, taken under timerMu immediately after each straggler call —
// armT7/cancelT7 mutate their target bundle synchronously before returning, so a wrong write is
// visible at once and no timing window is needed.
// A functional "does gSucc's dwell still fire" check would NOT have teeth here: gN's own
// armT7(gN) call installs a real, running T7 goroutine on gN, so a mutation that touches the
// wrong bundle can still leave something firing — the assertion has to be WHICH bundle holds the
// handle, not whether one eventually fires.
//
// Teeth: have armT7/cancelT7 resolve their target off t.wg instead of the g parameter they were
// called with (this test sets tr.wg = gSucc) — gN.t7Cancel then stays nil after armT7(gN) (the
// straggler's own arm silently landed on gSucc instead), which the first assertion below catches.
func TestGenTimers_StragglerCannotTouchSuccessorT7(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 2 * time.Second}) // long enough that nothing fires during the test

	tr := newLinktestTransport(t, rt, t.Context())

	gN := &genWG{gen: 1, ctx: t.Context()}    // an ended generation, given its own live ctx so armT7(gN) installs a real handle instead of deriving from a nil parent
	gSucc := &genWG{gen: 2, ctx: t.Context()} // the live successor
	tr.wg = gSucc

	t.Cleanup(func() { _ = boundedStop(t, tr) })        // stop + join gSucc's T7 goroutine
	t.Cleanup(func() { tr.cancelT7(gN); gN.t7.Wait() }) // stop + join gN's own T7 goroutine

	tr.armT7(gSucc)
	require.True(t, t7HandleArmed(gSucc), "setup: gSucc's T7 must be armed")

	tr.armT7(gN) // straggler arms its OWN generation's T7
	require.True(t, t7HandleArmed(gN), "the straggler's armT7(gN) must install the handle on gN, not silently land elsewhere")
	require.True(t, t7HandleArmed(gSucc), "a straggler's armT7(gN) must not touch the LIVE successor's T7 handle")

	tr.cancelT7(gN) // straggler: cancel its OWN handle
	require.False(t, t7HandleArmed(gN), "the straggler's cancelT7(gN) must clear gN's own handle")
	require.True(t, t7HandleArmed(gSucc), "a straggler's cancelT7(gN) must not touch the LIVE successor's T7 handle")
}

// TestGenTimers_StragglerCannotTouchSuccessorLinktest mirrors
// TestGenTimers_StragglerCannotTouchSuccessorT7 for the linktest handle: a gen-N straggler's
// startLinktest(gN)/stopLinktest(gN) calls must install/clear ONLY gN's own handle, never the LIVE
// successor's. See that test's doc comment for why the check is a handle snapshot under timerMu
// rather than a functional "does it still send" wait.
//
// Teeth: have startLinktest/stopLinktest resolve their target off t.wg instead of the g parameter
// (this test sets tr.wg = gSucc) — gN.linktestCancel then stays nil after startLinktest(gN), which
// the first assertion below catches.
func TestGenTimers_StragglerCannotTouchSuccessorLinktest(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)
	rt.setLinktest(2*time.Second, 5) // long enough that nothing fires during the test
	rt.setTimers(hsms.TimerConfig{T6: time.Second})
	rt.setWriteMsgFn(func(_ context.Context, msg hsms.Message) (hsms.Message, error) {
		return msg, nil
	})

	tr := newLinktestTransport(t, rt, t.Context())

	gN := &genWG{gen: 1, ctx: t.Context()}
	gSucc := &genWG{gen: 2, ctx: t.Context()}
	tr.wg = gSucc

	t.Cleanup(func() { _ = boundedStop(t, tr) })                  // stop + join gSucc's linktest goroutine
	t.Cleanup(func() { tr.stopLinktest(gN); gN.linktest.Wait() }) // stop + join gN's own linktest goroutine

	tr.startLinktest(gSucc)
	require.True(t, linktestHandleArmed(gSucc), "setup: gSucc's linktest must be started")

	tr.startLinktest(gN) // straggler starts its OWN generation's linktest
	require.True(t, linktestHandleArmed(gN), "the straggler's startLinktest(gN) must install the handle on gN, not silently land elsewhere")
	require.True(t, linktestHandleArmed(gSucc), "a straggler's startLinktest(gN) must not touch the LIVE successor's linktest handle")

	tr.stopLinktest(gN) // straggler: stop its OWN handle
	require.False(t, linktestHandleArmed(gN), "the straggler's stopLinktest(gN) must clear gN's own handle")
	require.True(t, linktestHandleArmed(gSucc), "a straggler's stopLinktest(gN) must not touch the LIVE successor's linktest handle")
}

// TestGenTimers_RefusedAfterStopSealsBundle covers the sealed-bundle case for BOTH timer
// handles: once Stop has sealed and joined a generation's bundle g, arming a T7 dwell OR starting
// a linktest goroutine on that SAME (already-sealed) g must be refused — no Add, no goroutine —
// rather than spawning a goroutine Stop already stopped waiting for.
//
// The refusal is synchronous: check-install-Add is one critical section under g.timerMu, so the
// handle staying nil right after the call is itself the proof — no window needs to elapse.
// The linktest interval must be POSITIVE: startLinktest bails out before the seal check on a
// non-positive one, which would make its half of this test pass without the seal check ever
// running (the same trap the Deselect test documents for T7).
//
// Teeth: revert armT7/startLinktest to skip the per-generation "stopped" latch check, and either
// one installs a real handle on the sealed bundle; the two assertions below are independent, so
// dropping just one seal check still fails this test.
func TestGenTimers_RefusedAfterStopSealsBundle(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 40 * time.Millisecond})
	rt.setLinktest(20*time.Millisecond, 5)

	tr := newLinktestTransport(t, rt, t.Context())
	g := tr.wg

	require.NoError(t, boundedStop(t, tr), "Stop must return (nothing was armed yet)")

	tr.armT7(g)         // arming the SEALED bundle must be refused
	tr.startLinktest(g) // starting linktest on the SEALED bundle must equally be refused

	require.False(t, t7HandleArmed(g), "arming a bundle whose generation Stop already sealed must be refused — no Add, no goroutine")
	require.False(t, linktestHandleArmed(g), "starting linktest on a bundle whose generation Stop already sealed must be refused — no Add, no goroutine")
}

// TestGenTimers_ArmRacingStopNeverAddsAfterJoin covers a third case: an armT7 call that races
// a Stop sealing/joining the SAME bundle must never win by adding to (and spawning a goroutine on)
// a WaitGroup Stop has already Waited to completion on — it must either be refused entirely,
// or Stop's join must actually wait for it.
// The seam (testHookArmT7BeforeLock) pauses armT7 right before its critical section
// so the test can force Stop to complete its bounded join first, deterministically,
// without a time.Sleep-based race.
//
// In this test's construction the pause always lands BEFORE Stop's seal, so Stop's g.t7.Wait
// always completes without waiting for the paused arm's Add (it has not happened yet);
// once released, the arm is therefore always the one that observes the seal and refuses.
// The handle staying nil after armDone is the deterministic proof of that outcome —
// no window needs to elapse.
//
// Teeth: revert armT7 to skip the "was this bundle already sealed by a Stop that already joined"
// check, and the paused call proceeds to Add(1) and spawn a goroutine Stop will never wait for
// again — an orphaned straggler that outlives Stop, and the handle is non-nil below.
func TestGenTimers_ArmRacingStopNeverAddsAfterJoin(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.NotSelectedState)
	rt.setTimers(hsms.TimerConfig{T7: 40 * time.Millisecond})

	tr := newLinktestTransport(t, rt, t.Context())
	g := tr.wg

	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release) // a failed assertion below must not leave the paused arm blocked
	armEntered := make(chan struct{})
	tr.testHookArmT7BeforeLock = func() {
		close(armEntered)
		<-gate
	}

	armDone := make(chan struct{})
	go func() {
		defer close(armDone)
		tr.armT7(g)
	}()

	select {
	case <-armEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("armT7 never reached the pre-lock test hook")
	}

	require.NoError(t, boundedStop(t, tr), "Stop must complete its bounded join while armT7 is paused")

	release()

	select {
	case <-armDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the paused armT7 call never returned after release")
	}

	require.False(t, t7HandleArmed(g),
		"an arm racing a Stop that already sealed/joined this bundle must be refused, "+
			"never left running as an unjoined straggler past Stop")
}
