package secs1integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs1"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

type endpoint struct {
	conn       *secs1.Connection
	session    hsms.Session
	selectedCh chan struct{}
}

func newEndpoint(
	t *testing.T,
	ctx context.Context,
	port int,
	isEquip bool,
	isActive bool,
	handlers ...hsms.DataMessageHandler,
) *endpoint {
	t.Helper()

	opts := []secs1.ConnOption{
		secs1.WithDeviceID(10),
		secs1.WithT1Timeout(secs1.MinT1Timeout),
		secs1.WithT2Timeout(secs1.MinT2Timeout),
		secs1.WithT3Timeout(secs1.MinT3Timeout),
		secs1.WithT4Timeout(secs1.MinT4Timeout),
		secs1.WithRetryLimit(2),
		secs1.WithConnectTimeout(300 * time.Millisecond),
		secs1.WithSendTimeout(500 * time.Millisecond),
	}

	if isEquip {
		opts = append(opts, secs1.WithEquipRole())
	} else {
		opts = append(opts, secs1.WithHostRole())
	}

	if isActive {
		opts = append(opts, secs1.WithActive())
	} else {
		opts = append(opts, secs1.WithPassive())
	}

	cfg, err := secs1.NewConnectionConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := secs1.NewConnection(ctx, cfg)
	require.NoError(t, err)

	session := conn.AddSession(10)
	selectedCh := make(chan struct{}, 16)
	session.AddConnStateChangeHandler(func(_ hsms.Connection, _ hsms.ConnState, cur hsms.ConnState) {
		if cur.IsSelected() {
			select {
			case selectedCh <- struct{}{}:
			default:
			}
		}
	})

	if len(handlers) > 0 {
		session.AddDataMessageHandler(handlers...)
	}

	return &endpoint{conn: conn, session: session, selectedCh: selectedCh}
}

func waitSelected(t *testing.T, ep *endpoint, timeout time.Duration) {
	t.Helper()
	select {
	case <-ep.selectedCh:
	case <-time.After(timeout):
		t.Fatal("timeout waiting for selected state")
	}
}

func closeEndpoint(t *testing.T, ep *endpoint) {
	t.Helper()
	if ep == nil || ep.conn == nil {
		return
	}
	require.NoError(t, ep.conn.Close())
}

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()

	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return addr.Port
}

func runEchoReply(msg *hsms.DataMessage, s hsms.Session) {
	if msg.FunctionCode()%2 == 1 {
		_ = s.ReplyDataMessage(msg, msg.Item())
	}
}

func TestSECS1_Integration_StabilityBidirectional(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port := getFreePort(t)

	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(t, eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(t, host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	const rounds = 40
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range rounds {
			reply, err := host.session.SendDataMessage(1, 1, true, secs2.A(fmt.Sprintf("h-%d", i)))
			if err != nil {
				errCh <- fmt.Errorf("host send failed: %w", err)
				return
			}
			if reply == nil {
				errCh <- fmt.Errorf("host reply is nil")
				return
			}
			reply.Free()
		}
	}()

	go func() {
		defer wg.Done()
		for i := range rounds {
			reply, err := eqp.session.SendDataMessage(1, 3, true, secs2.A(fmt.Sprintf("e-%d", i)))
			if err != nil {
				errCh <- fmt.Errorf("eqp send failed: %w", err)
				return
			}
			if reply == nil {
				errCh <- fmt.Errorf("eqp reply is nil")
				return
			}
			reply.Free()
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("stability test timed out")
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	default:
	}
}

func TestSECS1_Integration_ActiveRetryUntilPassiveAvailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	port := getFreePort(t)

	// Start active first: it should enter retry loop while passive is unavailable.
	host := newEndpoint(t, ctx, port, false, true, runEchoReply)
	require.NoError(t, host.conn.Open(false))
	defer closeEndpoint(t, host)

	// Bring up passive later and verify active side eventually connects.
	time.Sleep(500 * time.Millisecond)
	eqp := newEndpoint(t, ctx, port, true, false, runEchoReply)
	require.NoError(t, eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	waitSelected(t, host, 10*time.Second)
	waitSelected(t, eqp, 10*time.Second)

	reply, err := host.session.SendDataMessage(1, 1, true, secs2.A("after passive available"))
	require.NoError(t, err)
	require.NotNil(t, reply)
	reply.Free()
}

func TestSECS1_Integration_RetryOnMissingEOTThenRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	port := getFreePort(t)

	errPeer := make(chan error, 1)
	go func() {
		errPeer <- runRetryPeer("127.0.0.1", port)
	}()

	host := newEndpoint(t, ctx, port, false, true)
	require.NoError(t, host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)

	reply, err := host.session.SendDataMessage(1, 1, true, nil)
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, byte(2), reply.FunctionCode())
	reply.Free()

	require.Equal(t, uint64(2), host.conn.GetMetrics().BlockRetryCount.Load())
	require.NoError(t, <-errPeer)
}

func TestSECS1_Integration_ContentionSimultaneousSend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port := getFreePort(t)

	var hostPrimRecv atomic.Int32
	var eqpPrimRecv atomic.Int32

	hostHandler := func(msg *hsms.DataMessage, s hsms.Session) {
		if msg.FunctionCode()%2 == 1 {
			hostPrimRecv.Add(1)
			_ = s.ReplyDataMessage(msg, secs2.A("host-ok"))
		}
	}
	eqpHandler := func(msg *hsms.DataMessage, s hsms.Session) {
		if msg.FunctionCode()%2 == 1 {
			eqpPrimRecv.Add(1)
			_ = s.ReplyDataMessage(msg, secs2.A("eqp-ok"))
		}
	}

	eqp := newEndpoint(t, ctx, port, true, false, eqpHandler)
	require.NoError(t, eqp.conn.Open(false))
	defer closeEndpoint(t, eqp)

	host := newEndpoint(t, ctx, port, false, true, hostHandler)
	require.NoError(t, host.conn.Open(true))
	defer closeEndpoint(t, host)

	waitSelected(t, host, 5*time.Second)
	waitSelected(t, eqp, 5*time.Second)

	const rounds = 25
	for i := range rounds {
		start := make(chan struct{})
		resultCh := make(chan error, 2)

		go func(n int) {
			<-start
			reply, err := host.session.SendDataMessage(2, 1, true, secs2.A(fmt.Sprintf("host-%d", n)))
			if err != nil {
				resultCh <- fmt.Errorf("host send: %w", err)
				return
			}
			if reply == nil {
				resultCh <- fmt.Errorf("host reply is nil")
				return
			}
			reply.Free()
			resultCh <- nil
		}(i)

		go func(n int) {
			<-start
			reply, err := eqp.session.SendDataMessage(2, 3, true, secs2.A(fmt.Sprintf("eqp-%d", n)))
			if err != nil {
				resultCh <- fmt.Errorf("eqp send: %w", err)
				return
			}
			if reply == nil {
				resultCh <- fmt.Errorf("eqp reply is nil")
				return
			}
			reply.Free()
			resultCh <- nil
		}(i)

		close(start)

		deadline := time.After(3 * time.Second)
		for range 2 {
			select {
			case err := <-resultCh:
				require.NoError(t, err)
			case <-deadline:
				t.Fatalf("round %d timed out", i)
			}
		}
	}

	require.GreaterOrEqual(t, hostPrimRecv.Load(), int32(rounds))
	require.GreaterOrEqual(t, eqpPrimRecv.Load(), int32(rounds))
}

func runRetryPeer(host string, port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer ln.Close()

	conn, err := ln.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	for i := 0; i < 3; i++ {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if b != secs1.ENQ {
			return fmt.Errorf("expected ENQ, got 0x%02X", b)
		}

		if i < 2 {
			continue
		}

		if _, err := conn.Write([]byte{secs1.EOT}); err != nil {
			return err
		}
		reqBlk, err := readBlock(reader, conn)
		if err != nil {
			return err
		}
		if _, err := conn.Write([]byte{secs1.ACK}); err != nil {
			return err
		}

		if _, err := conn.Write([]byte{secs1.ENQ}); err != nil {
			return err
		}
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		eot, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if eot != secs1.EOT {
			return fmt.Errorf("expected EOT, got 0x%02X", eot)
		}

		reply := buildSecondaryReply(reqBlk)
		if _, err := conn.Write(reply.Pack()); err != nil {
			return err
		}
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		ack, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if ack != secs1.ACK {
			return fmt.Errorf("expected ACK, got 0x%02X", ack)
		}
	}

	return nil
}

func readBlock(reader *bufio.Reader, conn net.Conn) (*secs1.Block, error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	lengthByte, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	remaining := int(lengthByte) + 2
	buf := make([]byte, remaining)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil, err
	}

	return secs1.ParseBlock(lengthByte, buf)
}

func buildSecondaryReply(req *secs1.Block) *secs1.Block {
	reply := &secs1.Block{}
	reply.SetDeviceID(req.DeviceID())
	reply.SetRBit(true)
	reply.SetWBit(false)
	reply.SetStreamCode(req.StreamCode())
	reply.SetFunctionCode(req.FunctionCode() + 1)
	reply.SetBlockNumber(0)
	reply.SetEBit(true)
	copy(reply.Header[6:10], req.Header[6:10])

	return reply
}
