package hsmsss

// transport_sealed_start_test.go — coverage for the I1 "sealed start" abort (errStartSealed) on
// both roles. startActive (transport_active.go) and startPassive (transport_passive.go) each
// register their generation's goroutines on t.wg ONLY while t.stopping is false, checked under
// t.startGate.RLock() right after the dial/listen succeeds.
// If a Stop has already sealed the transport (t.stopping = true, set under t.startGate.Lock()),
// Start rolls back the socket it just created — instead of racing Stop's WaitGroup Waits — and
// returns errStartSealed.
//
// These tests drive the seal DETERMINISTICALLY: they set t.stopping = true under t.startGate.Lock()
// (the exact lock discipline Stop uses — see transport.go's Stop, "t.startGate.Lock(); t.stopping =
// true; ... t.startGate.Unlock()") BEFORE calling the unexported startActive/startPassive directly.
// There is no race window and no time.Sleep: the seal is in place before Start's RLock check ever
// runs, so the sealed branch is taken every time.

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// sealCloseTrackingConn wraps a net.Conn and COUNTS Close calls, so a test can
// assert the just-dialed socket was rolled back by the sealed-start branch (mirrors the
// deadlineErrConn / refuseSetDeadlineRecorder wrap-and-record pattern already used in
// transport_passive_test.go).
//
// A count rather than a flag: the rollback contract is close EXACTLY once,
// and a second close is a real defect — the fd may have been reused by then,
// so the extra Close lands on an unrelated socket.
// A boolean cannot tell that apart from the correct behavior.
type sealCloseTrackingConn struct {
	net.Conn
	closes atomic.Int64
}

func (c *sealCloseTrackingConn) Close() error {
	c.closes.Add(1)

	return c.Conn.Close()
}

// sealCloseTrackingListener is the listener-side counterpart of sealCloseTrackingConn, used by
// the passive test to assert the just-listened socket was rolled back.
type sealCloseTrackingListener struct {
	net.Listener
	closes atomic.Int64
}

func (l *sealCloseTrackingListener) Close() error {
	l.closes.Add(1)

	return l.Listener.Close()
}

// sealTransport sets t.stopping = true under t.startGate.Lock(), copying Stop's own lock
// discipline (transport.go Stop: "t.startGate.Lock(); t.stopping = true; ...;
// t.startGate.Unlock()") exactly, minus the parts of Stop that only matter once a generation is
// actually live (capturing g, closing conn/listener, joining goroutines). This is the minimum
// action that makes startActive/startPassive's own RLock-guarded "if t.stopping" check observe
// the seal, and it does so deterministically: the flag is set and visible (Unlock happens-before
// the subsequent RLock in Start) before Start is ever called.
func sealTransport(t *testing.T, tr *transport) {
	t.Helper()

	tr.startGate.Lock()
	tr.stopping = true
	tr.startGate.Unlock()
}

// TestActive_StartAbortsWhenSealed proves the active-role sealed-start branch
// (transport_active.go startActive, ~line 149-155): a Stop that sealed the transport before
// startActive's RLock check makes Start close the just-dialed conn and return errStartSealed,
// WITHOUT storing the conn on t.conn.
func TestActive_StartAbortsWhenSealed(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	// Both ends are closed unconditionally: the client end is closed by the behavior under test,
	// but a failing mutation would otherwise leak it.
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	tracked := &sealCloseTrackingConn{Conn: client}
	dial := func(context.Context, string, string) (net.Conn, error) {
		return tracked, nil
	}

	cfg, err := NewConfig("127.0.0.1", 0, WithDialer(dial))
	require.NoError(t, err)

	tr := newTransport(cfg)
	sealTransport(t, tr)

	err = tr.startActive(t.Context())
	require.ErrorIs(t, err, errStartSealed, "a sealed transport must abort Start with errStartSealed")

	require.Equal(t, int64(1), tracked.closes.Load(),
		"the just-dialed conn must be closed exactly once when Start aborts on a sealed transport")

	tr.connMu.Lock()
	conn := tr.conn
	tr.connMu.Unlock()
	require.Nil(t, conn, "t.conn must remain nil — the sealed branch returns before storing it")
}

// TestPassive_StartAbortsWhenSealed proves the passive-role sealed-start branch
// (transport_passive.go startPassive, ~line 65-73): a Stop that sealed the transport before
// startPassive's RLock check makes Start close the just-created listener, clear t.listener back
// to nil (it is stored BEFORE the seal check, unlike the active conn), and return
// errStartSealed.
func TestPassive_StartAbortsWhenSealed(t *testing.T) {
	t.Parallel()

	realLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	tracked := &sealCloseTrackingListener{Listener: realLn}
	t.Cleanup(func() { _ = tracked.Close() })

	listen := func(context.Context, string, string) (net.Listener, error) {
		return tracked, nil
	}

	cfg, err := NewConfig("127.0.0.1", 0, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	tr := newTransport(cfg)
	sealTransport(t, tr)

	err = tr.startPassive(t.Context())
	require.ErrorIs(t, err, errStartSealed, "a sealed transport must abort Start with errStartSealed")

	require.Equal(t, int64(1), tracked.closes.Load(),
		"the just-created listener must be closed exactly once when Start aborts on a sealed transport")

	tr.connMu.Lock()
	ln := tr.listener
	tr.connMu.Unlock()
	require.Nil(t, ln, "t.listener must be cleared back to nil by the sealed branch")
}
