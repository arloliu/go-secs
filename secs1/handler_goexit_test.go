package secs1

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// lifecycleLog is a mutex-guarded record of the LifecycleEvent values a test subscription
// observes, for assertions from a goroutine other than the one delivering them.
type lifecycleLog struct {
	mu     sync.Mutex
	events []hsms.LifecycleEvent
}

func (l *lifecycleLog) record(ev hsms.LifecycleEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

// hasCause reports whether any recorded event carries cause.
func (l *lifecycleLog) hasCause(cause hsms.TransitionCause) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, ev := range l.events {
		if ev.Cause == cause {
			return true
		}
	}

	return false
}

// newHookedTransportConn builds a SECS-I connection the same way New does — active/host or
// passive/equipment, mirroring newSECS1Active/newSECS1Passive's role wiring — but also returns the
// underlying *transport, so a test in this package can arm testHookBeforeENQ: a synchronization
// seam (nil in production; see lineIO.testHookBeforeENQ) that lets it deterministically create the
// SEMI E4 §7.8.2.1 line-contention window the scenario below depends on, instead of racing on
// timing.
// It deliberately does not touch newSECS1Active/newSECS1Passive, which many other tests share.
func newHookedTransportConn(t *testing.T, port int, active bool, opts ...Option) (*secs1E, *transport) {
	t.Helper()

	base := []Option{
		WithDeviceID(0),
		WithT1(100 * time.Millisecond),
		WithT2(2 * time.Second),
		WithT4(2 * time.Second),
	}
	if active {
		base = append(base, WithActive(), WithHost(), WithConnectionOption(hsms.WithT3(30*time.Second)))
	} else {
		base = append(base, WithPassive(), WithEquipment(), WithConnectionOption(hsms.WithT3(3*time.Second)))
	}
	base = append(base, opts...)

	cfg, err := NewConfig("127.0.0.1", port, base...)
	require.NoError(t, err)

	tr := newTransport(cfg)
	core, err := hsms.NewConnection(&cfg.ConnectionConfig, tr)
	require.NoError(t, err)

	return &secs1E{conn: &connection{Connection: core, deviceID: cfg.deviceID, metrics: tr.metrics}}, tr
}

// TestSECS1_DataMessageHandlerGoexit_ReleasesPendingWriterAndReconnects proves that when a
// DataMessageHandler calls runtime.Goexit instead of returning, the line engine goroutine running
// it exits, and the connection drops the generation that handler was running for — with
// hsms.CauseHandlerExit — rather than leaving the line selected with no reader.
//
// SECS-I is half-duplex: the same line engine goroutine both reads inbound blocks and services
// queued outbound writes.
// The contention case this test targets is the one [transport.Write] (secs1/transport.go) itself
// documents: it hands its blocks to the line engine and then awaits req.done, and it is the line
// engine's OWN sendBlock call — still in flight, req.done not yet sent — that can deliver an
// inbound block when SEMI E4 §7.8.2.1 line contention hands control to a contending peer mid-send.
// That is reproduced deterministically here: the active (slave/host) endpoint's own outbound send
// is made to be inside sendBlockOnce's wait-for-response loop at the exact moment the passive
// (master/equipment) endpoint's ENQ arrives, via the testHookBeforeENQ seam on both sides —
// timing this from wall-clock alone would be racy in the wrong direction (see the seam's doc).
// Active then yields control (§7.8.2.1), delivers the master's block inline INSIDE its own
// in-flight sendBlock call, and that delivery is what trips the Goexiting handler — while active's
// own Write is still parked on req.done, never on the reply-wait select.
//
// ContentionYieldCount == 1 and DataMsgSendCount == 0 are the test's teeth against silently falling
// back to the non-contention path (deliver via the idle-poll, with active's own send already
// complete): a completed send would leave DataMsgSendCount == 1 (writeFrame counts it only after
// tr.Write returns nil) and ContentionYieldCount == 0.
func TestSECS1_DataMessageHandlerGoexit_ReleasesPendingWriterAndReconnects(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	var claimed atomic.Bool
	entered := make(chan struct{})
	goexiting := hsms.DataMessageHandler(func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		if msg.Function() != 1 {
			return
		}
		if claimed.CompareAndSwap(false, true) {
			close(entered)
			runtime.Goexit()
		}
	})

	active, atr := newHookedTransportConn(t, port, true,
		WithConnectionOption(hsms.WithReconnectBackoff(5*time.Millisecond, 1.0)),
		WithConnectionOption(hsms.WithT5(50*time.Millisecond)),
	)
	passive, ptr := newHookedTransportConn(t, port, false)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	active.conn.AddDataMessageHandler(goexiting)

	log := &lifecycleLog{}
	active.conn.SubscribeLifecycle(log.record)

	// Arm the contention seam on BOTH sides BEFORE Open: each generation's line engine copies the
	// transport's hook onto its own lineIO once, at spawn (transport.lineEngine), so an assignment
	// after Open would not reach the already-running engine.
	//
	// passive's hook signals passiveArrived, THEN blocks — parking the master's line engine
	// immediately before it writes its own ENQ, past its idle poll and short of the wire — until
	// this test releases it.
	// The test waits for passiveArrived before proceeding, rather than assuming passive has already
	// reached the hook by some fixed point: under GOMAXPROCS=1 in particular, passive's own engine
	// goroutine can be slow to dequeue its send request, and releasing before it actually parks
	// would make the release a no-op (a receive from an already-closed channel never blocks) — the
	// exact gap that let this test observe the old idle-poll delivery instead of contention when
	// first written; see the ContentionYieldCount / DataMsgSendCount teeth below.
	passiveArrived := make(chan struct{})
	var passiveOnce sync.Once
	passiveRelease := make(chan struct{})
	ptr.testHookBeforeENQ = func() {
		passiveOnce.Do(func() { close(passiveArrived) })
		<-passiveRelease
	}

	// active's hook only signals and returns — it never parks — so active writes its ENQ and enters
	// sendBlockOnce's wait-for-response loop right after.
	activeArrived := make(chan struct{})
	var activeOnce sync.Once
	atr.testHookBeforeENQ = func() { activeOnce.Do(func() { close(activeArrived) }) }

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	// Start the master's (passive) contending send: it enters sendBlockOnce and parks on the hook,
	// past its idle poll, before writing ENQ.
	go func() {
		_, _ = passive.conn.SendDataMessage(context.Background(), 1, 1, false, secs2.A("trigger"))
	}()

	select {
	case <-passiveArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("passive's contending send never reached the line-contention seam")
	}

	// Start the slave's (active) own pending send, now that passive is confirmed parked.
	// Its hook fires and returns immediately (it does not park), so active writes its ENQ and
	// enters sendBlockOnce's wait-for-response loop right after — the window this test needs open
	// before releasing the master's contending ENQ.
	writeDone := make(chan error, 1)
	go func() {
		_, err := active.conn.SendDataMessage(ctx, 2, 1, true, secs2.A("ping"))
		writeDone <- err
	}()

	select {
	case <-activeArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("active's own ENQ for its pending send was never observed")
	}
	close(passiveRelease) // release the master's contending ENQ now that active is waiting for a response

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the Goexiting handler was never entered")
	}

	select {
	case err := <-writeDone:
		require.ErrorIs(t, err, hsms.ErrConnClosed,
			"the pending write must be released by the disconnect, not by the (far longer) T3 timeout")
	case <-time.After(5 * time.Second):
		t.Fatal("the pending write was never released")
	}

	require.Eventually(t, func() bool { return log.hasCause(hsms.CauseHandlerExit) }, 5*time.Second, 10*time.Millisecond,
		"the disconnect must be reported with the handler-exit cause")

	activeConn, ok := active.conn.(Connection)
	require.True(t, ok, "the test helper's connection must satisfy secs1.Connection")
	require.Equal(t, uint64(1), activeConn.BlockMetrics().ContentionYieldCount(),
		"active must have yielded to the master's contending ENQ mid-send — proves the contention path, not the idle-poll one")
	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgSendCount(),
		"active's own send must never have completed — writeFrame counts it only after tr.Write returns nil")

	waitSECS1Selected(t, active) // the link reconnects
}
