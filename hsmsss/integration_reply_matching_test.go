package hsmsss

// integration_reply_matching_test.go is Task 6's REQUIRED control-transaction guard (E37 §9.4.1
// reply matching, hsms.WithStrictReplyMatching).
// It proves a real Select handshake and linktest
// round trip still work under both postures — the failure mode a broken control exemption would
// produce here is an end-to-end failure to reach Selected (strict mode) or a polluted
// ReplyMismatchCount (default mode), exactly as it would against real equipment.
//
// TEETH (recorded in the Task 6 commit body — verified empirically, not merely asserted):
// replyRegistry.route compares a candidate only when BOTH legs hold — the entry was registered
// for a data primary (isData) AND the routed result is itself a *DataMessage.
// These two tests
// bite ONLY when BOTH legs are gone at once (an unconditional comparison, where a result that
// cannot supply a stream/function — a *ControlMessage, or a field-less RejectError — is itself
// treated as a mismatch rather than passing through uncompared): under that revert,
// TestReplyMatching_StrictMode_SelectReachesSelected fails (every Select.rsp is unconditionally
// a mismatch, so every Select transaction misses under strict mode, CommitSelected never runs,
// and waitSelected below times out — the same NotSelected -> NotConnected loop shape as the
// v2.0.1 field bug this task fixes), and TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch
// fails (ReplyMismatchCount climbs on every successful Linktest.rsp instead of staying 0).
//
// Neither leg alone is sufficient to make these two tests bite, so removing either leg alone is
// NOT teeth-checked here:
//   - Leg 1 (isData) alone: with leg 2 (the *DataMessage type assertion) still gating the
//     comparison, a Select.rsp/Linktest.rsp — which always decodes to *ControlMessage
//     (hsms/decode.go), never *DataMessage — still fails the type assertion and is excluded
//     regardless of isData's value, so these two tests keep passing.
//     Empirically confirmed: removing isData alone leaves both tests green.
//   - Leg 2 alone: with leg 1 (isData) still gating, a control-registered entry (isData=false)
//     never reaches the type-assertion code at all, so these two tests keep passing here too.
//
// isData IS independently load-bearing, just not on this file's two tests: it guards a genuine
// DATA secondary whose System Bytes collide with an open CONTROL transaction (e.g. Select.req
// colliding with an in-flight data reply's System Bytes) — a case where leg 2 passes (the result
// really is a *DataMessage) and only isData keeps it uncompared.
// That door is driven by
// hsms/reply_matching_test.go's TestReplyMatching_ControlRegistration_DataSecondaryDeliveredUncompared
// and hsms/reply_registry_test.go's TestReplyRegistry_ControlExemption_RegistrationLeg, both of
// which fail when isData is removed and pass when it is restored.

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestReplyMatching_StrictMode_SelectReachesSelected proves WithStrictReplyMatching does not
// break the Select handshake: a real active+passive pair with strict mode enabled on BOTH sides
// must still reach Selected, and ReplyMismatchCount must stay 0 on both — Select.rsp registers
// with isData false (see sendWaitReply/connection_send.go) and so is never a compared candidate.
func TestReplyMatching_StrictMode_SelectReachesSelected(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	strictOpt := []Option{WithConnectionOption(hsms.WithStrictReplyMatching(true))}
	passive, active := newEndpointPair(t, strictOpt...)

	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	// waitState's own bounded require.Eventually is the failure mode a broken control exemption
	// produces: Select.rsp misclassified as a mismatch never commits Selected, so this times out.
	waitSelected(t, active)
	waitSelected(t, passive)

	require.Equal(t, uint64(0), active.conn.Metrics().ReplyMismatchCount(),
		"a Select.rsp must never be compared under strict mode")
	require.Equal(t, uint64(0), passive.conn.Metrics().ReplyMismatchCount(),
		"the passive's own Select.rsp accounting must also stay clean")
}

// TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch proves a normal linktest round trip
// under the (unmodified) default posture never feeds ReplyMismatchCount: Linktest.rsp registers
// with isData false exactly like Select.rsp, so it is never a compared candidate even though the
// default observes rather than enforces.
func TestReplyMatching_DefaultMode_LinktestRoundTripZeroMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	port := freeLoopbackPort(t)

	passive := newEndpoint(t, port, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	active := newEndpoint(t, port, true, []Option{
		WithConnectionOption(hsms.WithLinktestInterval(50 * time.Millisecond)),
		WithConnectionOption(hsms.WithT6(2 * time.Second)),
	})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	m := controlMetrics(t, active)
	require.Eventually(t, func() bool {
		return m.LinktestRecvCount() >= 1
	}, 5*time.Second, 20*time.Millisecond, "the auto-linktest must complete at least one round trip")

	require.Equal(t, uint64(0), m.LinktestErrCount(), "the round trip must succeed, not fail on T6")
	require.Equal(t, uint64(0), active.conn.Metrics().ReplyMismatchCount(),
		"a Linktest.rsp must never feed ReplyMismatchCount")
}
