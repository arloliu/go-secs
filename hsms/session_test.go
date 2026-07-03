package hsms

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// mustDataMsg creates a minimal DataMessage for use in tests.
func mustDataMsg(t *testing.T) *DataMessage {
	t.Helper()

	msg, err := NewDataMessage(1, 1, true, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)

	return msg
}

// TestSession_FanOutDeliversSamePointer registers two DataMessageHandlers, delivers a
// message via recvDataMsg, and asserts that both handlers observe the exact same
// *DataMessage pointer (no Clone — D7).
func TestSession_FanOutDeliversSamePointer(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	var got1, got2 *DataMessage
	var wg sync.WaitGroup
	wg.Add(2)
	s.AddDataMessageHandler(
		func(m *DataMessage, ep SECS2Endpoint) { got1 = m; wg.Done() },
		func(m *DataMessage, ep SECS2Endpoint) { got2 = m; wg.Done() },
	)

	msg := mustDataMsg(t)
	s.recvDataMsg(msg)
	wg.Wait()

	require.Same(t, msg, got1)
	require.Same(t, got1, got2, "every handler gets the SAME immutable pointer (no Clone, D7)")
}

// TestSession_SessionID verifies the SECS2Endpoint identity accessor (SessionID, the sole
// SessionID method on the endpoint surface) reports the session's ID through the interface.
func TestSession_SessionID(t *testing.T) {
	rt := newMockRuntime(t)
	var ep SECS2Endpoint = newSession(0x1234, rt, &sysBytesGen{})
	require.Equal(t, uint16(0x1234), ep.SessionID())
}

// TestSession_J5_BlockedChannelDoesNotWedgeFanOut verifies that a full/stalled channel
// handler cannot block recvDataMsg past connection teardown (J5).
//
// Design: an unbuffered channel with no reader is registered. The goroutine running
// recvDataMsg blocks on the channel-delivery select. Closing rt.Done() must unblock it.
//
// Teeth-check: temporarily omit `case <-s.rt.Done()` from the channel-delivery select
// in session.go and run this test — it times out at 2 s, confirming J5 is real.
func TestSession_J5_BlockedChannelDoesNotWedgeFanOut(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	// Unbuffered channel with no reader: ch <- msg would block indefinitely without
	// the <-rt.Done() guard in the select.
	blockedCh := make(chan *DataMessage)
	s.addChanHandler(blockedCh)

	msg := mustDataMsg(t)

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		s.recvDataMsg(msg)
	}()

	// Give the goroutine time to enter the channel-delivery select before closing Done.
	// Even if closeDone fires first, the early-exit in recvDataMsg returns immediately —
	// the test still passes because the fan-out did not block permanently.
	time.Sleep(20 * time.Millisecond)
	rt.closeDone()

	select {
	case <-recvDone:
		// J5 satisfied: fan-out unblocked when rt.Done() closed.
	case <-time.After(2 * time.Second):
		t.Fatal("J5 violated: recvDataMsg did not return after rt.Done() was closed")
	}
}

// TestSession_SendDataMessage_DelegatesToWriteMessage is a smoke test confirming that
// SendDataMessage builds a DataMessage and calls rt.WriteMessage with it.
func TestSession_SendDataMessage_DelegatesToWriteMessage(t *testing.T) {
	rt := newMockRuntime(t)
	s := newSession(0xFFFF, rt, &sysBytesGen{})

	// Canned reply the mock will return.
	replyMsg, err := NewDataMessage(1, 2, false, 0xFFFF, [4]byte{0, 0, 0, 1}, secs2.NewEmptyItem())
	require.NoError(t, err)
	rt.writeReply = replyMsg

	reply, err := s.SendDataMessage(context.Background(), 1, 1, true, secs2.NewEmptyItem())
	require.NoError(t, err)

	rt.mu.Lock()
	writeCalled := rt.writeCalled
	writeMsg := rt.writeMsg
	rt.mu.Unlock()

	require.True(t, writeCalled, "WriteMessage must be called")
	require.NotNil(t, writeMsg, "WriteMessage must receive a message")

	dm, ok := writeMsg.(*DataMessage)
	require.True(t, ok, "message passed to WriteMessage must be a *DataMessage")
	require.Equal(t, uint8(1), dm.Stream())
	require.Equal(t, uint8(1), dm.Function())
	require.True(t, dm.WaitBit())
	require.Equal(t, replyMsg, reply, "SendDataMessage must return the reply from WriteMessage")
}
