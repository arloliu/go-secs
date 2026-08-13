package hsmsss

// passive_test.go — full-connection loopback tests for the passive-role connect procedure
// (spec §6.3 / §7.D): listen/accept, refuse a 2nd connection, async background Start, and the
// shared H2 synchronous-commit Select responder (REUSED from the active build, handleSelectReq).
//
// Each test drives a REAL passive connection (hsms.NewConnection + Open(OpenBackground), real
// supervisor / FSM / recv loop) that LISTENS on 127.0.0.1, and a scripted CLIENT that dials it
// and speaks HSMS. All under -race. The file is package hsmsss (not hsmsss_test) so it can build
// the unexported *transport and reuse the frame helpers from active_test.go / reader_test.go
// (selectReqFrame, dataFrame, peerReadFrame, frameBytes).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// ── harness ─────────────────────────────────────────────────────────────────────

// portLockDir is where the cross-process port reservations this file takes live.
// It is deliberately under os.TempDir rather than t.TempDir:
// the reservation has to be a rendezvous SHARED by every test binary
// `go test ./... -p N` runs concurrently (secs1 and hsms allocate loopback ports the same way),
// so it cannot be scoped to one test, one package, or one process.
var portLockDir = filepath.Join(os.TempDir(), "go-secs-test-ports")

// reserveTestPort takes an exclusive, OS-HELD reservation on port and reports whether it got one.
// The returned release is idempotent and frees the reservation.
//
// The lock is an flock on a per-port file, which is what makes this work across processes:
// a package-level map only ever excluded callers inside ONE test binary,
// while the CI gate (`go test ./... -p $(GO_TEST_P)`, see the Makefile) runs several of them at once,
// each picking from the same ephemeral range.
// During any picker's close-before-real-bind window another binary could pick and confirm the same port,
// which is the full-suite "bind: address already in use" failure class.
//
// flock also excludes goroutines within this process — each reservation opens its own file description —
// so it subsumes the mutex-and-map guard it replaces.
// A lock file left behind by a killed test run poisons nothing:
// the kernel drops an flock when the holding process dies, and the empty file is then re-lockable.
func reserveTestPort(t *testing.T, port int) (release func(), ok bool) {
	t.Helper()

	if err := os.MkdirAll(portLockDir, 0o700); err != nil {
		return nil, false
	}

	//nolint:gosec // a test-only path under os.TempDir, built from an int port number.
	f, err := os.OpenFile(filepath.Join(portLockDir, strconv.Itoa(port)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		return nil, false
	}

	// Closing the descriptor releases the flock, so one idempotent close is the whole release.
	return sync.OnceFunc(func() { _ = f.Close() }), true
}

// freeLoopbackPort binds 127.0.0.1:0, records the OS-chosen port, and closes the listener
// so a passive connection can bind that concrete port.
// Go listeners set SO_REUSEADDR,
// so the rebind (here and across reconnect generations) succeeds despite the brief close.
//
// Under make stress-test's default-GOMAXPROCS -p 32 pass,
// many goroutines call this helper concurrently,
// so a just-freed ephemeral port is disproportionately likely to be reissued to another freeLoopbackPort call
// before this one's caller gets to bind it for real —
// a TOCTOU race observed as "bind: address already in use".
// reserveTestPort closes that window structurally, across processes as well as goroutines:
// the reservation is taken before the confirming bind and held until the CALLER's test ends (t.Cleanup),
// not merely until this function returns,
// so the port cannot be handed to a second caller while the first is still setting up (or mid-test).
// A collision is therefore never fatal: it just means retry with the next OS-assigned port.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	const maxAttempts = 50

	for range maxAttempts {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
		require.NoError(t, err)

		addr, ok := ln.Addr().(*net.TCPAddr)
		require.True(t, ok, "listener addr must be *net.TCPAddr")
		require.NoError(t, ln.Close())

		release, reserved := reserveTestPort(t, addr.Port)
		if !reserved {
			continue // another in-flight (or still-running) test already owns this port; retry
		}

		// Confirm the port is actually rebindable right now (guards against an OS-level reuse
		// delay unrelated to this package's own callers, e.g. a lingering TIME_WAIT edge case).
		// A failure here releases the reservation and retries rather than failing the test.
		confirm, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: addr.Port})
		if err != nil {
			release()

			continue
		}
		require.NoError(t, confirm.Close())

		t.Cleanup(release)

		return addr.Port
	}

	t.Fatalf("freeLoopbackPort: failed to acquire a free, unreserved port after %d attempts", maxAttempts)

	return 0
}

// newPassiveConn builds a full passive-role HSMS-SS connection listening on 127.0.0.1:port,
// wiring a real hsmsss *transport into the shared hsms core. opts tune the shared timers.
func newPassiveConn(t *testing.T, port int, opts ...hsms.ConnOption) hsms.Connection {
	t.Helper()

	conn, _ := newPassiveConnTr(t, port, opts...)

	return conn
}

// newPassiveConnTr is newPassiveConn but also returns the underlying *transport, so a test can
// reach transport-only state hsms.Connection does not expose: the passive-refusal handoff
// fields (refuseMu/refuseStopped/refuseConn) and the test-only refusePrePublishHook seam used to
// drive the Stop-races-prepublication race deterministically.
func newPassiveConnTr(t *testing.T, port int, opts ...hsms.ConnOption) (hsms.Connection, *transport) {
	t.Helper()

	hopts := make([]Option, 0, len(opts)+1)
	hopts = append(hopts, WithPassive())

	for _, o := range opts {
		hopts = append(hopts, WithConnectionOption(o))
	}

	cfg, err := NewConfig("127.0.0.1", port, hopts...)
	require.NoError(t, err)

	tr := newTransport(cfg)

	conn, err := hsms.NewConnection(&cfg.ConnectionConfig, tr)
	require.NoError(t, err)

	return conn, tr
}

// dialPassive dials the passive listener at 127.0.0.1:port as a scripted client. It retries
// (require.Eventually) so it tolerates the reconnect re-listen window, where the passive rebinds
// the same port a moment after a drop.
func dialPassive(t *testing.T, port int) *net.TCPConn {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	var tcpConn *net.TCPConn

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return false
		}

		c, ok := conn.(*net.TCPConn)
		if !ok {
			_ = conn.Close()

			return false
		}

		tcpConn = c

		return true
	}, 5*time.Second, 20*time.Millisecond, "client must be able to dial the passive listener at %s", addr)

	return tcpConn
}

// waitNextGeneration blocks until tr's current generation differs from prevGen —
// i.e. until the reconnect loop has published a fresh generation for the transport to use.
//
// This is the correct fence for a test that must dial again after a passive drop.
// The reconnect loop's connectLoop (hsms/connection_lifecycle.go) publishes the new generation
// (c.cur.Store, which is what CurrentGeneration surfaces) only AFTER prev.wait() —
// the prior epoch's FULL teardown, including the transport Stop that closes the prior generation's listener
// and joins its accept goroutine (see transport_passive.go's startPassive doc and hsms/epoch.go's join).
// So by the time this returns, the OLD listener is guaranteed closed.
//
// conn.State()==NotConnectedState alone is NOT that fence:
// it fires as soon as the FSM processes TCPDown, which happens BEFORE react runs
// (react is what actually kicks the async tr.Stop that closes the old listener).
// A dial that races that window can land on the dying OLD listener's backlog
// and be treated as an EXTRA connection of the ending generation (refused with SelectStatusAlreadyActive)
// instead of reaching the fresh generation's real Select responder — the passive-reconnect flake this helper closes.
func waitNextGeneration(t *testing.T, tr *transport, prevGen uint64) {
	t.Helper()

	require.Eventually(t, func() bool {
		g := tr.currentGeneration()

		return g != 0 && g != prevGen
	}, 5*time.Second, time.Millisecond,
		"the reconnect loop must publish a fresh generation (old listener fully closed) before dialing again")
}

// expectSelectRsp reads one frame from client and asserts it is a success Select.rsp — the passive
// side's H2 responder answer to a genuine (NotSelected->Selected) Select.req.
func expectSelectRsp(t *testing.T, client net.Conn) {
	t.Helper()

	expectSelectRspStatus(t, client, hsms.SelectStatusSuccess)
}

// expectSelectRspStatus reads one frame from client and asserts it is a Select.rsp carrying the
// given SelectStatus (0 = Communication Established; 1 = Communication Already Active for a
// duplicate Select while already Selected, E37 Table 7 / M5).
// wantSysBytes, if given, additionally asserts the response echoes those System Bytes (E37
// §8.3.4.4 — a Select.rsp mirrors the System Bytes of the Select.req it answers).
func expectSelectRspStatus(t *testing.T, client net.Conn, wantStatus byte, wantSysBytes ...[4]byte) {
	t.Helper()

	frame, err := peerReadFrame(client, 5*time.Second)
	require.NoError(t, err, "client must receive the passive side's Select.rsp")
	require.GreaterOrEqual(t, len(frame), 10, "control frame must carry a 10-byte header")
	require.Equal(t, byte(hsms.SelectRspType), frame[5], "passive must answer a Select.req with a Select.rsp")
	require.Equal(t, wantStatus, frame[3], "passive Select.rsp must carry the expected SelectStatus")

	if len(wantSysBytes) > 0 {
		require.Equal(t, wantSysBytes[0][:], frame[6:10], "Select.rsp must echo the request's System Bytes")
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestPassive_AcceptsAndSelects — a passive connection Opens (background), a scripted client dials
// and sends Select.req, and the passive side answers Select.rsp and reaches Selected via the shared
// H2 responder (handleSelectReq). Passive never INITIATES Select — it only responds (§6.3 / §7.D).
func TestPassive_AcceptsAndSelects(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// With no peer yet, the passive sits at NotConnected (listening) — it does NOT initiate Select.
	require.Equal(t, hsms.NotConnectedState, conn.State(),
		"passive must be listening (NotConnected) right after background Open, before any peer")

	client := dialPassive(t, port)
	t.Cleanup(func() { _ = client.Close() })

	_, err := client.Write(selectReqFrame([4]byte{0xAA, 0xBB, 0xCC, 0xDD}))
	require.NoError(t, err)

	expectSelectRsp(t, client)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive must reach Selected after responding to Select.req")
}

// TestPassive_OpenBackgroundReturnsBeforePeer — Open(OpenBackground) on a passive connection MUST
// return promptly with NO peer connected: the listen/accept is async inside the transport (the
// accept runs on a goroutine, not inline in Start). Teeth: a synchronous blocking AcceptTCP in
// Start would block Open here → the 3 s timeout fires → the test fails.
func TestPassive_OpenBackgroundReturnsBeforePeer(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port)
	t.Cleanup(func() { _ = conn.Close() })

	done := make(chan error, 1)
	go func() { done <- conn.Open(context.Background(), hsms.OpenBackground) }()

	select {
	case err := <-done:
		require.NoError(t, err, "passive Open(OpenBackground) must succeed")
	case <-time.After(3 * time.Second):
		t.Fatal("passive Open(OpenBackground) blocked waiting for a peer — the accept must be async " +
			"(teeth: a synchronous AcceptTCP in Start hangs Open here)")
	}

	// No peer has connected, so the passive must sit at NotConnected (listening), never Selected.
	require.Equal(t, hsms.NotConnectedState, conn.State(),
		"with no peer connected, the passive side stays NotConnected (listening)")
}

// TestPassive_RefusesSecondConnection — one peer connected + selected; a 2nd peer dials while the
// session is live and sends a Select.req.
// HSMS-SS is single-session (§3 / §6.3), and E37 §9.2.4.1.1 option 1 (the standard's OWN
// "preferred option") is: accept, but answer the Select
// with Communication Already Active. So the 2nd connection must receive a Select.rsp carrying
// status 1 and then be closed — NOT be silently accepted-then-closed with no response at all
// (none of E37's three listed procedures) — and the live 1st session must be undisturbed
// throughout: the counter-assertion that catches a refusal path tearing down the wrong connection.
func TestPassive_RefusesSecondConnection(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	// t.Context() is safe here: Open's ctx bounds ONLY the OpenWaitSelected wait
	// (hsms/connection_lifecycle.go's Open — e.ctx, threaded to Start/Close, is rooted at a
	// bare context.Background(), never the caller's ctx); this test uses OpenBackground, so the
	// ctx is not retained past this call and its cancellation at cleanup cannot affect Close.
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// First peer: connect + select → the single live session.
	client1 := dialPassive(t, port)
	t.Cleanup(func() { _ = client1.Close() })

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	// Second peer dials while the first session is live and sends its own Select.req.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	client2SysBytes := [4]byte{0x0B, 0x0B, 0x0B, 0x0B}
	_, err = client2.Write(selectReqFrame(client2SysBytes))
	require.NoError(t, err)

	// The 2nd connection must receive Select.rsp status 1 (Communication Already Active, E37
	// §9.2.4.1.1 option 1 / Table 7) directly on ITS OWN socket — not silence, and not routed to
	// the live 1st session.
	// It must also echo THIS request's System Bytes, not some other transaction's.
	expectSelectRspStatus(t, client2, hsms.SelectStatusAlreadyActive, client2SysBytes)

	// The live 1st session must stay Selected throughout: refusing the 2nd connection must never
	// disturb it.
	require.Equal(t, hsms.SelectedState, conn.State(),
		"the live 1st session must stay Selected while the 2nd connection is refused")

	// The 2nd connection is then closed (the helper's defer Close, after its one-shot answer).
	require.NoError(t, client2.SetReadDeadline(time.Now().Add(3*time.Second)))

	buf := make([]byte, 1)
	_, rerr := client2.Read(buf)
	require.Error(t, rerr, "the 2nd connection must be closed after its Select.rsp")

	// Teeth: a CLOSED connection reads EOF / reset; a connection merely left open (not refused)
	// reads a net timeout.
	// Reject a timeout so removing the refuse-close makes this test fail.
	var netErr net.Error
	if errors.As(rerr, &netErr) {
		require.False(t, netErr.Timeout(),
			"the 2nd connection must be CLOSED (EOF/reset), not merely idle — a read timeout means it was not refused")
	}

	// The live 1st session must still be usable: a duplicate Select.req on the LIVE connection
	// (routed through the normal H2 responder, not the refusal path) is likewise answered status 1,
	// proving the link is intact and still serviced.
	_, err = client1.Write(selectReqFrame([4]byte{0x05, 0x06, 0x07, 0x08}))
	require.NoError(t, err, "the 1st connection must still be writable after the 2nd was refused")
	expectSelectRspStatus(t, client1, hsms.SelectStatusAlreadyActive)

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the 1st session stays Selected after the refused 2nd dial")
}

// TestPassive_RefuseExtraConn_IdleDialerClosedAtAbsoluteDeadline is the teeth-check against
// reusing readFrame/readN for the refusal exchange (see refuseExtraConn's doc comment).
// A 2nd dialer connects and sends NOTHING: readN's idle-first-byte policy would clear the read
// deadline and block forever, so with the fix reverted this test hangs past its own bound
// instead of observing a server-side close.
// With the fix, an ABSOLUTE deadline (armed once, from t.rt.Timers().T7) closes the idle socket
// without ever reading a byte, and — critically — the accept loop is not stuck: a 3rd dialer
// right after is still served, proving no goroutine leaked/wedged.
func TestPassive_RefuseExtraConn_IdleDialerClosedAtAbsoluteDeadline(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	// T7 doubles as the live session's NOT-SELECTED dwell (armT7 on TCPUp), so it must stay large
	// enough that client1's near-instant Select doesn't race a T7 expiry under load; ~1s is short
	// enough to keep the test fast and long enough to avoid that flake.
	const t7 = time.Second

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second), hsms.WithT7(t7))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	client1 := dialPassive(t, port)
	t.Cleanup(func() { _ = client1.Close() })

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	// 2nd dialer: connect, send nothing.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	// Give the client-side read a deadline well past t7 (3s vs t7=1s): the discriminating signal
	// is WHICH side closed the connection, not how fast — a server-side close arrives well before
	// this client deadline, while the pre-fix readN reuse would hit THIS timeout instead (the
	// idle-first-byte wait never expires on its own).
	require.NoError(t, client2.SetReadDeadline(time.Now().Add(3*time.Second)))

	buf := make([]byte, 1)
	_, rerr := client2.Read(buf)
	require.Error(t, rerr, "the idle 2nd dialer must be closed once the absolute refusal deadline expires")

	var netErr net.Error
	if errors.As(rerr, &netErr) {
		require.False(t, netErr.Timeout(),
			"must be closed by the server at ~T7, not merely time out client-side "+
				"(teeth: readN's idle-first-byte deadline-clear would produce exactly this timeout)")
	}

	// No leaked/wedged goroutine: the SAME serial accept loop must still be alive to serve a 3rd
	// dialer right after — if refuseExtraConn had gotten stuck on client2, this dial would never
	// be answered.
	client3 := dialPassive(t, port)
	t.Cleanup(func() { _ = client3.Close() })

	client3SysBytes := [4]byte{0x09, 0x0A, 0x0B, 0x0C}
	_, err = client3.Write(selectReqFrame(client3SysBytes))
	require.NoError(t, err)
	expectSelectRspStatus(t, client3, hsms.SelectStatusAlreadyActive, client3SysBytes)

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the live 1st session must stay Selected throughout")
}

// refuseSetDeadlineRecorder wraps a net.Conn and records the argument of every SetDeadline call
// (refuseExtraConn's absolute-bound primitive, distinct from deadlineRecorder's SetReadDeadline in transport_recv_clock_test.go)
// so a test can assert exactly how many times it was called and with what value, while still delegating to the wrapped conn.
type refuseSetDeadlineRecorder struct {
	net.Conn
	mu    sync.Mutex
	calls []time.Time
}

func (r *refuseSetDeadlineRecorder) SetDeadline(t time.Time) error {
	r.mu.Lock()
	r.calls = append(r.calls, t)
	r.mu.Unlock()

	return r.Conn.SetDeadline(t)
}

func (r *refuseSetDeadlineRecorder) deadlineCalls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]time.Time(nil), r.calls...)
}

// TestPassive_RefuseExtraConn_ArmsExactlyOneAbsoluteT7Deadline is the deterministic teeth-check
// for the T7 absolute-deadline EXPRESSION itself (clock-now + T7), complementing
// IdleDialerClosedAtAbsoluteDeadline above, which only proves SOME server-side close happens.
// That alone would pass for an early close, the wrong timer, or a wrongly computed deadline.
// A fixed fake clock plus a SetDeadline-recording conn pins the exact armed value and proves it
// is set exactly once — never cleared or extended by a later call.
func TestPassive_RefuseExtraConn_ArmsExactlyOneAbsoluteT7Deadline(t *testing.T) {
	t.Parallel()

	// An hour ahead of the wall clock: clearly distinguishable from real time, giving the
	// assertion teeth against a bare time.Now() (mirrors transport_recv_clock_test.go's fakeNow).
	fakeNow := time.Now().Add(time.Hour)
	const t7 = 250 * time.Millisecond

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T7: t7})
	tr := &transport{rt: rt, now: func() time.Time { return fakeNow }}

	server, client := net.Pipe()
	defer func() { _ = client.Close() }()

	rec := &refuseSetDeadlineRecorder{Conn: server}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.refuseExtraConn(rec) // the idle dialer sends nothing; parks in io.ReadFull
	}()

	// Deterministically observe the deadline was armed (no sleep): poll the recorder.
	require.Eventually(t, func() bool {
		return len(rec.deadlineCalls()) >= 1
	}, 15*time.Second, time.Millisecond, "refuseExtraConn must arm the absolute deadline before reading")

	// Release the parked read so refuseExtraConn returns and its goroutine does not outlive this
	// test.
	require.NoError(t, client.Close())

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("refuseExtraConn did not return")
	}

	calls := rec.deadlineCalls()
	require.Len(t, calls, 1, "refuseExtraConn must arm the absolute deadline exactly once — never cleared or extended")

	want := fakeNow.Add(t7)
	require.True(t, want.Equal(calls[0]), "SetDeadline = %v, want clock-now + T7 = %v", calls[0], want)
}

// TestPassive_RefuseExtraConn_NonSelectReqClosedWithoutResponse — a 2nd dialer sends a well-formed
// but non-Select.req control frame (Linktest.req). E37 §9.2.4.1.1 option 1 answers ONLY a Select;
// refuseExtraConn's mandated shape closes any other frame unanswered rather than routing it
// through the normal responder (which would require treating the refused socket as a real session).
func TestPassive_RefuseExtraConn_NonSelectReqClosedWithoutResponse(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	client1 := dialPassive(t, port)
	t.Cleanup(func() { _ = client1.Close() })

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	_, err = client2.Write(frameBytes(10, header10(0, byte(hsms.LinktestReqType)), nil))
	require.NoError(t, err)

	// No response at all — not even a truncated/partial frame.
	// A single raw Read (not peerReadFrame's io.ReadFull,
	// which would also error on a SHORT write and so cannot tell "wrote nothing" apart from "wrote a few bytes then closed")
	// must return ZERO bytes alongside the closed-socket error:
	// the mandated shape is close-without-responding, never a partial write.
	require.NoError(t, client2.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 32)
	n, rerr := client2.Read(buf)
	require.Error(t, rerr, "a non-Select.req on the refused 2nd connection must get NO response")
	require.Equal(t, 0, n, "the refused connection must write ZERO bytes — not even a partial frame — for a non-Select.req")

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the live 1st session must stay Selected throughout")
}

// TestPassive_RefuseExtraConn_StopDuringInFlightRefusalReturnsPromptly proves Stop does not wait
// out the refusal deadline when a refusal is already published and parked in its read (the
// two-sided handoff's Stop side, haltRefusal). T7 is set far longer than the connection's default
// close-timeout (10s, hsms.WithCloseTimeout's default) so that if haltRefusal's close is dropped,
// Stop can only return via ErrCloseTimeout at ~10s or hang past this test's bound — either way a
// clean, unambiguous failure, never a coincidental pass.
func TestPassive_RefuseExtraConn_StopDuringInFlightRefusalReturnsPromptly(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	const t7 = 30 * time.Second

	conn, tr := newPassiveConnTr(t, port, hsms.WithT6(2*time.Second), hsms.WithT7(t7))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	client1 := dialPassive(t, port)
	defer func() { _ = client1.Close() }()

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	// 2nd dialer: connect, send nothing — it parks in refuseExtraConn's io.ReadFull once published.
	client2 := dialPassive(t, port)
	defer func() { _ = client2.Close() }()

	// Deterministically observe publication (no sleep, no hook needed here): poll the same
	// refuseMu-guarded slot refuseExtraConn itself publishes into.
	require.Eventually(t, func() bool {
		tr.refuseMu.Lock()
		defer tr.refuseMu.Unlock()

		return tr.refuseConn != nil
	}, 15*time.Second, 2*time.Millisecond, "the extra socket must be published to the refusal slot")

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- conn.Close() }()

	// No t.Cleanup(conn.Close) registered above (mirrors TestPassive_CloseDuringAcceptConnectWindowIsBounded):
	// if the fix regresses, a second Close at cleanup would itself block.
	select {
	case cerr := <-done:
		require.NoError(t, cerr, "Close must succeed, not time out, while a refusal is in flight")
		require.Less(t, time.Since(start), 3*time.Second,
			"Stop must not wait out the refusal deadline (T7=30s) for an in-flight refusal")
	case <-time.After(4 * time.Second):
		t.Fatal("Stop hung on an in-flight refusal — the two-sided handoff's Stop side did not close it")
	}
}

// TestPassive_RefuseExtraConn_StopRacesPrePublicationWindow drives the Accept-returned/
// not-yet-published race deterministically via the test-only refusePrePublishHook: Stop runs
// (and sets refuseStopped) WHILE the extra socket has been accepted but is still parked before
// refuseExtraConn's publish.
// The losing side (refuseExtraConn, once released) must observe the flag and close without ever
// publishing or reading — proving the handoff has no gap where Stop sees "no in-flight socket"
// yet the helper still settles in for the full deadline.
func TestPassive_RefuseExtraConn_StopRacesPrePublicationWindow(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	const t7 = 30 * time.Second

	conn, tr := newPassiveConnTr(t, port, hsms.WithT6(2*time.Second), hsms.WithT7(t7))

	hookReached := make(chan struct{})
	var hookOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once

	// Fail-safe: whatever else happens, unwedge a parked hook so no goroutine (test or
	// production) can leak past this test's return.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	// Set the hook BEFORE Open (same happens-before discipline as now/allocFrame): assigning a
	// test seam concurrently with the accept goroutine reading it would itself be a data race.
	tr.refusePrePublishHook = func() {
		hookOnce.Do(func() { close(hookReached) })
		<-release // hold the extra socket UNPUBLISHED until the test releases it
	}

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	client1 := dialPassive(t, port)
	defer func() { _ = client1.Close() }()

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	client2 := dialPassive(t, port)
	defer func() { _ = client2.Close() }()

	<-hookReached // Accept has returned; refuseExtraConn is parked BEFORE publishing

	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	// Wait for Stop to reach haltRefusal WHILE the helper is still parked in the hook — at this
	// instant refuseConn is nil (nothing published yet), so this is exactly the race the
	// two-sided handoff (not a bare field) exists to close.
	require.Eventually(t, func() bool {
		tr.refuseMu.Lock()
		defer tr.refuseMu.Unlock()

		return tr.refuseStopped
	}, 15*time.Second, 2*time.Millisecond, "Stop must set refuseStopped even though no socket is published yet")

	// Close cannot complete until the helper (still parked in the hook above) is released, so
	// baselining `start` HERE — rather than before the Eventually poll above — measures Stop's
	// own promptness after the race is resolved, not this test's own polling latency (test 5's
	// StopDuringInFlightRefusalReturnsPromptly baselines the same way, with no poll in between).
	start := time.Now()
	releaseOnce.Do(func() { close(release) }) // let the helper observe refuseStopped and close

	select {
	case cerr := <-done:
		require.NoError(t, cerr, "Close must succeed")
		require.Less(t, time.Since(start), 3*time.Second,
			"Stop must not wait on the extra socket at all when it races the pre-publication window")
	case <-time.After(4 * time.Second):
		t.Fatal("Stop hung racing the accept-returned/not-yet-published window")
	}

	// The extra socket must have been closed by the LOSING side (refuseExtraConn observing
	// refuseStopped and closing via its own defer, without reading).
	require.NoError(t, client2.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, rerr := client2.Read(buf)
	require.Error(t, rerr, "the extra socket must be closed by the losing side of the handoff")
}

// TestPassive_RefuseExtraConn_ArmStartResetsAcrossReconnect is the reconnect teeth-check for the
// refusal handoff (E37 §9.2.4.1.1 option 1): a first generation's involuntary drop drives the
// engine's teardown, which calls tr.Stop — setting refuseStopped — before the reconnect loop
// calls tr.ArmStart and starts a fresh generation.
// A THIRD dialer, extra in the fresh generation, must still receive the conformant status-1
// Select.rsp: if ArmStart did not reset refuseStopped, every later generation's refusal would be
// silently poisoned into close-without-response.
func TestPassive_RefuseExtraConn_ArmStartResetsAcrossReconnect(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn, tr := newPassiveConnTr(t, port, hsms.WithT6(2*time.Second), hsms.WithT5(50*time.Millisecond))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// First generation: connect + select.
	client1 := dialPassive(t, port)

	_, err := client1.Write(selectReqFrame([4]byte{0x11, 0x11, 0x11, 0x11}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first generation must reach Selected")

	genN := tr.currentGeneration()

	// Drop the link: the passive recv loop sees EOF → TCPDown → tr.Stop tears the generation down
	// (setting refuseStopped, even though no refusal was in flight) → the reconnect loop's
	// tr.ArmStart resets it before re-listening.
	require.NoError(t, client1.Close())

	require.Eventually(t, func() bool { return conn.State() == hsms.NotConnectedState },
		5*time.Second, 5*time.Millisecond, "the drop must drive the passive back to NotConnected")

	// Do not dial on NotConnectedState alone — see waitNextGeneration's doc for why that races the OLD listener's close.
	// Wait for the reconnect loop to actually publish the fresh generation.
	waitNextGeneration(t, tr, genN)

	// Second generation: a fresh peer selects again.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	_, err = client2.Write(selectReqFrame([4]byte{0x22, 0x22, 0x22, 0x22}))
	require.NoError(t, err)
	expectSelectRsp(t, client2)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive must re-listen and re-select after a drop")

	// A 3rd dialer, extra for THIS generation, must still be refused per option 1 — proving
	// refuseStopped was reset by ArmStart, not left poisoning this generation from gen 1's Stop.
	client3 := dialPassive(t, port)
	t.Cleanup(func() { _ = client3.Close() })

	client3SysBytes := [4]byte{0x33, 0x33, 0x33, 0x33}
	_, err = client3.Write(selectReqFrame(client3SysBytes))
	require.NoError(t, err)
	expectSelectRspStatus(t, client3, hsms.SelectStatusAlreadyActive, client3SysBytes)

	require.NoError(t, client3.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1)
	_, rerr := client3.Read(buf)
	require.Error(t, rerr, "the refused extra socket must be closed after its status-1 rsp")

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the 2nd generation's live session must stay Selected throughout")
}

// deadlineErrConn is a minimal fake net.Conn whose SetDeadline always errors — the teeth-check
// for refuseExtraConn's close-without-read branch (Step 3's mandated "returning immediately
// (close, no read) if SetDeadline errors" behavior). A real *net.TCPConn essentially never fails
// SetDeadline outside of an already-closed socket, so this branch is otherwise unreachable from
// the full-connection loopback tests above; a bare fake conn exercises it directly, mirroring
// transport_recv_clock_test.go's deadlineRecorder pattern.
type deadlineErrConn struct {
	net.Conn
	closed     atomic.Bool
	readCalled atomic.Bool
}

func (c *deadlineErrConn) SetDeadline(time.Time) error { return errors.New("deadlineErrConn: boom") }

func (c *deadlineErrConn) Read(b []byte) (int, error) {
	c.readCalled.Store(true)

	return 0, io.EOF
}

func (c *deadlineErrConn) Close() error {
	c.closed.Store(true)

	return nil
}

// TestPassive_RefuseExtraConn_SetDeadlineErrorClosesWithoutRead drives refuseExtraConn directly
// (a bare *transport + recRT double, no full connection) against a conn whose SetDeadline always
// fails: without an armed absolute deadline the promised bound does not exist, so the helper must
// close the socket and must NEVER attempt a read.
func TestPassive_RefuseExtraConn_SetDeadlineErrorClosesWithoutRead(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T7: time.Second})

	tr := &transport{rt: rt}
	c := &deadlineErrConn{}

	tr.refuseExtraConn(c)

	require.True(t, c.closed.Load(), "must close even when SetDeadline errors")
	require.False(t, c.readCalled.Load(), "must not read when the absolute deadline could not be armed")
}

// nonComparableConn wraps a net.Conn plus a slice field, so the struct VALUE (not a pointer to it) is non-comparable:
// Go rejects "==" on any struct containing a slice field.
// WithListener (config.go) explicitly permits a custom ListenFunc layering the passive listener on a different transport,
// so an accepted net.Conn whose dynamic type looks like this — a plain value type, not a pointer — is a valid, in-contract connection.
// The net.Conn field is embedded so its methods promote automatically; no method bodies are needed here.
type nonComparableConn struct {
	net.Conn
	tag []byte //nolint:unused // present only to make the struct value non-comparable
}

// TestPassive_RefuseExtraConn_NonComparableConnDoesNotPanic is the P0 regression test, ISOLATED variant:
// it calls tr.refuseExtraConn directly against a nonComparableConn, bypassing the production Accept path.
// Before the fix, refuseExtraConn's cleanup compared "t.refuseConn == extra" as net.Conn interface values.
// Interface equality panics at runtime when the shared dynamic type is non-comparable,
// and a nonComparableConn VALUE (as opposed to a pointer to one) is exactly such a type —
// so passing one through the real refusal path must complete the status-1 exchange and close cleanly, never panic.
// See TestPassive_RefuseExtraConn_WithListenerNonComparableConnDoesNotPanic below for the same proof through the actual WithListener/Accept seam a regression here would escape through.
func TestPassive_RefuseExtraConn_NonComparableConnDoesNotPanic(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setTimers(hsms.TimerConfig{T7: 2 * time.Second})

	tr := &transport{rt: rt}

	// net.Pipe is a real, synchronous, deadline-capable net.Conn pair — refuseExtraConn's
	// SetDeadline/Read/Write calls all work against it exactly as they would against a TCP socket.
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()

	var wrapped net.Conn = nonComparableConn{Conn: server, tag: []byte("marker")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NotPanics(t, func() { tr.refuseExtraConn(wrapped) })
	}()

	sysBytes := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	_, err := client.Write(selectReqFrame(sysBytes))
	require.NoError(t, err)

	frame, rerr := peerReadFrame(client, 2*time.Second)
	require.NoError(t, rerr, "the refusal exchange must complete a Select.rsp even for a non-comparable conn")
	require.GreaterOrEqual(t, len(frame), 10)
	require.Equal(t, byte(hsms.SelectRspType), frame[5])
	require.Equal(t, byte(hsms.SelectStatusAlreadyActive), frame[3])
	require.Equal(t, sysBytes[:], frame[6:10], "Select.rsp must echo this request's System Bytes")

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("refuseExtraConn did not return")
	}

	// refuseExtraConn's own defer already closed the server-side pipe end by the time done
	// closed, so the peer-closed Read below returns immediately without needing its own
	// deadline (net.Pipe's SetReadDeadline errors once the peer end is already closed).
	buf := make([]byte, 1)
	_, rerr = client.Read(buf)
	require.Error(t, rerr, "the socket must be closed after the refusal exchange")
}

// secondAcceptNonComparableListener wraps a net.Listener and passes the FIRST accepted connection through verbatim (the live session),
// but wraps every SUBSEQUENT accepted connection in nonComparableConn before returning it.
// It reproduces a custom WithListener transport (config.go) whose extra-dialer connections are a non-comparable value type on the REAL Accept path —
// the exported seam the P0 panic would have escaped through, per the c10 v2 re-review.
type secondAcceptNonComparableListener struct {
	net.Listener
	mu    sync.Mutex
	count int
}

// Accept satisfies net.Listener: it wraps every accepted connection after the first.
func (l *secondAcceptNonComparableListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	l.count++
	n := l.count
	l.mu.Unlock()

	if n == 1 {
		return conn, nil
	}

	return nonComparableConn{Conn: conn, tag: []byte("marker")}, nil
}

// TestPassive_RefuseExtraConn_WithListenerNonComparableConnDoesNotPanic drives the P0 regression through the REAL production path, complementing the isolated direct-call test above:
// a pipe-backed net.Listener installed via WithListener (config.go's exported custom-transport seam) wraps its SECOND accepted connection in nonComparableConn,
// so the extra dialer's socket reaches refuseExtraConn the way production does — via acceptLoop's own ln.Accept() call, inside the unrecovered accept goroutine —
// rather than via a direct call under require.NotPanics.
// A regression that reintroduces "t.refuseConn == extra" panics inside that goroutine
// and crashes this test binary outright, not just fails one assertion.
func TestPassive_RefuseExtraConn_WithListenerNonComparableConnDoesNotPanic(t *testing.T) {
	t.Parallel()

	fakeLn := newPipeListener()
	defer func() { _ = fakeLn.Close() }()

	wrappedLn := &secondAcceptNonComparableListener{Listener: fakeLn}
	listen := func(_ context.Context, _, _ string) (net.Listener, error) {
		return wrappedLn, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(),
		WithListener(listen),
		WithConnectionOption(hsms.WithT6(2*time.Second)),
		WithConnectionOption(hsms.WithT7(2*time.Second)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// First dialer: the live session, served by the listener's first (unwrapped) Accept.
	peer1 := fakeLn.dialFresh()
	t.Cleanup(func() { _ = peer1.Close() })

	go func() {
		_, werr := peer1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
		if werr != nil {
			return
		}

		for {
			if _, rerr := peerReadFrame(peer1, 5*time.Second); rerr != nil {
				return
			}
		}
	}()

	require.NoError(t, conn.Open(t.Context(), hsms.OpenWaitSelected),
		"passive connection must accept the first (unwrapped) dialer over the fake listener and reach Selected")
	require.Equal(t, hsms.SelectedState, conn.State())

	// Second dialer: the extra connection. secondAcceptNonComparableListener wraps THIS conn as
	// nonComparableConn before acceptLoop's refuse loop (and refuseExtraConn) ever see it.
	peer2 := fakeLn.dialFresh()
	t.Cleanup(func() { _ = peer2.Close() })

	sysBytes := [4]byte{0x0B, 0x0B, 0x0B, 0x0B}
	_, err = peer2.Write(selectReqFrame(sysBytes))
	require.NoError(t, err)

	frame, rerr := peerReadFrame(peer2, 5*time.Second)
	require.NoError(t, rerr, "the refusal exchange must complete a Select.rsp over the real Accept path even for a non-comparable conn")
	require.GreaterOrEqual(t, len(frame), 10)
	require.Equal(t, byte(hsms.SelectRspType), frame[5])
	require.Equal(t, byte(hsms.SelectStatusAlreadyActive), frame[3])
	require.Equal(t, sysBytes[:], frame[6:10], "Select.rsp must echo the 2nd dialer's System Bytes")

	// The 2nd (wrapped) connection must be closed by refuseExtraConn's own defer after its one-shot answer —
	// this proves the PRODUCTION close, not merely this test's t.Cleanup above
	// (whose deferred Close would mask a regression that left the socket open).
	// refuseExtraConn's Close races this test's own goroutine (unlike the isolated variant above, there is no "done" channel here proving the close already happened),
	// and net.Pipe's own SetReadDeadline errors once the PEER end is already closed —
	// so either SetReadDeadline failing or the subsequent Read failing is equally valid proof of the production close.
	var n int
	closeErr := peer2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if closeErr == nil {
		buf := make([]byte, 1)
		n, closeErr = peer2.Read(buf)
	}
	require.Error(t, closeErr, "the 2nd (wrapped) connection must be closed after its Select.rsp")
	require.Zero(t, n, "no bytes should follow the Select.rsp before the close")

	// Reject a timeout so a regression that stops closing the socket cannot pass by coincidence:
	// a CLOSED connection reads EOF/reset (or refuses a new deadline outright);
	// a connection merely left open reads a net timeout.
	var netErr net.Error
	if errors.As(closeErr, &netErr) {
		require.False(t, netErr.Timeout(),
			"the 2nd (wrapped) connection must be CLOSED (EOF/reset), not merely idle — a read timeout means it was not refused")
	}

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the live 1st session must stay Selected while the 2nd (wrapped) connection is refused")
}

// TestPassive_H2NoSpuriousRejectOnPipelinedData — the H2 crux on the passive RESPONDER side (§7.D).
// The client sends Select.req then IMMEDIATELY a data frame, back-to-back in one write, so the data
// lands in the same kernel buffer right after Select.req. The passive responder commits Selected
// synchronously (CommitSelected) BEFORE writing its Select.rsp, so the pipelined data finds
// IsSelected()==true and is NOT spuriously Rejected(NotSelected) (the efb220b regression).
func TestPassive_H2NoSpuriousRejectOnPipelinedData(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	client := dialPassive(t, port)
	t.Cleanup(func() { _ = client.Close() })

	req := selectReqFrame([4]byte{0x11, 0x22, 0x33, 0x44})
	data := dataFrame(0xFFFF, 1, 1, [4]byte{0x55, 0x66, 0x77, 0x88}, nil)
	_, err := client.Write(append(req, data...)) // Select.req THEN data, back-to-back
	require.NoError(t, err)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive must reach Selected via the H2 responder")

	// Collect the passive's replies for a bounded window: exactly one Select.rsp, and NO Reject.req
	// for the pipelined data (any Reject is the efb220b / H2 failure).
	sawSelectRsp := false
	deadline := time.Now().Add(500 * time.Millisecond)

	for time.Now().Before(deadline) {
		frame, rerr := peerReadFrame(client, 100*time.Millisecond)
		if rerr != nil {
			break // read timeout — no more frames
		}

		switch frame[5] {
		case byte(hsms.SelectRspType):
			sawSelectRsp = true
		case byte(hsms.RejectReqType):
			t.Fatalf("passive spuriously Rejected pipelined data after Select.rsp (efb220b / H2): reason %d", frame[3])
		default:
			// other frame types ignored
		}
	}

	require.True(t, sawSelectRsp, "passive must answer the Select.req with a Select.rsp")
}

// TestPassive_CloseDuringAcceptConnectWindowIsBounded is the load-bearing deadlock teeth-test
// (T22b). A peer connects (so acceptLoop adopts it and calls TCPUp) but sends NO Select.req, so the
// FSM sits at NotSelected — the accept-connect window. A Close() issued in that window MUST return
// within a bounded timeout.
//
// With the OLD async TCPUp (inject(evTCPUp)) + the waitPassiveSelectable poll-fence, a back-to-back
// [evTCPUp, evClose] drain leaves State at NotConnected, the 500µs poller misses the µs NotSelected
// window, and the accept goroutine parks in the fence escaping only on rt.Done() — which closes only
// AFTER tr.Stop's g.accept.Wait joins that very goroutine: an unbounded Close deadlock. TCPUp's
// synchronous guarded CAS (CommitConnected) plus deleting the fence make that deadlock structurally
// impossible.
func TestPassive_CloseDuringAcceptConnectWindowIsBounded(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))

	// Dial a raw peer so acceptLoop adopts it and calls TCPUp, but send NO Select.req: the FSM
	// stays at NotSelected (the accept-connect window). No conn.Close in t.Cleanup: if the fix
	// regresses and Close deadlocks, a second Close would block forever on lifeMu at cleanup.
	client := dialPassive(t, port)
	defer func() { _ = client.Close() }()

	// Bounded wait for the accept goroutine to adopt the peer and advance the FSM to NotSelected
	// (synchronous via TCPUp's guarded CAS with the fix). Never a fixed Sleep-to-sync. A tight tick
	// gives the teeth-check its window: it detects NotSelected fast enough to fire Close before the
	// old 500µs fence poller could observe the transient NotSelected.
	require.Eventually(t, func() bool { return conn.State() == hsms.NotSelectedState },
		5*time.Second, 100*time.Microsecond,
		"passive must reach NotSelected after adopting the peer (TCPUp synchronous commit)")

	// Close during this window must return bounded — never hang on a poll-fence.
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	select {
	case err := <-done:
		require.NoError(t, err, "Close during the accept-connect window must succeed")
	case <-time.After(15 * time.Second):
		t.Fatal("Close hung during the passive accept-connect window (poll-fence deadlock)")
	}
}

// TestPassive_ReconnectsAfterDrop — after the established link drops, the passive reconnects through
// the FSM (§6.3): the recv loop's TCPDown drives NotConnected, the engine's reconnect loop calls
// tr.Start again, and startPassive RE-LISTENS on the same port so a fresh peer can select again.
func TestPassive_ReconnectsAfterDrop(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn, tr := newPassiveConnTr(t, port, hsms.WithT6(2*time.Second), hsms.WithT5(50*time.Millisecond))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// First generation: connect + select.
	client1 := dialPassive(t, port)

	_, err := client1.Write(selectReqFrame([4]byte{0x11, 0x11, 0x11, 0x11}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first generation must reach Selected")

	genN := tr.currentGeneration()

	// Drop the link: the passive recv loop sees EOF → TCPDown → NotConnected → the engine's
	// reconnect loop re-listens on the same port (through the FSM).
	require.NoError(t, client1.Close())

	require.Eventually(t, func() bool { return conn.State() == hsms.NotConnectedState },
		5*time.Second, 5*time.Millisecond, "the drop must drive the passive back to NotConnected")

	// Do not dial on NotConnectedState alone — see waitNextGeneration's doc for why that races the OLD listener's close.
	waitNextGeneration(t, tr, genN)

	// Second generation: a fresh peer dials the re-listened port and selects again.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	_, err = client2.Write(selectReqFrame([4]byte{0x22, 0x22, 0x22, 0x22}))
	require.NoError(t, err)
	expectSelectRsp(t, client2)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive must re-listen and re-select after a drop")
}
