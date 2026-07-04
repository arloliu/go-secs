package hsms

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionMetrics_DataMsgInflight(t *testing.T) {
	var m ConnectionMetrics

	require.Equal(t, int64(0), m.DataMsgInflightCount())

	m.incDataMsgInflight()
	m.incDataMsgInflight()
	require.Equal(t, int64(2), m.DataMsgInflightCount())

	m.decDataMsgInflight()
	require.Equal(t, int64(1), m.DataMsgInflightCount())

	m.decDataMsgInflight()
	require.Equal(t, int64(0), m.DataMsgInflightCount())
}

func TestConnectionMetrics_DataMsgDropNotSelected(t *testing.T) {
	var m ConnectionMetrics

	require.Equal(t, uint64(0), m.DataMsgDropNotSelectedCount())

	m.incDataMsgDropNotSelected()
	m.incDataMsgDropNotSelected()
	m.incDataMsgDropNotSelected()
	require.Equal(t, uint64(3), m.DataMsgDropNotSelectedCount())
}

func TestConnectionMetrics_DataMsgCounters(t *testing.T) {
	var m ConnectionMetrics

	require.Equal(t, uint64(0), m.DataMsgSendCount())
	require.Equal(t, uint64(0), m.DataMsgRecvCount())
	require.Equal(t, uint64(0), m.DataMsgErrCount())

	m.incDataMsgSend()
	m.incDataMsgSend()
	require.Equal(t, uint64(2), m.DataMsgSendCount())

	m.incDataMsgRecv()
	require.Equal(t, uint64(1), m.DataMsgRecvCount())

	m.incDataMsgErr()
	m.incDataMsgErr()
	require.Equal(t, uint64(2), m.DataMsgErrCount())
}

func TestConnectionMetrics_ReconnectsAndReconnecting(t *testing.T) {
	m := &ConnectionMetrics{}

	require.Equal(t, uint64(0), m.Reconnects())
	require.Equal(t, int64(0), m.Reconnecting())

	m.incReconnects()
	m.incConnRetry()

	require.Equal(t, uint64(1), m.Reconnects())
	require.Equal(t, int64(1), m.Reconnecting())

	m.decConnRetry()
	require.Equal(t, int64(0), m.Reconnecting())
}
