package hsmsss

// reader_test.go — framed-reader tests for the hsmsss transport (spec §6.1).
//
// The file lives in package hsmsss (not hsmsss_test) because transport is unexported;
// only in-package tests can construct *transport values and set the allocFrame seam.
//
// All tests use real loopback TCP (127.0.0.1:0) so the reader exercises the real
// *net.TCPConn read/deadline/writev paths, and run with -race. A recording
// TransportRuntime (recRT) captures DeliverOwnedFrame / RouteReply / TCPDown so the
// adversarial-input assertions (J1/J2/J3, control-header, peer Separate) have teeth.

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ── recording runtime ──────────────────────────────────────────────────────────
//
// recRT is a richer TransportRuntime double than transport_test.go's mockRT: it counts
// and captures DeliverOwnedFrame / RouteReply calls and records the TCPDown cause, and
// exposes a settable State so the peer-Separate branch (Selected vs NotSelected) can be
// driven. Every accessor is goroutine-safe (the recv loop calls these from its own
// goroutine while the test asserts from the test goroutine).

// testSysGen is a minimal concurrency-safe System Bytes generator for the hsmsss test doubles
// (hsms.sysBytesGen is unexported, so the mocks cannot borrow it). Uniqueness is not asserted by
// the transport-level tests; the generator exists only to satisfy the TransportRuntime contract.
type testSysGen struct{ n atomic.Uint32 }

func (g *testSysGen) next() [4]byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], g.n.Add(1))

	return b
}

type recRT struct {
	mu sync.Mutex

	delivered    [][]byte       // copies of every frame handed to DeliverOwnedFrame
	routed       []hsms.Message // every msg handed to RouteReply
	sent         []hsms.Message // every msg handed to SendAsync (async fire-and-forget, e.g. Reject.req)
	written      []hsms.Message // every msg handed to WriteMessage (synchronous, e.g. auto Linktest.req)
	tcpDownErr   error
	tcpDownFired bool
	tcpDownCh    chan struct{}
	tcpDownOnce  sync.Once

	// linktest test hooks (guarded by mu). Set before startLinktest; read by the auto-linktest
	// goroutine via the LinktestInterval/LinktestFailThreshold/Timers methods.
	linktestInterval      time.Duration
	linktestFailThreshold int
	timers                hsms.TimerConfig
	// writeMsgFn, when non-nil, supplies WriteMessage's return value (used to simulate a T6
	// timeout or a linktest success). When nil, WriteMessage falls back to blocking on ctx
	// (the active Select procedure parks there — unchanged from the framed-reader tests).
	writeMsgFn      func(ctx context.Context, msg hsms.Message) (hsms.Message, error)
	selectLostCount int // number of SelectLost() calls (Deselect responder transition)

	t7ExpiredCount int           // number of T7Expired() calls (NOT-SELECTED dwell expiry)
	t7ExpiredCh    chan struct{} // closed on the FIRST T7Expired call
	t7ExpiredOnce  sync.Once

	sysGen  testSysGen    // System Bytes for NextSystemBytes
	state   atomic.Uint32 // hsms.ConnState
	tcpUpCh chan struct{}
	tcpUp   sync.Once

	// deliverGate, when non-nil, blocks DeliverOwnedFrame until the gate is closed — the NEW-1
	// abandoned-straggler test uses it to WEDGE the recv goroutine inside frame delivery (mirroring
	// a blocking inline data handler) so a bounded Stop times out and abandons the straggler. nil
	// (the default) preserves the normal non-blocking delivery for every other test.
	deliverGate       chan struct{}
	deliveredCh       chan struct{} // when non-nil, closed once on the FIRST DeliverOwnedFrame (delivery observed)
	deliveredJustOnce sync.Once
}

func newRecRT() *recRT {
	rt := &recRT{
		tcpDownCh:             make(chan struct{}),
		tcpUpCh:               make(chan struct{}),
		t7ExpiredCh:           make(chan struct{}),
		linktestFailThreshold: 3, // sensible default (>= 1); overridden per test via setLinktest
		// Default to the shared timer set so the recv framing loop reads a sane LIVE T8 via
		// rt.Timers() (readFrame sources T8 from rt, not the frozen cfg — see transport.readFrame).
		// A zero T8 would make every inter-byte read deadline "now" (immediate timeout). Tests that
		// exercise a specific T8 override this via setTimers.
		timers: hsms.DefaultConnectionConfig().Timers(),
	}
	rt.state.Store(uint32(hsms.SelectedState))

	return rt
}

func (m *recRT) setState(s hsms.ConnState) { m.state.Store(uint32(s)) }

func (m *recRT) TCPUp(_ net.Conn) { m.tcpUp.Do(func() { close(m.tcpUpCh) }) }

func (m *recRT) TCPDown(cause error) {
	m.mu.Lock()
	m.tcpDownErr = cause
	m.tcpDownFired = true
	m.mu.Unlock()
	m.tcpDownOnce.Do(func() { close(m.tcpDownCh) })
}

func (m *recRT) CommitSelected() bool { return false }

// SelectLost records the call and transitions the mock to NotSelected (mirroring the real
// supervisor's evSelectLost) so the Deselect-responder tests can assert the transition fired.
func (m *recRT) SelectLost() {
	m.mu.Lock()
	m.selectLostCount++
	m.mu.Unlock()
	m.setState(hsms.NotSelectedState)
}

// T7Expired records the NOT-SELECTED dwell expiry: it counts the call and closes t7ExpiredCh on
// the first invocation so a test can both wait for the fire and assert exact call counts.
func (m *recRT) T7Expired() {
	m.mu.Lock()
	m.t7ExpiredCount++
	m.mu.Unlock()
	m.t7ExpiredOnce.Do(func() { close(m.t7ExpiredCh) })
}

func (m *recRT) t7ExpiredCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.t7ExpiredCount
}

func (m *recRT) DeliverOwnedFrame(frame []byte) error {
	cp := append([]byte(nil), frame...)
	m.mu.Lock()
	m.delivered = append(m.delivered, cp)
	gate := m.deliverGate
	doneCh := m.deliveredCh
	m.mu.Unlock()

	if doneCh != nil {
		m.deliveredJustOnce.Do(func() { close(doneCh) })
	}
	if gate != nil {
		<-gate // wedge the recv goroutine here (mirrors a blocking inline data handler) — NEW-1
	}

	return nil
}

func (m *recRT) RouteReply(msg hsms.Message) bool {
	m.mu.Lock()
	m.routed = append(m.routed, msg)
	m.mu.Unlock()

	return false
}

func (m *recRT) RouteData(_ *hsms.DataMessage) error { return nil }

func (m *recRT) State() hsms.ConnState { return hsms.ConnState(m.state.Load()) }

func (m *recRT) Done() <-chan struct{} { return nil }

func (m *recRT) Timers() hsms.TimerConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.timers
}

func (m *recRT) SessionID() uint16 { return 0xFFFF }

func (m *recRT) NextSystemBytes() [4]byte { return m.sysGen.next() }

func (m *recRT) LinktestInterval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.linktestInterval
}

func (m *recRT) LinktestFailThreshold() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.linktestFailThreshold
}

// setLinktest configures the auto-linktest interval/threshold before startLinktest is called.
func (m *recRT) setLinktest(interval time.Duration, threshold int) {
	m.mu.Lock()
	m.linktestInterval = interval
	m.linktestFailThreshold = threshold
	m.mu.Unlock()
}

// setTimers configures the timer set returned by Timers() (linktest reads T6 for its bound).
func (m *recRT) setTimers(tc hsms.TimerConfig) {
	m.mu.Lock()
	m.timers = tc
	m.mu.Unlock()
}

// setWriteMsgFn installs the WriteMessage hook (linktest success / T6-timeout simulation).
func (m *recRT) setWriteMsgFn(fn func(ctx context.Context, msg hsms.Message) (hsms.Message, error)) {
	m.mu.Lock()
	m.writeMsgFn = fn
	m.mu.Unlock()
}

// WriteMessage records the call, then either delegates to writeMsgFn (when set) or blocks until
// the caller's ctx is cancelled and returns ctx.Err(). The block-on-ctx default is what the
// active Select procedure spawned by startActive parks on (rather than treating an immediate
// error as a Select failure and firing a spurious TCPDown); Stop's ctx cancel then unwinds it.
func (m *recRT) WriteMessage(ctx context.Context, msg hsms.Message) (hsms.Message, error) {
	m.mu.Lock()
	m.written = append(m.written, msg)
	fn := m.writeMsgFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, msg)
	}

	<-ctx.Done()

	return nil, ctx.Err()
}

// WriteMessageNoReply records the call synchronously and returns nil (the forward/no-reply path
// never blocks on a reply). It is unexercised on the current recv-path tests.
func (m *recRT) WriteMessageNoReply(_ context.Context, msg hsms.Message) error {
	m.mu.Lock()
	m.written = append(m.written, msg)
	m.mu.Unlock()

	return nil
}

func (m *recRT) SendAsync(_ context.Context, msg hsms.Message) error {
	m.mu.Lock()
	m.sent = append(m.sent, msg)
	m.mu.Unlock()

	return nil
}

// ── accessors (goroutine-safe) ──────────────────────────────────────────────────

func (m *recRT) deliveredCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.delivered)
}

func (m *recRT) lastDelivered() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.delivered) == 0 {
		return nil
	}

	return m.delivered[len(m.delivered)-1]
}

func (m *recRT) routedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.routed)
}

func (m *recRT) lastRouted() hsms.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.routed) == 0 {
		return nil
	}

	return m.routed[len(m.routed)-1]
}

func (m *recRT) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.sent)
}

func (m *recRT) lastSent() hsms.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sent) == 0 {
		return nil
	}

	return m.sent[len(m.sent)-1]
}

func (m *recRT) writtenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.written)
}

func (m *recRT) lastWritten() hsms.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.written) == 0 {
		return nil
	}

	return m.written[len(m.written)-1]
}

func (m *recRT) selectLostCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.selectLostCount
}

func (m *recRT) tcpDownCause() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.tcpDownFired {
		return nil
	}

	if m.tcpDownErr == nil {
		// distinguish "fired with nil cause" from "not fired"
		return errors.New("tcpDown fired with nil cause")
	}

	return m.tcpDownErr
}

func (m *recRT) tcpDownDidFire() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.tcpDownFired
}

// ── harness ─────────────────────────────────────────────────────────────────────

// startReader wires an active loopback transport to rt and returns the running transport
// plus the peer end of the connection (from which the test writes frames byte-streams and
// reads any Reject the reader emits). setups run after newTransport but BEFORE Start, so a
// test can install the allocFrame seam before the recv loop spawns (J2).
func startReader(t *testing.T, rt *recRT, opts []Option, setups ...func(*transport)) *net.TCPConn {
	t.Helper()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	peerCh := acceptOneAsync(ln)

	cfg, err := NewConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	tr := newTransport(cfg)
	for _, s := range setups {
		s(tr)
	}

	require.NoError(t, tr.Start(context.Background(), rt))
	t.Cleanup(func() { _ = tr.Stop(context.Background()) })

	var peer *net.TCPConn
	select {
	case peer = <-peerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept the connection in time")
	}
	t.Cleanup(func() { _ = peer.Close() })

	return peer
}

// frameBytes assembles a raw on-wire HSMS frame: a 4-byte big-endian msgLen prefix,
// then header, then body. msgLen is supplied explicitly so malformed frames (a length
// field that disagrees with the payload, an oversized length) can be constructed.
func frameBytes(msgLen uint32, header []byte, body []byte) []byte {
	out := make([]byte, 0, 4+len(header)+len(body))
	out = binary.BigEndian.AppendUint32(out, msgLen)
	out = append(out, header...)
	out = append(out, body...)

	return out
}

// header10 builds a 10-byte HSMS header with the given sType/pType and a fixed
// session/system-bytes pattern.
func header10(pType, sType byte) []byte {
	h := make([]byte, 10)
	h[0] = 0xFF // session ID hi
	h[1] = 0xFF // session ID lo
	h[4] = pType
	h[5] = sType
	h[6] = 0xDE // system bytes
	h[7] = 0xAD
	h[8] = 0xBE
	h[9] = 0xEF

	return h
}

// expectNoPeerBytes asserts that conn delivers no bytes within timeout (used to prove the
// reader sent NO farewell Separate after a peer Separate).
func expectNoPeerBytes(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))

	var buf [1]byte
	n, err := conn.Read(buf[:])
	require.Zero(t, n, "expected no bytes from peer, got %d", n)
	require.Error(t, err, "expected a read timeout (no farewell), got nil error")

	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
		"expected a timeout error, got %v", err)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestReader_ValidDataFrameDelivered — a well-formed data frame (SType 0, non-empty body)
// is handed to rt.DeliverOwnedFrame as the owned [header||body] (NO 4-byte length prefix).
func TestReader_ValidDataFrameDelivered(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	body := []byte{0x01, 0x02, 0x03}
	header := header10(0, byte(hsms.DataMsgType))
	wire := frameBytes(uint32(len(header)+len(body)), header, body)

	_, err := peer.Write(wire)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return rt.deliveredCount() == 1 },
		5*time.Second, 10*time.Millisecond, "data frame must reach DeliverOwnedFrame")

	want := append(append([]byte(nil), header...), body...)
	require.Equal(t, want, rt.lastDelivered(), "delivered frame must be header||body, no length prefix")
	require.Equal(t, 0, rt.routedCount(), "a data frame must not be routed as a reply")
}

// TestReader_ValidControlRspRouted — a Linktest.rsp (SType 6, len 10) is decoded and routed
// to the waiting sender via rt.RouteReply (not delivered as data).
func TestReader_ValidControlRspRouted(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	header := header10(0, byte(hsms.LinktestRspType))
	wire := frameBytes(10, header, nil)

	_, err := peer.Write(wire)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return rt.routedCount() == 1 },
		5*time.Second, 10*time.Millisecond, "Linktest.rsp must reach RouteReply")

	require.Equal(t, 0, rt.deliveredCount(), "a control rsp must not be delivered as data")
	require.Equal(t, hsms.LinktestRspType, rt.lastRouted().Type())
}

// TestReader_LenBelow10ProtocolError (J2 lower bound) — a length field < 10 is a protocol
// error, not a zero-body data message: the generation ends via rt.TCPDown, with no body
// allocation and no panic.
func TestReader_LenBelow10ProtocolError(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	_, err := peer.Write(frameBytes(5, nil, nil)) // msgLen=5, no payload
	require.NoError(t, err)

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("length < 10 must end the generation via TCPDown")
	}

	require.Error(t, rt.tcpDownCause())
	require.Equal(t, 0, rt.deliveredCount(), "a sub-10 length must not be delivered as data")
}

// TestReader_OversizedLengthRejectedBeforeAlloc (J2) — an attacker-controlled length beyond
// secs2.MaxByteSize is rejected BEFORE make([]byte, msgLen). The allocFrame seam records
// any oversized allocation attempt; it must never fire.
func TestReader_OversizedLengthRejectedBeforeAlloc(t *testing.T) {
	t.Parallel()

	const maxGuard = 1 << 20 // any frame alloc above 1 MiB is treated as the attacker length

	var trippedAt atomic.Int64
	guard := func(n int) []byte {
		if n > maxGuard {
			trippedAt.Store(int64(n))
			return make([]byte, 1) // never allocate the attacker-sized buffer in the test
		}

		return make([]byte, n)
	}

	rt := newRecRT()
	peer := startReader(t, rt, nil, func(tr *transport) { tr.allocFrame = guard })

	hugeLen := uint32(secs2.MaxByteSize) + 1
	require.NoError(t, binary.Write(peer, binary.BigEndian, hugeLen))
	_ = peer.Close()

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("oversized length must end the generation via TCPDown")
	}

	require.Zero(t, trippedAt.Load(),
		"oversized msgLen must be validated BEFORE make([]byte, msgLen) (J2); allocFrame was called with %d bytes",
		trippedAt.Load())
}

// TestReader_ControlFrameLenNot10Rejected — a Select.req carrying a body (len=20) is
// malformed (standard control is header-only, exactly 10). It is answered with a Reject.req
// routed through rt.SendAsync and NOT delivered as a body.
func TestReader_ControlFrameLenNot10Rejected(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	header := header10(0, byte(hsms.SelectReqType))
	body := make([]byte, 10) // 10-byte spurious body → msgLen=20
	wire := frameBytes(20, header, body)

	_, err := peer.Write(wire)
	require.NoError(t, err)

	// Reject is enqueued via rt.SendAsync (not written to the raw conn).
	require.Eventually(t, func() bool { return rt.sentCount() == 1 },
		2*time.Second, 10*time.Millisecond, "malformed control frame must trigger a Reject via rt.SendAsync")

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.RejectReqType, got.Type(), "reader must answer with a Reject.req")

	require.Equal(t, 0, rt.deliveredCount(), "a malformed control frame must not be delivered")
	require.Equal(t, 0, rt.routedCount(), "a malformed control frame must not be routed")
	require.False(t, rt.tcpDownDidFire(), "a Reject must keep the link (no TCPDown)")
}

// TestReader_UnsupportedSTypeRejectsKeepsLink (J3, §7.10.3) — an undefined SType is answered
// with a Reject.req (routed through rt.SendAsync) while the link stays up: a subsequent valid
// data frame is still delivered. J3 teeth: if the reader tore the link down instead of
// Rejecting, tcpDownDidFire() would be true AND the following data frame would never arrive —
// both assertions would fail.
func TestReader_UnsupportedSTypeRejectsKeepsLink(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	const badSType = 8 // undefined per SEMI E37
	_, err := peer.Write(frameBytes(10, header10(0, badSType), nil))
	require.NoError(t, err)

	// Reject is enqueued via rt.SendAsync; check the recorded message for correct SType/reason.
	require.Eventually(t, func() bool { return rt.sentCount() == 1 },
		2*time.Second, 10*time.Millisecond, "unsupported SType must be Rejected via rt.SendAsync (J3)")

	got := rt.lastSent()
	require.NotNil(t, got)
	h := got.HeaderBytes()
	require.Equal(t, byte(hsms.RejectReqType), h[5], "unsupported SType must be Rejected")
	require.Equal(t, byte(hsms.RejectSTypeNotSupported), h[3], "reason must be SType-not-supported")
	require.Equal(t, byte(badSType), h[2], "Reject must echo the offending SType")
	require.False(t, rt.tcpDownDidFire(), "Reject must not tear down (J3)")

	// The link is still up — a valid data frame delivered after the Reject proves it.
	body := []byte{0xAA}
	dh := header10(0, byte(hsms.DataMsgType))
	_, err = peer.Write(frameBytes(uint32(len(dh)+len(body)), dh, body))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return rt.deliveredCount() == 1 },
		5*time.Second, 10*time.Millisecond, "link must stay up after a Reject (J3)")
}

// TestReader_BadPTypeRejectsKeepsLink (J3) — a non-zero PType is answered with a Reject.req
// (reason PType-not-supported) routed through rt.SendAsync and the link stays up.
func TestReader_BadPTypeRejectsKeepsLink(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	peer := startReader(t, rt, nil)

	const badPType = 3
	_, err := peer.Write(frameBytes(10, header10(badPType, byte(hsms.LinktestReqType)), nil))
	require.NoError(t, err)

	// Reject is enqueued via rt.SendAsync; check the recorded message for correct PType/reason.
	require.Eventually(t, func() bool { return rt.sentCount() == 1 },
		2*time.Second, 10*time.Millisecond, "bad PType must be Rejected via rt.SendAsync (J3)")

	got := rt.lastSent()
	require.NotNil(t, got)
	h := got.HeaderBytes()
	require.Equal(t, byte(hsms.RejectReqType), h[5], "bad PType must be Rejected")
	require.Equal(t, byte(hsms.RejectPTypeNotSupported), h[3], "reason must be PType-not-supported")
	require.Equal(t, byte(badPType), h[2], "Reject must echo the offending PType")
	require.False(t, rt.tcpDownDidFire(), "Reject must not tear down (J3)")
}

// TestReader_SelectReqAnswered — a well-formed Select.req (len 10) is answered by the Select
// responder (H2, §7.D): it is neither delivered as data nor Rejected, but produces a
// Select.rsp enqueued via rt.SendAsync while the link stays up (a following data frame is
// delivered because recRT reports Selected).
func TestReader_SelectReqAnswered(t *testing.T) {
	t.Parallel()

	rt := newRecRT() // recRT defaults to Selected, so the following data frame is delivered
	peer := startReader(t, rt, nil)

	_, err := peer.Write(frameBytes(10, header10(0, byte(hsms.SelectReqType)), nil))
	require.NoError(t, err)

	// The responder enqueues a Select.rsp (SType 2) via SendAsync (not a Reject, not a teardown).
	require.Eventually(t, func() bool { return rt.sentCount() == 1 },
		2*time.Second, 10*time.Millisecond, "Select.req must be answered with a Select.rsp via SendAsync")

	got := rt.lastSent()
	require.NotNil(t, got)
	require.Equal(t, hsms.SelectRspType, got.Type(), "responder must answer Select.req with a Select.rsp")

	// A following data frame must still arrive → the loop kept reading after the responder ran.
	body := []byte{0x42}
	dh := header10(0, byte(hsms.DataMsgType))
	_, err = peer.Write(frameBytes(uint32(len(dh)+len(body)), dh, body))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return rt.deliveredCount() == 1 },
		5*time.Second, 10*time.Millisecond, "loop must keep reading after a control req")

	require.Equal(t, 0, rt.routedCount(), "a control req is not routed as a reply")
	require.False(t, rt.tcpDownDidFire())
}

// TestReader_SlowTrickleWithinT8PerGapSucceeds (J1) — each inter-byte gap is < T8 but the
// whole frame takes >> T8. A correct per-gap deadline reset lets it through; a single
// whole-frame deadline (the teeth-check) makes it time out.
func TestReader_SlowTrickleWithinT8PerGapSucceeds(t *testing.T) {
	t.Parallel()

	const t8 = 100 * time.Millisecond
	const gap = 30 * time.Millisecond

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T8: t8}) // readFrame sources T8 LIVE from rt (Finding 2), not the frozen cfg
	peer := startReader(t, rt, nil)

	body := []byte{0x01, 0x02, 0x03}
	header := header10(0, byte(hsms.DataMsgType))
	wire := frameBytes(uint32(len(header)+len(body)), header, body) // 4 + 13 = 17 bytes

	go func() {
		for _, b := range wire {
			time.Sleep(gap) // < T8 each gap; total (17*30ms=510ms) >> T8
			if _, err := peer.Write([]byte{b}); err != nil {
				return
			}
		}
	}()

	require.Eventually(t, func() bool { return rt.deliveredCount() == 1 },
		5*time.Second, 10*time.Millisecond,
		"per-gap T8 reset must let a slow-but-never-stalled frame through (J1)")
	require.False(t, rt.tcpDownDidFire(), "a within-T8 trickle must not trip T8")
}

// TestReader_StallMidLengthHeaderTripsT8 (J1) — one byte of the 4-byte length prefix arrives,
// then the stream stalls > T8. T8 governs the gap across the length header, so TCPDown fires.
func TestReader_StallMidLengthHeaderTripsT8(t *testing.T) {
	t.Parallel()

	const t8 = 80 * time.Millisecond

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T8: t8}) // readFrame sources T8 LIVE from rt (Finding 2), not the frozen cfg
	peer := startReader(t, rt, nil)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	go func() {
		_, _ = peer.Write([]byte{0x00}) // 1 byte of the length prefix, then stall
		<-stop
	}()

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("T8 must trip on a stall across the length header (J1)")
	}
	require.Error(t, rt.tcpDownCause())
	require.Equal(t, 0, rt.deliveredCount())
}

// TestReader_RecvLoopUsesLiveT8NotFrozenCfg is the Finding-2 guard: readFrame must source T8 from
// the LIVE runtime (rt.Timers().T8), NOT the transport's frozen cfg copy — so a runtime T8 change
// (UpdateConfigOptions(hsms.WithT8(...)), modelled here by the mock runtime's live timer set) reaches
// the recv loop. The frozen cfg is given a deliberately huge, contradictory T8 (30s) while the live
// runtime carries a small T8 (60ms); a mid-length-header stall must trip at the SMALL live value.
// TEETH: reverting readFrame to t.cfg.Timers().T8 makes the huge frozen value govern and this test
// times out.
func TestReader_RecvLoopUsesLiveT8NotFrozenCfg(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	// The live T8 the recv loop must observe (what UpdateConfigOptions(hsms.WithT8) would publish).
	rt.setTimers(hsms.TimerConfig{T8: 60 * time.Millisecond})

	// The frozen cfg carries a huge, contradictory T8 — it must NOT govern the recv framing loop.
	peer := startReader(t, rt, []Option{WithConnectionOption(hsms.WithT8(30 * time.Second))})

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	go func() {
		_, _ = peer.Write([]byte{0x00}) // 1 byte of the length prefix, then stall
		<-stop
	}()

	select {
	case <-rt.tcpDownCh:
	case <-time.After(2 * time.Second):
		t.Fatal("recv loop must trip on the LIVE 60ms T8, not the frozen 30s cfg T8 (Finding 2)")
	}
	require.Error(t, rt.tcpDownCause())
}

// TestReader_PeerSeparateWhileSelected — a Separate.req (SType 9) received while Selected
// tears the link down (TCPDown, comms-failure cause) and the reader sends NO farewell
// Separate back (§7.9.2). While NOT Selected the Separate is ignored and the loop keeps
// reading.
func TestReader_PeerSeparateWhileSelected(t *testing.T) {
	t.Parallel()

	t.Run("while_selected_disconnects_no_farewell", func(t *testing.T) {
		t.Parallel()

		rt := newRecRT()
		rt.setState(hsms.SelectedState)
		peer := startReader(t, rt, nil)

		_, err := peer.Write(frameBytes(10, header10(0, byte(hsms.SeparateReqType)), nil))
		require.NoError(t, err)

		select {
		case <-rt.tcpDownCh:
		case <-time.After(2 * time.Second):
			t.Fatal("peer Separate while Selected must drive TCPDown")
		}
		require.ErrorIs(t, rt.tcpDownCause(), errPeerSeparate)
		require.Equal(t, 0, rt.deliveredCount(), "a Separate must not be delivered as data")

		// The reader must NOT answer with a farewell Separate (§7.9.2).
		expectNoPeerBytes(t, peer, 200*time.Millisecond)
	})

	t.Run("while_not_selected_ignored", func(t *testing.T) {
		t.Parallel()

		rt := newRecRT()
		rt.setState(hsms.NotSelectedState)
		peer := startReader(t, rt, nil)

		_, err := peer.Write(frameBytes(10, header10(0, byte(hsms.SeparateReqType)), nil))
		require.NoError(t, err)

		// The loop must keep reading after ignoring the Separate. Probe with a data frame:
		// while NotSelected, a data message is refused with Reject(reason 4) and the link is
		// kept (§7.10.3 / §6.3), so seeing the Reject proves the loop read the frame — and the
		// data is NOT delivered as a body.
		body := []byte{0x07}
		dh := header10(0, byte(hsms.DataMsgType))
		_, err = peer.Write(frameBytes(uint32(len(dh)+len(body)), dh, body))
		require.NoError(t, err)

		require.Eventually(t, func() bool { return rt.sentCount() == 1 },
			5*time.Second, 10*time.Millisecond, "data while NotSelected must be Rejected (loop kept reading)")

		got := rt.lastSent()
		require.NotNil(t, got)
		h := got.HeaderBytes()
		require.Equal(t, byte(hsms.RejectReqType), h[5], "data while NotSelected must be Rejected")
		require.Equal(t, byte(hsms.RejectNotSelected), h[3], "reason must be NotSelected (4)")
		require.Equal(t, 0, rt.deliveredCount(), "data while NotSelected must not be delivered as a body")
		require.False(t, rt.tcpDownDidFire(), "a NotSelected Separate/data must not tear down")
	})
}
