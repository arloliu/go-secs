package hsmsss

// transport_tcpup_refused_test.go — coverage for the TCP-up generation-gate refusal
// (the refused-TCP-up socket leak fix):
// when the core's generation gate refuses a just-established socket
// (t.tcpUp / genRuntime.TCPUpFromGeneration returns false,
// meaning the generation already ended —
// a concurrent teardown raced the dial/accept to completion),
// the caller — startActive or acceptLoop —
// must close the socket itself (nobody else ever will)
// and must NOT spawn a recv loop or a Select procedure for a generation that is already gone.
//
// Both tests drive the refusal DETERMINISTICALLY via genRecRT.setRefuseTCPUp(true) (transport_control_test.go),
// rather than racing a real teardown against a real dial/accept:
// the mock reports the refusal exactly like the real core would,
// so the transport-level branch under test runs every time, with no time.Sleep and no race window.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// singleAcceptListener is a minimal net.Listener whose FIRST Accept returns conn;
// every subsequent call errors.
// It is deliberately narrower than transport_listener_test.go's pipeListener:
// acceptLoop returns immediately after a refused first accept
// (never reaching the refuse-extra-connections loop),
// so nothing here needs to model a second, live connection.
type singleAcceptListener struct {
	conn   net.Conn
	served bool
}

func (l *singleAcceptListener) Accept() (net.Conn, error) {
	if !l.served {
		l.served = true

		return l.conn, nil
	}

	return nil, errors.New("singleAcceptListener: no further connections")
}

func (l *singleAcceptListener) Close() error   { return nil }
func (l *singleAcceptListener) Addr() net.Addr { return pipeAddr{} }

// waitWaitGroup asserts wg.Wait() returns within timeout,
// proving nothing was ever Add'd to it (or that whatever was has already Done'd) —
// the goroutine-accounting proxy for "no recv loop / Select procedure was left parked."
func waitWaitGroupDone(t *testing.T, name string, wait func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: a refused TCP-up must not leave a goroutine parked on this generation's bundle", name)
	}
}

// TestActive_RefusedTCPUpClosesConnAndSkipsRecvLoop is the active-role half of the refused-TCP-up fix
// (transport_active.go startActive):
// a generation-gate refusal reported by t.tcpUp must close the just-dialed conn exactly once,
// leave t.conn/t.procCancel untouched
// (so a live successor generation's bookkeeping can never be clobbered by a dead one),
// and spawn neither the recv loop nor the Select procedure.
//
// Teeth: revert the fix (call rt.TCPUp unconditionally and proceed to spawn regardless of the
// return) — the close count stays 0 and this test fails on that assertion;
// close the conn twice on the refusal path and the same assertion fails from the other side.
func TestActive_RefusedTCPUpClosesConnAndSkipsRecvLoop(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	tracked := &sealCloseTrackingConn{Conn: client}
	dial := func(context.Context, string, string) (net.Conn, error) {
		return tracked, nil
	}

	cfg, err := NewConfig("127.0.0.1", 0, WithDialer(dial))
	require.NoError(t, err)

	tr := newTransport(cfg)
	rt := newGenRecRT(2) // an arbitrary non-zero identity: this mock refuses unconditionally, so the value itself carries no meaning here
	rt.setRefuseTCPUp(true)
	tr.rt = rt              // re-bind to the capability-offering, refusal-controllable runtime
	tr.genCtx = t.Context() // Start normally sets this before startActive; set it directly since this test calls startActive without going through Start

	err = tr.startActive(t.Context())
	require.ErrorIs(t, err, errStartSealed, "a refused TCP-up must abort Start with errStartSealed")

	require.Equal(t, int64(1), tracked.closes.Load(),
		"the refused conn must be closed EXACTLY once — a second close can land on a reused fd")

	tr.connMu.Lock()
	conn := tr.conn
	procCancel := tr.procCancel
	tr.connMu.Unlock()
	require.Nil(t, conn, "t.conn must remain untouched by a refused generation")
	require.Nil(t, procCancel, "t.procCancel must remain untouched by a refused generation")

	tr.startGate.RLock()
	g := tr.wg
	tr.startGate.RUnlock()

	waitWaitGroupDone(t, "recv loop", g.recv.Wait)
	waitWaitGroupDone(t, "Select procedure", g.proc.Wait)
}

// TestPassive_RefusedTCPUpClosesConnAndSkipsRecvLoop is the passive-role half of the refused-TCP-up fix
// (transport_passive.go acceptLoop):
// a generation-gate refusal reported by t.tcpUp must close the just-accepted conn exactly once,
// leave t.conn untouched, and spawn no recv loop.
// acceptLoop is called directly
// (mirroring the responder-procedure tests in transport_procedures_test.go):
// it returns synchronously after a refused first accept,
// so no background goroutine or real listener is needed to observe the outcome.
//
// Teeth: revert the fix (call rt.TCPUp unconditionally and proceed to spawn regardless of the
// return) — the close count stays 0 and this test fails on that assertion;
// close the conn twice on the refusal path and the same assertion fails from the other side.
func TestPassive_RefusedTCPUpClosesConnAndSkipsRecvLoop(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	tracked := &sealCloseTrackingConn{Conn: client}
	ln := &singleAcceptListener{conn: tracked}

	cfg, err := NewConfig("127.0.0.1", 0, WithPassive())
	require.NoError(t, err)

	tr := newTransport(cfg)
	rt := newGenRecRT(1)
	rt.setRefuseTCPUp(true)
	tr.rt = rt              // re-bind to the capability-offering, refusal-controllable runtime
	tr.genCtx = t.Context() // startPassive normally sets this before acceptLoop runs

	g := &genWG{gen: 1}
	g.accept.Add(1) // mirrors startPassive's own Add before spawning acceptLoop

	tr.acceptLoop(g, ln)

	require.Equal(t, int64(1), tracked.closes.Load(),
		"the refused conn must be closed EXACTLY once — a second close can land on a reused fd")

	tr.connMu.Lock()
	conn := tr.conn
	tr.connMu.Unlock()
	require.Nil(t, conn, "t.conn must remain untouched by a refused generation")

	waitWaitGroupDone(t, "recv loop", g.recv.Wait)
}
