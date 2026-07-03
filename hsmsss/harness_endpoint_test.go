package hsmsss

// integration_helpers_test.go — v2 port of the v1 tests/hsmsss_integration/helpers_test.go
// real active+passive pair harness, re-pointed onto the v2 public surface (New / NewConfig /
// Open(ctx, mode) / hsms.Connection / SECS2Endpoint data handlers / (prev,next) state handlers).
//
// The v1 harness modelled host/equip roles and a separate hsms.Session handle; the v2 single-session
// model drops both — a Connection IS its own endpoint. All lifecycle waits poll conn.State() through
// require.Eventually (never time.Sleep-to-sync). The file is package hsmsss (white-box, like the rest
// of the suite) so it reuses freeLoopbackPort (passive_test.go) and assertCleanShutdown
// (assert_clean_shutdown_test.go); every assertion drives only the exported hsms.Connection surface.

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// endpoint wraps a real hsms.Connection plus a best-effort state-change observer channel. It is the
// v2 analogue of the v1 endpoint struct (which also carried an hsms.Session — gone in v2's
// single-session model). The states channel is fed by a non-blocking, drop-on-full handler
// registered in newEndpoint; tests that only need to wait for a state use waitState/waitSelected
// (which poll conn.State() directly, the authoritative source), while the channel remains available
// for draining/observation across cycles.
type endpoint struct {
	conn   hsms.Connection
	states chan hsms.ConnState
}

// newEndpoint builds a real hsms.Connection (active or passive) on 127.0.0.1:port with short-but-safe
// integration timers and auto-linktest disabled (LinktestInterval 0), registers a best-effort
// state-change observer, and attaches any supplied data-message handlers. It mirrors the v1
// newEndpoint minus the dropped ctx (moved to Open) and isEquip (no host/equip role in v2).
//
// opts are appended after the base timers so a caller can override any knob (e.g. a longer T6).
func newEndpoint(t *testing.T, port int, active bool, opts []Option, handlers ...hsms.DataMessageHandler) *endpoint {
	t.Helper()

	base := []Option{
		WithConnectionOption(hsms.WithT3(3 * time.Second)),
		WithConnectionOption(hsms.WithT5(200 * time.Millisecond)),
		WithConnectionOption(hsms.WithT6(3 * time.Second)),
		WithConnectionOption(hsms.WithT7(5 * time.Second)),
		WithConnectionOption(hsms.WithT8(1 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(0)), // v1 WithAutoLinktest(false)
	}

	if active {
		base = append(base, WithActive())
	} else {
		base = append(base, WithPassive())
	}

	base = append(base, opts...)

	cfg, err := NewConfig("127.0.0.1", port, base...)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	ep := &endpoint{
		conn:   conn,
		states: make(chan hsms.ConnState, 64),
	}

	// Best-effort observer: non-blocking so it can never stall the supervisor notifier.
	conn.AddConnStateChangeHandler(func(_, next hsms.ConnState) {
		select {
		case ep.states <- next:
		default:
		}
	})

	if len(handlers) > 0 {
		conn.AddDataMessageHandler(handlers...)
	}

	return ep
}

// newEndpointPair builds a passive+active endpoint pair sharing one freeLoopbackPort. The passive is
// returned first (callers must Open it before the active dials). opts apply to both endpoints.
func newEndpointPair(t *testing.T, opts ...Option) (passive, active *endpoint) {
	t.Helper()

	port := freeLoopbackPort(t)
	passive = newEndpoint(t, port, false, opts)
	active = newEndpoint(t, port, true, opts)

	return passive, active
}

// waitState blocks (require.Eventually on conn.State(), never time.Sleep) until the endpoint reaches
// the target state or a generous timeout elapses.
func waitState(t *testing.T, ep *endpoint, state hsms.ConnState) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		return ep.conn.State() == state
	}, 10*time.Second, 5*time.Millisecond, "timeout waiting for state %s", state)
}

// waitSelected is waitState(ep, SelectedState).
func waitSelected(t *testing.T, ep *endpoint) {
	t.Helper()
	waitState(t, ep, hsms.SelectedState)
}

// closeEndpoint closes an endpoint's connection and asserts a clean (nil) return. Close is
// idempotent in v2 (a second Close returns the prior error), so calling this after the connection
// was already closed cleanly still yields nil.
func closeEndpoint(t *testing.T, ep *endpoint) {
	t.Helper()
	if ep == nil || ep.conn == nil {
		return
	}
	require.NoError(t, ep.conn.Close())
}

// drainStateCh empties an endpoint's best-effort observer channel so a subsequent cycle starts from
// an empty queue (mirrors the v1 lifecycle helper).
func drainStateCh(ch chan hsms.ConnState) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// echoHandler replies to primary (odd-function) data messages by echoing the received item back via
// ReplyDataMessage. It is the v2 port of the v1 echoHandler (Session -> SECS2Endpoint; FunctionCode
// -> Function; Item now returns (item, err)). Non-primary messages are ignored.
func echoHandler(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
	if msg.Function()%2 != 1 {
		return
	}

	item, err := msg.Item()
	if err != nil {
		return
	}

	_ = ep.ReplyDataMessage(context.Background(), msg, item)
}
