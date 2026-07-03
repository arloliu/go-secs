package hsmsss

// chaos_proxy_test.go — filter-driven MITM TCP proxy for HSMS-SS fault injection.
//
// ChaosProxy intercepts the raw TCP stream between an active and passive
// HSMS-SS connection.  Each HSMS frame (4-byte length prefix + payload) is
// presented to a caller-supplied ProxyFilter before being forwarded, dropped,
// truncated, or responded to with a TCP close.  The proxy machinery is
// entirely v2-agnostic; only the connection-facing scaffolding in the smoke
// test below is wired to the v2 API.
//
// This file is package hsmsss (not hsmsss_test) so it can reuse freeLoopbackPort
// from passive_test.go.
//
// Full scenario matrix (dropped-Select→T6, truncated→T8, CloseTCP→reconnect,
// byte-fault→Reject) is T29; only the T3-timeout smoke test lives here.

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

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ProxyAction selects what ChaosProxy does with each intercepted HSMS frame.
type ProxyAction int

const (
	// ProxyActionForward forwards the frame unmodified to the destination.
	ProxyActionForward ProxyAction = iota
	// ProxyActionDrop silently discards the frame.
	ProxyActionDrop
	// ProxyActionCloseTCP closes both sides of the proxy connection immediately.
	ProxyActionCloseTCP
	// ProxyActionTruncate writes the length header then only the first 5 bytes
	// of payload, leaving the destination's ReadFull hanging (drives T8).
	ProxyActionTruncate
)

// ProxyFilter is the per-frame decision function.  isClientToTarget is true
// when the frame travels from the active (dialling) side to the passive
// (listening) side; false for the reverse.  header is the first 10 bytes of
// payload (the HSMS message header).
type ProxyFilter func(isClientToTarget bool, header []byte, payload []byte) (action ProxyAction, delay time.Duration)

// ChaosProxy is a filter-driven MITM TCP proxy that intercepts raw HSMS frames.
type ChaosProxy struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	filter ProxyFilter

	clientConn net.Conn
	targetConn net.Conn

	wg     sync.WaitGroup
	closed atomic.Bool
}

// newChaosProxy creates a ChaosProxy that proxies to the given targetPort on
// 127.0.0.1.  The proxy binds its own listener on an OS-assigned port.
func newChaosProxy(t *testing.T, targetPort int) *ChaosProxy {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	return &ChaosProxy{
		listener: l,
		target:   fmt.Sprintf("127.0.0.1:%d", targetPort),
	}
}

// Port returns the OS-assigned port that the proxy is listening on.
func (cp *ChaosProxy) Port() int {
	addr, ok := cp.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}

	return addr.Port
}

// SetFilter replaces the current frame filter.  nil means forward everything.
func (cp *ChaosProxy) SetFilter(f ProxyFilter) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.filter = f
}

// Start begins accepting connections in a background goroutine.  For each
// accepted connection it dials the target and spawns a pair of pump goroutines
// (one per direction).
func (cp *ChaosProxy) Start(t *testing.T) {
	t.Helper()
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
				_ = clientConn.Close()

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

// Stop closes the listener and all active proxy connections, then waits for
// all goroutines to exit.  Idempotent.
func (cp *ChaosProxy) Stop() {
	if cp.closed.CompareAndSwap(false, true) {
		_ = cp.listener.Close()
		cp.CloseConnections()
		cp.wg.Wait()
	}
}

// CloseConnections closes the current active client and target connections.
func (cp *ChaosProxy) CloseConnections() {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	if cp.clientConn != nil {
		_ = cp.clientConn.Close()
		cp.clientConn = nil
	}

	if cp.targetConn != nil {
		_ = cp.targetConn.Close()
		cp.targetConn = nil
	}
}

// pump relays HSMS frames from src to dst, applying the active ProxyFilter to
// each frame before forwarding.
func (cp *ChaosProxy) pump(src, dst net.Conn, isClientToTarget bool) {
	defer cp.wg.Done()

	for {
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(src, lenBuf); err != nil {
			_ = src.Close()
			_ = dst.Close()

			return
		}

		length := binary.BigEndian.Uint32(lenBuf)
		payload := make([]byte, length)

		if _, err := io.ReadFull(src, payload); err != nil {
			_ = src.Close()
			_ = dst.Close()

			return
		}

		cp.mu.Lock()
		f := cp.filter
		cp.mu.Unlock()

		action := ProxyActionForward

		var delay time.Duration

		if f != nil {
			// header is the first 10 bytes of payload (the HSMS message header).
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
			_ = src.Close()
			_ = dst.Close()

			return
		case ProxyActionDrop:
			continue
		case ProxyActionTruncate:
			_, _ = dst.Write(lenBuf)

			if len(payload) > 5 {
				_, _ = dst.Write(payload[:5]) // write incomplete payload
			}

			continue
		case ProxyActionForward:
			_, _ = dst.Write(lenBuf)
			_, _ = dst.Write(payload)
		default:
			panic(fmt.Sprintf("chaos_proxy: unknown ProxyAction %d", action))
		}
	}
}

// ── Smoke test ────────────────────────────────────────────────────────────────

// TestChaos_DroppedDataReply_T3Timeout verifies end-to-end T3 timeout delivery
// through the proxy:
//
//  1. A passive connection listens on portP with a data handler that echoes a
//     reply via ReplyDataMessage.
//  2. A ChaosProxy sits between the active and passive; its filter forwards all
//     HSMS control frames (Select handshake) but DROPs any secondary data
//     message travelling from passive→active (SType=0, even function).
//  3. The active sends a W-bit S1F1 primary.  The passive replies S1F2, but the
//     proxy drops it.  After T3 (1 s) the active's SendDataMessage must return
//     hsms.ErrT3Timeout.
func TestChaos_DroppedDataReply_T3Timeout(t *testing.T) {
	t.Parallel()

	portP := freeLoopbackPort(t)
	ctx := t.Context()

	// ── passive ──────────────────────────────────────────────────────────────
	passiveCfg, err := NewConfig("127.0.0.1", portP,
		WithPassive(),
		WithConnectionOption(hsms.WithT3(1*time.Second)),
	)
	require.NoError(t, err)

	passive, err := New(passiveCfg)
	require.NoError(t, err)

	// The passive handler replies S1F2 to any incoming primary.  The reply is
	// sent into the proxy, which will drop it before it reaches the active.
	passive.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
	})

	require.NoError(t, passive.Open(ctx, hsms.OpenBackground))
	t.Cleanup(func() { _ = passive.Close() })

	// ── proxy ─────────────────────────────────────────────────────────────────
	proxy := newChaosProxy(t, portP)

	// Forward all control messages (SType != 0) so the Select handshake
	// completes.  Drop only data secondaries (SType=0, even function = reply)
	// travelling from passive→active.
	proxy.SetFilter(func(isClientToTarget bool, header []byte, payload []byte) (ProxyAction, time.Duration) {
		if !isClientToTarget && len(header) >= 10 && header[5] == 0 && header[3]%2 == 0 {
			return ProxyActionDrop, 0
		}

		return ProxyActionForward, 0
	})

	proxy.Start(t)
	t.Cleanup(proxy.Stop)

	// ── active ────────────────────────────────────────────────────────────────
	activeCfg, err := NewConfig("127.0.0.1", proxy.Port(),
		WithConnectionOption(hsms.WithT3(1*time.Second)),
	)
	require.NoError(t, err)

	active, err := New(activeCfg)
	require.NoError(t, err)

	require.NoError(t, active.Open(ctx, hsms.OpenBackground))
	t.Cleanup(func() { _ = active.Close() })

	// Both endpoints must reach Selected before we send data (the Select
	// handshake frames are forwarded unfiltered by the proxy).
	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState && passive.State() == hsms.SelectedState
	}, 5*time.Second, 10*time.Millisecond, "active and passive must both reach SelectedState through the proxy")

	// ── smoke assertion ───────────────────────────────────────────────────────
	// W-bit send: the passive will reply, but the proxy drops the reply.
	// SendDataMessage must return ErrT3Timeout after T3 (1 s) expires.
	_, sendErr := active.SendDataMessage(ctx, 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, sendErr, hsms.ErrT3Timeout,
		"dropped data reply must cause SendDataMessage to return ErrT3Timeout")
}
