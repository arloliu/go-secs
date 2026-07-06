package hsmsss

import (
	"errors"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
)

// Config holds the full configuration for an HSMS-SS connection.
// It embeds [hsms.ConnectionConfig] so all shared timer and policy knobs
// are promoted directly onto Config and can be read via the promoted accessors:
// [hsms.ConnectionConfig.Timers], [hsms.ConnectionConfig.SessionID], and
// [hsms.ConnectionConfig.WriteTimeout]. Other knobs (linktest interval/threshold,
// close timeout, logger, queue size) are write-only via [WithConnectionOption] and
// have no read accessor.
// Transport-specific settings (TCP endpoint, role, keep-alive) are
// separate unexported fields set through [Option] values.
type Config struct {
	hsms.ConnectionConfig

	host         string
	port         int
	active       bool
	tcpKeepAlive time.Duration
	dial         DialFunc
	listen       ListenFunc
}

// Option is a functional option that mutates a [Config].
// It returns an error if the provided value is invalid.
type Option func(*Config) error

// DialFunc is an alias of [hsms.DialFunc] — see its doc for the contract. Kept as a named type
// in this package so existing signatures (WithDialer) don't need touching, and so callers can
// keep writing hsmsss.DialFunc without importing hsms for this alone.
type DialFunc = hsms.DialFunc

// ListenFunc is an alias of [hsms.ListenFunc] — see its doc for the contract.
type ListenFunc = hsms.ListenFunc

// NewConfig constructs a Config for the given TCP host and port, applying the
// supplied options transactionally (all-or-nothing: every option is validated
// on a scratch copy; if any option returns an error the original is untouched
// and the joined set of errors is returned).
//
// The embedded [hsms.ConnectionConfig] is initialised to the E37-recommended
// defaults (T3=45 s, T5=10 s, T6=5 s, T7=10 s, T8=5 s, sessionID=0xFFFF).
// The default role is active (dials outbound); pass [WithPassive] to listen.
func NewConfig(host string, port int, opts ...Option) (Config, error) {
	base := hsms.DefaultConnectionConfig()
	cfg := Config{
		ConnectionConfig: *base,
		host:             host,
		port:             port,
		active:           true, // default: active role (dials outbound)
		dial:             (&net.Dialer{}).DialContext,
		listen:           (&net.ListenConfig{}).Listen,
	}

	if err := cfg.apply(opts...); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// apply validates all options against a scratch copy of cfg (all-or-nothing).
// On success the live cfg is updated atomically; on any error cfg is unchanged
// and the joined error is returned.
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

// WithActive sets the connection role to active (dials outbound).
// Active is the default role; this option is provided for explicit clarity.
func WithActive() Option {
	return func(c *Config) error {
		c.active = true
		return nil
	}
}

// WithPassive sets the connection role to passive (listens for inbound
// connections). Passive and active are mutually exclusive; calling
// [WithActive] and [WithPassive] in the same [NewConfig] call results in
// whichever option appears last winning, since both simply assign the field.
func WithPassive() Option {
	return func(c *Config) error {
		c.active = false
		return nil
	}
}

// WithDialer overrides how the active connection is established. The default dials with
// [net.Dialer.DialContext] over TCP. Supply a custom [DialFunc] to layer the connection on a
// different transport — for example a TLS or proxy dialer, or an in-memory connection for tests.
// Passing nil is a configuration error. This option affects the active (dialing) role only; the
// passive role always listens on the configured TCP endpoint.
func WithDialer(dial DialFunc) Option {
	return func(c *Config) error {
		if dial == nil {
			return errors.New("WithDialer: dial must not be nil")
		}

		c.dial = dial

		return nil
	}
}

// WithListener overrides how the passive connection listens for an inbound peer. The default
// uses [net.ListenConfig.Listen] over TCP. Supply a custom [ListenFunc] to layer the listener on
// a different transport — for example an in-memory pipe-backed listener for tests. Unlike
// WithDialer's single active dial, a passive connection re-listens on every reconnect
// generation, so listen is a FACTORY called fresh each time, never a single reused net.Listener.
// Passing nil is a configuration error. This option affects the passive (listening) role only;
// the active role always dials the configured TCP endpoint via WithDialer.
func WithListener(listen ListenFunc) Option {
	return func(c *Config) error {
		if listen == nil {
			return errors.New("WithListener: listen must not be nil")
		}

		c.listen = listen

		return nil
	}
}

// WithTCPKeepAlive sets the TCP keep-alive interval for the underlying
// socket. A value of 0 uses the OS default (keep-alive probes at the OS
// interval). Must be >= 0.
func WithTCPKeepAlive(d time.Duration) Option {
	return func(c *Config) error {
		if d < 0 {
			return errors.New("WithTCPKeepAlive: duration must be >= 0")
		}

		c.tcpKeepAlive = d

		return nil
	}
}

// WithEquipRole sets the connection role to equipment: a synchronous data-message send whose reply
// times out (T3) automatically notifies the peer with an S9F9 (Transaction Timeout, SEMI E5 §10.13)
// before the timeout error is returned to the caller. Host (the default) sends no such notification.
// Equipment and host are mutually exclusive — whichever of WithEquipRole/WithHostRole appears last in
// a NewConfig call wins.
func WithEquipRole() Option {
	return func(c *Config) error {
		return hsms.WithAutoS9F9(true)(&c.ConnectionConfig)
	}
}

// WithHostRole sets the connection role to host (the default). See WithEquipRole.
func WithHostRole() Option {
	return func(c *Config) error {
		return hsms.WithAutoS9F9(false)(&c.ConnectionConfig)
	}
}

// Host returns the TCP host that this Config targets (active) or binds to
// (passive).
func (c Config) Host() string { return c.host }

// Port returns the TCP port that this Config targets (active) or listens on
// (passive).
func (c Config) Port() int { return c.port }

// Active reports whether the connection role is active (dials outbound).
// A false return means the role is passive (listens for inbound connections).
func (c Config) Active() bool { return c.active }

// TCPKeepAlive returns the configured TCP keep-alive probe interval.
// Zero means use the OS default.
func (c Config) TCPKeepAlive() time.Duration { return c.tcpKeepAlive }

// IsEquip reports whether this connection is configured as equipment role (see WithEquipRole). A
// false return means host (the default).
func (c Config) IsEquip() bool {
	return c.AutoS9F9()
}

// ApplyOptions applies the supplied options to the Config transactionally
// (all-or-nothing: see [NewConfig]).  This is the public counterpart to the
// unexported apply method and is used by UpdateConfigOptions-style callers
// that adjust settings after initial construction.
func (c *Config) ApplyOptions(opts ...Option) error {
	return c.apply(opts...)
}

// WithConnectionOption wraps an [hsms.ConnOption] as an [Option] so callers
// can set any shared knob (T3–T8, sessionID, linktest, close timeout, logger,
// queue size) through the same [NewConfig] call without needing a separate
// re-export for every shared option.
//
// Example:
//
//	cfg, err := hsmsss.NewConfig("192.0.2.1", 5000,
//	    hsmsss.WithPassive(),
//	    hsmsss.WithConnectionOption(hsms.WithT3(30*time.Second)),
//	    hsmsss.WithConnectionOption(hsms.WithSessionID(0x0001)),
//	)
func WithConnectionOption(opt hsms.ConnOption) Option {
	return func(c *Config) error {
		return opt(&c.ConnectionConfig)
	}
}
