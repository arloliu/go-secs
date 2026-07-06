package secs1

import (
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// Connection is the SECS-I consumer-facing connection handle.
//
// It embeds hsms.Connection (every shared HSMS-II send/reply/handler operation is
// available unchanged) and adds BlockMetrics, the SECS-I (SEMI E4) block-level
// counters — a wire-framing layer below the shared hsms engine, so it is not part of
// hsms.ConnectionMetrics.
//
// New returns this interface.
// Existing code that only needs hsms.Connection is unaffected — Connection satisfies
// hsms.Connection automatically (interface widening), so a variable/parameter/return
// typed hsms.Connection accepts a secs1.Connection value with no change.
//
// Code that wants SECS-I block metrics either declares its variable as secs1.Connection
// directly, or type-asserts an existing hsms.Connection value it obtained from New:
// conn.(secs1.Connection).
type Connection interface {
	hsms.Connection

	// BlockMetrics returns the SECS-I block/line-level counters.
	BlockMetrics() *ConnectionMetrics
}

// New builds a SECS-I (SEMI E4) connection from the provided configuration.
//
// It returns the consumer-facing secs1.Connection (which is also a valid hsms.Connection —
// see the Connection doc).
//
// It constructs the secs1 transport and hands it to the shared hsms engine via
// hsms.NewConnection.
// The active/passive role, host:port, T1–T4 / RTY line policy, device ID, and
// equipment/host role all come from cfg.
//
// This is THE consumer entry point for the SECS-I transport: the sealed package
// boundary is established here where the engine lives once in hsms and the app holds
// only the Connection/hsms.Connection interface.
//
// cfg MUST originate from [NewConfig], which seeds the default TCP dialer.
// Passing a hand-built [Config] literal leaves the dialer nil and will cause New to
// return a clear error for the active role, or a nil-pointer panic on the passive
// accept path.
//
// Always use [NewConfig].
func New(cfg Config) (Connection, error) {
	if cfg.active && cfg.dial == nil {
		return nil, errors.New("secs1: active Config has a nil dialer; always construct Config via NewConfig")
	}

	t := newTransport(cfg)

	core, err := hsms.NewConnection(&cfg.ConnectionConfig, t)
	if err != nil {
		return nil, err
	}

	return &connection{Connection: core, deviceID: cfg.deviceID, metrics: t.metrics}, nil
}

// connection decorates the shared hsms.Connection so the SECS-I identity invariant survives live
// reconfiguration, and adds BlockMetrics (the Connection interface). NewConfig forces the core
// session ID to the device ID at construction, but the core's UpdateConfigOptions would otherwise let
// a caller re-arm hsms.WithSessionID at runtime and diverge SessionID() from the device ID the
// SECS-I block header carries on the wire. All other hsms.Connection methods are promoted from the
// embedded engine unchanged.
type connection struct {
	hsms.Connection

	deviceID uint16
	metrics  *ConnectionMetrics
}

// BlockMetrics returns the SECS-I block/line-level counters (see the Connection doc).
func (c *connection) BlockMetrics() *ConnectionMetrics {
	return c.metrics
}

// UpdateConfigOptions applies the caller's options and then re-forces the two invariants
// NewConfig establishes at construction, as the last two options: the core session ID stays the
// SECS-I device ID (so a caller's hsms.WithSessionID cannot divert SessionID() from the wire
// device ID at runtime), and the core write timeout stays 0 (so a caller's hsms.WithWriteTimeout
// cannot re-arm a deadline that would fire mid-transaction inside the line engine's own
// T2 x (RetryLimit+1) budget and desync the line — see NewConfig's D5b-11 comment). The
// underlying update is still transactional (validate-all, then commit atomically): a failed
// caller option rejects the whole set and leaves the live config untouched.
func (c *connection) UpdateConfigOptions(opts ...hsms.ConnOption) error {
	all := make([]hsms.ConnOption, 0, len(opts)+2)
	all = append(all, opts...)
	all = append(all, hsms.WithSessionID(c.deviceID), hsms.WithWriteTimeout(0))

	return c.Connection.UpdateConfigOptions(all...)
}
