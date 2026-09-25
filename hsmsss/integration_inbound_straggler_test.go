package hsmsss

// integration_inbound_straggler_test.go — end-to-end coverage for the generation-fenced inbound path (R10 / D2):
// the inbound fan-out and admission path must be fenced to the generation whose recv goroutine actually read the frame,
// so a straggler abandoned by a bounded Close cannot reach a live successor's handlers, channels, or reply registry
// once that successor exists.
//
// Both tests drive a REAL passive hsms.Connection + hsmsss transport (hsms.NewConnection wired to a real *transport,
// real TCP loopback peers scripted by hand) rather than a mock TransportRuntime,
// because the finding is about the REAL hsms core's DeliverOwnedFrame/RouteReply/session fan-out resolving
// "whatever generation is current" at call time (hsms/connection_runtime.go, hsms/session.go) —
// a mock runtime cannot reproduce that.
//
// session.recvDataMsg now applies a per-handler precheck.
// The …FromGeneration variants (DeliverOwnedFrameFromGeneration / RouteReplyFromGeneration) resolve the FRAME'S OWN epoch —
// the generation named by the caller —
// while the plain DeliverOwnedFrame / RouteReply still resolve whichever generation is CURRENT at call time;
// see their own doc comments for the fenced behavior the tests below pin.
// hsmsss's dispatchFrame always names its own generation (transport_control.go's deliverOwnedFrame/routeReply wrappers),
// so it always takes the FromGeneration path when the runtime offers it.

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

// rejectReqFrame builds a raw Reject.req frame (SType 7, E37 §7.10) carrying the given System Bytes and reason code.
// Used by TestAdmissionStraggler as the "control response" collision flavor:
// reply_registry.route's control-transaction exemption offers a Reject.req uncompared to ANY open registration —
// including a DATA transaction's — because it routes as a field-less *RejectError (msg == nil),
// so it is the one control-typed frame that CAN satisfy a data primary's wait
// (unlike Select/Deselect/Linktest.rsp, which always miss against a data registration).
func rejectReqFrame(sb [4]byte, reason byte) []byte {
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:2], 0xFFFF)
	h[3] = reason
	h[5] = byte(hsms.RejectReqType)
	copy(h[6:10], sb[:])

	return frameBytes(10, h, nil)
}

// ── test 11: fan-out straggler ──────────────────────────────────────────────

// TestInboundFanout_HandlerStragglerCannotReachSuccessor pins test 11:
// a data-message handler wedged inline on generation N's recv goroutine is abandoned by a bounded Close
// (independent of the wedge itself — the voluntary Close's own bounded join times out and moves on).
// Once a successor generation N+1 reaches Selected,
// releasing the wedged handler must not let handler 2 or a registered channel observe the stale generation-N message.
// It must also not put any frame onto the successor's live peer link.
// The end-to-end D3 check:
// handler 1's own reply attempt after release must be refused rather than delivered to the successor's peer.
//
// Teeth: session.recvDataMsg's per-handler loop must recheck cancellation before every handler,
// not just once before the loop starts.
// Removing that per-handler precheck lets handler 2 and the channel both observe the stale message,
// which is exactly what this test catches.
func TestInboundFanout_HandlerStragglerCannotReachSuccessor(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	releaseFn := sync.OnceFunc(func() { close(release) })
	var handler1Calls, handler2Calls atomic.Int32
	replyErrCh := make(chan error, 1)

	handler1 := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		handler1Calls.Add(1)
		close(entered)
		<-release // wedge — mirrors a blocking inline data handler (Close must abandon this)

		// D3 end-to-end: by the time this reply attempt runs, generation N has already ended
		// (Close returned and the successor is Selected),
		// so the reply must be refused rather than sent on the successor under N's stale System Bytes.
		replyErrCh <- ep.ReplyDataMessage(context.Background(), msg, secs2.A("stale-reply"))
	}
	handler2 := func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		handler2Calls.Add(1)
	}

	ch := make(chan *hsms.DataMessage, 4)

	conn, tr := newPassiveConnTr(t, port,
		hsms.WithSessionID(testDeviceID),
		hsms.WithLinktestInterval(0),
		hsms.WithCloseTimeout(150*time.Millisecond),
		hsms.WithT7(5*time.Second),
	)
	conn.AddDataMessageHandler(handler1, handler2)
	conn.AddDataMessageChan(ch)
	events := watchStates(conn) // registered before Open, per 300-testing.md

	t.Cleanup(func() { _ = conn.Close() }) // registered before releaseFn below, so it runs AFTER it
	t.Cleanup(releaseFn)                   // released before Close is attempted, in case of an early failure

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	peer1 := dialPassive(t, port)
	t.Cleanup(func() { _ = peer1.Close() })
	_, err := peer1.Write(selectReqFrame([4]byte{0, 0, 0, 1}))
	require.NoError(t, err)
	expectSelectRsp(t, peer1)
	events.await(t, hsms.SelectedState) // gen N reaches Selected over peer1

	genN := tr.currentGeneration()
	require.NotZero(t, genN)
	oldG := tr.wg // captured BEFORE teardown — the bundle the wedged recv loop registered on

	_, err = peer1.Write(dataFrame(testDeviceID, 1, 1, [4]byte{0, 0, 0, 9}, secs2.A("stale").ToBytes()))
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler1 never entered — the stale primary was never delivered")
	}

	// Trigger teardown INDEPENDENTLY of the wedged reader:
	// a voluntary Close's own bounded join (WithCloseTimeout above) times out waiting on handler1.
	// It abandons the straggler rather than hanging forever.
	closeStart := time.Now()
	_ = conn.Close()
	require.Less(t, time.Since(closeStart), 5*time.Second, "Close must be bounded, not wait out the wedged handler")

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground), "reopen must succeed even with the straggler still wedged")

	peer2 := dialPassive(t, port)
	t.Cleanup(func() { _ = peer2.Close() })
	_, err = peer2.Write(selectReqFrame([4]byte{0, 0, 0, 2}))
	require.NoError(t, err)
	expectSelectRsp(t, peer2)
	events.await(t, hsms.SelectedState) // gen N+1 reaches Selected over peer2

	genN1 := tr.currentGeneration()
	require.NotEqual(t, genN, genN1, "gen N+1 must carry a distinct identity")

	releaseFn() // let the wedged handler 1 return; the stale fan-out (if unfenced) continues from here

	waitWaitGroupDone(t, "the old generation's recv loop", oldG.recv.Wait)

	require.Zero(t, handler2Calls.Load(),
		"D2: handler 2 must never see the message delivered to the ended generation N")

	select {
	case msg := <-ch:
		t.Fatalf("D2: the registered channel must never receive the stale generation-N message, got %v", msg)
	default:
	}

	select {
	case replyErr := <-replyErrCh:
		require.ErrorIs(t, replyErr, hsms.ErrConnClosed,
			"D3: handler 1's reply from the ended generation must be refused, not sent on the successor")
	case <-time.After(2 * time.Second):
		t.Fatal("handler1 never attempted its post-release ReplyDataMessage")
	}

	require.NoError(t, peer2.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	buf := make([]byte, 1)
	_, err = peer2.Read(buf)
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
		"the successor's peer must receive nothing from generation N's stale fan-out or D3-refused reply (got err=%v)", err)
}

// ── test 12: admission straggler ────────────────────────────────────────────

// admissionStragglerFixture is what setupAdmissionStraggler hands back:
// everything a flavor-specific test needs to release the paused straggler and assert on the successor.
type admissionStragglerFixture struct {
	conn    hsms.Connection
	tr      *transport
	oldG    *genWG
	peer2   net.Conn
	release func() // idempotent; releases the paused straggler goroutine
}

// setupAdmissionStraggler is the shared setup for test 12's three flavors
// (data secondary, control response / Reject.req, primary).
// It opens a passive connection and selects generation N over peer1.
// It pauses buildStaleFrame's frame's admission via testHookAfterReadFrame right after readFrame completes.
// It tears generation N down with a bounded Close (abandoning the paused goroutine).
// It then reopens and selects generation N+1 over peer2.
//
// Teeth: replace g.gen routing with routing by t.currentGeneration() (today's live generation)
// at either call site (deliverOwnedFrame / routeReply, transport_control.go) —
// the paused frame is then admitted onto whatever generation is live when it resumes,
// which every flavor built on this fixture catches from its own angle.
func setupAdmissionStraggler(t *testing.T, label string, buildStaleFrame func(sb [4]byte) []byte, staleSB [4]byte) *admissionStragglerFixture {
	t.Helper()

	port := freeLoopbackPort(t)

	conn, tr := newPassiveConnTr(t, port,
		hsms.WithSessionID(testDeviceID),
		hsms.WithLinktestInterval(0),
		hsms.WithCloseTimeout(150*time.Millisecond),
		hsms.WithT3(5*time.Second),
	)
	events := watchStates(conn) // registered before Open, per 300-testing.md

	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	peer1 := dialPassive(t, port)
	t.Cleanup(func() { _ = peer1.Close() })
	_, err := peer1.Write(selectReqFrame([4]byte{0, 0, 0, 1}))
	require.NoError(t, err)
	expectSelectRsp(t, peer1)
	events.await(t, hsms.SelectedState) // gen N reaches Selected over peer1

	genN := tr.currentGeneration()
	require.NotZero(t, genN)
	oldG := tr.wg

	entered := make(chan struct{})
	release := make(chan struct{})
	releaseFn := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseFn) // registered after the conn.Close cleanup above, so it runs first

	hookFn := func() {
		// One-shot: clear the seam immediately so no OTHER frame — in particular gen N+1's own
		// Select.req, read by a different recv loop after reconnect — is ever paused by it.
		tr.testHookAfterReadFrame.Store(nil)
		close(entered)
		<-release
	}
	tr.testHookAfterReadFrame.Store(&hookFn)

	_, err = peer1.Write(buildStaleFrame(staleSB))
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the recv loop never reached the admission hook", label)
	}

	closeStart := time.Now()
	_ = conn.Close() // bounded; abandons the paused straggler
	require.Less(t, time.Since(closeStart), 5*time.Second)

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	peer2 := dialPassive(t, port)
	t.Cleanup(func() { _ = peer2.Close() })
	_, err = peer2.Write(selectReqFrame([4]byte{0, 0, 0, 2}))
	require.NoError(t, err)
	expectSelectRsp(t, peer2)
	events.await(t, hsms.SelectedState) // gen N+1 reaches Selected over peer2

	require.NotEqual(t, genN, tr.currentGeneration())

	return &admissionStragglerFixture{conn: conn, tr: tr, oldG: oldG, peer2: peer2, release: releaseFn}
}

// runCollisionCase is test 12's shared body for the two collision flavors (data secondary, control response / Reject.req):
// the stale frame's System Bytes are engineered to collide with a live transaction the successor opens,
// and releasing the straggler must not complete that transaction.
func runCollisionCase(t *testing.T, label string, buildStaleFrame func(sb [4]byte) []byte) {
	t.Helper()

	// staleSB is only a PREDICTION until confirmed against the wire below:
	// the connection's SECOND-EVER NextSystemBytes() value.
	// The FIRST value is consumed by the graceful teardown's own courtesy farewell Separate.req
	// (hsms/connection_lifecycle.go's writeFarewellSeparate, fired once per Selected->NotConnected Close).
	// So the successor's own first send — the "live" transaction this test opens — is deterministically the second value.
	staleSB := [4]byte{0, 0, 0, 2}

	f := setupAdmissionStraggler(t, label, buildStaleFrame, staleSB)

	type sendResult struct {
		msg *hsms.DataMessage
		err error
	}
	replyCh := make(chan sendResult, 1)
	go func() {
		msg, sendErr := f.conn.SendDataMessage(context.Background(), 1, 1, true, secs2.A("live"))
		replyCh <- sendResult{msg, sendErr}
	}()

	// Confirm the live primary actually hit the wire (its System-Bytes registration is open).
	// Confirm too that its ACTUAL wire System Bytes are the ones the stale frame collides with —
	// not merely assumed — before releasing the paused straggler.
	live, err := peerReadFrame(f.peer2, 5*time.Second)
	require.NoError(t, err, "peer2 must observe the successor's live primary")
	require.GreaterOrEqual(t, len(live), 10, "a data frame must carry at least a 10-byte header")
	var liveSB [4]byte
	copy(liveSB[:], live[6:10])
	require.Equal(t, staleSB, liveSB,
		"the stale frame's System Bytes must collide with the successor's ACTUAL live transaction, not an assumed constant")

	f.release()

	waitWaitGroupDone(t, "the old generation's recv loop", f.oldG.recv.Wait)

	select {
	case res := <-replyCh:
		t.Fatalf("%s: the successor's open transaction must NOT be completed by generation N's stale frame (msg=%v, err=%v)",
			label, res.msg, res.err)
	case <-time.After(500 * time.Millisecond):
		// still pending — correct: the stale frame must not have satisfied it.
	}
}

// TestAdmissionStraggler_DataSecondary is test 12's data-secondary flavor.
func TestAdmissionStraggler_DataSecondary(t *testing.T) {
	t.Parallel()

	runCollisionCase(t, "DataSecondary", func(sb [4]byte) []byte {
		return dataFrame(testDeviceID, 1, 2, sb, secs2.A("stale-secondary").ToBytes())
	})
}

// TestAdmissionStraggler_ControlResponse is test 12's control-response flavor.
// Reject.req is the one control-typed frame the reply registry's control-transaction exemption lets satisfy a DATA transaction uncompared;
// see rejectReqFrame's doc comment.
func TestAdmissionStraggler_ControlResponse(t *testing.T) {
	t.Parallel()

	runCollisionCase(t, "ControlResponse/RejectReq", func(sb [4]byte) []byte {
		return rejectReqFrame(sb, 3)
	})
}

// TestAdmissionStraggler_Primary is test 12's primary flavor:
// unlike the secondary/control-response flavors, a stale PRIMARY has nothing open on the successor to collide with —
// the successor must simply never deliver it to a data handler, or place any frame on the successor's peer socket.
func TestAdmissionStraggler_Primary(t *testing.T) {
	t.Parallel()

	var handlerCalls atomic.Int32
	handler := func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) { handlerCalls.Add(1) }

	staleSB := [4]byte{0, 0, 0, 2} // arbitrary — a primary has nothing to collide with
	f := setupAdmissionStraggler(t, "Primary",
		func(sb [4]byte) []byte { return dataFrame(testDeviceID, 1, 1, sb, secs2.A("stale-primary").ToBytes()) },
		staleSB,
	)
	f.conn.AddDataMessageHandler(handler)

	f.release()

	waitWaitGroupDone(t, "the old generation's recv loop", f.oldG.recv.Wait)

	require.Zero(t, handlerCalls.Load(),
		"D2: the successor must never deliver generation N's stale primary to a data handler")

	require.NoError(t, f.peer2.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	buf := make([]byte, 1)
	_, err := f.peer2.Read(buf)
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
		"the successor's peer must receive nothing from generation N's stale primary (got err=%v)", err)
}
