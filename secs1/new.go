package secs1

import (
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// New builds a SECS-I (SEMI E4) connection from cfg and returns the consumer-facing
// hsms.Connection. It constructs the secs1 transport and hands it to the shared hsms engine via
// hsms.NewConnection; the active/passive role, host:port, T1–T4 / RTY line policy, device ID, and
// equipment/host role all come from cfg. This is THE consumer entry point for the SECS-I transport
// (the sealed package boundary: the engine lives once in hsms, the app holds only the
// hsms.Connection interface).
//
// cfg MUST originate from [NewConfig], which seeds the default TCP dialer. Passing a hand-built
// [Config] literal leaves the dialer nil and will cause New to return a clear error for the active
// role, or a nil-pointer panic on the passive accept path. Always use [NewConfig].
func New(cfg Config) (hsms.Connection, error) {
	if cfg.active && cfg.dial == nil {
		return nil, errors.New("secs1: active Config has a nil dialer; always construct Config via NewConfig")
	}

	core, err := hsms.NewConnection(&cfg.ConnectionConfig, newTransport(cfg))
	if err != nil {
		return nil, err
	}

	return &connection{Connection: core, deviceID: cfg.deviceID}, nil
}

// connection decorates the shared hsms.Connection so the SECS-I identity invariant survives live
// reconfiguration. NewConfig forces the core session ID to the device ID at construction, but the
// core's UpdateConfigOptions would otherwise let a caller re-arm hsms.WithSessionID at runtime and
// diverge SessionID() from the device ID the SECS-I block header carries on the wire. All other
// hsms.Connection methods are promoted from the embedded engine unchanged.
type connection struct {
	hsms.Connection

	deviceID uint16
}

// UpdateConfigOptions applies the caller's options and then re-forces the core session ID to the
// SECS-I device ID as the last option, so a caller's hsms.WithSessionID cannot divert SessionID()
// from the wire device ID at runtime. This mirrors the last-step override NewConfig applies at
// construction. The underlying update is still transactional (validate-all, then commit atomically):
// a failed caller option rejects the whole set and leaves the live config untouched.
func (c *connection) UpdateConfigOptions(opts ...hsms.ConnOption) error {
	all := make([]hsms.ConnOption, 0, len(opts)+1)
	all = append(all, opts...)
	all = append(all, hsms.WithSessionID(c.deviceID))

	return c.Connection.UpdateConfigOptions(all...)
}
