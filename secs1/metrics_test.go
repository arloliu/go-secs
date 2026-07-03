package secs1

// metrics_test.go — SP5b T8 metrics verification. The SECS-I transport rides the shared hsms core,
// so the core's DataMsg counters (hsms.ConnectionMetrics) are driven automatically by the engine —
// incDataMsgSend at the writeFrame on-wire chokepoint (after secs1.Write ACKs the last block),
// incDataMsgRecv at the DeliverOwnedFrame chokepoint (the secs1 assembler's delivery), and
// incDataMsgErr on a dropped W-bit reply (T3 timeout). No secs1-specific metric type is added:
// SECS-I-only counters (retry, contention) are deferred (spec §T8 default) — the core send/recv/err
// counters already give the send/recv/error observability. These tests prove that flow end-to-end
// over the public API, reusing the T6 harness (newSECS1Passive / newSECS1Active / secs1EchoHandler).

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestSECS1Metrics_DataMsgSendRecv proves the core DataMsgSend/DataMsgRecv counters flow through the
// SECS-I transport. secs1 has no HSMS Select handshake (Selected auto-commits at TCP connect), so
// BEFORE any send every data counter is 0. After N W-bit round-trips the active has sent N primaries
// and received N replies; the passive has received N primaries and sent N replies. The active side is
// exact immediately after the synchronous loop (SendDataMessage returns only once the reply — counted
// at DeliverOwnedFrame — has arrived); the passive's reply sends are asynchronous (ReplyDataMessage →
// SendAsync), so its counters are awaited via require.Eventually.
func TestSECS1Metrics_DataMsgSendRecv(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port, secs1EchoHandler)
	active := newSECS1Active(t, port)
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	// No data has crossed yet — bringing the link to Selected must not touch the data counters.
	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgSendCount(), "no send before the first data message")
	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgRecvCount(), "no recv before the first reply")
	require.Equal(t, uint64(0), passive.conn.Metrics().DataMsgRecvCount(), "no recv before the first primary")

	const n = 5
	for i := range n {
		reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
		require.NoErrorf(t, err, "round-trip %d must succeed", i)
		require.NotNil(t, reply)
	}

	// Active: exactly N primaries on the wire, and exactly N replies received back.
	require.Equal(t, uint64(n), active.conn.Metrics().DataMsgSendCount(), "active must count exactly its N primary sends")
	require.Equal(t, uint64(n), active.conn.Metrics().DataMsgRecvCount(), "active must count exactly its N received replies")

	// Passive: N primaries received, N replies sent (the reply send is async — let it settle).
	require.Equal(t, uint64(n), passive.conn.Metrics().DataMsgRecvCount(), "passive must count exactly its N received primaries")
	require.Eventually(t, func() bool {
		return passive.conn.Metrics().DataMsgSendCount() == uint64(n)
	}, 3*time.Second, 10*time.Millisecond, "passive must count exactly its N reply sends")

	// SECS-I arms no linktest (O1) — the linktest counters stay untouched.
	require.Equal(t, uint64(0), active.conn.Metrics().LinktestSendCount(), "SECS-I arms no linktest — no linktest sends")
}

// TestSECS1Metrics_DataMsgErr_T3Timeout proves a dropped W-bit reply increments the core
// DataMsgErrCount over the SECS-I transport. The passive has no handler, so it ACKs the primary at
// the line layer (DataMsgSend fires — the primary reached the wire) but never replies; the active's
// SendDataMessage times out at T3 with hsms.ErrT3Timeout and bumps DataMsgErrCount. A T3 timeout is a
// transaction-level event, so the SECS-I link stays Selected (mirrors the hsmsss metrics test).
func TestSECS1Metrics_DataMsgErr_T3Timeout(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	passive := newSECS1Passive(t, port) // no handler → never replies
	active := newSECS1Active(t, port, WithConnectionOption(hsms.WithT3(500*time.Millisecond)))
	defer closeSECS1Endpoint(t, active)
	defer closeSECS1Endpoint(t, passive)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	waitSECS1Selected(t, passive)
	waitSECS1Selected(t, active)

	require.Equal(t, uint64(0), active.conn.Metrics().DataMsgErrCount(), "no error before the timed-out send")

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("x"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout)
	require.Nil(t, reply)

	require.Equal(t, uint64(1), active.conn.Metrics().DataMsgErrCount(), "a dropped W-bit reply must count as a data-message error")
	require.Equal(t, uint64(1), active.conn.Metrics().DataMsgSendCount(), "the primary reached the wire (ACK'd) before T3 fired")
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a T3 timeout must not tear down the SECS-I link")
}
