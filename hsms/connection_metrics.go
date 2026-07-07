package hsms

import "sync/atomic"

// paddedUint64 pads an atomic.Uint64 to a 64-byte cache line to prevent false sharing. Padding
// assumes a 64-byte cache line (x86-64, arm64); it's a no-op rather than a correctness bug on
// architectures with larger lines (e.g. some ppc64/s390x variants).
type paddedUint64 struct {
	atomic.Uint64
	_ [56]byte // pad to 64 bytes (cache line size) to prevent false sharing
}

// paddedInt64 is paddedUint64's signed counterpart, for the same false-sharing reason.
type paddedInt64 struct {
	atomic.Int64
	_ [56]byte // pad to 64 bytes (cache line size) to prevent false sharing
}

// ConnectionMetrics holds lock-free connection counters shared by every transport (HSMS-SS and SECS-I).
//
// All reads and writes are atomic; safe to read concurrently with the protocol goroutines.
//
// Transport-specific observability lives in the owning transport package instead of here:
//   - HSMS-SS-only counters (linktest, Select/Separate/Reject) are in hsmsss.ConnectionMetrics,
//     reached via hsmsss.Connection.ControlMetrics().
//   - SECS-I-only counters (block send/recv/retry/etc.) are in secs1.ConnectionMetrics,
//     reached via secs1.Connection.BlockMetrics().
type ConnectionMetrics struct {
	_                      [64]byte // isolate dataMsgSend from whatever precedes this struct in memory
	dataMsgSend            paddedUint64
	dataMsgRecv            paddedUint64
	dataMsgInflight        paddedInt64
	dataMsgErr             atomic.Uint64
	dataMsgDropNotSelected atomic.Uint64 // B3 chokepoint: dropped because not SELECTED
	decodeErr              atomic.Uint64 // inbound frame read successfully but failed to decode/route
	bodyDecodeErr          atomic.Uint64 // frame framed OK + counted as received, but its lazy SECS-II body failed to decode and was diverted to a decode-error handler
	asyncSendErr           atomic.Uint64 // write failures on the fire-and-forget async send path
	connRetry              atomic.Int64  // gauge: 1 while a reconnect loop is actively retrying, else 0
	reconnects             atomic.Uint64 // cumulative count of successful re-establishments after an involuntary drop
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

// DecodeErrCount returns the total number of inbound data frames that were read successfully.
//
// These are frames read off the wire but failed to decode or route (DeliverOwnedFrame's
// decode-error path).
//
// This is disjoint from DataMsgRecvCount: a decode failure never reaches the receive chokepoint.
func (m *ConnectionMetrics) DecodeErrCount() uint64 {
	return m.decodeErr.Load()
}

// BodyDecodeErrCount returns the number of inbound data messages that framed
// successfully and were counted by DataMsgRecvCount, but whose lazy SECS-II body
// failed to decode and were diverted to a registered DecodeErrorHandler instead of
// the normal handlers.
//
// Unlike DecodeErrCount (frame-level failures that never reach the receive
// chokepoint), a body-decode failure is counted AFTER DataMsgRecvCount — the two are
// intentionally distinct so neither double-counts the other.
func (m *ConnectionMetrics) BodyDecodeErrCount() uint64 {
	return m.bodyDecodeErr.Load()
}

// AsyncSendErrCount returns the total number of fire-and-forget async sends whose transport write failed.
//
// This counts SendAsync, ForwardDataMessageAsync, and internal control-message async sends
// (such as Reject, Select.rsp) that failed on the transport write.
//
// These are otherwise silent: SendAsync itself only reports enqueue-boundary errors, never a
// later write failure.
// This is the only signal for "an async frame never reached the wire."
//
// See WithAsyncSendErrorHandler for a per-message callback.
func (m *ConnectionMetrics) AsyncSendErrCount() uint64 {
	return m.asyncSendErr.Load()
}

// DataMsgSendCount returns the total number of data messages committed to the wire.
//
// The message is counted once per frame at the single on-wire chokepoint when the writev
// succeeded.
//
// A message refused by the not-Selected gate (see DataMsgDropNotSelectedCount) never reaches
// the wire and is NOT counted here.
// Neither is an async (fire-and-forget) send that fails before the wire.
//
// Note this counts both primaries and replies — "send" here means "a data frame reached
// the wire," not "a primary transaction was initiated."
func (m *ConnectionMetrics) DataMsgSendCount() uint64 {
	return m.dataMsgSend.Load()
}

// DataMsgRecvCount returns the total number of data messages received.
func (m *ConnectionMetrics) DataMsgRecvCount() uint64 {
	return m.dataMsgRecv.Load()
}

// DataMsgErrCount returns the total number of synchronous (reply-expected) data sends that failed locally.
//
// This is triggered by a transport write error, or a T3 timeout awaiting the reply.
// It counts only the synchronous send path (sendWaitReply).
//
// Deliberately NOT counted here:
//   - A peer Reject of our transaction (surfaced as *RejectError — a peer-signalled outcome, not
//     a local send error).
//   - A fire-and-forget (non-W-bit) send, which returns before any reply wait.
//   - An async-path (SendAsync/ReplyDataMessage) failure.
//
// See RejectError for peer rejections.
func (m *ConnectionMetrics) DataMsgErrCount() uint64 {
	return m.dataMsgErr.Load()
}

// Reconnecting reports whether a reconnect loop is currently actively retrying.
//
// It returns 1 while retrying, and 0 when idle or connected.
// This is a GAUGE, not a cumulative counter — it goes up when a reconnect loop starts
// and back down when it exits, regardless of how many dial attempts happen inside.
//
// The gauge is also held at 1 while an active connection's OpenBackground initial-connect
// retry is in flight (it is the same underlying loop).
//
// See Reconnects for the cumulative count of successful re-establishments.
func (m *ConnectionMetrics) Reconnecting() int64 {
	return m.connRetry.Load()
}

// Reconnects returns the cumulative number of times this connection successfully re-established.
//
// This is counted once per successful re-establishment after an involuntary drop.
// It is never counted per failed dial attempt, and never for the very first Open() — including when
// that first connect had to retry a cold peer in the background (see countReconnect in connectLoop).
func (m *ConnectionMetrics) Reconnects() uint64 {
	return m.reconnects.Load()
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

func (m *ConnectionMetrics) incDecodeErr() {
	m.decodeErr.Add(1)
}

func (m *ConnectionMetrics) incBodyDecodeErr() {
	m.bodyDecodeErr.Add(1)
}

func (m *ConnectionMetrics) incAsyncSendErr() {
	m.asyncSendErr.Add(1)
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

func (m *ConnectionMetrics) incConnRetry() {
	m.connRetry.Add(1)
}

func (m *ConnectionMetrics) decConnRetry() {
	m.connRetry.Add(-1)
}

func (m *ConnectionMetrics) incReconnects() {
	m.reconnects.Add(1)
}
