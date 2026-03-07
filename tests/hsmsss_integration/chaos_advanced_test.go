package hsmsssintegration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestChaos_BurstDisconnectReconnect
//
// Rapidly cycle Open/Close through a chaos proxy while data messages are
// in-flight. This targets:
//
//   - #3 dropAllReplyMsgs close-while-send race
//   - #5 closeConn timeout masking incomplete teardown
//   - #4 raw ctx field access (now fixed, regression test)
//
// The test must complete without panics, goroutine leaks, or data races.
// ---------------------------------------------------------------------------
func TestChaos_BurstDisconnectReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	proxy.SetFilter(func(_ bool, _ []byte, _ []byte) (ProxyAction, time.Duration) {
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(1 * time.Second),
		hsmsss.WithT5Timeout(100 * time.Millisecond),
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithT7Timeout(1 * time.Second),
		hsmsss.WithConnectRemoteTimeout(1 * time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)

	for range 5 {
		require.NoError(t, host.conn.Open(false))
		requireStateEvent(t, host.selectedCh, hsms.SelectedState)

		// Fire some async messages while connected.
		for range 3 {
			_ = host.session.SendDataMessageAsync(1, 1, false, secs2.A("burst"))
		}

		// Abrupt close through proxy — kills the TCP socket mid-flight.
		proxy.CloseConnections()

		// The host should detect the disconnect.
		requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)

		require.NoError(t, host.conn.Close())
		// Allow the proxy to accept a new connection for the next cycle.
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// TestChaos_ConcurrentSendDuringClose
//
// Multiple goroutines send messages while Close() is called concurrently.
// This targets:
//
//   - #3 dropAllReplyMsgs close-while-send race (channel close vs channel send)
//   - #1 replyToSender timeout path
//   - #2 replyErrToSender on orphaned channel
//
// Must not panic with "send on closed channel" or deadlock.
// ---------------------------------------------------------------------------
func TestChaos_ConcurrentSendDuringClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(1 * time.Second),
		hsmsss.WithT5Timeout(100 * time.Millisecond),
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithT7Timeout(1 * time.Second),
		hsmsss.WithAutoLinktest(false),
	}
	host := newEndpoint(t, ctx, equipPort, false, true, hostOpts)
	defer closeEndpoint(t, host)

	for range 5 {
		require.NoError(t, host.conn.Open(false))
		requireStateEvent(t, host.selectedCh, hsms.SelectedState)

		// Launch concurrent senders.
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 5 {
					_ = host.session.SendDataMessageAsync(1, 1, false, secs2.A("race"))
				}
			}()
		}

		// Close while senders are still firing.
		time.Sleep(5 * time.Millisecond)
		_ = host.conn.Close()

		wg.Wait()
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// TestChaos_BlackHoleProxy
//
// Proxy accepts the TCP connection but drops ALL messages (simulates a
// network black hole). This targets:
//
//   - #5 closeConn timeout with hung receiver
//   - T7 timeout detection (host should disconnect after T7)
//
// The host must gracefully time out and transition to NotConnected.
// ---------------------------------------------------------------------------
func TestChaos_BlackHoleProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	var selected atomic.Bool
	// Forward Select.req/rsp to let connection establish, then black-hole everything.
	proxy.SetFilter(func(_ bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if len(header) >= 10 {
			// Allow Select.req (1) and Select.rsp (2) through.
			stype := header[5]
			if stype == byte(hsms.SelectReqType) || stype == byte(hsms.SelectRspType) {
				if stype == byte(hsms.SelectRspType) {
					selected.Store(true)
				}
				return ProxyActionForward, 0
			}
		}
		// After selection, drop everything.
		if selected.Load() {
			return ProxyActionDrop, 0
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(1 * time.Second),
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithT7Timeout(1 * time.Second),
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(500 * time.Millisecond),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	// The black hole should cause linktest failures → T6 timeout → disconnect.
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

// ---------------------------------------------------------------------------
// TestChaos_MidWriteTCPReset
//
// Proxy resets TCP after the host sends a data message, simulating a
// network failure during a synchronous SendDataMessage with W-bit=true.
// The host must:
//   - Return an error from SendDataMessage (not hang forever)
//   - Transition cleanly to NotConnected
//   - Not panic in replyToSender/replyErrToSender
//
// ---------------------------------------------------------------------------
func TestChaos_MidWriteTCPReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	var killed atomic.Bool
	// Kill TCP on the first data message from host→equip.
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		if isClientToTarget && len(header) >= 10 && header[4] == 0 && header[5] == 0 {
			if killed.CompareAndSwap(false, true) {
				return ProxyActionCloseTCP, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(2 * time.Second),
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	// Synchronous send — should error out, not hang.
	item := secs2.NewASCIIItem("will fail")
	reply, err := host.session.SendDataMessage(1, 1, true, item)

	// Either T3 timeout or connection error — both acceptable.
	require.Error(t, err)
	require.Nil(t, reply)

	// Should reach NotConnected eventually.
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

// ---------------------------------------------------------------------------
// TestChaos_RapidLinktestToggle
//
// Toggle auto-linktest on/off rapidly while the connection is alive.
// This targets:
//
//   - #12 tickerCtl RLock misuse (now fixed, regression test)
//   - #6 stale T7 goroutine from previous lifecycle
//
// Must not race or panic.
// ---------------------------------------------------------------------------
func TestChaos_RapidLinktestToggle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(100 * time.Millisecond),
	}
	host := newEndpoint(t, ctx, equipPort, false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	// Rapidly toggle linktest + send messages concurrently.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			_ = host.conn.UpdateConfigOptions(hsmsss.WithAutoLinktest(false))
			time.Sleep(10 * time.Millisecond)
			_ = host.conn.UpdateConfigOptions(
				hsmsss.WithAutoLinktest(true),
				hsmsss.WithLinktestInterval(50*time.Millisecond),
			)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 30 {
			_ = host.session.SendDataMessageAsync(1, 1, false, secs2.A("toggle"))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Connection should still be alive after the storm.
	item := secs2.NewASCIIItem("alive")
	reply, err := host.session.SendDataMessage(1, 1, true, item)
	require.NoError(t, err)
	require.NotNil(t, reply)
}

// ---------------------------------------------------------------------------
// TestChaos_ReplyDuringClose
//
// Send a synchronous message, then close the host before the reply arrives.
// The proxy delays the reply so it arrives during/after Close().
// This targets:
//
//   - #1 replyToSender timer vs close
//   - #3 dropAllReplyMsgs racing with replyToSender
//
// Must not panic with "send on closed channel".
// ---------------------------------------------------------------------------
func TestChaos_ReplyDuringClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	// Delay reply messages from equip→host by 500ms.
	proxy.SetFilter(func(isClientToTarget bool, header []byte, _ []byte) (ProxyAction, time.Duration) {
		// Data message replies (equip→host, stype=0 for data)
		if !isClientToTarget && len(header) >= 10 && header[4] == 0 && header[5] == 0 {
			return ProxyActionForward, 500 * time.Millisecond
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(3 * time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	// Fire async send (reply expected) then immediately close.
	_ = host.session.SendDataMessageAsync(1, 1, true, secs2.A("close-race"))

	// Brief pause to let the message reach the proxy, then close.
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, host.conn.Close())

	// The delayed reply arrives after Close() — must not panic.
	time.Sleep(600 * time.Millisecond)
}
