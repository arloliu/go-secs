package secs1

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// Config holds the full configuration for a SECS-I (SEMI E4) connection over TCP/IP.
// It embeds [hsms.ConnectionConfig] so the shared core knobs it reuses (T1/T2/T3/T4/T5 timers,
// session/device wiring, logger, close/queue policy) are promoted directly onto Config and read
// via the same accessors. SECS-I-specific settings — the TCP endpoint, active/passive role,
// keep-alive, the RTY retry limit, and the equipment/host role and device ID — are separate
// unexported fields set through [Option] values.
//
// Note two SECS-I invariants Config enforces at construction (see [NewConfig]):
//   - The embedded core write timeout is forced to 0 so a core write deadline can never preempt a
//     SECS-I line transaction, which is bounded instead by T2 × (RetryLimit+1).
//   - The core auto-linktest stays inert: secs1 never exposes a linktest option nor sets an
//     interval, and the embedded default interval is 0 (disabled), so no linktest goroutine arms.
type Config struct {
	hsms.ConnectionConfig // embedded by value; promotes Timers/SessionID/WriteTimeout accessors

	host         string
	port         int
	active       bool          // true = active (dials); false = passive (listens). Default active.
	tcpKeepAlive time.Duration // 0 = OS default

	retryLimit int        // RTY: block-send retry count, 0..31 (SEMI E4)
	isEquip    bool       // true = equipment (master); false = host (slave)
	deviceID   uint16     // SECS-I device ID, 0..0x7FFF
	dial       DialFunc   // active-mode dialer; default (&net.Dialer{}).DialContext
	listen     ListenFunc // passive-mode listener; default (&net.ListenConfig{}).Listen

	connectTimeout time.Duration // 0 = unbounded (OS default); >0 bounds each active dial attempt
}

// Option is a functional option that mutates a [Config].
//
// It returns an error if the provided value is invalid.
// NewConfig applies the whole option set transactionally (all-or-nothing).
type Option func(*Config) error

// DialFunc is an alias of [hsms.DialFunc] — see its doc for the contract.
//
// It is kept as a named type in this package so existing signatures (WithDialer)
// don't need touching, and so callers can keep writing secs1.DialFunc without
// importing hsms for this alone.
type DialFunc = hsms.DialFunc

// ListenFunc is an alias of [hsms.ListenFunc] — see its doc for the contract.
type ListenFunc = hsms.ListenFunc

// NewConfig constructs a Config for the given TCP host and port, applying the supplied options
// transactionally.
//
// The application is all-or-nothing: every option is validated on a scratch copy; if any option
// returns an error the original is discarded and the joined set of errors is returned.
//
// The embedded [hsms.ConnectionConfig] is initialised to the core defaults (T3=45 s reply timeout,
// sessionID=0xFFFF, logger set). The SECS-I timers default to the SEMI E4 recommendations:
// T1=500 ms, T2=10 s, T4=45 s, RetryLimit=3. The default role is active (dials outbound) and host
// (isEquip=false); pass [WithPassive] / [WithEquipment] / [WithHost] to change them.
//
// As its LAST construction step — after all user options have applied — NewConfig FORCES the
// embedded writeTimeout to 0. SECS-I bounds each on-wire block transaction
// with its own T2 × (RetryLimit+1) budget; a non-zero core write deadline could preempt that
// budget mid-transaction and desync the line, so the override is applied last and a user cannot
// re-arm it (e.g. via WithConnectionOption(hsms.WithWriteTimeout(...))).
//
// Also as a LAST step, NewConfig forces the core session ID to the configured SECS-I device ID (see
// [WithDeviceID]). The device ID IS the effective session identity, so the connection and its
// outbound messages report it through SessionID(); a user cannot override it via
// WithConnectionOption(hsms.WithSessionID(...)).
func NewConfig(host string, port int, opts ...Option) (Config, error) {
	base := hsms.DefaultConnectionConfig()
	cfg := Config{
		ConnectionConfig: *base,
		host:             host,
		port:             port,
		active:           true, // default: active role (dials outbound)
		retryLimit:       3,    // SEMI E4 recommended default

		dial:   (&net.Dialer{}).DialContext,
		listen: (&net.ListenConfig{}).Listen,
	}

	if err := cfg.apply(opts...); err != nil {
		return Config{}, err
	}

	// D5b-11: force writeTimeout=0 as the LAST step so no user option can leave it re-armed.
	// hsms.WithWriteTimeout(0) validates trivially (0 is the "unbounded" sentinel), so this
	// cannot fail; the error is asserted only to satisfy the option contract.
	if err := hsms.WithWriteTimeout(0)(&cfg.ConnectionConfig); err != nil {
		return Config{}, err
	}

	// Force the core session ID to the SECS-I device ID as the LAST step. The SECS-I device ID IS
	// the effective session identity, so the connection and its outbound messages report the device
	// ID through SessionID() instead of the 0xFFFF core sentinel; a user cannot override it via
	// WithConnectionOption(hsms.WithSessionID(...)). Any uint16 is a valid session ID, so this
	// cannot fail; the error is asserted only to satisfy the option contract.
	if err := hsms.WithSessionID(cfg.deviceID)(&cfg.ConnectionConfig); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// apply validates all options against a scratch copy of cfg (all-or-nothing). On success the live
// cfg is updated atomically; on any error cfg is unchanged and the joined error is returned.
func (c *Config) apply(opts ...Option) error {
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

// ApplyOptions applies the supplied options to the Config transactionally.
//
// This is the public counterpart to the unexported apply method (all-or-nothing: see [NewConfig]).
//
// It does NOT re-apply the writeTimeout=0 override, so callers adjusting settings
// after construction must not re-arm writeTimeout through it.
func (c *Config) ApplyOptions(opts ...Option) error {
	return c.apply(opts...)
}

// WithActive sets the connection role to active (dials outbound). Active is the default role; this
// option is provided for explicit clarity.
func WithActive() Option {
	return func(c *Config) error {
		c.active = true
		return nil
	}
}

// WithPassive sets the connection role to passive (listens for inbound connections).
//
// Passive and active are mutually exclusive.
// Calling [WithActive] and [WithPassive] in the same [NewConfig] call results in
// whichever option appears last winning, since both simply assign the field.
func WithPassive() Option {
	return func(c *Config) error {
		c.active = false
		return nil
	}
}

// WithT1 sets the SECS-I T1 inter-character timeout (max time between bytes of a block).
//
// The duration must be > 0.
func WithT1(d time.Duration) Option {
	return func(c *Config) error {
		return hsms.WithT1(d)(&c.ConnectionConfig)
	}
}

// WithT2 sets the SECS-I T2 protocol/reply timeout (max time to wait for a handshake reply).
//
// This bounds the wait for EOT or the reply block.
// The duration must be > 0.
func WithT2(d time.Duration) Option {
	return func(c *Config) error {
		return hsms.WithT2(d)(&c.ConnectionConfig)
	}
}

// WithT4 sets the SECS-I T4 inter-block timeout.
//
// This bounds the max time to wait for the next block of a multi-block message.
// The duration must be > 0.
func WithT4(d time.Duration) Option {
	return func(c *Config) error {
		return hsms.WithT4(d)(&c.ConnectionConfig)
	}
}

// WithT5 sets the T5 reconnect-backoff ceiling.
//
// The exponential backoff between successive connect attempts after a TCP disconnect
// ramps up to at most this duration (see hsms.WithReconnectBackoff for the growth curve).
//
// SEMI E4 does not define a T5 timer; go-secs reuses the shared hsms T5 slot as the
// reconnect-cadence knob for BOTH transports, so this exists for parity with
// WithT1/WithT2/WithT4.
// Prior to this option, the only way to tune SECS-I's reconnect cadence was the
// WithConnectionOption(hsms.WithT5(...)) wrapper form, which left connections on the
// default value if omitted.
//
// The duration must be > 0.
func WithT5(d time.Duration) Option {
	return func(c *Config) error {
		return hsms.WithT5(d)(&c.ConnectionConfig)
	}
}

// WithRetryLimit sets the SECS-I RTY block-send retry limit.
//
// The limit must be in the range 0..31 per SEMI E4.
func WithRetryLimit(n int) Option {
	return func(c *Config) error {
		if n < 0 || n > 31 {
			return fmt.Errorf("WithRetryLimit: retry limit must be 0..31, got %d", n)
		}

		c.retryLimit = n

		return nil
	}
}

// WithEquipment sets the SECS-I role to equipment (master, controls line contention).
//
// Equipment is the master role: it initiates ENQ and ignores inbound ENQ during an active send.
// It also enables the shared hsms.WithAutoS9F9 behavior: a synchronous data-message
// send whose reply times out (T3) automatically notifies the peer with an S9F9
// (SEMI E5 §10.13) before the timeout error is returned.
//
// The default role is host; use this option to override it.
// Equipment and host are mutually exclusive — whichever option appears last in a
// [NewConfig] call wins.
func WithEquipment() Option {
	return func(c *Config) error {
		c.isEquip = true

		return hsms.WithAutoS9F9(true)(&c.ConnectionConfig)
	}
}

// WithHost sets the SECS-I role to host (slave), and disables the shared AutoS9F9 T3-timeout notification.
//
// The host is the default role.
// This option is provided for explicit clarity or to override a preceding [WithEquipment] call.
//
// Equipment and host are mutually exclusive — whichever option appears last in a
// [NewConfig] call wins (see WithEquipment).
func WithHost() Option {
	return func(c *Config) error {
		c.isEquip = false

		return hsms.WithAutoS9F9(false)(&c.ConnectionConfig)
	}
}

// WithDeviceID sets the SECS-I device ID.
//
// The ID must be in the range 0..0x7FFF (15-bit, per SEMI E4).
func WithDeviceID(id uint16) Option {
	return func(c *Config) error {
		if id > 0x7FFF {
			return fmt.Errorf("WithDeviceID: device ID must be 0..0x7FFF, got 0x%X", id)
		}

		c.deviceID = id

		return nil
	}
}

// WithDialer overrides how the active connection is established.
//
// The default dials with [net.Dialer.DialContext] over TCP.
// Supply a custom [DialFunc] to layer the connection on a different transport — for
// example a TLS or proxy dialer, or an in-memory connection for tests.
//
// Passing nil is a configuration error.
// This option affects the active (dialing) role only; the passive role always listens
// on the configured TCP endpoint.
func WithDialer(dial DialFunc) Option {
	return func(c *Config) error {
		if dial == nil {
			return errors.New("WithDialer: dial must not be nil")
		}

		c.dial = dial

		return nil
	}
}

// WithConnectTimeout bounds each active-role dial attempt to d. The default (0) leaves the dial
// unbounded, so a dial to an unreachable peer blocks for the OS connect timeout (~2 minutes). A
// positive d wraps every dial attempt — including background reconnect attempts — in a
// per-attempt deadline.
//
// This affects the active (dialing) role only. It composes with WithDialer: the deadline wraps
// whatever DialFunc is configured. A negative d is a configuration error.
func WithConnectTimeout(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return errors.New("WithConnectTimeout: timeout must not be negative")
		}

		c.connectTimeout = d

		return nil
	}
}

// WithListener overrides how the passive connection listens for an inbound peer.
//
// The default listens with [net.ListenConfig.Listen] over TCP.
// Supply a custom [ListenFunc] to layer the listener on a different transport — for
// example an in-memory pipe-backed listener for tests.
//
// Unlike WithDialer's single active dial, a passive connection re-listens on every reconnect
// generation, so listen is a FACTORY called fresh each time, never a single reused net.Listener.
//
// Passing nil is a configuration error.
// This option affects the passive (listening) role only; the active role always dials
// the configured TCP endpoint via [WithDialer].
func WithListener(listen ListenFunc) Option {
	return func(c *Config) error {
		if listen == nil {
			return errors.New("WithListener: listen must not be nil")
		}

		c.listen = listen

		return nil
	}
}

// WithTCPKeepAlive sets the TCP keep-alive probe interval for the underlying socket.
//
// A value of 0 uses the OS default.
// The duration must be >= 0.
func WithTCPKeepAlive(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return errors.New("WithTCPKeepAlive: duration must be >= 0")
		}

		c.tcpKeepAlive = d

		return nil
	}
}

// WithConnectionOption wraps an [hsms.ConnOption] as an [Option].
//
// This allows callers to tune the shared core knobs that SECS-I reuses, without a
// separate re-export per option.
//
// Knobs that take effect for SECS-I:
//   - [hsms.WithT1] — E4 inter-character timeout (default 500 ms).
//   - [hsms.WithT2] — E4 protocol/reply timeout (default 10 s).
//   - [hsms.WithT3] — S-II reply timeout (default 45 s); the most commonly tuned knob.
//   - [hsms.WithT4] — E4 inter-block timeout (default 45 s).
//   - [hsms.WithT5] — reconnect backoff ceiling; the exponential backoff between successive
//     connect attempts after a TCP disconnect ramps up to at most this duration.
//   - [hsms.WithSenderQueueSize] — maximum number of messages that may be queued ahead of the
//     line engine before SendDataMessage blocks.
//   - [hsms.WithCloseTimeout] — maximum time [hsms.Connection.Close] waits for a clean teardown
//     before forcing the transport down.
//   - [hsms.WithLogger] — replaces the default no-op logger.
//
// [secs1.WithT1]/[secs1.WithT2]/[secs1.WithT4]/[secs1.WithT5] remain the preferred spelling for
// build-time config; this wrapper form exists so the SAME options also work with
// [hsms.Connection.UpdateConfigOptions] at runtime (SECS-I's [Connection] exposes only
// [hsms.ConnOption] there, not [secs1.Option]).
//
// Knobs that are inert or overridden for SECS-I (do not use):
//   - [hsms.WithWriteTimeout] — [NewConfig] forces writeTimeout to 0 as its last construction
//     step; any value set here is overridden.
//   - [hsms.WithSessionID] — [NewConfig] forces the session ID to the SECS-I device ID (see
//     [WithDeviceID]) as its last construction step; any value set here is overridden.
//   - [hsms.WithLinktestInterval], [hsms.WithLinktestFailThreshold] — SECS-I never arms an
//     auto-linktest; both settings are ignored.
//   - [hsms.WithT6], [hsms.WithT7], [hsms.WithT8] — HSMS-SS-specific timers with no
//     counterpart in SEMI E4; setting them has no effect on the SECS-I transport.
//
// Example:
//
//	cfg, err := secs1.NewConfig("192.0.2.1", 5000,
//	    secs1.WithPassive(),
//	    secs1.WithConnectionOption(hsms.WithT3(30*time.Second)),
//	)
func WithConnectionOption(opt hsms.ConnOption) Option {
	return func(c *Config) error {
		return opt(&c.ConnectionConfig)
	}
}

// Host returns the TCP host that this Config targets (active) or binds to (passive).
func (c Config) Host() string { return c.host }

// Port returns the TCP port that this Config targets (active) or listens on (passive).
func (c Config) Port() int { return c.port }

// Active reports whether the connection role is active (dials outbound).
//
// A false return means the role is passive (listens for inbound connections).
func (c Config) Active() bool { return c.active }

// TCPKeepAlive returns the configured TCP keep-alive probe interval. Zero means use the OS default.
func (c Config) TCPKeepAlive() time.Duration { return c.tcpKeepAlive }

// ConnectTimeout returns the configured per-attempt active dial timeout.
// Zero means the dial is unbounded (see [WithConnectTimeout]).
func (c Config) ConnectTimeout() time.Duration { return c.connectTimeout }

// T1 returns the SECS-I T1 inter-character timeout.
func (c Config) T1() time.Duration { return c.ConnectionConfig.Timers().T1 }

// T2 returns the SECS-I T2 protocol/reply timeout.
func (c Config) T2() time.Duration { return c.ConnectionConfig.Timers().T2 }

// T4 returns the SECS-I T4 inter-block timeout.
func (c Config) T4() time.Duration { return c.ConnectionConfig.Timers().T4 }

// RetryLimit returns the SECS-I RTY block-send retry limit.
func (c Config) RetryLimit() int { return c.retryLimit }

// IsEquip reports whether the SECS-I role is equipment (master).
//
// A false return means host (slave).
func (c Config) IsEquip() bool { return c.isEquip }

// DeviceID returns the configured SECS-I device ID (0..0x7FFF).
func (c Config) DeviceID() uint16 { return c.deviceID }
