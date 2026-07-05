// Package hsms provides immutable HSMS (High-Speed SECS Message Services) message
// types and factories for semiconductor-equipment communication per SEMI E37.
//
// HSMS defines a TCP/IP framing layer and handshake protocol for SECS-II data messages
// together with a set of control messages (Select, Deselect, Linktest, Reject, Separate).
// This package implements the message layer: construction, encode, decode, and read-only
// accessor access. Connection, state-machine, and transport concerns are handled in
// sibling packages such as hsmsss and secs1.
//
// # Message model
//
// Two concrete immutable message types are defined:
//
//   - [ControlMessage]: an HSMS control message (SType 1–9). Header-only; no body.
//   - [DataMessage]: an HSMS data message (SType 0) carrying a SECS-II item body.
//
// Both satisfy the [Message] interface, which exposes the header fields common to all
// HSMS messages:
//
//   - [Message.Type]: the HSMS SType as a [MsgType] constant.
//   - [Message.SessionID], [Message.SystemBytes], [Message.HeaderBytes]: header values,
//     all returned as value types so callers may retain them without aliasing risk.
//   - [Message.ToBytes]: full on-wire frame: 4-byte big-endian length, 10-byte header,
//     and the optional SECS-II body.
//   - [Message.ToDataMessage]: narrows to a [*DataMessage], or (nil, false) for a
//     [*ControlMessage].
//
// The Reject.req reason code is read from a Reject.req message via
// [ControlMessage.RejectReasonCode]; a peer's rejection of a synchronous send is
// surfaced to the caller as a [RejectError].
//
// [DataMessage] extends the interface with [DataMessage.Stream], [DataMessage.Function],
// [DataMessage.WaitBit], [DataMessage.Item], [DataMessage.DecodeErr],
// [DataMessage.BodyLen], and [DataMessage.AppendBodyTo].
//
// # Construction
//
// Control messages are built via typed factories:
//
//   - [NewSelectReq] / [NewSelectRsp]
//   - [NewDeselectReq] / [NewDeselectRsp]
//   - [NewLinktestReq] / [NewLinktestRsp]
//   - [NewRejectReq] / [NewRejectReqRaw]
//   - [NewSeparateReq]
//
// Data messages are built via [NewDataMessage]. Before construction, [NewDataMessage]
// performs Q3 validation (SEMI E37 §8.3.3.3):
//
//   - item.Error() must be nil, including recursive aggregate errors in list children.
//   - replyExpected (W=1) is rejected when function is even (a reply function).
//   - stream must be in [0, 127].
//
// To derive a new [DataMessage] from an existing one with structural field changes, use
// the builder:
//
//	derived, err := msg.Derive().
//	    WithStream(3).
//	    WithFunction(7).
//	    WithWaitBit(false).
//	    WithItem(newItem).
//	    Build()
//
// [DataMessageBuilder.Build] runs the full Q3 validation. Fields not overridden by the
// builder are inherited from the source message. [DataMessage.WithSessionID] and
// [DataMessage.WithSystemBytes] skip validation entirely because those envelope fields
// are always valid.
//
// # Error model
//
// Two distinct error channels are kept separate and never conflated:
//
//   - [DataMessage.Item] returns (item, error): the lazy wire-to-item decode error on
//     the raw-frame decode path. An empty body yields ([secs2.NewEmptyItem], nil). The
//     error is non-nil only when the body bytes cannot be parsed by [secs2.Decode].
//   - [DataMessage.DecodeErr]: the cached error returned by Item after the decode fires.
//     Useful when the body is needed only in error-checking, not in full item form.
//
// A peer's rejection of a synchronous send is a separate, transaction-scoped outcome
// surfaced as a [RejectError], not a message-level error channel.
//
// Construction and [DataMessageBuilder.Build] errors are returned as (msg, error) and
// include the item's aggregate Error() gate.
//
// # Decode
//
// [DecodeHSMSMessage] decodes a complete on-wire HSMS frame (4-byte big-endian length
// prefix + 10-byte header + optional body) into a [Message]. All length fields are
// validated before any slice operation to prevent panics on malformed input:
//
//	msg, err := hsms.DecodeHSMSMessage(frame)
//
// [DecodeHSMSMessage] copies the payload into an owned buffer so the caller may reuse
// or free frame immediately after the call.
//
// For [DataMessage] the SECS-II body is decoded lazily: [DataMessage.Item] triggers
// [secs2.Decode] exactly once (under a [sync.Once]) on the first call; subsequent calls
// return the cached item and error.
//
// An internal zero-copy decode path (unexported) accepts a caller-owned per-message
// buffer and retains it without copying, eliminating the decode-time allocation on the
// receive hot path. That path is used by in-repo transports; external callers use
// [DecodeHSMSMessage].
//
// # Immutability, concurrency, and fan-out
//
// All [Message] values are safe for concurrent use once constructed. No method mutates
// the receiver.
//
// Envelope-field restamping via [DataMessage.WithSessionID] and
// [DataMessage.WithSystemBytes] (and the equivalent [ControlMessage] methods) returns a
// new message that shares the original body, making restamp O(header) and body-size-
// independent:
//
//	stamped := msg.WithSessionID(newID)   // new *DataMessage; body shared, not re-encoded
//
// The decode state is also shared across With* copies: [secs2.Decode] fires at most once
// regardless of which copy calls [DataMessage.Item] first, and every copy sees the same
// cached item.
//
// For fan-out, multiple goroutines may hold and call any method on the same *[DataMessage]
// concurrently without external locking.
//
// # Dissolved v1 landmines
//
// The v2 redesign did not merely patch four v1 concurrency/memory hazards — it restructured
// the connection engine and message model so the conditions that created them no longer exist.
// The notes below record what each hazard was and why it is now structurally impossible.
//
// Send-gate lock-order inversion. v1 held a send read-lock and then
// took the context mutex; a deadlock class existed if the send-gate reset (a send write-lock)
// were ever placed inside context creation (holding the context mutex), inverting the order
// against a concurrent send. v2 has no send gate at all: sends flow through a per-generation
// send channel created with the connection generation and reclaimed at teardown, and the live
// context is a single atomic pointer read on the send path (no context mutex). The lock that
// created the ordering constraint no longer exists, so this deadlock class is structurally impossible.
//
// Stale frame carried across generations. v1's sender channel lived for the whole
// connection lifetime; a send racing a close could enqueue a frame after the gate closed but
// before the drain finished, so a stale frame (stale System Bytes/session) could be sent as the
// first frame of the next Open generation, and v1 needed an elaborate send-lock span plus a
// background-context drain to prevent it. v2 allocates a fresh send channel per generation and
// abandons the old one at close; the next generation's sender goroutine reads a different
// channel, so a frame enqueued in generation N physically cannot reach generation N+1's sender.
// No send-gate drain logic is required.
//
// Free/double-Free/clone/aliasing. v1 required idempotent Free (an atomic
// compare-and-swap to survive overlapping teardown paths), fan-out cloning so concurrent handlers
// would not use a freed message, a System-Bytes accessor that returned a slice aliasing internal
// buffers (a retain-past-Free hazard), and an explicit Free at every drop/reject/drain exit. v2
// removes the pooling API entirely: there is no Free, no pool toggle, and no reference counting.
// Messages are GC-owned immutable values that concurrent goroutines share safely, and
// [Message.SystemBytes] / [Message.HeaderBytes] return array VALUE types, so the aliasing hazard
// is impossible by type. The remaining zero-copy discipline lives on the internal wire seam, not
// on any public API.
//
// Reply-channel close asymmetry. v1 had two transports with different reply-channel idioms
// (one could close reply channels at teardown, the other forbade it and used a drain-then-nil
// send); unifying them onto one multi-goroutine reply-routing core while keeping the close idiom
// would risk a send-on-closed-channel panic. v2 uses sender-owned reply channels throughout: each
// waiting sender allocates its own reply channel, registers it in the per-generation reply
// registry, and owns its full lifecycle (a timeout or cancel unregisters and drains; the router
// sends without ever closing). No goroutine closes a channel it did not create, so both transports
// share one pattern and the asymmetry is dissolved. (The SECS-I half-duplex I/O-ownership sense of
// this hazard is a separate transport concern, out of scope for this package.)
package hsms
