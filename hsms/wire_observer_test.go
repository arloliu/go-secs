package hsms

// wire_observer_test.go — white-box tests for the outbound half of WithWireObserver:
// the two core write sites (writeFrame and the courtesy Separate)
// report a frame only when the transport offers the wire-reporting capability,
// only after the write succeeded, with the exact bytes the transport was handed,
// and without adding an allocation per frame once the per-epoch scratch buffer has grown.
// The inbound half and the real-socket comparison live in hsmsss/wire_observer_test.go.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// wireReportingMock is the send-path mock transport plus the wire-reporting capability,
// standing in for the HSMS-SS transport, whose Write puts the handed buffers on a socket unchanged.
type wireReportingMock struct {
	*mockTransport
}

func (wireReportingMock) WireReporting() {}

// wireLog collects WireEvents, copying each frame because the observer contract makes it valid only during the call.
type wireLog struct {
	mu     sync.Mutex
	events []WireEvent
}

func (l *wireLog) observe(ev WireEvent) {
	frame := make([]byte, len(ev.Frame))
	copy(frame, ev.Frame)
	ev.Frame = frame

	l.mu.Lock()
	l.events = append(l.events, ev)
	l.mu.Unlock()
}

func (l *wireLog) snapshot() []WireEvent {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]WireEvent, len(l.events))
	copy(out, l.events)

	return out
}

// newWireSendConn is newTestSendConn over a transport that offers the wire-reporting capability.
// The live epoch gets a non-zero generation and socket identity,
// so an event's Generation and Socket are distinguishable from the zero value.
func newWireSendConn(t *testing.T, opts ...ConnOption) (*connection, *mockTransport) {
	t.Helper()

	cfg := DefaultConnectionConfig()
	require.NoError(t, cfg.apply(opts...))

	mock := &mockTransport{}
	conn, err := NewConnection(cfg, wireReportingMock{mock})
	require.NoError(t, err)

	c, ok := conn.(*connection)
	require.True(t, ok, "NewConnection must return the concrete *connection")

	sup := newSupervisor(func(_, _ ConnState, _ TransitionCause) {}, &c.handlers, &c.lifecycleSubs)
	sup.state.Store(uint32(SelectedState))
	c.sup.Store(sup)

	e := newEpoch(t.Context(), cfg.logger, cfg.senderQueueSize)
	e.id = 7
	e.adoptConn(fakeConn{}, 11)
	c.cur.Store(e)

	return c, mock
}

// Every frame writeFrame puts on the wire is reported once, in write order,
// as the exact bytes the transport was handed (length prefix, header and body),
// with the writing epoch's generation and a set timestamp.
func TestWireObserver_WriteFrameReportsWrittenBytes(t *testing.T) {
	log := &wireLog{}
	c, tr := newWireSendConn(t, WithWireObserver(log.observe))
	e := c.cur.Load()

	msgs := []Message{
		mustSendData(t, [4]byte{0, 0, 0, 1}, true),
		NewLinktestReq([4]byte{0, 0, 0, 2}),
		mustSendReply(t, [4]byte{0, 0, 0, 3}),
	}
	for _, m := range msgs {
		require.NoError(t, c.writeFrame(t.Context(), e, m))
	}

	events := log.snapshot()
	written := tr.writtenFrames()
	require.Len(t, written, len(msgs))
	require.Len(t, events, len(msgs), "one event per frame written")

	for i, ev := range events {
		require.Equal(t, WireOutbound, ev.Direction, "event %d", i)
		require.Equal(t, uint64(7), ev.Generation, "event %d names the writing epoch", i)
		require.Equal(t, uint64(11), ev.Socket, "event %d names the socket it was written to", i)
		require.False(t, ev.At.IsZero(), "event %d carries a timestamp", i)
		require.Equal(t, written[i], ev.Frame, "event %d is the frame the transport wrote", i)
		require.Equal(t, msgs[i].ToBytes(), ev.Frame, "event %d is the message's wire encoding", i)
	}
}

// A frame whose write failed is not reported, nor is one refused before the write
// (here: a data message the B2 gate drops because the link is no longer Selected).
func TestWireObserver_WriteFrameFailuresNotReported(t *testing.T) {
	log := &wireLog{}
	c, tr := newWireSendConn(t, WithWireObserver(log.observe))
	e := c.cur.Load()

	tr.writeErr = errors.New("boom")
	require.Error(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 1}, false)))

	tr.writeErr = nil
	c.sup.Load().state.Store(uint32(NotSelectedState))
	require.ErrorIs(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 2}, false)), ErrNotSelectedState)

	require.Empty(t, log.snapshot())
}

// A transport without the wire-reporting capability — SECS-I, whose Write drops control frames and re-frames data as blocks —
// never has a frame reported for it, even with an observer installed.
func TestWireObserver_TransportWithoutCapabilityReportsNothing(t *testing.T) {
	log := &wireLog{}
	c, _ := newTestSendConn(t, SelectedState, WithWireObserver(log.observe))
	e := c.cur.Load()

	require.NoError(t, c.writeFrame(t.Context(), e, mustSendData(t, [4]byte{0, 0, 0, 1}, false)))
	require.NoError(t, c.writeFrame(t.Context(), e, NewLinktestReq([4]byte{0, 0, 0, 2})))
	c.writeFarewellSeparate(e)

	require.Empty(t, log.snapshot())
}

// The courtesy Separate written on a graceful close is reported like any other frame,
// and not reported when its write fails.
func TestWireObserver_CourtesySeparateReported(t *testing.T) {
	log := &wireLog{}
	c, tr := newWireSendConn(t, WithWireObserver(log.observe))
	e := c.cur.Load()

	c.writeFarewellSeparate(e)

	events := log.snapshot()
	written := tr.writtenFrames()
	require.Len(t, written, 1)
	require.Len(t, events, 1)
	require.Equal(t, WireOutbound, events[0].Direction)
	require.Equal(t, uint64(7), events[0].Generation)
	require.Equal(t, uint64(11), events[0].Socket)
	require.Equal(t, written[0], events[0].Frame)
	require.Equal(t, byte(SeparateReqType), events[0].Frame[4+5], "the reported frame is the Separate.req")

	tr.writeErr = errors.New("boom")
	c.writeFarewellSeparate(e)
	require.Len(t, log.snapshot(), 1, "a failed courtesy write is not reported")
}

// A panic in the observer is not recovered by the send path:
// it reaches the caller of the write, and the write lock is released on the way out.
func TestWireObserver_PanicPropagatesAndReleasesWriteLock(t *testing.T) {
	c, _ := newWireSendConn(t, WithWireObserver(func(WireEvent) { panic("observer boom") }))
	e := c.cur.Load()

	require.PanicsWithValue(t, "observer boom", func() {
		_ = c.writeFrame(t.Context(), e, NewLinktestReq([4]byte{0, 0, 0, 1}))
	})

	require.True(t, e.writeMu.TryLock(), "the write lock must be released by the unwinding panic")
	e.writeMu.Unlock()
}

// With no observer installed, ObserveWire returns without doing anything.
func TestWireObserver_ObserveWireWithoutObserverIsNoop(t *testing.T) {
	c, _ := newWireSendConn(t)

	require.NotPanics(t, func() {
		c.ObserveWire(WireEvent{Direction: WireInbound, Frame: []byte{0, 0, 0, 10}})
	})
}

// ObserveWire hands the event it is given to the installed observer unchanged.
func TestWireObserver_ObserveWireDelivers(t *testing.T) {
	log := &wireLog{}
	c, _ := newWireSendConn(t, WithWireObserver(log.observe))

	frame := NewLinktestReq([4]byte{0, 0, 0, 1}).ToBytes()
	c.ObserveWire(WireEvent{Direction: WireInbound, Generation: 3, Frame: frame})

	events := log.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, WireInbound, events[0].Direction)
	require.Equal(t, uint64(3), events[0].Generation)
	require.Equal(t, frame, events[0].Frame)
}

// Installing an observer adds no allocation per outbound frame once the per-epoch scratch buffer has grown:
// writeFrame allocates exactly as often with the observer as without it,
// for a single-buffer control frame and a multi-buffer data frame.
func TestWireObserver_WriteFrameNoExtraAllocs(t *testing.T) {
	var sink int
	observer := func(ev WireEvent) { sink += len(ev.Frame) }

	measure := func(t *testing.T, msg Message, opts ...ConnOption) float64 {
		t.Helper()

		c, tr := newWireSendConn(t, opts...)
		e := c.cur.Load()

		// A Write that consumes the buffers without allocating, so the measurement is the send path's own cost.
		tr.writeFn = func(_ context.Context, _ net.Conn, bufs net.Buffers) error {
			for _, b := range bufs {
				sink += len(b)
			}

			return nil
		}

		return testing.AllocsPerRun(100, func() {
			if err := c.writeFrame(context.Background(), e, msg); err != nil {
				t.Fatal(err)
			}
		})
	}

	msgs := map[string]Message{
		"control": NewLinktestReq([4]byte{0, 0, 0, 1}),
		"data":    mustSendData(t, [4]byte{0, 0, 0, 2}, false),
	}
	for name, msg := range msgs {
		t.Run(name, func(t *testing.T) {
			without := measure(t, msg)
			with := measure(t, msg, WithWireObserver(observer))
			require.Equal(t, without, with, "the observer must add no allocation per frame (without=%v with=%v)", without, with)
		})
	}

	require.Positive(t, sink)
}
