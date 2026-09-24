package hsmsss

// integration_select_status1_test.go — the legitimate-simultaneous-select e2e over a raw TCP peer
// (companion to integration_simultaneous_select_test.go): a status-1 Select.rsp (Communication
// Already Active) answering OUR Select.req is a success, not a refusal, when this generation
// already selected via the RESPONDER path first.
//
// White-box (package hsmsss): reuses newEndpoint / closeEndpoint / listenLoopback /
// peerReadFrame / selectReqFrame / selectRspFrame / dataFrame, plus secs2.A.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestSimultaneousSelect_Status1AfterResponderCommit_NeverDisconnects is the raw-peer e2e for the legitimate simultaneous-select race:
// the peer sends its OWN Select.req first —
// our responder commits Selected and answers it status 0 —
// and only THEN answers OUR outstanding Select.req with a correlated status 1.
// This generation's selectedOnce latch is already true by the time that status-1 answer arrives,
// so it must be accepted as the legitimate simultaneous-select race it is:
// no teardown, and the link must still carry a data round-trip afterward.
//
// Teeth: an implementation that rejects every status 1 unconditionally (regardless of the latch)
// races two events on the recv goroutine's own timeline:
// the responder commit that briefly reaches Selected,
// and the almost-immediate teardown the unconditional rejection then drives.
// That window is normally far too small for the polling wait below to observe Selected at all,
// so the failure this test catches surfaces as a waitSelected timeout,
// not as the require.Never bite below — see the fail-fast check inlined into that wait.
func TestSimultaneousSelect_Status1AfterResponderCommit_NeverDisconnects(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	sutSelected := make(chan struct{})
	roundTripDone := make(chan struct{})
	keepPeerOpen := make(chan struct{}) // held open past the round-trip so a LATER teardown (the bug) is still observable

	// releasePeer and joinPeer are idempotent (sync.Once):
	// require.Never below can FailNow (Goexit) partway through the test, skipping every later plain statement —
	// including the unconditional close(keepPeerOpen) and peerErrCh receive that used to sit at the end of this function.
	// Without an unconditional release the peer goroutine leaks forever, blocked on <-keepOpen with its own socket still open.
	// t.Cleanup guarantees both run on every exit path;
	// the success path below also calls them explicitly
	// so a genuine peer failure is still reported under its own message rather than the cleanup's generic one.
	var releaseOnce, joinOnce sync.Once
	var joinErr error
	releasePeer := func() { releaseOnce.Do(func() { close(keepPeerOpen) }) }
	joinPeer := func() error {
		joinOnce.Do(func() {
			select {
			case joinErr = <-peerErrCh:
			case <-time.After(10 * time.Second):
				joinErr = fmt.Errorf("timeout waiting for raw peer to finish")
			}
		})

		return joinErr
	}
	t.Cleanup(func() {
		releasePeer()
		if err := joinPeer(); err != nil {
			t.Errorf("raw peer goroutine did not exit cleanly: %v", err)
		}
	})

	go func() {
		peerErrCh <- runStatus1AfterResponderCommitPeer(ln, sutSelected, roundTripDone, keepPeerOpen)
	}()

	// The SUT replies "pong" to primary (odd-function) data messages so the peer's S1F1 round-trip
	// completes; it does not read msg.Item(), so an empty-body S1F1 is fine.
	handler := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		if msg.Function()%2 == 1 {
			_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
		}
	}

	sut := newEndpoint(t, port, true, nil, handler)
	defer closeEndpoint(t, sut)

	// Subscribe to lifecycle BEFORE Open, so a NotConnected transition can never be missed.
	var mu sync.Mutex
	var sawNotConnected bool
	cancel := sut.conn.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		if ev.Current != hsms.NotConnectedState {
			return
		}
		mu.Lock()
		sawNotConnected = true
		mu.Unlock()
	})
	defer cancel()

	require.NoError(t, sut.conn.Open(context.Background(), hsms.OpenBackground))

	// A plain waitSelected here would just time out after its full bound with no clue why an unconditional-reject implementation fails (see the Teeth comment above).
	// The responder commit and the almost-immediate status-1 teardown race so tightly
	// that polling can miss the intervening Selected state entirely.
	// Fail fast instead once a NotConnected event lands first.
	waitSelectedFailFast := func() {
		deadline := time.After(10 * time.Second)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if sut.conn.State() == hsms.SelectedState {
					return
				}
				mu.Lock()
				saw := sawNotConnected
				mu.Unlock()
				if saw {
					t.Fatal("observed a NotConnected transition before Selected — an implementation " +
						"that rejects status 1 unconditionally can tear the link down so quickly " +
						"after the responder commit that polling never even catches Selected")
				}
			case <-deadline:
				t.Fatal("timeout waiting for Selected")
			}
		}
	}
	waitSelectedFailFast()
	close(sutSelected) // tell the peer the active reached Selected -> it runs the S1F1 round-trip

	select {
	case <-roundTripDone:
	case err := <-peerErrCh:
		t.Fatalf("raw peer failed before completing the round-trip: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the raw peer's data round-trip to finish")
	}

	// The peer keeps the socket open past the round-trip (below), so a DELAYED disconnect — one
	// that a buggy implementation might fire only after the round-trip settles — is still
	// observable here rather than being masked by the peer's own close.
	require.Never(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return sawNotConnected
	}, 500*time.Millisecond, 20*time.Millisecond,
		"a legitimate status-1 Select.rsp (this generation already selected via our responder commit) must never disconnect")

	releasePeer()
	require.NoError(t, joinPeer(), "raw peer should complete the status-1-after-commit exchange without error")
}

// runStatus1AfterResponderCommitPeer scripts the race: read the active's initiator Select.req
// (keeping its header to answer later), send OUR OWN Select.req first and require the responder to
// answer it status 0 (the commit that latches this generation's selectedOnce), and ONLY THEN answer
// the active's outstanding Select.req with a correlated status 1. After the active reaches Selected
// (signaled via sutSelected) it runs an S1F1 -> S1F2 data round-trip to prove the link still works,
// closes roundTripDone, and then holds the socket open until keepOpen closes — so the caller's
// post-round-trip disconnect check runs against a connection this peer has not itself torn down.
func runStatus1AfterResponderCommitPeer(ln net.Listener, sutSelected <-chan struct{}, roundTripDone chan<- struct{}, keepOpen <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Read the active's Select.req (keep a copy of its header for the delayed answer below).
	activeReq, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read active select.req: %w", err)
	}
	if len(activeReq) < 10 || activeReq[5] != byte(hsms.SelectReqType) {
		return fmt.Errorf("expected select.req, got SType=%d", activeReq[5])
	}
	activeReqHdr := append([]byte(nil), activeReq[:10]...)

	// Send OUR OWN Select.req FIRST: the responder must commit Selected on this generation and
	// answer it status 0 BEFORE we ever answer the active's own Select.req.
	peerSB := [4]byte{0x11, 0x22, 0x33, 0x44}
	if _, err := conn.Write(selectReqFrame(peerSB)); err != nil {
		return fmt.Errorf("write peer select.req: %w", err)
	}

	// Read the responder's answer to OUR Select.req: it must be status 0 (Success) — the first,
	// genuine commit on this generation.
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
		return fmt.Errorf("expected peer select success (status 0), got status %d", rsp[3])
	}

	// NOW answer the active's OWN Select.req with status 1 (Communication Already Active): this
	// generation already selected via the responder commit above, so this is the legitimate
	// simultaneous-select race, not a refusal.
	if _, err := conn.Write(selectRspFrame(activeReqHdr, hsms.SelectStatusAlreadyActive)); err != nil {
		return fmt.Errorf("write select.rsp (status 1) for active: %w", err)
	}

	// Wait for the active to reach Selected before exercising the data round-trip.
	select {
	case <-sutSelected:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for active SELECTED state")
	}

	// S1F1 -> S1F2 round-trip to prove the link is functional.
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

	close(roundTripDone)

	// Hold the connection open until the caller has checked for a delayed disconnect: closing here
	// would itself end the TCP session and make any teardown after this point unobservable.
	<-keepOpen

	return nil
}
