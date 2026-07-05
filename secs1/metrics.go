package secs1

import "sync/atomic"

// ConnectionMetrics holds lock-free SECS-I (SEMI E4) block/line-level counters: the wire-framing
// layer beneath the shared hsms data-message counters (see hsms.ConnectionMetrics, reached via
// Connection's embedded hsms.Connection.Metrics()). All reads and writes are atomic; safe to read
// concurrently with the line-engine goroutine. Reach an instance via Connection.BlockMetrics().
// Padding assumes a 64-byte cache line (x86-64, arm64); it's a no-op rather than a correctness bug
// on architectures with larger lines (e.g. some ppc64/s390x variants).
type paddedUint64 struct {
	atomic.Uint64
	_ [56]byte // pad to 64 bytes (cache line size) to prevent false sharing
}

type ConnectionMetrics struct {
	_                [64]byte      // isolate blockSend from whatever precedes this struct in memory
	blockSend        paddedUint64  // blocks successfully sent and ACK'd
	blockRecv        paddedUint64  // blocks received (and ACK'd) from the peer
	blockRetry       atomic.Uint64 // block-send retries (NAK, T2 timeout, or a failed contention-yield receive)
	blockSendFailed  atomic.Uint64 // RTY retry limit exhausted without an ACK (E4 §7.8.6) — a link-teardown-triggering event
	blockNAKSent     atomic.Uint64 // NAKs we sent for an inbound block (length error, checksum error, T1 or T2 timeout)
	contentionYield  atomic.Uint64 // this end (as slave) yielded to a contending master (E4 §7.8.2.1)
	blockDupDrop     atomic.Uint64 // duplicate-block detected and dropped (E4 §9.4.2 — the peer missed our ACK)
	partialTimeout   atomic.Uint64 // a stale partial multi-block message was discarded (E4 §9.4.3, T4 elapsed)
	blockDirDrop     atomic.Uint64 // an inbound block was dropped for the wrong R-bit direction (E4 §8.2)
	deviceIDMismatch atomic.Uint64 // inbound block dropped for the wrong device ID (E4 §9.4.1 routing check)

	blockNumberMismatch atomic.Uint64 // inbound block continuing an open message had the wrong number/header (E4 §9.4.4.2)
	invalidFirstBlock   atomic.Uint64 // inbound block starting a new message was not a valid first block (E4 §9.4.4.2)
}

// BlockSendCount returns the total number of SECS-I blocks successfully sent and ACK'd.
func (m *ConnectionMetrics) BlockSendCount() uint64 { return m.blockSend.Load() }

// BlockRecvCount returns the total number of SECS-I blocks received (and ACK'd) from the peer.
func (m *ConnectionMetrics) BlockRecvCount() uint64 { return m.blockRecv.Load() }

// BlockRetryCount returns the total number of SECS-I block-send retries (a NAK, a T2 timeout, or a
// failed receive during a slave-contention yield — SEMI E4 §7.8.2).
func (m *ConnectionMetrics) BlockRetryCount() uint64 { return m.blockRetry.Load() }

// BlockSendFailedCount returns the total number of times the RTY retry limit was exhausted without
// an ACK (SEMI E4 §7.8.6). Unlike BlockRetryCount (routine, expected under a noisy line), this is a
// link-failure signal worth alerting on.
func (m *ConnectionMetrics) BlockSendFailedCount() uint64 { return m.blockSendFailed.Load() }

// BlockNAKSentCount returns the total number of NAKs this connection sent for an inbound block
// (aggregating a length error, a checksum/parse error, a T1 timeout reading the body, or a T2
// timeout waiting for the length byte). A nonzero, growing count diagnoses a misframing or noisy
// peer.
func (m *ConnectionMetrics) BlockNAKSentCount() uint64 { return m.blockNAKSent.Load() }

// ContentionYieldCount returns the total number of times this connection, acting as slave, yielded
// line control to a contending master (SEMI E4 §7.8.2.1) — a pure half-duplex contention signal.
func (m *ConnectionMetrics) ContentionYieldCount() uint64 { return m.contentionYield.Load() }

// BlockDupDropCount returns the total number of duplicate blocks detected and dropped (SEMI E4
// §9.4.2). A nonzero count means an ACK this connection sent was lost in transit.
func (m *ConnectionMetrics) BlockDupDropCount() uint64 { return m.blockDupDrop.Load() }

// PartialTimeoutCount returns the total number of stale partial multi-block messages discarded
// after the T4 inter-block deadline elapsed (SEMI E4 §9.4.3).
func (m *ConnectionMetrics) PartialTimeoutCount() uint64 { return m.partialTimeout.Load() }

// BlockDirDropCount returns the total number of inbound blocks dropped for carrying the wrong R-bit
// direction (SEMI E4 §8.2) — typically a bring-up configuration mistake (host/equipment role
// mismatch), near-zero in steady state.
func (m *ConnectionMetrics) BlockDirDropCount() uint64 { return m.blockDirDrop.Load() }

// DeviceIDMismatchCount returns the total number of inbound blocks dropped for carrying a device ID
// that does not match this connection's configured device ID (SEMI E4 §9.4.1 routing check).
func (m *ConnectionMetrics) DeviceIDMismatchCount() uint64 { return m.deviceIDMismatch.Load() }

// BlockNumberMismatchCount returns the total number of inbound blocks that aborted an open partial
// message because they carried the wrong block number or a mismatched block-invariant header (SEMI
// E4 §9.4.4.2).
func (m *ConnectionMetrics) BlockNumberMismatchCount() uint64 { return m.blockNumberMismatch.Load() }

// InvalidFirstBlockCount returns the total number of inbound blocks dropped for being neither block
// number 1 nor a lone block 0 with the E-bit set when starting a new message (SEMI E4 §9.4.4.2).
func (m *ConnectionMetrics) InvalidFirstBlockCount() uint64 { return m.invalidFirstBlock.Load() }

// Unexported increment helpers used by the line engine (secs1/line.go, secs1/assembler.go). Kept
// private so ConnectionMetrics can only ever reflect real protocol events, never application-code
// manipulation — the same encapsulation hsms.ConnectionMetrics itself uses.

func (m *ConnectionMetrics) incBlockSendCount()        { m.blockSend.Add(1) }
func (m *ConnectionMetrics) incBlockRecvCount()        { m.blockRecv.Add(1) }
func (m *ConnectionMetrics) incBlockRetryCount()       { m.blockRetry.Add(1) }
func (m *ConnectionMetrics) incBlockSendFailedCount()  { m.blockSendFailed.Add(1) }
func (m *ConnectionMetrics) incBlockNAKSentCount()     { m.blockNAKSent.Add(1) }
func (m *ConnectionMetrics) incContentionYieldCount()  { m.contentionYield.Add(1) }
func (m *ConnectionMetrics) incBlockDupDropCount()     { m.blockDupDrop.Add(1) }
func (m *ConnectionMetrics) incPartialTimeoutCount()   { m.partialTimeout.Add(1) }
func (m *ConnectionMetrics) incBlockDirDropCount()     { m.blockDirDrop.Add(1) }
func (m *ConnectionMetrics) incDeviceIDMismatchCount() { m.deviceIDMismatch.Add(1) }

func (m *ConnectionMetrics) incBlockNumberMismatchCount() { m.blockNumberMismatch.Add(1) }
func (m *ConnectionMetrics) incInvalidFirstBlockCount()   { m.invalidFirstBlock.Add(1) }
