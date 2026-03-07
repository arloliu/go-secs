package hsmsssintegration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
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

type ProxyAction int

const (
	ProxyActionForward ProxyAction = iota
	ProxyActionDrop
	ProxyActionCloseTCP
	ProxyActionTruncate
)

type ProxyFilter func(isClientToTarget bool, header []byte, payload []byte) (action ProxyAction, delay time.Duration)

type ChaosProxy struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	filter ProxyFilter

	clientConn net.Conn
	targetConn net.Conn
	wg         sync.WaitGroup
	closed     atomic.Bool
}

func newChaosProxy(t *testing.T, targetPort int) *ChaosProxy {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	return &ChaosProxy{
		listener: l,
		target:   fmt.Sprintf("127.0.0.1:%d", targetPort),
	}
}

func (cp *ChaosProxy) Port() int {
	addr, ok := cp.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

func (cp *ChaosProxy) SetFilter(f ProxyFilter) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.filter = f
}

func (cp *ChaosProxy) Start(t *testing.T) {
	cp.wg.Add(1)
	go func() {
		defer cp.wg.Done()
		for {
			clientConn, err := cp.listener.Accept()
			if err != nil {
				return
			}

			cp.mu.Lock()
			cp.clientConn = clientConn
			cp.mu.Unlock()

			targetConn, err := net.Dial("tcp", cp.target)
			if err != nil {
				clientConn.Close()
				continue
			}

			cp.mu.Lock()
			cp.targetConn = targetConn
			cp.mu.Unlock()

			cp.wg.Add(2)
			go cp.pump(clientConn, targetConn, true)
			go cp.pump(targetConn, clientConn, false)
		}
	}()
}

func (cp *ChaosProxy) Stop() {
	if cp.closed.CompareAndSwap(false, true) {
		cp.listener.Close()
		cp.CloseConnections()
		cp.wg.Wait()
	}
}

func (cp *ChaosProxy) CloseConnections() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.clientConn != nil {
		cp.clientConn.Close()
		cp.clientConn = nil
	}
	if cp.targetConn != nil {
		cp.targetConn.Close()
		cp.targetConn = nil
	}
}

func (cp *ChaosProxy) pump(src, dst net.Conn, isClientToTarget bool) {
	defer cp.wg.Done()

	for {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(src, lenBuf); err != nil {
			src.Close()
			dst.Close()
			return
		}

		length := binary.BigEndian.Uint32(lenBuf)
		payload := make([]byte, length)
		if _, err := io.ReadFull(src, payload); err != nil {
			src.Close()
			dst.Close()
			return
		}

		cp.mu.Lock()
		f := cp.filter
		cp.mu.Unlock()

		action := ProxyActionForward
		var delay time.Duration
		if f != nil {
			// header is the first 10 bytes of payload
			header := payload
			if len(payload) >= 10 {
				header = payload[:10]
			}
			action, delay = f(isClientToTarget, header, payload)
		}

		if delay > 0 {
			time.Sleep(delay)
		}

		switch action {
		case ProxyActionCloseTCP:
			src.Close()
			dst.Close()
			return
		case ProxyActionDrop:
			continue
		case ProxyActionTruncate:
			_, _ = dst.Write(lenBuf)
			if len(payload) > 5 {
				_, _ = dst.Write(payload[:5]) // Write incomplete payload
			}
			continue
		case ProxyActionForward:
			_, _ = dst.Write(lenBuf)
			_, _ = dst.Write(payload)
		default:
			panic(fmt.Sprintf("unknown action: %v", action))
		}
	}
}

func TestChaos_DroppedSelectRsp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	// Drop Select.rsp from equip to host
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.SelectRspType) {
			return ProxyActionDrop, 0
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	// T6 timeout is for receiving a control message reply (Select.rsp)
	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT6Timeout(1 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	host.session.AddConnStateChangeHandler(func(_ hsms.Connection, prev hsms.ConnState, cur hsms.ConnState) {
		t.Logf("State transition: %s -> %s", prev.String(), cur.String())
	})

	// Since Select.rsp is dropped, Host will fail to receive it, triggering T6 timeout
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

func TestChaos_TruncatedDataMessage_T8Timeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	var truncated atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if isClientToTarget && len(header) >= 10 && header[4] == 0 && header[5] == 0 {
			if truncated.CompareAndSwap(false, true) {
				return ProxyActionTruncate, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	item := secs2.NewASCIIItem("0123456789")
	err := host.session.SendDataMessageAsync(1, 1, false, item)
	require.NoError(t, err)

	requireStateEvent(t, equip.selectedCh, hsms.NotConnectedState)
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

func TestChaos_DelayedT3Reply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	var delayed atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[4] == 0 && header[5] == 0 {
			if delayed.CompareAndSwap(false, true) {
				return ProxyActionForward, 2 * time.Second
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(1 * time.Second),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	item := secs2.NewASCIIItem("1")
	reply, err := host.session.SendDataMessage(1, 1, true, item)

	// T3 expires in 1s. SendAndWait returns timeout error.
	require.Error(t, err)
	require.Contains(t, err.Error(), "T3 timeout")
	require.Nil(t, reply)

	// Wait to let the delayed reply arrive
	time.Sleep(1500 * time.Millisecond)

	// Connection should still be Selected
	item2 := secs2.NewASCIIItem("2")
	reply2, err := host.session.SendDataMessage(1, 3, true, item2)
	require.NoError(t, err)
	require.NotNil(t, reply2)
}

func TestChaos_DroppedLinktestRsp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == byte(hsms.LinkTestRspType) {
			return ProxyActionDrop, 0
		}
		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT6Timeout(1 * time.Second),
		hsmsss.WithAutoLinktest(true),
		hsmsss.WithLinktestInterval(500 * time.Millisecond),
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	// Let the auto linktest drop the connection due to dropped response
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

func TestChaos_AbruptClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newChaosProxy(t, equipPort)
	var closed atomic.Bool
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if isClientToTarget && len(header) >= 10 && header[4] == 0 && header[5] == 0 {
			if closed.CompareAndSwap(false, true) {
				return ProxyActionCloseTCP, 0
			}
		}

		return ProxyActionForward, 0
	})
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithLogger(logger.GetLogger()),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)

	item := secs2.NewASCIIItem("1")
	_ = host.session.SendDataMessageAsync(1, 1, false, item)

	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
	requireStateEvent(t, equip.selectedCh, hsms.NotConnectedState)
}
