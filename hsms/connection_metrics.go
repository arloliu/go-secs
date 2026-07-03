package hsms

import "sync/atomic"

// ConnectionMetrics holds lock-free connection counters. All reads and writes
// are atomic; safe to read concurrently with the protocol goroutines.
type ConnectionMetrics struct {
	linktestSend           atomic.Uint64
	linktestRecv           atomic.Uint64
	linktestErr            atomic.Uint64
	dataMsgSend            atomic.Uint64
	dataMsgRecv            atomic.Uint64
	dataMsgErr             atomic.Uint64
	dataMsgDropNotSelected atomic.Uint64 // B3 chokepoint: dropped because not SELECTED
	dataMsgInflight        atomic.Int64  // I1 gauge (data W-bit messages in flight)
	connRetry              atomic.Int64  // reconnect attempt gauge
}

// DataMsgInflightCount returns the current number of data messages in flight.
func (m *ConnectionMetrics) DataMsgInflightCount() int64 {
	return m.dataMsgInflight.Load()
}

// DataMsgDropNotSelectedCount returns the total number of data messages dropped
// because the connection was not in SELECTED state.
func (m *ConnectionMetrics) DataMsgDropNotSelectedCount() uint64 {
	return m.dataMsgDropNotSelected.Load()
}

// DataMsgSendCount returns the total number of data messages committed to the wire (the writev
// succeeded), counted once per frame at the single on-wire chokepoint. A message refused by the
// not-Selected gate (see DataMsgDropNotSelectedCount) never reaches the wire and is NOT
// counted here; neither is an async (fire-and-forget) send that fails before the wire.
func (m *ConnectionMetrics) DataMsgSendCount() uint64 {
	return m.dataMsgSend.Load()
}

// DataMsgRecvCount returns the total number of data messages received.
func (m *ConnectionMetrics) DataMsgRecvCount() uint64 {
	return m.dataMsgRecv.Load()
}

// DataMsgErrCount returns the total number of synchronous (reply-expected) data sends that failed
// locally: a transport write error, or a T3 timeout awaiting the reply. It counts only the
// synchronous send path (sendWaitReply). Deliberately NOT counted here: a peer Reject of our
// transaction (surfaced as *RejectError — a peer-signalled outcome, not a local send error); a
// fire-and-forget (non-W-bit) send, which returns before any reply wait; and an async-path
// (SendAsync/ReplyDataMessage) failure. See RejectError for peer rejections.
func (m *ConnectionMetrics) DataMsgErrCount() uint64 {
	return m.dataMsgErr.Load()
}

// LinktestSendCount returns the total number of linktest messages sent.
//
// Linktest is an HSMS-SS mechanism; a SECS-I connection performs no HSMS linktest,
// so this counter reads zero for a SECS-I connection.
func (m *ConnectionMetrics) LinktestSendCount() uint64 {
	return m.linktestSend.Load()
}

// LinktestRecvCount returns the total number of linktest responses received.
//
// Linktest is an HSMS-SS mechanism; a SECS-I connection performs no HSMS linktest,
// so this counter reads zero for a SECS-I connection.
func (m *ConnectionMetrics) LinktestRecvCount() uint64 {
	return m.linktestRecv.Load()
}

// LinktestErrCount returns the cumulative number of failed initiator linktest attempts (a T6
// timeout or write error on a linktest transaction we sent). It only ever grows. It is DISTINCT
// from the internal consecutive-failure counter that drives the linktest-fail-threshold disconnect
// (that counter resets on any success); a teardown-race may bump this cumulative count without
// advancing the internal one. Purely observational — it never influences the disconnect decision.
//
// Linktest is an HSMS-SS mechanism; a SECS-I connection performs no HSMS linktest,
// so this counter reads zero for a SECS-I connection.
func (m *ConnectionMetrics) LinktestErrCount() uint64 {
	return m.linktestErr.Load()
}

// ConnRetryCount returns the reconnect-activity GAUGE: the number of reconnect loops currently
// running (0 when idle/connected, 1 while the single-session engine is retrying dials). Like
// DataMsgInflightCount it is a gauge, not a cumulative counter — it goes up when a reconnect loop
// starts and back down when it exits; it does NOT accumulate one-per-dial-attempt.
func (m *ConnectionMetrics) ConnRetryCount() int64 {
	return m.connRetry.Load()
}

// Unexported increment/decrement helpers used by the protocol engine.

func (m *ConnectionMetrics) incDataMsgInflight() {
	m.dataMsgInflight.Add(1)
}

func (m *ConnectionMetrics) decDataMsgInflight() {
	m.dataMsgInflight.Add(-1)
}

func (m *ConnectionMetrics) incDataMsgDropNotSelected() {
	m.dataMsgDropNotSelected.Add(1)
}

func (m *ConnectionMetrics) incDataMsgSend() {
	m.dataMsgSend.Add(1)
}

func (m *ConnectionMetrics) incDataMsgRecv() {
	m.dataMsgRecv.Add(1)
}

func (m *ConnectionMetrics) incDataMsgErr() {
	m.dataMsgErr.Add(1)
}

func (m *ConnectionMetrics) incLinktestSend() {
	m.linktestSend.Add(1)
}

func (m *ConnectionMetrics) incLinktestRecv() {
	m.linktestRecv.Add(1)
}

func (m *ConnectionMetrics) incLinktestErr() {
	m.linktestErr.Add(1)
}

func (m *ConnectionMetrics) incConnRetry() {
	m.connRetry.Add(1)
}

func (m *ConnectionMetrics) decConnRetry() {
	m.connRetry.Add(-1)
}
