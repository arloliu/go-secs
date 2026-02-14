package secs1

import (
	"sync/atomic"
)

// ConnectionMetrics contains atomic metrics for a SECS-I connection.
// Metrics can be used as the value of a prometheus CounterFunc or GaugeFunc.
type ConnectionMetrics struct {
	// BlockSendCount indicates the number of blocks sent (ACK'd).
	BlockSendCount atomic.Uint64
	// BlockRecvCount indicates the number of blocks received.
	BlockRecvCount atomic.Uint64
	// BlockRetryCount indicates the total number of block send retries.
	BlockRetryCount atomic.Uint64

	// DataMsgSendCount indicates the number of data messages sent.
	DataMsgSendCount atomic.Uint64
	// DataMsgRecvCount indicates the number of data messages received.
	DataMsgRecvCount atomic.Uint64
	// DataMsgErrCount indicates the number of data message errors.
	DataMsgErrCount atomic.Uint64
	// DataMsgInflightCount indicates the number of data messages in flight (waiting for reply).
	DataMsgInflightCount atomic.Int64

	// ConnRetryGauge indicates the number of connection retries (active mode).
	ConnRetryGauge atomic.Uint32
}

func (m *ConnectionMetrics) incBlockSendCount() {
	m.BlockSendCount.Add(1)
}

func (m *ConnectionMetrics) incBlockRecvCount() {
	m.BlockRecvCount.Add(1)
}

func (m *ConnectionMetrics) incBlockRetryCount() {
	m.BlockRetryCount.Add(1)
}

func (m *ConnectionMetrics) incDataMsgSendCount() {
	m.DataMsgSendCount.Add(1)
}

func (m *ConnectionMetrics) incDataMsgRecvCount() {
	m.DataMsgRecvCount.Add(1)
}

func (m *ConnectionMetrics) incDataMsgErrCount() {
	m.DataMsgErrCount.Add(1)
}

func (m *ConnectionMetrics) incDataMsgInflightCount() {
	m.DataMsgInflightCount.Add(1)
}

func (m *ConnectionMetrics) decDataMsgInflightCount() {
	m.DataMsgInflightCount.Add(-1)
}

func (m *ConnectionMetrics) incConnRetryGauge() {
	m.ConnRetryGauge.Add(1)
}

func (m *ConnectionMetrics) resetConnRetryGauge() {
	m.ConnRetryGauge.Store(0)
}
