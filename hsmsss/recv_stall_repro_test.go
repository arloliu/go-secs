package hsmsss

// Tests around the reported failure: with an hsmsss session, while the target
// peer is disconnected the application keeps sending; the connection then loops
// "not-selected -> not-connected" repeatedly and never reaches Selected.
//
// ROOT CAUSE (now FIXED): the application sends non-wait-bit / response messages
// via session.SendMessageAsync. Two coupled library defects, both now fixed:
//
//	(1) sendMsgAsync had NO IsSelected gate, unlike sendMsg and sendMsgSync, so it
//	    enqueued data messages regardless of connection state. FIXED: sendMsgAsync
//	    now rejects data while !IsSelected (returns ErrNotSelectedState, no enqueue).
//	(2) When senderTask dequeued such a queued data message while NotSelected,
//	    sendMsgSync returned ErrNotSelectedState and processSendRequest treated it
//	    as fatal -> cancelSenderTask -> ToNotConnected. FIXED: that case is now a
//	    non-fatal drop (defense-in-depth for the residual check-then-enqueue race).
//
// REGRESSION GUARDS for the fix (assert the FIXED behavior):
//   - TestRecvStall_AsyncDataSendGatedWhileNotSelected — the async gate.
//   - TestRecvStall_QueuedDataNotSelectedIsNonFatal — the sender-task backstop.
//   - TestRecvStall_AsyncFloodDoesNotStarveSelect — end-to-end: an async flood
//     across connect no longer starves the Select handshake.
//
// CHARACTERIZATION of adjacent, still-open stall modes (NOT the reported incident;
// these assert current behavior and are not addressed by the async-gate fix):
//   - TestRecvStall_ControlBlockedByFullDataChannel (Candidate A, inbound stall):
//     a slow/stuck data-message consumer fills the inbound channel and parks the
//     single receiver goroutine, stalling all control-message processing.
//   - TestRecvStall_OutboundQueueFullOnHalfOpen (Candidate B, outbound stall):
//     during a half-open window (still Selected), data sends fill the sender queue
//     and a send returns ErrSendMsgTimeout.
//   - TestRecvStall_ParkedReceiverDelaysDisconnectDetection: a parked receiver
//     misses a graceful FIN.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// stallProbePeer is a raw-TCP HSMS-SS passive peer that gives the test
// byte-level control the real passive Connection abstracts away: it completes
// the Select handshake, can push an exact number of primary data frames to fill
// the host's inbound data channel, and can fire an independent Linktest.req
// probe to measure whether the host's single receiver goroutine is still able
// to process control traffic.
type stallProbePeer struct {
	t  *testing.T
	ln net.Listener

	mu   sync.Mutex
	conn net.Conn

	rcvBuf       int  // when > 0, shrink the accepted socket's SO_RCVBUF
	freeze       bool // when true, stop reading right after answering Select.req
	ignoreSelect bool // when true, read and discard Select.req without replying

	linktestRsp chan struct{}
	closed      chan struct{}
	stopOnce    sync.Once
}

func newStallProbePeer(t *testing.T, port int) *stallProbePeer {
	t.Helper()

	return startPeer(t, port, 0, false, false)
}

// newFreezePeer returns a peer that shrinks its receive buffer to rcvBuf bytes
// and stops reading immediately after completing the Select handshake. The host's
// writes then block almost at once — modeling a half-open link where the peer is
// gone but no FIN/RST has been delivered.
func newFreezePeer(t *testing.T, port int, rcvBuf int) *stallProbePeer {
	t.Helper()

	return startPeer(t, port, rcvBuf, true, false)
}

// newDeafSelectPeer returns a peer that accepts and drains the socket but never
// answers Select.req, holding an active host in NotSelected until its T6 expires.
func newDeafSelectPeer(t *testing.T, port int) *stallProbePeer {
	t.Helper()

	return startPeer(t, port, 0, false, true)
}

func startPeer(t *testing.T, port int, rcvBuf int, freeze bool, ignoreSelect bool) *stallProbePeer {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(testIP, strconv.Itoa(port)))
	require.NoError(t, err)

	p := &stallProbePeer{
		t:            t,
		ln:           ln,
		rcvBuf:       rcvBuf,
		freeze:       freeze,
		ignoreSelect: ignoreSelect,
		linktestRsp:  make(chan struct{}, 16),
		closed:       make(chan struct{}),
	}
	go p.serve()

	return p
}

func (p *stallProbePeer) serve() {
	conn, err := p.ln.Accept()
	if err != nil {
		return
	}

	if p.rcvBuf > 0 {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(p.rcvBuf)
		}
	}

	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()

	for {
		select {
		case <-p.closed:
			return
		default:
		}

		body, err := readHSMSFrame(conn, 200*time.Millisecond)
		if err != nil {
			var netErr net.Error
			if ok := asNetTimeout(err, &netErr); ok {
				continue // idle poll
			}

			return // EOF / reset / closed
		}

		msg, derr := hsms.DecodeMessage(uint32(len(body)), body) //nolint:gosec // test frame, small
		if derr != nil {
			continue
		}

		switch msg.Type() {
		case hsms.SelectReqType:
			if p.ignoreSelect {
				// Discard without replying: the host stays in NotSelected.
				continue
			}
			rsp, _ := hsms.NewSelectRsp(msg, hsms.SelectStatusSuccess)
			p.write(rsp.ToBytes())
			if p.freeze {
				// Stop reading: the connection stays open at the TCP layer, but
				// the peer never drains, so the host's send window fills.
				return
			}
		case hsms.LinkTestReqType:
			// Answer a host-initiated linktest so an enabled auto-linktest does
			// not trip the fail threshold during a test that does not want it to.
			rsp, _ := hsms.NewLinktestRsp(msg)
			p.write(rsp.ToBytes())
		case hsms.LinkTestRspType:
			select {
			case p.linktestRsp <- struct{}{}:
			default:
			}
		default:
		}
	}
}

// pushData writes n primary S1F1 data frames addressed to the host's session.
// W-bit is unset; primary (odd-function) messages route to Session.recvDataMsg
// regardless, which is the channel-filling path under test.
func (p *stallProbePeer) pushData(n int) {
	p.t.Helper()
	for range n {
		msg, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("flood"))
		require.NoError(p.t, err)
		p.write(msg.ToBytes())
		msg.Free()
	}
}

// probeLinktest fires one Linktest.req and reports whether the host answered
// with a Linktest.rsp within d. It drains any stale response first so repeated
// probes are independent.
func (p *stallProbePeer) probeLinktest(d time.Duration) bool {
	p.t.Helper()

	for {
		select {
		case <-p.linktestRsp:
			continue
		default:
		}

		break
	}

	req := hsms.NewLinktestReq(hsms.GenerateMsgSystemBytes())
	p.write(req.ToBytes())
	req.Free()

	select {
	case <-p.linktestRsp:
		return true
	case <-time.After(d):
		return false
	}
}

func (p *stallProbePeer) write(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil {
		return
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = p.conn.Write(b)
}

func (p *stallProbePeer) stop() {
	p.stopOnce.Do(func() {
		close(p.closed)
		p.mu.Lock()
		if p.conn != nil {
			_ = p.conn.Close()
		}
		p.mu.Unlock()
		_ = p.ln.Close()
	})
}

// readHSMSFrame reads one length-prefixed HSMS frame and returns the body
// (10-byte header + payload, i.e. the input DecodeMessage expects).
func readHSMSFrame(conn net.Conn, timeout time.Duration) ([]byte, error) {
	lenBuf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, err
	}

	n := binary.BigEndian.Uint32(lenBuf)
	body := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}

	return body, nil
}

func asNetTimeout(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() { //nolint:errorlint // direct net.Error from conn.Read
		*target = ne
		return true
	}

	return false
}

// TestRecvStall_ControlBlockedByFullDataChannel is the discriminating
// reproduction. A stuck data-message consumer fills the inbound data channel and
// parks the single receiver goroutine; the test then shows (1) the outbound
// sender queue stays EMPTY — ruling out outbound saturation as the trigger — and
// (2) the host can no longer answer a Linktest probe, i.e. ALL control-message
// processing is stalled by application back-pressure. Releasing the consumer
// restores control traffic, confirming the full data channel was the sole cause.
func TestRecvStall_ControlBlockedByFullDataChannel(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	peer := newStallProbePeer(t, port)
	defer peer.stop()

	// Active host, tiny inbound queue, auto-linktest OFF so the only linktest
	// traffic on the wire is the test's explicit probe.
	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithDataMsgQueueSize(1),
		WithT6Timeout(1*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)

	// Gate handler: blocks on release, simulating a slow/stuck consumer.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)

	var delivered int32
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		atomic.AddInt32(&delivered, 1)
		<-release
		msg.Free()
	})

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Baseline: with no backlog the host answers a linktest probe promptly.
	require.True(peer.probeLinktest(time.Second),
		"baseline: host should answer linktest before any backlog")

	// Flood primary data messages. With dataMsgQueueSize=1 the receiver parks in
	// recvDataMsg once the channel is full: 1 in the blocked handler, 1 buffered,
	// the rest stranded in the host's TCP receive buffer.
	peer.pushData(6)

	// The handler grabs exactly one message and blocks; the single receiver
	// goroutine is now parked delivering into the full data channel.
	require.Eventually(func() bool { return atomic.LoadInt32(&delivered) == 1 },
		2*time.Second, 10*time.Millisecond,
		"handler should receive exactly one message then block")

	// DISCRIMINATOR 1 — where does the failure surface?
	// The outbound sender queue is NOT the bottleneck: it stays empty. This rules
	// out Candidate B (outbound saturation) as the trigger in this scenario.
	require.Never(func() bool { return len(host.conn.senderMsgChan) != 0 },
		500*time.Millisecond, 50*time.Millisecond,
		"sender queue must stay empty (rules out outbound saturation as trigger)")

	// DISCRIMINATOR 2 — control liveness.
	// While the receiver is parked it cannot read the Linktest.req frame, so the
	// host never replies. This is the reproduced defect: application back-pressure
	// on the inbound data channel stalls ALL control-message processing.
	require.False(peer.probeLinktest(time.Second),
		"BUG: host cannot answer linktest while its receiver is parked on a full data channel")

	// Release the consumer: the channel drains, the receiver un-parks, and
	// control traffic flows again — confirming the full data channel was the
	// sole cause.
	releaseFn()
	require.True(peer.probeLinktest(2*time.Second),
		"after draining the data channel the host answers linktest again")

	require.NoError(host.close())
}

// TestRecvStall_OutboundQueueFullOnHalfOpen reproduces Candidate B — the
// reported symptom. The peer freezes (stops reading) right after Select while the
// TCP connection stays up, modeling a half-open link with no FIN/RST. The host
// stays Selected (auto-linktest off), so application data sends keep passing the
// IsSelected gate; senderTask stalls on the dead socket, the small outbound
// senderMsgChan saturates, and a further send can no longer be enqueued.
//
// This needs NO slow/stuck consumer (the handler here returns immediately) and NO
// receiver stall — i.e. it is independent of Candidate A.
func TestRecvStall_OutboundQueueFullOnHalfOpen(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Tiny receive buffer + freeze-after-select: the host's writes block almost
	// immediately once the peer stops draining.
	peer := newFreezePeer(t, port, 256)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),        // stay Selected through the half-open window
		WithSenderQueueSize(2),         // small outbound queue
		WithSendTimeout(1*time.Second), // minimum allowed; bounds the queue-full wait
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) { msg.Free() })

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Fire-and-forget data sends against the dead socket. The kernel absorbs the
	// first few MB into the host's auto-tuned send buffer, so we send until real
	// backpressure appears rather than guessing a fixed count: once writes block,
	// senderTask stalls, the cap-2 senderMsgChan saturates, and a send can no
	// longer be enqueued. Payload size is incidental; the ceiling is generous
	// margin over the host's tcp_wmem max so the loop always reaches backpressure.
	big := func() secs2.Item { return secs2.NewBinaryItemWithBytes(make([]byte, 256*1024)) }

	const maxSends = 512 // 512 * 256KiB = 128MiB ceiling — far beyond any sndbuf autotune
	var sendErr error
	maxQueueLen := 0
	for range maxSends {
		err := host.session.SendDataMessageAsync(1, 1, false, big())
		if l := len(host.conn.senderMsgChan); l > maxQueueLen {
			maxQueueLen = l
		}
		if err != nil {
			sendErr = err
			break
		}
	}

	require.GreaterOrEqual(maxQueueLen, 1,
		"outbound sender queue must back up while the dead socket stalls senderTask")
	require.Error(sendErr,
		"BUG: a send fails once the outbound queue is full and the dead socket stalls the sender")
	require.True(errors.Is(sendErr, hsms.ErrSendMsgTimeout) || errors.Is(sendErr, hsms.ErrConnClosed),
		"expected ErrSendMsgTimeout or ErrConnClosed, got: %v", sendErr)
	t.Logf("reproduced Candidate B: send failed with %v (sender queue backed up to len=%d)", sendErr, maxQueueLen)

	_ = host.close()
}

// TestRecvStall_AsyncDataSendDuringNotSelectedKillsConn reproduces the user's
// actual incident: an application sends non-wait-bit / response messages via
// session.SendMessageAsync. Before the fix, two coupled library defects made
// this fatal while the link is not Selected:
//
//	(1) sendMsgAsync had NO IsSelected gate — unlike sendMsg and sendMsgSync, it
//	    enqueued a data message regardless of connection state.
//	(2) When senderTask later pulled that queued data message during NotSelected,
//	    sendMsgSync rejected it with ErrNotSelectedState, and processSendRequest
//	    treated that as fatal -> cancelSenderTask -> NotConnected.
//
// A single async data send while NotSelected therefore tore the connection down;
// with the application sending continuously, every reconnect re-choked senderTask,
// yielding a NotSelected -> NotConnected loop that never reached Selected.
//
// This test is the regression guard for the primary fix (the async gate): a data
// message submitted via SendMessageAsync while NotSelected must be REJECTED with
// ErrNotSelectedState, must NOT enqueue, and must NOT drop the connection —
// identical to the sync SendDataMessage path.
func TestRecvStall_AsyncDataSendGatedWhileNotSelected(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Peer accepts and drains but never answers Select.req, holding the host in
	// NotSelected (until T6, which we set long).
	peer := newDeafSelectPeer(t, port)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithT6Timeout(20*time.Second), // keep the NotSelected window open
		WithT7Timeout(20*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) { msg.Free() })

	// Record whether the connection drops after we arm the watch.
	var watching, sawDrop atomic.Bool
	host.session.AddConnStateChangeHandler(func(_ hsms.Connection, _, cur hsms.ConnState) {
		if watching.Load() && cur == hsms.NotConnectedState {
			sawDrop.Store(true)
		}
	})

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.NotSelectedState))
	time.Sleep(150 * time.Millisecond)
	require.True(host.conn.stateMgr.State().IsNotSelected(), "host should be holding in NotSelected")

	// The sync gated path refuses while NotSelected without dropping the connection.
	_, gErr := host.session.SendDataMessage(1, 1, false, secs2.A("x"))
	require.ErrorIs(gErr, hsms.ErrNotSelectedState, "gated send path must reject data while NotSelected")
	require.True(host.conn.stateMgr.State().IsNotSelected(), "a gated rejection must not drop the connection")

	// The async path must behave identically: reject with ErrNotSelectedState,
	// do not enqueue, do not drop the connection.
	watching.Store(true)
	errBefore := host.conn.metrics.DataMsgErrCount.Load()
	dropBefore := host.conn.metrics.DataMsgDropNotSelectedCount.Load()
	m, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
	require.NoError(err)
	require.ErrorIs(host.session.SendMessageAsync(m), hsms.ErrNotSelectedState,
		"async data send must be gated (ErrNotSelectedState) while NotSelected")
	require.Equal(0, len(host.conn.senderMsgChan), "rejected async data message must not be enqueued")

	// The drop is counted as expected backpressure, NOT as a data-message error.
	require.Equal(dropBefore+1, host.conn.metrics.DataMsgDropNotSelectedCount.Load(),
		"a not-Selected drop must increment DataMsgDropNotSelectedCount")
	require.Equal(errBefore, host.conn.metrics.DataMsgErrCount.Load(),
		"a not-Selected drop must NOT increment DataMsgErrCount")

	// The connection must remain in NotSelected — the async send must not tear it
	// down (no senderTask choke). Confirm no drop over a window well below T6/T7.
	require.Never(sawDrop.Load, time.Second, 50*time.Millisecond,
		"async data send while NotSelected must not drop the connection")
	require.True(host.conn.stateMgr.State().IsNotSelected(), "host should still be holding in NotSelected")

	_ = host.close()
}

// TestRecvStall_AsyncFloodDoesNotStarveSelect is the end-to-end regression guard:
// an application that continuously fires SendMessageAsync (non-wait-bit data)
// across the connect must not prevent the active host from reaching Selected.
// Before the fix, the ungated async data filled the small sender queue and
// choked/starved the Select handshake, looping NotSelected→NotConnected. With
// the gate, data submitted while not Selected is rejected and the handshake
// completes.
func TestRecvStall_AsyncFloodDoesNotStarveSelect(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	// Cooperative peer: completes Select and drains.
	peer := newStallProbePeer(t, port)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithSenderQueueSize(2), // small queue: easy to starve without the gate
		WithT6Timeout(2*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) { msg.Free() })

	// Flood async data sends from several goroutines, starting before connect.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m, _ := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
				_ = host.session.SendMessageAsync(m) // ErrNotSelectedState while not Selected; that's fine
			}
		}()
	}

	require.NoError(host.open(false))

	// The handshake must complete despite the flood. Bound the wait so a
	// regression (the NotSelected→NotConnected loop, which never reaches
	// Selected) fails fast and cleanly instead of hanging to the suite timeout.
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(host.conn.stateMgr.WaitState(waitCtx, hsms.SelectedState),
		"async data flood must not starve the Select handshake")

	close(stop)
	wg.Wait()
	_ = host.close()
}

// TestRecvStall_QueuedDataNotSelectedIsNonFatal is the regression guard for the
// defense-in-depth backstop (Phase 2): the async gate prevents data from being
// enqueued while NotSelected in the common case, but a check-then-enqueue race
// can still leave a data message in senderMsgChan that is dequeued after the link
// leaves Selected. When senderTask processes such a message, sendMsgSync returns
// ErrNotSelectedState — and that must be treated as a non-fatal, transient drop
// (processSendRequest returns true; the caller is signaled) rather than escalated
// to a connection teardown.
func TestRecvStall_QueuedDataNotSelectedIsNonFatal(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	peer := newDeafSelectPeer(t, port)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithT6Timeout(20*time.Second),
		WithT7Timeout(20*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) { msg.Free() })

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.NotSelectedState))
	time.Sleep(100 * time.Millisecond)
	require.True(host.conn.stateMgr.State().IsNotSelected())

	// Drive processSendRequest directly with a data message while NotSelected —
	// modeling a message that was queued while Selected and dequeued after the
	// link dropped (the residual race the async gate cannot fully close).
	m, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
	require.NoError(err)
	errBefore := host.conn.metrics.DataMsgErrCount.Load()
	dropBefore := host.conn.metrics.DataMsgDropNotSelectedCount.Load()
	sentChan := make(chan error, 1)
	keepGoing := host.conn.processSendRequest(&sendRequest{msg: m, sentChan: sentChan})

	require.True(keepGoing,
		"a queued data message that is NotSelected at dequeue must NOT be fatal to senderTask")
	require.ErrorIs(<-sentChan, hsms.ErrNotSelectedState,
		"the caller must still be signaled with ErrNotSelectedState")

	// Counted as a not-Selected drop, not as a data-message error.
	require.Equal(dropBefore+1, host.conn.metrics.DataMsgDropNotSelectedCount.Load(),
		"the dropped queued message must increment DataMsgDropNotSelectedCount")
	require.Equal(errBefore, host.conn.metrics.DataMsgErrCount.Load(),
		"the dropped queued message must NOT increment DataMsgErrCount")

	_ = host.close()
}

// TestRecvStall_QueuedDataNotSelectedFireAndForgetRoutesError is the regression
// guard for the fire-and-forget (sentChan==nil) sub-path of the defense-in-depth
// backstop in processSendRequest. When a data message is dequeued after the link
// left Selected and sentChan is nil (i.e. the message was submitted via
// SendMessageAsync without a reply channel), replyErrToSender constructs a
// synthetic ErrorDataMessage and delivers it to the session's data-message handler.
// This test verifies that:
//   - processSendRequest still returns true (non-fatal to senderTask)
//   - the drop is counted as DataMsgDropNotSelectedCount, not DataMsgErrCount
//   - the handler receives the synthetic message wrapping ErrNotSelectedState with
//     the correct StreamCode and FunctionCode
//
// This complements TestRecvStall_QueuedDataNotSelectedIsNonFatal which covers the
// W-bit synchronous send sub-path (sentChan!=nil).
func TestRecvStall_QueuedDataNotSelectedFireAndForgetRoutesError(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	peer := newDeafSelectPeer(t, port)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false),
		WithT6Timeout(20*time.Second),
		WithT7Timeout(20*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)

	// Capturing handler: records the error, stream code, and function code from
	// the delivered synthetic ErrorDataMessage before freeing it. Uses a buffered
	// channel so the handler never blocks the goroutine that delivers the message.
	type capturedMsg struct {
		err          error
		streamCode   uint8
		functionCode uint8
	}
	captured := make(chan capturedMsg, 1)
	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		// Capture fields before Free — handler owns the delivered message.
		c := capturedMsg{
			err:          msg.Error(),
			streamCode:   msg.StreamCode(),
			functionCode: msg.FunctionCode(),
		}
		msg.Free()
		captured <- c
	})

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.NotSelectedState))
	time.Sleep(100 * time.Millisecond)
	require.True(host.conn.stateMgr.State().IsNotSelected())

	// Build a non-W-bit data message (fire-and-forget path: sentChan left nil),
	// modeling a message that was queued while Selected and dequeued while
	// NotSelected — the residual check-then-enqueue race the async gate cannot
	// fully close.
	m, err := hsms.NewDataMessage(1, 1, false, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
	require.NoError(err)
	errBefore := host.conn.metrics.DataMsgErrCount.Load()
	dropBefore := host.conn.metrics.DataMsgDropNotSelectedCount.Load()

	// Drive the fire-and-forget path: sentChan is nil, so replyErrToSender routes
	// the error to the data-message handler as a synthetic ErrorDataMessage.
	keepGoing := host.conn.processSendRequest(&sendRequest{msg: m})

	require.True(keepGoing,
		"a fire-and-forget queued data message that is NotSelected at dequeue must NOT be fatal to senderTask")

	// Counted as a not-Selected drop, not as a data-message error.
	require.Equal(dropBefore+1, host.conn.metrics.DataMsgDropNotSelectedCount.Load(),
		"the dropped fire-and-forget message must increment DataMsgDropNotSelectedCount")
	require.Equal(errBefore, host.conn.metrics.DataMsgErrCount.Load(),
		"the dropped fire-and-forget message must NOT increment DataMsgErrCount")

	// The handler must receive the synthetic ErrorDataMessage with the correct
	// error and original stream/function codes.
	select {
	case got := <-captured:
		require.ErrorIs(got.err, hsms.ErrNotSelectedState,
			"synthetic error message must wrap ErrNotSelectedState")
		require.Equal(uint8(1), got.streamCode,
			"synthetic error message must carry the original stream code")
		require.Equal(uint8(1), got.functionCode,
			"synthetic error message must carry the original function code")
	case <-time.After(2 * time.Second):
		t.Fatal("data-message handler did not receive the synthetic ErrorDataMessage within 2s")
	}

	_ = host.close()
}

// TestRecvStall_ParkedReceiverDelaysDisconnectDetection demonstrates a distinct
// defect: a receiver parked in recvDataMsg never reads the socket, so a GRACEFUL
// peer close (FIN) — which a healthy receiver detects within ~250ms, verified by
// counterfactual — goes UNDETECTED and the host lingers in Selected. This is one
// way the half-open window widens; it is NOT a precondition for Candidate B,
// which opens its own window on a silent drop (see the file header).
func TestRecvStall_ParkedReceiverDelaysDisconnectDetection(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	port := getPort()

	peer := newStallProbePeer(t, port)
	defer peer.stop()

	host := doNewTestComm(ctx, t, port, true, true,
		WithAutoLinktest(false), // no linktest-based disconnect detection
		WithDataMsgQueueSize(1),
		WithT6Timeout(1*time.Second),
		WithT7Timeout(30*time.Second),
		WithConnectRemoteTimeout(1*time.Second),
		WithCloseConnTimeout(2*time.Second),
	)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)

	host.session.AddDataMessageHandler(func(msg *hsms.DataMessage, _ hsms.Session) {
		<-release
		msg.Free()
	})

	require.NoError(host.open(false))
	require.NoError(host.conn.stateMgr.WaitState(ctx, hsms.SelectedState))

	// Park the receiver on a full data channel.
	peer.pushData(6)
	time.Sleep(200 * time.Millisecond)
	require.True(host.conn.stateMgr.State().IsSelected(), "host should still be Selected after flood")

	// Abruptly drop the peer TCP socket. A healthy receiver would observe EOF and
	// tear the connection down; a receiver parked in recvDataMsg never reads, so
	// the disconnect goes undetected.
	peer.stop()

	// With auto-linktest off and the receiver parked, nothing detects the drop:
	// the host stays Selected well past a clean-teardown window. This is the
	// half-open window during which application data sends still pass the
	// IsSelected gate and can saturate the outbound sender queue.
	require.Never(func() bool { return !host.conn.stateMgr.State().IsSelected() },
		1500*time.Millisecond, 100*time.Millisecond,
		"BUG: parked receiver leaves host Selected on a dead socket (half-open window)")

	// Unblock the consumer before closing so teardown is clean.
	releaseFn()
	_ = host.close()
}

// TestSendGates_CountNotSelectedDropsExactlyOnce is the teeth check for the
// single-chokepoint refactor: every public send path (SendMessage → sendMsg,
// SendMessageSync → sendMsgSync, SendMessageAsync → sendMsgAsync) must increment
// DataMsgDropNotSelectedCount by EXACTLY one per rejected data message while not
// Selected — never zero (forgot to count) and never two (double count). All three
// route through dropNotSelected, the only site that increments the metric.
//
// A never-opened connection is deterministically not Selected, so each gate fires
// at its entry check before any TCP I/O. SendMessageSync is included specifically
// because it is reachable directly (bypassing processSendRequest), the path the
// pre-refactor code left uncounted.
func TestSendGates_CountNotSelectedDropsExactlyOnce(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)
	session := conn.AddSession(testSessionID)

	require.False(conn.stateMgr.IsSelected(), "never-opened connection must not be Selected")

	newMsg := func(wbit bool) hsms.HSMSMessage {
		m, mErr := hsms.NewDataMessage(1, 1, wbit, testSessionID, hsms.GenerateMsgSystemBytes(), secs2.A("x"))
		require.NoError(mErr)

		return m
	}

	// SendMessage (sendMsg gate) — exercise both W-bit variants; the gate fires
	// before the WaitBit branch, so each must count once.
	_, err = session.SendMessage(newMsg(false))
	require.ErrorIs(err, hsms.ErrNotSelectedState)
	require.Equal(uint64(1), conn.metrics.DataMsgDropNotSelectedCount.Load(), "sendMsg (non-W-bit) must count once")

	_, err = session.SendMessage(newMsg(true))
	require.ErrorIs(err, hsms.ErrNotSelectedState)
	require.Equal(uint64(2), conn.metrics.DataMsgDropNotSelectedCount.Load(), "sendMsg (W-bit) must count once")

	// SendMessageSync (sendMsgSync gate) — the direct-caller path that was
	// previously uncounted.
	require.ErrorIs(session.SendMessageSync(newMsg(false)), hsms.ErrNotSelectedState)
	require.Equal(uint64(3), conn.metrics.DataMsgDropNotSelectedCount.Load(), "sendMsgSync must count once")

	// SendMessageAsync (sendMsgAsync gate).
	require.ErrorIs(session.SendMessageAsync(newMsg(false)), hsms.ErrNotSelectedState)
	require.Equal(uint64(4), conn.metrics.DataMsgDropNotSelectedCount.Load(), "sendMsgAsync must count once")

	// Drops are expected backpressure, never data-message errors.
	require.Equal(uint64(0), conn.metrics.DataMsgErrCount.Load(),
		"not-Selected drops must NOT increment DataMsgErrCount")
}
