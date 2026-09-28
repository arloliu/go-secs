package hsms

import (
	"errors"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
)

// TimerConfig holds the HSMS and SECS-I protocol timer durations.
//
// T1: SECS-I inter-character timeout (max time between bytes of a block; SEMI E4 §7.3.1).
// T2: SECS-I protocol/reply timeout (max time to wait for a handshake reply such as EOT or ACK;
//
//	SEMI E4 §7.8).
//
// T3: reply timeout (host waits for equipment response).
// T4: SECS-I inter-block timeout (max time to wait for the next block of a multi-block message;
//
//	SEMI E4 §9.4.3).
//
// T5: connection separation timeout — the CEILING an exponential reconnect backoff ramps up to
//
//	and never exceeds (see WithReconnectBackoff); time between connect attempts.
//
// T6: control transaction timeout (time to receive response to a control message).
// T7: NOT SELECTED timeout (time to wait for SELECT.req after TCP connect).
// T8: network inter-character timeout (max time between bytes in a message).
//
// T1/T2/T4 are SECS-I (SEMI E4) concepts; they are unused by HSMS-SS.
// T5/T6/T7/T8 are HSMS (SEMI E37) concepts; they are unused by SECS-I.
// Both sets live in one struct so every timer rides the same live-update rail (see [ConnectionConfig] / [Connection.UpdateConfigOptions]).
type TimerConfig struct {
	T1, T2, T3, T4, T5, T6, T7, T8 time.Duration
}

// ConnectionConfig holds the configuration for an HSMS connection.
//
// All fields are unexported; use With* options to configure via apply.
//
// Only a subset of the configuration is exposed for reading: [ConnectionConfig.Timers], [ConnectionConfig.SessionID],
// and [ConnectionConfig.WriteTimeout].
// The remaining fields are set via With* options and have no read accessor.
type ConnectionConfig struct {
	timers                     TimerConfig
	sessionID                  uint16
	linktestInterval           time.Duration
	linktestFailThreshold      int
	linktestSuppression        bool
	senderQueueSize            int
	closeTimeout               time.Duration
	writeTimeout               time.Duration
	logger                     logger.Logger
	validateSessionID          bool
	strictReplyMatching        bool
	autoS9F9                   bool
	traceTraffic               bool
	asyncSendErrHandler        func(msg Message, err error)
	txObserver                 func(TxEvent)
	wireObserver               func(WireEvent)
	socketObserver             func(SocketEvent)
	reconnectBackoffInitial    time.Duration
	reconnectBackoffMultiplier float64
}

// DefaultConnectionConfig returns a ConnectionConfig populated with defaults.
//
// The defaults include SEMI E37 (HSMS) and SEMI E4 (SECS-I)-recommended timer values
// and conservative operational defaults.
func DefaultConnectionConfig() *ConnectionConfig {
	return &ConnectionConfig{
		timers: TimerConfig{
			T1: 500 * time.Millisecond,
			T2: 10 * time.Second,
			T3: 45 * time.Second,
			T4: 45 * time.Second,
			T5: 10 * time.Second,
			T6: 5 * time.Second,
			T7: 10 * time.Second,
			T8: 5 * time.Second,
		},
		sessionID:                  0xFFFF,
		linktestInterval:           0, // 0 = disabled
		linktestFailThreshold:      3,
		linktestSuppression:        true,
		senderQueueSize:            64,
		closeTimeout:               10 * time.Second,
		writeTimeout:               30 * time.Second,
		logger:                     logger.Default(),
		reconnectBackoffInitial:    100 * time.Millisecond,
		reconnectBackoffMultiplier: 2.0,
	}
}

// ConnOption is a functional option that mutates a ConnectionConfig.
//
// It returns an error if the provided value is invalid.
type ConnOption func(*ConnectionConfig) error

// apply applies options TRANSACTIONALLY (landmine D): all options are validated
// against a scratch copy of the config. If any option returns an error, the
// live config is left untouched and the joined error is returned. Only when
// every option succeeds is the live config updated atomically.
func (c *ConnectionConfig) apply(opts ...ConnOption) error {
	scratch := *c
	var errs []error

	for _, opt := range opts {
		if opt == nil {
			errs = append(errs, errors.New("hsms: option must not be nil"))
			continue
		}

		if err := opt(&scratch); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	*c = scratch

	return nil
}

// WithT3 sets the T3 (reply timeout) timer.
//
// Must be > 0.
func WithT3(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT3: duration must be > 0")
		}

		c.timers.T3 = d

		return nil
	}
}

// WithT5 sets the T5 (connection separation timeout) timer.
//
// Must be > 0.
// The default is 10s.
//
// SEMI E37 §9.2.1.1 specifies T5 as the flat wait between every reconnect attempt, not a ceiling a faster ramp climbs toward.
// The package's default reconnect backoff (see WithReconnectBackoff) seeds well under T5
// and grows,
// so several attempts land inside the first T5 window under the defaults.
// See WithReconnectBackoff for the deviation and the conformant setting.
func WithT5(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT5: duration must be > 0")
		}

		c.timers.T5 = d

		return nil
	}
}

// WithReconnectBackoff configures the exponential backoff between reconnect dial attempts.
//
// The configuration applies to dial retries per SEMI E37 §5.2 and §6.3.
//
// The first reconnect attempt of an Open cycle waits for the duration specified by initial.
// For an active connection, this also applies to the first background retry of a cold-peer initial connect (see OpenBackground).
// On a passive connection the wait separates re-listens rather than dials:
// after repeated peers that never select — dropped by T7, for example —
// the listening port stays closed for up to T5 between listens.
//
// Each subsequent attempt multiplies the previous wait by multiplier, capped at the configured T5 (see WithT5).
// T5 functions as the backoff CEILING, not the flat per-attempt delay.
// The wait keeps growing across separate reconnect attempts,
// including across a generation that comes up and drops again without ever reaching Selected —
// for example a peer that refuses every Select.req.
// It resets to initial only once a connection reaches Selected and later drops, or on the next Open.
//
// The initial duration must be > 0.
// The multiplier must be >= 1.0.
// A multiplier of 1.0 disables growth, giving a flat wait at initial capped by T5 (e.g. pass WithReconnectBackoff(t5, 1.0) for the flat-T5 behavior).
//
// The default of (100ms, 2.0) approximates the SECS-I reconnect curve.
//
// # Conformance
//
// SEMI E37 §9.2.1.1 specifies T5 as a flat separation timeout:
// every reconnect attempt waits exactly T5, with no faster seed and no ramp.
// The 100ms default seed is a deliberate deviation from that.
// With the default T5 of 10s and the default (100ms, 2.0) backoff, six attempts land inside the first T5 window at cumulative offsets of 0.1, 0.3, 0.7, 1.5, 3.1, and 6.3s,
// where §9.2.1.1 wants none —
// the seventh attempt, at 12.7s, is the first to fall outside that window.
// Fast recovery from a transient drop is the deliberate product choice:
// defaulting the seed to T5 would slow reconnect from roughly 100ms to 10s for every existing deployment.
//
// Pass WithReconnectBackoff(t5, 1.0),
// where t5 is the same duration configured via WithT5, for the conformant flat-T5 behavior.
func WithReconnectBackoff(initial time.Duration, multiplier float64) ConnOption {
	return func(c *ConnectionConfig) error {
		if initial <= 0 {
			return errors.New("WithReconnectBackoff: initial must be > 0")
		}
		if multiplier < 1.0 {
			return errors.New("WithReconnectBackoff: multiplier must be >= 1.0")
		}

		c.reconnectBackoffInitial = initial
		c.reconnectBackoffMultiplier = multiplier

		return nil
	}
}

// WithT6 sets the T6 (control transaction timeout) timer.
//
// Must be > 0.
func WithT6(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT6: duration must be > 0")
		}

		c.timers.T6 = d

		return nil
	}
}

// WithT7 sets the T7 (NOT SELECTED timeout) timer.
//
// Must be > 0.
func WithT7(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT7: duration must be > 0")
		}

		c.timers.T7 = d

		return nil
	}
}

// WithT8 sets the T8 (network inter-character timeout) timer.
//
// Must be > 0.
func WithT8(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT8: duration must be > 0")
		}

		c.timers.T8 = d

		return nil
	}
}

// WithT1 sets the SECS-I T1 (inter-character timeout) timer.
//
// The duration must be > 0.
// Unused by HSMS-SS.
func WithT1(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT1: duration must be > 0")
		}

		c.timers.T1 = d

		return nil
	}
}

// WithT2 sets the SECS-I T2 (protocol/reply timeout) timer.
//
// The duration must be > 0.
// Unused by HSMS-SS.
func WithT2(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT2: duration must be > 0")
		}

		c.timers.T2 = d

		return nil
	}
}

// WithT4 sets the SECS-I T4 (inter-block timeout) timer.
//
// The duration must be > 0.
// Unused by HSMS-SS.
func WithT4(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT4: duration must be > 0")
		}

		c.timers.T4 = d

		return nil
	}
}

// WithSessionID sets the HSMS session ID — the device ID stamped into outbound DATA messages (SEMI E37.1 §8.1).
//
// It does NOT affect control messages:
// Select.req, Separate.req, and Linktest.req always go out with [ControlSessionID] (0xFFFF), as HSMS-SS requires.
//
// Any uint16 value is valid.
// The default is 0xFFFF.
func WithSessionID(id uint16) ConnOption {
	return func(c *ConnectionConfig) error {
		c.sessionID = id

		return nil
	}
}

// WithLinktestInterval sets the interval between automatic linktest messages.
//
// A value of 0 disables automatic linktests, and 0 is the default.
// The duration must be >= 0.
//
// Without linktests an idle HSMS-SS link notices a silent half-open peer only through TCP keep-alive,
// because the receive side waits for the first byte of a frame without a deadline;
// a peer that closes or resets the connection is noticed at once either way.
// Go's default dialer and listener enable keep-alive,
// but on common platforms a dead peer is then detected only after minutes,
// and a socket from a custom dialer or listener may have no keep-alive at all.
// A production HSMS-SS link should therefore set an interval, for example 30 seconds.
func WithLinktestInterval(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d < 0 {
			return errors.New("WithLinktestInterval: duration must be >= 0")
		}

		c.linktestInterval = d

		return nil
	}
}

// WithLinktestFailThreshold sets the number of consecutive linktest failures before the connection is considered broken.
//
// The threshold must be >= 1.
func WithLinktestFailThreshold(n int) ConnOption {
	return func(c *ConnectionConfig) error {
		if n < 1 {
			return errors.New("WithLinktestFailThreshold: threshold must be >= 1")
		}

		c.linktestFailThreshold = n

		return nil
	}
}

// WithLinktestSuppression enables or disables activity-based suppression of the automatic linktest (enabled by default).
//
// When enabled, the auto-linktest (see [WithLinktestInterval]) only probes a link that is actually idle:
//
//   - A Linktest.req is sent only after a full linktest interval with no HSMS frame sent or received on the connection.
//     A line carrying traffic is in active use, so probing it adds redundant load — some older equipment is slow to answer Linktest.req while busy.
//     Note the asymmetry: outbound traffic suppresses probing, but only received frames or an outstanding reply count as proof of peer liveness for the failure rules below (a successful write proves local buffering, not the peer).
//   - No Linktest.req is sent while a sent data message is awaiting its reply.
//     During that window the T3 reply timeout already bounds failure detection, and probing equipment
//     that is busy processing a long-running command (a recipe transfer can take minutes) risks a spurious T6 timeout.
//   - A linktest failure (T6 timeout) is not counted toward the linktest-failure disconnect threshold (see [WithLinktestFailThreshold]) while the link shows other signs of life:
//     a frame arrived after the Linktest.req went out, a data reply is still outstanding, or a frame arrived since the previous counted failure.
//     Only consecutive failures on a silent link accumulate toward the disconnect.
//
// A truly dead link is still detected: a silent link with nothing outstanding is probed every interval exactly as before,
// so it is dropped within roughly threshold x (interval + T6) of its last sign of life;
// an unanswered reply is bounded by T3 first.
// Two rare races can extend that bound: a frame that races the probe's own wire write can earn false credit, pushing detection out by up to one extra probe cycle (interval + T6);
// and immediately after a reconnect, a late frame from the torn-down session can restart the failure run once, making the post-reconnect worst case roughly 2 x threshold x (interval + T6).
// Use a linktest failure threshold of at least 2 (the default is 3): a single probe timeout
// that races a lone received frame is then absorbed instead of disconnecting, and the disconnect decision re-checks for signs of life (received frames, or a reply still outstanding) immediately before dropping the link —
// though life arriving in the final instants of that decision can still be missed.
// With a threshold of 1, any single counted probe timeout — one with no observed sign of life —
// disconnects, with or without suppression.
// Disable suppression to restore unconditional periodic linktests — for example when the application streams fire-and-forget messages continuously (the line is never silent,
// so a suppressed linktest would never run).
func WithLinktestSuppression(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.linktestSuppression = enabled
		return nil
	}
}

// WithSenderQueueSize sets the size of the outbound message queue.
//
// The size must be >= 1.
func WithSenderQueueSize(n int) ConnOption {
	return func(c *ConnectionConfig) error {
		if n < 1 {
			return errors.New("WithSenderQueueSize: size must be >= 1")
		}

		c.senderQueueSize = n

		return nil
	}
}

// WithCloseTimeout sets the maximum time to wait for in-flight goroutines to finish during connection shutdown.
//
// It bounds two separate joins Close performs, one after the other: the per-generation teardown join
// (the transport recv loop, and any live task goroutines) and, from whatever remains, the state-change notifier join.
// It does NOT cover the best-effort farewell Separate write on a graceful teardown from Selected,
// which has its own fixed allowance of at most 500ms
// and is skipped rather than allowed to block Close — see [Connection.Close].
//
// The duration must be > 0.
func WithCloseTimeout(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithCloseTimeout: duration must be > 0")
		}

		c.closeTimeout = d

		return nil
	}
}

// WithWriteTimeout bounds each on-wire frame write (the writev).
//
// Without it, a wedged or zero-window peer that stops reading can stall the connection's sole writer — and the auto-linktest
// that shares the send path — indefinitely, so the failure the linktest exists to detect goes undetected.
//
// On expiry the write fails and the generation is torn down (an involuntary drop).
// The always-on reconnect loop then re-establishes the link.
//
// The default is 30s. A value of 0 disables the bound (a write may block indefinitely on a wedged peer).
// Negative values are rejected.
func WithWriteTimeout(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d < 0 {
			return errors.New("WithWriteTimeout: duration must be >= 0")
		}

		c.writeTimeout = d

		return nil
	}
}

// Timers returns a copy of the timer configuration.
func (c *ConnectionConfig) Timers() TimerConfig {
	return c.timers
}

// SessionID returns the configured HSMS session ID (the device ID for data messages; see [WithSessionID]).
func (c *ConnectionConfig) SessionID() uint16 {
	return c.sessionID
}

// WriteTimeout returns the per-frame write (writev) deadline; 0 means unbounded.
func (c *ConnectionConfig) WriteTimeout() time.Duration {
	return c.writeTimeout
}

// Logger returns the configured logger.
func (c *ConnectionConfig) Logger() logger.Logger {
	return c.logger
}

// WithLogger sets the logger used by the connection.
//
// The logger is called on connection goroutines, including during teardown —
// a call that blocks there blocks that goroutine, and can keep Close from completing (see [Connection.Close]).
// It must not block.
//
// Must not be nil.
func WithLogger(l logger.Logger) ConnOption {
	return func(c *ConnectionConfig) error {
		if l == nil {
			return errors.New("WithLogger: logger must not be nil")
		}

		c.logger = l

		return nil
	}
}

// WithSessionIDValidation enables or disables inbound SessionID validation.
//
// When enabled, any inbound data message whose SessionID does not match this connection's configured SessionID is dropped (never delivered to a DataMessageHandler)
// and answered with an S9F1 sent from this connection's own SessionID.
//
// An inbound message that is itself S9F1 (SEMI E5's own "unrecognized device ID" notification) is exempted from the check entirely:
// it is delivered to DataMessageHandlers normally, regardless of its own SessionID,
// and never triggers an outbound S9F1 in response.
//
// This exemption exists for two reasons:
//  1. It avoids an S9F1-answers-S9F1 notification loop.
//  2. An inbound S9F1 is itself diagnostic information about a protocol problem
//     the PEER detected — silently dropping it would hide exactly the kind of
//     signal an application wants visibility into (this mirrors how S9Fx messages
//     are typically handled: logged and counted, not discarded).
//
// Disabled (the default, matching v2's shipped behavior prior to this option's introduction) means every inbound data message is delivered regardless of its SessionID.
// The application is fully responsible for any session-ID cross-checking it needs.
//
// Enable this for compliant HSMS/SECS-I peers where a SessionID mismatch signals a real protocol problem (e.g. a cross-wired connection) worth rejecting automatically.
//
// Leave it disabled when talking to equipment whose SessionID encoding does not follow the SEMI convention exactly (for example a peer
// that overlays a direction bit on the SessionID) — such a peer's legitimate traffic would otherwise be misclassified as mismatched
// and dropped.
func WithSessionIDValidation(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.validateSessionID = enabled

		return nil
	}
}

// WithStrictReplyMatching enables strict SEMI E37 §9.4.1 reply-field validation.
//
// §9.4.1 requires a reply to match its primary on SessionID, Stream, Function (primary + 1, or 0 — SxF0, the transaction-abort secondary, E5 §7.2/§10.4.1), and System Bytes.
// This option covers the stream/function half; System Bytes is always the registry key.
// SessionID is a separate seam:
// enable [WithSessionIDValidation] too for full §9.4.1 field enforcement (mirrors how [WithReconnectBackoff](t5, 1.0) composes with [WithT5]).
//
// Disabled (the default) means every candidate reply —
// a W-clear, even-function *DataMessage secondary hitting an open System-Bytes entry registered for a data primary —
// is delivered to the waiting sender even when its stream or function diverges from the primary;
// the divergence is only counted (see [ConnectionMetrics.ReplyMismatchCount]).
// Control transactions (Select/Deselect/Linktest) and a peer Reject.req are never subject to this check either way —
// they carry no stream/function to compare.
//
// Enabled, a field mismatch is a MISS: the message falls through to the session's data handlers as unsolicited instead of satisfying the waiting sender,
// and the registration is NOT consumed, so a later conforming reply can still complete the transaction.
// Against a peer that is sloppy about echoing stream or function, this turns a transaction that previously "worked" (by receiving the wrong reply quickly) into a stall that runs to T3 —
// returning ErrT3Timeout — unless a conforming reply follows.
//
// Recommended path: soak under the default first and enable this only once [ConnectionMetrics.ReplyMismatchCount] stays at 0,
// so enforcement is turned on against a peer already known to reply correctly, not discovered against one that does not.
func WithStrictReplyMatching(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.strictReplyMatching = enabled

		return nil
	}
}

// WithAutoS9F9 enables automatic S9F9 (Transaction Timeout, SEMI E5 §10.13) notification.
//
// When a synchronous data-message send's reply wait times out (T3), an S9F9 whose body is the timed-out message's 10-byte SHEAD is sent to the peer before the timeout error is returned to the caller (fire-and-forget —
// a failure to send it does not change the returned error).
//
// Disabled by default.
//
// Most callers should not call this directly: it is the shared primitive behind hsmsss.WithEquipRole / hsmsss.WithHostRole
// and secs1.WithEquipment / secs1.WithHost, which express the concept applications actually configure (equipment vs. host role)
// and keep this and the transport's own role-specific state (e.g. secs1's line-contention role) in sync.
func WithAutoS9F9(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.autoS9F9 = enabled

		return nil
	}
}

// AutoS9F9 reports whether automatic S9F9-on-T3-timeout notification is enabled (see WithAutoS9F9).
func (c *ConnectionConfig) AutoS9F9() bool {
	return c.autoS9F9
}

// WithTraceTraffic enables per-frame wire-level tracing.
//
// Every frame sent or received is logged (Debug level, via the connection's configured Logger) with a hex dump of its raw bytes, including frames
// that fail to decode.
//
// Off by default.
// Expensive (every frame is hex-encoded even when no Debug sink consumes it) — intended for interactive debugging only, not production use.
func WithTraceTraffic(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.traceTraffic = enabled

		return nil
	}
}

// TraceTraffic reports whether per-frame wire tracing is enabled (see WithTraceTraffic).
func (c *ConnectionConfig) TraceTraffic() bool {
	return c.traceTraffic
}

// WithAsyncSendErrorHandler installs a callback invoked whenever a fire-and-forget async send fails.
//
// The callback is invoked when SendAsync, ForwardDataMessageAsync, and internal control-message async sends (such as Reject, Select.rsp, S9Fx) fail during async send processing —
// either a transport write error, or a frame rejected before the write for exceeding MaxMessageSize (ErrMessageTooLarge).
// The msg is the message that failed to reach the wire and err is the failure (the write error, or ErrMessageTooLarge).
// This is the ONLY way to observe such a failure per-message:
// SendAsync/ForwardDataMessageAsync themselves report only enqueue-boundary errors (see AsyncSendErrCount for the always-on counter form).
//
// The fn callback runs SYNCHRONOUSLY on the per-generation async-sender goroutine.
// A slow fn delays every other queued async send on that generation, so keep it fast (increment a counter, log, push to a buffered channel) rather than doing blocking I/O.
//
// A panic inside fn is recovered, logged at Error with its stack, and counted in
// [ConnectionMetrics.HandlerPanicCount]; the sender goroutine keeps draining queued sends for the
// rest of this generation.
// Calling runtime.Goexit from fn is not a panic: it is logged, not counted, and ends the sender goroutine,
// dropping the generation it was draining for so the connection reconnects rather than leaving that generation's async sends unserved.
//
// Passing nil (the default) disables the callback.
func WithAsyncSendErrorHandler(fn func(msg Message, err error)) ConnOption {
	return func(c *ConnectionConfig) error {
		c.asyncSendErrHandler = fn

		return nil
	}
}

// WithTransactionObserver installs a hook invoked once per completed synchronous send transaction:
// SendDataMessage, SendSECS2Message, and ForwardDataMessage (SECS2Endpoint).
// Each call that reaches the send path reports exactly one TxEvent describing how its transaction ended.
// A call that fails validation before the send path — such as ErrEvenFunctionPrimary or ErrInvalidStreamCode from message construction —
// returns its error directly and reports no TxEvent.
//
// The hook runs SYNCHRONOUSLY on the CALLING goroutine, after the send's own outcome is known and before the call returns to its caller —
// a slow fn slows that one send call and nothing else;
// it never runs on a protocol goroutine.
// Async sends are out of scope for this hook:
// SendDataMessageAsync, ForwardDataMessageAsync, SendAsync, and ReplyDataMessage
// (which is itself built on SendAsync, not a synchronous write)
// never report a TxEvent — see WithAsyncSendErrorHandler for their failure observability.
// A control transaction — the Select.req / Linktest.req sends a transport issues internally — never reports one either;
// only a caller-issued DATA message send does.
//
// A panic inside fn is NOT recovered:
// it propagates to the caller of the send exactly as if fn's body ran inline in that call,
// so a panicking observer surfaces immediately instead of being silently swallowed.
// Keep fn panic-free, or add your own recover if that risk matters to you.
//
// Passing nil (the default) disables the hook.
// Reading it costs one atomic pointer load already paid by the send path, plus one nil check —
// no allocation and no observable cost when unset.
//
// Example — a Prometheus histogram keyed by stream/function/outcome:
//
//	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
//		Name: "hsms_transaction_duration_seconds",
//	}, []string{"stream", "function", "outcome"})
//
//	opt := hsms.WithTransactionObserver(func(ev hsms.TxEvent) {
//		hist.WithLabelValues(
//			strconv.Itoa(int(ev.Stream)),
//			strconv.Itoa(int(ev.Function)),
//			ev.Outcome.String(),
//		).Observe(ev.Duration.Seconds())
//	})
func WithTransactionObserver(fn func(TxEvent)) ConnOption {
	return func(c *ConnectionConfig) error {
		c.txObserver = fn

		return nil
	}
}

// WithWireObserver installs a hook called once for every complete HSMS frame that crosses the connection's socket, in either direction.
//
// Every frame is reported, data and control alike:
// the frames the connection sends on its own (Select, Linktest, Reject, Separate, auto S9Fx),
// and inbound frames the connection goes on to reject or drop after reading them
// (an unsupported PType or SType, a control frame with a body, a data message while not Selected, a frame that fails to decode).
// An inbound frame is reported as soon as it has been read, before it is interpreted.
// An outbound frame is reported after its write returned successfully;
// a frame whose write failed, and a frame refused before the write, are not reported.
// [WireEvent.At] is the time an inbound frame was fully read, and the time immediately before an outbound frame's write was issued:
// a frame the peer sends in answer never carries an earlier At than the frame it answers,
// however late the write of the frame it answers returned.
// A frame the connection could not read completely,
// because the socket closed partway through it,
// is never a frame and is not reported.
// The frames a passive connection exchanges with an extra peer it refuses are reported too,
// with that socket's identity and generation 0 (see [WithSocketObserver]).
//
// Frames are reported only by a transport whose socket carries HSMS frames, which is the HSMS-SS transport.
// A SECS-I connection built on the same core reports nothing,
// because the bytes on its wire are SECS-I blocks, not HSMS frames.
//
// The contract fn must keep:
//   - ev.Frame is valid only during the call: it must not be retained and must not be modified.
//     The inbound buffer is the one the received data message later adopts as its body without a copy,
//     so writing into it would corrupt that message.
//     Copy the bytes to keep them.
//   - fn runs synchronously on the path that moved the frame:
//     the receive goroutine for an inbound frame, and the writing goroutine, under the connection's write lock, for an outbound one.
//     Two frames are the exception, because the goroutines that move them are ones Close waits for:
//     the courtesy Separate a graceful close writes, and the frames of a refused peer's exchange,
//     are delivered from a goroutine started for that one report,
//     once the peer's frame was fully read or the outbound frame's write succeeded.
//     Their At follows the same rule as every other frame's:
//     when an inbound frame was fully read, and immediately before an outbound frame's write was issued.
//     It must be cheap — copy the frame into your own queue and return.
//     A slow fn delays the link, including the reads that the T8 inter-character timer bounds,
//     and delays the SocketClosed report of the socket the frame crossed, which follows the call;
//     closing the socket itself never waits for fn,
//     and Close and Stop wait for it only through the bounded teardown join, never without a bound.
//     An fn that never returns from one of the delivered reports leaks that goroutine
//     and never delivers that socket's SocketClosed event, nothing else.
//   - Calls for the two directions can run at the same time, so fn must be safe for concurrent use.
//   - fn must not send on the same connection from inside any call.
//     During an outbound call the write lock is held or the connection is closing, so such a send waits for itself or fails;
//     during an inbound call the receive goroutine is the one parked in fn,
//     so a send that waits for its reply waits out T3 for a reply nothing can read.
//   - A panic in fn is not recovered by the hook:
//     it unwinds whichever goroutine raised the frame —
//     a synchronous send's caller, the receive goroutine, one of the connection's internal send goroutines,
//     or the goroutine delivering a courtesy Separate or a refused peer's frames, which nothing else runs on.
//     Keep fn panic-free.
//
// Within one socket, inbound frames are reported in wire order, and so are outbound frames.
// No order is promised between the two directions beyond the events' timestamps,
// except on a refused socket, whose two frames one goroutine reports in wire order, the peer's first.
// Every frame a socket carried is reported before that socket's SocketClosed event (see [WithSocketObserver]),
// because that event is held back while a frame report is in progress on the socket
// and is then delivered by the goroutine that made the frame's report, once fn has returned;
// a frame read or written once the socket's close is pending is not reported.
//
// Passing nil (the default) disables the hook.
// With an observer installed, an outbound frame is copied into a scratch buffer reused for the life of the link,
// so observation adds no allocation per frame once that buffer has grown to the largest frame written;
// the courtesy Separate alone is copied into a buffer of its own, once per graceful close.
func WithWireObserver(fn func(WireEvent)) ConnOption {
	return func(c *ConnectionConfig) error {
		c.wireObserver = fn

		return nil
	}
}

// WithSocketObserver installs a hook called for every socket the connection dials or accepts,
// once when the socket comes up, once more if it is refused, and once when it closes.
//
// Every socket is reported, including those the connection never uses for a session:
// an extra peer the passive role accepts and then refuses because a session is already live,
// a dial that completes after Close began, and a socket declined because its generation had already ended.
// Each socket yields, in this order:
//   - SocketConnected (active role) or SocketAccepted (passive role), as soon as the dial or accept returned;
//   - SocketRefused, for an extra passive peer only, as soon as the connection decides to refuse it,
//     before the refusal exchange with that peer begins;
//   - SocketClosed, exactly once, whichever side or path closed the socket first.
//
// Frames the socket carried are reported by [WithWireObserver] with the same [SocketEvent.Socket] value,
// between that socket's first event and its close:
// SocketClosed is held back while a frame report is in progress on the socket, so no frame event of a socket follows it.
// Closing the socket never waits for that report, and neither do Close and Stop;
// a wire observer that does not return delays only the SocketClosed event,
// and the bounded teardown join when it is parked on the receive goroutine or a sender.
// A SocketClosed delivered late still carries the time the socket was closed in [SocketEvent.At].
// A refused socket's frames —
// the peer's Select.req and the Select.rsp answering it, whichever of the two crossed the wire —
// follow its SocketRefused and carry Generation 0.
// [SocketEvent.Err] on the close names the failure that initiated it, and is nil for a close nobody's failure caused;
// [SocketEvent.Cause] names why the socket closed.
// The two are one close reason, from whichever path closed the socket first,
// so Cause can differ from the cause of the lifecycle transition that ended the socket's generation.
//
// Sockets are reported only by a transport that owns sockets and reports them, which is the HSMS-SS transport.
// A SECS-I connection reports nothing.
//
// The contract fn must keep:
//   - fn runs synchronously on whichever goroutine dialed, accepted, refused or closed the socket —
//     including the connection's own lifecycle goroutine when a teardown closes the socket —
//     or, for a SocketClosed held back behind a frame report, on the goroutine that made that report
//     (the receive goroutine, a sender under the write lock,
//     or the goroutine delivering a courtesy Separate or a refused peer's frames),
//     so it must be cheap: copy what you need and return.
//   - Close and Stop wait for a call of fn in progress on the dialing, accepting, refusing or closing goroutine to return,
//     because the connection joins those goroutines, so an fn that never returns there holds Close.
//     A SocketClosed held back behind a frame report is waited for only through the reporting goroutine's join:
//     bounded for the receive goroutine and a sender, reported as ErrCloseTimeout,
//     and not at all for the goroutine delivering a courtesy Separate or a refused peer's frames,
//     where an fn that never returns leaks that goroutine and the event is never delivered.
//   - Calls for different sockets, and a socket's close racing another socket's accept, can run at the same time,
//     so fn must be safe for concurrent use.
//   - fn must not call back into the connection (Open, Close, a send) from inside the call.
//   - A panic in fn is not recovered by the hook: it unwinds the goroutine that raised the event.
//     Keep fn panic-free.
//
// Passing nil (the default) disables the hook.
func WithSocketObserver(fn func(SocketEvent)) ConnOption {
	return func(c *ConnectionConfig) error {
		c.socketObserver = fn

		return nil
	}
}
