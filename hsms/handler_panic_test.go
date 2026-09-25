package hsms

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/logger/loggertest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// ── runCallback (unit) ───────────────────────────────────────────────────────

// callbackReports counts how many times onPanic / onGoexit fired for one runCallback case.
type callbackReports struct {
	panics  atomic.Int32
	goexits atomic.Int32
}

// TestRunCallback_NormalReturn proves a callback that returns normally triggers neither report.
func TestRunCallback_NormalReturn(t *testing.T) {
	t.Parallel()

	r := &callbackReports{}
	waitRunCallback(t, r, func() {})

	require.Equal(t, int32(0), r.panics.Load())
	require.Equal(t, int32(0), r.goexits.Load())
}

// TestRunCallback_SelfRecoveredPanic proves a panic fn recovers ITSELF never reaches runCallback's
// own reports: recovering inside fn is indistinguishable from fn returning normally.
func TestRunCallback_SelfRecoveredPanic(t *testing.T) {
	t.Parallel()

	r := &callbackReports{}
	waitRunCallback(t, r, func() {
		defer func() { _ = recover() }()
		panic("recovered by fn itself")
	})

	require.Equal(t, int32(0), r.panics.Load())
	require.Equal(t, int32(0), r.goexits.Load())
}

// TestRunCallback_PlainPanic proves an unrecovered panic inside fn is reported exactly once via
// onPanic and never as a Goexit.
//
// An unrecovered panic on any goroutine crashes the process,
// so it must be run in isolation (go test -run TestRunCallback_PlainPanic)
// to observe that crash rather than losing it among other subtests.
func TestRunCallback_PlainPanic(t *testing.T) {
	t.Parallel()

	r := &callbackReports{}
	waitRunCallback(t, r, func() { panic("plain panic") })

	require.Equal(t, int32(1), r.panics.Load())
	require.Equal(t, int32(0), r.goexits.Load())
}

// TestRunCallback_PlainGoexit proves runtime.Goexit is reported via onGoexit and never counted as a panic.
// Goexit alone never crashes a goroutine — every deferred call still runs —
// so without the outer Goexit-detecting frame this test fails by assertion (onGoexit never called), not by a crash.
func TestRunCallback_PlainGoexit(t *testing.T) {
	t.Parallel()

	r := &callbackReports{}
	waitRunCallback(t, r, func() { runtime.Goexit() })

	require.Equal(t, int32(0), r.panics.Load())
	require.Equal(t, int32(1), r.goexits.Load())
}

// TestRunCallback_GoexitWithDeferredPanic proves the two-frame design: a deferred function that
// panics WHILE a Goexit is unwinding fn is recovered and counted once,
// and the Goexit it interrupted is still reported once that recovery completes —
// Go resumes the Goexit after a deferred call recovers a panic raised during its own unwind.
//
// An unrecovered nested panic here also crashes the process (see TestRunCallback_PlainPanic),
// so this case must likewise run in isolation.
func TestRunCallback_GoexitWithDeferredPanic(t *testing.T) {
	t.Parallel()

	r := &callbackReports{}
	waitRunCallback(t, r, func() {
		defer panic("nested panic raised while Goexit unwinds")
		runtime.Goexit()
	})

	require.Equal(t, int32(1), r.panics.Load())
	require.Equal(t, int32(1), r.goexits.Load())
}

// waitRunCallback runs fn through runCallback on its own goroutine, recording every report on r,
// and fails the test if that goroutine does not finish within a bounded wait.
func waitRunCallback(t *testing.T, r *callbackReports, fn func()) {
	t.Helper()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runCallback(fn, func(any) { r.panics.Add(1) }, func() { r.goexits.Add(1) })
	}()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("the goroutine running runCallback never returned")
	}
}

// ── DataMessageHandler / DecodeErrorHandler panics ──────────────────────────

// TestDataMessageHandler_PanicRecoveredFanOutContinues proves a panicking DataMessageHandler is
// recovered, counted, and logged with its stack, and that the rest of the fan-out —
// the next handler and a registered channel — still receives the message.
// The connection stays Selected and keeps receiving afterward.
func TestDataMessageHandler_PanicRecoveredFanOutContinues(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	var second atomic.Bool
	c.AddDataMessageHandler(
		func(*DataMessage, SECS2Endpoint) { panic("handler one blew up") },
		func(*DataMessage, SECS2Endpoint) { second.Store(true) },
	)
	ch := make(chan *DataMessage, 1)
	c.AddDataMessageChan(ch)

	msg := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg)))

	require.True(t, second.Load(), "the second handler must still run after the first panicked")
	select {
	case got := <-ch:
		require.Equal(t, msg.SystemBytes(), got.SystemBytes())
	default:
		t.Fatal("the registered channel must still receive the message")
	}

	require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	assertLoggedPanic(t, mockLog, "handler one blew up")

	msg2 := mustSendData(t, [4]byte{0, 0, 0, 2}, false)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg2)))
	select {
	case got := <-ch:
		require.Equal(t, msg2.SystemBytes(), got.SystemBytes())
	default:
		t.Fatal("a later message must still be delivered after the panic")
	}
}

// TestDecodeErrorHandler_PanicRecoveredFanOutContinues proves a panicking DecodeErrorHandler is
// recovered and counted, the second decode-error handler still runs, and the undecodable message
// stays diverted — it does not also reach the normal handlers or channels (unchanged from the
// non-panicking case).
func TestDecodeErrorHandler_PanicRecoveredFanOutContinues(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	var second atomic.Bool
	c.AddDecodeErrorHandler(
		func(*DataMessage, error, SECS2Endpoint) { panic("decode handler one blew up") },
		func(*DataMessage, error, SECS2Endpoint) { second.Store(true) },
	)
	var normalCalled atomic.Bool
	c.AddDataMessageHandler(func(*DataMessage, SECS2Endpoint) { normalCalled.Store(true) })
	ch := make(chan *DataMessage, 1)
	c.AddDataMessageChan(ch)

	bad := malformedDataMsg(t, 6, 11)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, bad)))

	require.True(t, second.Load(), "the second decode-error handler must still run after the first panicked")
	require.False(t, normalCalled.Load(), "an undecodable message must not also reach the normal data handler")
	select {
	case <-ch:
		t.Fatal("an undecodable message must not also reach a registered channel")
	default:
	}

	require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	assertLoggedPanic(t, mockLog, "decode handler one blew up")
}

// assertLoggedPanic asserts mockLog recorded an Error call whose key/value pairs carry want (the
// panic value) under the "panic" key and a non-empty string "stack" entry.
func assertLoggedPanic(t *testing.T, mockLog *loggertest.MockLogger, want any) {
	t.Helper()

	for _, call := range mockLog.Calls {
		if call.Method != "Error" {
			continue
		}

		keyvals, ok := call.Arguments[1].([]any)
		require.True(t, ok, "Error's variadic keysAndValues must be forwarded as a []any slice")
		if !containsPair(keyvals, "panic", want) {
			continue
		}

		stack, ok := pairValue(keyvals, "stack")
		require.True(t, ok, "the panic log must carry a stack entry")
		stackStr, ok := stack.(string)
		require.True(t, ok, "the stack entry must be a string")
		require.NotEmpty(t, stackStr, "the stack entry must not be empty")

		return
	}

	t.Fatalf("no Error log carried panic=%v with a non-empty stack", want)
}

// assertLoggedGoexit asserts mockLog recorded an Error call whose message names runtime.Goexit —
// the [logHandlerGoexit] report — and that no Error call's message reports a panic, proving a
// Goexit is never also counted or logged as one.
func assertLoggedGoexit(t *testing.T, mockLog *loggertest.MockLogger) {
	t.Helper()

	var sawGoexit bool
	for _, call := range mockLog.Calls {
		if call.Method != "Error" {
			continue
		}

		msg, ok := call.Arguments[0].(string)
		require.True(t, ok, "Error's first argument must be the log message string")
		require.NotContains(t, msg, "panicked", "a Goexit must never also be logged as a panic")
		if strings.Contains(msg, "runtime.Goexit") {
			sawGoexit = true
		}
	}

	require.True(t, sawGoexit, "no Error log named runtime.Goexit")
}

// containsPair reports whether keyvals (an alternating key, value, key, value, ... slice) holds
// key immediately followed by value.
func containsPair(keyvals []any, key string, value any) bool {
	for i := 0; i+1 < len(keyvals); i += 2 {
		if keyvals[i] == key && keyvals[i+1] == value {
			return true
		}
	}

	return false
}

// pairValue returns the value immediately following key in keyvals (an alternating key, value,
// key, value, ... slice), and whether key was found at all.
func pairValue(keyvals []any, key string) (any, bool) {
	for i := 0; i+1 < len(keyvals); i += 2 {
		if keyvals[i] == key {
			return keyvals[i+1], true
		}
	}

	return nil, false
}

// ── StateChangeHandler / SubscribeLifecycle panics ──────────────────────────

// TestStateChangeHandler_PanicRecoveredNotifierContinues proves a panicking StateChangeHandler is
// recovered, counted, and logged, the next handler still runs for that SAME Selected transition
// (not merely for some later, unrelated transition), and the terminal transition (Close) is still
// delivered afterward.
func TestStateChangeHandler_PanicRecoveredNotifierContinues(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	var selected, terminal atomic.Bool
	c.AddConnStateChangeHandler(
		func(_, next ConnState) {
			if next == SelectedState {
				panic("state handler blew up")
			}
		},
		func(_, next ConnState) {
			switch next { //nolint:exhaustive // NotSelectedState is an intermediate transition, not under test
			case SelectedState:
				selected.Store(true)
			case NotConnectedState:
				terminal.Store(true)
			default:
			}
		},
	)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	require.Eventually(t, selected.Load, 2*time.Second, time.Millisecond,
		"the second handler must still run for the Selected transition after the first panicked")

	require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	assertLoggedPanic(t, mockLog, "state handler blew up")

	require.NoError(t, c.Close())
	require.Eventually(t, terminal.Load, 2*time.Second, time.Millisecond,
		"the terminal transition must still be delivered")
}

// TestLifecycleSubscriber_PanicRecoveredNotifierContinues mirrors
// TestStateChangeHandler_PanicRecoveredNotifierContinues for a SubscribeLifecycle callback.
func TestLifecycleSubscriber_PanicRecoveredNotifierContinues(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	var selected, terminal atomic.Bool
	c.SubscribeLifecycle(func(ev LifecycleEvent) {
		if ev.Current == SelectedState {
			panic("lifecycle subscriber blew up")
		}
	})
	c.SubscribeLifecycle(func(ev LifecycleEvent) {
		switch ev.Current { //nolint:exhaustive // NotSelectedState is an intermediate transition, not under test
		case SelectedState:
			selected.Store(true)
		case NotConnectedState:
			terminal.Store(true)
		default:
		}
	})

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	require.Eventually(t, selected.Load, 2*time.Second, time.Millisecond,
		"the second subscriber must still run for the Selected transition after the first panicked")

	require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	assertLoggedPanic(t, mockLog, "lifecycle subscriber blew up")

	require.NoError(t, c.Close())
	require.Eventually(t, terminal.Load, 2*time.Second, time.Millisecond,
		"the terminal transition must still be delivered")
}

// TestHandlerPanicReport_PanickingLoggerDoesNotCrashGoroutine proves the guard around the log
// call itself: a configured logger that panics while reporting a recovered callback panic does
// not escape and crash the goroutine running the report, and the panic is still counted.
func TestHandlerPanicReport_PanickingLoggerDoesNotCrashGoroutine(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())

	panicLog := loggertest.NewMockLogger()
	panicLog.On("Error", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { panic("logger blew up") }). //nolint:forbidigo // proving the guarded log call survives a panicking logger
		Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(panicLog)))

	var second atomic.Bool
	c.AddConnStateChangeHandler(
		func(_, next ConnState) {
			if next == SelectedState {
				panic("state handler blew up")
			}
		},
		func(_, next ConnState) {
			if next == SelectedState {
				second.Store(true)
			}
		},
	)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	require.Eventually(t, second.Load, 2*time.Second, time.Millisecond,
		"the notifier goroutine must survive a panicking logger and keep delivering")
	require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount(),
		"the panic must still be counted even though logging it panicked")

	require.NoError(t, c.Close())
}

// ── Async send error handler panic ───────────────────────────────────────────

// TestAsyncSendErrorHandler_PanicRecoveredSenderContinues proves a panicking async send error
// handler, triggered by a NON-fatal oversized-message error, is recovered, counted, and logged —
// and that a later async send on the SAME generation still reaches the wire, proving the original
// sender goroutine kept draining rather than being replaced.
func TestAsyncSendErrorHandler_PanicRecoveredSenderContinues(t *testing.T) {
	mockLog := loggertest.NewMockLogger()
	logged := make(chan struct{})
	mockLog.On("Error", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { close(logged) }). // signals AFTER the call is recorded, under the mock's own lock
		Return()

	var handlerCalls atomic.Int32
	c, tr := newTestSendConn(t, SelectedState,
		WithLogger(mockLog),
		WithAsyncSendErrorHandler(func(Message, error) {
			handlerCalls.Add(1)
			panic("async send error handler blew up")
		}),
	)
	e := c.cur.Load()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go c.drainSendCh(ctx, e)

	oversized := fakeBodyDataMessage(t, maxHSMSMsgLen-10+1)
	require.NoError(t, c.SendAsync(t.Context(), oversized), "enqueue succeeds; the size check runs in the sender")

	require.Eventually(t, func() bool { return handlerCalls.Load() == 1 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return c.Metrics().HandlerPanicCount() == 1 }, 2*time.Second, time.Millisecond)

	// Wait for the log call itself (a channel receive, not a direct read of the mock's internal
	// call log) before asserting on mockLog: the sender goroutine that recorded the call, and this
	// goroutine reading it back, would otherwise race on the mock's own bookkeeping.
	select {
	case <-logged:
	case <-time.After(2 * time.Second):
		t.Fatal("the panic was never logged at Error")
	}
	assertLoggedPanic(t, mockLog, "async send error handler blew up")

	wellFormed := mustSendData(t, [4]byte{0, 0, 0, 20}, false)
	require.NoError(t, c.SendAsync(t.Context(), wellFormed))
	require.Eventually(t, func() bool { return tr.writeCount() == 1 }, 2*time.Second, time.Millisecond,
		"a later async send on the same generation must still reach the wire")
}

// ── Close called from a handler that then panics ─────────────────────────────

// TestHandlerPanic_CloseThenPanicDropsRemainingFanOut covers a handler that calls Close (which
// times out — Close waits for the receive goroutine, and this handler IS that goroutine) and then
// panics.
// The panic is recovered and counted, but the generation's cancellation is already observed by
// then, so a later handler and a registered channel do not receive the frame — the SAME
// observed-cancellation contract that already applies to a handler that merely blocks.
//
// The transport's Stop actually joins the goroutine running the handler (armHeldRouteData), so
// the close-timeout contract is exercised rather than simulated.
func TestHandlerPanic_CloseThenPanicDropsRemainingFanOut(t *testing.T) {
	t.Run("data", func(t *testing.T) {
		c := newShortCloseConn(t, withMockTransport())
		mt, ok := c.tr.(*mockTransport)
		require.True(t, ok)

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		var second atomic.Bool
		closeErrCh := make(chan error, 1)
		c.AddDataMessageHandler(
			func(*DataMessage, SECS2Endpoint) {
				closeErrCh <- c.Close()
				panic("handler A blew up after closing")
			},
			func(*DataMessage, SECS2Endpoint) { second.Store(true) },
		)
		ch := make(chan *DataMessage, 1)
		c.AddDataMessageChan(ch)

		msg := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
		mt.armHeldRouteData(c, msg)

		require.ErrorIs(t, waitErr(t, closeErrCh, "Close called from inside the handler running it"), ErrCloseTimeout,
			"Close called from inside the handler running it must time out")
		waitHeldRouteData(t, mt)

		require.False(t, second.Load(), "a later handler must not see a frame whose delivery a panicking predecessor started tearing down")
		select {
		case <-ch:
			t.Fatal("a registered channel must not receive the frame either")
		default:
		}
		require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	})

	t.Run("decode-error", func(t *testing.T) {
		c := newShortCloseConn(t, withMockTransport())
		mt, ok := c.tr.(*mockTransport)
		require.True(t, ok)

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		var second atomic.Bool
		closeErrCh := make(chan error, 1)
		c.AddDecodeErrorHandler(
			func(*DataMessage, error, SECS2Endpoint) {
				closeErrCh <- c.Close()
				panic("decode handler A blew up after closing")
			},
			func(*DataMessage, error, SECS2Endpoint) { second.Store(true) },
		)

		bad := malformedDataMsg(t, 6, 11)
		mt.armHeldRouteData(c, bad)

		require.ErrorIs(t, waitErr(t, closeErrCh, "Close called from inside the decode-error handler running it"), ErrCloseTimeout)
		waitHeldRouteData(t, mt)

		require.False(t, second.Load())
		require.Equal(t, uint64(1), c.Metrics().HandlerPanicCount())
	})
}

// waitErr receives one error from ch, failing the test with what if nothing arrives within a
// bounded wait — never a bare, unbounded channel receive.
func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: timed out waiting for a result", what)
		return nil
	}
}

// waitClosed waits for ch to close (a struct{} signal channel), failing the test with what if it
// does not within a bounded wait — never a bare, unbounded channel receive.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: timed out waiting", what)
	}
}

// waitHeldRouteData waits for the goroutine armHeldRouteData started to finish running the
// panicking handler's fan-out and return from RouteData.
func waitHeldRouteData(t *testing.T, mt *mockTransport) {
	t.Helper()

	mt.mu.Lock()
	exited := mt.heldRouteDataExited
	mt.mu.Unlock()

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the goroutine running the panicking handler never returned from RouteData")
	}
}

// ── runtime.Goexit from a callback ───────────────────────────────────────────

// fastReconnectOpts shortens the reconnect backoff and T5 so a test driving a real disconnect ->
// reconnect cycle does not wait out the production defaults.
func fastReconnectOpts() []ConnOption {
	return []ConnOption{
		WithReconnectBackoff(5*time.Millisecond, 1.0),
		WithT5(50 * time.Millisecond),
	}
}

// TestDataMessageHandler_GoexitDisconnectsGenerationAndReconnects proves a DataMessageHandler
// that calls runtime.Goexit instead of returning is logged (not counted as a panic), and that the
// generation it was running for is dropped with CauseHandlerExit rather than left Selected with
// no receiver.
// The link then reconnects, and a later message is delivered normally.
func TestDataMessageHandler_GoexitDisconnectsGenerationAndReconnects(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(fastReconnectOpts()...))

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	log := &eventLog{}
	subscribeLog(t, c, log)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	var claimed atomic.Bool
	delivered := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		if claimed.CompareAndSwap(false, true) {
			runtime.Goexit()
		}
		delivered <- msg
	})

	firstGen := c.cur.Load()
	msg1 := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
	exitedCh := make(chan struct{})
	go func() {
		defer close(exitedCh)
		_ = c.DeliverOwnedFrame(ownedFrame(t, msg1))
	}()
	waitClosed(t, exitedCh, "the goroutine running the Goexiting handler never returned")

	// Found by content (the cause), not position: a Selected notification that lands after
	// State() already reports Selected would otherwise shift a positional index onto the wrong row.
	disconnect := log.requireHandlerExitCause(t)
	require.Equal(t, SelectedState, disconnect.prev)
	require.Equal(t, NotConnectedState, disconnect.next)

	require.Equal(t, uint64(0), c.Metrics().HandlerPanicCount(), "a Goexit is not a panic")
	assertLoggedGoexit(t, mockLog)

	requireSelected(t, c) // the link reconnects
	require.NotEqual(t, firstGen.id, c.cur.Load().id, "the reconnect must publish a fresh generation")

	msg2 := mustSendData(t, [4]byte{0, 0, 0, 2}, false)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg2)))
	select {
	case got := <-delivered:
		require.Equal(t, msg2.SystemBytes(), got.SystemBytes())
	case <-time.After(3 * time.Second):
		t.Fatal("a message must be delivered normally after the reconnect")
	}
}

// TestDecodeErrorHandler_GoexitDisconnectsGenerationAndReconnects mirrors
// TestDataMessageHandler_GoexitDisconnectsGenerationAndReconnects for a DecodeErrorHandler.
func TestDecodeErrorHandler_GoexitDisconnectsGenerationAndReconnects(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(fastReconnectOpts()...))

	mockLog := loggertest.NewMockLogger()
	mockLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

	log := &eventLog{}
	subscribeLog(t, c, log)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	var claimed atomic.Bool
	delivered := make(chan *DataMessage, 1)
	c.AddDecodeErrorHandler(func(msg *DataMessage, _ error, _ SECS2Endpoint) {
		if claimed.CompareAndSwap(false, true) {
			runtime.Goexit()
		}
		delivered <- msg
	})

	bad := malformedDataMsg(t, 6, 11)
	exitedCh := make(chan struct{})
	go func() {
		defer close(exitedCh)
		_ = c.DeliverOwnedFrame(ownedFrame(t, bad))
	}()
	waitClosed(t, exitedCh, "the goroutine running the Goexiting decode-error handler never returned")

	disconnect := log.requireHandlerExitCause(t)
	require.Equal(t, SelectedState, disconnect.prev)
	require.Equal(t, NotConnectedState, disconnect.next)
	require.Equal(t, uint64(0), c.Metrics().HandlerPanicCount())
	assertLoggedGoexit(t, mockLog)

	requireSelected(t, c) // the link reconnects

	bad2 := malformedDataMsg(t, 6, 11)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, bad2)))
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("a decode-error message must be delivered normally after the reconnect")
	}
}

// TestAsyncSendErrorHandler_GoexitDisconnectsGeneration proves an async send error handler that
// calls runtime.Goexit — reached through the SAME non-fatal ErrMessageTooLarge trigger test 5
// uses, so nothing else disconnects the link — drops the generation with CauseHandlerExit and the
// connection reconnects.
func TestAsyncSendErrorHandler_GoexitDisconnectsGeneration(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(fastReconnectOpts()...))

	mockLog := loggertest.NewMockLogger()
	logged := make(chan struct{})
	mockLog.On("Error", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { close(logged) }). // signals AFTER the call is recorded, under the mock's own lock
		Return()

	var claimed atomic.Bool
	require.NoError(t, c.UpdateConfigOptions(
		WithLogger(mockLog),
		WithAsyncSendErrorHandler(func(Message, error) {
			if claimed.CompareAndSwap(false, true) {
				runtime.Goexit()
			}
		}),
	))

	log := &eventLog{}
	subscribeLog(t, c, log)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	oversized := fakeBodyDataMessage(t, maxHSMSMsgLen-10+1)
	require.NoError(t, c.SendAsync(t.Context(), oversized))

	// Wait for the log call itself before reading mockLog back — the async sender goroutine that
	// recorded the call, and this goroutine reading it, would otherwise race on the mock's own
	// bookkeeping (the same discipline test 5's panic counterpart uses).
	waitClosed(t, logged, "the Goexit was never logged at Error")

	disconnect := log.requireHandlerExitCause(t)
	require.Equal(t, SelectedState, disconnect.prev)
	require.Equal(t, NotConnectedState, disconnect.next)
	require.Equal(t, uint64(0), c.Metrics().HandlerPanicCount())
	assertLoggedGoexit(t, mockLog)

	requireSelected(t, c) // the link reconnects
}

// TestStateChangeHandlerAndLifecycleSubscriber_GoexitLogsOnlyCloseStillReturns proves a
// StateChangeHandler or lifecycle subscriber that calls runtime.Goexit is logged but not counted,
// and — since there is no per-generation transport to drop for a notifier-side callback — Close
// still completes normally rather than reporting ErrCloseTimeout.
// The notifier goroutine itself ends with that Goexit (it runs the callback inline), so no later
// notification for this Open is delivered; notifierDone still closes via its own defer.
func TestStateChangeHandlerAndLifecycleSubscriber_GoexitLogsOnlyCloseStillReturns(t *testing.T) {
	t.Run("state-change-handler", func(t *testing.T) {
		c, _ := newLifeConn(t, withMockTransport())

		mockLog := loggertest.NewMockLogger()
		mockLog.On("Error", mock.Anything, mock.Anything).Return()
		require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

		var claimed atomic.Bool
		c.AddConnStateChangeHandler(func(_, next ConnState) {
			if next == SelectedState && claimed.CompareAndSwap(false, true) {
				runtime.Goexit()
			}
		})

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		s := c.sup.Load()
		require.Eventually(t, func() bool { return notifierFinished(s) }, 3*time.Second, time.Millisecond,
			"the notifier goroutine must exit cleanly, not crash, once its callback Goexits")

		require.Equal(t, uint64(0), c.Metrics().HandlerPanicCount())
		assertLoggedGoexit(t, mockLog)

		require.NoError(t, c.Close(), "Close must still complete normally, not report ErrCloseTimeout")
	})

	t.Run("lifecycle-subscriber", func(t *testing.T) {
		c, _ := newLifeConn(t, withMockTransport())

		mockLog := loggertest.NewMockLogger()
		mockLog.On("Error", mock.Anything, mock.Anything).Return()
		require.NoError(t, c.UpdateConfigOptions(WithLogger(mockLog)))

		var claimed atomic.Bool
		c.SubscribeLifecycle(func(ev LifecycleEvent) {
			if ev.Current == SelectedState && claimed.CompareAndSwap(false, true) {
				runtime.Goexit()
			}
		})

		require.NoError(t, c.Open(t.Context(), OpenBackground))
		requireSelected(t, c)

		s := c.sup.Load()
		require.Eventually(t, func() bool { return notifierFinished(s) }, 3*time.Second, time.Millisecond)

		require.Equal(t, uint64(0), c.Metrics().HandlerPanicCount())
		assertLoggedGoexit(t, mockLog)

		require.NoError(t, c.Close())
	})
}

// TestDataMessageHandler_GoexitOldGenerationDoesNotDisconnectSuccessor proves the disconnect a
// Goexiting handler reports targets the generation the handler was running FOR, threaded in from
// where it was admitted —
// never re-resolved from whatever generation is current when the handler finally exits.
// A handler gated past its own generation's teardown and a reconnect must not disconnect the successor it never ran on.
func TestDataMessageHandler_GoexitOldGenerationDoesNotDisconnectSuccessor(t *testing.T) {
	c, genN, genSucc := newTwoGenConn(t)
	sup := c.sup.Load()

	entered := make(chan struct{})
	release := make(chan struct{})
	c.AddDataMessageHandler(func(*DataMessage, SECS2Endpoint) {
		close(entered)
		<-release
		runtime.Goexit()
	})

	msg := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
	exitedCh := make(chan struct{})
	go func() {
		defer close(exitedCh)
		_ = c.deliverOwnedFrameOn(genN, ownedFrame(t, msg))
	}()

	waitClosed(t, entered, "the Goexiting handler was never entered")
	// genN ends and genSucc is published WHILE the handler is still running for genN, mirroring a
	// reconnect that completes before a handler admitted by the old generation finishes running.
	endAndSucceed(c, genN, genSucc)
	close(release)

	waitClosed(t, exitedCh, "the goroutine running the Goexiting handler never returned")

	require.False(t, genSucc.commsFailure.Load(), "the successor generation must not be marked as a comms failure")
	require.Equal(t, uint64(1), sup.staleGen.Load(), "the disconnect report must be discarded as stale rather than applied to the successor")
}

// TestDisconnectHandlerGeneration_ZeroIsNoOp proves gen == 0 is treated as "no generation to
// disconnect", NOT [connection.TCPDownFromGeneration]'s ordinary "0 is a wildcard" convention —
// which would otherwise disconnect whatever generation happens to be current, exactly wrong for a
// Goexit report that must target only the generation the exiting callback actually ran for.
//
// Teeth: removing the `gen == 0` guard in disconnectHandlerGeneration makes commsFailure flip
// synchronously and this test fails.
func TestDisconnectHandlerGeneration_ZeroIsNoOp(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	c.disconnectHandlerGeneration(0)

	// injectDisconnect marks commsFailure SYNCHRONOUSLY, before it ever queues the disconnect event
	// (connection_lifecycle.go), so this is a deterministic check, not a race against the supervisor.
	require.False(t, c.cur.Load().commsFailure.Load(), "gen == 0 must not mark the current generation as a comms failure")
	require.Equal(t, SelectedState, c.State())
}

// TestDataMessageHandler_GoexitOldGenerationLoggerPanicDoesNotCrash proves the report path for a
// STALE generation is immune to a panicking logger: injectDisconnect's "reported generation has
// ended" branch (connection_lifecycle.go) logs at Debug through the user-supplied logger, and that
// call is reached from a goroutine that is already unwinding via runtime.Goexit — a panic there
// must not escape and crash the process.
//
// Teeth: removing disconnectHandlerGeneration's recover guard crashes this test.
func TestDataMessageHandler_GoexitOldGenerationLoggerPanicDoesNotCrash(t *testing.T) {
	c, genN, genSucc := newTwoGenConn(t)
	sup := c.sup.Load()

	entered := make(chan struct{})
	release := make(chan struct{})
	c.AddDataMessageHandler(func(*DataMessage, SECS2Endpoint) {
		close(entered)
		<-release
		runtime.Goexit()
	})

	msg := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
	exitedCh := make(chan struct{})
	go func() {
		defer close(exitedCh)
		_ = c.deliverOwnedFrameOn(genN, ownedFrame(t, msg))
	}()

	waitClosed(t, entered, "the Goexiting handler was never entered")
	endAndSucceed(c, genN, genSucc)

	// Installed only NOW, after setup: newTwoGenConn/endAndSucceed drive no Debug logging of their
	// own (state is set directly on the supervisor's atomic, never through commitGate/selectGate),
	// so this scopes the panic to exactly the stale-generation report under test, not any incidental
	// Debug call from unrelated machinery.
	panicLog := loggertest.NewMockLogger()
	panicLog.On("Debug", "hsms: dropped a disconnect reported by a generation that has ended", mock.Anything).
		Run(func(mock.Arguments) { panic("logger blew up") }). //nolint:forbidigo // proving the guard survives a panicking logger
		Return()
	panicLog.On("Error", mock.Anything, mock.Anything).Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(panicLog)))

	close(release)

	waitClosed(t, exitedCh, "the goroutine running the Goexiting handler never returned")

	require.False(t, genSucc.commsFailure.Load(), "the successor generation must not be marked as a comms failure")
	require.Equal(t, uint64(1), sup.staleGen.Load(), "the disconnect report must still be counted as stale even though logging it panicked")
}

// TestDataMessageHandler_GoexitLoggerGoexitStillDisconnects proves the disconnect-before-log
// ordering the onGoexit closures rely on (session.go, connection_send.go): a configured logger
// whose Error call itself calls runtime.Goexit cannot be recovered — Goexit is not a panic — so the
// only thing that keeps the generation drop from being silently skipped is that
// disconnectHandlerGeneration runs BEFORE the log call, never after.
//
// Teeth: swapping the onGoexit closure's call order to log-then-disconnect makes this test fail,
// because the logger's own Goexit would then abort the closure before the disconnect ever ran.
func TestDataMessageHandler_GoexitLoggerGoexitStillDisconnects(t *testing.T) {
	c, _ := newLifeConn(t, withMockTransport())
	require.NoError(t, c.UpdateConfigOptions(fastReconnectOpts()...))

	goexitLog := loggertest.NewMockLogger()
	goexitLog.On("Error", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { runtime.Goexit() }). //nolint:forbidigo // proving disconnect-before-log survives a Goexiting logger
		Return()
	require.NoError(t, c.UpdateConfigOptions(WithLogger(goexitLog)))

	log := &eventLog{}
	subscribeLog(t, c, log)

	require.NoError(t, c.Open(t.Context(), OpenBackground))
	requireSelected(t, c)

	c.AddDataMessageHandler(func(*DataMessage, SECS2Endpoint) { runtime.Goexit() })

	msg := mustSendData(t, [4]byte{0, 0, 0, 1}, false)
	exitedCh := make(chan struct{})
	go func() {
		defer close(exitedCh)
		_ = c.DeliverOwnedFrame(ownedFrame(t, msg))
	}()
	waitClosed(t, exitedCh, "the goroutine running the Goexiting handler and Goexiting logger never returned")

	disconnect := log.requireHandlerExitCause(t)
	require.Equal(t, SelectedState, disconnect.prev)
	require.Equal(t, NotConnectedState, disconnect.next)

	requireSelected(t, c) // the link reconnects
}
