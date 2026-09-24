package hsmsss

// integration_orphan_control_rsp_test.go exercises, end-to-end over a real TCP loopback pair, the scenario a hostile or buggy peer can trigger:
// a control response answering a pending DATA transaction.
// Our own Linktest and data traffic share one System Bytes generator,
// so a peer that (by bug or by design) answers a data primary with a Linktest.rsp carrying that primary's own System Bytes must not be allowed to complete the send.
// Per SEMI E37 §8.3.20 the correct answer to an orphan control RESPONSE is Reject.req(TransactionNotOpen=3), echoing the System Bytes,
// while the link stays up and the data primary's transaction remains open for its real reply.
//
// White-box (package hsmsss):
// reuses listenLoopback / newEndpoint / closeEndpoint / waitSelected / selectRspFrame / dataFrame / peerReadFrame / buildControlFrame from the existing harness and raw-frame builders.
// The scripted peer re-implements the select-req and reject-frame checks inline instead of reusing peerReadSelectReqHeader / assertRejectFrame,
// since it runs on its own goroutine and cannot take a *testing.T.

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

// TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes is the end-to-end pin for the fix:
// an in-flight W-bit SendDataMessage must not be completed by a Linktest.rsp carrying its System Bytes.
// The scripted peer sends that Linktest.rsp first,
// must receive back a Reject.req(TransactionNotOpen=3) echoing the same System Bytes,
// and only THEN sends the genuine S1F2 reply —
// which must be the one SendDataMessage actually returns, with the link still Selected throughout.
func TestReplyMatching_ControlResponseOnDataPrimary_RejectedThenGenuineReplyCompletes(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	peerReady := make(chan struct{})
	peerDone := make(chan struct{})

	go func() { peerErrCh <- runControlOrphanOnDataPrimaryPeer(ln, peerReady, peerDone) }()

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
		require.NoError(t, res.err, "a control response must not fail the send; the genuine reply must still complete it")
		require.NotNil(t, res.reply, "the send must complete with the GENUINE data reply, not the orphaned control response")
		require.Equal(t, uint8(1), res.reply.Stream())
		require.Equal(t, uint8(2), res.reply.Function())
		item, err := res.reply.Item()
		require.NoError(t, err)
		got, err := item.ToASCII()
		require.NoError(t, err)
		require.Equal(t, "pong", got)
	case err := <-peerErrCh:
		t.Fatalf("peer failed while SUT was sending: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("SendDataMessage did not complete after the genuine reply — the orphan control response likely stole or stalled the transaction")
	}

	require.Equal(t, hsms.SelectedState, sut.conn.State(),
		"connection must remain Selected throughout an orphan-control-response/Reject round trip")

	close(peerDone)

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "raw peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// runControlOrphanOnDataPrimaryPeer accepts one connection from the active SUT,
// completes the Select handshake (as responder),
// reads the SUT's W-bit data primary,
// answers it with a Linktest.rsp carrying the primary's own System Bytes (the orphan control response under test),
// reads back the Reject.req(TransactionNotOpen=3) the SUT must emit for it,
// and only then sends the genuine S1F2 reply
// so the SUT's send can complete for the right reason.
func runControlOrphanOnDataPrimaryPeer(ln net.Listener, ready chan<- struct{}, done <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

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

	// Read the SUT's W-bit S1F1 data primary.
	data, err := peerReadFrame(conn, 5*time.Second)
	if err != nil {
		return fmt.Errorf("read S1F1 primary: %w", err)
	}
	if len(data) < 10 || data[5] != byte(hsms.DataMsgType) {
		return fmt.Errorf("expected data message (SType=0), got SType=%d", data[5])
	}
	var sb [4]byte
	copy(sb[:], data[6:10])

	// The orphan control response under test: a Linktest.rsp carrying the data primary's own
	// System Bytes, with no matching open control transaction.
	if _, err := conn.Write(buildControlFrame(byte(hsms.LinktestRspType), 0, sb)); err != nil {
		return fmt.Errorf("write linktest.rsp: %w", err)
	}

	// The SUT must answer the orphan control response with Reject.req(TransactionNotOpen=3),
	// echoing the same System Bytes, WITHOUT completing the still-open data transaction.
	reject, err := peerReadFrame(conn, 5*time.Second)
	if err != nil {
		return fmt.Errorf("read reject.req: %w", err)
	}
	if len(reject) < 10 || reject[5] != byte(hsms.RejectReqType) {
		return fmt.Errorf("expected reject.req (SType=7), got SType=%d", reject[5])
	}
	if reject[3] != hsms.RejectTransactionNotOpen {
		return fmt.Errorf("expected reject reason %d (TransactionNotOpen), got %d", hsms.RejectTransactionNotOpen, reject[3])
	}
	var rejectSB [4]byte
	copy(rejectSB[:], reject[6:10])
	if rejectSB != sb {
		return fmt.Errorf("reject.req System Bytes mismatch: got %v, want %v", rejectSB, sb)
	}

	// Now send the genuine S1F2 reply, same System Bytes.
	body := secs2.A("pong").ToBytes()
	if _, err := conn.Write(dataFrame(0xFFFF, 1, 2, sb, body)); err != nil {
		return fmt.Errorf("write genuine S1F2 reply: %w", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	return nil
}
