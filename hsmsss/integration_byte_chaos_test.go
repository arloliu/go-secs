package hsmsss

// byte_chaos_scenarios_test.go — v2 port of the v1 tests/hsmsss_integration/byte_chaos_scenarios_test.go
// sub-frame byte-fault matrix, re-pointed onto the v2 public surface (New / Open(ctx, mode) /
// hsms.Connection / SendDataMessageAsync/SendDataMessage(ctx, ...) / SECS2Endpoint handlers).
//
// This is the FIRST real exercise of ByteChaosProxy (ported in T28 — see byte_chaos_proxy_test.go —
// but UNVERIFIED until now). ByteChaosProxy expresses faults the filter-driven ChaosProxy cannot:
// partial length-header drip, partial-payload drip, PType/SType header-byte substitution, and
// length-field override. Each test drives one branch of the v2 reader / dispatcher:
//
//   - partial length / partial payload -> readFrame's per-gap T8 deadline (transport.go readN/readFrame,
//     §9.2.3.1 / J1): once the first byte of a frame is read, EVERY inter-byte gap (incl. the length
//     header) is T8-bounded; a held partial frame trips T8 -> TCPDown -> NotConnected.
//   - PType!=0 / undefined SType -> dispatchFrame's J3 keep-link Reject.req path (§7.10.3): the frame
//     is answered with a Reject.req (reason 2 / reason 1) and the link STAYS Selected. The corrupted
//     secondary is consumed by the Reject path and never routed to the W-bit sender, so that first send
//     T3-times out; a follow-up exchange then succeeds, proving link survival.
//   - length override > secs2.MaxByteSize -> readFrame's validate-before-alloc guard (J2): the oversized
//     length is rejected WITHOUT allocating the body buffer -> TCPDown -> NotConnected.
//
// Wiring topology (active -> byte-proxy -> passive): the passive listens on portP; the ByteChaosProxy
// targets portP and binds its own port; the active dials bp.Port(). In the proxy's fault API,
// toClient==true is the target->client (passive->active) direction, false is client->target
// (active->passive). A Reject.req emitted BY THE ACTIVE therefore travels client->target, so it is
// observed via bp.ObservedRejects(false).
//
// v2 adaptation (same as chaos_scenarios_test.go): auto-reconnect is ALWAYS ON, so every "->
// NotConnected" scenario asserts the NotConnected TRANSITION via waitNotConnectedEvent (from
// chaos_scenarios_test.go) rather than an instantaneous conn.State() poll. The reconnect backoff is
// T5-floored (newEndpoint base T5=200ms), so NotConnected dwells long enough for the event to fire.
//
// The file is package hsmsss (white-box) so it reuses the shared harness (newEndpoint / waitSelected /
// closeEndpoint / drainStateCh / echoHandler / waitNotConnectedEvent) plus ByteChaosProxy. All
// readiness waits are event- or State()-driven (never time.Sleep-to-sync) and run under -race.

import (
	"slices"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 1. TestChaos_PartialLengthHeader_T8
//
// After Selected the active fires an S1F1; the passive echoHandler replies S1F2, and the byte-proxy
// writes only 1 of the 4 length-prefix bytes of that passive->active reply, then holds 3s (> T8) and
// closes. The active's reader has read one byte (started==true), so T8 governs the next-byte wait across
// the length header (J1) — the held gap trips T8 (1s) -> TCPDown -> NotConnected, well before the 3s close.
//
// Coverage note: this confirms a partial length header causes a NON-immediate disconnect consistent with
// T8 (it excludes a fast/immediate teardown). It does NOT isolate T8 from the proxy's eventual 3s close by
// assertion alone — with T8 disabled the reader would instead hit EOF at the 3s close, still inside
// waitNotConnectedEvent's 10s window, so the test would still pass. Pinning T8 exactly would need the proxy
// to hold past the 10s event window (regresses cleanup) or a sub-3s elapsed-time bound (CI-flake risk);
// deferred as a known teeth-precision limitation.
// ---------------------------------------------------------------------------
func TestChaos_PartialLengthHeader_T8(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	bp := newByteChaosProxy(t, portP)
	bp.Start(t)
	t.Cleanup(bp.Stop)

	// Short T8 on the active so the partial length header surfaces fast.
	active := newEndpoint(t, bp.Port(), true, []Option{WithConnectionOption(hsms.WithT8(1 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	// Arm: the next passive->active frame gets only 1 length byte, then the proxy holds 3s (> T8).
	bp.QueuePartialLengthHold(true, 1, 3*time.Second)

	// Trigger: an odd-function primary makes echoHandler reply S1F2 — the frame the proxy cripples.
	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("partial-length"))

	// The partial length header stalls the active's reader mid-length -> T8 -> TCPDown -> NotConnected.
	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 2. TestChaos_PartialPayload_T8_ByteProxy
//
// Same as #1 but the proxy writes the FULL 4-byte length prefix plus only the first 5 payload bytes of
// the passive->active reply, then holds > T8. The active's reader allocates the body buffer, reads 5
// bytes, then T8 governs the wait for the rest — the held gap trips T8 -> NotConnected. This drives the
// mid-payload T8 path through the byte-level proxy (parity with the frame-proxy TruncatedDataMessage).
//
// Coverage note: same teeth-precision limitation as TestChaos_PartialLengthHeader_T8 — the 3s hold-then-
// close excludes a fast teardown but does not isolate T8 from the eventual EOF-at-close within the 10s
// event window. Deferred.
// ---------------------------------------------------------------------------
func TestChaos_PartialPayload_T8_ByteProxy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	bp := newByteChaosProxy(t, portP)
	bp.Start(t)
	t.Cleanup(bp.Stop)

	active := newEndpoint(t, bp.Port(), true, []Option{WithConnectionOption(hsms.WithT8(1 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	// Arm: next passive->active frame gets full length + 5 payload bytes, then hold > T8.
	bp.QueuePartialPayloadHold(true, 5, 3*time.Second)

	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("partial-payload"))

	waitNotConnectedEvent(t, active)
}

// ---------------------------------------------------------------------------
// 3. TestChaos_UnsupportedPType_RejectAndContinue
//
// The proxy substitutes PType=99 into an inbound (passive->active) S1F2 reply. dispatchFrame's J3 path
// (§7.10.3) answers a non-zero PType with a Reject.req(reason 2 = PTypeNotSupported) and KEEPS the link
// Selected. Because the corrupted secondary is consumed by the Reject path (not routed to the W-bit
// waiter), the FIRST send T3-times out; the link survives, so a follow-up S1F1 completes normally.
// TEETH: if the reader treated the unsupported PType as a fatal decode error the link would drop and
// the follow-up send would fail (or the Reject.req would never be observed).
// ---------------------------------------------------------------------------
func TestChaos_UnsupportedPType_RejectAndContinue(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	bp := newByteChaosProxy(t, portP)
	bp.Start(t)
	t.Cleanup(bp.Stop)

	// Short T3 so the first (rejected-reply) send times out fast.
	active := newEndpoint(t, bp.Port(), true, []Option{WithConnectionOption(hsms.WithT3(1 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// Arm: next passive->active frame gets PType=99 (unsupported).
	bp.QueueSubstitutePType(true, 99)

	// First W-bit send: the echo reply is PType-corrupted -> consumed by the Reject path, never routed
	// to this waiter -> T3 timeout. A T3 timeout is transaction-level, not link-level.
	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("ptype-test"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout, "PType-corrupted reply must surface as ErrT3Timeout")
	require.Nil(t, reply)
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a Reject.req must keep the link Selected")

	// Follow-up S1F1: the one-shot fault is spent, so this reply is forwarded intact and correlates.
	reply2, err2 := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("after-reject"))
	require.NoError(t, err2, "follow-up S1F1 must succeed after Reject.req")
	require.NotNil(t, reply2)

	// The active must have emitted a Reject.req(reason=PTypeNotSupported) in the active->passive direction.
	require.Eventually(t, func() bool {
		return slices.Contains(bp.ObservedRejects(false), byte(hsms.RejectPTypeNotSupported))
	}, 2*time.Second, 10*time.Millisecond,
		"active did not emit Reject.req(reason=PTypeNotSupported); observed=%v", bp.ObservedRejects(false))
}

// ---------------------------------------------------------------------------
// 4. TestChaos_UndefinedSType_RejectAndContinue
//
// Same as #3 but the proxy substitutes SType=10 (undefined — not in the E37 SType set) into an inbound
// frame. dispatchFrame answers an undefined SType with a Reject.req(reason 1 = STypeNotSupported) and
// keeps the link. The corrupted secondary is again consumed by the Reject path, so the first send
// T3-times out; a follow-up exchange succeeds.
// ---------------------------------------------------------------------------
func TestChaos_UndefinedSType_RejectAndContinue(t *testing.T) {
	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	bp := newByteChaosProxy(t, portP)
	bp.Start(t)
	t.Cleanup(bp.Stop)

	active := newEndpoint(t, bp.Port(), true, []Option{WithConnectionOption(hsms.WithT3(1 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)

	// Arm: next passive->active frame gets SType=10 (undefined).
	bp.QueueSubstituteSType(true, 10)

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("stype-test"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout, "SType-corrupted reply must surface as ErrT3Timeout")
	require.Nil(t, reply)
	require.Equal(t, hsms.SelectedState, active.conn.State(), "a Reject.req must keep the link Selected")

	reply2, err2 := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("after-reject"))
	require.NoError(t, err2, "follow-up S1F1 must succeed after Reject.req")
	require.NotNil(t, reply2)

	require.Eventually(t, func() bool {
		return slices.Contains(bp.ObservedRejects(false), byte(hsms.RejectSTypeNotSupported))
	}, 2*time.Second, 10*time.Millisecond,
		"active did not emit Reject.req(reason=STypeNotSupported); observed=%v", bp.ObservedRejects(false))
}

// ---------------------------------------------------------------------------
// 5. TestChaos_MalformedLength_TooLarge
//
// The proxy overrides the 4-byte length prefix of an inbound frame to secs2.MaxByteSize+1 (and writes
// no body). readFrame's validate-before-alloc guard (J2) rejects any length > secs2.MaxByteSize BEFORE
// calling make() for the body, so the reader errors out without an allocation spike -> TCPDown ->
// NotConnected.
//
// Coverage note: the proxy closes the connection immediately after writing the oversized length (no hold),
// so this asserts only that the malformed length triggers a disconnect. It does NOT isolate the J2
// validate-before-alloc guard: with J2 disabled the reader would allocate MaxByteSize+1 (=16 MiB, benign,
// no OOM) then hit EOF at the proxy's close -> also a fast NotConnected -> the test would still pass. The
// observed ~ms disconnect comes from the proxy's close (EOF), not proof of J2. Pinning J2 needs a
// hold-open length override + a sub-T8 elapsed bound to separate J2's ~ms reject from the no-J2 EOF/T8
// path; deferred as a known teeth-precision limitation.
// ---------------------------------------------------------------------------
func TestChaos_MalformedLength_TooLarge(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	portP := freeLoopbackPort(t)

	passive := newEndpoint(t, portP, false, nil, echoHandler)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, passive)

	bp := newByteChaosProxy(t, portP)
	bp.Start(t)
	t.Cleanup(bp.Stop)

	active := newEndpoint(t, bp.Port(), true, []Option{WithConnectionOption(hsms.WithT8(1 * time.Second))})
	require.NoError(t, active.conn.Open(ctx, hsms.OpenBackground))
	defer closeEndpoint(t, active)

	waitSelected(t, active)
	waitSelected(t, passive)
	drainStateCh(active.states)

	// Arm: next passive->active frame gets its length overridden to MaxByteSize+1.
	bp.QueueOverrideLength(true, uint32(secs2.MaxByteSize)+1)

	_ = active.conn.SendDataMessageAsync(ctx, 1, 1, false, secs2.A("malformed-len"))

	// The oversized length is rejected before allocation -> the connection tears down.
	waitNotConnectedEvent(t, active)
}
