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
