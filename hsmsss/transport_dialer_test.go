package hsmsss

// transport_dialer_test.go — coverage for the WithDialer active-dial seam: a custom dialer is
// honored, nil is rejected, keep-alive is silently skipped for a non-TCP conn, the default dialer
// is installed and works, and a real active connection dials + selects over an in-memory net.Pipe.
//
// The file is in package hsmsss (not hsmsss_test) so it can read the unexported cfg.dial field,
// construct the unexported *transport, and reuse the frame helpers from the sibling test files.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestWithDialer_CustomDialerHonored verifies WithDialer installs the supplied dialer and the
// config invokes it (rather than the default net.Dialer) when the transport dials.
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
// conn (e.g. net.Pipe): the *net.TCPConn-only keep-alive setters are skipped without panicking,
// even when a keep-alive interval is configured.
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

// TestWithDialer_PipeAcceptance drives a REAL active HSMS-SS connection over an in-memory net.Pipe
// injected via WithDialer: the dial reaches the injected pipe, the recv loop starts, an inline
// scripted responder answers Select.req with Select.rsp, the connection reaches Selected, and Close
// tears it down cleanly. (The full cross-transport round-trip is re-proven in the integration suite;
// this asserts the dial seam wires a working connection.)
func TestWithDialer_PipeAcceptance(t *testing.T) {
	t.Parallel()

	// Dial factory: mint a fresh net.Pipe + inline responder per dial, so a reconnect (should one
	// occur) gets its own pipe rather than a reused, closed one.
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		endA, endB := net.Pipe()
		go hsmsPipeResponder(endB)

		return endA, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithActive(),
		WithDialer(dial),
		WithConnectionOption(hsms.WithT6(2*time.Second)),
	)
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, conn.Open(ctx, hsms.OpenWaitSelected),
		"active connection must dial over net.Pipe and reach Selected")
	require.Equal(t, hsms.SelectedState, conn.State())

	require.NoError(t, conn.Close(), "clean Close over net.Pipe")
}

// hsmsPipeResponder is a minimal inline HSMS responder for the net.Pipe acceptance test: it reads
// whole HSMS frames off the peer end and answers any Select.req with a status-0 Select.rsp,
// draining (and ignoring) every other frame — including the courtesy Separate on Close. It exits on
// the first read error (the pipe closing at teardown). Because net.Pipe is synchronous and
// unbuffered, an always-reading responder is what lets the connection's writes complete.
func hsmsPipeResponder(peer net.Conn) {
	defer func() { _ = peer.Close() }()

	for {
		payload, err := peerReadFrame(peer, 5*time.Second)
		if err != nil {
			return // pipe closed at teardown, or deadline elapsed
		}

		if len(payload) >= 10 && payload[5] == byte(hsms.SelectReqType) {
			if _, werr := peer.Write(selectRspFrame(payload[:10], hsms.SelectStatusSuccess)); werr != nil {
				return
			}
		}
	}
}
