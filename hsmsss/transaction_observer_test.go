package hsmsss

// transaction_observer_test.go exercises hsms.WithTransactionObserver end to end
// over the real active+passive loopback pair harness (newEndpointPair / newEndpoint, harness_endpoint_test.go):
// one test per TxOutcome the sync send paths can produce,
// plus duration sanity, the unset-path no-op, and observer panic propagation.
// See connection_send.go's classifyTxOutcome doc comment in the hsms package for the full return-path -> outcome table this suite exercises.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// txEventRecorder collects TxEvents under a mutex so it is safe to install as a
// WithTransactionObserver callback and read back from the test goroutine afterward.
type txEventRecorder struct {
	mu     sync.Mutex
	events []hsms.TxEvent
}

func (r *txEventRecorder) observe(ev hsms.TxEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *txEventRecorder) snapshot() []hsms.TxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]hsms.TxEvent, len(r.events))
	copy(out, r.events)

	return out
}

// TestTransactionObserver_Replied proves a replied W-bit SendDataMessage reports exactly one TxReplied event, with the header fields and a sane, positive Duration.
func TestTransactionObserver_Replied(t *testing.T) {
	t.Parallel()

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithTransactionObserver(rec.observe)))
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	passive.conn.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
	})

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	start := time.Now()
	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1, "exactly one TxEvent for one SendDataMessage call")

	ev := events[0]
	require.Equal(t, hsms.TxReplied, ev.Outcome)
	require.Equal(t, uint8(1), ev.Stream)
	require.Equal(t, uint8(1), ev.Function)
	require.True(t, ev.ReplyWaited)
	require.NoError(t, ev.Err)
	require.Positive(t, ev.Duration, "Duration must be positive")
	require.LessOrEqual(t, ev.Duration, elapsed+50*time.Millisecond, "Duration must not overshoot the test's own wall-clock measurement")
}

// TestTransactionObserver_Sent_WBitClear proves a fire-and-forget (W-bit clear) SendDataMessage reports TxSent with ReplyWaited false, once the frame is on the wire.
func TestTransactionObserver_Sent_WBitClear(t *testing.T) {
	t.Parallel()

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithTransactionObserver(rec.observe)))
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, false, secs2.A("ping"))
	require.NoError(t, err)
	require.Nil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxSent, ev.Outcome)
	require.False(t, ev.ReplyWaited)
	require.NoError(t, ev.Err)
	require.Positive(t, ev.Duration)
}

// TestTransactionObserver_Sent_Forward proves ForwardDataMessage always reports TxSent with ReplyWaited false,
// even when the forwarded message's own W-bit is set —
// the forward path never opens a LOCAL reply-wait transaction regardless of the wire-level W-bit (WriteMessageNoReply's contract).
func TestTransactionObserver_Sent_Forward(t *testing.T) {
	t.Parallel()

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithTransactionObserver(rec.observe)))
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	msg, err := hsms.NewDataMessage(1, 1, true, active.conn.SessionID(), [4]byte{0x09, 0x09, 0x09, 0x09}, secs2.A("fwd"))
	require.NoError(t, err)

	require.NoError(t, active.conn.ForwardDataMessage(t.Context(), msg))

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxSent, ev.Outcome)
	require.False(t, ev.ReplyWaited, "ForwardDataMessage never opens a local reply-wait transaction, regardless of the message's own W-bit")
	require.NoError(t, ev.Err)
}

// TestTransactionObserver_T3Timeout proves a W-bit SendDataMessage whose peer never answers reports TxT3Timeout once T3 expires, with Duration at least T3.
func TestTransactionObserver_T3Timeout(t *testing.T) {
	t.Parallel()

	const t3 = 300 * time.Millisecond

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t,
		WithConnectionOption(hsms.WithTransactionObserver(rec.observe)),
		WithConnectionOption(hsms.WithT3(t3)),
	)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	// No data handler on passive: it never answers, so the send times out.
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	start := time.Now()
	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, err, hsms.ErrT3Timeout)
	require.Nil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxT3Timeout, ev.Outcome)
	require.True(t, ev.ReplyWaited)
	require.ErrorIs(t, ev.Err, hsms.ErrT3Timeout)
	require.GreaterOrEqual(t, ev.Duration, t3, "Duration must be at least T3")
	require.GreaterOrEqual(t, time.Since(start), t3)
}

// TestTransactionObserver_Rejected proves a W-bit SendDataMessage answered by a peer Reject.req reports TxRejected with Err carrying the *hsms.RejectError.
// It reuses the raw inbound-Reject peer from integration_reject_test.go (runInboundRejectPeer).
func TestTransactionObserver_Rejected(t *testing.T) {
	t.Parallel()

	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()

	peerErrCh := make(chan error, 1)
	peerReady := make(chan struct{})
	peerDone := make(chan struct{})
	go func() { peerErrCh <- runInboundRejectPeer(ln, peerReady, peerDone) }()

	rec := &txEventRecorder{}
	sut := newEndpoint(t, port, true, []Option{WithConnectionOption(hsms.WithTransactionObserver(rec.observe))})
	defer closeEndpoint(t, sut)
	require.NoError(t, sut.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, sut)

	select {
	case <-peerReady:
	case err := <-peerErrCh:
		t.Fatalf("peer failed during select: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to become ready")
	}

	reply, err := sut.conn.SendDataMessage(context.Background(), 1, 1, true, secs2.A("hello"))
	var rejectErr *hsms.RejectError
	require.ErrorAs(t, err, &rejectErr)
	require.Nil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxRejected, ev.Outcome)
	require.True(t, ev.ReplyWaited)
	require.ErrorAs(t, ev.Err, &rejectErr)

	close(peerDone)
	select {
	case err := <-peerErrCh:
		require.NoError(t, err, "raw peer completed with error")
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw peer to finish")
	}
}

// TestTransactionObserver_ContextCanceled proves a W-bit SendDataMessage whose caller context expires before any reply arrives reports TxCanceled with Err wrapping the context error.
func TestTransactionObserver_ContextCanceled(t *testing.T) {
	t.Parallel()

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithTransactionObserver(rec.observe)))
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	// No data handler on passive: it never answers, so only the caller ctx can end the wait —
	// its deadline (well under T3=3s, the harness default) always wins the select race.
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	reply, err := active.conn.SendDataMessage(ctx, 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxCanceled, ev.Outcome)
	require.True(t, ev.ReplyWaited)
	require.ErrorIs(t, ev.Err, context.DeadlineExceeded)
}

// TestTransactionObserver_SendError_NotSelected proves a SendDataMessage issued before the B1
// IsSelected gate opens reports TxSendError with Err wrapping ErrNotSelectedState.
func TestTransactionObserver_SendError_NotSelected(t *testing.T) {
	t.Parallel()

	rec := &txEventRecorder{}
	port := freeLoopbackPort(t)
	passive := newEndpoint(t, port, false, []Option{WithConnectionOption(hsms.WithTransactionObserver(rec.observe))})
	defer closeEndpoint(t, passive)

	// Opened but no peer has dialed yet: the link is not Selected (NotConnected),
	// so the B1 gate refuses the send before any write is attempted.
	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.Equal(t, hsms.NotConnectedState, passive.conn.State())

	reply, err := passive.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	require.ErrorIs(t, err, hsms.ErrNotSelectedState)
	require.Nil(t, reply)

	events := rec.snapshot()
	require.Len(t, events, 1)

	ev := events[0]
	require.Equal(t, hsms.TxSendError, ev.Outcome)
	require.ErrorIs(t, ev.Err, hsms.ErrNotSelectedState)
}

// TestTransactionObserver_Unset proves omitting WithTransactionObserver leaves send behavior unchanged —
// the option's absence is not exercised via a callback (there is none to call),
// but through an ordinary replied round trip completing exactly as TestTransactionObserver_Replied's does.
// The zero measurable overhead this implies is pinned by BenchmarkWriteMessage_ObserverUnset in connection_send_bench_test.go (hsms package).
func TestTransactionObserver_Unset(t *testing.T) {
	t.Parallel()

	passive, active := newEndpointPair(t)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	passive.conn.AddDataMessageHandler(func(msg *hsms.DataMessage, ep hsms.SECS2Endpoint) {
		_ = ep.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
	})

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)
}

// TestTransactionObserver_PanicPropagates proves an observer panic is NOT recovered:
// it surfaces to the caller of the send exactly as documented on WithTransactionObserver.
func TestTransactionObserver_PanicPropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("observer boom")
	passive, active := newEndpointPair(t, WithConnectionOption(hsms.WithTransactionObserver(func(hsms.TxEvent) {
		panic(boom)
	})))
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	require.PanicsWithValue(t, boom, func() {
		_, _ = active.conn.SendDataMessage(t.Context(), 1, 1, false, secs2.A("ping"))
	})

	// The panic unwound only after sendWaitReply/sendNoReply fully completed
	// (WriteMessage calls the observer AFTER the inner send returns),
	// so the connection itself is unaffected: it must still be Selected and able to complete a normal send.
	require.Equal(t, hsms.SelectedState, active.conn.State())
}

// TestTransactionObserver_ControlTransaction_NoEvent proves the isData gate in WriteMessage:
// the internal Select.req / Linktest.req control sends a transport issues never reach the
// classifier, so they never report a TxEvent — only a caller-issued DATA message send does.
//
// Teeth-checked by deleting the gate (dm, _ := msg.(*DataMessage), no isData short-circuit):
// every test in this file that Opens a connection with an observer configured — not just this one —
// immediately crashes with a nil-pointer panic in newTxEvent's dm.WaitBit() call,
// because the internal Select.req handshake (dm == nil) reaches the classifier first.
// Restoring the gate restores a clean suite.
// That crash is real teeth, but it is a blunt, whole-process signal that stops at the first control send;
// this test adds a targeted, non-crashing assertion specifically on auto-linktest traffic
// (several completed Linktest.req/.rsp round trips, zero events observed),
// so a narrower future regression — Select.req made nil-safe but Linktest.req left reaching the classifier —
// still fails a clean assertion here instead of going unnoticed.
func TestTransactionObserver_ControlTransaction_NoEvent(t *testing.T) {
	t.Parallel()

	const linktestInterval = 100 * time.Millisecond

	rec := &txEventRecorder{}
	passive, active := newEndpointPair(t,
		WithConnectionOption(hsms.WithTransactionObserver(rec.observe)),
		WithConnectionOption(hsms.WithLinktestInterval(linktestInterval)),
	)
	defer closeEndpoint(t, passive)
	defer closeEndpoint(t, active)

	require.NoError(t, passive.conn.Open(context.Background(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(context.Background(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	// Wait for several completed auto-linktest round trips on each side — both sides configured
	// WithLinktestInterval and WithTransactionObserver, so this exercises the gate on both the
	// active and passive connection's own WriteMessage(Linktest.req) calls.
	require.Eventually(t, func() bool {
		return controlMetrics(t, active).LinktestRecvCount() >= 3 && controlMetrics(t, passive).LinktestRecvCount() >= 3
	}, 20*linktestInterval, 5*time.Millisecond, "expected several completed auto-linktest round trips on both sides")

	require.Empty(t, rec.snapshot(), "a control transaction (Linktest.req/.rsp) must never report a TxEvent")
}
