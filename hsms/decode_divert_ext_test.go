package hsms_test

// External test package (hsms_test): hsmstest imports hsms, so a white-box package hsms test
// cannot import hsmstest without a cycle. From here only the exported surface is reachable.
//
// RouteData / DeliverOwnedFrame live on the hsms.TransportRuntime interface, NOT on the
// app-facing hsms.Connection interface, and the hsmsss consumer wrapper embeds hsms.Connection
// (not the concrete engine), so those inbound entries are not promoted onto the returned value
// and cannot be called directly from here. The only externally reachable inbound path is the
// real recv loop: a peer forwards the malformed frame verbatim over the wire, the receiver's
// recv loop decodes the framing (valid) and funnels the lazy message through DeliverOwnedFrame
// -> RouteData. Delivery therefore happens on the receiver's recv goroutine, so handler
// observation is synchronized via buffered channels rather than plain bools.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
	"github.com/arloliu/go-secs/v2/hsmsss"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// link is a live, Selected HSMS-SS loopback pair. recv is the connection under test (handlers are
// registered here, and its recv loop drives RouteData); send forwards frames to it verbatim.
type link struct {
	recv hsms.Connection
	send hsms.Connection
}

// freeDivertPort reserves and immediately releases a loopback TCP port for the pair to share.
func freeDivertPort(t *testing.T) int {
	t.Helper()

	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener addr must be *net.TCPAddr")
	require.NoError(t, ln.Close())

	return addr.Port
}

// newDivertEndpoint builds one side of an HSMS-SS loopback pair with short integration timers and
// auto-linktest disabled.
func newDivertEndpoint(t *testing.T, port int, active bool) hsms.Connection {
	t.Helper()

	opts := []hsmsss.Option{
		hsmsss.WithConnectionOption(hsms.WithT3(3 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT5(200 * time.Millisecond)),
		hsmsss.WithConnectionOption(hsms.WithT6(3 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT7(5 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithT8(1 * time.Second)),
		hsmsss.WithConnectionOption(hsms.WithLinktestInterval(0)),
	}
	if active {
		opts = append(opts, hsmsss.WithActive())
	} else {
		opts = append(opts, hsmsss.WithPassive())
	}

	cfg, err := hsmsss.NewConfig("127.0.0.1", port, opts...)
	require.NoError(t, err)

	conn, err := hsmsss.New(cfg)
	require.NoError(t, err)

	return conn
}

// buildConn stands up a real active+passive loopback pair and drives both to Selected. The active
// side is the receiver under test (its recv loop is the inbound RouteData path); the passive side
// is the sender.
func buildConn(t *testing.T) link {
	t.Helper()

	port := freeDivertPort(t)
	passive := newDivertEndpoint(t, port, false)
	active := newDivertEndpoint(t, port, true)

	ctx := context.Background()
	require.NoError(t, passive.Open(ctx, hsms.OpenBackground)) // passive must listen before active dials
	require.NoError(t, active.Open(ctx, hsms.OpenBackground))

	waitSelected := func(c hsms.Connection, name string) {
		require.Eventuallyf(t, func() bool {
			return c.State() == hsms.SelectedState
		}, 10*time.Second, 5*time.Millisecond, "%s did not reach Selected", name)
	}
	waitSelected(active, "active")
	waitSelected(passive, "passive")

	t.Cleanup(func() {
		_ = active.Close()
		_ = passive.Close()
	})

	return link{recv: active, send: passive}
}

// routeInbound forwards msg verbatim from the peer so it arrives on recv's inbound RouteData path
// over the wire (buildFrameBuffers sends the cached body bytes, so the malformation is preserved).
func routeInbound(t *testing.T, l link, msg *hsms.DataMessage) {
	t.Helper()
	require.NoError(t, l.send.ForwardDataMessage(context.Background(), msg))
}

func TestRouteData_divertsUndecodableWhenHandlerRegistered(t *testing.T) {
	l := buildConn(t)

	normalCh := make(chan struct{}, 1)
	decodeErrCh := make(chan struct{}, 1)
	l.recv.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		select {
		case normalCh <- struct{}{}:
		default:
		}
	})
	l.recv.AddDecodeErrorHandler(func(_ *hsms.DataMessage, _ error, _ hsms.SECS2Endpoint) {
		select {
		case decodeErrCh <- struct{}{}:
		default:
		}
	})

	bad := hsmstest.MalformedDataMessage(6, 11, false)
	routeInbound(t, l, bad)

	// The decode-error handler must fire. RouteData increments BodyDecodeErrCount BEFORE dispatching
	// (same goroutine), so once we observe this signal the counter is already settled.
	select {
	case <-decodeErrCh:
	case <-time.After(3 * time.Second):
		t.Fatal("decode-error handler was NOT called for an undecodable message")
	}

	if got := l.recv.Metrics().BodyDecodeErrCount(); got != 1 {
		t.Errorf("BodyDecodeErrCount() = %d, want 1", got)
	}
	// The body-decode failure is diverted AFTER the frame was received and counted, so it must
	// not also land in DecodeErrCount (frame-level failures) — the two counters are disjoint.
	if got := l.recv.Metrics().DecodeErrCount(); got != 0 {
		t.Errorf("DecodeErrCount() = %d, want 0 (body-decode failure must not count as a frame-decode failure)", got)
	}

	// The normal handler must NOT also see the message: a single RouteData call diverts XOR fans out.
	// Because the divert returns from RouteData BEFORE recvDataMsg runs, this also proves the message
	// never reaches recvDataMsg's channel-handler fan-out (the only other delivery path).
	select {
	case <-normalCh:
		t.Error("normal handler was called for an undecodable message")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRouteData_routesNormallyWhenHandlerRegisteredAndBodyDecodes covers the fall-through: even
// with a decode-error handler registered, a WELL-FORMED primary forces the eager body decode in
// RouteData, sees it succeed, and routes to the normal handler — the decode-error handler is not
// invoked and BodyDecodeErrCount stays 0.
func TestRouteData_routesNormallyWhenHandlerRegisteredAndBodyDecodes(t *testing.T) {
	l := buildConn(t)

	normalCh := make(chan struct{}, 1)
	decodeErrCh := make(chan struct{}, 1)
	l.recv.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		select {
		case normalCh <- struct{}{}:
		default:
		}
	})
	l.recv.AddDecodeErrorHandler(func(_ *hsms.DataMessage, _ error, _ hsms.SECS2Endpoint) {
		select {
		case decodeErrCh <- struct{}{}:
		default:
		}
	})

	good, err := hsms.NewDataMessage(6, 11, false, 0, [4]byte{0, 0, 0, 1}, secs2.NewBinaryItem([]byte{0x00}))
	require.NoError(t, err)
	routeInbound(t, l, good)

	// The well-formed message must reach the normal handler.
	select {
	case <-normalCh:
	case <-time.After(3 * time.Second):
		t.Fatal("well-formed message did not reach the normal handler")
	}

	// The decode-error handler must NOT fire for a message whose body decodes cleanly.
	select {
	case <-decodeErrCh:
		t.Error("decode-error handler was called for a well-formed message")
	case <-time.After(200 * time.Millisecond):
	}

	if got := l.recv.Metrics().BodyDecodeErrCount(); got != 0 {
		t.Errorf("BodyDecodeErrCount() = %d, want 0", got)
	}
}

func TestRouteData_normalRoutingWhenNoDecodeHandler(t *testing.T) {
	l := buildConn(t)

	normalCh := make(chan struct{}, 1)
	l.recv.AddDataMessageHandler(func(_ *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		select {
		case normalCh <- struct{}{}:
		default:
		}
	})

	bad := hsmstest.MalformedDataMessage(6, 11, false)
	routeInbound(t, l, bad)

	select {
	case <-normalCh:
	case <-time.After(3 * time.Second):
		t.Fatal("with no decode-error handler, the message must route normally (lazy)")
	}

	if got := l.recv.Metrics().BodyDecodeErrCount(); got != 0 {
		t.Errorf("BodyDecodeErrCount() = %d, want 0", got)
	}
}
