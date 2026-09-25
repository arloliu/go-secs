package secs1

// transport_bounddial_test.go covers the core's dial-bounding back-channel (gencap.DialBounder)
// over a REAL active secs1 transport.
// A channel-controlled DialFunc drives the dial deterministically (no real network black-hole timing),
// and every Open/Close call runs on a goroutine, awaited with a bounded timeout,
// so a red assertion here fails promptly instead of hanging the package.
//
// secs1 has no Select handshake: TCPUp and CommitSelected run synchronously, back to back, the
// instant the dial succeeds (startActive), so a delivered conn reaches Selected immediately with
// no peer responder needed — unlike hsmsss's equivalent file.

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// boundDialLifecycleTimeout bounds every Open/Close call this file drives on a goroutine.
const boundDialLifecycleTimeout = 5 * time.Second

// channelDialer returns a DialFunc that blocks until either ctx is done (returns ctx.Err()) or a
// conn is delivered on connCh (returns it unconditionally — modelling a dial that has already
// committed to succeeding, the way a real dialer's connect() that already completed is not
// retroactively undone by a later ctx cancellation). started, if non-nil, is closed once the
// DialFunc has been called and is parked.
func channelDialer(started chan<- struct{}, connCh <-chan net.Conn) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if started != nil {
			close(started)
		}

		select {
		case conn := <-connCh:
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// channelDialerHeldAfterConn is channelDialer with an added post-commit hold: once the dial has
// committed to succeeding (a conn was delivered on connCh), it closes committed and then blocks on
// resume before returning — so a test can cancel the caller ctx or start a Close during exactly the
// window between the dial's own commit and Start's return, without racing the dial's own select
// between the delivered conn and ctx.Done().
func channelDialerHeldAfterConn(started, committed chan<- struct{}, connCh <-chan net.Conn, resume <-chan struct{}) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if started != nil {
			close(started)
		}

		select {
		case conn := <-connCh:
			if committed != nil {
				close(committed)
			}
			<-resume

			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// closeOnce returns a function that closes ch exactly once; further calls are safe no-ops.
func closeOnce(ch chan struct{}) func() {
	var once sync.Once

	return func() { once.Do(func() { close(ch) }) }
}

// closeBounded calls conn.Close() on a goroutine and fails the test if it does not return within
// boundDialLifecycleTimeout — cleanup must never itself be a bare blocking call.
func closeBounded(t *testing.T, conn hsms.Connection) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	select {
	case <-done:
	case <-time.After(boundDialLifecycleTimeout):
		t.Errorf("cleanup Close did not return within %s", boundDialLifecycleTimeout)
	}
}

// newBoundDialConn builds a real active secs1 connection wired to channelDialer,
// returning the connection plus the started handle this file's tests drive.
// A test that needs to deliver a conn builds its own connection with channelDialerHeldAfterConn instead.
//
// Cleanup Closes the connection first: channelDialer honours ctx, so Close's abort — firing the
// in-flight Open's abort token — is what unblocks a dial still parked in its select, without ever
// needing a conn delivered on connCh. Anything left sitting in connCh afterward (never consumed by
// a dial) is drained and closed so nothing leaks.
func newBoundDialConn(t *testing.T) (conn hsms.Connection, started <-chan struct{}) {
	t.Helper()

	startedCh := make(chan struct{})
	connChan := make(chan net.Conn, 1)

	cfg, err := NewConfig("127.0.0.1", 0, WithActive(), WithDialer(channelDialer(startedCh, connChan)))
	require.NoError(t, err)

	conn, err = New(cfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		closeBounded(t, conn)

		select {
		case c := <-connChan:
			_ = c.Close()
		default:
		}
	})

	return conn, startedCh
}

// drainUntilClosed reads and discards from conn until it errors (the peer closed it), then closes
// it — keeps an unbuffered net.Pipe from wedging a writer. secs1 has no Select handshake, so the
// peer end here only needs to drain, never respond.
func drainUntilClosed(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

// runConnOpen starts conn.Open(ctx, mode) on its own goroutine and returns a channel with the one result.
func runConnOpen(conn hsms.Connection, ctx context.Context, mode hsms.OpenMode) <-chan error {
	done := make(chan error, 1)
	go func() { done <- conn.Open(ctx, mode) }()

	return done
}

// runConnClose starts conn.Close() on its own goroutine and returns a channel with the one result.
func runConnClose(conn hsms.Connection) <-chan error {
	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	return done
}

// awaitConnErr waits for done within boundDialLifecycleTimeout, failing the test if it does not
// arrive — never a bare blocking receive on the test goroutine.
func awaitConnErr(t *testing.T, done <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(boundDialLifecycleTimeout):
		t.Fatalf("%s did not return within %s", what, boundDialLifecycleTimeout)

		return nil
	}
}

// awaitSig waits for ch to close within boundDialLifecycleTimeout, failing the test if it does not
// arrive — the signal-channel counterpart to awaitConnErr.
func awaitSig(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(boundDialLifecycleTimeout):
		t.Fatalf("%s did not arrive within %s", what, boundDialLifecycleTimeout)
	}
}

// ── active first dial blocked, interrupted by the caller's ctx or by Close ──

// TestActive_FirstDialBlockedCtxDeadline: Open(ctx with a deadline) on a dial that never resolves
// returns the ctx error.
func TestActive_FirstDialBlockedCtxDeadline(t *testing.T) {
	t.Parallel()

	conn, started := newBoundDialConn(t)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	openDone := runConnOpen(conn, ctx, hsms.OpenWaitSelected)
	awaitSig(t, started, "the dial reaching channelDialer")

	err := awaitConnErr(t, openDone, "Open")
	require.ErrorIs(t, err, context.DeadlineExceeded, "an expired caller ctx must roll back a blocked first dial with ctx.Err()")
	require.Equal(t, hsms.NotConnectedState, conn.State())
}

// TestActive_FirstDialBlockedCloseAborts: Close during a blocked first dial aborts Open with
// ErrConnClosed, and Close itself completes.
// It also proves secs1's specific dial-bounding wiring —
// the original Start ctx threaded through startActive as a SEPARATE parameter from engineCtx —
// actually binds: using engineCtx by mistake would leave secs1 dials silently never bound
// (the abort would then have no effect, and this assertion would instead time out via the
// boundDialLifecycleTimeout wrapper rather than fail on a mismatched error).
func TestActive_FirstDialBlockedCloseAborts(t *testing.T) {
	t.Parallel()

	conn, started := newBoundDialConn(t)

	openDone := runConnOpen(conn, t.Context(), hsms.OpenWaitSelected)
	awaitSig(t, started, "the dial reaching channelDialer")

	closeDone := runConnClose(conn)

	err := awaitConnErr(t, openDone, "Open")
	require.ErrorIs(t, err, hsms.ErrConnClosed, "a Close during a blocked first dial must abort Open with ErrConnClosed")

	require.NoError(t, awaitConnErr(t, closeDone, "Close"))
}

// ── dial already succeeded, then the caller's ctx / a Close arrives ──

// TestActive_DialAlreadySucceededLaterCtxCancelHasNoEffect: a dial that has already committed to
// succeeding, cancelled during the window before Start returns, must not be undone —
// OpenBackground succeeds (auto-committing to Selected, secs1 has no Select handshake),
// and the generation is unaffected by the cancel.
func TestActive_DialAlreadySucceededLaterCtxCancelHasNoEffect(t *testing.T) {
	t.Parallel()

	startedCh := make(chan struct{})
	committedCh := make(chan struct{})
	connChan := make(chan net.Conn, 1)
	resumeCh := make(chan struct{})
	resumeOnce := closeOnce(resumeCh)

	cfg, err := NewConfig("127.0.0.1", 0, WithActive(),
		WithDialer(channelDialerHeldAfterConn(startedCh, committedCh, connChan, resumeCh)))
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	endA, endB := net.Pipe()
	t.Cleanup(func() { _ = endA.Close(); _ = endB.Close() })
	go drainUntilClosed(endB)

	// resumeOnce must release the held dial BEFORE Close runs,
	// or Close blocks on lifeMu for the whole cleanup timeout behind a Start call this cleanup itself has not unblocked yet.
	// t.Cleanup is LIFO,
	// so registering both releases in ONE func, in this order, is what pins that order regardless of how many more t.Cleanup calls this test adds later.
	t.Cleanup(func() {
		resumeOnce()
		closeBounded(t, conn)
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	openDone := runConnOpen(conn, ctx, hsms.OpenBackground)
	awaitSig(t, startedCh, "the dial reaching channelDialer")
	connChan <- endA // deliver the conn: the dial commits to succeeding
	awaitSig(t, committedCh, "the dial committing to its conn")

	// Cancel the caller's ctx WHILE the dial is held past its own commit, before Start has
	// returned — race-free, since the dial has already left its select (proven by committedCh),
	// so this cancel can no longer influence which branch that select picked.
	cancel()
	resumeOnce()

	require.NoError(t, awaitConnErr(t, openDone, "Open"))
	require.Equal(t, hsms.SelectedState, conn.State(), "secs1 auto-commits to Selected at TCP-up")

	require.Never(t, func() bool { return conn.State() != hsms.SelectedState }, 200*time.Millisecond, 5*time.Millisecond,
		"a caller ctx cancelled during or after the post-dial commit window must not tear the generation down")
}

// TestActive_DialAlreadySucceededCloseOwnsTeardown: a Close started while the dial is held in its
// post-commit, pre-return window must not race the (already inert) abort path — Open's outcome is
// the normal success, and Close performs the ordinary teardown.
func TestActive_DialAlreadySucceededCloseOwnsTeardown(t *testing.T) {
	t.Parallel()

	startedCh := make(chan struct{})
	committedCh := make(chan struct{})
	connChan := make(chan net.Conn, 1)
	resumeCh := make(chan struct{})
	resumeOnce := closeOnce(resumeCh)

	cfg, err := NewConfig("127.0.0.1", 0, WithActive(),
		WithDialer(channelDialerHeldAfterConn(startedCh, committedCh, connChan, resumeCh)))
	require.NoError(t, err)

	conn, err := New(cfg)
	require.NoError(t, err)

	endA, endB := net.Pipe()
	t.Cleanup(func() { _ = endA.Close(); _ = endB.Close() })
	go drainUntilClosed(endB)

	// resumeOnce must release the held dial BEFORE Close runs,
	// or Close blocks on lifeMu for the whole cleanup timeout behind a Start call this cleanup itself has not unblocked yet.
	// t.Cleanup is LIFO,
	// so registering both releases in ONE func, in this order, is what pins that order regardless of how many more t.Cleanup calls this test adds later.
	t.Cleanup(func() {
		resumeOnce()
		closeBounded(t, conn)
	})

	openDone := runConnOpen(conn, t.Context(), hsms.OpenBackground)
	awaitSig(t, startedCh, "the dial reaching channelDialer")
	connChan <- endA
	awaitSig(t, committedCh, "the dial committing to its conn")

	// Start Close WHILE the dial is held past its own commit — Close's registration fires the
	// (already inert) abort token, which has no effect since the dial no longer selects on it.
	closeDone := runConnClose(conn)
	resumeOnce()

	require.NoError(t, awaitConnErr(t, openDone, "Open"), "Open's outcome must be the normal success")
	require.NoError(t, awaitConnErr(t, closeDone, "Close"), "Close must own the teardown of the just-opened generation")
}
