package hsms

import (
	"testing"

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
