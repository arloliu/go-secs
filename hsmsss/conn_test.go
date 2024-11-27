package hsmsss

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

const (
	testIP        = "127.0.0.1"
	testPort      = 15000
	testSessionID = 9527
)

func TestConnection_ActiveHost_PassiveEQP(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	logger.SetLevel(logger.InfoLevel)

	testConnection(ctx, require, true)
}

func TestConnection_PassiveHost_ActiveEQP(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	logger.SetLevel(logger.InfoLevel)

	testConnection(ctx, require, false)
}

func TestConnection_ActiveHost_RetryConnect(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	logger.SetLevel(logger.DebugLevel)

	hostComm := newTestComm(ctx, require, true, true,
		WithT5Timeout(100*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, require, false, false,
		WithT5Timeout(100*time.Millisecond),
		WithConnectRemoteTimeout(100*time.Millisecond),
	)

	var hostMetrics *ConnectionMetrics

	require.NoError(hostComm.conn.Open(false))

	time.Sleep(300 * time.Millisecond)

	hostMetrics = hostComm.conn.GetMetrics()
	require.GreaterOrEqual(hostMetrics.ConnRetryGauge.Load(), uint32(3))

	require.NoError(eqpComm.conn.Open(true))

	err := hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState)
	require.NoError(err)

	require.Equal(hostMetrics.ConnRetryGauge.Load(), uint32(0))

	// close host
	require.NoError(hostComm.conn.Close())
	// close equipment
	require.NoError(eqpComm.conn.Close())
}

func TestConnection_Linktest(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	logger.SetLevel(logger.InfoLevel)

	hostComm := newTestComm(ctx, require, true, true,
		WithAutoLinktest(true),
		WithLinktestInterval(100*time.Millisecond),
		WithT6Timeout(1000*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, require, false, false, WithAutoLinktest(true), WithLinktestInterval(100*time.Millisecond))

	var hostMetrics *ConnectionMetrics
	var eqpMetrics *ConnectionMetrics

	require.NoError(eqpComm.conn.Open(true))
	require.NoError(hostComm.conn.Open(true))

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
	require.NoError(hostComm.conn.Close())
	// close equipment
	require.NoError(eqpComm.conn.Close())
}

func testConnection(ctx context.Context, require *require.Assertions, hostIsActive bool) {
	hostComm := newTestComm(ctx, require, true, hostIsActive)
	eqpComm := newTestComm(ctx, require, false, !hostIsActive)

	if hostIsActive {
		require.NoError(eqpComm.conn.Open(true))
		require.NoError(hostComm.conn.Open(true))
	} else {
		require.NoError(hostComm.conn.Open(true))
		require.NoError(eqpComm.conn.Open(true))
	}
	defer hostComm.conn.Close()
	defer eqpComm.conn.Close()

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
	require.NoError(eqpComm.conn.Close())

	// expects to get error when equipment has closed
	hostComm.testMsgNetError(3, 15, nil, hsms.ErrConnClosed, hsms.ErrNotSelectedState)

	// close host
	require.NoError(hostComm.conn.Close())

	// reopen
	if hostIsActive {
		require.NoError(eqpComm.conn.Open(true))
	} else {
		require.NoError(hostComm.conn.Open(true))
	}

	// expects to get error when host has closed
	eqpComm.testMsgNetError(3, 17, nil, hsms.ErrNotSelectedState)

	// reopen
	if hostIsActive {
		// hostComm = newTestComm(ctx, require, true, hostIsActive)
		require.NoError(hostComm.conn.Open(true))
	} else {
		// eqpComm = newTestComm(ctx, require, false, !hostIsActive)
		require.NoError(eqpComm.conn.Open(true))
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
	require.NoError(hostComm.conn.Close())
	// close equipment
	require.NoError(eqpComm.conn.Close())
}

type testComm struct {
	ctx     context.Context
	require *require.Assertions
	conn    *Connection
	session hsms.Session

	recvReplyChan chan *hsms.DataMessage
}

func newTestComm(ctx context.Context, require *require.Assertions, isHost bool, isActive bool, extraOpts ...ConnOption) *testComm {
	c := &testComm{ctx: ctx, require: require}

	c.recvReplyChan = make(chan *hsms.DataMessage, 1)

	c.conn = newConn(ctx, require, isHost, isActive, extraOpts...)
	require.NotNil(c.conn)

	require.True(c.conn.IsSingleSession())
	require.False(c.conn.IsGeneralSession())
	require.NotNil(c.conn.GetLogger())

	require.ErrorIs(c.conn.Open(false), hsms.ErrSessionNil)

	c.session = c.conn.AddSession(testSessionID)
	require.NotNil(c.session)

	c.session.AddDataMessageHandler(c.msgEchoHandler)

	return c
}

func (c *testComm) msgEchoHandler(msg *hsms.DataMessage, session hsms.Session) {
	// receives reply message
	if msg.FunctionCode()%2 == 0 {
		c.recvReplyChan <- msg
		return
	}

	err := session.ReplyDataMessage(msg, msg.Item())
	c.require.NoError(err)
}

func (c *testComm) testMsgSuccess(stream byte, function byte, dataItem secs2.Item, expectedSML string) {
	reply, err := c.session.SendDataMessage(stream, function, true, dataItem)
	c.require.NoError(err)
	c.require.NotNil(reply)
	c.require.Equal(stream, reply.StreamCode())
	c.require.Equal(function+1, reply.FunctionCode())
	c.require.Equal(expectedSML, reply.Item().ToSML())
}

func (c *testComm) testAsyncMsgSuccess(stream byte, function byte, dataItem secs2.Item, expectedSML string) {
	err := c.session.SendDataMessageAsync(stream, function, true, dataItem)
	c.require.NoError(err)

	reply := <-c.recvReplyChan
	c.require.Equal(stream, reply.StreamCode())
	c.require.Equal(function+1, reply.FunctionCode())
	c.require.Equal(expectedSML, reply.Item().ToSML())
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

func newConn(ctx context.Context, require *require.Assertions, isHost, isActive bool, extraOpts ...ConnOption) *Connection {
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

	connCfg, err := NewConnectionConfig(testIP, testPort, opts...)
	require.NotNil(connCfg)
	require.NoError(err)

	conn, err := NewConnection(ctx, connCfg)
	require.NotNil(conn)
	require.NoError(err)

	return conn
}
