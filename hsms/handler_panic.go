package hsms

import (
	"errors"
	"runtime/debug"

	"github.com/arloliu/go-secs/v2/logger"
)

// Names for the "kind" argument every panic/Goexit report carries, identifying which callback
// site produced it.
const (
	kindDataMessageHandler    = "DataMessageHandler"
	kindDecodeErrorHandler    = "DecodeErrorHandler"
	kindStateChangeHandler    = "StateChangeHandler"
	kindLifecycleSubscriber   = "LifecycleSubscriber"
	kindAsyncSendErrorHandler = "AsyncSendErrorHandler"
)

// errHandlerGoexit is the TCPDownFromGeneration cause paired with CauseHandlerExit.
// It never leaves the package: TransitionCause is the classification a caller observes
// (see CauseHandlerExit), and this sentinel exists only to satisfy TCPDownFromGeneration's error parameter.
var errHandlerGoexit = errors.New("hsms: a user callback called runtime.Goexit instead of returning")

// handlerHost is the narrow capability [session]'s panic-recovering callback call sites —
// callDataHandler and callDecodeErrorHandler — need from their connection:
// the shared metrics counter, the live-config logger,
// and the hook that disconnects the generation a callback ran for when the callback called runtime.Goexit instead of returning.
// *connection satisfies it (asserted below).
//
// A session built over a runtime that does not — every in-package mock TransportRuntime a
// session-level unit test constructs, and a session built with a nil rt —
// falls back to calling the callback directly, unrecovered:
// there is no connection to count the panic against, log it through, or disconnect on its behalf.
type handlerHost interface {
	handlerMetrics() *ConnectionMetrics
	handlerLogger() logger.Logger
	disconnectHandlerGeneration(gen uint64)
}

var _ handlerHost = (*connection)(nil)

// runCallback runs fn and reports how it ended: a normal return, a recovered panic, or a
// runtime.Goexit.
// It is the panic/Goexit isolation shared by every user callback the connection invokes on its own goroutines —
// DataMessageHandler, DecodeErrorHandler, StateChangeHandler, a SubscribeLifecycle callback, and the async send error handler.
//
// onPanic is called with the recovered value when fn panics and that panic is not itself
// recovered inside fn; it is never called for a panic fn recovers on its own.
// onGoexit is called when fn ends via runtime.Goexit instead of returning, including when a
// deferred call raises a panic WHILE that Goexit is unwinding fn — Go resumes the Goexit once
// that panic is recovered, so both onPanic (for the nested panic) and onGoexit (for the Goexit it
// interrupted) fire in that case.
// A plain panic never also reports a Goexit, and a plain Goexit never also reports a panic.
//
// runCallback itself never panics and never calls runtime.Goexit,
// so the goroutine running it always leaves in one of these three ways, never a fourth.
func runCallback(fn func(), onPanic func(r any), onGoexit func()) {
	returned := false
	defer func() {
		if !returned {
			// The inner call below never returned normally: fn either called runtime.Goexit
			// directly, or a deferred call did so while its own panic was being recovered — either
			// way, this goroutine is still unwinding via Goexit once this defer completes.
			onGoexit()
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				// Also covers a panic a deferred call raises WHILE a Goexit fn started is
				// unwinding: recover() still catches it here, and Go resumes the interrupted
				// Goexit once this deferred function returns.
				onPanic(r)
			}
		}()
		fn()
	}()

	returned = true
}

// countHandlerPanic increments the shared HandlerPanicCount and logs the recovered panic at
// Error, with its stack.
func countHandlerPanic(m *ConnectionMetrics, log logger.Logger, kind string, r any) {
	m.incHandlerPanic()
	logHandlerEvent(log, "hsms: "+kind+" panicked; recovered", "panic", r, "stack", string(debug.Stack()))
}

// logHandlerGoexit logs that kind called runtime.Goexit instead of returning.
// Every call site invokes this AFTER its own generation-disconnect action (where one applies),
// so a logger that itself calls runtime.Goexit cannot be recovered here and skip that disconnect —
// see the call sites in session.go, connection_send.go, and supervisor.go.
func logHandlerGoexit(log logger.Logger, kind string) {
	logHandlerEvent(log, "hsms: "+kind+" called runtime.Goexit; the goroutine running it is exiting")
}

// logHandlerEvent is the guarded log call every panic/Goexit report funnels through: its own
// deferred recover keeps a panicking user-supplied logger from escaping into — and crashing — the
// goroutine running the report.
func logHandlerEvent(log logger.Logger, msg string, keysAndValues ...any) {
	defer func() { _ = recover() }()
	log.Error(msg, keysAndValues...)
}

// handlerMetrics returns the connection's shared metrics counter (handlerHost).
func (c *connection) handlerMetrics() *ConnectionMetrics {
	return &c.metrics
}

// handlerLogger returns the connection's CURRENT configured logger (handlerHost), read fresh on
// every call so a callback report always uses the live config rather than one snapshotted earlier.
func (c *connection) handlerLogger() logger.Logger {
	return c.cfg.Load().logger
}

// disconnectHandlerGeneration reports a Goexit-forced drop of gen, the generation a callback was
// running for (handlerHost).
//
// gen == 0 is a no-op.
// Zero means the injection site named no generation, and [connection.TCPDownFromGeneration] treats
// 0 as a wildcard — "disconnect whatever generation is current" — not "nothing to disconnect".
// That wildcard is exactly wrong for a Goexit report, which must target ONLY the generation the
// exiting callback actually ran for, never whatever happens to be current when it finally returns.
//
// It is also a no-op once gen is no longer the live generation — see [connection.TCPDownFromGeneration].
//
// The call runs under its own recover.
// TCPDownFromGeneration's stale-generation branch (connection.injectDisconnect) logs through the
// user-supplied logger, and a logger that panics there must not escape this report and crash a
// goroutine that is already unwinding via runtime.Goexit.
// When gen IS still live, the disconnect action itself always completes before any log call it
// could reach, so this recover changes nothing about that ordering — it only keeps a panicking
// logger from turning a Goexit report into a process crash.
func (c *connection) disconnectHandlerGeneration(gen uint64) {
	if gen == 0 {
		return
	}

	defer func() { _ = recover() }()
	c.TCPDownFromGeneration(gen, errHandlerGoexit, CauseHandlerExit)
}
