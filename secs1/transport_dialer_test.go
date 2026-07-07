package secs1

// transport_dialer_test.go — coverage for the WithDialer active-dial seam: a custom dialer is
// honored, nil is rejected, keep-alive is silently skipped for a non-TCP conn, the default dialer is
// installed and works, and a real active connection dials + auto-commits to Selected over an
// in-memory net.Pipe.
//
// The file is in package secs1 (not secs1_test) so it can read the unexported cfg.dial field,
// construct the unexported *transport, and call applyKeepAlive directly.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestWithDialer_CustomDialerHonored verifies WithDialer installs the supplied dialer and the config
// invokes it (rather than the default net.Dialer) when the transport dials.
func TestWithDialer_CustomDialerHonored(t *testing.T) {
	t.Parallel()

	var called bool
	pipeA, pipeB := net.Pipe()
	defer func() { _ = pipeB.Close() }()

	cfg, err := NewConfig("127.0.0.1", 5000, WithActive(), WithDialer(
		func(_ context.Context, _, _ string) (net.Conn, error) {
			called = true
			return pipeA, nil
		}))
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if cfg.dial == nil {
		t.Fatal("dial not set")
	}

	// Invoke the stored dialer the way the transport will.
	c, err := cfg.dial(context.Background(), "tcp", "127.0.0.1:5000")
	if err != nil || !called || c != pipeA {
		t.Fatalf("custom dialer not used: called=%v err=%v", called, err)
	}
	_ = c.Close()
}

// TestWithDialer_NilRejected verifies that a nil dialer is a configuration error.
func TestWithDialer_NilRejected(t *testing.T) {
	t.Parallel()

	if _, err := NewConfig("127.0.0.1", 5000, WithDialer(nil)); err == nil {
		t.Fatal("expected error for nil dialer")
	}
}

// TestWithDialer_DefaultDialerInstalled verifies NewConfig installs a non-nil default dialer that
// dials a real TCP endpoint (so the production path is unchanged when WithDialer is not used).
func TestWithDialer_DefaultDialerInstalled(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	cfg, err := NewConfig("127.0.0.1", 5000)
	require.NoError(t, err)
	require.NotNil(t, cfg.dial, "NewConfig must install a default dialer")

	conn, err := cfg.dial(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err, "the default dialer must establish a real TCP connection")
	require.IsType(t, &net.TCPConn{}, conn, "the default dialer yields a *net.TCPConn")
	_ = conn.Close()
}

// TestApplyKeepAlive_SkippedForNonTCPConn verifies applyKeepAlive is a silent no-op for a non-TCP
// conn (e.g. net.Pipe): the *net.TCPConn-only keep-alive setters are skipped without panicking, even
// when a keep-alive interval is configured.
func TestApplyKeepAlive_SkippedForNonTCPConn(t *testing.T) {
	t.Parallel()

	cfg, err := NewConfig("127.0.0.1", 5000, WithTCPKeepAlive(10*time.Second))
	require.NoError(t, err)

	tr := newTransport(cfg)

	pipeA, pipeB := net.Pipe()
	defer func() { _ = pipeA.Close() }()
	defer func() { _ = pipeB.Close() }()

	require.NotPanics(t, func() { tr.applyKeepAlive(pipeA) },
		"keep-alive must be silently skipped for a non-TCP conn")
}

// TestWithDialer_PipeAcceptance drives a REAL active SECS-I connection over an in-memory net.Pipe
// injected via WithDialer: the dial reaches the injected pipe and the line engine starts, the
// connection auto-commits to Selected at TCP-up (SECS-I has no Select handshake), and Close tears it
// down cleanly. The peer end is drained by a helper goroutine so the unbuffered pipe never wedges.
func TestWithDialer_PipeAcceptance(t *testing.T) {
	t.Parallel()

	// Dial factory: mint a fresh net.Pipe per dial and drain/close its peer end at teardown, so a
	// reconnect (should one occur) gets its own pipe and no goroutine leaks.
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		endA, endB := net.Pipe()
		go drainAndClose(endB)

		return endA, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithActive(), WithDialer(dial))
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected),
		"active connection must dial over net.Pipe and auto-commit to Selected")
	require.Equal(t, hsms.SelectedState, conn.State())

	require.NoError(t, conn.Close(), "clean Close over net.Pipe")
}

// TestStartActive_ConnectTimeoutBoundsDial verifies WithConnectTimeout wraps the dial site with a
// per-attempt deadline: a dialer that blocks until its ctx is done (simulating a black-holed peer
// that never completes the TCP handshake) is forced to return within the configured timeout rather
// than riding out the OS connect timeout (~2 min) or hanging on the unbounded engine ctx.
func TestStartActive_ConnectTimeoutBoundsDial(t *testing.T) {
	t.Parallel()

	// blockingDial never returns on its own; it only unblocks when its ctx is cancelled/expires. If
	// WithConnectTimeout did not wrap the dial ctx with a deadline, this call would hang on the
	// unbounded background ctx passed to Start, and the test would time out instead of failing fast.
	blockingDial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	cfg, err := NewConfig("127.0.0.1", 5000,
		WithActive(),
		WithDialer(blockingDial),
		WithConnectTimeout(50*time.Millisecond),
	)
	require.NoError(t, err)

	tr := newTransport(cfg)
	tr.ArmStart()

	done := make(chan error, 1)
	go func() { done <- tr.Start(context.Background(), newMockRuntime()) }()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"Start error must wrap context.DeadlineExceeded")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start did not return within 500ms — connectTimeout was not honored at the dial site")
	}
}

// drainAndClose reads and discards everything the peer end receives until the pipe closes at
// teardown, then closes it. It keeps the unbuffered net.Pipe from wedging if the connection ever
// writes, and exits on the first read error (the connection's Close closing the other end).
func drainAndClose(peer net.Conn) {
	defer func() { _ = peer.Close() }()

	buf := make([]byte, 64)
	for {
		if _, err := peer.Read(buf); err != nil {
			return
		}
	}
}
