package hsms

import (
	"errors"
	"time"

	"github.com/arloliu/go-secs/v2/logger"
)

// TimerConfig holds the HSMS protocol timer durations.
//
// T3: reply timeout (host waits for equipment response).
// T5: connection separation timeout (time between connect attempts).
// T6: control transaction timeout (time to receive response to a control message).
// T7: NOT SELECTED timeout (time to wait for SELECT.req after TCP connect).
// T8: network inter-character timeout (max time between bytes in a message).
type TimerConfig struct {
	T3, T5, T6, T7, T8 time.Duration
}

// ConnectionConfig holds the configuration for an HSMS connection.
// All fields are unexported; use With* options to configure via apply.
//
// Only a subset of the configuration is exposed for reading: [ConnectionConfig.Timers],
// [ConnectionConfig.SessionID], and [ConnectionConfig.WriteTimeout]. The remaining
// fields are set via With* options and have no read accessor.
type ConnectionConfig struct {
	timers                TimerConfig
	sessionID             uint16
	linktestInterval      time.Duration
	linktestFailThreshold int
	senderQueueSize       int
	closeTimeout          time.Duration
	writeTimeout          time.Duration
	logger                logger.Logger
}

// DefaultConnectionConfig returns a ConnectionConfig populated with
// SEMI E37-recommended default timer values and conservative operational defaults.
func DefaultConnectionConfig() *ConnectionConfig {
	return &ConnectionConfig{
		timers: TimerConfig{
			T3: 45 * time.Second,
			T5: 10 * time.Second,
			T6: 5 * time.Second,
			T7: 10 * time.Second,
			T8: 5 * time.Second,
		},
		sessionID:             0xFFFF,
		linktestInterval:      0, // 0 = disabled
		linktestFailThreshold: 3,
		senderQueueSize:       64,
		closeTimeout:          10 * time.Second,
		writeTimeout:          30 * time.Second,
		logger:                logger.Default(),
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

// WithSessionID sets the HSMS session ID. Any uint16 value is valid.
func WithSessionID(id uint16) ConnOption {
	return func(c *ConnectionConfig) error {
		c.sessionID = id

		return nil
	}
}

// WithLinktestInterval sets the interval between automatic linktest messages.
// A value of 0 disables automatic linktests. Must be >= 0.
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
// before the connection is considered broken. Must be >= 1.
func WithLinktestFailThreshold(n int) ConnOption {
	return func(c *ConnectionConfig) error {
		if n < 1 {
			return errors.New("WithLinktestFailThreshold: threshold must be >= 1")
		}

		c.linktestFailThreshold = n

		return nil
	}
}

// WithSenderQueueSize sets the size of the outbound message queue. Must be >= 1.
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
// finish during connection shutdown. Must be > 0.
func WithCloseTimeout(d time.Duration) ConnOption {
	return func(c *ConnectionConfig) error {
		if d <= 0 {
			return errors.New("WithCloseTimeout: duration must be > 0")
		}

		c.closeTimeout = d

		return nil
	}
}

// WithWriteTimeout bounds each on-wire frame write (the writev). Without it, a wedged or
// zero-window peer that stops reading can stall the connection's sole writer — and the auto-linktest
// that shares the send path — indefinitely, so the failure the linktest exists to detect goes
// undetected. On expiry the write fails and the generation is torn down (an involuntary drop); the
// always-on reconnect loop then re-establishes the link. The default is 30s. A value of 0 disables
// the bound (a write may block indefinitely on a wedged peer). Negative values are rejected.
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
