package hsmsss

import (
	"sync/atomic"
)

// ConnectionMetrics contains atomic metrics for a connection.
// Metrics can be used as the value of a prometheus CounterFunc or GaugeFunc.
type ConnectionMetrics struct {
	// LinktestSendCount indicates the number of linktest messages sent.
	LinktestSendCount atomic.Uint64
	// LinktestRecvCount indicates the number of linktest messages received.
	LinktestRecvCount atomic.Uint64
	// LinktestErrCount indicates the number of linktest errors.
	LinktestErrCount atomic.Uint64

	// DataMsgSendCount indicates the number of data messages sent.
	DataMsgSendCount atomic.Uint64
	// DataMsgRecvCount indicates the number of data messages received.
	DataMsgRecvCount atomic.Uint64
	// DataMsgErrCount indicates the number of data message errors.
	DataMsgErrCount atomic.Uint64
	// DataMsgInflightCount indicates the number of data messages in flight.
	DataMsgInflightCount atomic.Int64

	// ConnRetryGauge indicates the number of connection retries.
	ConnRetryGauge atomic.Uint32
}

func (m *ConnectionMetrics) incLinktestSendCount() {
	m.LinktestSendCount.Add(1)
}

func (m *ConnectionMetrics) incLinktestRecvCount() {
	m.LinktestRecvCount.Add(1)
}

func (m *ConnectionMetrics) incLinktestErrCount() {
	m.LinktestErrCount.Add(1)
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
