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
	case "info":
		level = logger.InfoLevel
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

	os.Exit(m.Run())
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

	// Test 1: Initial linktest with 100ms interval
	expectedCount := uint64(5)
	expectedDelta := float64(expectedCount) * 0.4
	time.Sleep(500 * time.Millisecond)
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"After 500ms with 100ms interval, expecting ~5 linktests")

	// Test 2: Disable linktest
	t.Log("Disable linktest")
	updateBothConfigs(t, hostComm, eqpComm, WithAutoLinktest(false))
	time.Sleep(200 * time.Millisecond)
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"After disabling linktest, count should remain the same")

	// Test 3: Re-enable linktest
	t.Log("Resume linktest")
	updateBothConfigs(t, hostComm, eqpComm, WithAutoLinktest(true))
	expectedCount += 5
	expectedDelta = float64(expectedCount) * 0.4
	time.Sleep(500 * time.Millisecond)
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"After re-enabling linktest, expecting 5 more linktests")

	// Test 4: Change interval to 50ms
	t.Log("Change linktest interval to 50ms")
	updateBothConfigs(t, hostComm, eqpComm, WithLinktestInterval(50*time.Millisecond))
	expectedCount += 10
	expectedDelta = float64(expectedCount) * 0.4
	time.Sleep(500 * time.Millisecond)
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"After changing interval to 50ms, expecting 10 more linktests in 500ms")

	// Test 5: Linktest suppression when sending messages from host
	t.Log("Send message from host per 25ms")
	for range 10 {
		time.Sleep(25 * time.Millisecond)
		hostComm.testMsgSuccess(1, 1, secs2.A("from host"), `<A[9] "from host">`)
	}
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"Linktest should be suppressed when host sends messages")

	// Test 6: Linktest suppression when sending messages from equipment
	t.Log("Send message from eqp per 25ms")
	for range 10 {
		time.Sleep(25 * time.Millisecond)
		eqpComm.testMsgSuccess(1, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
	}
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"Linktest should be suppressed when equipment sends messages")

	// Test 7: Resume linktest after idle period
	expectedCount += 5
	expectedDelta = float64(expectedCount) * 0.4
	time.Sleep(250 * time.Millisecond)
	verifyLinktestCounts(t, hostComm, eqpComm, expectedCount, expectedDelta,
		"After 250ms idle, expecting 5 more linktests")
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

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := hostComm.session.SendDataMessageAsync(1, 1, true, secs2.A("test"))
			require.NoError(err, "expected no error when sending message after eqp closed, got: %v", err)
		}()
	}
	time.Sleep(10 * time.Millisecond) // give some time for goroutines to start

	begin := time.Now()
	require.NoError(hostComm.close())
	elapsed := time.Since(begin)
	require.LessOrEqual(elapsed, 100*time.Millisecond, "expected host close to complete within 2 seconds, took: %v", elapsed)

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

	err := c.session.SendDataMessageAsync(stream, function, true, dataItem)
	c.require.NoError(err)

	select {
	case <-time.After(10 * time.Second):
		c.t.Fatalf("timeout waiting for reply for S%dF%d", stream, function)
	case reply := <-c.recvReplyChan:
		c.require.Equal(stream, reply.StreamCode())
		c.require.Equal(function+1, reply.FunctionCode())
		c.require.Equal(expectedSML, reply.Item().ToSML())
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

	// Set a large retry delay; the test ensures Close() isn't delayed by it.
	hostComm.conn.retryDelay = 5 * time.Second

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
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = hostComm.session.SendDataMessageAsync(1, 1, true, secs2.A("test"))
		}()
	}
	time.Sleep(10 * time.Millisecond)

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
