package hsmsss

// chaos_scenarios_test.go — v2 port of the v1 tests/hsmsss_integration/chaos_test.go and
// chaos_advanced_test.go frame-level fault matrix, re-pointed onto the v2 public surface
// (New / Open(ctx, mode) / hsms.Connection / SendDataMessage(ctx, ...) / SECS2Endpoint handlers).
//
// It drives the ChaosProxy (a filter-driven MITM TCP proxy, ported in T28 — see chaos_proxy_test.go)
// between a real active and passive New() pair, injecting dropped / delayed / truncated frames and
// abrupt TCP resets to prove the v2 engine's T3 / T6 / T8 timeout paths, drop-detection, linktest
// failure, and no-panic-under-close behavior end-to-end.
//
// Wiring topology (active -> proxy -> passive): the passive listens on portP; the ChaosProxy targets
// portP and binds its own port; the active dials proxy.Port(). In the filter, isClientToTarget==true
// is active->passive, false is passive->active; header[5] is the SType (0=data, 1=Select.req,
// 2=Select.rsp, 5=Linktest.req, 6=Linktest.rsp) and header[3] is the function (even == a secondary
// reply). Tests that need no fault on the link (RapidLinktestToggle, ConcurrentSendDuringClose) wire a
// direct pair with no proxy.
//
// CRITICAL v2 adaptation — auto-reconnect is ALWAYS ON (no WithReconnect(false) in v2). Every "->
// NotConnected" scenario therefore asserts the NotConnected TRANSITION via waitNotConnectedEvent (which
// reads the endpoint's state-change observer channel) rather than the instantaneous conn.State(): a one-shot
// fault is recovered by the reconnect loop, so State() could flip back to Selected before a poll
// observes it, but the transition event is still captured. The reconnect backoff is capped at T5
// (see hsms.WithReconnectBackoff); NotConnected dwells at least long enough for the event-driven wait
// below to observe it regardless of the exact backoff delay, and reconnect churn under a persistent
// fault (dropped Select.rsp / linktest.rsp / black hole) is expected — the tests never assert "stays
// NotConnected forever".
//
// The file is package hsmsss (white-box) so it reuses the shared harness (newEndpoint / waitSelected /
// waitState / closeEndpoint / drainStateCh / echoHandler from integration_helpers_test.go,
// freeLoopbackPort from passive_test.go, assertCleanShutdown from assert_clean_shutdown_test.go, and
// the ChaosProxy from chaos_proxy_test.go). All readiness waits are event- or State()-driven through
// require.Eventually / the observer channel (never time.Sleep-to-sync) and run under -race.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// waitNotConnectedEvent blocks until the endpoint's best-effort observer channel reports a TRANSITION
// into NotConnected, failing after a generous timeout. It is the v2 analogue of the v1
// requireStateEvent(NotConnected) idiom. Unlike waitState (which polls the instantaneous conn.State()),
// it observes a state-change EVENT, so it reliably catches a transient NotConnected that v2's always-on
// auto-reconnect immediately recovers from — an instantaneous poll could miss it. Drain the observer
// channel (drainStateCh) before triggering the fault so the matched event is the one the scenario
// induces, not a stale transition left over from the initial Select handshake.
func waitNotConnectedEvent(t *testing.T, ep *endpoint) {
	t.Helper()

	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-ep.states:
			if got == hsms.NotConnectedState {
				return
			}
		case <-timeout:
			t.Fatalf("timeout waiting for NotConnected state-change event (current State()=%s)", ep.conn.State())
		}
	}
}

// ---------------------------------------------------------------------------
// 1. TestChaos_DroppedSelectRsp
//
// The proxy drops every passive->active Select.rsp, so the active's Select.req transaction never
// completes: its T6 (control-reply timeout) fires, the active drives TCPDown, and the FSM returns to
// NotConnected. With auto-reconnect ON the active loops NotSelected<->NotConnected (never Selected).
// TEETH: waitNotConnectedEvent only passes because the drop forces a T6 teardown — if the
// Select.rsp were forwarded the active would reach Selected and stay there, and no NotConnected event
// would ever arrive (timeout).
// ---------------------------------------------------------------------------
func TestChaos_DroppedSelectRsp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.SelectRspType) {
			return ProxyActionDrop, 0
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	// Short T6 so the dropped Select.rsp surfaces fast; base T5=200ms keeps reconnect churn cheap.
	active := newEndpoint(t, proxy.Port(), true, []Option{WithConnectionOption(hsms.WithT6(800 * time.Millisecond))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	// The active connects (NotSelected) and sends Select.req, but its Select.rsp is dropped -> T6 ->
	// NotConnected. Observing the NotConnected transition proves the Select transaction failed.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 2. TestChaos_TruncatedDataMessage_T8Timeout
//
// After both reach Selected the active fires an S1F1 primary; the passive echoHandler replies S1F2,
// and the proxy TRUNCATES that passive->active reply (writes the 4-byte length prefix promising a full
// frame, then only the first 5 payload bytes). The active's reader has begun the frame, so T8 (the
// inter-byte gap timer) governs the remaining bytes; with the tail never arriving, T8 fires and tears
// the link down -> NotConnected. This directly exercises the active's T8 path (readFrame/readN).
// Reconcile note (per brief): the truncated frame is the one ARRIVING at the peer whose T8 is tested
// (here the active), so the passive must send it — hence the echoHandler reply.
// ---------------------------------------------------------------------------
func TestChaos_TruncatedDataMessage_T8Timeout(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)

	var truncated atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		// First passive->active data secondary (SType 0, even function = reply) is truncated once.
		if !isClientToTarget && len(header) >= 10 && header[5] == 0 && header[3]%2 == 0 {
			if truncated.CompareAndSwap(false, true) {
				return ProxyActionTruncate, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	// Short T8 on the active so the incomplete inbound frame surfaces fast.
	active := newEndpoint(t, proxy.Port(), true, []Option{WithConnectionOption(hsms.WithT8(500 * time.Millisecond))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	// Fire-and-forget S1F1: echoHandler still replies to the odd-function primary; the reply is the
	// frame the proxy truncates. (We do not await the reply — the point is the incomplete inbound frame.)
	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("0123456789"))

	// The truncated reply stalls the active's reader mid-frame -> T8 -> TCPDown -> NotConnected.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 3. TestChaos_DelayedT3Reply
//
// The proxy delays ONLY the first passive->active S1F2 reply (one-shot) by longer than T3, so the
// active's first W-bit SendDataMessage returns ErrT3Timeout. A T3 timeout is a TRANSACTION-level
// failure, NOT a link failure: the connection stays Selected. A second W-bit send (its reply forwarded
// normally after the one-shot delay drains) then succeeds.
//
// Not t.Parallel(): the assertion has a two-sided timing margin (delay > T3 so the first send times
// out, delay < ~2*T3 so the second send's reply — serialized behind the delayed first reply in the
// proxy's single passive->active pump — arrives before the second send's own T3 deadline). Running it
// in the sequential phase keeps the wall-clock timers accurate under -race -count.
// ---------------------------------------------------------------------------
func TestChaos_DelayedT3Reply(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)

	const t3 = 1 * time.Second
	const replyDelay = 1500 * time.Millisecond // > T3 (first send times out) and < 2*T3 (second reply beats its deadline)

	var delayed atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == 0 && header[3]%2 == 0 {
			if delayed.CompareAndSwap(false, true) {
				return ProxyActionForward, replyDelay
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{WithConnectionOption(hsms.WithT3(t3))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// First W-bit send: the reply is delayed past T3, so SendDataMessage must return ErrT3Timeout.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("1"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout, "a delayed W-bit reply must surface as ErrT3Timeout")
	require.Nil(t, reply)

	// A T3 timeout is transaction-level, not link-level: the connection must stay Selected.
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a T3 timeout must not tear down the link")

	// Second W-bit send (odd function so echoHandler replies): the one-shot delay is spent, so this
	// reply is forwarded promptly and the transaction completes.
	reply2, err2 := active.conn.SendDataMessage(ctx, 1, 3, true, secs2.A("2"))
	require.NoError(t, err2, "a subsequent send must succeed once the delayed reply drains")
	require.NotNil(t, reply2)
}

// ---------------------------------------------------------------------------
// 4. TestChaos_DroppedLinktestRsp
//
// Auto-linktest is enabled on the active (WithLinktestInterval); the proxy drops every passive->active
// Linktest.rsp. The active's Linktest.req transaction times out on T6, and with a fail threshold of 1
// the first miss drives an involuntary disconnect -> NotConnected. Exercises the T23/T24 linktest
// failure path end-to-end. TEETH: forwarding the Linktest.rsp keeps the link healthy and no
// NotConnected event fires.
// ---------------------------------------------------------------------------
func TestChaos_DroppedLinktestRsp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinktestRspType) {
			return ProxyActionDrop, 0
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT6(500 * time.Millisecond)),
		WithConnectionOption(hsms.WithLinktestInterval(200 * time.Millisecond)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(1)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	drainStateCh(active.states)

	// The auto-linktest fires, its Linktest.rsp is dropped, T6 expires, threshold(1) trips -> disconnect.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 5. TestChaos_AbruptClose
//
// After Selected the active fires a data frame; the proxy abruptly closes BOTH TCP sockets on that
// first active->passive data frame (one-shot). The active detects the drop and transitions to
// NotConnected without panicking; Close afterward is clean. With auto-reconnect ON the active recovers
// after the one-shot cut — the property under test is: the abrupt drop is observed, nothing panics,
// and a subsequent Close quiesces cleanly.
// ---------------------------------------------------------------------------
func TestChaos_AbruptClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)

	var closed atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if isClientToTarget && len(header) >= 10 && header[5] == 0 {
			if closed.CompareAndSwap(false, true) {
				return ProxyActionCloseTCP, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, nil)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("1"))

	// The abrupt TCP reset must be detected as a drop (not a panic) -> NotConnected.
	waitNotConnectedEvent(t, active)

	// Close must quiesce cleanly after an abrupt mid-transaction drop.
	require.NoError(t, active.conn.Close())
	assertCleanShutdown(t, active.conn)
}

// ---------------------------------------------------------------------------
// 6. TestChaos_BurstDisconnectReconnect
//
// Five cycles: reach Selected, fire async sends, then proxy.CloseConnections() to cut the TCP mid-
// flight. Each cut must be observed as a NotConnected transition; with auto-reconnect ON the link
// re-Selects between cuts (the proxy's accept loop re-dials the passive), which is expected. The
// property across cycles is: no panic, no goroutine leak, every cut observed, and a clean final
// shutdown.
// ---------------------------------------------------------------------------
func TestChaos_BurstDisconnectReconnect(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(_ bool, _ []byte, _ []byte) (ProxyAction, time.Duration) {
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, nil)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	for range 5 {
		waitSelected(t, active)
		waitSelected(t, passive)

		for range 3 {
			_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("burst"))
		}

		// Drain first so the matched NotConnected event is THIS cut, not a stale reconnect transition.
		drainStateCh(active.states)
		proxy.CloseConnections()
		waitNotConnectedEvent(t, active)
	}

	require.NoError(t, active.conn.Close())
	assertCleanShutdown(t, active.conn)
}

// ---------------------------------------------------------------------------
// 7. TestChaos_ConcurrentSendDuringClose
//
// Ten goroutines hammer SendDataMessageAsync in a loop while Close() is called mid-flight (a direct
// pair — no proxy needed for the send/close race). The property: NO panic (no "send on closed
// channel"), Close returns bounded and clean, all senders join (no deadlock), and the connection ends
// NotConnected. Sends that fail after Close are tolerated (they are errors, never panics). Runs under
// -race.
// ---------------------------------------------------------------------------
func TestChaos_ConcurrentSendDuringClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, nil)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// started fires as soon as one sender is in flight, so Close races the senders without a
	// time.Sleep-to-sync.
	started := make(chan struct{}, 1)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 100 {
				err := active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("race"))

				select {
				case started <- struct{}{}:
				default:
				}

				if err != nil {
					return // connection closed — errors after Close are tolerated (never panics)
				}
			}
		})
	}

	<-started
	require.NoError(t, active.conn.Close(), "Close must return cleanly (bounded) with concurrent senders in flight")

	wg.Wait() // every sender must return — no deadlock, no panic

	require.Equal(t, hsms.NotConnectedState, active.conn.State(), "Close must end in NotConnected")
	assertCleanShutdown(t, active.conn)
}

// ---------------------------------------------------------------------------
// 8. TestChaos_BlackHoleProxy
//
// The proxy forwards the Select handshake so both reach Selected, then black-holes EVERY subsequent
// frame. With auto-linktest enabled the active's Linktest.req is dropped -> T6 -> threshold(1) ->
// involuntary disconnect -> NotConnected. TEETH: without the black hole the linktest succeeds and no
// NotConnected event fires. (Select frames stay forwarded, so the active re-Selects and the black hole
// re-triggers — expected churn; we assert only the first NotConnected transition.)
// ---------------------------------------------------------------------------
func TestChaos_BlackHoleProxy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)

	var selected atomic.Bool
	proxy.SetFilter(func(_ bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if len(header) >= 10 {
			stype := header[5]
			if stype == byte(hsms.SelectReqType) || stype == byte(hsms.SelectRspType) {
				if stype == byte(hsms.SelectRspType) {
					selected.Store(true)
				}

				return ProxyActionForward, 0
			}
		}

		if selected.Load() {
			return ProxyActionDrop, 0 // black hole after selection
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT6(500 * time.Millisecond)),
		WithConnectionOption(hsms.WithLinktestInterval(200 * time.Millisecond)),
		WithConnectionOption(hsms.WithLinktestFailThreshold(1)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	drainStateCh(active.states)

	// The post-Select black hole starves the linktest -> T6 -> disconnect.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 9. TestChaos_MidWriteTCPReset
//
// The proxy resets TCP on the first host data frame while the active is blocked in a synchronous W-bit
// SendDataMessage. The send must return an ERROR (not hang, not panic), and the link must transition
// to NotConnected. Confirms the send path unblocks on a connection drop rather than waiting out T3.
// ---------------------------------------------------------------------------
func TestChaos_MidWriteTCPReset(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)

	var killed atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if isClientToTarget && len(header) >= 10 && header[5] == 0 {
			if killed.CompareAndSwap(false, true) {
				return ProxyActionCloseTCP, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{
		WithConnectionOption(hsms.WithT3(2 * time.Second)),
		WithConnectionOption(hsms.WithT6(1 * time.Second)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	// Synchronous W-bit send: the mid-write reset must surface as an error, not a hang.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("will fail"))
	require.Error(t, err, "a mid-write TCP reset must make SendDataMessage return an error, not hang")
	require.Nil(t, reply)

	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 10. TestChaos_RapidLinktestToggle
//
// Rapidly toggle the auto-linktest interval on/off via UpdateConfigOptions while concurrently sending
// (a direct pair — no proxy). v2 has NO WithAutoLinktest, so the toggle is expressed as
// WithLinktestInterval(d) (d>0 on, 0 off). NOTE (v2 behavior difference): v2 reads the linktest
// interval once per Selected-entry (never a live mid-session re-read — see connection.go
// LinktestInterval), so the toggle does not start/stop the running linktest mid-session as v1
// expected; this instead stresses the transactional UpdateConfigOptions config-swap path concurrent
// with the send path. The property is unchanged: no race (-race), and the connection stays healthy
// (a final W-bit send succeeds).
// ---------------------------------------------------------------------------
func TestChaos_RapidLinktestToggle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithT6(1 * time.Second)),
		WithConnectionOption(hsms.WithLinktestInterval(100 * time.Millisecond)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	var wg sync.WaitGroup

	// Toggle loop: hammer UpdateConfigOptions (interval off/on) transactionally.
	wg.Go(func() {
		for range 50 {
			_ = active.conn.UpdateConfigOptions(hsms.WithLinktestInterval(0))
			_ = active.conn.UpdateConfigOptions(hsms.WithLinktestInterval(50 * time.Millisecond))
		}
	})

	// Send loop: concurrent async traffic while the config is churning.
	wg.Go(func() {
		for range 30 {
			_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("toggle"))
		}
	})

	wg.Wait()

	// The connection must remain healthy after the storm.
	require.Equal(t, hsms.SelectedState, active.conn.State(), "the toggle storm must not tear down a healthy link")
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("alive"))
	require.NoError(t, err)
	require.NotNil(t, reply)
}

// ---------------------------------------------------------------------------
// 11. TestChaos_ReplyDuringClose
//
// The proxy delays every passive->active reply by 500ms; the active fires an async W-bit S1F1, then
// Close()s while the reply is still in flight (delayed in the proxy). The property under test is that
// Close quiesces CLEANLY and WITHOUT panic while a peer reply is in flight — the teardown races the
// delayed reply. The passive handler signals when it has replied, so Close races an in-flight reply
// without a time.Sleep-to-sync.
//
// Note on coverage: because the send is async (fire-and-forget, no reply waiter registered) and the
// active socket is torn down within ~ms of Close, the delayed reply is dropped at the proxy/transport
// boundary and does not actually reach a torn-down reply registry — so this exercises close-race
// robustness at the transport level, NOT the reply-registry-teardown path specifically. Driving a late
// reply into a torn-down registry deterministically would need a white-box hsms-package test.
// ---------------------------------------------------------------------------
func TestChaos_ReplyDuringClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	replied := make(chan struct{}, 1)
	// Passive handler: echo the primary AND signal that a reply is now in flight (being delayed).
	replyAndSignal := func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		item, err := msg.Item()
		if err != nil {
			return
		}
		_ = ep.ReplyDataMessage(context.Background(), msg, item)

		select {
		case replied <- struct{}{}:
		default:
		}
	}

	passive := newEndpoint(t, portP, false, nil, replyAndSignal)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	proxy := newChaosProxy(t, portP)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == 0 && header[3]%2 == 0 {
			return ProxyActionForward, 500 * time.Millisecond
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	active := newEndpoint(t, proxy.Port(), true, []Option{WithConnectionOption(hsms.WithT3(3 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// Fire a W-bit primary (async, no local waiter), wait until the passive has replied (the reply is
	// now delayed 500ms in the proxy), then Close so the late reply arrives into a torn-down generation.
	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, true, secs2.A("close-race"))
	<-replied

	require.NoError(t, active.conn.Close(), "Close must be bounded and clean while a reply is in flight")
	assertCleanShutdown(t, active.conn)
}
