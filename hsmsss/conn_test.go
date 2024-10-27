package hsmsss

import (
	"context"
	"errors"
	"net"
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
	time.Sleep(time.Second)

	// empty item
	hostComm.testMsgSuccess(1, 3, nil, "")

	// even function code
	hostComm.testMsgError(1, 2, nil, hsms.ErrInvalidReqMsg)

	// send from eqp
	eqpComm.testMsgSuccess(2, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
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
	conn    hsms.Connection
	session hsms.Session
}

func newTestComm(ctx context.Context, require *require.Assertions, isHost bool, isActive bool) *testComm {
	c := &testComm{ctx: ctx, require: require}

	c.conn = newConn(ctx, require, isHost, isActive)
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

	opErr := &net.OpError{}
	if errors.As(err, &opErr) {
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

func newConn(ctx context.Context, require *require.Assertions, isHost, isActive bool) *Connection {
	opts := []ConnOption{
		WithHostRole(),
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
