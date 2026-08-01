package hsmsss

// transport_activity_test.go — unit tests for the monotonic activity stamps that feed
// activity-based linktest suppression: the send stamp written by (t *transport).Write,
// the recv stamp written by recvLoop (covered end-to-end by the integration suite; here
// the field is driven directly), the sinceLastActivity idle computation, and the
// per-generation stamp reset.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestTransport constructs a bare *transport (no Start) via the same config + newTransport
// path transport_test.go's tests use, for unit tests that only need to poke transport fields
// directly.
func newTestTransport(t *testing.T) *transport {
	t.Helper()

	cfg, err := NewConfig("127.0.0.1", 0)
	require.NoError(t, err)

	return newTransport(cfg)
}

func TestTransport_WriteStampsSendActivity(t *testing.T) {
	tr := newTestTransport(t)

	require.Zero(t, tr.lastSendStamp.Load(), "no send stamp before any Write")

	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() { buf := make([]byte, 16); _, _ = server.Read(buf) }()

	require.NoError(t, tr.Write(t.Context(), client, net.Buffers{[]byte("x")}))
	require.Positive(t, tr.lastSendStamp.Load(), "successful Write must stamp send activity")
}

func TestTransport_WriteErrorDoesNotStamp(t *testing.T) {
	tr := newTestTransport(t)

	require.Error(t, tr.Write(t.Context(), nil, net.Buffers{[]byte("x")}))
	require.Zero(t, tr.lastSendStamp.Load(), "failed Write must not stamp send activity")
}

func TestTransport_SinceLastActivity(t *testing.T) {
	tr := newTestTransport(t)

	// No stamps at all: idle since clockBase — a large duration, never negative.
	require.GreaterOrEqual(t, tr.sinceLastActivity(), time.Duration(0))

	// The most recent of the two stamps wins.
	tr.lastSendStamp.Store(tr.monoNanos() - int64(time.Hour))
	tr.lastRecvStamp.Store(tr.monoNanos() - int64(10*time.Millisecond))
	require.Less(t, tr.sinceLastActivity(), time.Second,
		"recv stamp 10ms ago must dominate the 1h-old send stamp")

	tr.lastSendStamp.Store(tr.monoNanos())
	require.Less(t, tr.sinceLastActivity(), 100*time.Millisecond, "fresh send stamp must reset idle")
}

func TestTransport_ResetActivityStamps(t *testing.T) {
	tr := newTestTransport(t)

	tr.lastSendStamp.Store(tr.monoNanos() - int64(time.Hour))
	tr.lastRecvStamp.Store(tr.monoNanos() - int64(time.Hour))

	tr.resetActivityStamps()

	require.Less(t, tr.sinceLastActivity(), time.Second,
		"reset must re-baseline both stamps to now so a new generation starts coherent")
}

// TestTransport_ActiveStartResetsActivityBaseline: the ACTIVE conn-publish path
// (startActive) must re-baseline the stamps through production wiring. The peer is a
// silent listener: it accepts and sends nothing, so lastRecvStamp can only be freshened
// by resetActivityStamps() at the t.conn publish — not by handshake traffic.
func TestTransport_ActiveStartResetsActivityBaseline(t *testing.T) {
	t.Parallel()

	// Arrange per transport_test.go:208-229: net.Listen on loopback (accept and hold the
	// conn open, never write), active-config bare transport via newTransport, the same
	// mock runtime that file passes to Start, t.Cleanup(Stop).
	ln, port := listenLoopback(t)
	defer ln.Close()

	peerConnCh := acceptOneAsync(ln) // silent peer: accepts and holds the conn open, never writes

	cfg, err := NewConfig("127.0.0.1", port)
	require.NoError(t, err)

	rt := newMockRT()
	tr := newTransport(cfg)

	stale := tr.monoNanos() - int64(time.Hour)
	tr.lastSendStamp.Store(stale)
	tr.lastRecvStamp.Store(stale)

	require.NoError(t, tr.Start(context.Background(), rt))
	t.Cleanup(func() { tr.Stop(context.Background()) }) //nolint:errcheck

	select {
	case peerConn := <-peerConnCh:
		t.Cleanup(func() { peerConn.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not accept the connection in time")
	}

	require.Eventually(t, func() bool {
		return tr.lastRecvStamp.Load() > stale+int64(30*time.Minute)
	}, 5*time.Second, 10*time.Millisecond,
		"startActive must re-baseline lastRecvStamp at conn-publish: the silent peer sent nothing, so only resetActivityStamps can freshen it")
}

// TestTransport_PassiveAcceptResetsActivityBaseline: the PASSIVE conn-publish path
// (acceptLoop) must re-baseline the stamps when a peer connects — not at listen time.
func TestTransport_PassiveAcceptResetsActivityBaseline(t *testing.T) {
	t.Parallel()

	// Arrange per transport_listener_test.go:123-179: passive-config bare transport with
	// newPipeListener, mock runtime, Start (listener armed, no conn yet), t.Cleanup(Stop).
	fakeLn := newPipeListener()
	defer func() { _ = fakeLn.Close() }()

	listen := func(_ context.Context, _, _ string) (net.Listener, error) {
		return fakeLn, nil
	}

	cfg, err := NewConfig("127.0.0.1", 5000, WithPassive(), WithListener(listen))
	require.NoError(t, err)

	rt := newMockRT()
	tr := newTransport(cfg)

	require.NoError(t, tr.Start(context.Background(), rt))
	t.Cleanup(func() { tr.Stop(context.Background()) }) //nolint:errcheck

	stale := tr.monoNanos() - int64(time.Hour)
	// Seed AFTER Start returns and BEFORE dialing in — proves the reset happens at
	// conn-publish inside acceptLoop, not anywhere in Start.
	tr.lastSendStamp.Store(stale)
	tr.lastRecvStamp.Store(stale)

	peer := fakeLn.dialFresh() // silent peer: connects, never writes
	t.Cleanup(func() { peer.Close() })

	require.Eventually(t, func() bool {
		return tr.lastRecvStamp.Load() > stale+int64(30*time.Minute)
	}, 5*time.Second, 10*time.Millisecond,
		"acceptLoop must re-baseline lastRecvStamp when it publishes the accepted conn")
}

func TestLinktestFailureStep_Branches(t *testing.T) {
	tests := []struct {
		name           string
		suppress       bool
		recvNow        int64
		sentAt         int64
		inflight       int64
		fails          int
		recvAtLastFail int64
		wantFails      int
		wantMem        int64
		wantCredited   bool
	}{
		{"suppression off always counts", false, 100, 50, 5, 2, 0, 3, 100, false},
		{"frame after probe credits, run resets", true, 100, 50, 0, 2, 10, 0, 10, true},
		{"inflight at failure credits (race closure)", true, 40, 50, 1, 2, 40, 0, 40, true},
		{"silence with no history counts", true, 40, 50, 0, 0, 0, 1, 40, false},
		{"consecutive silence increments", true, 40, 50, 0, 2, 40, 3, 40, false},
		{"life since last counted failure restarts run at 1", true, 45, 50, 0, 2, 40, 1, 45, false},
		{"restart needs prior fails", true, 45, 50, 0, 0, 40, 1, 45, false},
		{"equal stamps do not credit (strict >)", true, 50, 50, 0, 0, 0, 1, 50, false},
		{"equal recvAtLastFail does not restart (strict >)", true, 40, 50, 0, 1, 40, 2, 40, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotFails, gotMem, gotCredited := linktestFailureStep(tt.suppress, tt.recvNow, tt.sentAt, tt.inflight, tt.fails, tt.recvAtLastFail)
			require.Equal(t, tt.wantFails, gotFails)
			require.Equal(t, tt.wantMem, gotMem)
			require.Equal(t, tt.wantCredited, gotCredited)
		})
	}
}

// TestLinktestDisconnectRecheck covers runLinktest's final pre-disconnect re-check (the last
// guard before TCPDown once the reducer's consecutive-failure run reaches threshold): a
// suppression-off run always disconnects regardless of inflight/recvNow; with suppression on,
// either an inflight data send or a frame that postdates the probe converts to a credit,
// otherwise it disconnects — including the exact recvNow == sentAt boundary (strict >, no
// credit).
func TestLinktestDisconnectRecheck(t *testing.T) {
	tests := []struct {
		name     string
		suppress bool
		inflight int64
		recvNow  int64
		sentAt   int64
		want     bool // true = proceed with disconnect
	}{
		{"suppression off always disconnects", false, 5, 200, 100, true},
		{"inflight send credits", true, 1, 50, 100, false},
		{"frame after probe credits", true, 0, 150, 100, false},
		{"neither condition disconnects", true, 0, 50, 100, true},
		{"boundary recvNow == sentAt disconnects (strict >)", true, 0, 100, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := linktestDisconnectRecheck(tt.suppress, tt.inflight, tt.recvNow, tt.sentAt)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestLinktestFailureStep_Sequence: the stateful proof the integration suite cannot force
// deterministically. Threshold 2. Silent failure counts (run=1); a frame between probes
// restarts the run at 1 instead of disconnecting at 2; one further silent failure then
// reaches the threshold exactly.
func TestLinktestFailureStep_Sequence(t *testing.T) {
	const threshold = 2
	fails, mem := 0, int64(0)
	var credited bool

	// Probe 1 at sentAt=100, total silence (recvNow=90 predates it): counted, run=1.
	fails, mem, credited = linktestFailureStep(true, 90, 100, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 1, fails)
	require.Less(t, fails, threshold)

	// A frame arrives at t=150, BETWEEN probes. Probe 2 at sentAt=200 fails in silence:
	// recvNow(150) predates sentAt (no credit) but postdates mem(90) — run RESTARTS at 1.
	// A build missing the recvAtLastFail state would count to 2 and disconnect here.
	fails, mem, credited = linktestFailureStep(true, 150, 200, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 1, fails, "life between counted failures must restart the run, not extend it")
	require.Less(t, fails, threshold)

	// Probe 3 in total silence: the restarted run increments to 2 -> threshold reached.
	fails, mem, credited = linktestFailureStep(true, 150, 300, 0, fails, mem)
	require.False(t, credited)
	require.Equal(t, 2, fails)
	require.GreaterOrEqual(t, fails, threshold, "an uninterrupted silent run must still disconnect exactly at threshold")
	require.Equal(t, int64(150), mem, "the counting branch stamps recvAtLastFail to recvNow, not sentAt")
}
