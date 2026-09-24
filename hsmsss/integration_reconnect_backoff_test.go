package hsmsss

// integration_reconnect_backoff_test.go — a real active endpoint whose Select.req is repeatedly
// refused (status-2 Select.rsp, socket held open by the peer until the client itself closes it)
// must keep re-dialing on a GROWING backoff cadence, not a flat one.
//
// This is supplementary timing evidence over real TCP: jitter can only lengthen the observed
// gaps, so it can never false-fail a correct implementation, but a very slow CI run could let a
// still-broken implementation slip under the loose lower bound (false green). The exact delay
// sequences asserted by hsms's reconnect_backoff_persist_test.go (mock transport, no real
// timing) are the decisive oracle for the underlying behavior; this test only proves the
// mechanism holds end-to-end.

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestActiveReconnectBackoff_GrowsAcrossRepeatedSelectRejections proves the reconnect backoff
// grows across repeated real-TCP Select rejections, not just the mock-transport delay sequences
// in hsms's reconnect-backoff persistence suite.
//
// Before this fix, the reconnect loop reseeded its backoff to initial on every generation,
// so five accepts landed in roughly 4 * (50ms + handshake) ~= 250ms —
// well under the 600ms lower bound a genuinely growing (50, 100, 200, 400ms) cadence produces.
func TestActiveReconnectBackoff_GrowsAcrossRepeatedSelectRejections(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ln, port := listenLoopback(t)
	t.Cleanup(func() { _ = ln.Close() })

	const wantAccepts = 5

	var mu sync.Mutex
	var accepts []time.Time
	allAccepted := make(chan struct{})

	// The fake passive peer: accept, answer every Select.req with a correlated status-2
	// Select.rsp, and hold the socket open — it never closes its side.
	// The active endpoint's own Select-rejection teardown is what closes the connection,
	// which unblocks the drain read below and frees this goroutine to accept the next dial.
	go func() {
		for i := 0; i < wantAccepts; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			accepts = append(accepts, time.Now())
			n := len(accepts)
			mu.Unlock()

			go func(c net.Conn) {
				defer func() { _ = c.Close() }()

				reqHdr, err := peerReadFrame(c, 5*time.Second)
				if err != nil || len(reqHdr) < 10 {
					return
				}

				_, _ = c.Write(selectRspFrame(reqHdr[:10], byte(hsms.SelectStatusNotReady)))

				// Hold the socket open: drain reads (there are none — the active side only
				// closes) until the active endpoint's own teardown closes its side.
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)

			if n == wantAccepts {
				close(allAccepted)

				return
			}
		}
	}()

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithReconnectBackoff(50*time.Millisecond, 2.0)),
		WithConnectionOption(hsms.WithT5(time.Second)),
	})
	t.Cleanup(func() { closeEndpoint(t, active) })

	// Subscribe BEFORE Open, so every dropped-generation cause is captured from the start.
	var causesMu sync.Mutex
	var causes []hsms.TransitionCause
	cancel := active.conn.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		if ev.Current != hsms.NotConnectedState {
			return
		}
		causesMu.Lock()
		causes = append(causes, ev.Cause)
		causesMu.Unlock()
	})
	defer cancel()

	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))

	select {
	case <-allAccepted:
	case <-time.After(20 * time.Second):
		mu.Lock()
		n := len(accepts)
		mu.Unlock()
		t.Fatalf("timed out waiting for %d accepts (saw %d)", wantAccepts, n)
	}

	// Assert the cause BEFORE the timing assertion below: it is independent of the backoff
	// cadence, and asserting it first means a red run on the cadence still proves the cause path.
	// The fifth accept follows four dropped generations;
	// wait until all four drop notifications are delivered, so none can hide a wrong cause.
	var gotCauses []hsms.TransitionCause
	require.Eventually(t, func() bool {
		causesMu.Lock()
		defer causesMu.Unlock()
		gotCauses = append(gotCauses[:0], causes...)

		return len(gotCauses) >= wantAccepts-1
	}, 5*time.Second, time.Millisecond, "the first %d drops must be delivered as NotConnected notifications", wantAccepts-1)
	for i, c := range gotCauses {
		require.Equal(t, hsms.CauseSelectRejected, c, "drop %d must be caused by the peer's Select rejection", i)
	}

	mu.Lock()
	ts := append([]time.Time(nil), accepts...)
	mu.Unlock()
	require.Len(t, ts, wantAccepts)

	gap := ts[wantAccepts-1].Sub(ts[0])
	const wantLowerBound = 600 * time.Millisecond // 0.8 * (50+100+200+400)ms
	t.Logf("accept1..accept5 span: %v (want >= %v)", gap, wantLowerBound)
	require.GreaterOrEqual(t, gap, wantLowerBound,
		"the reconnect dial cadence must reflect a growing backoff across repeated Select rejections")
}
