package secs1

// transport_test.go — TDD coverage for the SP5b T3 line engine + transport lifecycle
// (secs1/transport.go): the single half-duplex line-engine goroutine, the active/passive bring-up
// with the synchronous auto-commit to Selected, inbound ENQ+block delivery, and the NEW-1
// per-generation bundle/channel swap. Each test drives a scripted raw-TCP peer over loopback and a
// mock hsms.TransportRuntime — no engine internals are stubbed, no time.Sleep for synchronisation.

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// mockRuntime is a minimal, goroutine-safe hsms.TransportRuntime for the T3 line-engine tests. It
// records the TCPUp/CommitSelected call ORDER (so a test can prove the auto-commit is synchronous
// and correctly ordered) and every TCPDown cause (so the C1 straggler-guard teeth-check can observe
// a spurious teardown). The FSM State is a simple atomic that TCPUp advances to NotSelected and
// CommitSelected advances to Selected, mirroring the real runtime's synchronous commits.
type mockRuntime struct {
	mu    sync.Mutex
	order []string   // sequence of "TCPUp" / "CommitSelected" calls (call-order assertions)
	conns []net.Conn // every conn handed to TCPUp

	state atomic.Uint32 // hsms.ConnState

	tcpDownMu    sync.Mutex
	tcpDownErr   error
	tcpDownCount int
	tcpDownCh    chan struct{}
	tcpDownOnce  sync.Once

	// Inbound frame capture: DeliverOwnedFrame records every delivered frame and, if a test armed a
	// capture channel via captureFrames, also signals it. Used by the T5 default-sink integration test.
	deliverMu sync.Mutex
	delivered [][]byte
	deliverCh chan []byte

	sysGen atomic.Uint32
	timers hsms.TimerConfig
}

// mockRuntime must satisfy the exported hsms.TransportRuntime seam (an external package CAN name and
// implement it — all method params are exported types).
var _ hsms.TransportRuntime = (*mockRuntime)(nil)

func newMockRuntime() *mockRuntime {
	m := &mockRuntime{
		tcpDownCh: make(chan struct{}),
		timers:    hsms.DefaultConnectionConfig().Timers(),
	}
	m.state.Store(uint32(hsms.NotConnectedState))

	return m
}

func (m *mockRuntime) TCPUp(conn net.Conn) {
	m.mu.Lock()
	m.order = append(m.order, "TCPUp")
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	// Mirror the real runtime: NotConnected -> NotSelected committed synchronously.
	m.state.Store(uint32(hsms.NotSelectedState))
}

func (m *mockRuntime) CommitSelected() bool {
	m.mu.Lock()
	m.order = append(m.order, "CommitSelected")
	m.mu.Unlock()
	// Mirror the real runtime: NotSelected -> Selected committed synchronously.
	m.state.Store(uint32(hsms.SelectedState))

	return true
}

func (m *mockRuntime) TCPDown(cause error) {
	m.tcpDownMu.Lock()
	m.tcpDownErr = cause
	m.tcpDownCount++
	m.tcpDownMu.Unlock()
	m.tcpDownOnce.Do(func() { close(m.tcpDownCh) })
}

func (m *mockRuntime) SelectLost() {}
func (m *mockRuntime) T7Expired()  {}

// DeliverOwnedFrame records the delivered inbound frame (a copy, since the caller transfers
// ownership) and signals any armed capture channel. The real runtime decodes+routes here; the mock
// only needs to prove the assembled frame reached the runtime seam.
func (m *mockRuntime) DeliverOwnedFrame(frame []byte) error {
	cp := append([]byte(nil), frame...)
	m.deliverMu.Lock()
	m.delivered = append(m.delivered, cp)
	ch := m.deliverCh
	m.deliverMu.Unlock()
	if ch != nil {
		ch <- cp
	}

	return nil
}

// captureFrames arms (idempotently) and returns the buffered channel DeliverOwnedFrame signals for
// each delivered inbound frame.
func (m *mockRuntime) captureFrames() chan []byte {
	m.deliverMu.Lock()
	defer m.deliverMu.Unlock()
	if m.deliverCh == nil {
		m.deliverCh = make(chan []byte, 8)
	}

	return m.deliverCh
}

func (m *mockRuntime) RouteReply(_ hsms.Message) bool      { return false }
func (m *mockRuntime) RouteData(_ *hsms.DataMessage) error { return nil }
func (m *mockRuntime) SendAsync(_ context.Context, _ hsms.Message) error {
	return nil
}
func (m *mockRuntime) State() hsms.ConnState           { return hsms.ConnState(m.state.Load()) }
func (m *mockRuntime) Done() <-chan struct{}           { return nil }
func (m *mockRuntime) Timers() hsms.TimerConfig        { return m.timers }
func (m *mockRuntime) SessionID() uint16               { return 0xFFFF }
func (m *mockRuntime) LinktestInterval() time.Duration { return 0 }
func (m *mockRuntime) LinktestFailThreshold() int      { return 3 }

// WriteMessage is never exercised on the T3 line-engine path (SECS-I has no core-driven control
// send), so it returns a sentinel rather than a bare (nil, nil).
func (m *mockRuntime) WriteMessage(_ context.Context, _ hsms.Message) (hsms.Message, error) {
	return nil, hsms.ErrNotOpen
}

func (m *mockRuntime) NextSystemBytes() [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], m.sysGen.Add(1))

	return b
}

// callOrder returns a copy of the recorded TCPUp/CommitSelected call sequence.
func (m *mockRuntime) callOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.order...)
}

// tcpDownFired reports whether TCPDown was ever called (C1 teeth-check observability).
func (m *mockRuntime) tcpDownFired() bool {
	m.tcpDownMu.Lock()
	defer m.tcpDownMu.Unlock()

	return m.tcpDownCount > 0
}

// --- Test harness ---

// transportTestConfig builds a Config for the given port with short T1/T2 so the timeout paths
// resolve fast under -race -count. Later options win, so a caller may override the defaults.
func transportTestConfig(t *testing.T, port int, opts ...Option) Config {
	t.Helper()
	base := []Option{WithT1(50 * time.Millisecond), WithT2(150 * time.Millisecond)}
	cfg, err := NewConfig("127.0.0.1", port, append(base, opts...)...)
	require.NoError(t, err)

	return cfg
}

// bringUpActive stands up an active transport against a loopback listener: it constructs the
// transport via newTransport (so configure may set tr.newSink before Start), spawns an accept
// goroutine, then ArmStart + Start (which dials). It returns the transport and the adopted PEER end
// (the scripted-peer socket). The generation ctx is cancellable and cleaned up via t.Cleanup, so a
// test drives teardown purely through tr.Stop (whose engineCancel fires the C1 guard) — mirroring
// the core's teardown discipline.
func bringUpActive(t *testing.T, rt *mockRuntime, configure func(tr *transport)) (*transport, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	cfg := transportTestConfig(t, tcpAddr.Port)

	tr := newTransport(cfg)
	if configure != nil {
		configure(tr)
	}

	peerCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			peerCh <- c
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr.ArmStart()
	require.NoError(t, tr.Start(ctx, rt))

	var peer net.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("active transport: peer did not connect")
	}
	t.Cleanup(func() { _ = peer.Close() })

	return tr, peer
}

// --- (a) synchronous auto-commit ---

// TestTransport_ActiveAutoCommitSelectedSynchronous proves the D5b-2/G-B auto-commit: after an
// active Start returns, the FSM is ALREADY Selected (CommitSelected ran synchronously before the
// engine spawned), and TCPUp was ordered strictly before CommitSelected.
//
// Teeth-check: move CommitSelected onto a goroutine in startActive and this test fails — State reads
// NotSelected (or the order slice lacks CommitSelected) the instant Start returns.
func TestTransport_ActiveAutoCommitSelectedSynchronous(t *testing.T) {
	rt := newMockRuntime()
	tr, _ := bringUpActive(t, rt, nil)

	require.Equal(t, hsms.SelectedState, rt.State(),
		"auto-commit must make State() Selected the instant an active Start returns")
	require.Equal(t, []string{"TCPUp", "CommitSelected"}, rt.callOrder(),
		"TCPUp must be called synchronously before CommitSelected, both before Start returns")

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// --- (b) inbound ENQ + block delivered ---

// TestTransport_InboundENQBlockDelivered scripts the peer to initiate an inbound block transfer
// (ENQ, then the golden block wire bytes after our EOT) and asserts the line engine (1) granted the
// line with EOT, (2) delivered the received block to the inbound sink, and (3) ACK'd on the wire.
func TestTransport_InboundENQBlockDelivered(t *testing.T) {
	rt := newMockRuntime()
	delivered := make(chan block, 1)
	tr, peer := bringUpActive(t, rt, func(tr *transport) {
		// The T5 sink is a per-generation FACTORY; return a capturing closure for this generation.
		tr.newSink = func() func(block) error {
			return func(b block) error {
				delivered <- b

				return nil
			}
		}
	})

	golden := makeTestBlock(t, []byte("hello"))
	goldenWire := golden.appendTo(nil)

	ackCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, []byte{enq}) // request to send
		eotByte := peerReadN(t, peer, 1)
		if len(eotByte) == 1 && eotByte[0] != eot {
			t.Errorf("engine must grant the line with EOT, got 0x%02X", eotByte[0])
		}
		peerWrite(t, peer, goldenWire) // the block
		ackCh <- peerReadN(t, peer, 1) // engine's ACK
	}()

	select {
	case got := <-delivered:
		require.Equal(t, goldenWire, got.appendTo(nil),
			"the engine must deliver the exact golden block to the inbound sink")
	case <-time.After(5 * time.Second):
		t.Fatal("engine never delivered the inbound block")
	}
	require.Equal(t, []byte{ack}, <-ackCh, "engine must ACK the valid inbound block on the wire")

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// --- (d) NEW-1 fresh per-generation bundle + channels ---

// TestTransport_ArmStartInstallsFreshBundleAndChannels proves ArmStart installs a FRESH genWG,
// genDone, and sendReqCh for the next generation (NEW-1 / G-C): a straggler abandoned by a bounded
// Stop keeps draining the OLD bundle while the next generation Adds/Waits on its own, and a Write
// racing a reconnect hands off on the fresh sendReqCh — never a channel a prior Stop already broadcast.
//
// Teeth-check: drop any of the three swaps from ArmStart and the corresponding identity assertion bites.
func TestTransport_ArmStartInstallsFreshBundleAndChannels(t *testing.T) {
	cfg := transportTestConfig(t, 5000)
	tr := newTransport(cfg)

	wg0, done0, sr0 := tr.wg, tr.genDone, tr.sendReqCh
	require.NotNil(t, wg0, "newTransport must install an initial genWG bundle")
	require.NotNil(t, done0, "newTransport must install an initial genDone channel")
	require.NotNil(t, sr0, "newTransport must install an initial sendReqCh channel")

	tr.ArmStart()
	require.NotSame(t, wg0, tr.wg, "ArmStart must install a FRESH genWG bundle")
	require.True(t, tr.genDone != done0, "ArmStart must install a FRESH genDone channel")
	require.True(t, tr.sendReqCh != sr0, "ArmStart must install a FRESH sendReqCh channel")

	wg1, done1, sr1 := tr.wg, tr.genDone, tr.sendReqCh

	tr.ArmStart()
	require.NotSame(t, wg1, tr.wg, "each ArmStart must install a fresh genWG — no cross-generation reuse")
	require.True(t, tr.genDone != done1, "each ArmStart must install a fresh genDone — no cross-generation reuse")
	require.True(t, tr.sendReqCh != sr1, "each ArmStart must install a fresh sendReqCh — no cross-generation reuse")
}

// --- Engine send path (runSend via sendReqCh) + the G-C cap-1 done invariant ---

// TestTransport_SendReqResultDelivered drives the engine's outbound path directly (Write is a T4
// stub): it hands a *sendReq off on sendReqCh and asserts the engine runs the block send (ENQ/EOT/
// block/ACK on the wire) and delivers the result on req.done. This exercises runSend end-to-end and
// pre-validates the hand-off T4's Write will use.
func TestTransport_SendReqResultDelivered(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)

	golden := makeTestBlock(t, []byte("send-me"))
	goldenWire := golden.appendTo(nil)

	go func() {
		first := peerReadN(t, peer, 1) // ENQ
		if len(first) == 1 && first[0] != enq {
			t.Errorf("engine send must open with ENQ, got 0x%02X", first[0])
		}
		peerWrite(t, peer, []byte{eot})     // grant
		peerReadN(t, peer, len(goldenWire)) // block
		peerWrite(t, peer, []byte{ack})     // ACK
	}()

	req := newSendReq([]block{golden})
	tr.sendReqCh <- req // hand off to the engine (unbuffered: blocks until the engine takes it)

	select {
	case err := <-req.done:
		require.NoError(t, err, "engine must deliver the send result via req.done after the block ACKs")
	case <-time.After(5 * time.Second):
		t.Fatal("engine never delivered the send result")
	}

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// TestTransport_SendResultBufferedForAbandonedWaiter is the P1 regression (Codex v1): it proves the
// cap-1 done channel (newSendReq / G-C) lets the engine's unconditional `req.done <- err` succeed even
// when the (future Write) waiter has abandoned the result — so the engine never wedges. It hands off a
// request but NEVER reads req.done; after the block ACKs, a clean Stop must still join the engine
// promptly.
//
// Teeth: make newSendReq's done unbuffered (make(chan error)) and this fails — the engine blocks
// forever on `req.done <- err` (no reader, no buffer) and Stop times out.
func TestTransport_SendResultBufferedForAbandonedWaiter(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)

	golden := makeTestBlock(t, nil)
	goldenWire := golden.appendTo(nil)

	acked := make(chan struct{})
	go func() {
		peerReadN(t, peer, 1)               // ENQ
		peerWrite(t, peer, []byte{eot})     // grant
		peerReadN(t, peer, len(goldenWire)) // block
		peerWrite(t, peer, []byte{ack})     // ACK
		close(acked)
	}()

	// Hand off and ABANDON the waiter: never read req.done. The cap-1 buffer must absorb the engine's
	// result send so it does not block.
	tr.sendReqCh <- newSendReq([]block{golden})
	<-acked // the block round-trip completed; the engine delivers to the (unread) cap-1 done and loops

	done := make(chan error, 1)
	go func() { done <- tr.Stop(context.Background()) }()
	select {
	case err := <-done:
		require.NoError(t, err, "cap-1 done: an abandoned waiter must not wedge the engine — Stop joins cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("engine wedged on an abandoned req.done — is the cap-1 buffer missing?")
	}
}

// --- (T5) default-sink integration: real assembler → runtime.DeliverOwnedFrame ---

// TestTransport_DefaultSinkAssemblesInboundFrame drives the DEFAULT newSink (the real per-generation
// multi-block assembler) end-to-end: a scripted peer sends a 2-block inbound message over the live
// line, and the mock runtime's DeliverOwnedFrame must receive exactly ONE assembled HSMS frame whose
// header + concatenated body match the two blocks. This proves the T5 wiring — engine → assembler →
// rt.DeliverOwnedFrame — with no test override of the sink.
func TestTransport_DefaultSinkAssemblesInboundFrame(t *testing.T) {
	rt := newMockRuntime()
	frames := rt.captureFrames() // arm capture before the peer sends
	tr, peer := bringUpActive(t, rt, nil)

	// Host role by default (isEquip=false) → the assembler accepts to-host (rBit=true) blocks.
	mh := asmHeader(true)
	b1 := asmBlock(mh, 1, false, []byte("multi-")).appendTo(nil)
	b2 := asmBlock(mh, 2, true, []byte("block")).appendTo(nil)

	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		// Block 1 transaction.
		peerWrite(t, peer, []byte{enq})
		peerReadN(t, peer, 1) // EOT
		peerWrite(t, peer, b1)
		peerReadN(t, peer, 1) // ACK
		// Block 2 transaction (E-bit → completes the message).
		peerWrite(t, peer, []byte{enq})
		peerReadN(t, peer, 1) // EOT
		peerWrite(t, peer, b2)
		peerReadN(t, peer, 1) // ACK
	}()

	var frame []byte
	select {
	case frame = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("the default assembler sink never delivered the inbound frame to the runtime")
	}

	// Join the peer goroutine before the deferred t.Cleanup closes its socket: the block-2 frame is
	// delivered right after receiveBlock writes the ACK, so the main goroutine can otherwise race ahead
	// to the test's Cleanup (closing peer) before the peer goroutine's final ACK read runs — surfacing
	// a spurious "use of closed network connection" on that read.
	<-peerDone

	msg, err := hsms.DecodeHSMSMessage(withHSMSLengthPrefix(frame))
	require.NoError(t, err)
	data, ok := msg.(*hsms.DataMessage)
	require.True(t, ok, "the assembled frame must decode to a DataMessage")
	require.Equal(t, uint8(6), data.Stream())
	require.Equal(t, uint8(0x11), data.Function())
	require.True(t, data.WaitBit())
	require.Equal(t, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, data.SystemBytes())
	require.Equal(t, []byte("multi-block"), data.AppendBodyTo(nil),
		"the delivered frame body must be the ordered concatenation of the two blocks")

	// Exactly one frame — no spurious duplicate delivery.
	select {
	case extra := <-frames:
		t.Fatalf("the assembler delivered a second, unexpected frame: %q", extra)
	default:
	}

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// freePort returns a currently-free loopback TCP port (a throwaway listener is opened then closed).
// net.ListenTCP sets SO_REUSEADDR so a passive transport can immediately re-bind it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NoError(t, l.Close())

	return tcpAddr.Port
}

// dialPeer dials the passive transport's listener and returns the peer end.
func dialPeer(t *testing.T, port int) net.Conn {
	t.Helper()
	peer, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	return peer
}
