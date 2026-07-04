package secs1

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectionMetrics_BlockCounters(t *testing.T) {
	m := &ConnectionMetrics{}

	require.Equal(t, uint64(0), m.BlockSendCount())
	require.Equal(t, uint64(0), m.BlockRecvCount())
	require.Equal(t, uint64(0), m.BlockRetryCount())
	require.Equal(t, uint64(0), m.BlockSendFailedCount())
	require.Equal(t, uint64(0), m.BlockNAKSentCount())
	require.Equal(t, uint64(0), m.ContentionYieldCount())
	require.Equal(t, uint64(0), m.BlockDupDropCount())
	require.Equal(t, uint64(0), m.PartialTimeoutCount())
	require.Equal(t, uint64(0), m.BlockDirDropCount())

	m.incBlockSendCount()
	m.incBlockSendCount()
	m.incBlockRecvCount()
	m.incBlockRetryCount()
	m.incBlockSendFailedCount()
	m.incBlockNAKSentCount()
	m.incContentionYieldCount()
	m.incBlockDupDropCount()
	m.incPartialTimeoutCount()
	m.incBlockDirDropCount()

	require.Equal(t, uint64(2), m.BlockSendCount())
	require.Equal(t, uint64(1), m.BlockRecvCount())
	require.Equal(t, uint64(1), m.BlockRetryCount())
	require.Equal(t, uint64(1), m.BlockSendFailedCount())
	require.Equal(t, uint64(1), m.BlockNAKSentCount())
	require.Equal(t, uint64(1), m.ContentionYieldCount())
	require.Equal(t, uint64(1), m.BlockDupDropCount())
	require.Equal(t, uint64(1), m.PartialTimeoutCount())
	require.Equal(t, uint64(1), m.BlockDirDropCount())
}
