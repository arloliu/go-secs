package hsmsss

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// TestAssertCleanShutdown_HappyPath verifies the leak harness:
//
//   - Compiles and runs on a vanilla open → W-bit exchange → close cycle.
//   - All five primary internal counters return to zero post-close on a
//     correct build of hsmsss.
//
// This is the **positive smoke test** for hsmsss.assertCleanShutdown. The
// **verifiable-success criterion** for the harness from chaos plan §3 P0.4
// is that reverting any of the 1.16.1 leak-fix commits — 0eec4b2, 3d2e3ad,
// 075d6d3, b1d9cb4, f604180 — must trip ≥ 1 of the helper's assertions when
// the corresponding scenario test is run. That verification is performed
// out-of-tree via git revert + this happy path (and the targeted scenario
// tests already in conn_test.go), not by introducing a synthetic leak here.
//
// In particular:
//
//   - 075d6d3 (DataMsgInflightCount underflow) is exercised by the W-bit
//     exchange below: reverting it leaves the gauge at -1 and
//     assertCleanShutdown's DataMsgInflightCount check fires.
//   - 0eec4b2, 3d2e3ad, b1d9cb4, f604180 are covered by their pre-existing
//     scenario tests (TestConnection_SendMsg_PeerReject_NoReplyChanLeak,
//     TestHSMS_SelectCloseRace_NoPanicNoZombie,
//     TestConnection_CloseDrainsStrandedSenderFrame) — adding
//     assertCleanShutdown there would be redundant with their dedicated
//     per-counter assertions, so the harness is intentionally additive
//     rather than replacing.
func TestAssertCleanShutdown_HappyPath(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	hostComm := newTestComm(ctx, t, port, true, true,
		WithCloseConnTimeout(2*time.Second), WithAutoLinktest(false))
	eqpComm := newTestComm(ctx, t, port, false, false,
		WithCloseConnTimeout(2*time.Second), WithAutoLinktest(false))

	require.NoError(eqpComm.open(true))
	require.NoError(hostComm.open(true))

	require.NoError(hostComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))
	require.NoError(eqpComm.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Drive at least one W-bit exchange so DataMsgInflightCount is
	// exercised through the inc/dec cycle that 075d6d3 fixes.
	reply, err := hostComm.session.SendDataMessage(1, 1, true, secs2.A("ping"))
	require.NoError(err)
	require.NotNil(reply)
	if reply != nil {
		reply.Free()
	}

	require.NoError(hostComm.close())
	require.NoError(eqpComm.close())

	// Primary leak gate — all five internal counters must be zero.
	assertCleanShutdown(t, hostComm.conn)
	assertCleanShutdown(t, eqpComm.conn)
}
