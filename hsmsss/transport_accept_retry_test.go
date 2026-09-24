package hsmsss

// Regression coverage for acceptConn (transport_passive.go):
// a transient Accept failure must be retried, not read as a teardown.
//
// Before the fix, acceptLoop returned on ANY Accept error.
// A descriptor-exhaustion error (EMFILE) on the FIRST Accept left the passive connection stranded —
// the listener still open, the FSM at NotConnected, and no event to start a reconnect —
// and the same error in the refuse loop stopped refusing extra dialers for the rest of the generation.

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// errTransientAccept is the error failingAcceptListener injects: the same shape the runtime returns
// for descriptor exhaustion, which it does NOT retry internally.
var errTransientAccept = &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}

// failingAcceptListener wraps a real listener and fails the Accept calls whose 1-based index is in
// failOn with errTransientAccept, delegating every other call.
type failingAcceptListener struct {
	net.Listener
	failOn map[int64]bool
	calls  atomic.Int64
}

func (l *failingAcceptListener) Accept() (net.Conn, error) {
	if l.failOn[l.calls.Add(1)] {
		return nil, errTransientAccept
	}

	return l.Listener.Accept()
}

// failingListen returns a ListenFunc whose listener fails the given Accept calls once each.
func failingListen(failOn ...int64) ListenFunc {
	set := make(map[int64]bool, len(failOn))
	for _, n := range failOn {
		set[n] = true
	}

	return func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return &failingAcceptListener{Listener: ln, failOn: set}, nil
	}
}

// TestPassive_TransientFirstAcceptErrorIsRetried proves a transient error on the FIRST Accept
// does not strand the passive side: the peer that connects afterwards is still adopted and selected.
//
// Teeth: make acceptConn return on every error — the passive stays NotConnected, never answers the
// active's Select.req, and waitSelected times out.
func TestPassive_TransientFirstAcceptErrorIsRetried(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	passive := newEndpoint(t, port, false, []Option{WithListener(failingListen(1))})
	active := newEndpoint(t, port, true, nil)

	t.Cleanup(func() {
		_ = active.conn.Close()
		_ = passive.conn.Close()
	})

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenBackground))

	waitSelected(t, passive)
	waitSelected(t, active)
}

// TestPassive_TransientRefuseAcceptErrorIsRetried proves a transient error in the REFUSE loop
// (the second Accept, while a session is live) does not end refusal:
// a later extra dialer is still answered Select.rsp status 1 (E37 §9.2.4.1.1 option 1).
//
// Teeth: make acceptConn return on every error — the refuse loop exits, the extra dialer's
// Select.req lands in the backlog unanswered, and the read below times out.
func TestPassive_TransientRefuseAcceptErrorIsRetried(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	passive := newEndpoint(t, port, false, []Option{WithListener(failingListen(2))})
	active := newEndpoint(t, port, true, nil)

	t.Cleanup(func() {
		_ = active.conn.Close()
		_ = passive.conn.Close()
	})

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenBackground))
	waitSelected(t, passive)

	extra, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = extra.Close() })

	req := hsms.NewSelectReq(hsms.ControlSessionID, [4]byte{0, 0, 0, 42})
	_, err = extra.Write(req.ToBytes())
	require.NoError(t, err)

	require.NoError(t, extra.SetReadDeadline(time.Now().Add(3*time.Second)))
	rsp := make([]byte, 14)
	_, err = io.ReadFull(extra, rsp)
	require.NoError(t, err, "the extra dialer must still be answered after a transient refuse-loop Accept error")

	msg, err := hsms.DecodeHSMSMessage(rsp)
	require.NoError(t, err)
	require.Equal(t, hsms.SelectRspType, msg.Type())
	require.Equal(t, byte(hsms.SelectStatusAlreadyActive), selectStatus(msg))
	require.Equal(t, hsms.SelectedState, passive.conn.State(), "refusing the extra dialer must not disturb the live session")
}

// alwaysFailingListener fails every Accept with errTransientAccept until Close,
// after which it returns errCustomClosed — deliberately NOT net.ErrClosed, as a WithListener
// implementation is free to do.
type alwaysFailingListener struct {
	closed atomic.Bool
	calls  atomic.Int64
}

var errCustomClosed = errors.New("alwaysFailingListener: closed")

func (l *alwaysFailingListener) Accept() (net.Conn, error) {
	l.calls.Add(1)

	if l.closed.Load() {
		return nil, errCustomClosed
	}

	return nil, errTransientAccept
}

func (l *alwaysFailingListener) Close() error   { l.closed.Store(true); return nil }
func (l *alwaysFailingListener) Addr() net.Addr { return pipeAddr{} }

// TestPassive_StopEndsAcceptRetry proves the retry loop cannot outlive its generation:
// Stop must join the accept goroutine promptly even when the listener keeps failing and reports
// its own closure with an error other than net.ErrClosed, and the ctx handed to Start is never cancelled.
// That leaves the Stop seal as the only terminal signal, which is the case this test isolates.
//
// Teeth: drop the isStopping check from acceptConn — the loop keeps retrying after Stop,
// Stop's unbounded g.accept.Wait never returns, and the bound below trips.
func TestPassive_StopEndsAcceptRetry(t *testing.T) {
	t.Parallel()

	ln := &alwaysFailingListener{}
	listen := func(context.Context, string, string) (net.Listener, error) { return ln, nil }

	cfg, err := NewConfig("127.0.0.1", 0, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	tr := newTransport(cfg)
	require.NoError(t, tr.startPassive(context.Background()))

	require.Eventually(t, func() bool { return ln.calls.Load() >= 3 }, 3*time.Second, time.Millisecond,
		"a transient Accept error must be retried")

	stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tr.Stop(stopCtx) }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not join the accept goroutine: the retry loop outlived the generation")
	}
}
