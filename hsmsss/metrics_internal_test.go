package hsmsss

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionMetrics_ControlCounters(t *testing.T) {
	m := &ConnectionMetrics{}

	require.Equal(t, uint64(0), m.LinktestSendCount())
	require.Equal(t, uint64(0), m.LinktestRecvCount())
	require.Equal(t, uint64(0), m.LinktestErrCount())
	require.Equal(t, uint64(0), m.SelectEstablishedCount())
	require.Equal(t, uint64(0), m.SeparateRecvCount())
	require.Equal(t, uint64(0), m.RejectSentCount())
	require.Equal(t, uint64(0), m.RejectRecvCount())
	require.Equal(t, uint64(0), m.LinktestReqRecvCount())

	m.incLinktestSend()
	m.incLinktestSend()
	m.incLinktestRecv()
	m.incLinktestErr()
	m.incSelectEstablished()
	m.incSeparateRecv()
	m.incRejectSent()
	m.incRejectRecv()
	m.incLinktestReqRecv()

	require.Equal(t, uint64(2), m.LinktestSendCount())
	require.Equal(t, uint64(1), m.LinktestRecvCount())
	require.Equal(t, uint64(1), m.LinktestErrCount())
	require.Equal(t, uint64(1), m.SelectEstablishedCount())
	require.Equal(t, uint64(1), m.SeparateRecvCount())
	require.Equal(t, uint64(1), m.RejectSentCount())
	require.Equal(t, uint64(1), m.RejectRecvCount())
	require.Equal(t, uint64(1), m.LinktestReqRecvCount())
}
