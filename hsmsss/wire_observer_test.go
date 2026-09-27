package hsmsss

// wire_observer_test.go exercises hsms.WithWireObserver end to end over real loopback sockets.
//
// Most scenarios run a passive endpoint (the SUT) against a raw TCP peer that authors and reads the on-wire frames itself,
// so every observed frame is compared byte for byte with what the peer wrote or read.
// Within one direction events are asserted in wire order;
// across directions only a causal order is asserted,
// because an outbound frame is reported after its write returns, which can land after the peer has already answered it.

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// undefinedSType is an SType E37 leaves undefined, which the receive path answers with a Reject.req.
const undefinedSType = 12

// wireRecorder collects WireEvents in the order the observer saw them.
// It copies each frame, because a frame is valid only for the duration of the call.
type wireRecorder struct {
	mu     sync.Mutex
	events []hsms.WireEvent
}

func (r *wireRecorder) observe(ev hsms.WireEvent) {
	ev.Frame = bytes.Clone(ev.Frame)

	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *wireRecorder) snapshot() []hsms.WireEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]hsms.WireEvent, len(r.events))
	copy(out, r.events)

	return out
}

// direction returns the recorded events of one direction, in the order they were observed.
func (r *wireRecorder) direction(d hsms.WireDirection) []hsms.WireEvent {
	var out []hsms.WireEvent
	for _, ev := range r.snapshot() {
		if ev.Direction == d {
			out = append(out, ev)
		}
	}

	return out
}

// waitDirection waits (bounded poll) until at least n events of direction d were observed and returns them.
func (r *wireRecorder) waitDirection(t *testing.T, d hsms.WireDirection, n int) []hsms.WireEvent {
	t.Helper()

	var events []hsms.WireEvent
	require.Eventuallyf(t, func() bool {
		events = r.direction(d)

		return len(events) >= n
	}, 5*time.Second, 2*time.Millisecond, "timeout waiting for %d events of direction %d", n, d)

	return events
}

// newObservedPassive builds a passive SUT on port with a wire observer installed and the echo-reply data handler,
// and opens it.
func newObservedPassive(t *testing.T, port int, rec *wireRecorder) *endpoint {
	t.Helper()

	recvCh := make(chan *hsms.DataMessage, 8)
	ep := newEndpoint(t, port, false, []Option{WithConnectionOption(hsms.WithWireObserver(rec.observe))}, echoReplyHandler(recvCh))
	require.NoError(t, ep.conn.Open(t.Context(), hsms.OpenBackground))

	return ep
}

// rawExchange writes one frame from the raw peer and, when wantReply is set, reads the SUT's answer.
// It returns the answer's payload (header and body, without the length prefix), or nil.
func rawExchange(t *testing.T, raw net.Conn, frame []byte, wantReply bool) []byte {
	t.Helper()

	_, err := raw.Write(frame)
	require.NoError(t, err)

	if !wantReply {
		return nil
	}

	payload, err := peerReadFrame(raw, 5*time.Second)
	require.NoError(t, err)

	return payload
}

// requireOutboundMatches asserts that ev is an outbound frame whose bytes are exactly the payload the peer read,
// preceded by the length prefix announcing it.
func requireOutboundMatches(t *testing.T, ev hsms.WireEvent, payload []byte, msgAndArgs ...any) {
	t.Helper()

	require.Equal(t, hsms.WireOutbound, ev.Direction, msgAndArgs...)
	require.GreaterOrEqual(t, len(ev.Frame), 14, msgAndArgs...)
	require.Equal(t, uint32(len(payload)), binary.BigEndian.Uint32(ev.Frame[:4]), msgAndArgs...)
	require.Equal(t, payload, ev.Frame[4:], msgAndArgs...)
}

// requireOneGeneration asserts every event carries the same non-zero generation, the same non-zero socket and a timestamp,
// and that timestamps never go backwards within one direction; it returns the generation.
func requireOneGeneration(t *testing.T, events []hsms.WireEvent) uint64 {
	t.Helper()

	require.NotEmpty(t, events)
	gen := events[0].Generation
	require.NotZero(t, gen, "an adopted socket's frames carry its generation")
	sock := events[0].Socket
	require.NotZero(t, sock, "every frame names the socket it crossed")

	var last [3]time.Time
	for i, ev := range events {
		require.Equal(t, gen, ev.Generation, "event %d", i)
		require.Equal(t, sock, ev.Socket, "event %d: one generation's frames cross one socket", i)
		require.False(t, ev.At.IsZero(), "event %d carries a timestamp", i)
		require.False(t, ev.At.Before(last[ev.Direction]), "event %d: timestamps are in wire order within a direction", i)
		last[ev.Direction] = ev.At
	}

	return gen
}

// Every frame on the link is observed, control frames included, in wire order within each direction,
// each one exactly as the peer wrote or read it, and all of them with the generation that owns the socket.
func TestWireObserver_EveryFrameBothDirections(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	rec := &wireRecorder{}
	sut := newObservedPassive(t, port, rec)
	defer closeEndpoint(t, sut)

	raw := dialPassive(t, port)
	defer func() { _ = raw.Close() }()

	data, err := hsms.NewDataMessage(1, 1, true, 0xFFFF, [4]byte{0, 0, 0, 3}, secs2.A("ping"))
	require.NoError(t, err)

	inbound := [][]byte{
		selectReqFrame([4]byte{0, 0, 0, 1}),
		buildControlFrame(byte(hsms.LinktestReqType), 0, [4]byte{0, 0, 0, 2}),
		data.ToBytes(),
		buildControlFrame(undefinedSType, 0, [4]byte{0, 0, 0, 4}),
		buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0, 0, 0, 5}),
	}
	wantReply := []bool{true, true, true, true, false}

	var outbound [][]byte
	for i, frame := range inbound {
		if reply := rawExchange(t, raw, frame, wantReply[i]); reply != nil {
			outbound = append(outbound, reply)
		}
	}

	// The peer Separate tears the link down; it was observed before it was dispatched.
	waitState(t, sut, hsms.NotConnectedState)

	in := rec.waitDirection(t, hsms.WireInbound, len(inbound))
	out := rec.waitDirection(t, hsms.WireOutbound, len(outbound))
	require.Len(t, in, len(inbound))
	require.Len(t, out, len(outbound))

	for i := range inbound {
		require.Equal(t, inbound[i], in[i].Frame, "inbound frame %d is the bytes the peer wrote", i)
	}

	for i := range outbound {
		requireOutboundMatches(t, out[i], outbound[i], "outbound frame %d is the bytes the peer read", i)
	}

	wantOut := []hsms.MsgType{hsms.SelectRspType, hsms.LinktestRspType, hsms.DataMsgType, hsms.RejectReqType}
	for i, st := range wantOut {
		require.Equal(t, byte(st), out[i].Frame[9], "outbound frame %d SType", i)
	}

	requireOneGeneration(t, rec.snapshot())
}

// An inbound frame with an undefined SType is observed as read, before the receive path rejects it,
// and the Reject.req answering it is observed after it.
func TestWireObserver_UndefinedSTypeObservedThenRejected(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	rec := &wireRecorder{}
	sut := newObservedPassive(t, port, rec)
	defer closeEndpoint(t, sut)

	raw := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
	defer func() { _ = raw.Close() }()

	sb := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	bad := buildControlFrame(undefinedSType, 0, sb)
	reject := rawExchange(t, raw, bad, true)
	require.Equal(t, byte(hsms.RejectReqType), reject[5])
	require.Equal(t, sb[:], reject[6:10], "the Reject.req echoes the offending System Bytes")

	rec.waitDirection(t, hsms.WireOutbound, 2) // Select.rsp, Reject.req

	all := rec.snapshot()
	badIdx, rejectIdx := -1, -1
	for i, ev := range all {
		switch {
		case ev.Direction == hsms.WireInbound && bytes.Equal(ev.Frame, bad):
			badIdx = i
		case ev.Direction == hsms.WireOutbound && ev.Frame[9] == byte(hsms.RejectReqType):
			rejectIdx = i
			requireOutboundMatches(t, ev, reject)
		default:
		}
	}

	require.GreaterOrEqual(t, badIdx, 0, "the undefined-SType frame must be observed")
	require.GreaterOrEqual(t, rejectIdx, 0, "the Reject.req must be observed")
	require.Less(t, badIdx, rejectIdx, "the rejected frame is observed before the Reject.req answering it")
}

// The courtesy Separate a graceful Close writes from Selected is observed as the last outbound frame,
// exactly as the peer read it.
func TestWireObserver_CourtesySeparateObserved(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	rec := &wireRecorder{}
	sut := newObservedPassive(t, port, rec)

	raw := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
	defer func() { _ = raw.Close() }()
	waitSelected(t, sut)

	closeEndpoint(t, sut)

	sep, err := peerReadFrame(raw, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, byte(hsms.SeparateReqType), sep[5])

	out := rec.waitDirection(t, hsms.WireOutbound, 2) // Select.rsp, Separate.req
	require.Len(t, out, 2)
	requireOutboundMatches(t, out[1], sep)
	requireOneGeneration(t, rec.snapshot())
}

// Frames on a reconnected link carry the new generation and the new socket:
// every frame of the first peer's socket names one generation and socket, and every frame of the second peer's later ones.
func TestWireObserver_ReconnectAdvancesGeneration(t *testing.T) {
	t.Parallel()

	port := freeLoopbackPort(t)
	rec := &wireRecorder{}
	sut := newObservedPassive(t, port, rec)
	defer closeEndpoint(t, sut)

	first := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 1})
	rawExchange(t, first, buildControlFrame(byte(hsms.SeparateReqType), 0, [4]byte{0, 0, 0, 2}), false)
	waitState(t, sut, hsms.NotConnectedState)
	_ = first.Close()

	second := rawSelectHandshake(t, port, [4]byte{0, 0, 0, 3})
	defer func() { _ = second.Close() }()

	in := rec.waitDirection(t, hsms.WireInbound, 3)   // Select.req, Separate.req; Select.req
	out := rec.waitDirection(t, hsms.WireOutbound, 2) // Select.rsp; Select.rsp
	require.Len(t, in, 3)
	require.Len(t, out, 2)

	g1 := requireOneGeneration(t, []hsms.WireEvent{in[0], in[1], out[0]})
	g2 := requireOneGeneration(t, []hsms.WireEvent{in[2], out[1]})
	require.Greater(t, g2, g1, "the reconnected socket belongs to a later generation")
	require.Greater(t, in[2].Socket, in[0].Socket, "the reconnected socket has a later identity")
}

// Between two go-secs endpoints, what one side observes writing is exactly what the other observes reading, in both directions:
// the active side's own Select.req, a data round trip, and the active's courtesy Separate on Close.
func TestWireObserver_PairSidesAgree(t *testing.T) {
	t.Parallel()

	activeRec, passiveRec := &wireRecorder{}, &wireRecorder{}
	port := freeLoopbackPort(t)
	passive := newEndpoint(t, port, false, []Option{WithConnectionOption(hsms.WithWireObserver(passiveRec.observe))}, echoHandler)
	active := newEndpoint(t, port, true, []Option{WithConnectionOption(hsms.WithWireObserver(activeRec.observe))})
	defer closeEndpoint(t, passive)

	require.NoError(t, passive.conn.Open(t.Context(), hsms.OpenBackground))
	require.NoError(t, active.conn.Open(t.Context(), hsms.OpenBackground))
	waitSelected(t, passive)
	waitSelected(t, active)

	reply, err := active.conn.SendDataMessage(t.Context(), 1, 1, true, secs2.A("ping"))
	require.NoError(t, err)
	require.NotNil(t, reply)

	closeEndpoint(t, active)
	waitState(t, passive, hsms.NotConnectedState)

	// The active's courtesy Separate is reported from a goroutine of its own, which can trail its Close.
	activeOut := activeRec.waitDirection(t, hsms.WireOutbound, 3)
	activeIn := activeRec.direction(hsms.WireInbound)
	passiveIn := passiveRec.waitDirection(t, hsms.WireInbound, len(activeOut))
	// An outbound frame is reported after its write returns, which can trail the peer's read of it.
	passiveOut := passiveRec.waitDirection(t, hsms.WireOutbound, len(activeIn))

	require.Len(t, activeOut, 3, "Select.req, S1F1, Separate.req")
	require.Equal(t, byte(hsms.SelectReqType), activeOut[0].Frame[9])
	require.Equal(t, byte(hsms.SeparateReqType), activeOut[2].Frame[9])

	require.Equal(t, framesOf(activeOut), framesOf(passiveIn), "the passive read exactly what the active wrote")
	require.Equal(t, framesOf(passiveOut), framesOf(activeIn), "the active read exactly what the passive wrote")

	requireOneGeneration(t, activeRec.snapshot())
	requireOneGeneration(t, passiveRec.snapshot())
}

// The inbound frame is the buffer the reader allocated, handed to the observer without a copy:
// the length prefix sits in front of the header, and the frame shares the allocation the reader made.
func TestWireObserver_InboundFrameIsReaderBuffer(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		allocs [][]byte
	)
	record := func(n int) []byte {
		buf := make([]byte, n)
		mu.Lock()
		allocs = append(allocs, buf)
		mu.Unlock()

		return buf
	}

	var shared []bool
	observer := func(ev hsms.WireEvent) {
		mu.Lock()
		defer mu.Unlock()

		last := allocs[len(allocs)-1]
		shared = append(shared, len(last) == len(ev.Frame) && &last[0] == &ev.Frame[0])
	}

	rt := &wireRecRT{recRT: newRecRT(), observe: observer}
	peer := startReader(t, rt, nil, func(tr *transport) { tr.allocFrame = record })

	data, err := hsms.NewDataMessage(1, 1, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.A("ping"))
	require.NoError(t, err)
	_, err = peer.Write(data.ToBytes())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(shared) == 1
	}, 5*time.Second, 2*time.Millisecond)

	mu.Lock()
	sharedFirst := shared[0]
	mu.Unlock()
	require.True(t, sharedFirst, "the observed frame must be the reader's own buffer, prefix included")

	// The frame is observed before it is dispatched, so the delivery may still be in flight.
	require.Eventually(t, func() bool { return rt.deliveredCount() == 1 }, 5*time.Second, 2*time.Millisecond)
	require.Equal(t, data.ToBytes()[4:], rt.lastDelivered(), "dispatch receives the frame without its length prefix")
}

// wireRecRT is the receive-path test runtime plus the wire-observation capability.
// It carries no generation capability, so it also shows the capability is asserted on its own.
type wireRecRT struct {
	*recRT
	observe func(hsms.WireEvent)
}

func (r *wireRecRT) ObserveWire(ev hsms.WireEvent) { r.observe(ev) }

// framesOf returns the frames of events, in order.
func framesOf(events []hsms.WireEvent) [][]byte {
	out := make([][]byte, len(events))
	for i, ev := range events {
		out[i] = ev.Frame
	}

	return out
}
