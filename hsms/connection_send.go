package hsms

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/internal/pool"
)

// dropNotSelectedWarnInterval bounds how often the B3 not-selected drop is logged. The
// DataMsgDropNotSelectedCount metric (the single chokepoint) counts every drop exactly once;
// the Warn is rate-limited so a NotSelected reconnect storm cannot flood the log.
const dropNotSelectedWarnInterval = 1 * time.Second

// sendRequest is one fire-and-forget frame queued on epoch.sendCh (spec §5.5). It carries the
// immutable message only; the async sender goroutine (drainSendCh) frames and writes it.
type sendRequest struct {
	msg Message
}

// IsSelected reports whether the logical E37 FSM is currently in the Selected state — the B1
// data-send gate (spec §5.5). It reads the supervisor's lock-free atomic state via State(),
// so it is safe on the hot send path and nil-guards a not-yet-created supervisor.
func (c *connection) IsSelected() bool {
	return c.State() == SelectedState
}

// dropNotSelected records a B1 not-selected data-send refusal at the single B3 chokepoint: it
// increments DataMsgDropNotSelectedCount exactly once and emits a rate-limited Warn. Both send
// entry points (sendWaitReply and SendAsync) funnel every gated drop through here.
func (c *connection) dropNotSelected() {
	c.metrics.incDataMsgDropNotSelected()
	if c.dropWarn != nil && c.dropWarn.Allow() {
		c.cfg.Load().logger.Warn("hsms: data message dropped — connection not selected (B1 gate)",
			"drop_count", c.metrics.DataMsgDropNotSelectedCount())
	}
}

// buildFrameBuffers assembles the FRESH net.Buffers for one on-wire HSMS frame (spec §6.2): a
// 14-byte prefix (4-byte big-endian length = 10+bodyLen, then the 10-byte header) followed by
// the body's zero-copy sub-slices for a data message, or the single prefix slice for a control
// / empty-body message. A fresh slice is returned per call because writev consumes/advances it.
// The assembled bytes are byte-for-byte equal to msg.ToBytes().
//
// The body length is derived by summing the lengths of the body's Buffers (obtained once, which
// also memoizes the encoding) rather than calling Body.Len: for a list body Body.Len is a
// recursive EncodedLen walk that is NOT memoized, so summing the already-materialized buffers
// avoids re-walking the item tree (the prior code called Body.Len twice, then Buffers a third
// time).
func buildFrameBuffers(msg Message) net.Buffers {
	header := msg.HeaderBytes()

	// A fresh prefix per call (the Buffers slice is consumed/advanced by writev, §6.2).
	prefix := make([]byte, 14)
	copy(prefix[4:], header[:])

	if dm, ok := msg.(*DataMessage); ok {
		bodyBufs := dm.body.Buffers() // fresh per call — leaf slices alias the immutable body
		n := 0
		for _, b := range bodyBufs {
			n += len(b)
		}

		if n > 0 {
			binary.BigEndian.PutUint32(prefix[:4], uint32(10+n)) //nolint:gosec // n is a bounded body length
			bufs := make(net.Buffers, 0, 1+len(bodyBufs))
			bufs = append(bufs, prefix)
			bufs = append(bufs, bodyBufs...)

			return bufs
		}
	}

	// Control messages / empty-body data: header-only, length = 10, single-slice path.
	binary.BigEndian.PutUint32(prefix[:4], 10)

	return net.Buffers{prefix}
}

// writeFrame performs the synchronous framed writev of msg over THIS epoch's socket (spec §6.2).
// It builds the frame buffers (buildFrameBuffers), then hands them to the transport byte sink
// under e.writeMu so concurrent senders never interleave frames on the wire. The write is bound
// to the epoch's own conn (the I1 stale-epoch guard below), so a sender pinned to a superseded
// generation can never writev onto a successor's socket. The bytes written equal msg.ToBytes().
func (c *connection) writeFrame(ctx context.Context, e *epoch, msg Message) error {
	bufs := buildFrameBuffers(msg)

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	// I1 stale-epoch guard (PRIMARY fix): bind the write to THIS epoch's socket, captured up front
	// under writeMu. A synchronous sender pinned to THIS epoch (sendWaitReply loaded c.cur once) may
	// have stalled across a full teardown + reconnect and only now acquired writeMu. Capturing the
	// generation's own conn means the write below targets exactly epoch N's socket — which teardown
	// closed, so it errors — and can NEVER resolve epoch N+1's live socket. This closes the residual
	// TOCTOU that the ctx-only guard left: a sender descheduled AFTER passing an e.ctx check but
	// before the write could, with the old dynamic transport t.conn, writev onto the successor's
	// socket outside its writeMu (relying only on the OS writev lock for byte integrity).
	conn := e.liveConn()

	// Test seam (nil in production): allows the B2 teeth-test to simulate a Selected→NotSelected
	// transition — and the I1 teeth-test a teardown+reconnect — that lands after writeMu is acquired
	// but before the write, the only window that cannot be exercised without a seam. It runs AFTER
	// the conn capture so an I1 test can prove the write still targets the captured (epoch N) conn.
	if c.testHookAfterWriteLock != nil {
		c.testHookAfterWriteLock()
	}

	if conn == nil {
		// Teardown niled this generation's socket before we captured it — fail closed rather than
		// write onto nothing. The stale sender never reaches the wire; the reply wait unwinds via
		// ErrConnClosed exactly as if the ctx guard below had fired.
		return ErrConnClosed
	}

	// Fast-fail belt-and-suspenders: an already-cancelled generation ctx (Done closes at teardown
	// start) skips arming a deadline and attempting a doomed write. The conn binding above is the
	// actual race fix; this is a cheap early-out on the common teardown case.
	select {
	case <-e.ctx.Done():
		return ErrConnClosed
	default:
	}

	_, isData := msg.(*DataMessage)

	// B2 write-boundary re-check (data only, spec §5.5): a data message that passed the
	// B1 pre-register gate but finds the link no longer Selected at the write boundary
	// yields a counted, non-fatal ErrNotSelectedState — NEVER a teardown. This closes the
	// TOCTOU gap between the B1 gate and the actual transport Write under writeMu.
	if isData && !c.IsSelected() {
		c.dropNotSelected()
		return ErrNotSelectedState
	}

	// Bounded write (I5/CRIT-1): arm a write deadline so a wedged / zero-window peer cannot stall
	// this writev — and thus this generation's sole writer (and the auto-linktest that shares the
	// send path) — indefinitely. A non-positive writeTimeout disables the bound. The deadline is
	// cleared after the write (LIFO defer, still under writeMu) so it never leaks into the next one.
	if wt := c.cfg.Load().writeTimeout; wt > 0 {
		_ = c.tr.SetWriteDeadline(conn, time.Now().Add(wt))
		defer func() { _ = c.tr.SetWriteDeadline(conn, time.Time{}) }()
	}

	if err := c.tr.Write(ctx, conn, bufs); err != nil {
		// A write failure — a deadline exceeded on a wedged peer, or a broken socket — means this
		// generation's stream is dead, and a deadline may have truncated a frame mid-writev, so the
		// stream is desynced and cannot be reused. Tear the generation down (an involuntary drop) so
		// the reconnect loop re-establishes it, rather than returning over a possibly-partial frame.
		// TCPDown only injects evDisconnect (the actual teardown runs on the supervisor goroutine), so
		// it is safe under writeMu. Guarded on e.ctx so an already-tearing-down generation is not
		// redundantly re-dropped.
		if e.ctx.Err() == nil {
			c.TCPDown(err)
		}

		return err
	}

	// On-wire send counters — the single "committed to the wire" chokepoint, mirroring
	// incDataMsgRecv in DeliverOwnedFrame on the receive side. Counted exactly once per frame
	// that reached the transport: v2 never retries a send, and a B1/B2-dropped data message
	// returns above without reaching here.
	if isData {
		c.metrics.incDataMsgSend()
	}

	return nil
}

// sendWaitReply is the SYNCHRONOUS W-bit / control send path (spec §5.5, D5a-1): it frames and
// writes msg on the caller's goroutine — the write returning IS "on the wire" (§9.4.1.2) — then
// awaits the correlated reply. The wait ends at the earliest of four DISTINGUISHABLE outcomes:
// the reply, the protocol timer (T3 for data, T6 for control), the connection drop
// (epoch.ctx → ErrConnClosed), or the caller's ctx (→ ctx.Err()).
//
// Invariants (spec §5.5, weaken none):
//   - B1 gate: a data message is refused with ErrNotSelectedState BEFORE any register/write
//     while !IsSelected(), counted once at the B3 chokepoint. Control messages are not gated.
//   - B2 gate: writeFrame re-checks IsSelected under writeMu (the write boundary) to close
//     the TOCTOU gap between B1 and the actual transport Write. A data message that passes
//     B1 but finds NotSelected at the write boundary is counted and returned as
//     ErrNotSelectedState — non-fatal, never a teardown (spec §5.5).
//   - Reply correlation: the sender owns the reply channel's whole lifecycle — register before
//     the write, deregister via defer on every return path.
//   - I1: for a DATA W-bit send, incDataMsgInflight EXACTLY ONCE after the frame is on the wire,
//     decremented in ALL FOUR terminal branches (one defer covers them). Control (T6)
//     transactions never touch the data gauge.
//   - ctx-cancel contract (D5a-7): cancelling the wait does NOT un-send the primary or
//     Reject/Separate the peer — the defer-deregister drops the reply channel and a late reply
//     misses the registry (routes unsolicited / drops), exactly like a T3 timeout.
//
//nolint:cyclop // one send path with a fire-and-forget short-circuit plus four DISTINGUISHABLE wait outcomes (reply/Reject, timer, teardown, caller-ctx); splitting the branches would obscure the reply-correlation invariants documented above.
func (c *connection) sendWaitReply(callerCtx context.Context, msg Message) (Message, error) {
	e := c.cur.Load()
	if e == nil {
		return nil, ErrNotOpen
	}

	dm, isData := msg.(*DataMessage)

	// A DATA message with the W-bit CLEAR is fire-and-forget on this synchronous path: it expects
	// no reply, so it neither registers a reply channel nor arms a protocol timer, and returns the
	// instant the frame is on the wire (short-circuit below). Control messages inherently expect a
	// rsp (dm == nil here) and are never short-circuited.
	fireAndForget := dm != nil && !dm.WaitBit()

	// B1 gate (data only) — refuse before any register/write while not Selected (B3 chokepoint).
	if isData && !c.IsSelected() {
		c.dropNotSelected()
		return nil, ErrNotSelectedState
	}

	// Reply correlation: the sender owns the channel lifecycle (register + defer-deregister). A
	// fire-and-forget data send skips this entirely — there is no reply to correlate, so the
	// registry stays empty for that send.
	var ch chan replyResult
	if !fireAndForget {
		key := msg.SystemBytes()
		ch = e.replies.register(key)
		defer e.replies.deregister(key)
	}

	// Synchronous writev == on-wire (§9.4.1.2). writeFrame runs the B2 write-boundary
	// re-check under writeMu; ErrNotSelectedState from B2 is counted, non-fatal.
	if err := c.writeFrame(callerCtx, e, msg); err != nil {
		// The frame never reached the wire. A NotSelected drop (B2) has its own counter, and
		// teardown/caller-cancel are lifecycle/caller events — none count as a transaction
		// error; a genuine transport write failure does.
		if isData && isCountedSendErr(err) {
			c.metrics.incDataMsgErr()
		}

		return nil, err
	}

	// Fire-and-forget short-circuit: the frame is on the wire (incDataMsgSend already fired in
	// writeFrame), and no reply is expected. Return immediately — no inflight gauge (I1 is W-bit
	// only), no timer, no select-block. This is also why a !W send records DataMsgSend+1 with
	// DataMsgErr+0 (it never reaches a T3 timeout path).
	if fireAndForget {
		return nil, nil //nolint:nilnil // fire-and-forget contract: a !W data send has no reply and no error once on the wire.
	}

	// I1: DATA W-bit inflight — incremented exactly once AFTER on-wire, decremented in every
	// terminal branch below via this single defer. Control (T6) transactions do NOT touch it.
	if dm != nil && dm.WaitBit() {
		c.metrics.incDataMsgInflight()
		defer c.metrics.decDataMsgInflight()
	}

	// Protocol timer: T3 for a data transaction, T6 for a control transaction.
	timers := c.cfg.Load().timers
	timeout := timers.T6
	timeoutErr := ErrT6Timeout
	if isData {
		timeout = timers.T3
		timeoutErr = ErrT3Timeout
	}

	timer := pool.GetTimer(timeout)
	defer pool.PutTimer(timer)

	select {
	case res := <-ch:
		return res.msg, res.err
	case <-timer.C:
		// Protocol timeout: T3 (data) — a transaction failure.
		if isData {
			c.metrics.incDataMsgErr()

			if c.cfg.Load().autoS9F9 {
				c.sendAutoS9F9(msg)
			}
		}

		return nil, timeoutErr
	case <-e.ctx.Done():
		// Connection teardown/drop — a lifecycle event, NOT a data transaction error, so a
		// normal Close mid-transaction never inflates the cumulative error counter.
		return nil, ErrConnClosed
	case <-callerCtx.Done():
		return nil, callerCtx.Err()
	}
}

// isCountedSendErr reports whether a writeFrame error on a DATA send counts as a data-message
// error (DataMsgErrCount). A NotSelected drop (B2) has its own dedicated counter; connection
// teardown (ErrConnClosed) and caller cancellation (context.Canceled / DeadlineExceeded) are
// lifecycle/caller events, not send failures. Anything else (a genuine transport write error) does.
func isCountedSendErr(err error) bool {
	return !errors.Is(err, ErrNotSelectedState) &&
		!errors.Is(err, ErrConnClosed) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

// sendAutoS9F9 fire-and-forget sends an S9F9 (Transaction Timeout, SEMI E5 §10.13) notification to
// the peer after a data message's T3 reply-wait timed out, restoring v1's equipment-role behavior
// via WithAutoS9F9 (see hsmsss.WithEquipRole / secs1.WithEquipment). Per §10.13 the body is SHEAD —
// the 10-byte header of the timed-out message — obtained via msg.HeaderBytes(). Mirrors
// checkSessionID's S9F1 auto-reply construction exactly: a failure to send is not surfaced, since
// the caller already has the timeout error to report.
func (c *connection) sendAutoS9F9(msg Message) {
	s9 := gem.S9F9(msg.HeaderBytes())
	notice, err := NewDataMessage(
		s9.StreamCode(), s9.FunctionCode(), s9.WaitBit(),
		c.SessionID(), c.NextSystemBytes(), s9.Item(),
	)
	if err == nil {
		_ = c.SendAsync(context.Background(), notice)
	}
}

// sendNoReply is the SYNCHRONOUS write path that does NOT correlate a reply — the forward/relay
// path behind SECS2Endpoint.ForwardDataMessage. It frames and writes msg on the caller's goroutine
// (the write returning IS "on the wire", §9.4.1.2), enforcing the SAME B1 pre-write gate and B2
// write-boundary re-check as sendWaitReply, but it registers NO reply channel and arms NO protocol
// timer. Because nothing is registered in the per-generation reply registry, a secondary the peer
// later sends against msg's System Bytes misses RouteReply and is delivered to the session's
// DataMessageHandlers (RouteData) — the caller owns reply correlation. A W-bit-set msg is written
// with its W-bit intact (the peer is still asked for a reply); only the LOCAL reply-wait is skipped.
func (c *connection) sendNoReply(callerCtx context.Context, msg Message) error {
	e := c.cur.Load()
	if e == nil {
		return ErrNotOpen
	}

	_, isData := msg.(*DataMessage)

	// B1 gate (data only) — refuse before any write while not Selected (B3 chokepoint).
	if isData && !c.IsSelected() {
		c.dropNotSelected()
		return ErrNotSelectedState
	}

	// Synchronous writev == on-wire (§9.4.1.2). writeFrame runs the B2 write-boundary re-check
	// under writeMu; ErrNotSelectedState from B2 is counted, non-fatal. No reply channel is
	// registered, so the reply (if any) routes to the session handlers, not back to a waiting sender.
	if err := c.writeFrame(callerCtx, e, msg); err != nil {
		if isData && isCountedSendErr(err) {
			c.metrics.incDataMsgErr()
		}

		return err
	}

	return nil
}

// drainSendCh is the per-generation async sender goroutine (spec §5.5, D5a-1 / J3). Spawned in
// Open via epoch.spawn (which passes e.ctx as ctx), it drains e.sendCh and writes each queued
// frame so the receiver never blocks on a wedged peer. It exits when the generation ctx is
// cancelled by teardown; frames still queued at that point are stranded in the (soon-GC'd)
// epoch channel — never flushed as a later generation's first frame (C1). The channel is never
// closed (F5: send-on-closed is structurally unreachable).
func (c *connection) drainSendCh(ctx context.Context, e *epoch) {
	for {
		select {
		case req := <-e.sendCh:
			// A write error on the async path is non-fatal (best-effort fire-and-forget).
			_ = c.writeFrame(ctx, e, req.msg)
		case <-ctx.Done():
			return
		}
	}
}

// WriteMessage performs a synchronous framed write and awaits the protocol-bounded reply
// (TransportRuntime, spec §5.5/§6.2). It delegates to sendWaitReply, which enforces the B1
// gate, the synchronous writev under epoch.writeMu, I1 inflight accounting, and the
// four-outcome reply correlation (reply / T3|T6 / conn-drop / caller-ctx).
func (c *connection) WriteMessage(ctx context.Context, msg Message) (Message, error) {
	return c.sendWaitReply(ctx, msg)
}

// WriteMessageNoReply performs a synchronous framed write WITHOUT reply correlation
// (TransportRuntime, spec §5.5). It delegates to sendNoReply, which enforces the B1 gate and the
// synchronous writev under epoch.writeMu but registers no reply channel and arms no protocol timer,
// so any reply routes to the session's DataMessageHandlers rather than back to the caller. Backs
// SECS2Endpoint.ForwardDataMessage.
func (c *connection) WriteMessageNoReply(ctx context.Context, msg Message) error {
	return c.sendNoReply(ctx, msg)
}

// SendAsync enqueues a fire-and-forget message on the per-generation async send channel
// (TransportRuntime, spec §5.5, J3). It applies the B1 IsSelected gate to data messages
// BEFORE enqueuing (data refused while not Selected yields a counted, non-fatal
// ErrNotSelectedState at the B3 chokepoint — never enqueued), then performs a bounded enqueue
// onto epoch.sendCh: the send is released only by the queue, the generation ctx (ErrConnClosed),
// or the caller's ctx (ctx.Err()), so it never blocks forever on a wedged peer.
func (c *connection) SendAsync(ctx context.Context, msg Message) error {
	e := c.cur.Load()
	if e == nil {
		return ErrNotOpen
	}

	// B1 gate (data only) — refuse before enqueue while not Selected (B3 chokepoint).
	// Uses the same type-assert idiom as sendWaitReply for consistency.
	if _, isData := msg.(*DataMessage); isData && !c.IsSelected() {
		c.dropNotSelected()
		return ErrNotSelectedState
	}

	select {
	case e.sendCh <- &sendRequest{msg: msg}:
		return nil
	case <-e.ctx.Done():
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}
