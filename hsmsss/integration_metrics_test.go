package hsmsss

// metrics_test.go — end-to-end assertions for the send-path ConnectionMetrics counters over real
// active+passive loopback pairs (the same harness as the rest of the suite: newEndpoint /
// newEndpointPair / waitSelected / echoHandler / closeEndpoint from integration_helpers_test.go and
// freeLoopbackPort from passive_test.go). The hsms package proves the exact call-site wiring in a
// white-box unit test (send_metrics_test.go); here we prove the observable behaviour on the wire:
//   - DataMsgSendCount counts data sends (including replies) and NOT the control handshake.
//   - Linktest{Send,Recv} climb together on a healthy link while LinktestErr stays 0.
//   - DataMsgErrCount records a W-bit send whose reply is dropped (T3 timeout).
// All readiness/settling waits poll through require.Eventually (never time.Sleep-to-sync) and run
// under -race.

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

// controlMetrics returns an endpoint's HSMS-SS control-plane metrics via a CHECKED type assertion.
// Every endpoint.conn built by newEndpoint comes from New and is therefore a *connection, which
// satisfies the hsmsss.Connection interface; the comma-ok form keeps errcheck happy (a bare
// conn.(Connection) assertion would be an unchecked type assertion) and fails loudly if that ever
// stops holding.
func controlMetrics(t *testing.T, ep *endpoint) *ConnectionMetrics {
	t.Helper()

	c, ok := ep.conn.(Connection)
	require.True(t, ok, "endpoint.conn must satisfy hsmsss.Connection")

	return c.ControlMetrics()
}

// TestMetrics_DataMsgSend_Roundtrip proves DataMsgSendCount counts exactly the data messages each
// side puts on the wire: the active counts its N primaries, the passive counts its N echo replies,
// and the Select handshake (control) bumps neither.
func TestMetrics_DataMsgSend_Roundtrip(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil, echoHandler)
	active := newEndpoint(t, port, true, nil)
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// After the Select handshake (control frames only) NO data send has happened yet — proving the
	// handshake does not touch DataMsgSendCount.
	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgSendCount(), "the Select handshake must not bump DataMsgSendCount")
	require.Equal(t, uint64(0), passive.conn.Metrics().DataMsgSendCount())

	const n = 5
	for i := range n {
		reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
		require.NoErrorf(t, err, "send %d must succeed", i)
		require.NotNil(t, reply)
	}

	// The active put exactly N primaries on the wire.
	require.Equal(t, uint64(n), active.conn.Metrics().DataMsgSendCount(), "active must count exactly its N data sends")
	// The passive's echo replies are sent asynchronously (ReplyDataMessage → SendAsync), so let the
	// send counter settle.
	require.Eventually(t, func() bool {
		return passive.conn.Metrics().DataMsgSendCount() == uint64(n)
	}, 3*time.Second, 10*time.Millisecond, "passive must count exactly its N reply sends")

	// Linktest is disabled by the harness (interval 0), so the linktest send counter stays untouched.
	require.Equal(t, uint64(0), controlMetrics(t, active).LinktestSendCount(), "linktest is disabled — no linktest sends")
}

// TestMetrics_Linktest_SuccessPath enables auto-linktest on a healthy pair and proves the
// initiator success-path counters climb together (LinktestSendCount / LinktestRecvCount) while
// LinktestErrCount stays 0.
func TestMetrics_Linktest_SuccessPath(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	// Short linktest interval on a healthy loopback pair. WithLinktestInterval is appended after the
	// harness base (which sets interval 0), so it wins (last-writer). Both endpoints run the
	// initiator loop once Selected, so each side's counters climb.
	opt := WithConnectionOption(hsms.WithLinktestInterval(40 * time.Millisecond))
	passive := newEndpoint(t, port, false, []Option{opt}, echoHandler)
	active := newEndpoint(t, port, true, []Option{opt})
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	// On a healthy link every Linktest.req gets its Linktest.rsp, so the recv counter tracks the
	// send counter; wait for a few successful round-trips to accrue on the active side.
	const want = 3
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestRecvCount() >= want
	}, 10*time.Second, 20*time.Millisecond, "successful linktest round-trips must accrue on a healthy link")

	m := controlMetrics(t, active)
	// Send >= Recv (a send precedes its correlated recv) and both have climbed together.
	require.GreaterOrEqual(t, m.LinktestSendCount(), m.LinktestRecvCount(), "each recv is preceded by a send")
	require.GreaterOrEqual(t, m.LinktestRecvCount(), uint64(want))
	require.Equal(t, uint64(0), m.LinktestErrCount(), "a healthy link must record zero linktest errors")
}

// TestMetrics_DataMsgErr_T3Timeout proves a W-bit data send whose reply is never produced (the
// passive has no handler) records ErrT3Timeout AND bumps DataMsgErrCount, while the on-wire primary
// still counts as a send. A T3 timeout is a transaction failure, not a link failure.
func TestMetrics_DataMsgErr_T3Timeout(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passive := newEndpoint(t, port, false, nil) // no handler → never replies
	active := newEndpoint(t, port, true, []Option{WithConnectionOption(hsms.WithT3(1 * time.Second))})
	defer closeEndpoint(t, active)
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSelected(t, active)
	waitSelected(t, passive)

	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgErrCount())

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("x"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout)
	require.Nil(t, reply)

	require.Equal(t, uint64(1), active.conn.Metrics().DataMsgErrCount(), "a dropped W-bit reply must count as a data-message error")
	require.Equal(t, uint64(1), active.conn.Metrics().DataMsgSendCount(), "the primary reached the wire before T3 fired")
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a T3 timeout must not tear down the link")
}

// TestHSMSSSMetrics_ControlCounters drives the four control-plane counters that live on the RECEIVING
// side of an HSMS-SS control handshake through a real passive endpoint (the SUT) fed hand-authored
// frames by a raw peer, so each counter's exact production call site is exercised deterministically:
//
//   - SelectEstablishedCount: the SUT is the Select RESPONDER (handleSelectReq commits
//     NotSelected->Selected). The initiator side commits on the recv path and does NOT run
//     handleSelectReq, so this counter is proven on the responder — here the passive SUT — not on the
//     initiator. (Asserting it on a plain active endpoint of a pair would read 0.)
//   - RejectSentCount: a frame with an invalid SType makes the SUT emit a Reject.req (dispatchFrame ->
//     sendReject), keeping the link up.
//   - RejectRecvCount: an inbound (orphan) Reject.req is counted on receipt (transport_recv.go), then
//     dropped without a re-reject.
//   - SeparateRecvCount: a Separate.req while Selected is a peer-initiated teardown (handleSeparateReq);
//     asserted LAST because it drops the link.
func TestHSMSSSMetrics_ControlCounters(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil)
	defer closeEndpoint(t, passive)
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))

	m := controlMetrics(t, passive)

	raw := dialPassive(t, port)
	defer func() { _ = raw.Close() }()

	// (1) Select.req -> the SUT is the responder: it commits NotSelected->Selected and answers Select.rsp.
	_, err := raw.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, raw)
	waitSelected(t, passive)
	require.Eventually(t, func() bool {
		return m.SelectEstablishedCount() == uint64(1)
	}, 3*time.Second, 10*time.Millisecond, "the Select responder must count exactly one establishment")

	// (2) A frame with an INVALID SType -> dispatchFrame answers Reject.req (RejectSentCount++), link kept.
	const badSType = byte(0x63) // 99: not a valid HSMS SType
	rejectSB := [4]byte{0x11, 0x22, 0x33, 0x44}
	_, err = raw.Write(buildControlFrame(badSType, 0, rejectSB))
	require.NoError(t, err)
	// The SUT emits the Reject.req (reason 1 = SType-not-supported) back to us; drain it.
	assertRejectFrame(t, raw, hsms.RejectSTypeNotSupported, rejectSB)
	require.Eventually(t, func() bool {
		return m.RejectSentCount() == uint64(1)
	}, 3*time.Second, 10*time.Millisecond, "an invalid-SType frame must make the SUT emit exactly one Reject.req")

	// (3) An inbound ORPHAN Reject.req -> counted on receipt, then dropped (never re-rejected).
	_, err = raw.Write(buildRejectFrame(0, hsms.RejectNotSelected, [4]byte{0xAA, 0xBB, 0xCC, 0xDD}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return m.RejectRecvCount() == uint64(1)
	}, 3*time.Second, 10*time.Millisecond, "an inbound Reject.req must be counted on receipt")

	// (4) Separate.req while Selected -> a peer-initiated teardown (asserted last: it drops the link).
	_, err = raw.Write(buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0x00, 0x00, 0x00, 0x01}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return m.SeparateRecvCount() == uint64(1)
	}, 3*time.Second, 10*time.Millisecond, "a Separate.req while Selected must be counted as a peer teardown")
	waitState(t, passive, hsms.NotConnectedState)
}

// TestHSMSSSMetrics_LinktestErr_NotCountedOnClose proves — through the real end-to-end HSMS-SS harness —
// that a linktest round-trip aborted by a TEARDOWN (a graceful Close) is NOT counted as a linktest
// error. This mirrors the "teardown-not-counted" white-box scenario the hsms-level linktest tests used
// to cover before the counter relocation; by the SAME `ctx.Err()!=nil` guard in runLinktest it also
// covers the "caller-cancel / caller-deadline" case, because the only ctx an auto-linktest write is
// bound to is the generation ctx, which is cancelled solely by teardown — so teardown and caller-cancel
// are the identical guard here.
//
// Why it is NOT vacuous / cannot pass via the wrong code path: the peer answers Select but BLACKHOLES
// the auto Linktest.req (never replies), so the SUT's linktest WriteMessage blocks for the full T6. T6
// is set to 30s — orders of magnitude longer than the sub-second test window — so the T6 PROTOCOL timer
// physically cannot fire during the test: the ONLY way the in-flight linktest can resolve is the
// Close-driven ctx cancellation. LinktestSendCount>=1 proves a linktest was genuinely in flight and
// blocked (incLinktestSend runs before the blocked write); the final LinktestErrCount==0 proves that
// in-flight failure was correctly excluded. TEETH: delete the `if ctx.Err() != nil { return }` guard in
// runLinktest and this Close is miscounted -> LinktestErrCount==1.
//
// The COMPLEMENTARY case — a genuine T6 timeout with the parent ctx ALIVE DOES increment
// LinktestErrCount — is already covered end-to-end by TestHSMS_LinktestFailThreshold_ResetsOnSuccess in
// integration_linktest_threshold_test.go, which drops Linktest.rsp frames and asserts the cumulative
// LinktestErrCount climbs (to 2, then 4) while the link stays Selected (parent ctx alive throughout).
func TestHSMSSSMetrics_LinktestErr_NotCountedOnClose(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	peerReady := make(chan struct{})
	go func() { peerErrCh <- runLinktestBlackholePeer(ln, peerReady) }()

	// Active SUT: auto-linktest ON with a short interval, T6 far longer than the test window so the
	// protocol timer cannot fire, and a high fail-threshold so no stray timeout could ever disconnect
	// us mid-test. (The peer never answers Linktest.req, so no round-trip completes anyway.)
	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(50 * time.Millisecond)),
		WithConnectionOption(hsms.WithT6(30 * time.Second)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(1000)),
	})
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))

	select {
	case <-peerReady:
	case err := <-peerErrCh:
		t.Fatalf("blackhole peer failed during select: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the blackhole peer to answer Select")
	}
	waitSelected(t, active)

	m := controlMetrics(t, active)

	// A linktest is genuinely in flight: incLinktestSend fires before the (now-blocked) WriteMessage.
	require.Eventually(t, func() bool {
		return m.LinktestSendCount() >= 1
	}, 5*time.Second, 10*time.Millisecond, "the auto-linktest must put a Linktest.req in flight (blocked on the blackhole peer)")

	// No round-trip can have completed or errored yet: T6 (30s) cannot have fired in this window, and the
	// peer never replies, so both counters are still 0 while the first linktest blocks.
	require.Equal(t, uint64(0), m.LinktestErrCount(), "no T6 timeout can have fired yet (T6=30s >> test window)")
	require.Equal(t, uint64(0), m.LinktestRecvCount(), "the blackhole peer never answers, so no success is recorded")

	// Graceful teardown WHILE the linktest is in flight: Close -> Stop -> linktestCancel cancels the
	// in-flight linktest write. runLinktest observes ctx.Err()!=nil and returns WITHOUT incLinktestErr.
	// Close joins the linktest goroutine, so LinktestErrCount is final once Close returns.
	require.NoError(t, active.conn.Close())

	require.Equal(t, uint64(0), m.LinktestErrCount(),
		"a linktest aborted by teardown/Close (parent ctx cancelled) must NOT count as a linktest error")
	require.GreaterOrEqual(t, m.LinktestSendCount(), uint64(1),
		"a linktest was genuinely attempted and in flight when Close cancelled it (non-vacuous)")

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "blackhole peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the blackhole peer to finish")
	}
}

// runLinktestBlackholePeer accepts one connection from an active SUT, answers its Select.req as the
// responder, then DISCARDS every subsequent frame (notably the SUT's auto Linktest.req) WITHOUT ever
// replying — so each initiator linktest round-trip has no Linktest.rsp and blocks for the full T6. It
// keeps the TCP connection OPEN (reading and dropping frames) until the SUT closes it, so the SUT never
// sees a premature EOF that would tear the link down for the wrong reason.
func runLinktestBlackholePeer(ln net.Listener, ready chan<- struct{}) error {
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

	// Blackhole: read and drop every frame (the auto Linktest.req, and any farewell Separate on Close)
	// until the SUT closes the conn, at which point the read errors and the peer returns cleanly.
	for {
		if _, err := peerReadFrame(conn, 30*time.Second); err != nil {
			return nil // EOF / closed by the SUT teardown — the expected end of the peer.
		}
	}
}
