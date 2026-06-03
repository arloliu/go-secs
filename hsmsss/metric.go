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
	// DataMsgErrCount indicates the number of data message errors, including
	// decode/read failures and frames rejected due to an unsupported PType or
	// SType (answered with a Reject.req while the connection remains open).
	//
	// It does NOT include data messages dropped because the connection is not
	// Selected — those are expected backpressure, not faults, and are counted by
	// DataMsgDropNotSelectedCount instead.
	DataMsgErrCount atomic.Uint64
	// DataMsgDropNotSelectedCount indicates the number of outbound data messages
	// dropped because the connection was not in the Selected state — whether
	// rejected up front at a send entry point or dropped at the write boundary
	// when the link left Selected between enqueue and dequeue. Each dropped
	// message is counted exactly once. This is expected backpressure (e.g. the
	// application kept sending across a disconnect), not a protocol or I/O error.
	DataMsgDropNotSelectedCount atomic.Uint64
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

func (m *ConnectionMetrics) incDataMsgDropNotSelectedCount() {
	m.DataMsgDropNotSelectedCount.Add(1)
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
