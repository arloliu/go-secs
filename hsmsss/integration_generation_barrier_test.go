package hsmsss

// integration_generation_barrier_test.go — the hsmsss half of the disconnect-barrier coverage.
//
// hsms/connection_lifecycle_reconnect_test.go's TestReconnect_AbandonedGeneration* suite proves the barrier itself
// (supervisor.step's generation match, connection.injectDisconnect's, and the commitGate the three synchronous commits share)
// against a mock TransportRuntime that satisfies hsms's genCapability BY CONSTRUCTION.
// That leaves one thing unproven in this package:
// whether the REAL core, reached through THIS package's own genRuntime type assertion (transport_control.go), still dispatches a stale report correctly.
// A rename or signature drift on either side fails OPEN.
// hsmsss keeps compiling, falls back to the generation-unaware TCPDown/TCPUp/commitSelected path,
// and the mock-driven tests in this package (which also assert against a constructed mock) cannot see it.
// TestGenRuntime_IsSatisfiedByTheRealCore (transport_control_test.go) pins that the real core satisfies the interface at all;
// the tests below pin that a stale report through it is actually discarded, not just accepted without panicking.
//
// A genuinely wedged recvLoop/runLinktest goroutine that outlives its own generation's bounded Stop
// and then loses the race against its own genCtx.Err() guard (transport_recv.go, transport_procedures.go) is not reproducible on demand:
// the guard narrows the window,
// but the package's own genRuntime doc says cancellation can land between the check and the call at any time,
// so forcing that exact interleaving deterministically would need a new test-only pause hook in production code.
// Rather than add one, these tests call transport.tcpDown directly —
// the same unexported dispatch method a real straggler would have reached had it lost that race —
// naming a generation that has genuinely already ended over a real TCP link.
// That is ordinary same-package whitebox testing, not a new production seam.

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// assertAbandonedGenerationTCPDownCannotDropSuccessor drives a REAL passive core through two generations over a raw TCP peer,
// then fires gen N's tcpDown through the transport's OWN dispatch wrapper (tr.tcpDown) with the given cause,
// naming a generation that has already ended.
// gen N+1 must stay Selected and report no second NotConnected event.
//
// cause/transitionCause are the arguments a real producer in this package would have passed:
// errLinktestFailed/CauseLinktestFail for runLinktest, io.EOF/CauseIOError for recvLoop's read-error branch —
// so the two callers below differ only in which real call site they stand in for.
func assertAbandonedGenerationTCPDownCannotDropSuccessor(t *testing.T, cause error, transitionCause hsms.TransitionCause) {
	t.Helper()

	port := freeLoopbackPort(t)
	conn, tr := newPassiveConnTr(t, port, hsms.WithT5(time.Nanosecond)) // near-immediate redial

	var mu sync.Mutex
	var drops []hsms.TransitionCause
	cancel := conn.SubscribeLifecycle(func(ev hsms.LifecycleEvent) {
		if ev.Current != hsms.NotConnectedState {
			return
		}
		mu.Lock()
		drops = append(drops, ev.Cause)
		mu.Unlock()
	})
	defer cancel()

	dropCount := func() int {
		mu.Lock()
		defer mu.Unlock()

		return len(drops)
	}

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))

	raw1 := dialPassive(t, port)
	_, err := raw1.Write(selectReqFrame([4]byte{0, 0, 0, 1}))
	require.NoError(t, err)
	expectSelectRsp(t, raw1)
	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState }, 5*time.Second, 2*time.Millisecond,
		"gen N must reach Selected over the real link")

	genN := tr.currentGeneration()
	require.NotZero(t, genN, "a live generation must have an identity")

	require.NoError(t, raw1.Close()) // a real EOF drop: gen N tears down for real; reconnect starts

	// Do not dial as soon as raw1 drops — see waitNextGeneration's doc (transport_passive_test.go) for why
	// that races gen N's listener close and can land raw2 on gen N's refuse-extra-connection loop instead of gen N+1.
	waitNextGeneration(t, tr, genN)

	raw2 := dialPassive(t, port) // tolerant of the re-listen window (dialPassive retries)
	_, err = raw2.Write(selectReqFrame([4]byte{0, 0, 0, 2}))
	require.NoError(t, err)
	expectSelectRsp(t, raw2)
	defer func() { _ = raw2.Close() }()

	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState }, 5*time.Second, 2*time.Millisecond,
		"gen N+1 must reselect over a fresh real link")
	require.Eventually(t, func() bool { return dropCount() == 1 }, time.Second, time.Millisecond,
		"the real gen-N drop must have been reported exactly once")

	genN1 := tr.currentGeneration()
	require.NotEqual(t, genN, genN1, "gen N+1 must carry a distinct identity")

	// Simulate the abandoned straggler having lost its race against genCtx cancellation:
	// gen N's report reaches the FSM through the transport's own dispatch wrapper, naming gen N, while gen N+1 owns the link.
	tr.tcpDown(genN, cause, transitionCause)

	require.Never(t, func() bool {
		return conn.State() != hsms.SelectedState || dropCount() > 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"a disconnect reported by a generation that has ended must not drop its successor")

	require.NoError(t, conn.Close())
}

// TestReconnect_AbandonedGenerationLinktestFailCannotDropSuccessor is the linktest-failure half of the disconnect barrier over a REAL passive core:
// hsmsss.runLinktest's own tcpDown call (transport_procedures.go: t.tcpDown(g.gen, errLinktestFailed, hsms.CauseLinktestFail)), replayed for a generation that has already ended.
//
// Mechanically this exercises the SAME generation match as TestReconnect_AbandonedGenerationTCPDownCannotDropSuccessor in the hsms package —
// the barrier never reads the cause — so the value here is not a distinct code path in hsms.
// It is proof that THIS package's own genRuntime dispatch (transport_control.go's t.rt.(genRuntime) assertion),
// which the mock-driven tests in this package cannot exercise,
// still discards a stale report through a real core rather than silently falling back and letting it through.
//
// Teeth (verified by hand, not asserted by a second test): comment out the `cmd.gen != 0 && s.curGen() != cmd.gen` match
// in hsms/supervisor.go's step, and the `gen != 0 && e.id != gen` match in hsms/connection_lifecycle.go's injectDisconnect —
// the released report then legally tears gen N+1 down, a third generation is dialed, and a second NotConnected event reaches the subscriber.
func TestReconnect_AbandonedGenerationLinktestFailCannotDropSuccessor(t *testing.T) {
	t.Parallel()

	assertAbandonedGenerationTCPDownCannotDropSuccessor(t, errLinktestFailed, hsms.CauseLinktestFail)
}

// TestReconnect_AbandonedGenerationPassiveRecvTCPDownCannotDropSuccessor is the passive-role recv-loop half of the disconnect barrier over a REAL passive core:
// recvLoop's own read-error tcpDown call (transport_recv.go: t.tcpDown(g.gen, err, hsms.CauseIOError)), replayed for a generation that has already ended.
//
// The hsms package's TestReconnect_AbandonedGenerationTCPDownCannotDropSuccessor already covers this exact scenario end-to-end with the mock transport,
// and role (active/passive) makes no difference to the barrier itself:
// hsms.connection reads IsActive() in exactly one place (Open's Gap-1 cold-retry branch), never in the generation match.
// So a passive-flavored duplicate of that test at the hsms/mock level would exercise nothing new.
// The only place "passive" is a genuinely distinct code path is startPassive's accept loop and the real hsmsss dispatch it and recvLoop share —
// which is what this test, unlike the mock one, actually drives: a real passive core, accepting a real TCP peer, whose stale report crosses the same t.rt.(genRuntime) assertion TestReconnect_AbandonedGenerationLinktestFailCannotDropSuccessor exercises for the linktest call site.
//
// Teeth: same manual check as TestReconnect_AbandonedGenerationLinktestFailCannotDropSuccessor's doc.
func TestReconnect_AbandonedGenerationPassiveRecvTCPDownCannotDropSuccessor(t *testing.T) {
	t.Parallel()

	assertAbandonedGenerationTCPDownCannotDropSuccessor(t, io.EOF, hsms.CauseIOError)
}
