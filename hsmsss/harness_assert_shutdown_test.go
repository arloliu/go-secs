package hsmsss

// assert_clean_shutdown_test.go — black-box clean-shutdown gate for hsmsss integration tests
// (spec §8 / territory map §6.7, re-derived for the v2 public surface). The gate observes only
// what is reachable through the hsms.Connection interface; the deep per-generation checks (live
// tasks, reply registry) live in the hsms white-box gate (hsms/assert_clean_shutdown_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// assertCleanShutdown hard-fails unless the connection has returned to a quiesced public state
// after Close: State()==NotConnectedState and no in-flight data messages. The deep per-generation
// checks (live tasks, reply registry) are covered white-box in the hsms package gate; here only
// the public observables are reachable through the hsms.Connection interface.
func assertCleanShutdown(t *testing.T, c hsms.Connection) {
	t.Helper()
	require.Eventually(t, func() bool {
		return c.State() == hsms.NotConnectedState && c.Metrics().DataMsgInflightCount() == 0
	}, 2*time.Second, 5*time.Millisecond, "hsmsss connection did not reach clean public shutdown")
}

// TestCleanShutdown_EndToEnd exercises the gate on a real active+passive New pair (the T25
// end-to-end harness): Open → Select → W-bit exchange → Close, then both connections must pass
// assertCleanShutdown.
func TestCleanShutdown_EndToEnd(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	passiveCfg, err := NewConfig("127.0.0.1", port,
		WithPassive(),
		WithConnectionOption(hsms.WithT3(2*time.Second)),
	)
	require.NoError(t, err)

	activeCfg, err := NewConfig("127.0.0.1", port,
		WithConnectionOption(hsms.WithT3(2*time.Second)),
	)
	require.NoError(t, err)

	passive, err := New(passiveCfg)
	require.NoError(t, err)

	active, err := New(activeCfg)
	require.NoError(t, err)

	// The passive handler replies S1F2 to the S1F1 primary so the W-bit exchange completes.
	passive.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
	})

	// Passive must listen before active dials.
	require.NoError(t, passive.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.Open(ctx, hsms.OpenBackground))

	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState && passive.State() == hsms.SelectedState
	}, 5*time.Second, 10*time.Millisecond, "both endpoints must reach Selected")

	// W-bit round-trip to exercise the in-flight metric path.
	reply, err := active.SendDataMessage(ctx, 1, 1, true, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	require.NoError(t, active.Close())
	require.NoError(t, passive.Close())

	// Both connections must pass the clean-shutdown gate.
	assertCleanShutdown(t, active)
	assertCleanShutdown(t, passive)
}
