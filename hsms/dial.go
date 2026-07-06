package hsms

import (
	"context"
	"net"
)

// DialFunc establishes the active-mode TCP connection to the remote host. Its signature matches
// [net.Dialer.DialContext], so the standard dialer is a valid DialFunc as-is. hsmsss.DialFunc and
// secs1.DialFunc are aliases of this type, so a DialFunc built for one transport works with the
// other's WithDialer option too.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// ListenFunc establishes the passive-mode TCP listener. Its signature matches
// [net.ListenConfig.Listen], so the standard listen config is a valid ListenFunc as-is.
// hsmsss.ListenFunc and secs1.ListenFunc are aliases of this type. Unlike a single net.Listener
// value, a ListenFunc is called AGAIN on every reconnect generation — a passive connection
// re-listens after each drop, and a listener closed by a prior generation's teardown cannot be
// reused for the next one.
type ListenFunc func(ctx context.Context, network, address string) (net.Listener, error)
