package hsmsss

import (
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// Connection is the HSMS-SS consumer-facing connection handle. It embeds hsms.Connection (every
// shared HSMS-II send/reply/handler operation is available unchanged) and adds ControlMetrics, the
// HSMS-SS (SEMI E37.1) control-plane counters — linktest and the Select/Separate/Reject handshake —
// that have no SECS-I analogue and so are not part of hsms.ConnectionMetrics.
//
// New returns this interface. Existing code that only needs hsms.Connection is unaffected —
// Connection satisfies hsms.Connection automatically (interface widening), so a variable, parameter,
// or return typed hsms.Connection accepts an hsmsss.Connection value with no change. Code that wants
// HSMS-SS control-plane metrics either declares its variable as hsmsss.Connection directly, or
// type-asserts an existing hsms.Connection value it obtained from New: conn.(hsmsss.Connection).
type Connection interface {
	hsms.Connection

	// ControlMetrics returns the HSMS-SS control-plane counters (linktest, Select, Separate, Reject).
	ControlMetrics() *ConnectionMetrics
}

// New builds an HSMS-SS connection from cfg and returns the consumer-facing hsmsss.Connection
// (which is also a valid hsms.Connection — see the Connection doc). It constructs the hsmsss
// transport and hands it to the shared hsms engine via hsms.NewConnection; the active/passive role,
// host:port, timers, session ID, and linktest policy all come from cfg. This is the consumer entry
// point for the HSMS-SS transport: the engine lives once in the hsms package, and the application
// holds only the Connection/hsms.Connection interface.
//
// cfg MUST originate from [NewConfig], which seeds the default TCP dialer. Passing a hand-built
// [Config] literal leaves the dialer nil and will cause New to return a clear error for the active
// role. Always use [NewConfig].
func New(cfg Config) (Connection, error) {
	if cfg.active && cfg.dial == nil {
		return nil, errors.New("hsmsss: active Config has a nil dialer; always construct Config via NewConfig")
	}

	t := newTransport(cfg)

	core, err := hsms.NewConnection(&cfg.ConnectionConfig, t)
	if err != nil {
		return nil, err
	}

	return &connection{Connection: core, metrics: t.metrics}, nil
}

// connection decorates the shared hsms.Connection with ControlMetrics (the Connection interface).
// All hsms.Connection methods are promoted from the embedded engine unchanged.
type connection struct {
	hsms.Connection

	metrics *ConnectionMetrics
}

// ControlMetrics returns the HSMS-SS control-plane counters (see the Connection interface doc).
func (c *connection) ControlMetrics() *ConnectionMetrics {
	return c.metrics
}
