package hsmsss

// pergen_wg_test.go — NEW-1 tests: per-generation WaitGroup bundles.
//
// C1's bounded tr.Stop can time out and ABANDON a straggler recv goroutine (one wedged in a
// blocking inline data handler) that still holds its recv WaitGroup count >= 1. With the previous
// per-transport WaitGroups reused every generation, that stale +1 would (i) make EVERY later Stop
// wait it out (teardown-latency inflation) and (ii) risk the runtime panic "sync: WaitGroup is
// reused before previous Wait has returned" when the straggler finally Done()s while the next
// generation's Start is issuing an Add on the same WaitGroup. Per-generation bundles (a fresh
// *genWG installed by ArmStart, captured by each generation's goroutines at spawn) isolate each
// generation's counts.
//
// Consequence (i) is DETERMINISTIC and is the teeth below. Consequence (ii) is inherently a race
// (a straggler Done racing a fresh Start Add) and is exercised by the -race -count gate over the
// reconnect + blocking-handler suite rather than a single deterministic assertion.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestTransport_ArmStartInstallsFreshWaitGroupBundle proves the core NEW-1 mechanism: ArmStart
// installs a FRESH *genWG for the next generation, so a generation never shares WaitGroups with a
// prior one (and thus never with a prior generation's abandoned straggler).
//
// Teeth-check: drop the `t.wg = &genWG{}` swap from ArmStart and the pointers coincide → NotSame bites.
func TestTransport_ArmStartInstallsFreshWaitGroupBundle(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	tr := newTransport(cfg)

	b0 := tr.wg
	require.NotNil(t, b0, "newTransport must install an initial bundle")

	tr.ArmStart()
	b1 := tr.wg
	require.NotSame(t, b0, b1, "ArmStart must install a FRESH bundle for the next generation")

	tr.ArmStart()
	b2 := tr.wg
	require.NotSame(t, b1, b2, "each ArmStart must install a fresh bundle — no cross-generation reuse")
}

// TestTransport_AbandonedStragglerDoesNotStallNextGenerationStop is the faithful, DETERMINISTIC
// NEW-1 teeth (consequence i). Generation 1's recv goroutine wedges inside frame delivery (a
// stand-in for a blocking inline data handler), so gen 1's bounded Stop times out and ABANDONS the
// straggler while it still holds gen 1's recv count. A fresh generation 2 is then Started on the
// SAME transport; its Stop must complete PROMPTLY, proving it Waits only gen 2's own bundle and is
// unaffected by gen 1's still-live straggler.
//
// Teeth-check: revert to a single per-transport bundle shared across generations and gen 2's Stop
// would Wait the shared recv WaitGroup — which still counts gen 1's abandoned straggler — and hang
// until its own close-timeout, tripping the 2s guard below.
func TestTransport_AbandonedStragglerDoesNotStallNextGenerationStop(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)
	tr := newTransport(cfg)

	// ── Generation 1: its recv goroutine WEDGES inside frame delivery ──
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) }) // registered FIRST → runs LAST: release the abandoned straggler at test end

	rt1 := newRecRT()
	rt1.setState(hsms.SelectedState) // Selected → an inbound data frame is DELIVERED (not Rejected)
	rt1.deliverGate = gate
	rt1.deliveredCh = make(chan struct{})

	gen1Ctx, gen1Cancel := context.WithCancel(context.Background())
	peerCh1 := acceptOneAsync(ln)

	tr.ArmStart()
	require.NoError(t, tr.Start(gen1Ctx, rt1))

	peer1 := waitAccept(t, peerCh1, "gen 1")
	t.Cleanup(func() { _ = peer1.Close() })

	// Send a data frame so the recv loop delivers it and WEDGES in DeliverOwnedFrame.
	_, err = peer1.Write(dataFrame(0xFFFF, 1, 1, [4]byte{0, 0, 0, 1}, []byte("wedge")))
	require.NoError(t, err)

	select {
	case <-rt1.deliveredCh:
	case <-time.After(5 * time.Second):
		t.Fatal("gen 1 recv loop never delivered the data frame — it never wedged")
	}
	// A straggler now holds gen 1's recv WaitGroup count == 1 inside DeliverOwnedFrame.

	// Let gen 1's active Select procedure send its Select.req first, establishing generation
	// ordering before the next generation starts. (t.rt itself is bound write-once on the first
	// Start — F7 — so a later Start never re-writes it and cannot race the straggler's reads.)
	require.Eventually(t, func() bool { return rt1.writtenCount() >= 1 }, 2*time.Second, time.Millisecond,
		"gen 1 Select procedure must send its Select.req before the next generation starts")

	// Mirror teardown: cancel the generation ctx, then a BOUNDED Stop that times out on the wedged
	// recv straggler and ABANDONS it (recvLoop's gen-ctx guard keeps the released straggler from
	// injecting a stale TCPDown into a later generation).
	gen1Cancel()
	ctx1, cancel1 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err = tr.Stop(ctx1)
	cancel1()
	require.ErrorIs(t, err, hsms.ErrCloseTimeout,
		"a recv goroutine wedged in delivery must make the bounded Stop time out (straggler abandoned)")

	// ── Generation 2: ArmStart installs a FRESH bundle; gen 1's straggler is still alive holding
	// the OLD bundle's recv count. (The bundle swap itself is asserted deterministically in
	// TestTransport_ArmStartInstallsFreshWaitGroupBundle; here we prove its BEHAVIORAL effect.) ──
	// gen 2 is Started with the SAME runtime rt1: production binds ONE connection to a transport for
	// its whole life, and Start stores rt write-once (F7), so this mirrors the real contract.
	tr.ArmStart()

	peerCh2 := acceptOneAsync(ln)

	gen2Ctx, gen2Cancel := context.WithCancel(context.Background())
	require.NoError(t, tr.Start(gen2Ctx, rt1))
	peer2 := waitAccept(t, peerCh2, "gen 2")
	t.Cleanup(func() { _ = peer2.Close() })

	// Gen 2's recv goroutine is NOT wedged (no data sent). A bounded Stop of gen 2 must return
	// PROMPTLY — proving it Waits ONLY gen 2's bundle b2, unaffected by gen 1's abandoned straggler
	// still holding b1.recv.
	stopped := make(chan error, 1)
	go func() {
		// Mirror production teardown: cancel the generation ctx BEFORE tr.Stop. The epoch always
		// cancels e.ctx before calling tr.Stop, so the recv-loop-armed T7/linktest goroutines exit
		// on their genCtx-derived contexts even under the benign armT7-vs-Stop cancel-field race.
		// (Passing an uncancelled ctx here would leave gen 2's parked T7 to stall Stop's bounded join.)
		gen2Cancel()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		stopped <- tr.Stop(ctx2)
	}()

	select {
	case err := <-stopped:
		require.NoError(t, err,
			"gen 2 Stop must complete cleanly — its bundle is independent of gen 1's abandoned straggler")
	case <-time.After(2 * time.Second):
		t.Fatal("gen 2 Stop STALLED behind gen 1's abandoned straggler — a shared WaitGroup bundle (NEW-1 regression)")
	}
}

// waitAccept returns the next peer conn from ch or fails the test after a bounded wait.
func waitAccept(t *testing.T, ch chan *net.TCPConn, gen string) *net.TCPConn {
	t.Helper()

	select {
	case c := <-ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatalf("%s peer did not accept in time", gen)

		return nil
	}
}
