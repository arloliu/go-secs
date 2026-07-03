package secs1

// transport_lifecycle_test.go — TDD coverage for the T3 transport teardown (Stop) and the passive
// bring-up path. Covers: the clean bounded join (Stop returns nil and joins the idle engine
// promptly), the bounded-join TIMEOUT when the engine is wedged in a blocking inbound handler
// (ErrCloseTimeout, straggler abandoned), and the passive-role synchronous auto-commit + inbound
// delivery. All synchronisation is via channels / require.Eventually — no time.Sleep.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// --- (c) Stop bounded join: clean case ---

// TestTransport_StopCleanJoinReturnsNil brings an idle engine up (active), then Stop(Background)
// must close the socket, cancel the engine ctx, broadcast genDone, and join the engine PROMPTLY,
// returning nil. The C1 guard (engineCancel fired by Stop) must suppress any TCPDown: a Stop-driven
// socket close is a teardown, not a comms failure.
func TestTransport_StopCleanJoinReturnsNil(t *testing.T) {
	rt := newMockRuntime()
	tr, _ := bringUpActive(t, rt, nil)

	done := make(chan error, 1)
	go func() { done <- tr.Stop(context.Background()) }()

	select {
	case err := <-done:
		require.NoError(t, err, "a clean Stop of an idle engine must return nil")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join the idle engine promptly")
	}

	require.False(t, rt.tcpDownFired(),
		"a Stop-initiated socket close must NOT drive a spurious TCPDown (C1 straggler guard)")
}

// --- (c) Stop bounded join: wedged-inbound timeout ---

// TestTransport_StopBoundedJoinTimesOutOnWedgedInbound wedges the line engine inside a blocking
// inbound handler (a channel that never closes) while an inbound block is in flight, so the engine
// holds its g.line count and cannot exit. A bounded Stop must therefore return ErrCloseTimeout and
// ABANDON the straggler (engineCancel already fired, so no late TCPDown into a successor). Releasing
// the wedge at cleanup lets the straggler drain without a panic.
func TestTransport_StopBoundedJoinTimesOutOnWedgedInbound(t *testing.T) {
	rt := newMockRuntime()

	wedge := make(chan struct{})       // never closed while the test runs → inbound blocks forever
	t.Cleanup(func() { close(wedge) }) // registered FIRST → runs LAST: release the abandoned straggler at test end
	entered := make(chan struct{}, 1)

	tr, peer := bringUpActive(t, rt, func(tr *transport) {
		// Per-generation sink FACTORY (T5): return a closure that wedges the engine.
		tr.newSink = func() func(block) error {
			return func(_ block) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-wedge // WEDGE the engine here (stand-in for a blocking inline data handler)

				return nil
			}
		}
	})

	golden := makeTestBlock(t, []byte("wedge"))
	goldenWire := golden.appendTo(nil)
	go func() {
		peerWrite(t, peer, []byte{enq})
		peerReadN(t, peer, 1)          // EOT
		peerWrite(t, peer, goldenWire) // block → receiveBlock ACKs, then the engine wedges in inbound
		peerReadN(t, peer, 1)          // ACK (sent by receiveBlock before inbound is called)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("engine never entered the inbound handler — it never wedged")
	}

	// The engine is wedged in inbound holding g.line. A bounded Stop must time out.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := tr.Stop(ctx)
	require.ErrorIs(t, err, hsms.ErrCloseTimeout,
		"a wedged inbound handler must make the bounded Stop time out and abandon the straggler")
	// No panic; engineCancel fired; the straggler drains when wedge closes at cleanup.
}

// --- Passive role: synchronous auto-commit + inbound delivery ---

// bringUpPassive stands up a passive transport (it LISTENS) and dials it as the scripted peer. The
// auto-commit for passive happens on the accept goroutine (after a peer connects), so State reaching
// Selected is awaited via require.Eventually rather than assumed synchronous with Start.
func bringUpPassive(t *testing.T, rt *mockRuntime, configure func(tr *transport)) (*transport, net.Conn) {
	t.Helper()

	port := freePort(t)
	cfg := transportTestConfig(t, port, WithPassive())
	tr := newTransport(cfg)
	if configure != nil {
		configure(tr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr.ArmStart()
	require.NoError(t, tr.Start(ctx, rt))

	peer := dialPeer(t, port)

	require.Eventually(t, func() bool { return rt.State() == hsms.SelectedState }, 5*time.Second, time.Millisecond,
		"passive accept must TCPUp+CommitSelected the adopted peer")

	return tr, peer
}

// TestTransport_PassiveAutoCommitAndInbound proves the passive path: the accept goroutine adopts the
// dialed peer, auto-commits to Selected, and the engine delivers an inbound ENQ+block to the sink
// and ACKs it. TCPUp is ordered before CommitSelected, as on the active path.
func TestTransport_PassiveAutoCommitAndInbound(t *testing.T) {
	rt := newMockRuntime()
	delivered := make(chan block, 1)
	tr, peer := bringUpPassive(t, rt, func(tr *transport) {
		// Per-generation sink FACTORY (T5): return a capturing closure for this generation.
		tr.newSink = func() func(block) error {
			return func(b block) error {
				delivered <- b

				return nil
			}
		}
	})

	require.Equal(t, []string{"TCPUp", "CommitSelected"}, rt.callOrder(),
		"passive TCPUp must be ordered before CommitSelected")

	golden := makeTestBlock(t, []byte("passive"))
	goldenWire := golden.appendTo(nil)

	ackCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, []byte{enq})
		peerReadN(t, peer, 1) // EOT
		peerWrite(t, peer, goldenWire)
		ackCh <- peerReadN(t, peer, 1) // ACK
	}()

	select {
	case got := <-delivered:
		require.Equal(t, goldenWire, got.appendTo(nil), "engine must deliver the golden block on the passive path")
	case <-time.After(5 * time.Second):
		t.Fatal("passive engine never delivered the inbound block")
	}
	require.Equal(t, []byte{ack}, <-ackCh)

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// TestTransport_PassiveAcceptAfterSealDoesNotCommit is the P0 regression (Codex v1): a peer adopted
// by acceptLoop AFTER a Stop has sealed the generation must NOT drive TCPUp/CommitSelected. The core
// commits flip the FSM state atomic with an UNGUARDED CAS, so without the late-accept seal guard the
// state would pulse to Selected on a generation the supervisor believes is stopped. Here we seal the
// generation (as Stop's first step does) BEFORE the peer connects, so acceptLoop adopts the peer and
// then hits the guard; the subsequent Stop joins the accept goroutine (g.accept.Wait), making the
// assertion deterministic.
//
// Teeth: drop the `if t.stopping { … }` re-check in acceptLoop and this fails — callOrder gains
// TCPUp/CommitSelected and State() reads Selected.
func TestTransport_PassiveAcceptAfterSealDoesNotCommit(t *testing.T) {
	rt := newMockRuntime()

	port := freePort(t)
	cfg := transportTestConfig(t, port, WithPassive())
	tr := newTransport(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr.ArmStart()
	require.NoError(t, tr.Start(ctx, rt))

	// Simulate a Stop winning the seal race: set stopping under the same gate Stop uses, BEFORE a peer
	// is adopted. acceptLoop's AcceptTCP will still return the dialed peer, but the seal guard must then
	// bail without any runtime callback.
	tr.startGate.Lock()
	tr.stopping = true
	tr.startGate.Unlock()

	peer := dialPeer(t, port) // acceptLoop adopts this, then the seal guard makes it bail + close it
	_ = peer

	// Stop's g.accept.Wait joins acceptLoop, so once Stop returns acceptLoop has made its decision.
	require.NoError(t, tr.Stop(context.Background()))

	require.Empty(t, rt.callOrder(),
		"a peer adopted after the Stop seal must NOT drive TCPUp/CommitSelected (late-accept guard)")
	require.Equal(t, hsms.NotConnectedState, rt.State(),
		"the FSM must stay NotConnected — no spurious Selected pulse on a sealed generation")
	require.False(t, rt.tcpDownFired(), "a sealed-generation late accept is not a comms failure — no TCPDown")
}

// TestTransport_PassiveStopBeforePeerConnects proves Stop is clean when a passive transport is torn
// down BEFORE any peer connects: the accept goroutine is unblocked by the listener close and exits
// without adopting a peer or firing TCPDown, and Stop returns nil.
func TestTransport_PassiveStopBeforePeerConnects(t *testing.T) {
	rt := newMockRuntime()

	port := freePort(t)
	cfg := transportTestConfig(t, port, WithPassive())
	tr := newTransport(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr.ArmStart()
	require.NoError(t, tr.Start(ctx, rt))

	done := make(chan error, 1)
	go func() { done <- tr.Stop(context.Background()) }()

	select {
	case err := <-done:
		require.NoError(t, err, "Stop before any peer connects must return nil (accept goroutine exits cleanly)")
	case <-time.After(5 * time.Second):
		t.Fatal("passive Stop did not join the accept goroutine promptly")
	}

	require.False(t, rt.tcpDownFired(), "a first-accept listener close is a teardown, not a comms failure — no TCPDown")
}

// --- Involuntary peer drop on an idle link: the C1 FIRING side ---

// TestTransport_InvoluntaryPeerDropDrivesTCPDown is the POSITIVE side of the C1 discipline (the other
// half of the D5b-10 story): an idle line whose peer drops involuntarily — with NO outbound send in
// flight — must drive TCPDown so the core can tear the generation down and reconnect. The engine's
// idle-poll readByte gets a real (non-timeout) read error and, engineCtx still live (C1), calls
// rt.TCPDown exactly once, then exits. A subsequent Stop (the engine already gone) must NOT fire a
// second TCPDown. Every other TCPDown assertion in the suite is NEGATIVE (a clean Stop must not fire
// one); this pins the branch that actually detects a dead socket.
//
// Teeth: delete the `t.rt.TCPDown(...)` at the idle read-error branch in lineEngine (or invert its
// `engineCtx.Err() == nil` guard) and this fails — tcpDownCh never closes, so the receive times out.
func TestTransport_InvoluntaryPeerDropDrivesTCPDown(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)

	// Simulate an involuntary peer drop on an IDLE link: closing the peer makes the engine's next
	// idle-poll readByte return a real read error (EOF/reset), not a poll-deadline timeout.
	require.NoError(t, peer.Close())

	select {
	case <-rt.tcpDownCh:
	case <-time.After(5 * time.Second):
		t.Fatal("an involuntary peer drop must drive TCPDown from the engine's idle read-error branch")
	}

	rt.tcpDownMu.Lock()
	require.Equal(t, 1, rt.tcpDownCount, "an involuntary drop must drive exactly one TCPDown")
	require.Error(t, rt.tcpDownErr, "the TCPDown cause must carry the wrapped read error")
	rt.tcpDownMu.Unlock()

	// The engine already exited via the drop; Stop must join cleanly and NOT fire a second TCPDown
	// (engineCancel fires before any socket close, and the engine goroutine — the sole TCPDown caller
	// — is already gone).
	require.NoError(t, tr.Stop(context.Background()))

	rt.tcpDownMu.Lock()
	require.Equal(t, 1, rt.tcpDownCount, "Stop after an involuntary drop must not fire a second TCPDown")
	rt.tcpDownMu.Unlock()
}
