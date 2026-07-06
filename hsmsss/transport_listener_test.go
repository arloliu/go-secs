package hsmsss

// transport_listener_test.go — coverage for the WithListener passive-listen seam: a custom
// ListenFunc is honored, nil is rejected, the default listener is installed and works, and a real
// passive connection accepts + selects over an in-memory pipe-backed fake net.Listener.
//
// The file is in package hsmsss (not hsmsss_test) so it can read the unexported cfg.listen field,
// construct the unexported *transport, and reuse the frame helpers from the sibling test files.
// Mirrors transport_dialer_test.go's four-test shape for the active-dial seam.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestWithListener_CustomListenerHonored verifies WithListener installs the supplied ListenFunc
// and the config invokes it (rather than the default net.ListenConfig) when the transport listens.
func TestWithListener_CustomListenerHonored(t *testing.T) {
	t.Parallel()

	var called bool
	fakeLn := newPipeListener()
	defer func() { _ = fakeLn.Close() }()

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(), WithListener(
		func(_ context.Context, _, _ string) (net.Listener, error) {
			called = true
			return fakeLn, nil
		}))
	require.NoError(t, err)
	require.NotNil(t, cfg.listen, "listen not set")

	// Invoke the stored ListenFunc the way the transport will.
	ln, err := cfg.listen(context.Background(), "tcp", "127.0.0.1:5000")
	require.NoError(t, err)
	require.True(t, called, "custom ListenFunc not invoked")
	require.Same(t, fakeLn, ln)
}

// TestWithListener_NilRejected verifies that a nil ListenFunc is a configuration error.
func TestWithListener_NilRejected(t *testing.T) {
	t.Parallel()

	_, err := NewConfig("127.0.0.1", 5000, WithListener(nil))
	require.Error(t, err, "expected error for nil listen func")
}

// TestWithListener_DefaultListenerInstalled verifies NewConfig installs a non-nil default
// ListenFunc that listens on a real TCP endpoint (so the production path is unchanged when
// WithListener is not used).
func TestWithListener_DefaultListenerInstalled(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	require.NotNil(t, cfg.listen, "NewConfig must install a default ListenFunc")

	ln, err := cfg.listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "the default ListenFunc must bind a real TCP listener")
	defer func() { _ = ln.Close() }()

	require.IsType(t, &net.TCPListener{}, ln, "the default ListenFunc yields a *net.TCPListener")
}

// TestWithListener_PipeAcceptance drives a REAL passive HSMS-SS connection over an in-memory fake
// net.Listener injected via WithListener: the transport's Accept call is satisfied by one end of a
// fresh net.Pipe, a scripted peer on the other end sends Select.req (the passive side never
// initiates Select — it only responds), the connection reaches Selected, and Close tears it down
// cleanly. Mirrors TestWithDialer_PipeAcceptance for the active side.
func TestWithListener_PipeAcceptance(t *testing.T) {
	t.Parallel()

	fakeLn := newPipeListener()
	defer func() { _ = fakeLn.Close() }()

	listen := func(_ context.Context, _, _ string) (net.Listener, error) {
		return fakeLn, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(),
		WithListener(listen),
		WithConnectionOption(hsms.WithT6(2*time.Second)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Drive the scripted peer side: dial (via the fake listener's pipe generation) and send a
	// Select.req, expecting the passive side's Select.rsp.
	peer := fakeLn.dialFresh()
	go func() {
		_, werr := peer.Write(selectReqFrame([4]byte{0xAA, 0xBB, 0xCC, 0xDD}))
		if werr != nil {
			return
		}

		// Drain (and ignore) anything further, including the courtesy Separate on Close.
		for {
			if _, rerr := peerReadFrame(peer, 5*time.Second); rerr != nil {
				return
			}
		}
	}()

	require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected),
		"passive connection must accept over the fake listener and reach Selected")
	require.Equal(t, hsms.SelectedState, conn.State())

	require.NoError(t, conn.Close(), "clean Close over the fake listener")
	_ = peer.Close()
}

// pipeListener is a minimal in-memory fake net.Listener for tests: Accept reads off a channel fed
// by dialFresh, which mints a FRESH net.Pipe() per call — mirroring TestWithDialer_PipeAcceptance's
// "fresh pipe per generation" dial factory, except the freshness requirement here is on the
// listener's Accept side rather than a dial call, since a passive connection re-listens (and
// re-accepts) on every reconnect generation. There is no real socket underneath: Accept blocks
// until either a peer conn is queued by dialFresh or Close is called.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// newPipeListener constructs a ready-to-use pipeListener.
func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// Accept blocks until dialFresh queues a connection or the listener is closed, satisfying
// net.Listener.
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close unblocks any pending Accept, satisfying net.Listener. Idempotent.
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr returns a placeholder address, satisfying net.Listener. No real socket is bound.
func (l *pipeListener) Addr() net.Addr {
	return pipeAddr{}
}

// dialFresh mints a fresh net.Pipe(), queues one end for a future Accept, and returns the other
// end to the caller (the scripted test peer). Returns the peer end directly (rather than via a
// channel) since the test drives it synchronously right after calling dialFresh.
func (l *pipeListener) dialFresh() net.Conn {
	acceptEnd, peerEnd := net.Pipe()

	go func() {
		select {
		case l.conns <- acceptEnd:
		case <-l.closed:
			_ = acceptEnd.Close()
		}
	}()

	return peerEnd
}

// pipeAddr is a placeholder net.Addr for pipeListener.Addr.
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
