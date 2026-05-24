package hsmsss

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/stretchr/testify/require"
)

// These tests guard the close-race repair landed in this commit. See
// tmp/reply-channel-close-race-plan-v2.md §4 step 5 for the design.
//
// Test A (deterministic teeth): dropAllReplyMsgs must not panic a
// receiverTask that holds a previously-loaded reply channel and then sends.
// Test B (orphan-reply): replyToSender must not leak a DataMessage when the
// post-Clear interleaving fires.
// Test C (orphan-replyErrs): replyErrToSender must not leak a replyErrs
// entry when the post-Clear interleaving fires between Load and Store.

// TestDropAllReplyMsgs_NoPanicOnConcurrentSend reproduces the original
// 'send on closed channel' panic shape: a third goroutine loads the reply
// channel, dropAllReplyMsgs runs, then the third goroutine attempts a
// non-blocking send. Under the OLD close(ch) code the send would panic.
// Under the new drain-then-nil-send shape, neither branch panics.
func TestDropAllReplyMsgs_NoPanicOnConcurrentSend(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	id := uint32(42)
	_ = conn.addReplyExpectedMsg(id)

	rawCh, loaded := conn.replyMsgChans.Load(id)
	require.True(loaded)
	require.NotNil(rawCh)

	loadedCh := make(chan struct{})
	dropDone := make(chan struct{})
	sendDone := make(chan error, 1)

	go func() {
		// Signal that we hold the channel reference, then block until drop is done.
		close(loadedCh)
		<-dropDone

		// Attempt a non-blocking send on the previously-loaded channel.
		// Under the OLD close(ch) code this panics; under the new code it
		// either lands in the buffer (where dropAllReplyMsgs left room) or
		// hits default if the nil sentinel is still buffered. Either way:
		// no panic.
		defer func() {
			if r := recover(); r != nil {
				sendDone <- errors.New("panic: send on closed channel reproduced")
				return
			}
			sendDone <- nil
		}()

		msg, mErr := hsms.NewDataMessage(1, 2, false, 0, hsms.ToSystemBytes(id), nil)
		if mErr != nil {
			sendDone <- mErr
			return
		}
		select {
		case rawCh <- msg:
			// Send landed; nothing will drain. Free to keep the pool balanced.
			msg.Free()
		default:
			// Send did not land (buffer full of the nil sentinel). Free directly.
			msg.Free()
		}
	}()

	<-loadedCh
	conn.dropAllReplyMsgs()
	close(dropDone)

	select {
	case sendErr := <-sendDone:
		require.NoError(sendErr, "dropAllReplyMsgs must not cause a panic on a concurrent send")
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent sender goroutine did not finish")
	}
}

// TestReplyToSender_NoOrphanLeakAfterDropAll exercises the canonical
// orphan-reply scenario: replyToSender has Load'd the reply channel, then
// sendMsg's ctx.Done branch fires and calls removeReplyExpectedMsg (deleting
// the map entry). replyToSender's subsequent successful `replyChan <- msg`
// would land in a channel that is no longer reachable from the map or from
// sendMsg — the cap-1 buffer would hold the DataMessage indefinitely and
// the pool would never see it Free'd.
//
// The orphan-defense added in this commit re-checks the map after the send
// and drains+Frees the orphan. We verify directly that the channel buffer
// is empty after replyToSender returns.
func TestReplyToSender_NoOrphanLeakAfterDropAll(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	id := uint32(101)
	_ = conn.addReplyExpectedMsg(id)

	rawCh, loaded := conn.replyMsgChans.Load(id)
	require.True(loaded)
	require.NotNil(rawCh)

	// Hook fires after replyToSender's Load and before its send select.
	// We simulate sendMsg's ctx.Done branch by removing the reply entry
	// from the map — the local replyChan still references the buffer, but
	// nothing else does.
	var hookOnce sync.Once
	replyToSenderPreSendHook = func() {
		hookOnce.Do(func() {
			conn.removeReplyExpectedMsg(id)
		})
	}
	t.Cleanup(func() { replyToSenderPreSendHook = nil })

	reply, err := hsms.NewDataMessage(1, 2, false, 0, hsms.ToSystemBytes(id), nil)
	require.NoError(err)

	// Drive replyToSender on this goroutine; the hook is synchronous, so
	// by the time replyToSender returns, the orphan-drain (if any) has run.
	conn.replyToSender(reply)

	// After the race, the map must be empty (we removed it in the hook)
	// and the previously-loaded channel must be drained (by the orphan
	// defense in replyToSender). Without the defense, the buffer would
	// hold our reply message indefinitely.
	require.Equal(0, conn.replyMsgChans.Size(),
		"replyMsgChans must be empty after the simulated sendMsg ctx.Done")
	require.Equal(0, len(rawCh),
		"orphan-defense must drain the stale channel so the reply message returns to the pool")
}

// TestReplyErrToSender_NoOrphanReplyErrsAfterDropAll exercises the
// post-Clear interleaving on replyErrToSender: Load wins, then
// dropAllReplyMsgs.Clear() runs, then Store lands in a freshly-cleared
// map. The ctx.Done branch of the select must Delete the stranded entry so
// the clean-shutdown invariant holds.
func TestReplyErrToSender_NoOrphanReplyErrsAfterDropAll(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	// Create and immediately cancel a per-connection context so connCtx()
	// returns a Done context. renewConnCtx initialises connCtxVal /
	// connCancelFn — neither is populated by NewConnection alone.
	conn.renewConnCtx()
	if h := conn.connCancelFn.Load(); h != nil {
		h.cancel()
	} else {
		t.Fatal("renewConnCtx did not populate connCancelFn")
	}

	id := uint32(202)
	_ = conn.addReplyExpectedMsg(id)

	// Hook fires between Load and Store inside replyErrToSender. We force
	// the post-Clear interleaving by running dropAllReplyMsgs from the
	// hook: that clears replyErrs immediately before our Store lands. With
	// connCtx already cancelled, the select then takes the ctx.Done branch
	// — exactly the Load -> Clear -> Store -> ctx.Done sequence the defense
	// targets.
	var hookOnce sync.Once
	replyErrToSenderPreStoreHook = func() {
		hookOnce.Do(func() {
			conn.dropAllReplyMsgs()
		})
	}
	t.Cleanup(func() { replyErrToSenderPreStoreHook = nil })

	sentinel := errors.New("orphan-replyErrs sentinel")
	conn.replyErrToSender(hsms.NewLinktestReq(hsms.ToSystemBytes(id)), sentinel)

	require.Equal(0, conn.replyErrs.Size(),
		"replyErrToSender must Delete its own replyErrs entry when the ctx-done branch wins after a racing Clear")
	require.Equal(0, conn.replyMsgChans.Size(),
		"replyMsgChans must be empty after dropAllReplyMsgs")
}

// TestSendMsg_DrainOrphanReply exercises the terminal-branch defense that
// sendMsg installs as a defer. The scenario it guards: replyToSender's
// post-send Load saw the map entry as still registered (so replyToSender's
// own orphan-drain did nothing), then sendMsg's terminal branch
// (ctx.Done / T3-T6 timer / Phase 1 sendErr) fires and removes the entry
// without consuming the buffered reply. Without the defer-drain in sendMsg
// the DataMessage stays orphaned in the cap-1 buffer; with the defer-drain,
// it is Free'd here. We test the drain primitive directly: a stale reply is
// buffered into a registered channel, drainOrphanReply runs, and we verify
// the buffer is empty afterwards (the DataMessage went back to the pool via
// Free).
func TestSendMsg_DrainOrphanReply(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	id := uint32(404)
	_ = conn.addReplyExpectedMsg(id)
	rawCh, loaded := conn.replyMsgChans.Load(id)
	require.True(loaded)

	// Simulate replyToSender's successful send into the buffer.
	orphan, err := hsms.NewDataMessage(1, 2, false, 0, hsms.ToSystemBytes(id), nil)
	require.NoError(err)
	select {
	case rawCh <- orphan:
	default:
		t.Fatal("could not pre-buffer the orphan reply; channel unexpectedly full")
	}
	require.Equal(1, len(rawCh), "pre-condition: buffer holds the orphan reply")

	// Invoke the drain (this is what sendMsg's defer does).
	conn.drainOrphanReply(id, rawCh)

	require.Equal(0, len(rawCh), "drainOrphanReply must remove the buffered reply")
	// nil-in-buffer must also be tolerated (the drop-path delivers nil).
	select {
	case rawCh <- nil:
	default:
		t.Fatal("could not pre-buffer nil; channel unexpectedly full")
	}
	conn.drainOrphanReply(id, rawCh)
	require.Equal(0, len(rawCh), "drainOrphanReply must also accept a nil in the buffer without panicking")

	// Drain on an empty buffer must be a no-op.
	conn.drainOrphanReply(id, rawCh)
	require.Equal(0, len(rawCh), "drainOrphanReply on empty buffer must be a no-op")
}

// TestSendMsg_DrainOrphanReply_ClearsReplyErrsOnNilDrain exercises the
// terminal-branch defense for the case where replyErrToSender has stored
// an err and successfully sent nil into a registered channel, its
// post-send identity check observed the channel as still registered (so
// it did NOT delete replyErrs), and sendMsg then chose a terminal branch
// (ctx.Done / T3-T6 timer) instead of consuming the nil via the
// reply-case. The defer's drainOrphanReply receives the nil; without
// the replyErrs.Delete on the nil path, the stored err would be
// stranded, violating assertCleanShutdown's replyErrs.Size() == 0
// invariant.
func TestSendMsg_DrainOrphanReply_ClearsReplyErrsOnNilDrain(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	id := uint32(606)
	_ = conn.addReplyExpectedMsg(id)
	rawCh, _ := conn.replyMsgChans.Load(id)

	// Simulate the state replyErrToSender leaves behind when its post-send
	// identity check sees the channel as still registered: an err in
	// replyErrs and a nil sentinel in the cap-1 buffer.
	sentinel := errors.New("orphan-replyErrs-from-terminal-drain sentinel")
	conn.replyErrs.Store(id, sentinel)
	select {
	case rawCh <- nil:
	default:
		t.Fatal("could not pre-buffer nil sentinel; channel unexpectedly full")
	}

	// Simulate sendMsg's terminal branch: remove the map entry then run
	// the defer. drainOrphanReply must drain the nil AND Delete replyErrs[id].
	conn.removeReplyExpectedMsg(id)
	conn.drainOrphanReply(id, rawCh)

	require.Equal(0, len(rawCh), "drainOrphanReply must consume the nil sentinel")
	require.Equal(0, conn.replyErrs.Size(),
		"drainOrphanReply must Delete replyErrs[id] when it drains a nil sentinel — "+
			"otherwise replyErrToSender's stored err is stranded after sendMsg's terminal branch fires")
}

// TestReplyErrToSender_NoOrphanReplyErrsWhenSendBranchWins exercises the
// counterpart of Test C: the send branch (not the ctx.Done branch) is the
// one that wins the select, and we must still Delete the stranded replyErrs
// entry that was Stored after a racing Clear. Setup: connCtx is NOT
// cancelled (so ctx.Done is not ready), the hook calls dropAllReplyMsgs
// then manually drains the nil it inserted (simulating sendMsg consuming
// the drop's signal) so the channel is empty when replyErrToSender's select
// arms. The select then picks `replyChan <- nil`; the new identity-compare
// must detect that the map no longer contains our channel and Delete the
// orphan err.
func TestReplyErrToSender_NoOrphanReplyErrsWhenSendBranchWins(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	cfg, err := NewConnectionConfig("localhost", 6666, WithHostRole())
	require.NoError(err)
	conn, err := NewConnection(ctx, cfg)
	require.NoError(err)

	// connCtx must be alive (not cancelled) so the send branch can win.
	conn.renewConnCtx()

	id := uint32(505)
	_ = conn.addReplyExpectedMsg(id)
	rawCh, _ := conn.replyMsgChans.Load(id)

	var hookOnce sync.Once
	replyErrToSenderPreStoreHook = func() {
		hookOnce.Do(func() {
			conn.dropAllReplyMsgs()
			// Simulate sendMsg consuming the drop's nil signal so the buffer
			// is empty when replyErrToSender's select arms; otherwise the
			// send arm would block on the full buffer and the test would
			// hang or fall back to ctx.Done.
			select {
			case <-rawCh:
			default:
			}
		})
	}
	t.Cleanup(func() { replyErrToSenderPreStoreHook = nil })

	sentinel := errors.New("orphan-replyErrs send-branch sentinel")
	conn.replyErrToSender(hsms.NewLinktestReq(hsms.ToSystemBytes(id)), sentinel)

	require.Equal(0, conn.replyErrs.Size(),
		"send-branch must Delete its own replyErrs entry when the map has been Cleared between Load and Store")
	require.Equal(0, conn.replyMsgChans.Size(),
		"replyMsgChans must be empty after dropAllReplyMsgs")
}
