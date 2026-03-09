package hsmsss

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testIP        = "127.0.0.1"
	testPort      = 35000
	testSessionID = 9527
)

func TestMain(m *testing.M) {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	var level logger.LogLevel
	switch logLevel {
	case "debug":
		level = logger.DebugLevel
	case "warn":
		level = logger.WarnLevel
	case "error":
		level = logger.ErrorLevel
	case "fatal":
		level = logger.FatalLevel
	default:
		level = logger.InfoLevel
	}

	logger.SetLevel(level)

	m.Run()
}

func TestConnection_ActiveHost_PassiveEQP(t *testing.T) {
	ctx := t.Context()

	testConnection(ctx, t, true)
}

func TestConnection_PassiveHost_ActiveEQP(t *testing.T) {
	ctx := t.Context()

	testConnection(ctx, t, false)
}

func TestConnection_ActiveHost_AbnormalClose(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()

	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
		WithT8Timeout(1000*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(1*time.Second),
		WithT8Timeout(1000*time.Millisecond),
	)

	defer func() {
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	// open host & equipment
	require.NoError(eqpComm.open(false))
	require.NoError(hostComm.open(false))

	// wait eqp state to be selected
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// wait host state to be selected
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// make host close abnormally
	require.NoError(hostComm.abnormalClose())

	// wait host state to be not-connected
	t.Log("=== wait host state to be not-connected ===")
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState))
	t.Log("=== wait host state to be not-connected success ===")

	t.Log("=== wait eqp state to be connecting ===")
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.ConnectingState))
	t.Log("=== wait eqp state to be connecting success ===")

	// reopen host
	require.NoError(hostComm.open(false))

	// wait host state to be selected
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
}

func TestConnection_ActiveHost_CloseMultipleTimes(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()

	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(1000*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(5*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithT3Timeout(1000*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
	)

	// stop before open
	require.NoError(hostComm.close())
	require.NoError(eqpComm.close())

	// open host & equipment
	require.NoError(eqpComm.open(false))
	require.NoError(hostComm.open(false))

	// wait host state to be selected
	err := hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState)
	require.NoError(err)

	for range 5 {
		require.NoError(hostComm.close())
	}

	for range 5 {
		require.NoError(eqpComm.close())
	}

	// reopen host only
	require.NoError(hostComm.open(false))

	for range 5 {
		require.NoError(hostComm.close())
	}
}

// TestConnection_ActiveHost_SingleConnectLoop verifies that concurrent calls to
// startConnectLoop never produce multiple overlapping background goroutines.
// This is a regression test for the race condition where multiple connect loops
// could steal TCP connections from each other, leading to EOFs and stuck states.
func TestConnection_ActiveHost_SingleConnectLoop(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithT5Timeout(50*time.Millisecond),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
	)
	defer func() {
		require.NoError(hostComm.close())
	}()

	conn := hostComm.conn

	// Open without a passive side so the connect loop keeps retrying.
	require.NoError(hostComm.open(false))

	// Give the connect loop time to start.
	time.Sleep(20 * time.Millisecond)

	// Hammer startConnectLoop from many goroutines; only one should win.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn.startConnectLoop(conn.loadLoopCtx())
		}()
	}
	wg.Wait()

	// The atomic guard must still read true (exactly one loop running).
	require.True(conn.connectLoopRunning.Load(),
		"connectLoopRunning should be true while the loop is active")
}

// TestConnection_ActiveHost_CloseOpenNoOverlap verifies that rapid Close→Open
// cycles never produce overlapping connect loops. After each cycle the connection
// must reach SELECTED, proving no stale goroutine is stealing the TCP socket.
func TestConnection_ActiveHost_CloseOpenNoOverlap(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithT3Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
	)

	defer func() {
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	// First establish a connection so both sides are ready.
	require.NoError(hostComm.open(false))
	require.NoError(eqpComm.open(false))
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Now hammer rapid Close→Open on the active side while passive stays up.
	// Each cycle must reach SELECTED again, proving no overlapping loops.
	for i := range 5 {
		t.Logf("--- rapid cycle %d ---", i)

		require.NoError(hostComm.close())
		require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState))

		// Immediately reopen — createContexts must block until old loop is dead.
		require.NoError(hostComm.open(false))
		require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

		t.Logf("--- rapid cycle %d: SELECTED ---", i)
	}
}

func TestConnection_ActiveHost_RetryConnect(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()

	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(1000*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(3*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithT3Timeout(1000*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
		WithT5Timeout(10*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(3*time.Second),
	)

	defer func() {
		// close host
		t.Log("=== defer host close ===")
		require.NoError(hostComm.close())

		// close equipment
		t.Log("=== defer eqp close ===")
		require.NoError(eqpComm.close())
	}()

	for range 10 {
		randBool := rand.Intn(2) == 1
		begin := time.Now()
		if randBool {
			// open host first
			require.NoError(hostComm.open(false))
			// open equipment later
			require.NoError(eqpComm.open(false))
		} else {
			// open equipment first
			require.NoError(eqpComm.open(false))
			// open host later
			require.NoError(hostComm.open(false))
		}

		t.Logf("### wait host state to be selected ###")
		err := hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState)
		require.NoError(err)
		t.Logf("### wait host state to be selected success, elapsed: %v ###", time.Since(begin))

		t.Logf("### wait eqp state to be selected ###")
		err = eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState)
		require.NoError(err)
		t.Logf("### wait eqp state to be selected success, elapsed: %v ###", time.Since(begin))

		// send from host
		hostComm.testMsgSuccess(1, 3, secs2.A("from host"), `<A[9] "from host">`)
		hostComm.testAsyncMsgSuccess(1, 3, secs2.A("from host async"), `<A[15] "from host async">`)

		// send from eqp
		eqpComm.testMsgSuccess(2, 5, secs2.A("from eqp"), `<A[8] "from eqp">`)
		eqpComm.testAsyncMsgSuccess(2, 5, secs2.A("from eqp async"), `<A[14] "from eqp async">`)

		randBool = rand.Intn(2) == 1
		begin = time.Now()
		if randBool {
			// close equipment first
			require.NoError(eqpComm.randomClose())
			// close host later
			require.NoError(hostComm.randomClose())
		} else {
			// close host first
			require.NoError(hostComm.close())
			// close equipment later
			require.NoError(eqpComm.close())
		}

		t.Logf("### wait host state to be not-connected ###")
		err = hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState)
		require.NoError(err)
		t.Logf("### wait host state to be not-connected success, elapsed: %v ###", time.Since(begin))

		t.Logf("### wait eqp state to be not-connected ###")
		err = eqpComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState)
		require.NoError(err)
		t.Logf("### wait eqp state to be not-connected success, elapsed: %v ###", time.Since(begin))
	}
}

func verifyLinktestCounts(t *testing.T, hostComm, eqpComm *testComm, expected uint64, delta float64, message string) {
	t.Helper()
	require := require.New(t)

	t.Log(message)
	hostMetrics := hostComm.conn.GetMetrics()
	eqpMetrics := eqpComm.conn.GetMetrics()

	require.InDelta(hostMetrics.LinktestSendCount.Load(), expected, delta,
		"host send count mismatch: %s", message)
	require.InDelta(hostMetrics.LinktestRecvCount.Load(), expected, delta,
		"host recv count mismatch: %s", message)
	require.InDelta(eqpMetrics.LinktestSendCount.Load(), expected, delta,
		"eqp send count mismatch: %s", message)
	require.InDelta(eqpMetrics.LinktestRecvCount.Load(), expected, delta,
		"eqp recv count mismatch: %s", message)
}

// waitLinktestCountAtLeast polls until all four linktest counters (host send/recv,
// eqp send/recv) reach at least minCount, with a generous timeout. This replaces
// fragile time.Sleep + InDelta assertions that are sensitive to scheduler jitter.
func waitLinktestCountAtLeast(t *testing.T, hostComm, eqpComm *testComm, minCount uint64, message string) {
	t.Helper()
	require := require.New(t)

	require.Eventually(func() bool {
		hm := hostComm.conn.GetMetrics()
		em := eqpComm.conn.GetMetrics()

		return hm.LinktestSendCount.Load() >= minCount &&
			hm.LinktestRecvCount.Load() >= minCount &&
			em.LinktestSendCount.Load() >= minCount &&
			em.LinktestRecvCount.Load() >= minCount
	}, 5*time.Second, 20*time.Millisecond, "%s: expected all counters >= %d", message, minCount)
}

// waitLinktestCountStable verifies the linktest counters remain within [prevCount, prevCount+maxGrowth]
// over a settling period.  Used to confirm linktests are suppressed or disabled.
func waitLinktestCountStable(t *testing.T, hostComm, eqpComm *testComm, maxGrowth uint64, settleTime time.Duration, message string) {
	t.Helper()

	hm := hostComm.conn.GetMetrics()
	snapshot := hm.LinktestSendCount.Load()

	time.Sleep(settleTime)

	hm = hostComm.conn.GetMetrics()
	em := eqpComm.conn.GetMetrics()
	hostSend := hm.LinktestSendCount.Load()
	eqpSend := em.LinktestSendCount.Load()

	if hostSend > snapshot+maxGrowth || eqpSend > snapshot+maxGrowth {
		t.Fatalf("%s: counters grew too much (host send: %d→%d, eqp send: %d, maxGrowth: %d)",
			message, snapshot, hostSend, eqpSend, maxGrowth)
	}
}

func updateBothConfigs(t *testing.T, hostComm, eqpComm *testComm, opts ...ConnOption) {
	t.Helper()
	require := require.New(t)

	require.NoError(hostComm.conn.UpdateConfigOptions(opts...))
	require.NoError(eqpComm.conn.UpdateConfigOptions(opts...))
}

func TestConnection_Linktest(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Setup connections with initial linktest configuration (disabled)
	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithAutoLinktest(false),
		WithLinktestInterval(100*time.Millisecond),
	)

	// Open connections
	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer func() {
		t.Log("Close connection")
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	// Wait for both connections to reach the Selected state
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	time.Sleep(300 * time.Millisecond)

	// Initial verification: No linktests should have occurred
	verifyLinktestCounts(t, hostComm, eqpComm, 0, 0,
		"Initial state with linktest disabled, expecting 0 linktests")

	// Enable linktest on both connections with 100ms interval
	t.Log("Enable linktest with 100ms interval")
	updateBothConfigs(t, hostComm, eqpComm, WithAutoLinktest(true), WithLinktestInterval(100*time.Millisecond))

	// Test 1: Wait until at least 3 linktests have been exchanged (poll instead of fixed sleep)
	waitLinktestCountAtLeast(t, hostComm, eqpComm, 3,
		"After enabling linktest with 100ms interval, expecting >= 3 linktests")

	// Test 2: Disable linktest — counters should stabilize
	t.Log("Disable linktest")
	snapshotBeforeDisable := hostComm.conn.GetMetrics().LinktestSendCount.Load()
	updateBothConfigs(t, hostComm, eqpComm, WithAutoLinktest(false))
	waitLinktestCountStable(t, hostComm, eqpComm, 2, 300*time.Millisecond,
		"After disabling linktest, count should stabilize")

	// Test 3: Re-enable linktest — counters should grow again
	t.Log("Resume linktest")
	updateBothConfigs(t, hostComm, eqpComm, WithAutoLinktest(true))
	waitLinktestCountAtLeast(t, hostComm, eqpComm, snapshotBeforeDisable+3,
		"After re-enabling linktest, expecting at least 3 more linktests")

	// Test 4: Change interval to 50ms — should accumulate faster
	t.Log("Change linktest interval to 50ms")
	snapshotBeforeChange := hostComm.conn.GetMetrics().LinktestSendCount.Load()
	updateBothConfigs(t, hostComm, eqpComm, WithLinktestInterval(50*time.Millisecond))
	waitLinktestCountAtLeast(t, hostComm, eqpComm, snapshotBeforeChange+5,
		"After changing interval to 50ms, expecting at least 5 more linktests")

	// Test 5: Linktest suppression when sending messages from host
	t.Log("Send message from host per 25ms")
	for range 10 {
		time.Sleep(25 * time.Millisecond)
		hostComm.testMsgSuccess(1, 1, secs2.A("from host"), `<A[9] "from host">`)
	}
	waitLinktestCountStable(t, hostComm, eqpComm, 3, 100*time.Millisecond,
		"Linktest should be suppressed when host sends messages")

	// Test 6: Linktest suppression when sending messages from equipment
	t.Log("Send message from eqp per 25ms")
	for range 10 {
		time.Sleep(25 * time.Millisecond)
		eqpComm.testMsgSuccess(1, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
	}
	waitLinktestCountStable(t, hostComm, eqpComm, 3, 100*time.Millisecond,
		"Linktest should be suppressed when equipment sends messages")

	// Test 7: Resume linktest after idle period
	snapshotBeforeIdle := hostComm.conn.GetMetrics().LinktestSendCount.Load()
	waitLinktestCountAtLeast(t, hostComm, eqpComm, snapshotBeforeIdle+3,
		"After idle period, expecting at least 3 more linktests")
}

// selectOnlyPeer starts a raw TCP server that speaks just enough HSMS to complete
// the select.req/rsp handshake, then silently discards all subsequent messages
// (including linktest.req). This causes the host's periodic linktest requests to
// T6 timeout while the TCP connection stays alive.
//
// Returns a cleanup function that closes the listener and connection.
func selectOnlyPeer(t *testing.T, port int) func() {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(testIP, strconv.Itoa(port)))
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				_ = conn.Close()
				return
			default:
			}

			_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := conn.Read(buf)
			if err != nil {
				continue
			}
			if n < 14 {
				continue
			}

			// Parse HSMS: 4-byte length + 10-byte header.
			// SType is at offset 9 (buf[9]).
			sType := buf[9]
			if sType == hsms.SelectReqType {
				// Build select.rsp: copy the header and change SType to SelectRspType.
				rsp := make([]byte, 14)
				copy(rsp, buf[:14])
				rsp[9] = hsms.SelectRspType // SType = select.rsp
				rsp[6] = 0                  // SelectStatus = success

				_, _ = conn.Write(rsp)
			}
			// All other messages (linktest.req, etc.) are silently discarded.
		}
	}()

	return func() {
		close(done)
		_ = listener.Close()
	}
}

// TestConnection_LinktestFailThreshold_Default verifies that with the default threshold (1),
// a single linktest T6 timeout triggers an immediate transition to NotConnected.
func TestConnection_LinktestFailThreshold_Default(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Start a select-only peer: completes select handshake but ignores linktest.
	cleanup := selectOnlyPeer(t, port)
	defer cleanup()

	// Host: active, auto-linktest enabled, short T6 + linktest interval.
	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1*time.Second),
		WithT7Timeout(240*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
		// default threshold is 1
	)

	require.NoError(hostComm.open(false))

	// Wait for the host to reach Selected (select.rsp comes from the peer)
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	t.Log("Host reached Selected state")

	// Verify threshold is 1
	require.Equal(1, hostComm.conn.cfg.LinktestFailThreshold())

	// With threshold=1, the first linktest T6 timeout should disconnect.
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState))
	t.Log("Host transitioned to NotConnected after 1 linktest T6 timeout (threshold=1)")

	// Verify metrics: at least 1 linktest error
	hostMetrics := hostComm.conn.GetMetrics()
	require.GreaterOrEqual(hostMetrics.LinktestErrCount.Load(), uint64(1),
		"expected at least 1 linktest error")

	require.NoError(hostComm.close())
}

// TestConnection_LinktestFailThreshold_Multiple verifies that with threshold=3,
// the connection tolerates 2 consecutive T6 timeouts and only disconnects on the 3rd.
func TestConnection_LinktestFailThreshold_Multiple(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Start a select-only peer: completes select handshake but ignores linktest.
	cleanup := selectOnlyPeer(t, port)
	defer cleanup()

	// Host: active, threshold=3, short T6 + linktest interval.
	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1*time.Second),
		WithT7Timeout(240*time.Second),
		WithLinktestFailThreshold(3),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
	)

	require.NoError(hostComm.open(false))

	// Wait for the host to reach Selected
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	t.Log("Host reached Selected state")

	// Verify threshold is 3
	require.Equal(3, hostComm.conn.cfg.LinktestFailThreshold())

	// With threshold=3 the connection should tolerate 2 consecutive T6 timeouts
	// and only disconnect on the 3rd.  Instead of snapshotting an intermediate
	// state (which is scheduler-sensitive), we simply wait for the final outcome:
	// the host must eventually reach NotConnectedState.
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState))
	t.Log("Host transitioned to NotConnected after reaching threshold=3")

	// Verify metrics show at least 3 linktest errors
	hostMetrics := hostComm.conn.GetMetrics()
	require.GreaterOrEqual(hostMetrics.LinktestErrCount.Load(), uint64(3),
		"expected at least 3 linktest errors")

	require.NoError(hostComm.close())
}

// TestConnection_LinktestFailThreshold_ResetOnSuccess verifies that the consecutive failure
// counter resets to zero when a successful linktest response is received.
func TestConnection_LinktestFailThreshold_ResetOnSuccess(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// threshold=3, with a real equipment peer that responds to linktests
	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1*time.Second),
		WithLinktestFailThreshold(3),
		WithConnectRemoteTimeout(500*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithAutoLinktest(false),
		WithCloseConnTimeout(1*time.Second),
	)

	require.NoError(eqpComm.open(false))
	require.NoError(hostComm.open(false))
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Wait until a few successful linktests have been exchanged, then verify
	// the consecutive-failure counter stays at zero.
	require.Eventually(func() bool {
		return hostComm.conn.GetMetrics().LinktestSendCount.Load() >= 3
	}, 5*time.Second, 20*time.Millisecond, "expected at least 3 successful linktests")

	// Verify counter is zero after successful linktests
	require.Equal(int32(0), hostComm.conn.linktestFailCount.Load(),
		"fail counter should be zero after successful linktests")

	// Also verify we're still connected
	require.True(hostComm.conn.stateMgr.State().IsSelected(),
		"expected host to be in Selected state")

	require.NoError(hostComm.close())
	require.NoError(eqpComm.close())
}

// TestConnection_LinktestFailThreshold_HotReload verifies that changing the threshold
// at runtime via UpdateConfigOptions takes effect on the next linktest cycle.
func TestConnection_LinktestFailThreshold_HotReload(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Start a select-only peer: completes select handshake but ignores linktest.
	cleanup := selectOnlyPeer(t, port)
	defer cleanup()

	// Start with threshold=3 so the host survives the first linktest failures
	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1*time.Second),
		WithT7Timeout(240*time.Second),
		WithLinktestFailThreshold(3),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(3*time.Second),
	)

	require.NoError(hostComm.open(false))
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	t.Log("Host reached Selected state")

	// Hot-reload: lower threshold to 2
	require.NoError(hostComm.conn.UpdateConfigOptions(WithLinktestFailThreshold(2)))
	require.Equal(2, hostComm.conn.cfg.LinktestFailThreshold())
	t.Log("Hot-reloaded threshold from 3 to 2")

	// Should disconnect after 2 T6 timeouts (not 3).
	// 1st timeout at ~1.1s → failCount=1, tolerated
	// 2nd timeout at ~2.2s → failCount=2 >= threshold(2) → disconnect
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.NotConnectedState))
	t.Log("Host transitioned to NotConnected with hot-reloaded threshold=2")

	// Verify metrics show at least 2 linktest errors
	hostMetrics := hostComm.conn.GetMetrics()
	require.GreaterOrEqual(hostMetrics.LinktestErrCount.Load(), uint64(2),
		"expected at least 2 linktest errors")

	require.NoError(hostComm.close())
}

func TestSendMessageFail(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	port := getPort()
	hostComm := newTestComm(ctx, t, port, true, true)
	eqpComm := newSlowTestComm(ctx, t, port, false, false)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer hostComm.close()
	defer eqpComm.close()

	// close eqp before sending message
	require.NoError(eqpComm.close())

	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("test"))
	require.Error(err, "expected error when sending message to closed equipment, got: %v", err)
	require.Nil(reply, "expected reply to be nil, got: %v", reply)

	// open the slow eqp
	require.NoError(eqpComm.open(true))

	// wait eqp & host state to be selected
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	reply, err = hostComm.session.SendDataMessage(1, 1, true, secs2.A("test"))
	require.NoError(err, "expected no error when sending message to slow eqp, got: %v", err)
	require.NotNil(reply, "expected reply to be not nil, got: %v", reply)
	require.Equal(secs2.A("test").ToBytes(), reply.Item().ToBytes(), "expected reply item to be 'test', got: %v", reply.Item())
}

func TestDrainMessageOnConnClose(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	port := getPort()
	hostComm := newTestComm(ctx, t, port, true, true, WithSenderQueueSize(100), WithCloseConnTimeout(1*time.Second))
	eqpComm := newTestComm(ctx, t, port, false, false)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer eqpComm.close()
	defer hostComm.close()

	require.NoError(eqpComm.close())

	// Use a ready channel so goroutines signal they have started before we close.
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{} // signal ready before blocking on send
			err := hostComm.session.SendDataMessageAsync(1, 1, true, secs2.A("test"))
			if err != nil &&
				!errors.Is(err, hsms.ErrConnClosed) &&
				!errors.Is(err, hsms.ErrNotSelectedState) &&
				!errors.Is(err, hsms.ErrSendMsgTimeout) {
				require.NoError(err, "unexpected error when sending message after eqp closed")
			}
		}()
	}
	// Wait for all goroutines to have started.
	for range 100 {
		<-ready
	}

	begin := time.Now()
	require.NoError(hostComm.close())
	elapsed := time.Since(begin)
	require.LessOrEqual(elapsed, 2*time.Second, "expected host close to complete within 2 seconds, took: %v", elapsed)

	wg.Wait()
}

func TestConn_validateMsg(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	connCfg, err := NewConnectionConfig("localhost", 6666,
		WithHostRole(),
		WithValidateDataMessage(true),
	)
	require.NotNil(connCfg)
	require.NoError(err)

	conn, err := NewConnection(ctx, connCfg)
	require.NoError(err)
	require.NotNil(conn)

	testSessionID := uint16(0)

	_ = conn.AddSession(testSessionID)

	testCases := []struct {
		sessionID   uint16
		expectError bool
	}{
		{sessionID: testSessionID, expectError: false},
		{sessionID: testSessionID | 0x8000, expectError: true},
		{sessionID: 1234, expectError: true},
	}

	for _, tt := range testCases {
		msg, err := hsms.NewDataMessage(99, 1, true, tt.sessionID, hsms.ToSystemBytes(12345), secs2.NewEmptyItem())
		require.NoError(err)
		require.NotNil(msg)

		err = conn.validateMsg(msg)
		if tt.expectError {
			require.Error(err, "expected error for session ID %d", tt.sessionID)
		} else {
			require.NoError(err, "unexpected error for session ID %d", tt.sessionID)
		}
	}
}

func testConnection(ctx context.Context, t testing.TB, hostIsActive bool) {
	require := require.New(t)

	port := getPort()
	hostComm := newTestComm(ctx, t, port, true, hostIsActive)
	eqpComm := newTestComm(ctx, t, port, false, !hostIsActive)

	if hostIsActive {
		require.NoError(eqpComm.open(true))
		require.NoError(hostComm.open(true))
	} else {
		require.NoError(hostComm.open(true))
		require.NoError(eqpComm.open(true))
	}
	defer hostComm.close()
	defer eqpComm.close()

	// send from host
	hostComm.testMsgSuccess(1, 1, secs2.A("from host"), `<A[9] "from host">`)
	hostComm.testAsyncMsgSuccess(1, 1, secs2.A("from host async"), `<A[15] "from host async">`)

	// empty item
	hostComm.testMsgSuccess(1, 3, nil, "")

	// even function code
	hostComm.testMsgError(1, 2, nil, hsms.ErrInvalidReqMsg)

	// send from eqp
	eqpComm.testMsgSuccess(2, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
	eqpComm.testAsyncMsgSuccess(2, 1, secs2.A("from eqp async"), `<A[14] "from eqp async">`)

	// empty item
	eqpComm.testMsgSuccess(2, 3, nil, "")
	// even function code
	eqpComm.testMsgError(2, 4, secs2.I1(1), hsms.ErrInvalidReqMsg)

	// close equipment
	require.NoError(eqpComm.close())

	// expects to get error when equipment has closed
	hostComm.testMsgNetError(3, 15, nil, hsms.ErrConnClosed, hsms.ErrNotSelectedState)

	// close host
	require.NoError(hostComm.close())

	// reopen
	if hostIsActive {
		require.NoError(eqpComm.open(true))
	} else {
		require.NoError(hostComm.open(true))
	}

	// expects to get error when host has closed
	eqpComm.testMsgNetError(3, 17, nil, hsms.ErrNotSelectedState)

	// reopen
	if hostIsActive {
		require.NoError(hostComm.open(true))
	} else {
		require.NoError(eqpComm.open(true))
	}

	// send from host
	hostComm.testMsgSuccess(7, 1, secs2.A("from host"), `<A[9] "from host">`)
	// empty item
	hostComm.testMsgSuccess(7, 3, nil, "")
	// even function code
	hostComm.testMsgError(7, 2, nil, hsms.ErrInvalidReqMsg)

	// send from eqp
	eqpComm.testMsgSuccess(8, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
	// empty item
	eqpComm.testMsgSuccess(8, 3, nil, "")
	// even function code
	eqpComm.testMsgError(8, 2, nil, hsms.ErrInvalidReqMsg)

	// close host
	require.NoError(hostComm.close())
	// close equipment
	require.NoError(eqpComm.close())
}

var (
	addrPool      = make(map[string]struct{})
	addrPoolMutex sync.Mutex
)

func getRandomListener() (net.Listener, error) {
	for {
		// listen on TCP port 0, which tells the OS to pick a random port.
		listener, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			return nil, err
		}

		addr := listener.Addr().String()

		addrPoolMutex.Lock()
		_, existed := addrPool[addr]
		if existed {
			_ = listener.Close()
			addrPoolMutex.Unlock()
			continue
		}
		addrPool[addr] = struct{}{}
		addrPoolMutex.Unlock()

		return listener, nil
	}
}

func getPort() int {
	listener, err := getRandomListener()
	if err != nil {
		panic("failed to get random listener: " + err.Error())
	}
	defer listener.Close()

	addr := listener.Addr().String()

	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		panic("failed to split host and port: " + err.Error())
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic("failed to convert port string to int: " + err.Error())
	}

	return port
}

type testComm struct {
	ctx      context.Context
	t        testing.TB
	require  *require.Assertions
	isHost   bool
	isActive bool
	conn     *Connection
	session  hsms.Session

	recvReplyChan chan *hsms.DataMessage
}

func newTestComm(ctx context.Context, t testing.TB, port int, isHost bool, isActive bool, extraOpts ...ConnOption) *testComm {
	c := doNewTestComm(ctx, t, port, isHost, isActive, extraOpts...)
	c.session.AddDataMessageHandler(c.msgEchoHandler)
	return c
}

func newSlowTestComm(ctx context.Context, t testing.TB, port int, isHost bool, isActive bool, extraOpts ...ConnOption) *testComm {
	c := doNewTestComm(ctx, t, port, isHost, isActive, extraOpts...)
	c.session.AddDataMessageHandler(c.msgSlowEchoHandler)
	return c
}

func doNewTestComm(ctx context.Context, t testing.TB, port int, isHost bool, isActive bool, extraOpts ...ConnOption) *testComm {
	require := require.New(t)
	c := &testComm{ctx: ctx, t: t, require: require, isHost: isHost, isActive: isActive}

	c.recvReplyChan = make(chan *hsms.DataMessage, 1)

	c.conn = newConn(ctx, require, port, isHost, isActive, extraOpts...)
	require.NotNil(c.conn)

	require.True(c.conn.IsSingleSession())
	require.False(c.conn.IsGeneralSession())
	require.NotNil(c.conn.GetLogger())

	c.session = c.conn.AddSession(testSessionID)
	require.NotNil(c.session)

	return c
}

func (c *testComm) open(waitOpened bool) error {
	if c.isHost {
		c.t.Logf("=== host open [active: %t] ===", c.isActive)
	} else {
		c.t.Logf("=== eqp open [active: %t] ===", c.isActive)
	}

	err := c.conn.Open(waitOpened)

	if c.isHost {
		c.t.Logf("=== host open finished [active: %t] ===", c.isActive)
	} else {
		c.t.Logf("=== eqp open finished [active: %t] ===", c.isActive)
	}

	return err
}

func (c *testComm) close() error {
	if c.isHost {
		c.t.Logf("=== host close [active: %t] ===", c.isActive)
	} else {
		c.t.Logf("=== eqp close [active: %t] ===", c.isActive)
	}

	err := c.conn.Close()

	if c.isHost {
		c.t.Logf("=== host close finished [active: %t] ===", c.isActive)
	} else {
		c.t.Logf("=== eqp close finished [active: %t] ===", c.isActive)
	}

	return err
}

// abnormalClose closes the connection without sending separate control message.
func (c *testComm) abnormalClose() error {
	var target string
	if c.isHost {
		target = "host"
	} else {
		target = "eqp"
	}
	c.t.Logf("=== %s abnormal close [active: %t] ===", target, c.isActive)
	c.conn.shutdown.Store(true)
	c.conn.stateMgr.Stop()
	if err := c.conn.closeConn(time.Second); err != nil {
		c.t.Logf("=== %s abnormal close failed [active: %t] ===", target, c.isActive)
		return err
	}
	c.t.Logf("=== %s abnormal close finished [active: %t] ===", target, c.isActive)

	return nil
}

// randomClose normal or abnormal close the connection randomly.
func (c *testComm) randomClose() error {
	if rand.Intn(2) == 1 {
		return c.close()
	}

	return c.abnormalClose()
}

func (c *testComm) msgEchoHandler(msg *hsms.DataMessage, session hsms.Session) {
	// receives reply message
	if msg.FunctionCode()%2 == 0 {
		c.recvReplyChan <- msg
		return
	}

	_ = session.ReplyDataMessage(msg, msg.Item())
}

func (c *testComm) msgSlowEchoHandler(msg *hsms.DataMessage, session hsms.Session) {
	time.Sleep(500 * time.Millisecond) // simulate slow processing
	// receives reply message
	if msg.FunctionCode()%2 == 0 {
		c.recvReplyChan <- msg
		return
	}

	err := session.ReplyDataMessage(msg, msg.Item())
	c.require.NoError(err)
}

func (c *testComm) testMsgSuccess(stream byte, function byte, dataItem secs2.Item, expectedSML string) {
	c.t.Helper()

	var reply *hsms.DataMessage
	var err error
	for range 3 {
		reply, err = c.session.SendDataMessage(stream, function, true, dataItem)
		if err == nil {
			break
		}

		if errors.Is(err, hsms.ErrConnClosed) || errors.Is(err, hsms.ErrNotSelectedState) {
			c.t.Logf("retrying SendDataMessage for S%dF%d due to error: %v", stream, function, err)
			time.Sleep(100 * time.Millisecond) // wait a bit before retrying
			continue
		}
	}

	c.require.NoError(err, "the current state is %s", c.conn.stateMgr.State().String())
	c.require.NotNil(reply)
	c.require.Equal(stream, reply.StreamCode())
	c.require.Equal(function+1, reply.FunctionCode())
	c.require.Equal(expectedSML, reply.Item().ToSML())
}

func (c *testComm) testAsyncMsgSuccess(stream byte, function byte, dataItem secs2.Item, expectedSML string) {
	c.t.Helper()

	// Drain any stale replies that may have leaked from a previous sync
	// SendDataMessage whose T3/T6 timer fired before the reply arrived,
	// causing the late reply to be dispatched to the handler instead of
	// the sync waiter.
	c.drainRecvReplyChan()

	err := c.session.SendDataMessageAsync(stream, function, true, dataItem)
	c.require.NoError(err)

	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-timeout:
			c.t.Fatalf("timeout waiting for reply for S%dF%d", stream, function)
		case reply := <-c.recvReplyChan:
			if reply.StreamCode() == stream && reply.FunctionCode() == function+1 &&
				reply.Item().ToSML() == expectedSML {
				return // matched the expected reply
			}
			// Discard unexpected/stale reply and keep waiting.
			c.t.Logf("discarding stale reply S%dF%d item=%s (expected S%dF%d item=%s)",
				reply.StreamCode(), reply.FunctionCode(), reply.Item().ToSML(),
				stream, function+1, expectedSML)
		}
	}
}

// drainRecvReplyChan discards any buffered messages in recvReplyChan.
func (c *testComm) drainRecvReplyChan() {
	for {
		select {
		case stale := <-c.recvReplyChan:
			c.t.Logf("drained stale reply S%dF%d item=%s from recvReplyChan",
				stale.StreamCode(), stale.FunctionCode(), stale.Item().ToSML())
		default:
			return
		}
	}
}

func (c *testComm) testMsgError(stream byte, function byte, dataItem secs2.Item, expectedErrs ...error) {
	c.t.Helper()

	reply, err := c.session.SendDataMessage(stream, function, true, dataItem)

	isExpectedErr := false
	for _, targetErr := range expectedErrs {
		if errors.Is(err, targetErr) {
			isExpectedErr = true
			break
		}
	}
	c.require.Truef(isExpectedErr, "actual error:%v is not in the expected error list: %v", err, expectedErrs)

	c.require.Nil(reply)
}

func (c *testComm) testMsgNetError(stream byte, function byte, dataItem secs2.Item, expectedErrs ...error) {
	c.t.Helper()

	reply, err := c.session.SendDataMessage(stream, function, true, dataItem)

	if isNetOpError(err) {
		return
	}

	isExpectedErr := false
	for _, targetErr := range expectedErrs {
		if errors.Is(err, targetErr) {
			isExpectedErr = true
			break
		}
	}
	c.require.Truef(isExpectedErr, "actual error:%v is not in the expected error list: %v", err, expectedErrs)

	c.require.Nil(reply)
}

func newConn(ctx context.Context, require *require.Assertions, port int, isHost bool, isActive bool, extraOpts ...ConnOption) *Connection {
	opts := []ConnOption{
		WithT3Timeout(3 * time.Second),
		WithT5Timeout(500 * time.Millisecond),
		WithT6Timeout(3 * time.Second),
		WithT7Timeout(15 * time.Second),
		WithT8Timeout(1 * time.Second),
		WithConnectRemoteTimeout(2 * time.Second),
		WithAutoLinktest(true),
		WithLinktestInterval(500 * time.Millisecond),
	}

	if isHost {
		opts = append(opts, WithHostRole())
	} else {
		opts = append(opts, WithEquipRole())
	}

	opts = append(opts, extraOpts...)

	if isActive {
		l := logger.GetLogger().With("role", "ACTIVE")
		opts = append(opts, WithActive(), WithLogger(l))
	} else {
		l := logger.GetLogger().With("role", "PASSIVE")
		opts = append(opts, WithPassive(), WithLogger(l))
	}

	connCfg, err := NewConnectionConfig(testIP, port, opts...)
	require.NotNil(connCfg)
	require.NoError(err)

	conn, err := NewConnection(ctx, connCfg)
	require.NotNil(conn)
	require.NoError(err)

	return conn
}

// TestConnection_PassiveT7TimeoutRespectContext verifies that the T7 timeout goroutine
// in passive connection properly respects context cancellation when the connection is closed
// before the T7 timeout expires. This prevents goroutine leaks.
func TestConnection_PassiveT7TimeoutRespectContext(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Create passive connection with a long T7 timeout
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithT7Timeout(30*time.Second), // Long T7 timeout that won't expire during test
		WithCloseConnTimeout(1*time.Second),
	)

	// Open the passive connection
	require.NoError(eqpComm.open(false))

	// Give time for the connection to start listening
	time.Sleep(50 * time.Millisecond)

	// Close the connection before T7 timeout expires
	// This should not leave any goroutines running
	startClose := time.Now()
	require.NoError(eqpComm.close())
	closeTime := time.Since(startClose)

	// The close should complete quickly, not waiting for T7 timeout
	require.Less(closeTime, 5*time.Second,
		"Close took too long, T7 timeout goroutine may not have respected context cancellation")
}

// TestConnection_CloseEfficiency verifies that the Close() method uses an efficient
// ticker-based approach instead of a busy loop.
func TestConnection_CloseEfficiency(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(1*time.Second),
	)

	// Open both connections
	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	// Wait for selected state
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Close both connections and measure time
	startClose := time.Now()
	require.NoError(hostComm.close())
	require.NoError(eqpComm.close())
	closeTime := time.Since(startClose)

	// Close should complete in a reasonable time
	require.Less(closeTime, 3*time.Second, "Close took too long")
}

// TestConnection_NilSessionHandling verifies that the connection properly handles
// scenarios where session might be nil during message processing.
func TestConnection_NilSessionHandling(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	// Create a connection without adding a session
	connCfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	require.NotNil(connCfg)

	conn, err := NewConnection(ctx, connCfg)
	require.NoError(err)
	require.NotNil(conn)

	// Verify that session is nil
	require.Nil(conn.session)

	// Try to open without session - should return error
	err = conn.Open(false)
	require.Error(err)
	require.ErrorIs(err, hsms.ErrSessionNil)
}

// TestConnection_Constants verifies that the expected constants are defined.
func TestConnection_Constants(t *testing.T) {
	require := require.New(t)

	// Verify constants have expected values
	require.Equal(64*1024, defaultWriteBufferSize)
	require.Equal(time.Second, replyChannelTimeout)
	require.Equal(5*time.Millisecond, closeCheckInterval)
}

// TestConnection_ReplyChannelBuffered verifies that reply channels are buffered
// so that replyToSender never blocks when the consumer (sendMsg) has not yet
// read the channel. This prevents reply drops under high contention.
func TestConnection_ReplyChannelBuffered(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	connCfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)

	conn, err := NewConnection(ctx, connCfg)
	require.NoError(err)

	id := uint32(42)
	ch := conn.addReplyExpectedMsg(id)

	// Verify the channel is buffered(1): a send should not block even
	// though nobody is reading from ch yet.
	rawCh, loaded := conn.replyMsgChans.Load(id)
	require.True(loaded)
	require.Equal(1, cap(rawCh), "reply channel should be buffered with capacity 1")

	// Write into the channel without a consumer — must not block.
	msg, _ := hsms.NewDataMessage(1, 2, false, 0, hsms.ToSystemBytes(42), nil)
	done := make(chan struct{})
	go func() {
		rawCh <- msg
		close(done)
	}()

	select {
	case <-done:
		// good — the send didn't block
	case <-time.After(time.Second):
		t.Fatal("reply channel send blocked — channel is not buffered")
	}

	// Consumer should receive the message.
	received := <-ch
	require.NotNil(received)
	require.Equal(msg.ID(), received.ID())

	conn.removeReplyExpectedMsg(id)
}

// TestConnection_CloseTCPIdempotent verifies that closeTCP is idempotent:
// calling it multiple times (including concurrently) does not panic or
// double-close the underlying net.Conn.
func TestConnection_CloseTCPIdempotent(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(1*time.Second),
	)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	// Wait for both sides to be selected.
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Verify the conn resource is non-nil before close.
	require.NotNil(hostComm.conn.getConn(), "conn should be non-nil before closeTCP")

	// First closeTCP should succeed and return a non-empty remote address.
	remote := hostComm.conn.closeTCP(time.Second)
	require.NotEmpty(remote, "first closeTCP should return remote address")

	// Conn resource should now be nil.
	require.Nil(hostComm.conn.getConn(), "conn should be nil after closeTCP")

	// Second closeTCP should be a no-op and return empty string.
	remote2 := hostComm.conn.closeTCP(time.Second)
	require.Empty(remote2, "second closeTCP should return empty string")

	// Concurrent calls should not panic.
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = hostComm.conn.closeTCP(time.Second)
		}()
	}
	wg.Wait()

	// Clean up.
	hostComm.conn.shutdown.Store(true)
	_ = hostComm.conn.closeConn(time.Second)
	require.NoError(eqpComm.close())
}

// TestConnection_ValidateMsgConsolidation verifies that message validation
// is properly consolidated and works correctly for both active and passive connections.
func TestConnection_ValidateMsgConsolidation(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Test with passive connection (which had the duplicate validation)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithValidateDataMessage(true),
		WithT7Timeout(10*time.Second),
	)
	hostComm := newTestComm(ctx, t, port, true, true,
		WithValidateDataMessage(true),
	)

	defer func() {
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	// Open both connections
	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	// Wait for selected state
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Test normal message exchange (should work)
	hostComm.testMsgSuccess(1, 1, secs2.A("test"), `<A[4] "test">`)
	eqpComm.testMsgSuccess(2, 1, secs2.A("test2"), `<A[5] "test2">`)
}

// TestConnection_ActiveBackoffDoesNotBlockClose verifies active-mode retry backoff
// does not block the connection state manager handler, which would delay Close().
//
// This is a regression test for implementations that call time.Sleep inside
// ConnStateMgr handlers.
func TestConnection_ActiveBackoffDoesNotBlockClose(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectRemoteTimeout(100*time.Millisecond),
		WithCloseConnTimeout(1*time.Second),
		WithT5Timeout(5*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(1*time.Second),
		WithT5Timeout(5*time.Second),
	)

	// Open both connections
	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	// Wait for selected state
	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Drop the equipment side without setting host shutdown.
	require.NoError(eqpComm.abnormalClose())

	// Give the host a moment to observe disconnect and transition.
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	require.NoError(hostComm.close())
	elapsed := time.Since(start)

	// If backoff blocks the state handler, Close() would tend to stall ~retryDelay.
	require.Less(elapsed, 2*time.Second, "Close took too long; backoff may be blocking state handler")
}

// TestConnection_T3StartsAfterSend verifies that the T3 reply timer does not
// start until the message has actually been written to TCP (2-phase send).
//
// Strategy: use a slow equipment handler (500ms) with T3 = 1s (minimum allowed).
// The reply arrives at ~500ms, well within the 1s T3 window.
// Additionally, verify the sendRequest.sentChan mechanism works by checking
// that the send phase completes before the reply arrives.
func TestConnection_T3StartsAfterSend(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Host: T3 = 1s (minimum allowed).
	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(1*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)
	// Equipment: slow echo handler adds 500ms processing delay.
	eqpComm := newSlowTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(2*time.Second),
	)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer func() {
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// With the 2-phase send, T3 (1s) starts only after the message is
	// written to TCP. The slow handler adds 500ms, so the reply arrives
	// at ~500ms — well within the 1s T3 window.
	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("hello"))
	require.NoError(err, "T3 should NOT fire for a reply that arrives within the timer window")
	require.NotNil(reply)
	require.Equal(byte(1), reply.StreamCode())
	require.Equal(byte(2), reply.FunctionCode())
}

// TestConnection_TwoPhase_SentChan verifies the internal 2-phase send mechanism:
// the sentChan is signaled after the message is written to TCP.
func TestConnection_TwoPhase_SentChan(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithCloseConnTimeout(2*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(2*time.Second),
	)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer func() {
		require.NoError(hostComm.close())
		require.NoError(eqpComm.close())
	}()

	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Manually construct a sendRequest with a sentChan to verify the mechanism.
	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("test"))
	require.NoError(err)

	sentChan := make(chan error, 1)
	req := &sendRequest{msg: msg, sentChan: sentChan}

	err = hostComm.conn.queueSendRequest(req)
	require.NoError(err)

	// Phase 1: sentChan should be signaled quickly (message written to TCP).
	select {
	case sendErr := <-sentChan:
		require.NoError(sendErr, "sentChan should signal nil on successful send")
	case <-time.After(3 * time.Second):
		t.Fatal("sentChan was not signaled within timeout")
	}
}

// TestConnection_SendRequestDrainOnClose verifies that pending sendRequests
// are properly drained and their sentChans signaled with ErrConnClosed
// when the connection is closed, preventing goroutine leaks.
func TestConnection_SendRequestDrainOnClose(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithSenderQueueSize(100),
		WithCloseConnTimeout(1*time.Second),
		WithConnectRemoteTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))
	defer eqpComm.close()

	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Close equipment side to cause send failures.
	require.NoError(eqpComm.close())

	// Queue multiple async messages that will pile up.
	// Use a ready channel so goroutines signal they have started before we close.
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{} // signal ready before blocking on send
			_ = hostComm.session.SendDataMessageAsync(1, 1, true, secs2.A("test"))
		}()
	}
	// Wait for all goroutines to have started.
	for range 50 {
		<-ready
	}

	// Close the host — this should drain all pending requests.
	start := time.Now()
	require.NoError(hostComm.close())
	elapsed := time.Since(start)

	require.Less(elapsed, 500*time.Millisecond, "Close should drain quickly, not block")

	wg.Wait()
}

// TestConnection_ReceiverTask_DecodeErrorNoPanic verifies that receiverTask
// does not panic on decode errors when traceTraffic is enabled.
//
// Regression: receiverTask previously passed a nil message to hsms.MsgInfo in
// the decode-error path, which could panic.
func TestConnection_ReceiverTask_DecodeErrorNoPanic(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	c := newConn(ctx, require, getPort(), true, true,
		WithTraceTraffic(true),
		WithT8Timeout(1*time.Second),
	)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	c.setupResources(client)

	// Write malformed HSMS frame:
	// - Length=5 (valid framing)
	// - Payload too short for HSMS header decode (requires >= 10 bytes)
	go func() {
		_, _ = server.Write([]byte{0x00, 0x00, 0x00, 0x05})
		_, _ = server.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	}()

	msgLenBuf := make([]byte, 4)

	require.NotPanics(func() {
		ok := c.receiverTask(msgLenBuf)
		require.False(ok, "decode failure should stop receiverTask")
	})

	require.Equal(uint64(1), c.metrics.DataMsgErrCount.Load())
}

// TestConnection_Passive_CloseWhileAccepting verifies that calling Close() while the passive
// connection is blocked in the accept loop (ConnectingState or NotSelectedState)
// safely transitions to NotConnectedState and tears everything down, proving that
// ToNotConnectedAsync() in the accept loop is redundant on shutdown.
func TestConnection_Passive_CloseWhileAccepting(t *testing.T) {
	ctx := context.Background()

	mockLogger := logger.NewMockLogger()
	mockLogger.On("Debug", mock.Anything, mock.Anything).Return()
	mockLogger.On("Info", mock.Anything, mock.Anything).Return()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Return()
	mockLogger.On("Error", mock.Anything, mock.Anything).Return()

	cfg, err := NewConnectionConfig("127.0.0.1", getPort(), WithPassive(), WithLogger(mockLogger))
	require.NoError(t, err)

	conn, err := NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(1)
	require.NotNil(t, session)

	err = conn.Open(false) // Open without waiting for SelectedState
	require.NoError(t, err)

	// Wait a short moment for the accept task to start running
	time.Sleep(100 * time.Millisecond)

	// Since Open(false) on a passive connection doesn't advance the stateMgr out of NotConnectedState
	// until a connection is actually accepted (ToNotSelectedAsync), it should still be NotConnectedState.
	require.Equal(t, hsms.NotConnectedState, conn.stateMgr.State(), "State should be NotConnectedState before accept")

	// Call Close() while it's in the accept loop
	err = conn.Close()
	require.NoError(t, err, "Close() should succeed cleanly")

	// Verify state is forced to NotConnectedState without async help
	require.Equal(t, hsms.NotConnectedState, conn.stateMgr.State(), "State should be NotConnectedState after Close")
	require.Equal(t, hsms.NotConnectedState, conn.stateMgr.DesiredState(), "DesiredState should be NotConnectedState after Close")

	// Ensure the listener is properly closed
	conn.listenerMutex.Lock()
	listener := conn.listener
	conn.listenerMutex.Unlock()
	require.Nil(t, listener, "Listener should be nil after Close")
}
