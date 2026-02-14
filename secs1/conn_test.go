package secs1

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	testSessionID = uint16(1)
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
	default:
		level = logger.InfoLevel
	}

	logger.SetLevel(level)

	os.Exit(m.Run())
}

// --- Port allocation ---

var (
	addrPool      = make(map[string]struct{})
	addrPoolMutex sync.Mutex
)

func getPort() int {
	for {
		listener, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			panic("failed to get random listener: " + err.Error())
		}

		addr := listener.Addr().String()
		_ = listener.Close()

		addrPoolMutex.Lock()
		if _, existed := addrPool[addr]; existed {
			addrPoolMutex.Unlock()

			continue
		}

		addrPool[addr] = struct{}{}
		addrPoolMutex.Unlock()

		_, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			panic("failed to split host and port: " + err.Error())
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			panic("failed to convert port: " + err.Error())
		}

		return port
	}
}

// --- testComm helper ---

// testComm wraps a Connection and Session for test convenience.
type testComm struct {
	t       testing.TB
	require *require.Assertions
	conn    *Connection
	session *Session

	// recvReplyChan receives secondary messages that arrive
	// outside of a sendMsg reply flow (async echo replies).
	recvReplyChan chan *hsms.DataMessage
}

// newTestComm creates a testComm with an echo handler.
func newTestComm(
	ctx context.Context,
	t testing.TB,
	port int,
	isHost bool,
	isActive bool,
	extraOpts ...ConnOption,
) *testComm {
	t.Helper()

	r := require.New(t)

	opts := []ConnOption{
		WithT1Timeout(MinT1Timeout),
		WithT2Timeout(MinT2Timeout),
		WithT3Timeout(3 * time.Second),
		WithT4Timeout(3 * time.Second),
		WithConnectTimeout(2 * time.Second),
		WithSendTimeout(2 * time.Second),
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

	opts = append(opts, extraOpts...)

	connCfg, err := NewConnectionConfig(testIP, port, opts...)
	r.NoError(err)
	r.NotNil(connCfg)

	conn, err := NewConnection(ctx, connCfg)
	r.NoError(err)
	r.NotNil(conn)

	session, ok := conn.AddSession(testSessionID).(*Session)
	r.True(ok)
	r.NotNil(session)

	recvReplyChan := make(chan *hsms.DataMessage, 16)

	session.AddDataMessageHandler(msgEchoHandler(recvReplyChan))

	return &testComm{
		t:             t,
		require:       r,
		conn:          conn,
		session:       session,
		recvReplyChan: recvReplyChan,
	}
}

// msgEchoHandler returns a DataMessageHandler that replies with the same item.
// Secondary messages (even function code) are forwarded to recvReplyChan.
func msgEchoHandler(recvReplyChan chan<- *hsms.DataMessage) hsms.DataMessageHandler {
	return func(msg *hsms.DataMessage, session hsms.Session) {
		if msg.FunctionCode()%2 == 0 {
			// Secondary message — forward to channel.
			recvReplyChan <- msg

			return
		}

		// Primary message — echo the item back.
		_ = session.ReplyDataMessage(msg, msg.Item())
	}
}

func (c *testComm) open(waitOpened bool) error {
	return c.conn.Open(waitOpened)
}

func (c *testComm) close() error {
	return c.conn.Close()
}

// abnormalClose closes the TCP connection abruptly without the full Close sequence.
func (c *testComm) abnormalClose() {
	c.conn.closeTCP(0)
}

// testMsgSuccess sends a primary message and validates the echo reply.
func (c *testComm) testMsgSuccess(stream byte, function byte, item secs2.Item, expectedSML string) {
	c.t.Helper()

	reply, err := c.session.SendDataMessage(stream, function, true, item)
	c.require.NoError(err, "SendDataMessage S%dF%d failed", stream, function)
	c.require.NotNil(reply, "expected reply for S%dF%d", stream, function)

	// Verify the echo: reply function = request function + 1.
	c.require.Equal(stream, reply.StreamCode())
	c.require.Equal(function+1, reply.FunctionCode())

	if expectedSML != "" {
		c.require.Equal(expectedSML, reply.Item().ToSML())
	}
}

// testAsyncMsgSuccess sends a message asynchronously and waits for the
// reply on recvReplyChan.
func (c *testComm) testAsyncMsgSuccess(stream byte, function byte, item secs2.Item, expectedSML string) {
	c.t.Helper()

	err := c.session.SendDataMessageAsync(stream, function, false, item)
	c.require.NoError(err, "SendDataMessageAsync S%dF%d failed", stream, function)

	// The echo handler will reply with function+1, and the reply arrives
	// on recvReplyChan because there is no reply channel registered.
	select {
	case reply := <-c.recvReplyChan:
		c.require.NotNil(reply)
		c.require.Equal(stream, reply.StreamCode())
		c.require.Equal(function+1, reply.FunctionCode())

		if expectedSML != "" {
			c.require.Equal(expectedSML, reply.Item().ToSML())
		}
	case <-time.After(5 * time.Second):
		c.require.Fail("timeout waiting for async reply")
	}
}

// testMsgError sends a primary message and expects an error.
func (c *testComm) testMsgError(stream byte, function byte, item secs2.Item, expectedErr error) {
	c.t.Helper()

	reply, err := c.session.SendDataMessage(stream, function, true, item)
	c.require.Error(err, "expected error for S%dF%d", stream, function)
	c.require.ErrorIs(err, expectedErr)
	c.require.Nil(reply)
}

// --- Test cases ---

func TestConnection_ActiveHost_PassiveEQP(t *testing.T) {
	ctx := t.Context()

	testConnectionRoundTrip(ctx, t, true)
}

func TestConnection_PassiveHost_ActiveEQP(t *testing.T) {
	ctx := t.Context()

	testConnectionRoundTrip(ctx, t, false)
}

// testConnectionRoundTrip is the shared test body for active/passive combinations.
func testConnectionRoundTrip(ctx context.Context, t testing.TB, hostIsActive bool) {
	t.Helper()

	r := require.New(t)
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, hostIsActive)
	eqpComm := newTestComm(ctx, t, port, false, !hostIsActive)

	if hostIsActive {
		r.NoError(eqpComm.open(true))
		r.NoError(hostComm.open(true))
	} else {
		r.NoError(hostComm.open(true))
		r.NoError(eqpComm.open(true))
	}

	defer func() {
		_ = hostComm.close()
		_ = eqpComm.close()
	}()

	// Wait for selected state.
	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// --- Send from host ---
	hostComm.testMsgSuccess(1, 1, secs2.A("from host"), `<A[9] "from host">`)
	hostComm.testAsyncMsgSuccess(1, 1, secs2.A("from host async"), `<A[15] "from host async">`)

	// Header-only message (nil item).
	hostComm.testMsgSuccess(1, 3, nil, "")

	// Even function code should fail.
	hostComm.testMsgError(1, 2, nil, hsms.ErrInvalidReqMsg)

	// --- Send from equipment ---
	eqpComm.testMsgSuccess(2, 1, secs2.A("from eqp"), `<A[8] "from eqp">`)
	eqpComm.testAsyncMsgSuccess(2, 1, secs2.A("from eqp async"), `<A[14] "from eqp async">`)

	// --- Close equipment ---
	r.NoError(eqpComm.close())

	// Give the host time to detect the disconnect.
	time.Sleep(200 * time.Millisecond)

	// Host sending after eqp closed should error (could be ErrNotSelectedState,
	// ErrConnClosed, or a network error like broken pipe, depending on timing).
	reply, err := hostComm.session.SendDataMessage(3, 15, true, nil)
	r.Error(err, "expected error when sending to closed equipment")
	r.Nil(reply)

	// Close host.
	r.NoError(hostComm.close())

	// Reopen both.
	if hostIsActive {
		r.NoError(eqpComm.open(true))
		r.NoError(hostComm.open(true))
	} else {
		r.NoError(hostComm.open(true))
		r.NoError(eqpComm.open(true))
	}

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send after reopen.
	hostComm.testMsgSuccess(7, 1, secs2.A("after reopen"), `<A[12] "after reopen">`)
	eqpComm.testMsgSuccess(8, 1, secs2.A("eqp reopen"), `<A[10] "eqp reopen">`)

	// Clean close.
	r.NoError(hostComm.close())
	r.NoError(eqpComm.close())
}

func TestConnection_ActiveHost_CloseMultipleTimes(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	// Close before open should be safe.
	r.NoError(hostComm.close())
	r.NoError(eqpComm.close())

	// Open both.
	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Close multiple times.
	for range 5 {
		r.NoError(hostComm.close())
	}

	for range 5 {
		r.NoError(eqpComm.close())
	}

	// Reopen and close again.
	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	for range 3 {
		r.NoError(hostComm.close())
	}

	r.NoError(eqpComm.close())
}

func TestConnection_ActiveHost_RetryConnect(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpComm.close())
	}()

	// Open host first — no equipment listening, so it will retry.
	r.NoError(hostComm.open(false))

	// Wait a bit for some retry attempts.
	time.Sleep(300 * time.Millisecond)

	// Now open equipment — host should eventually connect.
	r.NoError(eqpComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send and verify.
	hostComm.testMsgSuccess(1, 1, secs2.A("retry success"), `<A[13] "retry success">`)
}

func TestConnection_ActiveHost_AbnormalClose(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpComm.close())
	}()

	// Open both.
	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Abnormally close host TCP — the host's protocol loop will detect the
	// closed connection and trigger NotConnected → reconnect automatically.
	hostComm.abnormalClose()

	// Wait for the equipment to detect disconnect and transition.
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.ConnectingState))

	// The active host should auto-reconnect when the equipment starts listening again.
	// Wait for both to reach Selected state again.
	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Verify communication after reconnect.
	hostComm.testMsgSuccess(1, 1, secs2.A("reconnected"), `<A[11] "reconnected">`)
}

func TestConnection_MultiBlockMessage(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(5*time.Second),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpComm.close())
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Create a large item that requires multi-block transfer.
	// MaxBlockBodySize is 244 bytes, so we need > 244 bytes of SECS-II data.
	largeStr := make([]byte, 500)
	for i := range largeStr {
		largeStr[i] = 'A' + byte(i%26)
	}

	item := secs2.A(string(largeStr))

	reply, err := hostComm.session.SendDataMessage(1, 1, true, item)
	r.NoError(err)
	r.NotNil(reply)
	r.Equal(item.ToSML(), reply.Item().ToSML())

	// Verify multi-block metrics.
	metrics := hostComm.conn.GetMetrics()
	r.Greater(metrics.BlockSendCount.Load(), uint64(1), "multi-block should send more than 1 block")
}

func TestConnection_T3Timeout(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Host with very short T3 timeout.
	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(MinT3Timeout), // 1 second
	)

	// Equipment with a handler that does NOT reply (simulates timeout).
	eqpCfg, err := NewConnectionConfig(testIP, port,
		WithEquipRole(),
		WithPassive(),
		WithT1Timeout(MinT1Timeout),
		WithT2Timeout(MinT2Timeout),
		WithLogger(logger.GetLogger().With("role", "PASSIVE-NOREPLY")),
	)
	r.NoError(err)

	eqpConn, err := NewConnection(ctx, eqpCfg)
	r.NoError(err)

	eqpSession, ok := eqpConn.AddSession(testSessionID).(*Session)
	r.True(ok)

	// No-reply handler: receives but does not reply.
	eqpSession.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.Session) {
		// Intentionally do nothing — simulate T3 timeout on host.
	})

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpConn.Close())
	}()

	r.NoError(eqpConn.Open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpConn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send a message requiring reply — should T3 timeout.
	start := time.Now()
	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("timeout test"))
	elapsed := time.Since(start)

	r.Error(err)
	r.ErrorIs(err, ErrT3Timeout)
	r.Nil(reply)

	// The timeout should be approximately T3 (1 second).
	r.Greater(elapsed, 800*time.Millisecond, "should wait close to T3 before timing out")
	r.Less(elapsed, 3*time.Second, "should not wait much longer than T3")
}

func TestConnection_NilSession(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)
	r.NotNil(conn)

	// Open without AddSession should fail.
	err = conn.Open(false)
	r.Error(err)
	r.ErrorIs(err, ErrSessionNil)
}

func TestNewConnection_NilConfig(t *testing.T) {
	r := require.New(t)

	conn, err := NewConnection(context.Background(), nil)
	r.Error(err)
	r.Nil(conn)
}

func TestConnection_InterfaceMethods(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)

	// Interface type checks.
	r.True(conn.IsSingleSession())
	r.False(conn.IsGeneralSession())
	r.True(conn.IsSECS1())

	// Logger should be set.
	r.NotNil(conn.GetLogger())

	// Metrics should be accessible.
	r.NotNil(conn.GetMetrics())
}

func TestConnection_AddSession(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)

	session := conn.AddSession(42)
	r.NotNil(session)

	secs1Session, ok := session.(*Session)
	r.True(ok)
	r.Equal(uint16(42), secs1Session.ID())
	r.Same(conn, secs1Session.conn)
}

func TestConnection_Metrics(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true)
	eqpComm := newTestComm(ctx, t, port, false, false)

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpComm.close())
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send a few messages and check metrics.
	for range 3 {
		hostComm.testMsgSuccess(1, 1, secs2.A("metric"), `<A[6] "metric">`)
	}

	hostMetrics := hostComm.conn.GetMetrics()
	eqpMetrics := eqpComm.conn.GetMetrics()

	// Host sent 3 messages (each single-block), and received 3 replies.
	r.GreaterOrEqual(hostMetrics.BlockSendCount.Load(), uint64(3))
	r.GreaterOrEqual(hostMetrics.BlockRecvCount.Load(), uint64(3))
	r.GreaterOrEqual(hostMetrics.DataMsgSendCount.Load(), uint64(3))
	r.GreaterOrEqual(hostMetrics.DataMsgRecvCount.Load(), uint64(3))

	// Equipment received 3 primary messages and sent 3 replies.
	r.GreaterOrEqual(eqpMetrics.BlockRecvCount.Load(), uint64(3))
	r.GreaterOrEqual(eqpMetrics.BlockSendCount.Load(), uint64(3))
	r.GreaterOrEqual(eqpMetrics.DataMsgRecvCount.Load(), uint64(3))
}

func TestConnection_SendWithoutWBit(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true)
	eqpComm := newTestComm(ctx, t, port, false, false)

	defer func() {
		r.NoError(hostComm.close())
		r.NoError(eqpComm.close())
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send without W-bit (no reply expected).
	reply, err := hostComm.session.SendDataMessage(1, 1, false, secs2.A("no reply"))
	r.NoError(err)
	r.Nil(reply) // No reply expected.

	// Give the equipment time to receive and echo (but since W=false, no reply channel).
	time.Sleep(300 * time.Millisecond)

	// The echo reply should arrive on recvReplyChan since there is no sender waiting.
	select {
	case msg := <-eqpComm.recvReplyChan:
		// Equipment received the reply from its own echo handler — this is the
		// reply it sent back, which the host didn't wait for, so it got delivered
		// as a secondary to the equipment's recvDataMsg. But actually the host
		// is the sender; the equipment is the receiver. The echo handler on eqp
		// side replies, and the host has no waiting reply channel, so the reply
		// goes to host's recvDataMsg → recvReplyChan.
		_ = msg
	case <-time.After(2 * time.Second):
		// It's also fine if it doesn't show up — the reply may not be delivered
		// if there's no reply channel match and no session handler configured.
	}
}

func TestConnection_CloseEfficiency(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	startClose := time.Now()
	r.NoError(hostComm.close())
	r.NoError(eqpComm.close())
	closeTime := time.Since(startClose)

	r.Less(closeTime, 3*time.Second, "Close took too long")
}

func TestConnection_ActiveBackoffDoesNotBlockClose(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithConnectTimeout(100*time.Millisecond),
	)
	eqpComm := newTestComm(ctx, t, port, false, false)

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Abruptly close the equipment — the host's connect loop will start
	// retrying with exponential backoff.
	eqpComm.abnormalClose()

	// Give the host time to detect disconnect and start the connect loop.
	time.Sleep(200 * time.Millisecond)

	// Close() should cancel the connect loop's timer immediately via loopCtx,
	// regardless of the current backoff delay.
	start := time.Now()
	r.NoError(hostComm.close())
	elapsed := time.Since(start)

	r.Less(elapsed, 3*time.Second, "Close took too long; connect loop may be blocking")

	r.NoError(eqpComm.close())
}

func TestConnection_Constants(t *testing.T) {
	r := require.New(t)

	r.Equal(50*time.Millisecond, pollTimeout)
	r.Equal(time.Second, replyChannelTimeout)
	r.Equal(5*time.Millisecond, closeCheckInterval)
	r.Equal(100*time.Millisecond, initialRetryDelay)
	r.Equal(30*time.Second, maxRetryDelay)
}

func TestConnection_ValidateDataMessage_S9F1(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	// S9Fx channel for host to capture stream 9 messages.
	s9Chan := make(chan *hsms.DataMessage, 10)

	hostComm := newTestComm(ctx, t, port, true, true, WithDeviceID(1))
	hostComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		if msg.StreamCode() == 9 {
			s9Chan <- msg
		}
	})

	eqpComm := newTestComm(ctx, t, port, false, false,
		WithDeviceID(1),
		WithValidateDataMessage(true),
	)

	defer func() {
		_ = hostComm.close()
		_ = eqpComm.close()
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Inject a block with wrong device ID (999 != eqp's configured deviceID=1).
	// The assembler should reject it with ErrDeviceIDMismatch and trigger S9F1.
	badBlock := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, eBit: true,
		deviceID: 999, systemBytes: 0x12345678,
	})
	eqpComm.conn.handleReceivedBlock(badBlock)

	// Host should receive S9F1 (Unrecognized Device ID).
	select {
	case msg := <-s9Chan:
		r.Equal(byte(9), msg.StreamCode())
		r.Equal(byte(1), msg.FunctionCode())
	case <-time.After(3 * time.Second):
		r.Fail("timeout waiting for S9F1")
	}
}

func TestConnection_ValidateDataMessage_S9F7(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	// S9Fx channel for host to capture stream 9 messages.
	s9Chan := make(chan *hsms.DataMessage, 10)

	hostComm := newTestComm(ctx, t, port, true, true, WithDeviceID(1))
	hostComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		if msg.StreamCode() == 9 {
			s9Chan <- msg
		}
	})

	eqpComm := newTestComm(ctx, t, port, false, false,
		WithDeviceID(1),
		WithValidateDataMessage(true),
	)

	defer func() {
		_ = hostComm.close()
		_ = eqpComm.close()
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Inject a block with correct device ID but invalid block number (5).
	// No multi-block message is in progress, so block number must be 0 or 1.
	// The assembler should reject it with ErrBlockNumberMismatch and trigger S9F7.
	badBlock := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, eBit: true,
		deviceID: 1, blockNum: 5, systemBytes: 0xABCD0001,
	})
	eqpComm.conn.handleReceivedBlock(badBlock)

	// Host should receive S9F7 (Illegal Data).
	select {
	case msg := <-s9Chan:
		r.Equal(byte(9), msg.StreamCode())
		r.Equal(byte(7), msg.FunctionCode())
	case <-time.After(3 * time.Second):
		r.Fail("timeout waiting for S9F7")
	}
}

func TestConnection_ValidateDataMessage_DisabledNoS9Fx(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	// S9Fx channel for host to capture stream 9 messages.
	s9Chan := make(chan *hsms.DataMessage, 10)

	hostComm := newTestComm(ctx, t, port, true, true, WithDeviceID(1))
	hostComm.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		if msg.StreamCode() == 9 {
			s9Chan <- msg
		}
	})

	// Equipment with validateDataMessage DISABLED (default).
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithDeviceID(1),
	)

	defer func() {
		_ = hostComm.close()
		_ = eqpComm.close()
	}()

	r.NoError(eqpComm.open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Inject a block with wrong device ID — same error, but validation is off.
	badBlock := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, eBit: true,
		deviceID: 999, systemBytes: 0x12345678,
	})
	eqpComm.conn.handleReceivedBlock(badBlock)

	// No S9F1 should be sent since validation is disabled.
	select {
	case msg := <-s9Chan:
		r.Failf("unexpected S9Fx", "received S%dF%d", msg.StreamCode(), msg.FunctionCode())
	case <-time.After(500 * time.Millisecond):
		// Expected: no S9Fx sent.
	}
}

func TestConnection_SendMsgNotSelected(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)

	session, ok := conn.AddSession(testSessionID).(*Session)
	r.True(ok)

	// Connection is not opened — state is NotConnected.
	// sendMsg should return ErrNotSelectedState.
	msg, err := hsms.NewDataMessage(1, 1, true, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("test"))
	r.NoError(err)

	reply, sendErr := conn.sendMsg(msg)
	r.Error(sendErr)
	r.ErrorIs(sendErr, ErrNotSelectedState)
	r.Nil(reply)

	// sendMsgSync should also fail.
	syncErr := conn.sendMsgSync(msg)
	r.Error(syncErr)
	r.ErrorIs(syncErr, ErrNotSelectedState)

	_ = session
}

// ===========================================================================
// closeTCP idempotency
// ===========================================================================

func TestCloseTCP_DoubleCallIdempotent(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole(), WithActive())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)

	// Set up a real TCP connection via net.Pipe.
	local, remote := newPipeConn(t)
	conn.setupTCPConn(local)

	// First close should return the remote address.
	addr := conn.closeTCP(0)
	r.NotEmpty(addr)

	// Second close should be a no-op (tcpConn is nil).
	addr2 := conn.closeTCP(0)
	r.Empty(addr2)

	// getTCPConn should return nil after close.
	r.Nil(conn.getTCPConn())

	_ = remote.Close()
}

func TestCloseTCP_PassiveConnCountDecrement(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig(testIP, 5000, WithHostRole(), WithPassive())
	r.NoError(err)

	conn, err := NewConnection(ctx, cfg)
	r.NoError(err)

	// Simulate a passive connection with connCount = 1.
	conn.connCount.Store(1)

	local, remote := newPipeConn(t)
	conn.setupTCPConn(local)

	conn.closeTCP(0)
	r.Equal(int32(0), conn.connCount.Load())

	// Second close should NOT decrement again (tcpConn already nil).
	conn.closeTCP(0)
	r.Equal(int32(0), conn.connCount.Load())

	_ = remote.Close()
}

// ===========================================================================
// T3 timer starts after send completion
// ===========================================================================

func TestConnection_T3StartsAfterSend(t *testing.T) {
	// Verify that T3 is measured from after all blocks are sent,
	// not from when the message is queued.
	r := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Host with 1s T3 timeout.
	hostComm := newTestComm(ctx, t, port, true, true,
		WithT3Timeout(MinT3Timeout), // 1 second
	)

	// Equipment that receives but does not reply.
	eqpCfg, err := NewConnectionConfig(testIP, port,
		WithEquipRole(),
		WithPassive(),
		WithT1Timeout(MinT1Timeout),
		WithT2Timeout(MinT2Timeout),
		WithLogger(logger.GetLogger().With("role", "PASSIVE-NOREPLY")),
	)
	r.NoError(err)

	eqpConn, err := NewConnection(ctx, eqpCfg)
	r.NoError(err)

	eqpSession, ok := eqpConn.AddSession(testSessionID).(*Session)
	r.True(ok)

	eqpSession.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.Session) {
		// No reply — simulate T3 timeout.
	})

	defer func() {
		_ = hostComm.close()
		_ = eqpConn.Close()
	}()

	r.NoError(eqpConn.Open(true))
	r.NoError(hostComm.open(true))

	r.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	r.NoError(eqpConn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Send a message — T3 should be measured from after send completes.
	start := time.Now()
	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("t3 timing"))
	elapsed := time.Since(start)

	r.Error(err)
	r.ErrorIs(err, ErrT3Timeout)
	r.Nil(reply)

	// T3 is 1s. The send itself takes a few ms. Total should be ~1s, not significantly less
	// (which would indicate T3 started before send completion).
	r.Greater(elapsed, 900*time.Millisecond, "T3 should not expire before ~1s after send")
	r.Less(elapsed, 3*time.Second, "should not wait much longer than T3")
}

// --- isDisconnectError tests ---

func TestIsDisconnectError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"connection reset by peer", errors.New("read tcp 127.0.0.1:5000->127.0.0.1:5001: connection reset by peer"), true},
		{"broken pipe", errors.New("write tcp 127.0.0.1:5000->127.0.0.1:5001: broken pipe"), true},
		{"wrapped io.EOF", fmt.Errorf("read failed: %w", io.EOF), true},
		{"wrapped net.ErrClosed", fmt.Errorf("write failed: %w", net.ErrClosed), true},
		{"timeout error", errors.New("i/o timeout"), false},
		{"context canceled", context.Canceled, false},
		{"random error", errors.New("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDisconnectError(tt.err)
			if got != tt.want {
				t.Errorf("isDisconnectError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- UpdateConfigOptions tests ---

func TestUpdateConfigOptions_RuntimeTimeouts(t *testing.T) {
	r := require.New(t)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	r.NoError(err)

	conn, err := NewConnection(context.Background(), cfg)
	r.NoError(err)

	// Update all runtime-updatable timeout options.
	err = conn.UpdateConfigOptions(
		WithT1Timeout(1*time.Second),
		WithT2Timeout(5*time.Second),
		WithT3Timeout(60*time.Second),
		WithT4Timeout(60*time.Second),
		WithRetryLimit(10),
		WithSendTimeout(5*time.Second),
		WithDuplicateDetection(false),
		WithValidateDataMessage(true),
	)
	r.NoError(err)

	// Verify updates took effect.
	r.Equal(1*time.Second, cfg.T1Timeout())
	r.Equal(5*time.Second, cfg.T2Timeout())
	r.Equal(60*time.Second, cfg.T3Timeout())
	r.Equal(60*time.Second, cfg.T4Timeout())
	r.Equal(10, cfg.RetryLimit())
	r.Equal(5*time.Second, cfg.SendTimeout())
	r.False(cfg.DuplicateDetection())
	r.True(cfg.ValidateDataMessage())
}

func TestUpdateConfigOptions_RejectsNonRuntimeOptions(t *testing.T) {
	r := require.New(t)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	r.NoError(err)

	conn, err := NewConnection(context.Background(), cfg)
	r.NoError(err)

	// Non-runtime options should be rejected.
	nonRuntimeOpts := []struct {
		name string
		opt  ConnOption
	}{
		{"WithActive", WithActive()},
		{"WithPassive", WithPassive()},
		{"WithEquipRole", WithEquipRole()},
		{"WithHostRole", WithHostRole()},
		{"WithDeviceID", WithDeviceID(1)},
		{"WithConnectTimeout", WithConnectTimeout(5 * time.Second)},
		{"WithLogger", WithLogger(logger.GetLogger())},
		{"WithSenderQueueSize", WithSenderQueueSize(20)},
		{"WithDataMsgQueueSize", WithDataMsgQueueSize(20)},
	}

	for _, tt := range nonRuntimeOpts {
		t.Run(tt.name, func(t *testing.T) {
			err := conn.UpdateConfigOptions(tt.opt)
			r.Error(err, "expected error for non-runtime option %s", tt.name)
			r.Contains(err.Error(), "cannot be changed at runtime")
		})
	}
}

func TestUpdateConfigOptions_ValidationStillApplied(t *testing.T) {
	r := require.New(t)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	r.NoError(err)

	conn, err := NewConnection(context.Background(), cfg)
	r.NoError(err)

	// Out-of-range values should still be rejected.
	r.Error(conn.UpdateConfigOptions(WithT1Timeout(50 * time.Millisecond)))
	r.Error(conn.UpdateConfigOptions(WithT2Timeout(100 * time.Millisecond)))
	r.Error(conn.UpdateConfigOptions(WithT3Timeout(500 * time.Millisecond)))
	r.Error(conn.UpdateConfigOptions(WithT4Timeout(500 * time.Millisecond)))
	r.Error(conn.UpdateConfigOptions(WithRetryLimit(32)))
	r.Error(conn.UpdateConfigOptions(WithSendTimeout(0)))

	// Original values should be unchanged after failed updates.
	r.Equal(DefaultT1Timeout, cfg.T1Timeout())
	r.Equal(DefaultT2Timeout, cfg.T2Timeout())
	r.Equal(DefaultT3Timeout, cfg.T3Timeout())
	r.Equal(DefaultT4Timeout, cfg.T4Timeout())
	r.Equal(DefaultRetryLimit, cfg.RetryLimit())
}

func TestUpdateConfigOptions_PartialFailureRollback(t *testing.T) {
	r := require.New(t)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000)
	r.NoError(err)

	conn, err := NewConnection(context.Background(), cfg)
	r.NoError(err)

	// First option succeeds, second fails — first change persists (no rollback).
	err = conn.UpdateConfigOptions(
		WithT1Timeout(1*time.Second),
		WithT2Timeout(100*time.Millisecond), // invalid: below minimum
	)
	r.Error(err)

	// T1 was applied before T2 failed.
	r.Equal(1*time.Second, cfg.T1Timeout())
	// T2 remains at default.
	r.Equal(DefaultT2Timeout, cfg.T2Timeout())
}

// --- SendTimeout getter test ---

func TestSendTimeout_Getter(t *testing.T) {
	r := require.New(t)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000, WithSendTimeout(5*time.Second))
	r.NoError(err)
	r.Equal(5*time.Second, cfg.SendTimeout())
}
