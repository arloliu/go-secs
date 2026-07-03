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
	// ctx bounds the synchronous wait when mode is OpenWaitSelected.
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
