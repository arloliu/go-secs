package hsmsss

import "sync/atomic"

// ConnectionMetrics holds lock-free HSMS-SS (SEMI E37.1) control-plane counters: linktest and the
// Select/Separate/Reject handshake — a control layer with no SECS-I analogue at all (SECS-I has no
// session/select concept, and its own line-level counters live in secs1.ConnectionMetrics instead).
// All reads and writes are atomic; safe to read concurrently with the transport goroutines. Reach an
// instance via Connection.ControlMetrics().
type ConnectionMetrics struct {
	linktestSend      atomic.Uint64 // Linktest.req sent by our own auto-linktest
	linktestRecv      atomic.Uint64 // Linktest.rsp received (successful round-trip)
	linktestErr       atomic.Uint64 // our own linktest round-trip failed (T6 timeout or write error)
	selectEstablished atomic.Uint64 // Select responder committed NotSelected -> Selected (E37 §9.2.2)
	separateRecv      atomic.Uint64 // peer Separate.req received while Selected (peer-initiated teardown)
	rejectSent        atomic.Uint64 // Reject.req WE emit (peer sent us a malformed/unexpected frame)
	rejectRecv        atomic.Uint64 // inbound Reject.req received (peer rejected one of our sends)
	linktestReqRecv   atomic.Uint64 // inbound Linktest.req answered (peer probing us)
	readErr           atomic.Uint64 // socket read / frame-length error in recvLoop
}

// LinktestSendCount returns the total number of Linktest.req messages sent by this connection's own
// auto-linktest.
func (m *ConnectionMetrics) LinktestSendCount() uint64 {
	return m.linktestSend.Load()
}

// LinktestRecvCount returns the total number of Linktest.rsp round-trips this connection's own
// auto-linktest completed successfully.
func (m *ConnectionMetrics) LinktestRecvCount() uint64 {
	return m.linktestRecv.Load()
}

// LinktestErrCount returns the cumulative number of failed initiator linktest attempts (a T6 timeout
// or write error). It only ever grows and is purely observational — it never influences the
// linktest-fail-threshold disconnect decision.
func (m *ConnectionMetrics) LinktestErrCount() uint64 {
	return m.linktestErr.Load()
}

// SelectEstablishedCount returns the total number of times this connection's Select responder
// committed NotSelected -> Selected (E37 §9.2.2's "communications established").
func (m *ConnectionMetrics) SelectEstablishedCount() uint64 {
	return m.selectEstablished.Load()
}

// SeparateRecvCount returns the total number of inbound Separate.req messages received while
// Selected (each one tears down the connection — a peer-initiated disconnect, E37 §7.9.2).
func (m *ConnectionMetrics) SeparateRecvCount() uint64 {
	return m.separateRecv.Load()
}

// RejectSentCount returns the total number of Reject.req messages this connection emitted in
// response to a malformed or unexpected inbound frame (E37 §7.9).
func (m *ConnectionMetrics) RejectSentCount() uint64 {
	return m.rejectSent.Load()
}

// RejectRecvCount returns the total number of inbound Reject.req messages received (the peer
// rejected one of our sends).
func (m *ConnectionMetrics) RejectRecvCount() uint64 {
	return m.rejectRecv.Load()
}

// LinktestReqRecvCount returns the total number of inbound Linktest.req messages answered (the peer
// probing this connection).
func (m *ConnectionMetrics) LinktestReqRecvCount() uint64 {
	return m.linktestReqRecv.Load()
}

// ReadErrCount returns the total number of socket read or frame-length errors encountered in the
// receive loop (recvLoop's readFrame error branch) — a plain transport-level read failure, distinct
// from hsms.ConnectionMetrics.DecodeErrCount (a frame that WAS read successfully but failed to
// decode).
func (m *ConnectionMetrics) ReadErrCount() uint64 {
	return m.readErr.Load()
}

// Unexported increment helpers used by the transport (see transport_procedures.go and
// transport_control.go). Kept private so ConnectionMetrics can only ever reflect real protocol
// events, never application-code manipulation — the same encapsulation hsms.ConnectionMetrics uses.

func (m *ConnectionMetrics) incLinktestSend() {
	m.linktestSend.Add(1)
}

func (m *ConnectionMetrics) incLinktestRecv() {
	m.linktestRecv.Add(1)
}

func (m *ConnectionMetrics) incLinktestErr() {
	m.linktestErr.Add(1)
}

func (m *ConnectionMetrics) incSelectEstablished() {
	m.selectEstablished.Add(1)
}

func (m *ConnectionMetrics) incSeparateRecv() {
	m.separateRecv.Add(1)
}

func (m *ConnectionMetrics) incRejectSent() {
	m.rejectSent.Add(1)
}

func (m *ConnectionMetrics) incRejectRecv() {
	m.rejectRecv.Add(1)
}

func (m *ConnectionMetrics) incLinktestReqRecv() {
	m.linktestReqRecv.Add(1)
}

func (m *ConnectionMetrics) incReadErrCount() {
	m.readErr.Add(1)
}
