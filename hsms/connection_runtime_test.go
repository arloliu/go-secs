package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/logger/loggertest"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// ownedFrame converts a DataMessage to the owned [header||body] frame layout that
// DeliverOwnedFrame consumes. ToBytes prepends a 4-byte length prefix that the owned recv
// path (readFrame) has already stripped before calling DeliverOwnedFrame, so we drop it here.
func ownedFrame(t *testing.T, msg *DataMessage) []byte {
	t.Helper()

	return msg.ToBytes()[4:]
}

// TestDeliverOwnedFrame_PrimaryNotRoutedAsReply is the load-bearing Finding-1 teeth-test.
// A peer's PRIMARY (odd function, W-bit set) whose System Bytes collide with a local waiting
// W-bit sender (both peers' generators start at 1, so their first transactions DO collide in
// symmetric HSMS-SS) must be delivered to the data handlers, NOT mis-consumed by the waiting
// sender as if it were the reply. Reply correlation applies ONLY to a secondary (E37 §8.2.6.9).
//
// TEETH: remove the isSecondaryReply guard in DeliverOwnedFrame (route EVERY message through
// RouteReply first) and this test fails — the primary is stolen by the waiting sender's reply
// channel and never reaches the handler.
func TestDeliverOwnedFrame_PrimaryNotRoutedAsReply(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	sb := [4]byte{0, 0, 0, 1}

	// Mimic a waiting W-bit sender: register a reply channel for SB on the live epoch's
	// sender-owned registry (exactly what sendWaitReply does before it writes the primary).
	replyCh := e.replies.register(sb)
	defer e.replies.deregister(sb)

	// A registered data handler must receive the peer's primary (synchronous fan-out).
	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	// A PRIMARY (S1F1: function 1 = odd, W-bit set) whose System Bytes == SB (the collision).
	primary := mustSendData(t, sb, true)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, primary)))

	// The waiting sender must NOT receive it — a primary is never a reply.
	select {
	case <-replyCh:
		t.Fatal("primary with colliding System Bytes was mis-routed to the waiting sender")
	default:
	}

	// The data handler MUST receive it.
	select {
	case got := <-handlerCh:
		require.Equal(t, sb, got.SystemBytes(), "handler must receive the peer's primary verbatim")
		require.Equal(t, uint8(1), got.Function(), "delivered message is the S1F1 primary")
	default:
		t.Fatal("primary was not delivered to the data handler")
	}

	// The inbound-data chokepoint counts the primary exactly once.
	require.Equal(t, uint64(1), c.metrics.DataMsgRecvCount(), "primary counted once at the recv chokepoint")
}

// TestDeliverOwnedFrame_SecondaryRoutedToSender proves a genuine SECONDARY (even non-zero
// function, W-bit clear) whose System Bytes match a waiting sender IS routed to that sender
// and is NOT also delivered to the data handlers (RouteReply consumed it). This is the
// behavior the existing unidirectional S1F1->S1F2 round-trip depends on.
func TestDeliverOwnedFrame_SecondaryRoutedToSender(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	e := c.cur.Load()

	sb := [4]byte{0, 0, 0, 2}

	replyCh := e.replies.register(sb)
	defer e.replies.deregister(sb)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	// A SECONDARY (S1F2: function 2 = even, no W-bit) reusing the primary's System Bytes.
	reply := mustSendReply(t, sb)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, reply)))

	// The waiting sender receives its reply.
	select {
	case res := <-replyCh:
		require.NotNil(t, res.msg)
		require.Equal(t, sb, res.msg.SystemBytes(), "the routed reply carries the correlated System Bytes")
	default:
		t.Fatal("secondary was not routed to the waiting sender")
	}

	// The handler must NOT also see it — a routed reply is consumed by the registry.
	select {
	case <-handlerCh:
		t.Fatal("secondary routed to the sender must not also reach the data handler")
	default:
	}
}

// TestDeliverOwnedFrame_OrphanSecondaryToHandler proves a SECONDARY whose System Bytes miss
// the registry (a late/unsolicited reply — e.g. after T3 with no waiting sender) falls through
// to RouteData and is delivered to the handlers as unsolicited (unchanged behavior).
func TestDeliverOwnedFrame_OrphanSecondaryToHandler(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	// No reply channel registered — RouteReply misses.
	sb := [4]byte{0, 0, 0, 3}

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	reply := mustSendReply(t, sb)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, reply)))

	select {
	case got := <-handlerCh:
		require.Equal(t, sb, got.SystemBytes(), "orphan secondary delivered to handler verbatim")
	default:
		t.Fatal("orphan secondary must fall through to the data handler")
	}
}

// TestIsSecondaryReply is a focused discriminator table test documenting exactly which
// messages the reply registry may consume: only an even, non-zero function with the W-bit
// clear (a SECS-II secondary). Primaries (odd function) and any W-bit message are never replies.
// The header is built directly (white-box) because NewDataMessage deliberately rejects the
// malformed even-function-with-W-bit combination that this discriminator must still classify.
func TestIsSecondaryReply(t *testing.T) {
	tests := []struct {
		name     string
		function uint8
		waitBit  bool
		want     bool
	}{
		{"S1F1 primary W-bit", 1, true, false},
		{"S1F1 primary no W-bit", 1, false, false},
		{"S1F2 secondary", 2, false, true},
		{"S1F2 with W-bit (malformed reply)", 2, true, false},
		{"S1F0 abort (secondary, E5 §7.2/§10.4.1)", 0, false, true},
		{"S1F0 with W-bit (malformed, not a reply)", 0, true, false},
		{"S5F6 secondary", 6, false, true},
		{"odd high function primary", 63, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dm := &DataMessage{}
			dm.header[3] = tt.function
			if tt.waitBit {
				dm.header[2] |= 0x80
			}
			require.Equal(t, tt.want, isSecondaryReply(dm))
		})
	}
}

// TestDeliverOwnedFrame_SessionIDValidation_DefaultOff proves v2's shipped behavior is unchanged:
// with WithSessionIDValidation NOT called (default false), a data message whose SessionID does not
// match this connection's configured SessionID is still delivered to the handler.
func TestDeliverOwnedFrame_SessionIDValidation_DefaultOff(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	// Connection's configured SessionID is 0xFFFF; this inbound message carries 0x0001 (a mismatch).
	mismatched, err := NewDataMessage(1, 1, false, 0x0001, [4]byte{0, 0, 0, 1}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mismatched)))

	select {
	case got := <-handlerCh:
		require.Equal(t, uint16(0x0001), got.SessionID(), "validation off delivers regardless of SessionID")
	default:
		t.Fatal("with validation off, a mismatched-SessionID message must still be delivered")
	}

	require.Empty(t, c.cur.Load().sendCh, "validation off must never emit an S9F1")
}

// TestDeliverOwnedFrame_SessionIDValidation_EnabledMismatchDropped proves that, with validation on,
// a NON-S9F1 message whose SessionID does not match is dropped (never delivered) AND answered with
// an S9F1 whose body is the offending message's 10-byte MHEAD (SEMI E5 §10.13), sent from this
// connection's own SessionID.
func TestDeliverOwnedFrame_SessionIDValidation_EnabledMismatchDropped(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	require.NoError(t, c.UpdateConfigOptions(WithSessionIDValidation(true)))

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	mismatched, err := NewDataMessage(1, 1, false, 0x0001, [4]byte{0, 0, 0, 7}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, mismatched)))

	// Dropped: the handler must NOT fire.
	select {
	case <-handlerCh:
		t.Fatal("a mismatched-SessionID message must be dropped, not delivered to the handler")
	default:
	}

	// Answered with exactly one S9F1 (enqueued on the async send channel; no drainer runs in this
	// unit harness, so the enqueued frame stays inspectable).
	e := c.cur.Load()
	require.Len(t, e.sendCh, 1, "a mismatched-SessionID message must be answered with exactly one S9F1")
	req := <-e.sendCh
	reply, ok := req.msg.(*DataMessage)
	require.True(t, ok, "the auto-reply is a DataMessage")
	require.Equal(t, uint8(9), reply.Stream(), "auto-reply stream is 9")
	require.Equal(t, uint8(1), reply.Function(), "auto-reply function is 1 (S9F1)")
	require.Equal(t, c.SessionID(), reply.SessionID(), "S9F1 is sent from this connection's own SessionID")

	// SEMI E5 §10.13: the S9F1 BODY is the 10-byte binary MHEAD of the offending message — asserted
	// explicitly so an empty-body S9F1 would fail (a weaker S=9/F=1-only check would not catch that).
	item, err := reply.Item()
	require.NoError(t, err)
	body, err := item.ToBinary()
	require.NoError(t, err)
	mhead := mismatched.HeaderBytes()
	require.Equal(t, mhead[:], body, "S9F1 body must equal the offending message's 10-byte MHEAD")
}

// TestDeliverOwnedFrame_SessionIDValidation_EnabledMatchDelivered proves that, with validation on, a
// message whose SessionID DOES match is delivered normally and triggers no S9F1.
func TestDeliverOwnedFrame_SessionIDValidation_EnabledMatchDelivered(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	require.NoError(t, c.UpdateConfigOptions(WithSessionIDValidation(true)))

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	matched, err := NewDataMessage(1, 1, false, c.SessionID(), [4]byte{0, 0, 0, 5}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, matched)))

	select {
	case got := <-handlerCh:
		require.Equal(t, c.SessionID(), got.SessionID(), "a matching-SessionID message is delivered verbatim")
	default:
		t.Fatal("a matching-SessionID message must be delivered to the handler")
	}

	require.Empty(t, c.cur.Load().sendCh, "a matching-SessionID message triggers no S9F1")
}

// TestDeliverOwnedFrame_SessionIDValidation_S9F1Exempted pins the exemption's exact behavior: with
// validation on, an inbound S9F1 with a MISMATCHED SessionID is (a) delivered to the handler
// normally, and (b) does NOT trigger an outbound S9F1 in response (no notification loop). Both
// assertions are required — this is the one place that fixes "delivered, no auto-reply" as the
// intended outcome (matching WithSessionIDValidation's and checkSessionID's doc comments).
func TestDeliverOwnedFrame_SessionIDValidation_S9F1Exempted(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	require.NoError(t, c.UpdateConfigOptions(WithSessionIDValidation(true)))

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) {
		handlerCh <- msg
	})

	// An inbound S9F1 whose own SessionID (0x0001) mismatches the connection's (0xFFFF). Its body is
	// a 10-byte MHEAD per SEMI E5 §10.13.
	var offendingHeader [10]byte
	inboundS9F1, err := NewDataMessage(9, 1, false, 0x0001, [4]byte{0, 0, 0, 9}, secs2.B(offendingHeader[:]))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, inboundS9F1)))

	// (a) Delivered to the handler normally, mismatched SessionID and all.
	select {
	case got := <-handlerCh:
		require.Equal(t, uint8(9), got.Stream(), "delivered message is S9")
		require.Equal(t, uint8(1), got.Function(), "delivered message is S9F1")
		require.Equal(t, uint16(0x0001), got.SessionID(), "inbound S9F1 delivered verbatim despite mismatched SessionID")
	default:
		t.Fatal("an inbound S9F1 must be delivered to the handler, not dropped")
	}

	// (b) No outbound S9F1 in response — the exemption prevents an S9F1-answers-S9F1 loop.
	require.Empty(t, c.cur.Load().sendCh, "an inbound S9F1 must not trigger an outbound S9F1 reply")
}

// TestDeliverOwnedFrame_TraceTraffic_LogsReceivedMessage proves that with WithTraceTraffic
// enabled, a successfully decoded inbound data message logs a Debug line carrying the message's
// SessionID, System Bytes, and hex dump. Mirrors TestWriteFrame_TraceTraffic_LogsSentFrame's
// assertion style (hsms/connection_send_test.go): assert on the actual logged payload, not just
// that some Debug call happened.
func TestDeliverOwnedFrame_TraceTraffic_LogsReceivedMessage(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	mockLog := loggertest.NewMockLogger()
	mockLog.On("Debug", mock.Anything, mock.Anything).Return()
	c.cfg.Load().logger = mockLog
	c.cfg.Load().traceTraffic = true

	msg, err := NewDataMessage(1, 1, false, c.SessionID(), [4]byte{0, 0, 0, 30}, secs2.NewASCIIItem("PING"))
	require.NoError(t, err)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, msg)))

	require.Len(t, mockLog.Calls, 1)
	call := mockLog.Calls[0]
	require.Equal(t, "Debug", call.Method)
	keyvals, ok := call.Arguments[1].([]any)
	require.True(t, ok, "Debug's variadic keysAndValues must be forwarded as a []any slice")
	require.Contains(t, keyvals, c.SessionID(), "the logged payload must carry the message's SessionID")
	require.Contains(t, keyvals, msg.SystemBytes(), "the logged payload must carry the message's System Bytes")
	require.Contains(t, keyvals, hexDump(msg.ToBytes()), "the logged payload must carry the exact frame hex dump")
}

// TestDeliverOwnedFrame_TraceTraffic_LogsDecodeFailure proves that with WithTraceTraffic
// enabled, an inbound frame that fails to decode logs a Debug line carrying the decode error and
// the raw frame's hex dump.
func TestDeliverOwnedFrame_TraceTraffic_LogsDecodeFailure(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)
	mockLog := loggertest.NewMockLogger()
	mockLog.On("Debug", mock.Anything, mock.Anything).Return()
	c.cfg.Load().logger = mockLog
	c.cfg.Load().traceTraffic = true

	garbage := []byte{0x01, 0x02, 0x03} // too short to decode
	err := c.DeliverOwnedFrame(garbage)
	require.Error(t, err)

	require.Len(t, mockLog.Calls, 1)
	call := mockLog.Calls[0]
	require.Equal(t, "Debug", call.Method)
	keyvals, ok := call.Arguments[1].([]any)
	require.True(t, ok, "Debug's variadic keysAndValues must be forwarded as a []any slice")
	require.Contains(t, keyvals, err, "the logged payload must carry the exact decode error")
	require.Contains(t, keyvals, hexDump(garbage), "the logged payload must carry the raw frame hex dump")
}

// TestDeliverOwnedFrame_DecodeErrCount proves an inbound frame that fails to decode increments
// DecodeErrCount exactly once and does NOT increment DataMsgRecvCount (the receive chokepoint is
// only reached after a successful decode).
func TestDeliverOwnedFrame_DecodeErrCount(t *testing.T) {
	c, _ := newTestSendConn(t, SelectedState)

	garbage := []byte{0x01, 0x02, 0x03}
	err := c.DeliverOwnedFrame(garbage)
	require.Error(t, err)

	require.Equal(t, uint64(1), c.metrics.DecodeErrCount())
	require.Equal(t, uint64(0), c.metrics.DataMsgRecvCount())
}
