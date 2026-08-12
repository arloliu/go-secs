package hsmsss

// Coverage for the listen-failure wrap in startPassive.
//
// Every existing WithListener test supplies a listener func that succeeds.
// This is the sibling path: a listen error, such as a bound port or a denied permission on a
// passive restart, surfacing to the engine's reconnect loop.
//
// It is not the sealed-start branch, which transport_sealed_start_test.go covers.
// The listener func here fails before the seal check is ever reached.

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// errListenBoom is the sentinel the test's ListenFunc returns, so the wrap can be asserted with
// errors.Is instead of string matching on the wrapped cause.
var errListenBoom = errors.New("listen boom")

// TestStartPassive_ListenFailureWrapsError proves startPassive wraps a listen failure into
// "hsmsss: listen %s: %w" (transport_passive.go ~line 49) and returns it to the caller without
// storing any listener state.
// The ListenFunc fails synchronously — no real port binding, no Sleep — so the wrap branch is
// taken deterministically.
func TestStartPassive_ListenFailureWrapsError(t *testing.T) {
	t.Parallel()

	listen := func(context.Context, string, string) (net.Listener, error) {
		return nil, errListenBoom
	}

	cfg, err := NewConfig("127.0.0.1", 0, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	tr := newTransport(cfg)

	err = tr.startPassive(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, errListenBoom, "the wrap must preserve the underlying listen error for errors.Is")
	require.ErrorContains(t, err, "hsmsss: listen 127.0.0.1:0", "the wrap message must match the exact fmt.Errorf format in startPassive")

	tr.connMu.Lock()
	ln := tr.listener
	tr.connMu.Unlock()
	require.Nil(t, ln, "no listener state must be retained when listen fails")
}
