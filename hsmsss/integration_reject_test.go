package hsmsss

// reject_test.go — v2 port of the v1 tests/hsmsss_integration/reject_test.go, restricted to the
// orphan-control §8.3.20 paths this task owns: Reject reason 4 (data-in-not-selected), the orphan
// control-response path (v1 reason 3), and the HEADLINE inbound-Reject-poisons-in-flight-send case.
//
// The PType/SType→Reject reasons 1/2 (byte-chaos) are covered by the byte-fault suite
// (byte_chaos_scenarios_test.go) and are intentionally NOT re-covered here.
//
// Two behaviors are asserted precisely (see the docs on
// TestReject_OrphanControlResponse_TransactionNotOpen and TestReject_InboundReject_PoisonsInFlightSend):
//
//   - v2 answers an orphan control RESPONSE (even-SType rsp with no open transaction) with
//     Reject.req(RejectTransactionNotOpen=3) per E37 §8.3.20, echoing the System Bytes and keeping the
//     link. (An inbound Reject.req, SType 7, is NOT re-rejected — it is dropped.)
//   - v2 surfaces an inbound Reject.req correlated to a blocked SendDataMessage as a typed
//     *hsms.RejectError (Reason = the E37 reject reason code): RouteReply detects the Reject.req
//     (SType 7) and delivers replyResult{err: &RejectError{Reason}} to the sender's reply channel,
//     so session.SendDataMessage returns (nil, *hsms.RejectError). The sender UNBLOCKS PROMPTLY and
//     the link STAYS Selected. (v1 returned hsms.ErrRejectNotSelected; v2 uses a reason-carrying
//     RejectError inspected via errors.As.)
//
// White-box (package hsmsss): reuses newEndpoint / closeEndpoint / waitSelected / drainStateCh /
// freeLoopbackPort / dialPassive / expectSelectRsp / peerReadFrame / selectReqFrame / dataFrame /
// listenLoopback / assertNoRejectWithin, plus the raw builders in raw_control_frames_test.go.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// echoReplyHandler pushes every inbound data message onto recvCh and replies "pong" to primary
// (odd-function) messages. It is the v2 port of the v1 reject-test handler (Session→SECS2Endpoint,
// FunctionCode→Function). It does not read msg.Item(), so it works for empty-body primaries.
func echoReplyHandler(recvCh chan<- *hsms.DataMessage) hsms.DataMessageHandler {
	return func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		select {
		case recvCh <- msg:
		default:
		}

		if msg.Function()%2 == 1 {
			_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
		}
	}
}

// assertRejectFrame reads one HSMS frame from conn and asserts it is a Reject.req (SType 7) with
// the expected reason code (header[3]) and System Bytes echo (header[6:10]).
func assertRejectFrame(t *testing.T, conn net.Conn, wantReason byte, wantSysBytes [4]byte) {
	t.Helper()

	payload, err := peerReadFrame(conn, 5*time.Second)
	require.NoError(t, err, "reading Reject.req frame")
	require.GreaterOrEqual(t, len(payload), 10, "Reject.req frame too short")
	require.Equal(t, byte(hsms.RejectReqType), payload[5], "expected SType=7 (Reject.req)")
	require.Equal(t, wantReason, payload[3], "Reject.req reason code mismatch")
	require.Equal(t, wantSysBytes, [4]byte(payload[6:10]), "Reject.req System Bytes mismatch")
}

// rawSelectHandshake dials the passive endpoint at port, sends a Select.req with sb, and reads a
// success Select.rsp. It returns the connected raw client (caller closes it).
func rawSelectHandshake(t *testing.T, port int, sb [4]byte) *net.TCPConn {
	t.Helper()

	raw := dialPassive(t, port)
	_, err := raw.Write(selectReqFrame(sb))
	require.NoError(t, err, "raw client must write Select.req")
	expectSelectRsp(t, raw)

	return raw
}

// assertValidS1F1RoundTrip sends a valid S1F1 (SType 0, function 1) from the raw client, asserts
// the endpoint delivered it to the handler (recvCh) and answered S1F2 on the wire. It proves the
// receiver loop was NOT poisoned by a preceding Reject round-trip.
func assertValidS1F1RoundTrip(t *testing.T, raw net.Conn, recvCh <-chan *hsms.DataMessage, sb [4]byte) {
	t.Helper()

	_, err := raw.Write(dataFrame(0xFFFF, 1, 1, sb, nil))
	require.NoError(t, err, "raw client must write a valid S1F1")

	select {
	case got := <-recvCh:
		require.Equal(t, uint8(1), got.Stream(), "delivered stream mismatch")
		require.Equal(t, uint8(1), got.Function(), "delivered function mismatch")
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint did not deliver the valid S1F1 after the Reject round-trip (receiver poisoned)")
	}

	reply, err := peerReadFrame(raw, 5*time.Second)
	require.NoError(t, err, "reading S1F2 reply")
	require.GreaterOrEqual(t, len(reply), 10, "S1F2 reply frame too short")
	require.Equal(t, byte(0), reply[5], "expected SType=0 (data message) for the reply")
	require.Equal(t, byte(2), reply[3], "expected function=2 (S1F2)")
}

// ---------------------------------------------------------------------------
// TestReject_DataMessageNotSelected (reason 4, E37 §7.10.3 / §8.3.20)
//
// A raw client connects to a passive endpoint, SKIPS the Select handshake, and sends an S1F1 data
// frame. The endpoint must answer Reject.req(RejectNotSelected=4) echoing the offending System
// Bytes and KEEP the link. The client then completes a proper Select and sends a valid S1F1,
// proving the receiver loop was not poisoned by the Reject.
// ---------------------------------------------------------------------------
func TestReject_DataMessageNotSelected(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	recvCh := make(chan *hsms.DataMessage, 4)

	passive := newEndpoint(t, port, false, nil, echoReplyHandler(recvCh))
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))

	// Dial but do NOT select: a peer that connected sits at NotSelected (TCPUp fired).
	raw := dialPassive(t, port)
	defer func() { _ = raw.Close() }()

	// Send S1F1 WITHOUT a preceding Select.req → data in non-selected state → Reject(4).
	sb := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	_, err := raw.Write(dataFrame(0xFFFF, 1, 1, sb, nil))
	require.NoError(t, err)
	assertRejectFrame(t, raw, hsms.RejectNotSelected, sb)

	// Complete a proper Select so the endpoint reaches Selected.
	_, err = raw.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, raw)
	waitSelected(t, passive)

	// A valid S1F1 must now be delivered and answered — the receiver loop survived the Reject.
	assertValidS1F1RoundTrip(t, raw, recvCh, [4]byte{0x00, 0x00, 0x00, 0x02})
}

// ---------------------------------------------------------------------------
// TestReject_OrphanControlResponse_TransactionNotOpen (reason 3 — E37 §8.3.20)
//
// An unsolicited control RESPONSE (a Select.rsp with no open transaction) misses the sender-owned
// reply registry. E37 §8.3.20 REQUIRES the receiver to answer such an orphan even-SType response
// with Reject.req(RejectTransactionNotOpen=3) — "[Reject.req] must be used when an entity receives a
// control message which is a response (even numbered SType) for which there was no corresponding
// open transaction" — while keeping the link up (a Reject never tears down). This test asserts the
// endpoint emits Reject.req reason 3 echoing the orphan System Bytes, and that the link survives (a
// subsequent valid S1F1 is delivered and answered). An inbound Reject.req miss is NOT re-rejected
// (covered by the headline InboundReject test below).
// ---------------------------------------------------------------------------
func TestReject_OrphanControlResponse_TransactionNotOpen(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	recvCh := make(chan *hsms.DataMessage, 4)

	passive := newEndpoint(t, port, false, nil, echoReplyHandler(recvCh))
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))

	raw := rawSelectHandshake(t, port, [4]byte{0x11, 0x22, 0x33, 0x44})
	defer func() { _ = raw.Close() }()
	waitSelected(t, passive)

	// Send an UNSOLICITED Select.rsp (a control response with no open transaction).
	orphanSB := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	_, err := raw.Write(buildControlFrame(byte(hsms.SelectRspType), hsms.SelectStatusSuccess, orphanSB))
	require.NoError(t, err)

	// E37 §8.3.20: the endpoint answers the orphan control response with
	// Reject.req(TransactionNotOpen=3), echoing the orphan System Bytes.
	assertRejectFrame(t, raw, hsms.RejectTransactionNotOpen, orphanSB)

	// The link survives: a subsequent valid S1F1 is delivered and answered.
	assertValidS1F1RoundTrip(t, raw, recvCh, [4]byte{0x00, 0x00, 0x00, 0x03})
}

// ---------------------------------------------------------------------------
// TestReject_InboundReject_PoisonsInFlightSend — THE HEADLINE (orphan control §8.3.20 / T19).
//
// The active SUT dials a raw peer, completes Select, then calls SendDataMessage(S1F1, W-bit). The
// peer replies with a Reject.req(RejectNotSelected=4) correlated by System Bytes to that in-flight
// send instead of an S1F2.
//
// v2 CONTRACT (Fix A): an inbound Reject.req (SType 7) is routed through dispatchFrame →
// RouteReply, which detects the Reject and delivers replyResult{err: &hsms.RejectError{Reason}}
// to the sender's reply channel. sendWaitReply returns (nil, that error); session.SendDataMessage
// propagates it, so the blocked send returns (nil, *hsms.RejectError) with Reason == the E37
// reject reason code (4 = RejectNotSelected here), inspected via errors.As.
//
// Required invariants: the sender UNBLOCKS PROMPTLY (well under T3, not a hang and not a T3-wait)
// and the connection STAYS Selected (a Reject to one transaction is not a link failure). A hang or
// a teardown would be a REAL v2 bug.
// ---------------------------------------------------------------------------
func TestReject_InboundReject_PoisonsInFlightSend(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	peerReady := make(chan struct{})
	peerDone := make(chan struct{})

	go func() { peerErrCh <- runInboundRejectPeer(ln, peerReady, peerDone) }()

	sut := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, sut)
	require.NoError(t, sut.conn.Open(context.Background(), hsms.OpenBackground))

	waitSelected(t, sut)

	select {
	case <-peerReady:
	case err := <-peerErrCh:
		t.Fatalf("peer failed during select: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to become ready")
	}

	drainStateCh(sut.states)

	// SendDataMessage blocks awaiting the reply; the peer answers Reject.req(4). Run it in a
	// goroutine so we can bound the unblock time (a hang or T3-wait fails the 2 s guard, < T3=3s).
	type sendResult struct {
		reply *hsms.DataMessage
		err   error
	}
	resCh := make(chan sendResult, 1)

	go func() {
		reply, err := sut.conn.SendDataMessage(context.Background(), 1, 1, true, secs2.A("hello"))
		resCh <- sendResult{reply: reply, err: err}
	}()

	select {
	case res := <-resCh:
		// Fix A: v2 surfaces the inbound Reject as a typed *hsms.RejectError carrying the E37
		// reason code (4 = RejectNotSelected), with a nil reply.
		var re *hsms.RejectError
		require.ErrorAs(t, res.err, &re,
			"v2 surfaces an inbound Reject.req as a *hsms.RejectError (correlated to the in-flight send)")
		require.Equal(t, byte(hsms.RejectNotSelected), re.Reason,
			"the RejectError must carry the E37 reject reason code the peer sent (4 = RejectNotSelected)")
		require.Nil(t, res.reply,
			"v2 returns a nil reply for an inbound Reject (the transaction was rejected, not answered)")
	case err := <-peerErrCh:
		t.Fatalf("peer failed while SUT was sending: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("SendDataMessage did NOT unblock within 2 s (< T3=3s) after an inbound Reject.req — " +
			"the sender hung / waited out T3 (REAL v2 BUG: inbound Reject not correlated)")
	}

	// The link must STAY Selected — a Reject to one transaction is not a link failure.
	require.Equal(t, hsms.SelectedState, sut.conn.State(),
		"connection must remain Selected after an inbound Reject.req")

	// And no NotConnected transition may arrive shortly after (receiver loop not poisoned).
	select {
	case s := <-sut.states:
		require.NotEqual(t, hsms.NotConnectedState, s,
			"connection dropped after an inbound Reject.req — receiver loop poisoned (REAL v2 BUG)")
	case <-time.After(300 * time.Millisecond):
		// No NotConnected event — the link is healthy.
	}

	close(peerDone)

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "raw peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// runInboundRejectPeer accepts one connection from the active SUT, completes the Select handshake
// (as responder), reads one data message, and replies with Reject.req(4) echoing the data
// message's System Bytes so it correlates to the SUT's in-flight send. It keeps the TCP connection
// OPEN until done is closed so the SUT's receiver loop does not see a premature EOF and tear down
// for the wrong reason.
func runInboundRejectPeer(ln net.Listener, ready chan<- struct{}, done <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Read the SUT's Select.req and answer Select.rsp(success), echoing its System Bytes.
	req, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}
	if len(req) < 10 || req[5] != byte(hsms.SelectReqType) {
		return fmt.Errorf("expected select.req, got SType=%d", req[5])
	}
	if _, err := conn.Write(selectRspFrame(req[:10], hsms.SelectStatusSuccess)); err != nil {
		return fmt.Errorf("write select.rsp: %w", err)
	}

	close(ready)

	// Read the SUT's S1F1 data message.
	data, err := peerReadFrame(conn, 5*time.Second)
	if err != nil {
		return fmt.Errorf("read S1F1: %w", err)
	}
	if len(data) < 10 || data[5] != byte(hsms.DataMsgType) {
		return fmt.Errorf("expected data message (SType=0), got SType=%d", data[5])
	}

	// Reply Reject.req(4) echoing the data message's System Bytes and SType (0 for data).
	var dataSB [4]byte
	copy(dataSB[:], data[6:10])
	if _, err := conn.Write(buildRejectFrame(data[5], hsms.RejectNotSelected, dataSB)); err != nil {
		return fmt.Errorf("write Reject.req(4): %w", err)
	}

	// Keep the connection open until the test finishes asserting.
	select {
	case <-done:
	case <-time.After(3 * time.Second): // safety valve so the goroutine never leaks
	}

	return nil
}
