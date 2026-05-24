package hsmsssintegration

import (
	"context"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/hsmsss"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// P0.2 scenarios from chaos plan §3: cover faults the existing ChaosProxy
// cannot express (partial length-header drip, header-byte substitution,
// length-field corruption) and verify each fix that landed in the 1.16.1
// backlog (`b4b9938`, `f110c68`, `08a64b3`).

// TestChaos_PartialLengthHeader_T8 — verifies the partial-length T8 path
// (SEMI E37 §4.1.32 / §9.2.3.1, code in messageReader.ReadMessage Phase 1).
// Reverting `b4b9938` (the per-byte T8 enforcement across the length
// header) leaves a partial 1-byte length sitting in the reader without a
// timeout and the connection hangs — this test would then fail by timing
// out on the NotConnected state event.
func TestChaos_PartialLengthHeader_T8(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newByteChaosProxy(t, equipPort)
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithT3Timeout(5 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// Arm: the next frame going equip → host gets only 1 byte of length,
	// then the proxy holds for > T8 and closes. Host's reader sees a
	// partial length header and T8 must fire on the next-byte wait.
	proxy.QueuePartialLengthHold(true, 1, 3*time.Second)

	// Trigger: send a W-bit S1F1 — the equipment's echo handler will reply
	// S1F2, which is the frame the proxy mutates on the way back.
	_, _ = host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("partial-length"))

	// Verifiable success: host transitions to NotConnected after T8 fires.
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

// TestChaos_PartialPayload_T8 — verifies the mid-payload T8 path. Existing
// TestChaos_TruncatedDataMessage_T8Timeout covers this with the frame
// proxy, but only because the frame proxy's Truncate writes the length
// header plus the first 5 bytes of payload. This test exercises the same
// path via the byte-level proxy as part of the P0.2 surface — kept as a
// thin parity test so the byte-level proxy itself is exercised on this
// pathway and any future divergence between the two proxies is caught.
func TestChaos_PartialPayload_T8_ByteProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newByteChaosProxy(t, equipPort)
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithT3Timeout(5 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// Arm: next equip→host frame gets full length + 5 bytes of payload,
	// then hold > T8.
	proxy.QueuePartialPayloadHold(true, 5, 3*time.Second)

	_, _ = host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("partial-payload"))

	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}

// TestChaos_UnsupportedPType_RejectAndContinue — substitute PType=99 in an
// inbound S1F2 reply; verify host emits Reject.req(reason=2) per
// SEMI E37 §7.10.3 and the connection survives so a follow-up exchange
// succeeds. Reverting `f110c68` (the PType/SType Reject.req path in
// messageReader Phase 3.5) makes the host treat the unsupported PType as
// a fatal decode error and the connection drops — at which point the
// follow-up SendDataMessage fails / the state goes NotConnected.
func TestChaos_UnsupportedPType_RejectAndContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newByteChaosProxy(t, equipPort)
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(2 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// Arm: next equip→host frame gets PType=99 (unsupported).
	proxy.QueueSubstitutePType(true, 99)

	// Trigger: W-bit S1F1. Equip replies S1F2 with PType=0; proxy mutates to
	// PType=99 on the way back. The first SendDataMessage will fail via
	// T3 timeout (reply was rejected by host's receiver instead of being
	// delivered to the W-bit sender), but the connection must survive.
	_, _ = host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("ptype-test"))

	// Connection must survive Reject.req — verified by the follow-up
	// exchange succeeding. If receiverTask had dropped the connection
	// instead of sending Reject.req, the follow-up SendDataMessage would
	// fail with ErrConnClosed (or block until T3 expiry).

	reply, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("after-reject"))
	require.NoError(t, err, "follow-up S1F1 must succeed after Reject.req")
	require.NotNil(t, reply)
	if reply != nil {
		reply.Free()
	}

	// Host must have emitted a Reject.req(reason=2) in the client→target direction.
	require.Eventually(t, func() bool {
		for _, reason := range proxy.ObservedRejects(false) {
			if reason == byte(hsms.RejectPTypeNotSupported) {
				return true
			}
		}

		return false
	}, 2*time.Second, 10*time.Millisecond,
		"host did not emit Reject.req(reason=PTypeNotSupported); observed=%v",
		proxy.ObservedRejects(false))
}

// TestChaos_UndefinedSType_RejectAndContinue — substitute SType=10 (not in
// the defined SType set) in an inbound frame; verify Reject.req(reason=1)
// and connection survival. Reverting `f110c68` breaks this too.
func TestChaos_UndefinedSType_RejectAndContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newByteChaosProxy(t, equipPort)
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT3Timeout(2 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// Arm: next equip→host frame gets SType=10 (undefined).
	proxy.QueueSubstituteSType(true, 10)

	_, _ = host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("stype-test"))

	// Connection must survive Reject.req — proven by the follow-up below.
	reply, err := host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("after-reject"))
	require.NoError(t, err, "follow-up S1F1 must succeed after Reject.req")
	require.NotNil(t, reply)
	if reply != nil {
		reply.Free()
	}

	require.Eventually(t, func() bool {
		for _, reason := range proxy.ObservedRejects(false) {
			if reason == byte(hsms.RejectSTypeNotSupported) {
				return true
			}
		}

		return false
	}, 2*time.Second, 10*time.Millisecond,
		"host did not emit Reject.req(reason=STypeNotSupported); observed=%v",
		proxy.ObservedRejects(false))
}

// TestChaos_MalformedLength_TooLarge — inject a length field claiming
// payload size > secs2.MaxByteSize; verify host rejects the frame and
// transitions to NotConnected without an allocation spike. Reverting
// `08a64b3` (the payload-size classification / length validation) would
// allow the receiver to attempt to allocate the absurd buffer.
func TestChaos_MalformedLength_TooLarge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	equipPort := getFreePort(t)
	equip := newEndpoint(t, ctx, equipPort, true, false, nil, echoHandler)
	defer closeEndpoint(t, equip)
	require.NoError(t, equip.conn.Open(false))

	proxy := newByteChaosProxy(t, equipPort)
	proxy.Start(t)
	defer proxy.Stop()

	hostOpts := []hsmsss.ConnOption{
		hsmsss.WithT8Timeout(1 * time.Second),
		hsmsss.WithT3Timeout(2 * time.Second),
	}
	host := newEndpoint(t, ctx, proxy.Port(), false, true, hostOpts)
	defer closeEndpoint(t, host)
	require.NoError(t, host.conn.Open(false))

	requireStateEvent(t, host.selectedCh, hsms.SelectedState)
	requireStateEvent(t, equip.selectedCh, hsms.SelectedState)

	// Arm: next equip→host frame gets length overridden to MaxByteSize+1.
	proxy.QueueOverrideLength(true, uint32(secs2.MaxByteSize)+1)

	_, _ = host.session.SendDataMessage(1, 1, true, secs2.NewASCIIItem("malformed-len"))

	// Host must reject the frame and tear the connection down.
	requireStateEvent(t, host.selectedCh, hsms.NotConnectedState)
}
