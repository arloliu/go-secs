package secs1

import (
	"testing"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestSendMessageAsync_GatedWhileNotSelected guards the secs1 parity fix: the
// async send path must reject a data message with ErrNotSelectedState while the
// connection is not Selected — mirroring sendMsg and sendMsgSync — rather than
// enqueuing it onto the protocol loop. The matching hsmsss bug produced a
// NotSelected→NotConnected reconnect loop; see
// docs/plans/hsmsss-async-send-notselected-loop/.
func TestSendMessageAsync_GatedWhileNotSelected(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Construct but do not open: the connection is NotConnected (not Selected).
	comm := newTestComm(ctx, t, port, true, true)
	require.False(comm.conn.stateMgr.IsSelected(), "fresh connection must not be Selected")

	m, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
	require.NoError(err)

	require.ErrorIs(comm.session.SendMessageAsync(m), ErrNotSelectedState,
		"secs1 async data send must be gated (ErrNotSelectedState) while not Selected")

	// The drop is counted as expected backpressure, not as a data-message error.
	require.Equal(uint64(1), comm.conn.metrics.DataMsgDropNotSelectedCount.Load(),
		"a not-Selected drop must increment DataMsgDropNotSelectedCount")
	require.Equal(uint64(0), comm.conn.metrics.DataMsgErrCount.Load(),
		"a not-Selected drop must NOT increment DataMsgErrCount")
}

// TestSendGates_FreeMessageWhileNotSelected guards that all three secs1 send-gate
// paths (sendMsg, sendMsgSync, sendMsgAsync) Free their pooled DataMessage when the
// connection is not Selected, closing the v1.17.0 leak where sendMsgSync did not
// Free before returning ErrNotSelectedState.
//
// Proof: hsms/pool.go putDataMessage sets msg.dataItem = nil before returning the
// struct to sync.Pool. DataMessage.Item() returns msg.dataItem. Therefore
// msg.Item() == nil is a deterministic post-free signal — valid here because the
// connection is never opened, so no sender goroutines run concurrently that could
// Get the same struct from the pool and repopulate dataItem between the gate call
// and the Item() read.
//
// Pool-aliasing ordering: each path's msg is created, passed to the gate, and its
// Item() checked before the next message is constructed. This is required because
// the freed struct can be immediately returned by the next pool Get.
func TestSendGates_FreeMessageWhileNotSelected(t *testing.T) {
	if !hsms.IsUsePool() {
		t.Skip("pool-free signal only works when pooling is enabled")
	}

	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole())
	require.NoError(t, err)

	conn, err := NewConnection(ctx, cfg)
	require.NoError(t, err)

	_ = conn.AddSession(testSessionID)

	// Sanity: the connection must not be Selected for any gate to fire.
	require.False(t, conn.stateMgr.IsSelected(), "never-opened connection must not be Selected")

	t.Run("sendMsg_NonWBit", func(t *testing.T) {
		r := require.New(t)

		msg, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		r.NoError(err)
		r.NotNil(msg.Item(), "sanity: item must be set before gate call")

		reply, sendErr := conn.sendMsg(msg)
		r.ErrorIs(sendErr, ErrNotSelectedState)
		r.Nil(reply)
		// putDataMessage (hsms/pool.go:95) sets dataItem = nil; Item() exposes that field.
		r.Nil(msg.Item(), "sendMsg gate must Free the message (putDataMessage nils dataItem)")
	})

	t.Run("sendMsg_WBit", func(t *testing.T) {
		r := require.New(t)

		msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		r.NoError(err)
		r.NotNil(msg.Item(), "sanity: item must be set before gate call")

		reply, sendErr := conn.sendMsg(msg)
		r.ErrorIs(sendErr, ErrNotSelectedState)
		r.Nil(reply)
		// putDataMessage (hsms/pool.go:95) sets dataItem = nil; Item() exposes that field.
		r.Nil(msg.Item(), "sendMsg gate must Free the message (putDataMessage nils dataItem)")
	})

	t.Run("sendMsgSync", func(t *testing.T) {
		r := require.New(t)

		msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		r.NoError(err)
		r.NotNil(msg.Item(), "sanity: item must be set before gate call")

		syncErr := conn.sendMsgSync(msg)
		r.ErrorIs(syncErr, ErrNotSelectedState)
		// This is the v1.17.0 fix path (secs1/conn.go ~line 1000).
		// putDataMessage (hsms/pool.go:95) sets dataItem = nil; Item() exposes that field.
		r.Nil(msg.Item(), "sendMsgSync gate must Free the message (v1.17.0 fix; putDataMessage nils dataItem)")
	})

	t.Run("sendMsgAsync", func(t *testing.T) {
		r := require.New(t)

		msg, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		r.NoError(err)
		r.NotNil(msg.Item(), "sanity: item must be set before gate call")

		asyncErr := conn.sendMsgAsync(msg)
		r.ErrorIs(asyncErr, ErrNotSelectedState)
		// putDataMessage (hsms/pool.go:95) sets dataItem = nil; Item() exposes that field.
		r.Nil(msg.Item(), "sendMsgAsync gate must Free the message (putDataMessage nils dataItem)")
	})
}
