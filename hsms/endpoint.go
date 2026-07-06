package hsms

import (
	"context"

	"github.com/arloliu/go-secs/v2/secs2"
)

// DataMessageHandler is the callback type for inbound HSMS data messages.
// ep is the SECS2Endpoint interface (not a concrete session type) so the session
// type remains unexported.
//
// A handler is invoked INLINE on the connection's single receive goroutine (not on a
// separate notifier goroutine — that is the StateChangeHandler contract, which differs).
// A handler therefore MUST NOT BLOCK: while it runs, the receive loop cannot read the next
// frame, and Close cannot complete until the handler returns (Close bounds this by the
// close timeout — see WithCloseTimeout — and returns ErrCloseTimeout if a handler stays
// blocked). Offload slow work to your own goroutine.
type DataMessageHandler func(msg *DataMessage, ep SECS2Endpoint)

// SECS2Endpoint is the capability surface exposed to DataMessageHandlers and the
// cross-transport swap contract. It covers all blocking SECS-II send/reply
// operations plus handler registration.
type SECS2Endpoint interface {
	// SessionID returns the HSMS session ID (no I/O; never blocks).
	SessionID() uint16

	// SendDataMessage sends a primary SECS-II data message and — when
	// replyExpected is true — waits for the matching secondary reply or a
	// protocol/context timeout. Returns the reply DataMessage on success.
	SendDataMessage(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*DataMessage, error)

	// SendDataMessageAsync enqueues a fire-and-forget data message on the
	// per-generation async send channel. The caller is not blocked waiting for a
	// reply; errors are surfaced only at the enqueue boundary.
	SendDataMessageAsync(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) error

	// SendSECS2Message sends a pre-constructed SECS-II message and waits for the
	// reply when the W-bit is set. Returns the reply DataMessage on success.
	SendSECS2Message(ctx context.Context, msg secs2.SECS2Message) (*DataMessage, error)

	// ForwardDataMessage writes a pre-built data message verbatim on the caller's
	// goroutine — preserving its System Bytes, W-bit, session ID, and
	// stream/function — and returns once the frame is on the wire. Unlike
	// SendDataMessage it does NOT register a reply expectation: any secondary the
	// peer sends misses the reply registry and is delivered to the registered
	// DataMessageHandlers instead of being returned here. Use this when YOU own
	// reply correlation — a router or proxy bridging transports that matches a
	// reply to an upstream request by System Bytes. Ordinary request/reply code
	// should use SendDataMessage. A W-bit-set msg keeps its W-bit on the wire (the
	// peer is still asked to reply); only the LOCAL reply-wait is skipped. Returns
	// ErrNilMessage if msg is nil.
	//
	// The caller owns System Bytes: they are sent verbatim and are NOT drawn from
	// the connection's monotonic generator, so they may collide with an in-flight
	// SendDataMessage/SendSECS2Message transaction OR another forwarded message on
	// the same connection. On a collision the peer's reply is routed to whichever
	// waiter/handler the registry resolves — reply theft. Keep forwarded System
	// Bytes disjoint from the library-generated space (which starts at 1 and
	// increments).
	//
	// The message's session ID is sent verbatim on HSMS-SS. If the connection
	// enables WithSessionIDValidation, a forwarded session ID that differs from the
	// connection's makes the peer's reply fail inbound validation and be dropped
	// (and answered with an S9F1), so it never reaches your handlers — forward with
	// the connection's own session ID, or leave validation disabled. On SECS-I the
	// block-header device ID is always the configured device ID regardless of msg's
	// session ID (only System Bytes / stream / function / W-bit go on the wire
	// verbatim).
	ForwardDataMessage(ctx context.Context, msg *DataMessage) error

	// ForwardDataMessageAsync is ForwardDataMessage but enqueues msg on the
	// per-generation async send channel instead of writing on the caller's
	// goroutine; a write failure is best-effort (surfaced only at the enqueue
	// boundary, not the wire). The System Bytes / session-ID caveats on
	// ForwardDataMessage apply here too. Returns ErrNilMessage if msg is nil.
	ForwardDataMessageAsync(ctx context.Context, msg *DataMessage) error

	// ReplyDataMessage sends a secondary data message in reply to primary. The
	// reply reuses primary's System Bytes verbatim (E37 §8.2.6.9) and carries no
	// W-bit.
	ReplyDataMessage(ctx context.Context, primary *DataMessage, item secs2.Item) error

	// AddDataMessageHandler appends one or more inbound data message handlers.
	// Registration is not blocking I/O and does not take a context.
	AddDataMessageHandler(handlers ...DataMessageHandler)

	// AddConnStateChangeHandler appends one or more connection state-change
	// handlers. Handlers persist across Open/Close cycles and are never removed.
	// Registration is not blocking I/O and does not take a context.
	AddConnStateChangeHandler(handlers ...StateChangeHandler)
}

// Connection is the app-facing handle for an HSMS connection. The concrete engine
// is unexported; hsmsss.New and secs1.New return this interface.
//
// Connection embeds SECS2Endpoint so callers can send/receive SECS-II messages
// and register handlers directly on the Connection value.
type Connection interface {
	SECS2Endpoint

	// Open starts the connection lifecycle (dial/listen + FSM). mode selects
	// blocking (OpenWaitSelected) or background (OpenBackground) behavior.
	// ctx bounds the synchronous wait when mode is OpenWaitSelected. For an
	// active connection under OpenBackground, a peer that is not yet reachable
	// at Open time is not an error: Open returns nil and the connection retries
	// the initial connect in the background (see the (*connection).Open doc for
	// the exact NotConnectedState-only scoping).
	Open(ctx context.Context, mode OpenMode) error

	// Close tears down the connection and all per-generation resources. Close is
	// ctx-free and always completes teardown (internally bounded by the configured
	// close timeout). Idempotent: a second Close returns the prior error.
	Close() error

	// State returns the current FSM ConnState. Returns NotConnectedState when the
	// connection has not been opened or has been closed. Never nil-derefs.
	State() ConnState

	// Metrics returns the live connection metrics (atomic counters).
	Metrics() *ConnectionMetrics

	// UpdateConfigOptions applies functional options to the connection's live
	// configuration transactionally (validate-all, then commit atomically).
	UpdateConfigOptions(opts ...ConnOption) error
}
