package hsmsss

// transport_control_test.go — pin tests for the two control-message deviations reclassified in
// docs/specs/e37-1-hsms-ss-conformance-audit.md's "Deviations reviewed and accepted" section:
// answering an inbound Deselect.req despite E37.1 §7.3's "Deselect shall not be used", and the
// Select responder accepting (and echoing) a non-conformant inbound control SessionID.
//
// The file lives in package hsmsss (not hsmsss_test) because the Deselect test drives the
// unexported *transport / recRT doubles shared with transport_procedures_test.go; the SessionID
// test reuses the loopback-TCP helpers from integration_control_session_id_test.go, which live in
// the same package for the identical reason.

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/stretchr/testify/require"
)

// TestDeselect_AnsweredDespiteE371Prohibition pins the deviation named in
// docs/specs/e37-1-hsms-ss-conformance-audit.md, "Deviations reviewed and accepted", §7.3:
// E37.1 §7.3 states "Deselect shall not be used" for HSMS-SS, yet handleDeselectReq answers an
// inbound Deselect.req received while Selected with status 0 (success) and transitions
// Selected -> NotSelected, rather than refusing it.
//
// This is the deviation's named pin — see the rationale comment at handleDeselectReq for why it
// is kept, and TestDeselect_WhileSelectedRepliesSuccessAndTransitions /
// TestDeselect_WhileNotSelectedRepliesFailureNoTransition (transport_procedures_test.go) for the
// mechanical coverage of both status branches and the auto-linktest stop.
func TestDeselect_AnsweredDespiteE371Prohibition(t *testing.T) {
	t.Parallel()

	rt := newRecRT()
	rt.setState(hsms.SelectedState)

	tr := newLinktestTransport(t, rt, t.Context())

	req := hsms.NewDeselectReq(0xFFFF, rt.NextSystemBytes())
	tr.handleDeselectReq(tr.wg, req)

	got := rt.lastSent()
	require.NotNil(t, got, "an inbound Deselect.req while Selected must be answered, not refused")
	require.Equal(t, hsms.DeselectRspType, got.Type(), "the answer must be a Deselect.rsp")
	require.Equal(t, byte(hsms.DeselectStatusSuccess), got.HeaderBytes()[3],
		"status must be 0 (success) despite E37.1 §7.3's prohibition on Deselect")
	require.Equal(t, hsms.NotSelectedState, rt.State(),
		"answering must still transition Selected -> NotSelected (rt.SelectLost)")
}

// TestSelectRsp_EchoesNonConformantSessionID pins BOTH halves of the "§8.1 echo" deviation named
// in docs/specs/e37-1-hsms-ss-conformance-audit.md, "Deviations reviewed and accepted":
//
// E37 §8.3.7.1 requires a Select.rsp to echo the Select.req's SessionID.
// E37.1 §8.1 requires every HSMS-SS control message — Select.req included — to carry 0xFFFF.
// The two cannot both be satisfied once a peer sends a non-0xFFFF Select.req: E37.1 says the
// request itself should never have carried anything else, while E37 says the response must carry
// back whatever the request carried.
// We follow E37, which binds the response to the request, rather than second-guessing the
// request's own conformance.
//
// TestPassive_SelectRspMirrorsRequestSessionID (integration_control_session_id_test.go) already
// pins the echo.
// This test pins the SAME resolution from the other side, with the acceptance half
// asserted directly (status 0, not merely an eventual state) rather than incidentally:
// the Select responder does not refuse a non-conformant SessionID either — it commits
// NotSelected -> Selected exactly as it would for a conformant 0xFFFF request.
func TestSelectRsp_EchoesNonConformantSessionID(t *testing.T) {
	t.Parallel()

	const probeSessionID uint16 = 0x1234 // non-conformant: neither 0xFFFF nor testDeviceID

	port := freeLoopbackPort(t)
	conn := newPassiveConn(t, port, hsms.WithSessionID(testDeviceID), hsms.WithLinktestInterval(0))
	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	peer := dialPassive(t, port)
	t.Cleanup(func() { _ = peer.Close() })

	_, err := peer.Write(selectReqFrameWithSessionID(probeSessionID, [4]byte{0x11, 0x22, 0x33, 0x44}))
	require.NoError(t, err)

	rspPayload, err := peerReadFrame(peer, 5*time.Second)
	require.NoError(t, err, "peer must read our Select.rsp")
	require.Equal(t, byte(hsms.SelectRspType), rspPayload[5])

	// Half 1: ACCEPT — status 0 (Communication Established), not refused for the bad SessionID.
	require.Equal(t, byte(hsms.SelectStatusSuccess), rspPayload[3],
		"a non-conformant SessionID must not cause the Select to be refused")
	require.Eventually(t, func() bool { return conn.State() == hsms.SelectedState },
		5*time.Second, 5*time.Millisecond,
		"the responder must commit Selected despite the non-conformant SessionID")

	// Half 2: ECHO — the response mirrors the request's SessionID (E37 §8.3.7.1) instead of
	// substituting 0xFFFF (E37.1 §8.1) or the configured device ID.
	require.Equal(t, probeSessionID, frameSessionID(t, rspPayload),
		"Select.rsp must echo the Select.req SessionID even though the request itself violates E37.1 §8.1")
}

// genRecRT is a recRT that also offers the generation-naming capability, recording the generation
// each disconnect is reported under.
// It is how a test observes WHICH generation this package speaks for, without reaching into hsms.
type genRecRT struct {
	*recRT

	mu          sync.Mutex
	liveGen     uint64
	reportedGen []uint64
	// refuseTCPUp, when true, makes TCPUpFromGeneration report a refusal
	// (as the real core does for a generation that has already ended) instead of recording the socket —
	// the refused-TCP-up teeth tests use it to drive startActive/acceptLoop's refusal branch deterministically,
	// without needing a real generation-gate race.
	refuseTCPUp bool
}

func newGenRecRT(liveGen uint64) *genRecRT {
	return &genRecRT{recRT: newRecRT(), liveGen: liveGen}
}

func (m *genRecRT) CurrentGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.liveGen
}

func (m *genRecRT) setLiveGeneration(gen uint64) {
	m.mu.Lock()
	m.liveGen = gen
	m.mu.Unlock()
}

func (m *genRecRT) setRefuseTCPUp(refuse bool) {
	m.mu.Lock()
	m.refuseTCPUp = refuse
	m.mu.Unlock()
}

func (m *genRecRT) TCPDownFromGeneration(gen uint64, cause error, _ hsms.TransitionCause) {
	m.mu.Lock()
	m.reportedGen = append(m.reportedGen, gen)
	m.mu.Unlock()

	m.TCPDown(cause)
}

func (m *genRecRT) T7ExpiredFromGeneration(gen uint64) {
	m.mu.Lock()
	m.reportedGen = append(m.reportedGen, gen)
	m.mu.Unlock()

	m.T7Expired()
}

func (m *genRecRT) TCPUpFromGeneration(gen uint64, conn net.Conn) bool {
	m.mu.Lock()
	m.reportedGen = append(m.reportedGen, gen)
	refuse := m.refuseTCPUp
	m.mu.Unlock()

	if refuse {
		return false
	}

	m.TCPUp(conn)

	return true
}

func (m *genRecRT) CommitSelectedFromGeneration(gen uint64) bool {
	m.mu.Lock()
	m.reportedGen = append(m.reportedGen, gen)
	m.mu.Unlock()

	return m.CommitSelected()
}

func (m *genRecRT) SelectLostFromGeneration(gen uint64) {
	m.mu.Lock()
	m.reportedGen = append(m.reportedGen, gen)
	m.mu.Unlock()

	m.SelectLost()
}

// A genRecRT that stops satisfying genRuntime would not fail to compile.
// The transport reaches the capability by type assertion,
// so it would silently fall back to the generation-unaware path,
// and every generation assertion below would keep passing while testing nothing.
// This assertion is what turns that into a build failure.
var _ genRuntime = (*genRecRT)(nil)

func (m *genRecRT) reportedGenerations() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]uint64(nil), m.reportedGen...)
}

// TestSeparate_ReportsItsOwnGeneration is the hsmsss half of the stale-disconnect barrier: a peer
// Separate must be reported under the generation of the recv goroutine that READ it, never under
// whichever generation happens to be live when the report lands.
//
// The generation here has already been superseded (the runtime's live generation moved on while the
// recv goroutine was descheduled) — exactly the state an abandoned straggler resumes in. The
// transport must still name its own bundle's generation, which is what lets the core discard the
// report instead of dropping the successor's link.
//
// Teeth: report the runtime's current generation (or 0) instead of g.gen → the reported value
// follows the live generation and the core can no longer tell the two apart.
func TestSeparate_ReportsItsOwnGeneration(t *testing.T) {
	t.Parallel()

	rt := newGenRecRT(1)
	rt.setState(hsms.NotSelectedState)

	ctx := t.Context()
	tr := newLinktestTransport(t, rt.recRT, ctx)
	tr.rt = rt // re-bind to the capability-offering runtime

	rt.setLiveGeneration(2) // a successor generation is live; this recv goroutine belongs to gen 1

	keepReading := tr.handleSeparateReq(ctx, &genWG{gen: 1})

	require.False(t, keepReading, "Separate always ends the recv loop")
	require.Equal(t, []uint64{1}, rt.reportedGenerations(),
		"the Separate must be reported under the generation that read it, not the live one")
}

// TestSelectCommits_ReportTheirOwnGeneration is the hsmsss half of the synchronous-commit barrier,
// and the mirror of TestSeparate_ReportsItsOwnGeneration for the two commits that change the FSM
// without going through the disconnect path.
//
// Both run on the recv goroutine, which a bounded Stop can abandon, so each must name the
// generation of the bundle it was spawned for — never whichever generation the runtime reports as
// live when the frame is finally processed. Here the runtime's live generation has already moved on
// to 2, exactly as it has for a straggler, while the bundle still says 1.
//
// Teeth: pass t.currentGeneration() (or 0) instead of g.gen at either call site → the reported
// value follows the live generation and the core loses the only thing it can discriminate on.
func TestSelectCommits_ReportTheirOwnGeneration(t *testing.T) {
	t.Parallel()

	t.Run("select responder", func(t *testing.T) {
		t.Parallel()

		rt := newGenRecRT(1)
		rt.setState(hsms.NotSelectedState)

		tr := newLinktestTransport(t, rt.recRT, t.Context())
		tr.rt = rt // re-bind to the capability-offering runtime

		rt.setLiveGeneration(2)

		tr.handleSelectReq(&genWG{gen: 1}, hsms.NewSelectReq(hsms.ControlSessionID, rt.NextSystemBytes()))

		require.Equal(t, []uint64{1}, rt.reportedGenerations(),
			"the Select commit must be made on behalf of the generation that read the request")
	})

	t.Run("deselect responder", func(t *testing.T) {
		t.Parallel()

		rt := newGenRecRT(1)
		rt.setState(hsms.SelectedState)

		tr := newLinktestTransport(t, rt.recRT, t.Context())
		tr.rt = rt

		rt.setLiveGeneration(2)

		tr.handleDeselectReq(&genWG{gen: 1}, hsms.NewDeselectReq(hsms.ControlSessionID, rt.NextSystemBytes()))

		require.Equal(t, []uint64{1}, rt.reportedGenerations(),
			"the Select-lost commit must be made on behalf of the generation that read the request")
	})
}

// TestGenRuntime_IsSatisfiedByTheRealCore is the one assertion nothing else in this package makes,
// and the whole generation barrier — the disconnect half as much as the commit half — rests on it.
//
// The capability is reached by a RUNTIME type assertion on t.rt, and a failed assertion does not
// error: it falls back to the generation-unaware path. So if the real core ever stops satisfying
// genRuntime — a renamed method, a signature drift, one more method added to the interface here but
// not there — every producer in this package silently reports gen 0, the core skips every match, and
// the entire suite still passes. The mock-driven tests above cannot see that: they assert against
// genRecRT, which satisfies the interface by construction.
//
// So this drives the REAL core, built exactly as New builds it, and checks both halves: that the
// core satisfies the interface at all, and that a live generation actually reports a non-zero
// identity through it (which additionally covers t.rt being bound and cur being published before
// Start reads it).
//
// Teeth: add a method to genRuntime that *hsms.connection does not have → this fails while
// everything else stays green.
func TestGenRuntime_IsSatisfiedByTheRealCore(t *testing.T) {
	t.Parallel()

	conn, tr := newPassiveConnTr(t, freeLoopbackPort(t))

	_, ok := conn.(genRuntime)
	require.True(t, ok, "the real hsms core must satisfy genRuntime, or every generation guard silently turns off")

	require.NoError(t, conn.Open(t.Context(), hsms.OpenBackground))
	t.Cleanup(func() { _ = conn.Close() })

	require.NotZero(t, tr.currentGeneration(),
		"a live generation must report a non-zero identity through the capability")

	// t.wg is read and written ONLY under startGate (ArmStart installs, Start captures, Stop captures),
	// so read it the same way rather than reaching straight for the field:
	// unsynchronized it is safe only by accident of the current spawn order,
	// and one behavior change away from a race report.
	tr.startGate.RLock()
	stamped := tr.wg.gen
	tr.startGate.RUnlock()

	require.Equal(t, tr.currentGeneration(), stamped,
		"Start must stamp that identity on the bundle every goroutine of this generation carries")
}
