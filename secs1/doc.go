// Package secs1 implements SECS-I (SEMI E4) over a TCP/IP stream for the v2 immutable-message model:
// both the message framing (splitting an immutable SECS-II body into transport blocks and
// reassembling received blocks) and the SECS-I connection, built on the shared hsms engine.
//
// # Framing
//
// A SECS-II message body is split into blocks of at most 244 body bytes, numbered 1..N with the
// last-block (E-bit) flag set on the final block; received blocks are reassembled in order back into
// the body for lazy decoding. Blocks are immutable value types holding a 10-byte SEMI E4 header and a
// zero-copy view of the shared message body (internal/wire) — the split is allocation-free per block.
//
// # Connection
//
// [New] builds the SECS-I transport and returns the consumer-facing [Connection] — which embeds
// hsms.Connection (every shared HSMS-II send/reply/handler operation is available unchanged) and
// adds BlockMetrics, the SECS-I block/line-level counters (see "Metrics" below) — mirroring how
// hsmsss.New returns hsmsss.Connection for HSMS-SS. [Config] configures it with transactional
// functional options that mirror that package (embedded core knobs plus SECS-I-specific settings):
// the active/passive TCP role, the T1 (inter-character), T2 (protocol), and T4 (inter-block)
// timers, the RTY retry limit, and the equipment/host role and device ID. The core reply timeout
// is the embedded T3.
//
// [New] forces the core write timeout to 0 (disabled): a SECS-I line transaction self-bounds via
// T2×(RTY+1), so a core write deadline is redundant and would spuriously preempt a legitimately
// retrying send. The override is enforced both at construction — [NewConfig] applies it as the LAST
// option, after all caller-supplied options — and at runtime: Connection.UpdateConfigOptions
// re-appends hsms.WithWriteTimeout(0) after the caller's options, so a WithWriteTimeout supplied at
// runtime is intercepted and neutralized rather than taking effect.
//
// # Half-duplex line engine
//
// SECS-I is a half-duplex block protocol: only one party drives the line at a time. A single
// line-engine goroutine owns the connection for each generation — it is the only code that reads or
// writes socket bytes — alternating between draining a pending outbound send and polling for an
// inbound block. Each block transfer is an ENQ/EOT/ACK/NAK handshake bounded by T1/T2; a block that
// is not ACK'd is retransmitted up to RTY times, and simultaneous send attempts are arbitrated by
// contention (the master — the equipment, per IsEquip — wins; the slave yields, delivers the master's
// block, then re-sends its own as a fresh transaction). A block send that exhausts RTY returns an
// error from the transport, which the core treats as a line failure: it tears the generation down and
// the reconnect loop re-dials.
//
// # Inbound handlers
//
// A data-message handler (registered via AddDataMessageHandler) runs INLINE on the single line-engine
// goroutine — the same goroutine that transmits. It therefore MUST NOT issue a SYNCHRONOUS send on the
// same connection: SendDataMessage and SendSECS2Message write on the caller's goroutine, so from a
// handler they wait on the very goroutine running the handler and deadlock the line — regardless of the
// W-bit (even a non-reply SendDataMessage blocks contending for the transmitter). A subsequent Close
// still recovers it, bounded by the close timeout, abandoning the wedged handler. To send from within a
// handler, use the asynchronous SendDataMessageAsync or ReplyDataMessage, or dispatch the synchronous
// send to a separate goroutine.
//
// # State naming
//
// SECS-I has no HSMS Select procedure, so the [hsms.SelectedState] reported by Connection.State()
// means simply "link established and usable" — the TCP connection is up and the transport auto-commits
// Selected. Read hsms.SelectedState as "usable" rather than "HSMS-selected" for a SECS-I connection.
//
// # Metrics
//
// A SECS-I connection reports the shared core data-message counters via hsms.Connection.Metrics():
// DataMsgSendCount (a message whose last block was ACK'd on the wire), DataMsgRecvCount (a reassembled
// inbound message delivered to the core), and DataMsgErrCount (e.g. a W-bit send whose reply times out
// at T3). Beneath that, Connection.BlockMetrics() exposes the SECS-I-specific block/line-level
// counters in [ConnectionMetrics]: block send/recv counts, block-send retries (NAK, T2 timeout, or a
// failed contention-yield receive), RTY-exhaustion failures, NAKs sent for inbound blocks, slave
// contention yields, duplicate-block drops, T4 partial-message timeouts, wrong-direction block drops,
// and three inbound-assembler violation counters: DeviceIDMismatchCount (a block carrying a device ID
// that does not match this connection's configured device ID), BlockNumberMismatchCount (a block that
// aborted an open multi-block partial by arriving out of sequence or with a mismatched block-invariant
// header), and InvalidFirstBlockCount (a block starting a new message that was neither block 1 nor a
// lone single-block-0). See "Assembler violations" below for how these interact with the equipment
// role's auto-notification.
//
// # Assembler violations
//
// Assembler-violation notifications are sent for the EQUIPMENT role only (IsEquip() == true) and are
// not separately configurable — there is no ValidateDataMessage-style toggle in v2, unlike v1. A
// device-ID mismatch (DeviceIDMismatchCount) auto-replies S9F1 (Unrecognized Device ID); a block-
// number mismatch, header mismatch, or invalid first block (BlockNumberMismatchCount or
// InvalidFirstBlockCount) auto-replies S9F7 (Illegal Data), per SEMI E5 §10.13. The host role sends
// nothing for either case.
package secs1
