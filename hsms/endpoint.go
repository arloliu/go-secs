package hsms

import (
	"context"

	"github.com/arloliu/go-secs/v2/secs2"
)

// DataMessageHandler is the callback type for inbound HSMS data messages. ep is the SECS2Endpoint interface (not a concrete session type)
// so the session type remains unexported.
//
// A handler is invoked INLINE on the connection's single receive goroutine (not on a separate notifier goroutine —
// that is the StateChangeHandler contract, which differs).
// A handler therefore MUST NOT BLOCK: while it runs, the receive loop cannot read the next frame,
// and Close cannot complete until the handler returns (Close bounds this by the close timeout — see WithCloseTimeout —
// and returns ErrCloseTimeout if a handler stays blocked).
// Offload slow work to your own goroutine.
type DataMessageHandler func(msg *DataMessage, ep SECS2Endpoint)

// DecodeErrorHandler is the callback for an inbound data message whose SECS-II body failed to decode.
//
// The header accessors (Stream/Function/WaitBit/SessionID/ID) are valid; the body is not —
// do NOT call msg.Item() expecting success. err is the deferred decode error (equal to msg.DecodeErr()).
//
// A DecodeErrorHandler is invoked only for PRIMARY messages (and orphan secondaries that miss the reply registry).
// A malformed reply to a synchronous SendDataMessage / SendSECS2Message is surfaced to that call's error return instead.
//
// Like DataMessageHandler, a DecodeErrorHandler is invoked INLINE on the connection's single receive goroutine and MUST NOT BLOCK:
// while it runs, the receive loop cannot read the next frame and Close cannot complete until it returns.
// Offload slow work to your own goroutine.
type DecodeErrorHandler func(msg *DataMessage, err error, ep SECS2Endpoint)

// SECS2Endpoint provides the capability surface exposed to message handlers.
//
// It covers all blocking SECS-II send/reply operations plus handler registration.
type SECS2Endpoint interface {
	// SessionID returns the HSMS session ID (no I/O; never blocks).
	SessionID() uint16

	// SendDataMessage sends a primary SECS-II data message.
	//
	// When replyExpected is true, it waits for the matching secondary reply or a protocol/context timeout.
	//
	// function must be odd (SEMI E5 §7.2); an even function returns ErrEvenFunctionPrimary without sending anything.
	//
	// Returns the reply DataMessage on success.
	SendDataMessage(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) (*DataMessage, error)

	// SendDataMessageAsync enqueues a fire-and-forget data message.
	//
	// The message is enqueued on the per-generation async send channel.
	// The caller is not blocked waiting for a reply; errors are surfaced only at the enqueue boundary.
	//
	// function must be odd (SEMI E5 §7.2); an even function returns ErrEvenFunctionPrimary without sending anything.
	// replyExpected must be false: the async path never begins a reply timer (SEMI E37 §9.4.1.2),
	// so a reply-expecting send returns ErrAsyncReplyExpected without sending anything;
	// use SendDataMessage instead.
	SendDataMessageAsync(ctx context.Context, stream, function byte, replyExpected bool, item secs2.Item) error

	// SendSECS2Message sends a pre-constructed SECS-II message.
	//
	// It waits for the reply when the W-bit is set.
	//
	// msg's function must be odd (SEMI E5 §7.2); an even function returns ErrEvenFunctionPrimary without sending anything.
	//
	// Returns the reply DataMessage on success.
	SendSECS2Message(ctx context.Context, msg secs2.SECS2Message) (*DataMessage, error)

	// ForwardDataMessage writes a pre-built data message verbatim on the caller's goroutine.
	//
	// It preserves the message's System Bytes, W-bit, session ID, and stream/function,
	// and returns once the frame is on the wire.
	//
	// Unlike SendDataMessage it does NOT register a reply expectation: any secondary the peer sends misses the reply registry
	// and is delivered to the registered DataMessageHandlers instead of being returned here.
	//
	// Use this when YOU own reply correlation — a router or proxy bridging transports
	// that matches a reply to an upstream request by System Bytes.
	//
	// Ordinary request/reply code should use SendDataMessage.
	//
	// A W-bit-set msg keeps its W-bit on the wire (the peer is still asked to reply); only the LOCAL reply-wait is skipped.
	//
	// Returns ErrNilMessage if msg is nil.
	//
	// The caller owns System Bytes: they are sent verbatim and are NOT drawn from the connection's monotonic generator,
	// so they may collide with an in-flight SendDataMessage/SendSECS2Message transaction OR another forwarded message on the same connection.
	//
	// On a collision the peer's reply is routed to whichever waiter/handler the registry resolves — reply theft.
	//
	// Keep forwarded System Bytes disjoint from the library-generated space (which starts at 1 and increments).
	//
	// The message's session ID is sent verbatim on HSMS-SS.
	//
	// If the connection enables WithSessionIDValidation, a forwarded session ID that differs from the connection's makes the peer's reply fail inbound validation
	// and be dropped (and answered with an S9F1), so it never reaches your handlers —
	// forward with the connection's own session ID, or leave validation disabled.
	//
	// On SECS-I the block-header device ID is always the configured device ID regardless of msg's session ID (only System Bytes / stream / function / W-bit go on the wire verbatim).
	ForwardDataMessage(ctx context.Context, msg *DataMessage) error

	// ForwardDataMessageAsync enqueues a pre-built data message verbatim.
	//
	// The message is enqueued on the per-generation async send channel instead of writing on the caller's goroutine.
	// A write failure is best-effort (surfaced only at the enqueue boundary, not the wire).
	//
	// The System Bytes / session-ID caveats on ForwardDataMessage apply here too.
	//
	// Returns ErrNilMessage if msg is nil.
	ForwardDataMessageAsync(ctx context.Context, msg *DataMessage) error

	// ReplyDataMessage sends a secondary data message in reply to a primary message.
	//
	// The reply reuses the primary message's System Bytes verbatim (E37 §8.2.6.9) and carries no W-bit.
	ReplyDataMessage(ctx context.Context, primary *DataMessage, item secs2.Item) error

	// AddDataMessageHandler appends one or more inbound data message handlers.
	//
	// Registration is not blocking I/O and does not take a context.
	AddDataMessageHandler(handlers ...DataMessageHandler)

	// AddDecodeErrorHandler appends one or more handlers for inbound data messages whose SECS-II body fails to decode.
	//
	// When at least one is registered, an undecodable primary is delivered to these handlers instead of the normal DataMessageHandlers/channel handlers.
	// With none registered, decoding stays lazy and the message routes normally (today's behavior).
	//
	// Registration is not blocking I/O and does not take a context.
	AddDecodeErrorHandler(handlers ...DecodeErrorHandler)

	// AddConnStateChangeHandler appends one or more connection state-change handlers.
	//
	// Handlers persist across Open/Close cycles and are never removed.
	// Registration is not blocking I/O and does not take a context.
	AddConnStateChangeHandler(handlers ...StateChangeHandler)

	// AddDataMessageChan registers ch to receive every inbound data message this connection
	// fans out to DataMessageHandlers: primaries, plus orphan secondaries — a late reply that
	// missed T3's window, and any reply to a message sent with ForwardDataMessage, which does
	// not register a reply-wait.
	// Nothing is filtered out at registration; use [DataMessage.IsPrimary] to split the stream
	// into primaries and secondaries yourself.
	//
	// Delivery to ch happens on the connection's receive goroutine, the same goroutine that
	// invokes DataMessageHandler — see that type's must-not-block contract, which applies here
	// too.
	// A full ch stalls the receive loop, including control-frame processing, exactly like a
	// blocked func handler.
	// A persistently full ch therefore surfaces as a linktest/T6 failure, which drops the
	// connection; teardown then unblocks the stalled delivery, and the connection reconnects.
	// Size ch's buffer for your consumer's burst depth, and keep the consumer loop draining.
	// Close's teardown cancels the connection's per-generation ctx immediately, which unblocks a stalled channel delivery —
	// Close still returns promptly, dropping the in-flight message.
	// A blocked func handler has no such escape:
	// it is what actually burns the full close timeout and returns ErrCloseTimeout.
	//
	// Do not send synchronously (SendDataMessage with the wait bit) from the goroutine that
	// drains ch while ch is full — the reply cannot be routed until the receive loop unblocks,
	// so the send self-deadlocks until its own T3 timeout expires.
	//
	// Do not close ch.
	// The receive goroutine has no recover around the delivery send, so a send on a closed ch
	// panics there — the same exposure a panicking func handler has.
	// End your consumer loop with your own quit signal instead.
	//
	// Registering the same channel more than once delivers each message once per registration
	// (parity with DataMessageHandler; registration is not deduplicated).
	//
	// If the connection tears down mid-fan-out, channels later in the registration order can
	// miss the in-flight message — delivery is at-most-once, with no teardown flush.
	//
	// Registration is permanent for the connection's lifetime: there is no removal API, and ch
	// keeps receiving across Close/Open cycles, exactly like a registered DataMessageHandler.
	//
	// Panics if ch is nil — a nil channel would make every delivery select wait only on the
	// connection's teardown signal, wedging the connection from the first inbound message.
	//
	// Registration is not blocking I/O and does not take a context.
	AddDataMessageChan(ch chan *DataMessage)
}

// Connection is the app-facing handle for an HSMS connection.
//
// The concrete engine is unexported; hsmsss.New and secs1.New return this interface.
// Connection embeds SECS2Endpoint so callers can send/receive SECS-II messages and register handlers directly on the Connection value.
type Connection interface {
	SECS2Endpoint

	// Open starts the connection lifecycle (dial/listen + FSM).
	//
	// The mode selects blocking (OpenWaitSelected) or background (OpenBackground) behavior.
	// The ctx bounds the synchronous wait when mode is OpenWaitSelected.
	//
	// For an active connection under OpenBackground, a peer that is not yet reachable at Open time is not an error: Open returns nil
	// and the connection retries the initial connect in the background (see the (*connection).Open doc for the exact NotConnectedState-only scoping).
	Open(ctx context.Context, mode OpenMode) error

	// Close tears down the connection and all per-generation resources.
	//
	// Close is ctx-free and always completes teardown (internally bounded by the configured close timeout).
	//
	// It is idempotent: a second Close returns the prior error.
	Close() error

	// State returns the current FSM ConnState.
	//
	// Returns NotConnectedState when the connection has not been opened or has been closed.
	// Never nil-derefs.
	State() ConnState

	// Metrics returns the live connection metrics (atomic counters).
	Metrics() *ConnectionMetrics

	// UpdateConfigOptions applies functional options to the connection's live configuration.
	//
	// The options are applied transactionally (validate-all, then commit atomically).
	UpdateConfigOptions(opts ...ConnOption) error

	// SubscribeLifecycle registers fn to observe every connection state transition together with the cause that drove it,
	// and returns the function that cancels the subscription.
	//
	// This is the cancellable, cause-carrying counterpart to AddConnStateChangeHandler.
	// Use it when the observer is shorter-lived than the connection,
	// or when it must tell a local Close from a peer Separate, a T7 expiry, or a dropped socket without correlating logs afterwards.
	//
	// fn runs on the connection's notifier goroutine — the same goroutine, in the same order, as a StateChangeHandler —
	// so it must not block: a slow fn delays delivery to every other subscriber and handler.
	// A panic inside fn is isolated and never stops the other subscribers.
	//
	// LifecycleEvent.Cause names the event that drove the transition the connection actually reported.
	// Transitions are deduplicated on the state entered:
	// when a second event would land the connection in a state it is already in, nothing is reported for it.
	// A Close issued on a link that has already dropped therefore reports nothing —
	// the drop's earlier event already carried the cause, not CauseLocalClose.
	//
	// Delivery is best-effort under a subscriber that does not keep up:
	// intermediate transitions, and their causes, may be coalesced away, but the connection's latest state is always delivered.
	//
	// The subscription persists across Open/Close cycles until cancel is called.
	// cancel is idempotent and safe to call from any goroutine, including from inside fn.
	// It is not a delivery barrier:
	// an event already being dispatched may still reach fn, so keep fn safe to run once more after cancel returns.
	//
	// A nil fn registers nothing and returns a cancel that does nothing.
	//
	// Registration is not blocking I/O and does not take a context.
	SubscribeLifecycle(fn func(LifecycleEvent)) (cancel func())
}
