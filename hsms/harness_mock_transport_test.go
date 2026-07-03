package hsms

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// mockTransport is a minimal in-package implementation of the unexported transport seam,
// used to construct a connection in unit tests without real TCP I/O. It is the first
// consumer of the seam (the seed for the Task 26 mock transport): Start records the
// TransportRuntime it is handed and can invoke an optional scripted startFn to drive rt
// callbacks; every other method is an inert stub sufficient for the skeleton tests.
type mockTransport struct {
	mu sync.Mutex

	// rt is the back-channel captured by Start (nil until Start has run).
	rt TransportRuntime

	// startFn, when set, is invoked by Start after rt is recorded. Later tasks (and Task
	// 26) use it to drive scripted rt.TCPUp / rt.CommitSelected / rt.TCPDown callbacks.
	startFn func(ctx context.Context, rt TransportRuntime) error

	started    bool
	stopped    bool
	startCount int // number of Start calls (gen 1, gen 2, ... across reconnects)

	// holdableRecv, when true, makes the FIRST Start arm a "held recv loop" for the round-7
	// stale-recv-loop reconnect test: a goroutine parked on holdRelease that fires exactly one
	// (stale) rt.TCPDown when released. Stop releases + JOINS it — mirroring teardown's tr.Stop
	// join of the real recv loop — so the reconnect loop's generation-serialization guarantees
	// that stale TCPDown fires (and is drained) while cur is still gen N, never gen N+1.
	holdableRecv bool
	holdRelease  chan struct{}
	holdExited   chan struct{}
	holdOnce     sync.Once

	// writeErr, when set, is returned by every Write (to exercise the write-error send path).
	writeErr error
	// lastWriteDeadline is the most recent SetWriteDeadline arg; capturedWriteDeadline is the value
	// armed at the instant of a Write (captured before writeFrame's defer clears it) — the
	// write-timeout arming (I5).
	lastWriteDeadline     time.Time
	capturedWriteDeadline time.Time
	// frames captures each Write's buffers flattened into one on-wire frame, in call order.
	// The send path (Task 12) issues exactly one Write per frame, so frames[i] is the i-th
	// message's complete on-wire bytes — comparable to Message.ToBytes().
	frames [][]byte
	// writeConns records the conn arg of EVERY Write call, in order (even a failed one) — the
	// I1 full-fix teeth: it proves a stale sender's write targeted the epoch it was pinned to
	// (the captured conn), never a successor generation's socket.
	writeConns []net.Conn
	// lastDeadlineConn is the conn arg of the most recent SetWriteDeadline call; capturedDeadlineConn
	// is that value snapshotted at the instant of a Write (before writeFrame's defer re-arms/clears
	// it) — the I1 teeth: the deadline must be armed on the same epoch socket the write targets.
	lastDeadlineConn     net.Conn
	capturedDeadlineConn net.Conn
}

// Start records rt and, if a scripted startFn was installed, runs it. When holdableRecv is set,
// the FIRST Start also arms a held recv loop (see the field docs).
func (m *mockTransport) Start(ctx context.Context, rt TransportRuntime) error {
	m.mu.Lock()
	m.rt = rt
	m.started = true
	m.startCount++
	fn := m.startFn
	armHold := m.holdableRecv && m.startCount == 1
	if armHold {
		m.holdRelease = make(chan struct{})
		m.holdExited = make(chan struct{})
	}
	release, exited := m.holdRelease, m.holdExited
	m.mu.Unlock()

	if armHold {
		go heldRecvLoop(rt, release, exited)
	}

	if fn != nil {
		return fn(ctx, rt)
	}

	return nil
}

// heldRecvLoop models a gen-N transport recv loop whose disconnect callback lands late: it parks
// until released, then fires exactly one (stale) rt.TCPDown and exits. releaseHeldTCPDown and the
// teardown join (Stop) both release it; whichever wins, it fires at most once.
func heldRecvLoop(rt TransportRuntime, release, exited chan struct{}) {
	<-release
	rt.TCPDown(io.EOF)
	close(exited)
}

// Stop marks the transport stopped and, if a held recv loop is armed, releases + JOINS it (the
// mock's stand-in for teardown's tr.Stop join of the real recv loop). The join is bounded by ctx.
func (m *mockTransport) Stop(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = true
	exited := m.holdExited
	m.mu.Unlock()

	if exited != nil {
		m.releaseHeld()
		select {
		case <-exited:
		case <-ctx.Done():
		}
	}

	return nil
}

// ArmStart is the mock's no-op implementation of the transport seam's I1 Stop-seal reset. The
// mock does not model the shared-WaitGroup Add-vs-Wait guard (it has no real per-generation
// recv/proc goroutines on reused WaitGroups), so there is no seal to clear.
func (m *mockTransport) ArmStart() {}

// releaseHeld closes holdRelease exactly once (idempotent), firing the held stale TCPDown.
func (m *mockTransport) releaseHeld() {
	m.mu.Lock()
	release := m.holdRelease
	m.mu.Unlock()

	if release != nil {
		m.holdOnce.Do(func() { close(release) })
	}
}

// dropGenerationButHoldTCPDown drives the involuntary drop that triggers gen-N teardown +
// reconnect (an immediate TCPDown reading cur=gen N). The gen-N held recv loop (armed in Start)
// stays parked, holding its stale duplicate TCPDown until released (by the teardown join or by
// releaseHeldTCPDown).
func (m *mockTransport) dropGenerationButHoldTCPDown() {
	if rt := m.runtime(); rt != nil {
		rt.TCPDown(io.EOF)
	}
}

// releaseHeldTCPDown releases the parked stale gen-N TCPDown (idempotent — a no-op if the
// teardown join already released it).
func (m *mockTransport) releaseHeldTCPDown() {
	m.releaseHeld()
}

// startCalls returns how many times Start has been invoked (generation count).
func (m *mockTransport) startCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.startCount
}

// Write flattens the writev buffers into one captured frame (in call order) and returns the
// configured writeErr (nil by default). Flattening mirrors what the peer would read off the
// wire, so a captured frame is byte-comparable to Message.ToBytes().
func (m *mockTransport) Write(ctx context.Context, conn net.Conn, bufs net.Buffers) error {
	var frame []byte
	for _, b := range bufs {
		frame = append(frame, b...)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.capturedWriteDeadline = m.lastWriteDeadline // the deadline writeFrame armed for THIS write (I5)
	m.capturedDeadlineConn = m.lastDeadlineConn   // the conn that deadline was armed on (I1)
	m.writeConns = append(m.writeConns, conn)     // I1: the epoch conn this write targeted (even on error)

	if m.writeErr != nil {
		return m.writeErr
	}

	m.frames = append(m.frames, frame)

	return nil
}

// writtenFrames returns a copy of the captured on-wire frames in call order.
func (m *mockTransport) writtenFrames() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([][]byte, len(m.frames))
	copy(out, m.frames)

	return out
}

// writeCount returns how many frames have been written.
func (m *mockTransport) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.frames)
}

// writeConnsUsed returns a copy of the conn each Write targeted, in call order (I1 full-fix teeth):
// it lets a test assert a stale sender's write hit the epoch it was pinned to, never a successor.
func (m *mockTransport) writeConnsUsed() []net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]net.Conn, len(m.writeConns))
	copy(out, m.writeConns)

	return out
}

// deadlineConnUsed returns the conn the write deadline was armed on AT the instant of the last
// Write (I1 teeth): it lets a test assert the bounded-write deadline was armed on the SAME epoch
// socket the write targets. Snapshotted at Write time so writeFrame's defer-clear (on the captured
// conn) cannot mask an arm on the wrong conn.
func (m *mockTransport) deadlineConnUsed() net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.capturedDeadlineConn
}

// controlWrites counts captured frames whose SType (header byte 5, at frame offset 9) equals
// sType — used to assert that the local give-up path never emits a Reject/Separate to the peer.
func (m *mockTransport) controlWrites(sType MsgType) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	for _, f := range m.frames {
		if len(f) >= 10 && MsgType(f[9]) == sType {
			n++
		}
	}

	return n
}

// separateWrites counts the farewell/normal Separate.req frames the core wrote (SType 9). The
// reaction tests assert exactly one on a graceful Close-from-Selected and zero on a
// comms-failure teardown (§9.1.1).
func (m *mockTransport) separateWrites() int {
	return m.controlWrites(SeparateReqType)
}

// simulateReadError drives an involuntary drop: it calls rt.TCPDown(err) exactly as a real
// recv loop would on a read error, so the core marks the generation comms-failure and injects
// evDisconnect. No-op if Start has not captured a runtime yet.
func (m *mockTransport) simulateReadError(err error) {
	if rt := m.runtime(); rt != nil {
		rt.TCPDown(err)
	}
}

// setWriteErr arms (or clears) the error every Write returns, race-safely — for tests that toggle
// the write fault while the connection's goroutines are running (Write reads writeErr under m.mu).
func (m *mockTransport) setWriteErr(err error) {
	m.mu.Lock()
	m.writeErr = err
	m.mu.Unlock()
}

// SetReadDeadline is an inert stub (conn-bound for seam symmetry, I1).
func (m *mockTransport) SetReadDeadline(_ net.Conn, t time.Time) error { return nil }

// SetWriteDeadline records the deadline (write-timeout arming, I5) and the conn it was armed on
// (I1: must be the same epoch socket the write targets); otherwise inert.
func (m *mockTransport) SetWriteDeadline(conn net.Conn, t time.Time) error {
	m.mu.Lock()
	m.lastWriteDeadline = t
	m.lastDeadlineConn = conn
	m.mu.Unlock()

	return nil
}

// writeDeadlineAtLastWrite returns the write deadline armed at the instant of the last Write.
func (m *mockTransport) writeDeadlineAtLastWrite() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.capturedWriteDeadline
}

// fakeConn is a minimal net.Conn used as the epoch socket in lifecycle tests. Only Close()
// matters (closeSocket closes it, then the epoch niles it); the byte methods are inert because
// the mock records writes through tr.Write, not through the conn.
type fakeConn struct{}

func (fakeConn) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (fakeConn) Write(b []byte) (int, error)        { return len(b), nil }
func (fakeConn) Close() error                       { return nil }
func (fakeConn) LocalAddr() net.Addr                { return nil }
func (fakeConn) RemoteAddr() net.Addr               { return nil }
func (fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

// idConn is a distinct-identity net.Conn for the I1 binding teeth (TestSend_I1WriteBindsToPinned‐
// EpochConn): fakeConn is a zero-size struct, so &fakeConn{} pointers may share the runtime.zerobase
// address — idConn's non-empty id field gives each generation's socket its own address so
// require.Same/NotSame can tell them apart. The byte methods are inherited (inert) from fakeConn.
type idConn struct {
	fakeConn
	id int
}

// driveSelect simulates the responder committing Selected: it retries rt.CommitSelected until
// the CAS succeeds (which only happens once the supervisor has processed evTCPUp and reached
// NotSelected) or the generation ctx is cancelled. It is a bounded ticker retry, never a
// time.Sleep-poll.
func driveSelect(ctx context.Context, rt TransportRuntime) {
	ticker := time.NewTicker(500 * time.Microsecond)
	defer ticker.Stop()

	for {
		if rt.CommitSelected() || rt.State() == SelectedState {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// mockScript is the shape of mockTransport.startFn — a scripted transport lifecycle driver.
type mockScript func(ctx context.Context, rt TransportRuntime) error

// withMockTransport drives a full connect+select: TCPUp then commit to Selected. Start runs
// the driver on its own goroutine (a real transport spawns the recv loop and returns), so
// OpenBackground returns promptly while selection completes asynchronously.
func withMockTransport() mockScript {
	return func(ctx context.Context, rt TransportRuntime) error {
		go func() {
			rt.TCPUp(fakeConn{})
			driveSelect(ctx, rt)
		}()

		return nil
	}
}

// withMockTransportNeverSelects drives TCPUp but NEVER commits, so the FSM parks at
// NotSelected (used to exercise OpenWaitSelected honoring ctx and Close-while-NotSelected).
func withMockTransportNeverSelects() mockScript {
	return func(ctx context.Context, rt TransportRuntime) error {
		go func() { rt.TCPUp(fakeConn{}) }()

		return nil
	}
}

// withMockTransportThatConnectsButNeverSelects is an alias for withMockTransportNeverSelects
// (TCP up, no selection) — the name the Close-while-NotSelected test reads.
func withMockTransportThatConnectsButNeverSelects() mockScript {
	return withMockTransportNeverSelects()
}

// withMockTransportThatNeverConnects never calls TCPUp, so the FSM stays NotConnected (dial in
// progress) — used to prove Close during dialing does not hang.
func withMockTransportThatNeverConnects() mockScript {
	return func(_ context.Context, _ TransportRuntime) error {
		return nil
	}
}

// withMockTransportTCPUpThenStartError drives TCPUp SYNCHRONOUSLY (so evTCPUp is enqueued and the
// FSM advances toward NotSelected) and THEN returns startErr — reproducing a tr.Start that fails
// after already driving evTCPUp. Used by the failed-Open rollback-fence test (reconciliation #2).
func withMockTransportTCPUpThenStartError(startErr error) mockScript {
	return func(_ context.Context, rt TransportRuntime) error {
		rt.TCPUp(fakeConn{})

		return startErr
	}
}

// runtime returns the TransportRuntime captured by Start (nil until Start ran).
func (m *mockTransport) runtime() TransportRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.rt
}

// wasStarted reports whether Start has been invoked.
func (m *mockTransport) wasStarted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.started
}

// wasStopped reports whether Stop has been invoked.
func (m *mockTransport) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.stopped
}

// Compile-time assertion that *mockTransport satisfies the unexported transport seam.
var _ transport = (*mockTransport)(nil)
