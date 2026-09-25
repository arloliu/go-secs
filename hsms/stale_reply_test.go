package hsms

import (
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// stale_reply_test.go covers session.ReplyDataMessage's same-connection provenance check: it must
// refuse a reply to a primary that was received on a generation which has since ended, and must
// leave any other reply path (unstamped primary, a Derive().Build() copy, a primary stamped by
// another connection) unaffected.

// stampOrigin marks msg as having been received by c on generation gen —
// the same stamp deliverOwnedFrameOn applies to a message admitted through a live generation.
// Same-package field assignment stands in for that call site where a test needs a stamp with no
// live delivery to exercise (e.g. a primary attributed to a DIFFERENT connection than the one under test).
func stampOrigin(msg *DataMessage, c *connection, gen uint64) *DataMessage {
	msg.originIdent = c.ident
	msg.originGen = gen

	return msg
}

// TestReplyDataMessage_StaleGenerationAfterSuccessorSelected is the primary row: a primary
// delivered through DeliverOwnedFrameFromGeneration on genN (exercising deliverOwnedFrameOn's real
// stamp, not the stampOrigin test double), answered after genN ended and genSucc became live.
func TestReplyDataMessage_StaleGenerationAfterSuccessorSelected(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	sent := mustSendData(t, [4]byte{0, 0, 0, 61}, false)
	require.NoError(t, c.DeliverOwnedFrameFromGeneration(genN.id, ownedFrame(t, sent)))

	var primary *DataMessage
	select {
	case primary = <-handlerCh:
	default:
		t.Fatal("the primary must have reached the data handler")
	}

	endAndSucceed(c, genN, genSucc)

	err := c.ReplyDataMessage(t.Context(), primary, secs2.NewASCIIItem("late"))
	require.ErrorIs(t, err, ErrConnClosed, "a reply to a primary from an ended generation must be refused")
	require.Empty(t, genSucc.sendCh, "the stale reply must never land on the successor's queue")
	require.Equal(t, uint64(1), c.staleSend.Load(), "the refused reply must be counted")
}

// TestReplyDataMessage_LiveGeneration is the live row: the reply is sent while genN, the
// generation that received the primary, is still current.
// The plain SendAsync path already enqueues on whichever generation is current, and here that
// happens to still be genN — a regression guard for the identity check's happy path.
func TestReplyDataMessage_LiveGeneration(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)
	primary := stampOrigin(mustSendData(t, [4]byte{0, 0, 0, 62}, false), c, genN.id)

	err := c.ReplyDataMessage(t.Context(), primary, secs2.NewASCIIItem("ok"))
	require.NoError(t, err)
	require.Len(t, genN.sendCh, 1, "the reply must be enqueued on the live generation's own queue")
	require.Zero(t, c.staleSend.Load())
}

// TestReplyDataMessage_MatchingButEndedNoSuccessor is the "no successor" row: genN's ended latch
// is set but no successor has been published yet (c.cur still points at genN).
func TestReplyDataMessage_MatchingButEndedNoSuccessor(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)
	primary := stampOrigin(mustSendData(t, [4]byte{0, 0, 0, 63}, false), c, genN.id)
	genN.markEnded() // teardown started; no successor published yet

	err := c.ReplyDataMessage(t.Context(), primary, secs2.NewASCIIItem("late"))
	require.ErrorIs(t, err, ErrConnClosed,
		"a reply to a primary whose generation has ended must be refused even with no successor yet published")
}

// TestReplyDataMessage_PlainDeliverOwnedFrameIngress is the SECS-I-ingress row: the primary
// arrives through the PLAIN DeliverOwnedFrame entry point (gen-less, as secs1 uses), which stamps
// the connection's current generation just the same.
func TestReplyDataMessage_PlainDeliverOwnedFrameIngress(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	sent := mustSendData(t, [4]byte{0, 0, 0, 64}, false)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, sent))) // admitted while genN is current

	var delivered *DataMessage
	select {
	case delivered = <-handlerCh:
	default:
		t.Fatal("the primary must have reached the data handler")
	}

	endAndSucceed(c, genN, genSucc)

	err := c.ReplyDataMessage(t.Context(), delivered, secs2.NewASCIIItem("late"))
	require.ErrorIs(t, err, ErrConnClosed,
		"a reply to a primary admitted through the plain DeliverOwnedFrame ingress must still be refused once its generation ends")
	require.Empty(t, genSucc.sendCh)
}

// TestReplyDataMessage_PlainDeliverOwnedFrameIngress_LiveReply is the plain-ingress row's live
// counterpart: a primary admitted through the same gen-less entry point, answered while its
// admitting generation is still current, must send normally.
func TestReplyDataMessage_PlainDeliverOwnedFrameIngress_LiveReply(t *testing.T) {
	t.Parallel()

	c, genN, _ := newTwoGenConn(t)

	handlerCh := make(chan *DataMessage, 1)
	c.AddDataMessageHandler(func(msg *DataMessage, _ SECS2Endpoint) { handlerCh <- msg })

	sent := mustSendData(t, [4]byte{0, 0, 0, 69}, false)
	require.NoError(t, c.DeliverOwnedFrame(ownedFrame(t, sent)))

	var delivered *DataMessage
	select {
	case delivered = <-handlerCh:
	default:
		t.Fatal("the primary must have reached the data handler")
	}

	err := c.ReplyDataMessage(t.Context(), delivered, secs2.NewASCIIItem("ok"))
	require.NoError(t, err)
	require.Len(t, genN.sendCh, 1, "the reply must be enqueued on the still-live admitting generation's own queue")
	require.Zero(t, c.staleSend.Load())
}

// TestReplyDataMessage_WitherCopiesPreserveTheStaleCheck is the wither rows: each of
// WithSessionID / WithSystemBytes / WithID must copy the origin stamp, so a reply to the DERIVED
// copy is refused exactly like a reply to the original stale primary.
func TestReplyDataMessage_WitherCopiesPreserveTheStaleCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		apply func(*DataMessage) *DataMessage
	}{
		{"WithSessionID", func(m *DataMessage) *DataMessage { return m.WithSessionID(0x1234) }},
		{"WithSystemBytes", func(m *DataMessage) *DataMessage { return m.WithSystemBytes([4]byte{9, 9, 9, 9}) }},
		{"WithID", func(m *DataMessage) *DataMessage { return m.WithID(0xABCD) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, genN, genSucc := newTwoGenConn(t)
			primary := stampOrigin(mustSendData(t, [4]byte{0, 0, 0, 65}, false), c, genN.id)
			endAndSucceed(c, genN, genSucc)

			copyMsg := tt.apply(primary)

			err := c.ReplyDataMessage(t.Context(), copyMsg, secs2.NewASCIIItem("late"))
			require.ErrorIs(t, err, ErrConnClosed,
				"%s must copy the origin stamp, so a reply to the copy is refused the same way as the original", tt.name)
			require.Empty(t, genSucc.sendCh)
		})
	}
}

// TestReplyDataMessage_DeriveBuildCopyIsUnchecked is the Derive().Build() row: the builder path
// deliberately does NOT copy the origin stamp — the reply is sent unconditionally, even though the
// source primary is stale.
func TestReplyDataMessage_DeriveBuildCopyIsUnchecked(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	primary := stampOrigin(mustSendData(t, [4]byte{0, 0, 0, 66}, false), c, genN.id)
	endAndSucceed(c, genN, genSucc)

	derived, err := primary.Derive().Build()
	require.NoError(t, err)

	err = c.ReplyDataMessage(t.Context(), derived, secs2.NewASCIIItem("unchecked"))
	require.NoError(t, err, "a Derive().Build() copy is unchecked and must be sent even though the source primary is stale")
	require.Len(t, genSucc.sendCh, 1)
}

// TestReplyDataMessage_UnstampedPrimaryIsUnchecked is the unstamped row: a primary that was never
// stamped (originIdent nil) is unchecked, regardless of the connection's generation state.
func TestReplyDataMessage_UnstampedPrimaryIsUnchecked(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	primary := mustSendData(t, [4]byte{0, 0, 0, 67}, false) // never stamped
	endAndSucceed(c, genN, genSucc)

	err := c.ReplyDataMessage(t.Context(), primary, secs2.NewASCIIItem("unchecked"))
	require.NoError(t, err, "an unstamped primary must never be checked against a generation")
	require.Len(t, genSucc.sendCh, 1)
}

// TestReplyDataMessage_StampedByAnotherConnectionIsUnchecked is the cross-connection row: a
// primary stamped by a DIFFERENT connection than the one replying is unchecked (cross-connection
// reply policy is unchanged and out of scope here).
func TestReplyDataMessage_StampedByAnotherConnectionIsUnchecked(t *testing.T) {
	t.Parallel()

	c, genN, genSucc := newTwoGenConn(t)
	other, otherGenN, _ := newTwoGenConn(t)

	primary := stampOrigin(mustSendData(t, [4]byte{0, 0, 0, 68}, false), other, otherGenN.id)
	endAndSucceed(c, genN, genSucc) // c's OWN generation ends; primary's stamp names a different connection

	err := c.ReplyDataMessage(t.Context(), primary, secs2.NewASCIIItem("unchecked"))
	require.NoError(t, err, "a primary stamped by a different connection must not be checked against THIS connection's generations")
	require.Len(t, genSucc.sendCh, 1)
}
