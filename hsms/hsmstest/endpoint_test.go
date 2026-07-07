package hsmstest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/hsms/hsmstest"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStreamSECS2Message is a minimal external secs2.SECS2Message implementation used to reach
// an invalid-stream case that secs2.NewMessage cannot produce (its own StreamCode() accessor
// masks the high bit).
type fakeStreamSECS2Message struct {
	stream byte
}

func (m fakeStreamSECS2Message) StreamCode() byte   { return m.stream }
func (m fakeStreamSECS2Message) FunctionCode() byte { return 1 }
func (m fakeStreamSECS2Message) WaitBit() bool      { return false }
func (m fakeStreamSECS2Message) Item() secs2.Item   { return secs2.NewEmptyItem() }

var _ secs2.SECS2Message = fakeStreamSECS2Message{}

func erroredItem(t *testing.T) secs2.Item {
	t.Helper()
	item := secs2.NewIntItem(3) // invalid byteSize -> deferred error
	require.Error(t, item.Error())

	return item
}

func TestFakeEndpoint_HandlerRepliesDuringDeliver(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	var replyItem secs2.Item

	ep.AddDataMessageHandler(func(msg *hsms.DataMessage, e hsms.SECS2Endpoint) {
		item, err := msg.Item()
		require.NoError(t, err)
		replyItem = item
		err = e.ReplyDataMessage(context.Background(), msg, secs2.A("pong"))
		require.NoError(t, err)
	})

	primary, err := hsms.NewDataMessage(1, 1, false, 7, [4]byte{0, 0, 0, 1}, secs2.A("ping"))
	require.NoError(t, err)

	ep.Deliver(primary)

	assert.NotNil(t, replyItem)
	sent := ep.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, "ReplyDataMessage", sent[0].Method)
	assert.Equal(t, primary, sent[0].Primary)
	assert.Equal(t, byte(2), sent[0].Message.Function()) // primary function 1 -> reply function 2

	item, err := sent[0].Message.Item()
	require.NoError(t, err)
	assert.True(t, secs2.Equal(secs2.A("pong"), item))
}

func TestFakeEndpoint_ScriptReply_DrivesSynchronousSend(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	scripted, err := hsms.NewDataMessage(1, 2, false, 0, [4]byte{}, secs2.A("scripted-reply"))
	require.NoError(t, err)
	ep.ScriptReply(scripted, nil)

	got, err := ep.SendDataMessage(context.Background(), 1, 1, true, secs2.A("req"))
	require.NoError(t, err)
	assert.Same(t, scripted, got)

	sent := ep.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, "SendDataMessage", sent[0].Method)
}

func TestFakeEndpoint_Deliver_FanOutMultipleHandlers(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	var calls []int
	var mu sync.Mutex

	for i := range 3 {
		ep.AddDataMessageHandler(func(msg *hsms.DataMessage, e hsms.SECS2Endpoint) {
			mu.Lock()
			calls = append(calls, i)
			mu.Unlock()
		})
	}

	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)
	ep.Deliver(msg)

	assert.ElementsMatch(t, []int{0, 1, 2}, calls)
}

// TestFakeEndpoint_ConcurrentDeliverAndSent exercises Deliver/Sent/AddDataMessageHandler under
// -race to prove the snapshot-then-unlock-then-invoke locking discipline is race-free.
func TestFakeEndpoint_ConcurrentDeliverAndSent(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ep.AddDataMessageHandler(func(*hsms.DataMessage, hsms.SECS2Endpoint) {})
			ep.Deliver(msg)
			_ = ep.Sent()
			_ = ep.SendDataMessageAsync(context.Background(), 1, 1, false, secs2.A("x"))
		})
	}
	wg.Wait()
}

// TestFakeEndpoint_HandlerReplyDuringDeliver_NoDeadlock proves Deliver releases its lock before
// invoking handlers: a handler calling back into ReplyDataMessage (which itself takes f.mu) must
// not deadlock. A short timeout fails fast if the lock-ordering fix regresses.
func TestFakeEndpoint_HandlerReplyDuringDeliver_NoDeadlock(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	ep.AddDataMessageHandler(func(msg *hsms.DataMessage, e hsms.SECS2Endpoint) {
		_ = e.ReplyDataMessage(context.Background(), msg, secs2.NewEmptyItem())
	})

	msg, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		ep.Deliver(msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver deadlocked when handler called back into ReplyDataMessage")
	}
}

// ────────────────────────────────────────────────────────────────
// W-bit gating (fire-and-forget vs synchronous)
// ────────────────────────────────────────────────────────────────

func TestFakeEndpoint_SendDataMessage_FireAndForget_NoScript(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	got, err := ep.SendDataMessage(context.Background(), 1, 1, false, secs2.A("x"))
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFakeEndpoint_SendDataMessage_ReplyExpected_NoScript(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	_, err := ep.SendDataMessage(context.Background(), 1, 1, true, secs2.A("x"))
	require.ErrorIs(t, err, hsmstest.ErrNoScriptedReply)
}

func TestFakeEndpoint_SendSECS2Message_FireAndForget_NoScript(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	got, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 1, false, secs2.A("x")))
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFakeEndpoint_SendSECS2Message_ReplyExpected_NoScript(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	_, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 1, true, secs2.A("x")))
	require.ErrorIs(t, err, hsmstest.ErrNoScriptedReply)
}

// ────────────────────────────────────────────────────────────────
// Invalid-constructor regression matrix
// ────────────────────────────────────────────────────────────────

func TestFakeEndpoint_SendDataMessage_InvalidConstruction(t *testing.T) {
	t.Parallel()

	t.Run("invalid stream", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		_, err := ep.SendDataMessage(context.Background(), 200, 1, false, secs2.NewEmptyItem())
		require.ErrorIs(t, err, hsms.ErrInvalidStreamCode)
		assert.Empty(t, ep.Sent())
	})

	t.Run("W-bit on even function", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		scripted, scriptErr := hsms.NewDataMessage(1, 2, false, 0, [4]byte{}, secs2.NewEmptyItem())
		require.NoError(t, scriptErr)
		ep.ScriptReply(scripted, nil)

		_, err := ep.SendDataMessage(context.Background(), 1, 2, true, secs2.NewEmptyItem())
		require.ErrorIs(t, err, hsms.ErrInvalidRspMsg)
		assert.Empty(t, ep.Sent())

		// The scripted reply must still be available: the invalid call above must not have
		// popped it.
		got, err := ep.SendDataMessage(context.Background(), 1, 1, true, secs2.NewEmptyItem())
		require.NoError(t, err)
		assert.Same(t, scripted, got)
	})

	t.Run("errored item", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		scripted, scriptErr := hsms.NewDataMessage(1, 2, false, 0, [4]byte{}, secs2.NewEmptyItem())
		require.NoError(t, scriptErr)
		ep.ScriptReply(scripted, nil)

		_, err := ep.SendDataMessage(context.Background(), 1, 1, true, erroredItem(t))
		require.Error(t, err)
		assert.Empty(t, ep.Sent())

		got, err := ep.SendDataMessage(context.Background(), 1, 1, true, secs2.NewEmptyItem())
		require.NoError(t, err)
		assert.Same(t, scripted, got)
	})
}

func TestFakeEndpoint_SendDataMessageAsync_InvalidConstruction(t *testing.T) {
	t.Parallel()

	t.Run("invalid stream", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		err := ep.SendDataMessageAsync(context.Background(), 200, 1, false, secs2.NewEmptyItem())
		require.ErrorIs(t, err, hsms.ErrInvalidStreamCode)
		assert.Empty(t, ep.Sent())
	})

	t.Run("W-bit on even function", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		err := ep.SendDataMessageAsync(context.Background(), 1, 2, true, secs2.NewEmptyItem())
		require.ErrorIs(t, err, hsms.ErrInvalidRspMsg)
		assert.Empty(t, ep.Sent())
	})

	t.Run("errored item", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		err := ep.SendDataMessageAsync(context.Background(), 1, 1, false, erroredItem(t))
		require.Error(t, err)
		assert.Empty(t, ep.Sent())
	})
}

func TestFakeEndpoint_SendSECS2Message_InvalidConstruction(t *testing.T) {
	t.Parallel()

	t.Run("invalid stream via custom SECS2Message", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		_, err := ep.SendSECS2Message(context.Background(), fakeStreamSECS2Message{stream: 200})
		require.ErrorIs(t, err, hsms.ErrInvalidStreamCode)
		assert.Empty(t, ep.Sent())
	})

	t.Run("W-bit on even function", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		scripted, scriptErr := hsms.NewDataMessage(1, 2, false, 0, [4]byte{}, secs2.NewEmptyItem())
		require.NoError(t, scriptErr)
		ep.ScriptReply(scripted, nil)

		_, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 2, true, secs2.NewEmptyItem()))
		require.ErrorIs(t, err, hsms.ErrInvalidRspMsg)
		assert.Empty(t, ep.Sent())

		got, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 1, true, secs2.NewEmptyItem()))
		require.NoError(t, err)
		assert.Same(t, scripted, got)
	})

	t.Run("errored item", func(t *testing.T) {
		t.Parallel()
		ep := hsmstest.NewFakeEndpoint()
		scripted, scriptErr := hsms.NewDataMessage(1, 2, false, 0, [4]byte{}, secs2.NewEmptyItem())
		require.NoError(t, scriptErr)
		ep.ScriptReply(scripted, nil)

		_, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 1, true, erroredItem(t)))
		require.Error(t, err)
		assert.Empty(t, ep.Sent())

		got, err := ep.SendSECS2Message(context.Background(), secs2.NewMessage(1, 1, true, secs2.NewEmptyItem()))
		require.NoError(t, err)
		assert.Same(t, scripted, got)
	})
}

func TestFakeEndpoint_ReplyDataMessage_InvalidConstruction(t *testing.T) {
	t.Parallel()

	primary, err := hsms.NewDataMessage(1, 1, false, 0, [4]byte{}, secs2.NewEmptyItem())
	require.NoError(t, err)

	ep := hsmstest.NewFakeEndpoint()
	err = ep.ReplyDataMessage(context.Background(), primary, erroredItem(t))
	require.Error(t, err)
	assert.Empty(t, ep.Sent())
}

func TestFakeEndpoint_ForwardDataMessage_RecordsVerbatim(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint(hsmstest.WithSessionID(7))

	// Pre-built message with caller-chosen System Bytes — the forward contract preserves them.
	sb := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	msg, err := hsms.NewDataMessage(1, 1, true, 42, sb, secs2.A("ping"))
	require.NoError(t, err)

	require.NoError(t, ep.ForwardDataMessage(context.Background(), msg))
	require.NoError(t, ep.ForwardDataMessageAsync(context.Background(), msg))

	sent := ep.Sent()
	require.Len(t, sent, 2)

	assert.Equal(t, "ForwardDataMessage", sent[0].Method)
	assert.Same(t, msg, sent[0].Message, "the fake must record the pre-built message verbatim, not a rebuilt copy")
	assert.Equal(t, sb, sent[0].Message.SystemBytes(), "forwarded System Bytes are preserved, not regenerated")
	assert.Equal(t, uint16(42), sent[0].Message.SessionID(), "forwarded session ID is preserved verbatim")

	assert.Equal(t, "ForwardDataMessageAsync", sent[1].Method)
	assert.Same(t, msg, sent[1].Message)
}

func TestFakeEndpoint_ForwardDataMessage_NilMessage(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()

	require.ErrorIs(t, ep.ForwardDataMessage(context.Background(), nil), hsms.ErrNilMessage)
	require.ErrorIs(t, ep.ForwardDataMessageAsync(context.Background(), nil), hsms.ErrNilMessage)
	assert.Empty(t, ep.Sent(), "a nil forward records nothing")
}

// TestFakeEndpoint_ScriptReply_MalformedReplyMirrorsRealDecodeError guards that the fake mirrors
// the real session's reply-path decode check: a scripted reply that frames but whose SECS-II body
// fails to decode is surfaced as (msg, decodeErr), not a clean (msg, nil). Without this a consumer
// test scripting a malformed reply would see the fake diverge from a live endpoint.
func TestFakeEndpoint_ScriptReply_MalformedReplyMirrorsRealDecodeError(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	bad := hsmstest.MalformedDataMessage(1, 14, false) // valid framing, undecodable body
	ep.ScriptReply(bad, nil)                           // scripted as a clean success

	got, err := ep.SendDataMessage(context.Background(), 1, 13, true, secs2.A("req"))
	require.Error(t, err, "an undecodable scripted reply must surface its decode error")
	require.ErrorIs(t, err, bad.DecodeErr())
	require.NotNil(t, got, "the reply must be returned alongside the error")
	assert.Equal(t, uint8(1), got.Stream())
	assert.Equal(t, uint8(14), got.Function())
}

// TestFakeEndpoint_ScriptReply_WellFormedReplyReturnsNilErr guards the happy path: a scripted reply
// whose body decodes cleanly still returns (msg, nil) — the decode check is a no-op false-positive check.
func TestFakeEndpoint_ScriptReply_WellFormedReplyReturnsNilErr(t *testing.T) {
	t.Parallel()

	ep := hsmstest.NewFakeEndpoint()
	good, err := hsms.NewDataMessage(1, 14, false, 0, [4]byte{}, secs2.A("ok"))
	require.NoError(t, err)
	ep.ScriptReply(good, nil)

	got, sendErr := ep.SendDataMessage(context.Background(), 1, 13, true, secs2.A("req"))
	require.NoError(t, sendErr, "a well-formed scripted reply must return a nil error")
	assert.Same(t, good, got)
}
