package hsmsss

// active_test.go — full-connection loopback tests for the active-role Select procedure and the
// H2 synchronous-commit responder (spec §6.3 / §7.D). Each test drives a REAL connection
// (hms.NewConnection + Open, real supervisor / FSM / recv loop / Select procedure) against a
// scripted peer over 127.0.0.1 loopback, under -race.
//
// The file is in package hsmsss (not hsmsss_test) so it can construct the unexported *transport
// and reuse the loopback / frame helpers from reader_test.go and transport_test.go.

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// ── harness ─────────────────────────────────────────────────────────────────────

// newActiveConn builds a full active-role HSMS-SS connection targeting 127.0.0.1:port, wiring a
// real hsmsss *transport into the shared hsms core. opts tune the shared timers.
func newActiveConn(t *testing.T, port int, opts ...hsms.ConnOption) hsms.Connection {
	t.Helper()

	hopts := make([]Option, 0, len(opts))
	for _, o := range opts {
		hopts = append(hopts, WithConnectionOption(o))
	}

	cfg, err := NewConfig("127.0.0.1", port, hopts...)
	require.NoError(t, err)

	conn, err := hsms.NewConnection(&cfg.ConnectionConfig, newTransport(cfg))
	require.NoError(t, err)

	return conn
}

// waitPeer waits up to 5 s for the scripted peer goroutine to accept the active dial.
func waitPeer(t *testing.T, peerCh chan *net.TCPConn) *net.TCPConn {
	t.Helper()

	select {
	case peer := <-peerCh:
		return peer
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept the active connection in time")

		return nil
	}
}

// peerReadFrame reads one whole HSMS frame (4-byte big-endian length prefix + payload) from
// conn, bounded by timeout. It returns the payload (10-byte header + optional body).
func peerReadFrame(conn net.Conn, timeout time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}

	payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}

	return payload, nil
}

// peerReadSelectReqHeader reads our active-side Select.req from conn (bounded by a fixed 5 s
// deadline), asserts it is a Select.req, and returns the 10-byte header (from which the caller
// echoes System Bytes into a matching Select.rsp).
func peerReadSelectReqHeader(t *testing.T, conn net.Conn) []byte {
	t.Helper()

	payload, err := peerReadFrame(conn, 5*time.Second)
	require.NoError(t, err, "peer must read our Select.req")
	require.GreaterOrEqual(t, len(payload), 10, "control frame must carry a 10-byte header")
	require.Equal(t, byte(hsms.SelectReqType), payload[5], "active side must send a Select.req first")

	return payload[:10]
}

// selectReqFrame builds a peer-originated Select.req frame with the given System Bytes. HSMS-SS
// control frames always carry SessionID 0xFFFF (E37.1 single-session, §3), so the session ID is
// fixed rather than a parameter.
func selectReqFrame(sb [4]byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF)
	h[5] = byte(hsms.SelectReqType)
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}

// selectRspFrame builds a Select.rsp frame answering the Select.req whose 10-byte header is
// reqHdr: it echoes the session ID and System Bytes and carries the given select-status.
func selectRspFrame(reqHdr []byte, status byte) []byte {
	h := make([]byte, 10)
	h[0] = reqHdr[0]
	h[1] = reqHdr[1]
	h[3] = status
	h[5] = byte(hsms.SelectRspType)
	copy(h[6:10], reqHdr[6:10])

	return frameBytes(10, h, nil)
}

// dataFrame builds a raw HSMS data-message frame (SType 0, no W-bit) with the given
// stream/function/System-Bytes and body. An empty body yields a 10-byte (header-only) frame.
//
//nolint:unparam // sessionID is always 0xFFFF across the HSMS-SS single-session tests; kept for readability.
func dataFrame(sessionID uint16, stream, function byte, sb [4]byte, body []byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], sessionID)
	h[2] = stream & 0x7F // strip the W-bit — no reply expected
	h[3] = function
	h[5] = 0 // SType 0 = data
	copy(h[6:10], sb[:])

	return frameBytes(uint32(10+len(body)), h, body)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestActive_SelectsAgainstScriptedPeer — the active side dials, sends Select.req, and reaches
// Selected when the scripted peer answers Select.rsp with select-status 0 (spec §6.3).
func TestActive_SelectsAgainstScriptedPeer(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	conn := newActiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	reqHdr := peerReadSelectReqHeader(t, peer)
	_, err := peer.Write(selectRspFrame(reqHdr, hsms.SelectStatusSuccess))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "active side must reach Selected after Select.rsp(success)")
}

// TestActive_H2_NoSpuriousRejectOnPipelinedData — the H2 crux on the INITIATOR side (efb220b
// regression). The scripted peer sends Select.rsp AND a data message back-to-back in one write,
// so the data lands in the same kernel buffer immediately after Select.rsp. The active side must
// commit Selected synchronously when the recv loop routes the Select.rsp — BEFORE it dispatches
// the pipelined data — so the data is NOT spuriously Rejected(NotSelected).
func TestActive_H2_NoSpuriousRejectOnPipelinedData(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	conn := newActiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	reqHdr := peerReadSelectReqHeader(t, peer)

	rsp := selectRspFrame(reqHdr, hsms.SelectStatusSuccess)
	data := dataFrame(0xFFFF, 1, 1, [4]byte{0x11, 0x22, 0x33, 0x44}, nil)
	_, err := peer.Write(append(rsp, data...)) // Select.rsp THEN data, back-to-back
	require.NoError(t, err)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "active side must reach Selected")

	// The peer must NOT receive a Reject.req (SType 7) for the pipelined data. A bounded read
	// that times out (no frame) is the pass; any Reject.req is the efb220b failure.
	assertNoRejectWithin(t, peer, 400*time.Millisecond)
}

// TestActive_SelectFailureDisconnects — a Select.req answered with a NON-ZERO (failure)
// select-status must NOT leave the link Selected: the active procedure drives TCPDown →
// NotConnected and the engine reconnects (spec §6.3).
func TestActive_SelectFailureDisconnects(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	conn := newActiveConn(t, port, hsms.WithT6(2*time.Second), hsms.WithT5(50*time.Millisecond))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	reqHdr := peerReadSelectReqHeader(t, peer)
	// A GENUINE rejection status (2 = Connection Not Ready). Status 1 (Communication Already Active)
	// is NOT a failure — E37 Table 7 / M5: it means the link is already established, so the active
	// procedure must NOT tear down on it (see runSelectProcedure).
	_, err := peer.Write(selectRspFrame(reqHdr, hsms.SelectStatusNotReady))
	require.NoError(t, err)

	// Close the listener so reconnect re-dials fail fast — the link stays NotConnected and never
	// flips to Selected.
	_ = ln.Close()

	require.Eventually(t, func() bool { return conn.State() == hsms.NotConnectedState },
		5*time.Second, 5*time.Millisecond, "a failure Select.rsp must drive the link to NotConnected")
	require.NotEqual(t, hsms.SelectedState, conn.State(), "a failure Select.rsp must never reach Selected")
}

// TestActive_H2_Responder_NoSpuriousRejectOnPipelinedData — the H2 crux on the RESPONDER side
// (the spec-mandated teeth scenario, §7.D). In simultaneous select (§7.4.3) the scripted peer
// sends ITS OWN Select.req and then a data message back-to-back. Our responder must commit
// Selected synchronously BEFORE writing its Select.rsp, so the pipelined data finds
// IsSelected()==true and is NOT spuriously Rejected. The peer also completes OUR Select.req so
// the active procedure does not T6-timeout mid-test.
func TestActive_H2_Responder_NoSpuriousRejectOnPipelinedData(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })
	peerCh := acceptOneAsync(ln)

	conn := newActiveConn(t, port, hsms.WithT6(2*time.Second))
	require.NoError(t, conn.Open(context.Background(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := waitPeer(t, peerCh)
	t.Cleanup(func() { _ = peer.Close() })

	// Read OUR Select.req (active initiator) so we can complete it after the responder exchange.
	ourReqHdr := peerReadSelectReqHeader(t, peer)

	// Simultaneous select: the peer's OWN Select.req, then a pipelined data frame, back-to-back.
	peerReq := selectReqFrame([4]byte{0xAB, 0xCD, 0xEF, 0x01})
	data := dataFrame(0xFFFF, 1, 1, [4]byte{0x55, 0x66, 0x77, 0x88}, nil)
	_, err := peer.Write(append(peerReq, data...))
	require.NoError(t, err)

	// Complete OUR outstanding Select.req so the active procedure's WriteMessage returns success.
	_, err = peer.Write(selectRspFrame(ourReqHdr, hsms.SelectStatusSuccess))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond, "responder commit must reach Selected")

	// The peer must receive OUR Select.rsp (answering its Select.req) and MUST NOT receive a
	// Reject.req for the pipelined data.
	sawSelectRsp := false
	deadline := time.Now().Add(500 * time.Millisecond)

	for time.Now().Before(deadline) {
		frame, rerr := peerReadFrame(peer, 100*time.Millisecond)
		if rerr != nil {
			break // read timeout — no more frames
		}

		switch frame[5] {
		case byte(hsms.SelectRspType):
			sawSelectRsp = true
		case byte(hsms.RejectReqType):
			t.Fatalf("responder pipelined data was spuriously Rejected (efb220b / H2): reason %d", frame[3])
		default:
			// other frame types ignored
		}
	}

	require.True(t, sawSelectRsp, "responder must answer the peer's Select.req with a Select.rsp")
}

// assertNoRejectWithin reads from conn for up to timeout and fails if a Reject.req (SType 7)
// arrives. A read timeout (no frame) is the expected pass. Any non-Reject frame is tolerated.
func assertNoRejectWithin(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := peerReadFrame(conn, time.Until(deadline))
		if err != nil {
			return // timeout / EOF — no Reject was sent (pass)
		}

		if len(frame) >= 10 && frame[5] == byte(hsms.RejectReqType) {
			t.Fatalf("pipelined data after Select.rsp was spuriously Rejected (efb220b / H2): reason %d", frame[3])
		}
	}
}
