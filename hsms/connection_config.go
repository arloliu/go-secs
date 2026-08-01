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
// T1/T2/T4 are SECS-I (SEMI E4) concepts; they are unused by HSMS-SS. T5/T6/T7/T8 are HSMS (SEMI
// E37) concepts; they are unused by SECS-I. Both sets live in one struct so every timer rides the
// same live-update rail (see [ConnectionConfig] / [Connection.UpdateConfigOptions]).
type TimerConfig struct {
	T1, T2, T3, T4, T5, T6, T7, T8 time.Duration
}

// ConnectionConfig holds the configuration for an HSMS connection.
// All fields are unexported; use With* options to configure via apply.
//
// Only a subset of the configuration is exposed for reading: [ConnectionConfig.Timers],
// [ConnectionConfig.SessionID], and [ConnectionConfig.WriteTimeout]. The remaining
// fields are set via With* options and have no read accessor.
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
	autoS9F9                   bool
	traceTraffic               bool
	asyncSendErrHandler        func(msg Message, err error)
	reconnectBackoffInitial    time.Duration
	reconnectBackoffMultiplier float64
}

// DefaultConnectionConfig returns a ConnectionConfig populated with defaults.
//
// The defaults include SEMI E37 (HSMS) and SEMI E4 (SECS-I)-recommended timer
// values and conservative operational defaults.
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

// WithT3 sets the T3 (reply timeout) timer. Must be > 0.
func WithT3(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT3: duration must be > 0")
		}

		c.timers.T3 = d

		return nil
	}
}

// WithT5 sets the T5 (connection separation timeout) timer. Must be > 0.
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
// The first attempt after a drop waits for the duration specified by initial.
// For an active connection, this also applies to the first background retry of a
// cold-peer initial connect (see OpenBackground).
//
// Each subsequent failed attempt multiplies the previous wait by multiplier, capped at the
// configured T5 (see WithT5).
// T5 functions as the backoff CEILING, not the flat per-attempt delay.
//
// The initial duration must be > 0.
// The multiplier must be >= 1.0.
// A multiplier of 1.0 disables growth, giving a flat wait at initial capped by T5
// (e.g. pass WithReconnectBackoff(t5, 1.0) for the flat-T5 behavior).
//
// The default of (100ms, 2.0) approximates the SECS-I reconnect curve.
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

// WithT6 sets the T6 (control transaction timeout) timer. Must be > 0.
func WithT6(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT6: duration must be > 0")
		}

		c.timers.T6 = d

		return nil
	}
}

// WithT7 sets the T7 (NOT SELECTED timeout) timer. Must be > 0.
func WithT7(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithT7: duration must be > 0")
		}

		c.timers.T7 = d

		return nil
	}
}

// WithT8 sets the T8 (network inter-character timeout) timer. Must be > 0.
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

// WithSessionID sets the HSMS session ID. Any uint16 value is valid.
func WithSessionID(id uint16) ConnOption {
	return func(c *ConnectionConfig) error {
		c.sessionID = id

		return nil
	}
}

// WithLinktestInterval sets the interval between automatic linktest messages.
//
// A value of 0 disables automatic linktests.
// The duration must be >= 0.
func WithLinktestInterval(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d < 0 {
			return errors.New("WithLinktestInterval: duration must be >= 0")
		}

		c.linktestInterval = d

		return nil
	}
}

// WithLinktestFailThreshold sets the number of consecutive linktest failures
// before the connection is considered broken.
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

// WithLinktestSuppression enables or disables activity-based suppression of the
// automatic linktest (enabled by default).
//
// When enabled, the auto-linktest (see [WithLinktestInterval]) only probes a link
// that is actually idle:
//
//   - A Linktest.req is sent only after a full linktest interval with no HSMS
//     frame sent or received on the connection. A line carrying traffic is in
//     active use, so probing it adds redundant load — some older equipment is
//     slow to answer Linktest.req while busy. Note the asymmetry: outbound
//     traffic suppresses probing, but only received frames or an outstanding
//     reply count as proof of peer liveness for the failure rules below (a
//     successful write proves local buffering, not the peer).
//   - No Linktest.req is sent while a sent data message is awaiting its reply.
//     During that window the T3 reply timeout already bounds failure detection,
//     and probing equipment that is busy processing a long-running command
//     (a recipe transfer can take minutes) risks a spurious T6 timeout.
//   - A linktest failure (T6 timeout) is not counted toward the
//     linktest-failure disconnect threshold (see [WithLinktestFailThreshold])
//     while the link shows other signs of life: a frame arrived after the
//     Linktest.req went out, a data reply is still outstanding, or a frame
//     arrived since the previous counted failure. Only consecutive failures on
//     a silent link accumulate toward the disconnect.
//
// A truly dead link is still detected: a silent link with nothing outstanding is
// probed every interval exactly as before, so it is dropped within roughly
// threshold x (interval + T6) of its last sign of life; an unanswered reply is
// bounded by T3 first. Two rare races can extend that bound: a frame that races
// the probe's own wire write can earn false credit, pushing detection out by up
// to one extra probe cycle (interval + T6); and immediately after a reconnect, a
// late frame from the torn-down session can restart the failure run once,
// making the post-reconnect worst case roughly 2 x threshold x (interval + T6).
// Use a linktest failure threshold of at least 2 (the default is 3): a single
// probe timeout that races a lone received frame is then absorbed instead of
// disconnecting, and the disconnect decision re-checks for signs of life
// (received frames, or a reply still outstanding) immediately before dropping
// the link — though life arriving in the final instants of that decision can
// still be missed. With a threshold of 1, any single counted probe timeout —
// one with no observed sign of life — disconnects, with or without suppression.
// Disable suppression to restore unconditional periodic linktests — for example
// when the application streams fire-and-forget messages continuously (the line
// is never silent, so a suppressed linktest would never run).
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

// WithCloseTimeout sets the maximum time to wait for in-flight goroutines to
// finish during connection shutdown.
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
// Without it, a wedged or zero-window peer that stops reading can stall the connection's
// sole writer — and the auto-linktest that shares the send path — indefinitely, so the
// failure the linktest exists to detect goes undetected.
//
// On expiry the write fails and the generation is torn down (an involuntary drop).
// The always-on reconnect loop then re-establishes the link.
//
// The default is 30s.
// A value of 0 disables the bound (a write may block indefinitely on a wedged peer).
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

// SessionID returns the configured HSMS session ID.
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

// WithLogger sets the logger used by the connection. Must not be nil.
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
// When enabled, any inbound data message whose SessionID does not match this
// connection's configured SessionID is dropped (never delivered to a
// DataMessageHandler) and answered with an S9F1 sent from this connection's own
// SessionID.
//
// An inbound message that is itself S9F1 (SEMI E5's own "unrecognized device ID"
// notification) is exempted from the check entirely: it is delivered to
// DataMessageHandlers normally, regardless of its own SessionID, and never
// triggers an outbound S9F1 in response.
//
// This exemption exists for two reasons:
//  1. It avoids an S9F1-answers-S9F1 notification loop.
//  2. An inbound S9F1 is itself diagnostic information about a protocol problem
//     the PEER detected — silently dropping it would hide exactly the kind of
//     signal an application wants visibility into (this mirrors how S9Fx messages
//     are typically handled: logged and counted, not discarded).
//
// Disabled (the default, matching v2's shipped behavior prior to this option's
// introduction) means every inbound data message is delivered regardless of its
// SessionID. The application is fully responsible for any session-ID
// cross-checking it needs.
//
// Enable this for compliant HSMS/SECS-I peers where a SessionID mismatch signals
// a real protocol problem (e.g. a cross-wired connection) worth rejecting
// automatically.
//
// Leave it disabled when talking to equipment whose SessionID encoding does not
// follow the SEMI convention exactly (for example a peer that overlays a
// direction bit on the SessionID) — such a peer's legitimate traffic would
// otherwise be misclassified as mismatched and dropped.
func WithSessionIDValidation(enabled bool) ConnOption {
	return func(c *ConnectionConfig) error {
		c.validateSessionID = enabled

		return nil
	}
}

// WithAutoS9F9 enables automatic S9F9 (Transaction Timeout, SEMI E5 §10.13) notification.
//
// When a synchronous data-message send's reply wait times out (T3), an S9F9 whose body
// is the timed-out message's 10-byte SHEAD is sent to the peer before the timeout error
// is returned to the caller (fire-and-forget — a failure to send it does not change the
// returned error).
//
// Disabled by default.
//
// Most callers should not call this directly: it is the shared primitive behind
// hsmsss.WithEquipRole / hsmsss.WithHostRole and secs1.WithEquipment / secs1.WithHost,
// which express the concept applications actually configure (equipment vs. host role) and
// keep this and the transport's own role-specific state (e.g. secs1's line-contention role)
// in sync.
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
// Every frame sent or received is logged (Debug level, via the connection's configured Logger)
// with a hex dump of its raw bytes, including frames that fail to decode.
//
// Off by default.
// Expensive (every frame is hex-encoded even when no Debug sink consumes it) — intended
// for interactive debugging only, not production use.
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
// The callback is invoked when SendAsync, ForwardDataMessageAsync, and internal control-message
// async sends (such as Reject, Select.rsp, S9Fx) fail their transport write.
// The msg is the message that failed to reach the wire and err is the write error.
// This is the ONLY way to observe such a failure per-message: SendAsync/ForwardDataMessageAsync
// themselves report only enqueue-boundary errors (see AsyncSendErrCount for the always-on
// counter form).
//
// The fn callback runs SYNCHRONOUSLY, panic-isolated, on the per-generation async-sender
// goroutine.
// A slow fn delays every other queued async send on that generation, so keep it fast
// (increment a counter, log, push to a buffered channel) rather than doing blocking I/O.
//
// Passing nil (the default) disables the callback.
func WithAsyncSendErrorHandler(fn func(msg Message, err error)) ConnOption {
	return func(c *ConnectionConfig) error {
		c.asyncSendErrHandler = fn

		return nil
	}
}
