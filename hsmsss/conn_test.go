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
	ctx := context.Background()

	testConnection(ctx, t, true)
}

func TestConnection_PassiveHost_ActiveEQP(t *testing.T) {
	ctx := context.Background()

	testConnection(ctx, t, false)
}

func TestConnection_ActiveHost_AbnormalClose(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

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

	ctx := context.Background()

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

	ctx := context.Background()

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

func TestConnection_Linktest(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
	)

	var hostMetrics *ConnectionMetrics
	var eqpMetrics *ConnectionMetrics

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	expectedTotal := uint64(5)
	expectedDelta := float64(expectedTotal) * 0.2
	time.Sleep(500 * time.Millisecond)

	// expects to receive 5 linktests after connection established
	hostMetrics = hostComm.conn.GetMetrics()
	eqpMetrics = eqpComm.conn.GetMetrics()
	require.InDelta(hostMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(hostMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)

	t.Log("Disable linktest")
	require.NoError(hostComm.conn.UpdateConfigOptions(WithAutoLinktest(false)))
	require.NoError(eqpComm.conn.UpdateConfigOptions(WithAutoLinktest(false)))

	time.Sleep(200 * time.Millisecond)

	// expects no mote linktests after linktest disabled
	hostMetrics = hostComm.conn.GetMetrics()
	eqpMetrics = eqpComm.conn.GetMetrics()
	require.InDelta(hostMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(hostMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)

	t.Log("Resume linktest")
	require.NoError(hostComm.conn.UpdateConfigOptions(WithAutoLinktest(true)))
	require.NoError(eqpComm.conn.UpdateConfigOptions(WithAutoLinktest(true)))

	expectedTotal += 5
	expectedDelta = float64(expectedTotal) * 0.2
	time.Sleep(500 * time.Millisecond)

	// expects to receive 5 more linktests after linktest enabled
	hostMetrics = hostComm.conn.GetMetrics()
	eqpMetrics = eqpComm.conn.GetMetrics()
	require.InDelta(hostMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(hostMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)

	t.Log("Change linktest interval to 50ms")
	require.NoError(hostComm.conn.UpdateConfigOptions(WithLinktestInterval(50 * time.Millisecond)))
	require.NoError(eqpComm.conn.UpdateConfigOptions(WithLinktestInterval(50 * time.Millisecond)))

	expectedTotal += 10
	expectedDelta = float64(expectedTotal) * 0.2
	time.Sleep(500 * time.Millisecond)

	// expects to receive 1- more linktests after linktest interval changed to 50ms
	hostMetrics = hostComm.conn.GetMetrics()
	eqpMetrics = eqpComm.conn.GetMetrics()
	require.InDelta(hostMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(hostMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestSendCount.Load(), expectedTotal, expectedDelta)
	require.InDelta(eqpMetrics.LinktestRecvCount.Load(), expectedTotal, expectedDelta)

	t.Log("Close connection")
	// close host
	require.NoError(hostComm.close())
	// close equipment
	require.NoError(eqpComm.close())
}

func TestSendMessageFail(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

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
	require.Equal(secs2.A("test"), reply.Item(), "expected reply item to be 'test', got: %v", reply.Item())
}

func TestDrainMessageOnConnClose(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()

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
	ctx := context.Background()

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
