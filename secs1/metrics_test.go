package secs1

// metrics_test.go — SECS-I metrics verification. The SECS-I transport rides the shared hsms core,
// so the core's DataMsg counters (hsms.ConnectionMetrics) are driven automatically by the engine —
// incDataMsgSend at the writeFrame on-wire chokepoint (after secs1.Write ACKs the last block),
// incDataMsgRecv at the DeliverOwnedFrame chokepoint (the secs1 assembler's delivery), and
// incDataMsgErr on a dropped W-bit reply (T3 timeout). On top of those, secs1 exposes its own
// block/line-level counters (secs1.ConnectionMetrics, reached via Connection.BlockMetrics()) for the
// wire-framing layer beneath the shared engine. These tests prove both flows end-to-end over the
// public API, reusing the T6 harness (newSECS1Passive / newSECS1Active / secs1EchoHandler).

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

// TestSECS1Metrics_BlockCounters proves the SECS-I block/line-level counters (secs1.ConnectionMetrics,
// reached via Connection.BlockMetrics()) fire on the clean single-block round-trip path. A W-bit
// exchange sends the active's primary (one block, ACK'd) and returns the passive's reply (one block,
// ACK'd), so after one round-trip: the active counts exactly one block SENT (the primary — the reply
// is received, not sent) with zero retries and zero send-failures, and the passive counts exactly one
// block RECEIVED (the primary). The passive's receive is driven asynchronously by its line engine, so
// its counter is awaited via require.Eventually, mirroring TestSECS1Metrics_DataMsgSendRecv.
func TestSECS1Metrics_BlockCounters(t *testing.T) {
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

	activeConn, ok := active.conn.(Connection)
	require.True(t, ok, "secs1.New must return a secs1.Connection")
	passiveConn, ok := passive.conn.(Connection)
	require.True(t, ok, "secs1.New must return a secs1.Connection")
	activeMetrics := activeConn.BlockMetrics()
	passiveMetrics := passiveConn.BlockMetrics()

	// Bringing the link to Selected touches no line block on either side (no HSMS handshake).
	require.Equal(t, uint64(0), activeMetrics.BlockSendCount(), "no blocks sent before the first data message")
	require.Equal(t, uint64(0), passiveMetrics.BlockRecvCount(), "no blocks received before the first primary")

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
	require.NoError(t, err, "the single-block round-trip must succeed")
	require.NotNil(t, reply)

	// Active: SendDataMessage returns only once the reply (received) has arrived, so its send counter
	// is exact immediately — exactly one block sent (the primary).
	require.Equal(t, uint64(1), activeMetrics.BlockSendCount(), "active must count exactly one block sent")

	// Passive: its receive of the primary is driven by its own line engine — let it settle.
	require.Eventually(t, func() bool {
		return passiveMetrics.BlockRecvCount() == uint64(1)
	}, 3*time.Second, 10*time.Millisecond, "passive must count exactly one block received")

	require.Equal(t, uint64(0), activeMetrics.BlockRetryCount(), "no retries on a clean single-block send")
	require.Equal(t, uint64(0), activeMetrics.BlockSendFailedCount(), "no RTY exhaustion on a clean send")
}
