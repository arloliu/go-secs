package hsmsss

// simultaneous_select_test.go — v2 port of the v1 tests/hsmsss_integration/simultaneous_select_test.go,
// covering the SEMI E37 §7.4 select-race edge cases with a RAW TCP peer (the precise message
// ordering cannot be produced with two hsms.Connection instances, since only the active side
// initiates Select). The v2 active is the initiator; a raw listener that the active dials scripts
// the race.
//
// E37 §7.4 / Table 7 behavior asserted here (see
// TestSimultaneousSelect_AlreadySelectedAcceptsSecondSelect): a 2nd inbound Select.req while
// already Selected is answered SelectStatusAlreadyActive (1, "Communication Already Active", M5) —
// matching v1 — while a TRUE simultaneous select (both sides transition NotSelected->Selected) is
// answered SelectStatusSuccess (0). handleSelectReq distinguishes them via the guarded CAS: CAS
// success → status 0; CAS fail (already Selected) → status 1. The link stays Selected either way.
//
// White-box (package hsmsss): reuses newEndpoint / closeEndpoint / waitSelected / freeLoopbackPort /
// listenLoopback / peerReadFrame / selectReqFrame / selectRspFrame / dataFrame, plus secs2.A.

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

// TestSimultaneousSelect_ActiveAcceptsRemoteSelect exercises SEMI E37 §7.4.3 simultaneous select:
// the active SUT sends its Select.req and, before answering it, the raw peer sends its OWN
// Select.req. Both must resolve — the active commits Selected via the RESPONDER path (handleSelectReq
// on the recv goroutine, §7.D) when it processes the peer's Select.req, and its own initiator
// Select.req completes when the peer answers it. The active must reach Selected and a subsequent
// data round-trip must work (the peer sends S1F1, the SUT's handler answers S1F2).
func TestSimultaneousSelect_ActiveAcceptsRemoteSelect(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	sutSelected := make(chan struct{})

	go func() { peerErrCh <- runSimultaneousSelectPeer(ln, sutSelected) }()

	// The SUT replies "pong" to primary (odd-function) data messages so the peer's S1F1 round-trip
	// completes; it does not read msg.Item(), so an empty-body S1F1 is fine.
	handler := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Function()%2 == 1 {
			_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
		}
	}

	sut := newEndpoint(t, port, true, nil, handler)
	defer closeEndpoint(t, sut)
	require.NoError(t, sut.conn.Open(context.Background(), hsms.OpenBackground))

	waitSelected(t, sut)
	close(sutSelected) // tell the peer the active reached Selected → it runs the S1F1 round-trip

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "raw peer should complete the simultaneous-select + data round-trip without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// runSimultaneousSelectPeer scripts the §7.4.3 race: read the active's Select.req, send our OWN
// Select.req before answering it, read the active's responder Select.rsp (which answers ours),
// then answer the active's Select.req. After the active reaches Selected (signaled via sutSelected)
// it runs an S1F1→S1F2 data round-trip to prove the link is functional.
func runSimultaneousSelectPeer(ln net.Listener, sutSelected <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Read the active's Select.req (keep a copy of its header for the answering Select.rsp).
	activeReq, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read active select.req: %w", err)
	}
	if len(activeReq) < 10 || activeReq[5] != byte(hsms.SelectReqType) {
		return fmt.Errorf("expected select.req, got SType=%d", activeReq[5])
	}
	activeReqHdr := append([]byte(nil), activeReq[:10]...)

	// Send OUR OWN Select.req before answering the active's (this creates the simultaneous select).
	peerSB := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	if _, err := conn.Write(selectReqFrame(peerSB)); err != nil {
		return fmt.Errorf("write peer select.req: %w", err)
	}

	// Read the active's RESPONDER Select.rsp answering OUR Select.req (success, echoing peerSB).
	rsp, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read select.rsp for peer: %w", err)
	}
	if len(rsp) < 10 || rsp[5] != byte(hsms.SelectRspType) {
		return fmt.Errorf("expected select.rsp, got SType=%d", rsp[5])
	}
	if [4]byte(rsp[6:10]) != peerSB {
		return fmt.Errorf("peer select.rsp System Bytes mismatch")
	}
	if rsp[3] != byte(hsms.SelectStatusSuccess) {
		return fmt.Errorf("expected peer select success, got status %d", rsp[3])
	}

	// Answer the active's OWN initiator Select.req so its Select procedure completes.
	if _, err := conn.Write(selectRspFrame(activeReqHdr, hsms.SelectStatusSuccess)); err != nil {
		return fmt.Errorf("write select.rsp for active: %w", err)
	}

	// Wait for the active to reach Selected before exercising the data round-trip.
	select {
	case <-sutSelected:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for active SELECTED state")
	}

	// S1F1 → S1F2 round-trip to prove the link is functional.
	if _, err := conn.Write(dataFrame(0xFFFF, 1, 1, [4]byte{0x00, 0x00, 0x00, 0x10}, nil)); err != nil {
		return fmt.Errorf("write S1F1: %w", err)
	}
	reply, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read S1F2 reply: %w", err)
	}
	if len(reply) < 10 || reply[5] != byte(hsms.DataMsgType) || reply[3] != 2 {
		return fmt.Errorf("expected S1F2, got SType=%d func=%d", reply[5], reply[3])
	}

	return nil
}

// TestSimultaneousSelect_AlreadySelectedAcceptsSecondSelect exercises SEMI E37 §7.4.2 / Table 7:
// once the link is Selected, a 2nd inbound Select.req arrives. v2 answers it SelectStatusAlreadyActive
// (1, "Communication Already Active", M5) and the link STAYS Selected (the responder never Rejects
// a duplicate Select).
func TestSimultaneousSelect_AlreadySelectedAcceptsSecondSelect(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	secondSelectDone := make(chan struct{})
	peerDone := make(chan struct{})

	go func() { peerErrCh <- runAlreadySelectedPeer(ln, secondSelectDone, peerDone) }()

	sut := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, sut)
	require.NoError(t, sut.conn.Open(context.Background(), hsms.OpenBackground))

	waitSelected(t, sut)

	// Wait for the peer to complete the 2nd-select exchange (it asserts the success status itself).
	select {
	case <-secondSelectDone:
	case err := <-peerErrCh:
		t.Fatalf("peer failed during the 2nd select: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the 2nd-select exchange")
	}

	// The link must stay Selected after the 2nd Select.req is answered (peer still holds the conn).
	require.Equal(t, hsms.SelectedState, sut.conn.State(),
		"link must stay Selected after answering a 2nd Select.req status 1 (already active, E37 Table 7)")

	close(peerDone)

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "raw peer should complete without error")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// runAlreadySelectedPeer completes the active's initial Select, then sends a 2nd Select.req while
// the active is already Selected and asserts the answer is Select.rsp status 1 (Communication
// Already Active, E37 Table 7 / M5).
// It keeps the connection open until done is closed so the test can assert the link stays Selected.
func runAlreadySelectedPeer(ln net.Listener, secondDone chan<- struct{}, done <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Complete the active's initial Select (as responder to its initiator Select.req).
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

	// Send a 2nd Select.req while the active is already Selected. TCP ordering guarantees the
	// active processes the first Select.rsp (committing Selected) before this frame.
	secondSB := [4]byte{0xDD, 0xEE, 0xFF, 0x00}
	if _, err := conn.Write(selectReqFrame(secondSB)); err != nil {
		return fmt.Errorf("write 2nd select.req: %w", err)
	}
	rsp, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read 2nd select.rsp: %w", err)
	}
	if len(rsp) < 10 || rsp[5] != byte(hsms.SelectRspType) {
		return fmt.Errorf("expected select.rsp, got SType=%d", rsp[5])
	}
	if [4]byte(rsp[6:10]) != secondSB {
		return fmt.Errorf("2nd select.rsp System Bytes mismatch")
	}
	// E37 Table 7 / M5: a 2nd Select.req while already Selected is answered SelectStatusAlreadyActive (1,
	// "Communication Already Active") — matching v1. The link stays Selected regardless.
	if rsp[3] != byte(hsms.SelectStatusAlreadyActive) {
		return fmt.Errorf("expected SelectStatusAlreadyActive (1) for a duplicate select while already Selected, got %d", rsp[3])
	}

	close(secondDone)

	select {
	case <-done:
	case <-time.After(3 * time.Second): // safety valve so the goroutine never leaks
	}

	return nil
}
