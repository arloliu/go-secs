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
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// ── harness ─────────────────────────────────────────────────────────────────────

// freeLoopbackPort binds 127.0.0.1:0, records the OS-chosen port, and closes the listener so a
// passive connection can bind that concrete port. Go listeners set SO_REUSEADDR, so the rebind
// (here and across reconnect generations) succeeds despite the brief close.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener addr must be *net.TCPAddr")
	require.NoError(t, ln.Close())

	return addr.Port
}

// newPassiveConn builds a full passive-role HSMS-SS connection listening on 127.0.0.1:port,
// wiring a real hsmsss *transport into the shared hsms core. opts tune the shared timers.
func newPassiveConn(t *testing.T, port int, opts ...hsms.ConnOption) hsms.Connection {
	t.Helper()

	hopts := make([]Option, 0, len(opts)+1)
	hopts = append(hopts, WithPassive())

	for _, o := range opts {
		hopts = append(hopts, WithConnectionOption(o))
	}

	cfg, err := NewConfig("127.0.0.1", port, hopts...)
	require.NoError(t, err)

	conn, err := hsms.NewConnection(&cfg.ConnectionConfig, newTransport(cfg))
	require.NoError(t, err)

	return conn
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

// expectSelectRsp reads one frame from client and asserts it is a success Select.rsp — the passive
// side's H2 responder answer to a genuine (NotSelected->Selected) Select.req.
func expectSelectRsp(t *testing.T, client net.Conn) {
	t.Helper()

	expectSelectRspStatus(t, client, hsms.SelectStatusSuccess)
}

// expectSelectRspStatus reads one frame from client and asserts it is a Select.rsp carrying the
// given SelectStatus (0 = Communication Established; 1 = Communication Already Active for a
// duplicate Select while already Selected, E37 Table 7 / M5).
func expectSelectRspStatus(t *testing.T, client net.Conn, wantStatus byte) {
	t.Helper()

	frame, err := peerReadFrame(client, 5*time.Second)
	require.NoError(t, err, "client must receive the passive side's Select.rsp")
	require.GreaterOrEqual(t, len(frame), 10, "control frame must carry a 10-byte header")
	require.Equal(t, byte(hsms.SelectRspType), frame[5], "passive must answer a Select.req with a Select.rsp")
	require.Equal(t, wantStatus, frame[3], "passive Select.rsp must carry the expected SelectStatus")
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
// session is live. HSMS-SS is single-session (§3 / §6.3): the passive ACCEPTS-then-immediately-
// CLOSES the 2nd connection and does NOT tear down the live 1st session.
func TestPassive_RefusesSecondConnection(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// First peer: connect + select → the single live session.
	client1 := dialPassive(t, port)
	t.Cleanup(func() { _ = client1.Close() })

	_, err := client1.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first peer must reach Selected")

	// Second peer dials while the first session is live. It must be refused (closed) promptly.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	require.NoError(t, client2.SetReadDeadline(time.Now().Add(3*time.Second)))

	buf := make([]byte, 1)
	_, rerr := client2.Read(buf)
	require.Error(t, rerr, "the 2nd connection must be refused (closed) by the passive side (single-session)")

	// Teeth: a CLOSED connection reads EOF / reset; a connection merely left open (not refused)
	// reads a net timeout. Reject a timeout so removing the refuse-close makes this test fail.
	var netErr net.Error
	if errors.As(rerr, &netErr) {
		require.False(t, netErr.Timeout(),
			"the 2nd connection must be CLOSED (EOF/reset), not merely idle — a read timeout means it was not refused")
	}

	// The live 1st session must stay Selected AND usable: a duplicate Select.req while already
	// Selected is answered Select.rsp status 1 (Communication Already Active, E37 Table 7 / M5),
	// proving the link is intact and still serviced (the responder never Rejects a duplicate).
	require.Equal(t, hsms.SelectedState, conn.State(),
		"the live 1st session must stay Selected after refusing the 2nd connection")

	_, err = client1.Write(selectReqFrame([4]byte{0x05, 0x06, 0x07, 0x08}))
	require.NoError(t, err, "the 1st connection must still be writable after the 2nd was refused")
	expectSelectRspStatus(t, client1, hsms.SelectStatusAlreadyActive)

	require.Equal(t, hsms.SelectedState, conn.State(),
		"the 1st session stays Selected after the refused 2nd dial")
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
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung during the passive accept-connect window (poll-fence deadlock)")
	}
}

// TestPassive_ReconnectsAfterDrop — after the established link drops, the passive reconnects through
// the FSM (§6.3): the recv loop's TCPDown drives NotConnected, the engine's reconnect loop calls
// tr.Start again, and startPassive RE-LISTENS on the same port so a fresh peer can select again.
func TestPassive_ReconnectsAfterDrop(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	conn := newPassiveConn(t, port, hsms.WithT6(2*time.Second), hsms.WithT5(50*time.Millisecond))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	// First generation: connect + select.
	client1 := dialPassive(t, port)

	_, err := client1.Write(selectReqFrame([4]byte{0x11, 0x11, 0x11, 0x11}))
	require.NoError(t, err)
	expectSelectRsp(t, client1)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "first generation must reach Selected")

	// Drop the link: the passive recv loop sees EOF → TCPDown → NotConnected → the engine's
	// reconnect loop re-listens on the same port (through the FSM).
	require.NoError(t, client1.Close())

	require.Eventually(t, func() bool { return conn.State() == hsms.NotConnectedState },
		5*time.Second, 5*time.Millisecond, "the drop must drive the passive back to NotConnected")

	// Second generation: a fresh peer dials the re-listened port and selects again.
	client2 := dialPassive(t, port)
	t.Cleanup(func() { _ = client2.Close() })

	_, err = client2.Write(selectReqFrame([4]byte{0x22, 0x22, 0x22, 0x22}))
	require.NoError(t, err)
	expectSelectRsp(t, client2)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "passive must re-listen and re-select after a drop")
}
