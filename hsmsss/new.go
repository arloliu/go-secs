package hsmsss

import (
	"errors"

	"github.com/arloliu/go-secs/v2/hsms"
)

// New builds an HSMS-SS connection from cfg and returns the consumer-facing hsms.Connection. It
// constructs the hsmsss transport and hands it to the shared hsms engine via hsms.NewConnection; the
// active/passive role, host:port, timers, session ID, and linktest policy all come from cfg. This is the
// consumer entry point for the HSMS-SS transport: the engine lives once in the hsms package, and the
// application holds only the hsms.Connection interface.
//
// cfg MUST originate from [NewConfig], which seeds the default TCP dialer. Passing a hand-built
// [Config] literal leaves the dialer nil and will cause New to return a clear error for the active
// role. Always use [NewConfig].
func New(cfg Config) (hsms.Connection, error) {
	if cfg.active && cfg.dial == nil {
		return nil, errors.New("hsmsss: active Config has a nil dialer; always construct Config via NewConfig")
	}

	return hsms.NewConnection(&cfg.ConnectionConfig, newTransport(cfg))
}
