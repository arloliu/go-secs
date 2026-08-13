package hsmsss

// integration_lifecycle_cause_test.go — end-to-end proof that hsms.Connection.SubscribeLifecycle reports the RIGHT cause
// for the transitions a real HSMS-SS link actually produces.
//
// Every scenario runs a real passive endpoint (the SUT) against a raw TCP peer that authors the
// on-wire frames itself, the same shape integration_metrics_test.go uses.
// A raw peer is what makes the causes deterministic:
// a peer Separate is written explicitly rather than depending on another endpoint's best-effort courtesy farewell,
// which TryLocks the write lock and may legitimately be skipped.

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// causeLog records the lifecycle events a subscription observes, in order.
type causeLog struct {
	mu     sync.Mutex
	events []hsms.LifecycleEvent
}

// subscribeCauses attaches a recording subscription to ep and registers its cancel for cleanup.
func subscribeCauses(t *testing.T, ep *endpoint) *causeLog {
	t.Helper()

	l := &causeLog{}
	t.Cleanup(ep.conn.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
	}))

	return l
}

// snapshot copies the recorded events so assertions never read while the notifier writes.
func (l *causeLog) snapshot() []hsms.LifecycleEvent {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]hsms.LifecycleEvent, len(l.events))
	copy(out, l.events)

	return out
}

// waitBringUp waits (bounded poll, never a Sleep) until the newest event past from is the entry into Selected,
// asserts every bring-up event's cause, and returns the full log.
//
// A bring-up legitimately reports EITHER two transitions (NotConnected -> NotSelected -> Selected) or one (NotConnected -> Selected).
// The supervisor dedups on the state entered,
// so a select handshake that completes before the TCP-up event is drained collapses the pair.
// Both shapes must end in an entry into Selected caused by the completed handshake,
// and a reported entry into NotSelected must name the local Open.
func (l *causeLog) waitBringUp(t *testing.T, from int) []hsms.LifecycleEvent {
	t.Helper()

	var events []hsms.LifecycleEvent

	require.Eventually(t, func() bool {
		events = l.snapshot()

		return len(events) > from && events[len(events)-1].Current == hsms.SelectedState
	}, 5*time.Second, 2*time.Millisecond, "timeout waiting for the link to report Selected")

	for i, ev := range events[from:] {
		if ev.Current == hsms.NotSelectedState {
			require.Equal(t, hsms.CauseLocalOpen, ev.Cause, "bring-up event %d: the accepted TCP link is the local Open's doing", i)

			continue
		}

		require.Equal(t, hsms.SelectedState, ev.Current, "bring-up event %d must enter NotSelected or Selected", i)
		require.Equal(t, hsms.CauseSelectAccepted, ev.Cause, "bring-up event %d: the select handshake completed", i)
	}

	return events
}

// waitTeardown waits for one more event past the after already recorded, asserts it is the drop out of Selected,
// and returns it so the caller can assert the cause under test.
func (l *causeLog) waitTeardown(t *testing.T, after int) hsms.LifecycleEvent {
	t.Helper()

	var events []hsms.LifecycleEvent

	require.Eventually(t, func() bool {
		events = l.snapshot()

		return len(events) > after
	}, 5*time.Second, 2*time.Millisecond, "timeout waiting for the teardown transition")

	ev := events[after]
	require.Equal(t, hsms.SelectedState, ev.Previous)
	require.Equal(t, hsms.NotConnectedState, ev.Current)

	return ev
}

// selectPassiveSUT brings a passive endpoint up to Selected against a raw peer that writes the
// Select.req itself, and returns the raw peer connection.
// It leaves the SUT Selected with the raw socket live, so a caller can drive whatever teardown the
// scenario needs.
func selectPassiveSUT(t *testing.T, ep *endpoint, port int) *net.TCPConn {
	t.Helper()

	require.NoError(t, ep.conn.Open(t.Context(), hsms.OpenBackground))

	raw := dialPassive(t, port)
	_, err := raw.Write(selectReqFrame([4]byte{0x01, 0x02, 0x03, 0x04}))
	require.NoError(t, err)
	expectSelectRsp(t, raw)
	waitSelected(t, ep)

	return raw
}

// A local Close from Selected reports CauseLocalClose, and the bring-up before it reports
// CauseLocalOpen then CauseSelectAccepted.
func TestLifecycleCause_LocalCloseOverRealLink(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ep := newEndpoint(t, port, false, nil)
	defer closeEndpoint(t, ep)

	log := subscribeCauses(t, ep)

	raw := selectPassiveSUT(t, ep, port)
	defer func() { _ = raw.Close() }()

	up := log.waitBringUp(t, 0)

	require.NoError(t, ep.conn.Close())

	require.Equal(t, hsms.CauseLocalClose, log.waitTeardown(t, len(up)).Cause)
}

// A peer Separate.req while Selected reports CausePeerSeparate — distinguishable from both a local
// Close and a bare socket drop, which is the whole point of the cause.
func TestLifecycleCause_PeerSeparateOverRealLink(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ep := newEndpoint(t, port, false, nil)
	defer closeEndpoint(t, ep)

	log := subscribeCauses(t, ep)

	raw := selectPassiveSUT(t, ep, port)
	defer func() { _ = raw.Close() }()

	up := log.waitBringUp(t, 0)

	// E37.1 §7.6: the peer announces it is leaving.
	// The SUT drives an involuntary disconnect.
	_, err := raw.Write(buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0x00, 0x00, 0x00, 0x01}))
	require.NoError(t, err)

	require.Equal(t, hsms.CausePeerSeparate, log.waitTeardown(t, len(up)).Cause)
}

// A peer that disappears without announcing it — the socket simply closes — reports CauseIOError,
// NOT CausePeerSeparate.
func TestLifecycleCause_IOErrorOverRealLink(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ep := newEndpoint(t, port, false, nil)
	defer closeEndpoint(t, ep)

	log := subscribeCauses(t, ep)

	raw := selectPassiveSUT(t, ep, port)
	up := log.waitBringUp(t, 0)

	require.NoError(t, raw.Close()) // no Separate, no warning: the read side just fails

	require.Equal(t, hsms.CauseIOError, log.waitTeardown(t, len(up)).Cause)
}

// A subscription survives a full Close/Open cycle and observes the second bring-up too,
// and its cancel stops delivery for good.
func TestLifecycleCause_SubscriptionSurvivesReopenAndCancels(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	ep := newEndpoint(t, port, false, nil)
	defer closeEndpoint(t, ep)

	l := &causeLog{}
	cancel := ep.conn.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
	})

	raw := selectPassiveSUT(t, ep, port)
	up := l.waitBringUp(t, 0)
	require.NoError(t, ep.conn.Close())
	_ = raw.Close()

	require.Equal(t, hsms.CauseLocalClose, l.waitTeardown(t, len(up)).Cause)
	firstCycle := len(l.snapshot())

	// Second cycle on the same connection value: the subscription was never re-registered.
	raw2 := selectPassiveSUT(t, ep, port)
	l.waitBringUp(t, firstCycle)

	// Quiescent: the Selected event is delivered and nothing is in flight,
	// so "nothing after cancel" is a deterministic claim rather than a race against an in-flight dispatch.
	cancel()
	cancel() // idempotent

	before := len(l.snapshot())
	require.NoError(t, ep.conn.Close())
	_ = raw2.Close()

	// The connection really did transition again; the cancelled subscription simply did not see it.
	waitState(t, ep, hsms.NotConnectedState)
	require.Len(t, l.snapshot(), before, "a cancelled subscription must receive nothing further")
}

// The active Select procedure's T6 expiry reports CauseT6Timeout, not CauseIOError.
// It is the one site whose cause is chosen conditionally on the error, so the branch is covered here rather than asserted.
func TestLifecycleCause_SelectT6TimeoutOverRealLink(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	peerDone := make(chan struct{})
	stopPeer := sync.OnceFunc(func() { close(peerDone) })

	defer stopPeer()

	go func() { peerErrCh <- runSelectBlackholePeer(ln, peerDone) }()

	// T6 short enough to fire well inside the test window, and far shorter than the T7 dwell,
	// so the drop under test is the Select transaction timing out rather than the NOT-SELECTED dwell expiring.
	ep := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithT6(300 * time.Millisecond)),
		WithConnectionOption(hsms.WithT7(30 * time.Second)),
	})
	defer closeEndpoint(t, ep)

	log := subscribeCauses(t, ep)

	require.NoError(t, ep.conn.Open(t.Context(), hsms.OpenBackground))

	var events []hsms.LifecycleEvent

	require.Eventually(t, func() bool {
		events = log.snapshot()

		return len(events) >= 2
	}, 10*time.Second, 2*time.Millisecond, "timeout waiting for the TCP-up and the T6 drop")

	require.Equal(t, hsms.NotSelectedState, events[0].Current)
	require.Equal(t, hsms.CauseLocalOpen, events[0].Cause)

	require.Equal(t, hsms.NotSelectedState, events[1].Previous)
	require.Equal(t, hsms.NotConnectedState, events[1].Current)
	require.Equal(t, hsms.CauseT6Timeout, events[1].Cause)

	stopPeer() // release the peer so its clean return is observable

	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "select-blackhole peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the select-blackhole peer to finish")
	}
}

// runSelectBlackholePeer accepts one connection from an active SUT, reads its Select.req, and never answers.
// The SUT's Select transaction therefore times out at T6 rather than failing on the socket,
// which is the only way to reach the CauseT6Timeout branch.
// It holds the connection open until done closes, so the SUT never sees an EOF that would report CauseIOError instead.
func runSelectBlackholePeer(ln net.Listener, done <-chan struct{}) error {
	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	req, err := peerReadFrame(conn, 10*time.Second)
	if err != nil {
		return fmt.Errorf("read select.req: %w", err)
	}

	if len(req) < 10 || req[5] != byte(hsms.SelectReqType) {
		return fmt.Errorf("expected select.req, got SType=%d", req[5])
	}

	<-done // hold the socket open: a close here would surface as CauseIOError, not CauseT6Timeout

	return nil
}
