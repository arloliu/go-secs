package hsmsssintegration

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/require"
)

type captureLog struct {
	logger.Logger
	dialFailTimes *[]time.Time
	mu            *sync.Mutex
}

func (l *captureLog) Debug(msg string, keysAndValues ...any) {
	if msg == "failed to dial to equipment" {
		l.mu.Lock()
		*l.dialFailTimes = append(*l.dialFailTimes, time.Now())
		l.mu.Unlock()
	}
	l.Logger.Debug(msg, keysAndValues...)
}

func (l *captureLog) With(keyValues ...any) logger.Logger {
	return &captureLog{
		Logger:        l.Logger.With(keyValues...),
		dialFailTimes: l.dialFailTimes,
		mu:            l.mu,
	}
}

// TestActiveExponentialBackoff_T5 verifies that the active connection loop
// properly backs off exponentially up to the T5 ceiling.
func TestActiveExponentialBackoff_T5(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t) // Guaranteed unused port

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var fails []time.Time
	myLogger := &captureLog{
		Logger:        logger.GetLogger(),
		dialFailTimes: &fails,
		mu:            &sync.Mutex{},
	}

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithConnectRemoteTimeout(1*time.Second),
		hsmsss.WithT5Timeout(400*time.Millisecond),
		hsmsss.WithLogger(myLogger),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	hsmsConn.AddSession(testSessionID)

	require.NoError(hsmsConn.Open(false))

	// Collect retries for a sufficient duration
	time.Sleep(1600 * time.Millisecond)

	_ = hsmsConn.Close()

	myLogger.mu.Lock()
	defer myLogger.mu.Unlock()

	require.GreaterOrEqual(len(*myLogger.dialFailTimes), 4, "expected multiple repeated connections")

	times := *myLogger.dialFailTimes
	for i := 1; i < len(times); i++ {
		delta := times[i].Sub(times[i-1])
		t.Logf("Conn %d -> %d delta duration: %v", i-1, i, delta)

		require.Less(delta, 550*time.Millisecond, "delay exceeded T5Timeout cap")
	}

	// Verify the backoff capped successfully. The 4th attempt delay must be roughly 400ms
	if len(times) >= 4 {
		delta := times[3].Sub(times[2])
		require.Greater(delta, 350*time.Millisecond, "delay should have reached T5Timeout ceiling")
	}
}

// TestPassiveRecoverFromAbruptDrop verifies that a passive connection can
// recover from a client abrupt disconnection and accept a new client.
func TestPassiveRecoverFromAbruptDrop(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithPassive(),
		hsmsss.WithEquipRole(),
		hsmsss.WithT3Timeout(3*time.Second),
		hsmsss.WithT7Timeout(5*time.Second),
		hsmsss.WithAcceptConnTimeout(1*time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(newLoggerWith("role", "PASSIVE_TEST")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := hsmsConn.AddSession(testSessionID)

	stateCh := make(chan hsms.ConnState, 10)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		stateCh <- cur
	})

	require.NoError(hsmsConn.Open(false))
	defer hsmsConn.Close()

	// 1. First raw client connects and does select
	err = simulateClientSelectAndDrop(t, port)
	require.NoError(err)

	requireStateEvent(t, stateCh, hsms.NotSelectedState)
	requireStateEvent(t, stateCh, hsms.SelectedState)
	t.Log("Passive reached Selected state initially")

	requireStateEvent(t, stateCh, hsms.NotConnectedState)
	t.Log("Passive recovered to NotConnected after drop")

	// 2. Second raw client connects
	err = simulateClientSelectAndDrop(t, port)
	require.NoError(err)

	requireStateEvent(t, stateCh, hsms.NotSelectedState)
	requireStateEvent(t, stateCh, hsms.SelectedState)
	t.Log("Passive successfully recovered and accepted second client")
}

func simulateClientSelectAndDrop(t *testing.T, port int) error {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	sysBytes := [4]byte{0x01, 0x02, 0x03, 0x04}
	selectReq := buildControlHeader(0x00, hsms.SelectReqType, sysBytes)
	if err := writeFrame(conn, selectReq); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write select.req: %w", err)
	}

	rspHeader, err := readFrame(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("read select.rsp: %w", err)
	}

	if rspHeader[5] != hsms.SelectRspType || rspHeader[3] != hsms.SelectStatusSuccess {
		_ = conn.Close()
		return fmt.Errorf("expected select.rsp success, got type %d status %d", rspHeader[5], rspHeader[3])
	}

	_ = conn.Close()

	return nil
}

// TestActiveRecoverFromAbruptDrop verifies an active connection
// recovers after a successful connection is abruptly dropped.
func TestActiveRecoverFromAbruptDrop(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer ln.Close()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithT3Timeout(3*time.Second),
		hsmsss.WithT5Timeout(500*time.Millisecond),
		hsmsss.WithConnectRemoteTimeout(1*time.Second),
		hsmsss.WithAutoLinktest(false),
		hsmsss.WithLogger(newLoggerWith("role", "ACTIVE_TEST")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := hsmsConn.AddSession(testSessionID)

	stateCh := make(chan hsms.ConnState, 100)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		select {
		case stateCh <- cur:
		default:
		}
	})

	require.NoError(hsmsConn.Open(false))
	defer hsmsConn.Close()

	// 1. Raw peer accepts and handshakes
	conn := rawPeerSelectHandshake(t, ln)

	requireStateEvent(t, stateCh, hsms.SelectedState)
	t.Log("Active reached Selected state initially")

	// 2. Abruptly close the socket
	require.NoError(conn.Close())

	requireStateEvent(t, stateCh, hsms.NotConnectedState)
	t.Log("Active recovered to NotConnected after drop")

	// 3. Accept re-connection
	conn2 := rawPeerSelectHandshake(t, ln)
	defer conn2.Close()

	requireStateEvent(t, stateCh, hsms.SelectedState)
	t.Log("Active successfully recovered, re-dialed, and re-selected")
}

// TestT7Timeout_PassiveWaitSelect verifies the passive connection
// drops a client that connects but never sends Select.req within T7.
func TestT7Timeout_PassiveWaitSelect(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithPassive(),
		hsmsss.WithEquipRole(),
		hsmsss.WithT7Timeout(1*time.Second),
		hsmsss.WithAcceptConnTimeout(1*time.Second),
		hsmsss.WithLogger(newLoggerWith("role", "PASSIVE_T7_TEST")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := hsmsConn.AddSession(testSessionID)

	stateCh := make(chan hsms.ConnState, 10)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		stateCh <- cur
	})

	require.NoError(hsmsConn.Open(false))
	defer hsmsConn.Close()

	// Raw client connects but stalls
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer conn.Close()

	requireStateEvent(t, stateCh, hsms.NotSelectedState)

	start := time.Now()
	requireStateEvent(t, stateCh, hsms.NotConnectedState)
	elapsed := time.Since(start)

	require.GreaterOrEqual(elapsed, 950*time.Millisecond, "should wait exact T7 time before dropping")
	require.Less(elapsed, 2500*time.Millisecond, "should drop roughly around T7 time")

	t.Logf("Passive connection enforcing T7 timeout correctly: %v", elapsed)
}

// TestT6Timeout_ActiveControlReply verifies the active connection
// drops when the remote never replies to Select.req within T6.
func TestT6Timeout_ActiveControlReply(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer ln.Close()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithConnectRemoteTimeout(1*time.Second),
		hsmsss.WithT6Timeout(1*time.Second),
		hsmsss.WithT5Timeout(2*time.Second),
		hsmsss.WithLogger(newLoggerWith("role", "ACTIVE_T6_TEST")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)

	session := hsmsConn.AddSession(testSessionID)

	stateCh := make(chan hsms.ConnState, 10)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		stateCh <- cur
	})

	require.NoError(hsmsConn.Open(false))
	defer hsmsConn.Close()

	// Raw listener accepts
	conn, err := ln.Accept()
	require.NoError(err)
	defer conn.Close()

	// Read Select.req but never reply
	req, err := readFrame(conn)
	require.NoError(err)
	require.Equal(byte(hsms.SelectReqType), req[5])

	start := time.Now()
	requireStateEvent(t, stateCh, hsms.NotConnectedState)
	elapsed := time.Since(start)

	require.GreaterOrEqual(elapsed, 950*time.Millisecond, "should wait exact T6 time before dropping")
	require.Less(elapsed, 2500*time.Millisecond, "should drop roughly around T6 time")

	t.Logf("Active connection enforcing T6 timeout correctly: %v", elapsed)
}

// TestConcurrentClose ensures that calling Close() from multiple goroutines
// does not cause a panic or a data race.
func TestConcurrentClose(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx := t.Context()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithLogger(newLoggerWith("test", "ConcurrentClose")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)
	hsmsConn.AddSession(testSessionID)

	go func() {
		_ = hsmsConn.Open(false)
	}()

	time.Sleep(50 * time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := hsmsConn.Close()
			require.NoError(err)
		}()
	}

	wg.Wait()
}

// TestPartialRead_T8Timeout verifies that a peer sending a partial payload and stalling
// is properly handled and triggers a T8 timeout, closing the connection.
func TestPartialRead_T8Timeout(t *testing.T) {
	require := require.New(t)
	port := getFreePort(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(err)
	defer ln.Close()

	cfg, err := hsmsss.NewConnectionConfig("127.0.0.1", port,
		hsmsss.WithActive(),
		hsmsss.WithHostRole(),
		hsmsss.WithT8Timeout(1*time.Second),
		hsmsss.WithLogger(newLoggerWith("test", "PartialRead")),
	)
	require.NoError(err)

	hsmsConn, err := hsmsss.NewConnection(ctx, cfg)
	require.NoError(err)
	hsmsConn.AddSession(testSessionID)

	require.NoError(hsmsConn.Open(false))

	// Raw peer accepts and completes select handshake
	conn := rawPeerSelectHandshake(t, ln)
	defer conn.Close()

	// Send a malicious partial payload:
	// length header promises 14 bytes (10 header + 4 payload), but we only send 11
	sysBytes := getSystemBytes([]byte{0, 0, 0, 0, 0, 0, 0x01, 0x02, 0x03, 0x04})
	lengthHeader := []byte{0, 0, 0, 14}
	header := buildControlHeader(0x00, 0x00, sysBytes)
	partialPayload := []byte{0x00} // Missing 3 bytes!

	_, err = conn.Write(lengthHeader)
	require.NoError(err)
	_, err = conn.Write(header)
	require.NoError(err)
	_, err = conn.Write(partialPayload)
	require.NoError(err)

	start := time.Now()

	// Read will return an error when the active side drops the connection on T8 timeout
	buf := make([]byte, 100)
	_, err = conn.Read(buf)
	require.Error(err, "expected read to fail as active side drops connection on T8 timeout")

	elapsed := time.Since(start)

	require.GreaterOrEqual(elapsed, 950*time.Millisecond, "should wait at least T8 time before aborting")
	require.Less(elapsed, 2500*time.Millisecond, "should abort shortly after T8 time")
}
