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
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

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
	require.Equal(t, uint64(0), active.conn.Metrics().LinktestSendCount(), "linktest is disabled — no linktest sends")
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
		return active.conn.Metrics().LinktestRecvCount() >= want
	}, 10*time.Second, 20*time.Millisecond, "successful linktest round-trips must accrue on a healthy link")

	m := active.conn.Metrics()
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
