package hsmsss

// transport_test.go — loopback TCP tests for the hsmsss transport concrete type.
//
// The file lives in package hsmsss (not hsmsss_test) because transport is unexported;
// only in-package tests can construct *transport values directly.
//
// All tests use real loopback TCP (127.0.0.1:0) and are run with -race.

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// ── mock runtime ─────────────────────────────────────────────────────────────
//
// mockRT is a minimal test double implementing hsms.TransportRuntime. TCPUp and TCPDown
// are the key methods for transport tests; every other method is an inert no-op stub.

type mockRT struct {
	mu sync.Mutex

	tcpUpConn net.Conn
	tcpUpOnce sync.Once
	tcpUpCh   chan struct{} // closed on the first TCPUp call

	tcpDownErr  error
	tcpDownOnce sync.Once
	tcpDownCh   chan struct{} // closed on the first TCPDown call

	// tcpDownFn, when non-nil, is called by TCPDown AFTER the tcpDownCh signal.
	// Tests use this to inject blocking behaviour for the recv-loop-join teeth-check.
	tcpDownFn func(err error)

	sysGen testSysGen    // System Bytes for NextSystemBytes (uniqueness not asserted by mock tests)
	doneCh chan struct{} // never closed; keeps Done() from nil-blocking
}

func newMockRT() *mockRT {
	return &mockRT{
		tcpUpCh:   make(chan struct{}),
		tcpDownCh: make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

func (m *mockRT) TCPUp(conn net.Conn) {
	m.mu.Lock()
	m.tcpUpConn = conn
	m.mu.Unlock()

	m.tcpUpOnce.Do(func() { close(m.tcpUpCh) })
}

func (m *mockRT) TCPDown(err error) {
	m.mu.Lock()
	m.tcpDownErr = err
	fn := m.tcpDownFn
	m.mu.Unlock()

	m.tcpDownOnce.Do(func() { close(m.tcpDownCh) })

	// Call the injected hook AFTER signalling tcpDownCh, so tests that gate on tcpDownCh
	// can be sure TCPDown is in flight (and blocking) when they proceed.
	if fn != nil {
		fn(err)
	}
}

func (m *mockRT) CommitSelected() bool {
	return false
}

func (m *mockRT) SelectLost() {}

func (m *mockRT) T7Expired() {}

func (m *mockRT) DeliverOwnedFrame(_ []byte) error {
	return nil
}

func (m *mockRT) RouteReply(_ hsms.Message) bool {
	return false
}

func (m *mockRT) RouteData(_ *hsms.DataMessage) error {
	return nil
}

func (m *mockRT) State() hsms.ConnState {
	return hsms.NotConnectedState
}

func (m *mockRT) Done() <-chan struct{} {
	return m.doneCh
}

func (m *mockRT) Timers() hsms.TimerConfig {
	return hsms.TimerConfig{}
}

func (m *mockRT) SessionID() uint16 {
	return 0xFFFF
}

func (m *mockRT) NextSystemBytes() [4]byte { return m.sysGen.next() }

func (m *mockRT) LinktestInterval() time.Duration { return 0 }
func (m *mockRT) LinktestFailThreshold() int      { return 3 }

// WriteMessage blocks until the caller's ctx is cancelled and returns ctx.Err(). mockRT.State
// is always NotConnected, so the active Select procedure never actually reaches this call — but
// blocking (rather than returning an error) keeps a hypothetical Select send from being
// misread as a Select failure, and lets Stop's ctx cancel unwind the goroutine cleanly.
func (m *mockRT) WriteMessage(ctx context.Context, _ hsms.Message) (hsms.Message, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *mockRT) SendAsync(_ context.Context, _ hsms.Message) error {
	return nil
}

// WriteMessageNoReply returns nil; like WriteMessage it is never reached on the mockRT path
// (State is always NotConnected), and the forward/no-reply path never blocks on a reply.
func (m *mockRT) WriteMessageNoReply(_ context.Context, _ hsms.Message) error {
	return nil
}

// waitTCPUp waits up to 5 s for TCPUp to fire, returning the conn it was called with.
func (m *mockRT) waitTCPUp(t *testing.T) net.Conn {
	t.Helper()

	select {
	case <-m.tcpUpCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TCPUp")
	}

	m.mu.Lock()
	conn := m.tcpUpConn
	m.mu.Unlock()

	return conn
}

// waitTCPDown waits up to 5 s for TCPDown to fire, returning the error it was called with.
func (m *mockRT) waitTCPDown(t *testing.T) error {
	t.Helper()

	select {
	case <-m.tcpDownCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TCPDown")
	}

	m.mu.Lock()
	err := m.tcpDownErr
	m.mu.Unlock()

	return err
}

// ── loopback helpers ──────────────────────────────────────────────────────────

// listenLoopback starts a TCP listener on 127.0.0.1:0 and returns it with the bound port.
// The caller is responsible for ln.Close().
func listenLoopback(t *testing.T) (*net.TCPListener, int) {
	t.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener addr must be *net.TCPAddr")

	return ln, addr.Port
}

// acceptOneAsync accepts one connection on ln in a background goroutine. The accepted
// *net.TCPConn is sent on the returned channel. If Accept errors (e.g. the listener is
// closed before a connection arrives), nothing is sent.
func acceptOneAsync(ln *net.TCPListener) chan *net.TCPConn {
	ch := make(chan *net.TCPConn, 1)

	go func() {
		conn, err := ln.AcceptTCP()
		if err != nil {
			return
		}

		ch <- conn
	}()

	return ch
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestTransport_ActiveDialFiresTCPUp verifies that an active transport dials the peer,
// establishes a TCP connection, and calls rt.TCPUp with a non-nil net.Conn.
func TestTransport_ActiveDialFiresTCPUp(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer ln.Close()

	peerConnCh := acceptOneAsync(ln)

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)

	rt := newMockRT()
	tr := newTransport(cfg)

	require.NoError(t, tr.Start(context.Background(), rt))
	defer tr.Stop(context.Background()) //nolint:errcheck

	// TCPUp must fire with a non-nil conn.
	conn := rt.waitTCPUp(t)
	require.NotNil(t, conn, "TCPUp must be called with a non-nil conn")

	// The peer goroutine must have accepted the connection.
	select {
	case peerConn := <-peerConnCh:
		peerConn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept the connection in time")
	}
}

// TestTransport_WriteSendsBytes verifies that Write sends the net.Buffers payload to the
// peer using the writev fast path (*net.TCPConn) and that the peer receives exactly those
// bytes in order.
func TestTransport_WriteSendsBytes(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer ln.Close()

	peerConnCh := acceptOneAsync(ln)

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)

	rt := newMockRT()
	tr := newTransport(cfg)

	require.NoError(t, tr.Start(context.Background(), rt))
	defer tr.Stop(context.Background()) //nolint:errcheck

	rt.waitTCPUp(t)

	// Obtain the peer connection before writing.
	var peerConn *net.TCPConn

	select {
	case peerConn = <-peerConnCh:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept in time")
	}

	defer peerConn.Close()

	// Write two buffers — the writev path should deliver them as one contiguous stream. The core
	// hands Write the epoch's captured conn (I1 full-fix); here the transport's own dialed conn is
	// that socket.
	want := []byte("hello world")
	bufs := net.Buffers{[]byte("hello"), []byte(" world")}

	require.NoError(t, tr.Write(context.Background(), tr.conn, bufs))

	// Peer must receive exactly the same bytes.
	got := make([]byte, len(want))
	_, err = io.ReadFull(peerConn, got)
	require.NoError(t, err)
	require.Equal(t, want, got, "peer received unexpected bytes")
}

// TestTransport_StopJoinsRecvLoop is the round-7 key test. It verifies that Stop closes the
// connection (unblocking the recv loop's parked Read) and JOINS the recv loop via g.recv.Wait
// before returning. Without g.recv.Wait, Stop returns before the recv loop's rt.TCPDown call
// has completed, violating the epoch generation-serialization contract.
//
// Mechanism: a gated TCPDown implementation signals when TCPDown is in flight, then blocks
// until the test releases it. With g.recv.Wait in Stop, Stop is also blocked at that point
// (waiting for g.recv.Done, which fires only after TCPDown returns). Without g.recv.Wait,
// Stop has already returned.
//
// Teeth-check: remove g.recv.Wait() from Stop → the "Stop returned before recv loop exited"
// assertion fires → the test correctly fails.
func TestTransport_StopJoinsRecvLoop(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer ln.Close()

	peerConnCh := acceptOneAsync(ln)

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)

	rt := newMockRT()

	// Gate: TCPDown signals tcpDownInFlight (so the test knows TCPDown is executing), then
	// blocks on tcpDownGate until the test releases it.
	tcpDownInFlight := make(chan struct{})
	tcpDownGate := make(chan struct{})
	var signalOnce sync.Once

	rt.tcpDownFn = func(_ error) {
		// Signal that TCPDown has been called and is now blocking below.
		signalOnce.Do(func() { close(tcpDownInFlight) })
		// Block until the test releases the gate.
		<-tcpDownGate
	}

	tr := newTransport(cfg)

	require.NoError(t, tr.Start(context.Background(), rt))

	// Keep the peer connection open so the recv loop stays parked in conn.Read.
	var peerConn *net.TCPConn

	select {
	case peerConn = <-peerConnCh:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept in time")
	}

	defer peerConn.Close()

	rt.waitTCPUp(t)

	// Run Stop in a goroutine. Stop will:
	//   1. Close the transport's conn → the recv loop's parked Read returns an error.
	//   2. The recv loop calls rt.TCPDown(err) → signals tcpDownInFlight, then blocks on gate.
	//   3. Stop calls g.recv.Wait() → blocks until g.recv.Done fires (after TCPDown returns).
	stopDone := make(chan struct{})

	go func() {
		defer close(stopDone)
		_ = tr.Stop(context.Background())
	}()

	// Wait until TCPDown is in flight (recv loop has received the error and called TCPDown).
	select {
	case <-tcpDownInFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("TCPDown did not fire; recv loop may not have received the connection error")
	}

	// Stop MUST NOT have returned yet.
	// WITH g.recv.Wait: Stop is blocked in g.recv.Wait() because the recv loop is blocked
	//   inside TCPDown (hasn't called g.recv.Done yet).
	// WITHOUT g.recv.Wait (teeth-check): Stop already returned → stopDone is closed → FAIL.
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the recv loop fully exited — g.recv.Wait() is missing")
	default:
		// Correct: Stop is still blocked, waiting for the recv loop to exit.
	}

	// Release the gate. Sequence that follows:
	//   TCPDown unblocks → returns → recvLoop returns → defer g.recv.Done() fires →
	//   g.recv.Wait() in Stop unblocks → Stop returns → stopDone closes.
	close(tcpDownGate)

	select {
	case <-stopDone:
		// Pass: Stop returned only after the recv loop fully exited.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the recv loop exited")
	}
}

// TestTransport_ReadErrorFiresTCPDown verifies that when the peer closes its side of the
// connection, the recv loop's Read returns an error and rt.TCPDown is called with that error.
func TestTransport_ReadErrorFiresTCPDown(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer ln.Close()

	peerConnCh := acceptOneAsync(ln)

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)

	rt := newMockRT()
	tr := newTransport(cfg)

	require.NoError(t, tr.Start(context.Background(), rt))
	defer tr.Stop(context.Background()) //nolint:errcheck

	rt.waitTCPUp(t)

	// Close the peer connection — this sends EOF to the transport's recv loop.
	select {
	case peerConn := <-peerConnCh:
		peerConn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept in time")
	}

	// The recv loop must detect the read error and call TCPDown with a non-nil error.
	downErr := rt.waitTCPDown(t)
	require.Error(t, downErr, "TCPDown must receive a non-nil error when the peer closes")
}
