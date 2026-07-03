package hsmsss

// new_test.go — tests for the hsmsss.New consumer entry point (spec §6 / D5a) and the FIRST
// true end-to-end HSMS-SS exercise: a real active+passive pair on a loopback port that dials,
// completes the Select handshake, exchanges a W-bit data message (passive handler replies via
// ReplyDataMessage; the active SendDataMessage receives the correlated reply), and closes cleanly.
//
// The file is package hsmsss (like the rest of the suite) so it can reuse freeLoopbackPort from
// passive_test.go, but every assertion below drives only the exported hsms.Connection surface.
// All tests use require.Eventually on State() / counters (never time.Sleep) and run under -race.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestNew_NilDialerReturnsError verifies that a hand-built active Config with a nil dialer causes
// New to return a clear error instead of panicking. Passive role is unaffected.
func TestNew_NilDialerReturnsError(t *testing.T) {
	t.Parallel()

	// A zero-value Config has active=false, so set active=true manually to exercise the guard.
	// This is the hand-built literal path that bypasses NewConfig's default dialer.
	var cfg Config
	cfg.active = true
	// cfg.dial is nil (zero value)

	_, err := New(cfg)
	require.Error(t, err, "active Config with nil dialer must return an error")
	require.Contains(t, err.Error(), "nil dialer")
	require.Contains(t, err.Error(), "NewConfig")
}

// TestNew_ReturnsUsableConnection verifies New builds a non-nil hsms.Connection (nil error) for
// both roles and that a freshly-built, not-yet-opened connection reports NotConnectedState.
func TestNew_ReturnsUsableConnection(t *testing.T) {
	t.Parallel()

	activeCfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	active, err := New(activeCfg)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.Equal(t, hsms.NotConnectedState, active.State(), "unopened connection is NotConnected")

	passiveCfg, err := NewConfig("127.0.0.1", 5000, WithPassive())
	require.NoError(t, err)
	passive, err := New(passiveCfg)
	require.NoError(t, err)
	require.NotNil(t, passive)
	require.Equal(t, hsms.NotConnectedState, passive.State(), "unopened connection is NotConnected")
}

// TestNew_EndToEndSelectAndWBitExchange is the first end-to-end proof of the whole HSMS-SS stack:
// dial/listen -> Select handshake -> W-bit data send -> recv -> DeliverOwnedFrame reply-correlation
// -> reply -> clean Close. The passive side registers a handler that replies to the primary; the
// active side's SendDataMessage must receive the correlated secondary.
func TestNew_EndToEndSelectAndWBitExchange(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ctx := t.Context()

	// Short T3 so a routing failure surfaces as a fast error rather than a 45 s hang; auto-linktest
	// stays disabled by default (interval 0) to keep the exchange quiet.
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

	// The passive handler replies S1F2("pong") to the S1F1("ping") primary. ReplyDataMessage reuses
	// the primary's System Bytes verbatim (E37 §8.2.6.9), which is what lets the active sender's
	// reply registry correlate the secondary back to its waiting SendDataMessage.
	passive.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
	})

	// Passive must be listening before active dials.
	require.NoError(t, passive.Open(ctx, hsms.OpenBackground))
	require.NoError(t, active.Open(ctx, hsms.OpenBackground))

	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState && passive.State() == hsms.SelectedState
	}, 5*time.Second, 10*time.Millisecond, "both endpoints must reach Selected")

	// W-bit round-trip: SendDataMessage blocks until the correlated reply arrives.
	reply, err := active.SendDataMessage(ctx, 1, 1, true /* W-bit */, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, uint8(1), reply.Stream(), "reply stream is S1")
	require.Equal(t, uint8(2), reply.Function(), "reply function is F2 (primary F+1)")

	replyItem, err := reply.Item()
	require.NoError(t, err)
	replyStr, err := replyItem.ToASCII()
	require.NoError(t, err)
	require.Equal(t, "pong", replyStr, "reply payload must be what the passive handler sent")

	// The passive received both the primary and correlated nothing back to a local waiter, so the
	// data-receive chokepoint counter (wired in DeliverOwnedFrame) advanced. (Send-side counters are
	// asserted directly in metrics_test.go, so they are out of scope here.)
	require.Positive(t, passive.Metrics().DataMsgRecvCount(), "passive must have received a data message")

	require.NoError(t, active.Close())
	require.NoError(t, passive.Close())

	require.Eventually(t, func() bool {
		return active.State() == hsms.NotConnectedState && passive.State() == hsms.NotConnectedState
	}, 5*time.Second, 10*time.Millisecond, "both endpoints must return to NotConnected after Close")
}
