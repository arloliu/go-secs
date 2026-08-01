package hsmsss

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// makeFrame is the default frame allocator: a fresh GC-owned buffer of n bytes (NOT pooled,
// §5.F option (b)). It is the production value of transport.allocFrame; tests override the
// field to assert that an attacker-controlled length is rejected BEFORE allocation (J2).
func makeFrame(n int) []byte { return make([]byte, n) }

// recvLoop is the per-generation receive goroutine. It reads E37 frames via readFrame and
// dispatches them (spec §6.1): data frames go zero-copy to rt.DeliverOwnedFrame, control
// responses to rt.RouteReply, control requests to the responder procedures, and a peer
// Separate drives teardown. On any read error (including the conn.Close in Stop) it calls
// rt.TCPDown and returns. g.recv.Done fires via defer on return, unblocking Stop's
// g.recv.Wait on this generation's captured bundle (Codex round-7 / NEW-1). g is threaded
// into every WaitGroup-registering call this loop makes (armT7, startLinktest) so a straggler
// abandoned by a bounded Stop registers only on ITS generation's bundle, never a successor's.
func (t *transport) recvLoop(g *genWG) {
	defer g.recv.Done()

	// Cache the conn once (T18-review Minor): the socket is immutable for the life of this
	// goroutine — Start publishes it before spawning recvLoop, and Stop's nil-out is paired
	// with a conn.Close that surfaces here as a Read error the loop handles below. Reading it
	// once avoids a connMu acquisition per frame.
	t.connMu.Lock()
	conn := t.conn
	genCtx := t.genCtx // capture THIS generation's ctx once — never re-read t.genCtx (a later Start overwrites it)
	t.connMu.Unlock()

	if conn == nil {
		// Start always publishes conn before spawning recvLoop; a nil here means Stop already
		// ran (a torn-down generation), so genCtx is cancelled — teardown owns the disconnect and
		// a stale TCPDown must not be injected (C1 straggler guard). Exit.
		if genCtx == nil || genCtx.Err() == nil {
			t.rt.TCPDown(errors.New("hsmsss: recvLoop: not connected"))
		}

		return
	}

	// Entering NotSelected (NotConnected->NotSelected): TCPUp committed NotSelected synchronously
	// (T22b) before this recv loop was spawned, so the T7 dwell applies now. Arm it for a LIVE
	// generation only (after the conn nil-check above) — §9.2.2.
	t.armT7(g)

	for {
		frame, err := t.readFrame(conn)
		if err != nil {
			t.metrics.incReadErrCount()

			// C1 straggler guard: drive TCPDown only for a LIVE generation. The epoch ctx (genCtx)
			// is rooted at context.Background() and cancelled ONLY by this generation's teardown, so
			// a non-nil Err means teardown already began — either a voluntary Close (which owns the
			// disconnect) or a straggler that outlived a bounded Stop. Injecting TCPDown then could
			// hit a LATER generation's supervisor. An involuntary peer drop (genCtx not cancelled)
			// still drives the disconnect that initiates teardown.
			if genCtx.Err() == nil {
				t.rt.TCPDown(err)
			}

			return
		}

		t.lastRecvStamp.Store(t.monoNanos()) // any complete inbound frame is proof of link liveness

		if !t.dispatchFrame(g, frame) {
			// dispatchFrame already drove teardown (a peer Separate while Selected called
			// rt.TCPDown). Do not call TCPDown again; just end the loop so Stop can join it.
			return
		}
	}
}

// dispatchFrame routes one owned E37 frame (spec §6.1). frame is the [header || body] the
// core adopts zero-copy for data; readFrame guarantees len(frame) >= 10. It returns true to
// keep reading and false when it has itself driven teardown (peer Separate while Selected).
// g is the recv goroutine's captured generation bundle (NEW-1), threaded onward to any
// responder path that registers a linktest / T7 goroutine so it lands on this generation's bundle.
func (t *transport) dispatchFrame(g *genWG, frame []byte) bool {
	pType := frame[4]
	sType := frame[5]

	// J3 (§7.10.3): an unsupported PType or an undefined SType is answered with a Reject.req
	// while the link stays UP — never a teardown.
	if pType != 0 || !hsms.IsValidSType(sType) {
		t.sendReject(frame, pType, sType)
		return true
	}

	msgType := hsms.MsgType(sType)

	// Control frames are header-only (E37 §9.3.3.1): a standard control SType MUST have
	// PType 0 and Message Length EXACTLY 10. Only a data message (SType 0) carries a body
	// (len >= 10, empty body permitted). A control frame with a body is malformed → Reject.
	if msgType != hsms.DataMsgType && len(frame) != 10 {
		t.sendReject(frame, pType, sType)
		return true
	}

	if msgType != hsms.DataMsgType && t.cfg.TraceTraffic() {
		t.cfg.Logger().Debug("hsmsss: trace: received control frame",
			"stype", sType, "raw", hexDumpFrame(frame))
	}

	switch msgType { //nolint:exhaustive // UndefinedMsgType is filtered by IsValidSType above; default is unreachable.
	case hsms.DataMsgType:
		// A data message received while NOT Selected is refused with Reject(reason 4) and the
		// link is KEPT (E37 §7.10.3 / spec §6.3). The H2 synchronous commit (Select responder /
		// initiator, below and in handleSelectReq) is what keeps a peer that pipelines data right
		// after Select.rsp from being spuriously Rejected here (the efb220b regression).
		if t.rt.State() != hsms.SelectedState {
			t.sendRejectNotSelected(frame)

			break
		}

		// Zero-copy hand-off: the core decodes via decodeOwnedFrame and routes to the session.
		// A decode/route error is a protocol failure, not a transport failure — keep reading.
		_ = t.rt.DeliverOwnedFrame(frame)

	case hsms.SelectRspType, hsms.DeselectRspType, hsms.LinktestRspType, hsms.RejectReqType:
		// Terminal responses to one of our transactions (a Reject.req rejects a message we sent).
		// Route by System Bytes to the waiting sender.
		msg, err := decodeControlFrame(frame)
		if err != nil {
			break
		}

		// Count an inbound Reject.req (SType 7) BEFORE routing: the peer rejected one of our sends.
		// Gated on the actual type because this response-routing case also handles Select/Deselect/
		// Linktest.rsp — only the Reject variant is a peer-emitted Reject of our traffic. An orphan
		// Reject (a RouteReply miss) is still counted here since it was genuinely received, even
		// though it is then silently dropped rather than re-rejected (§8.3.20).
		if msg.Type() == hsms.RejectReqType {
			t.metrics.incRejectRecv()
		}

		if t.rt.RouteReply(msg) {
			// HIT. H2 initiator commit (§7.D): a routed Select.rsp with select-status 0 completes
			// our active Select. Commit to Selected SYNCHRONOUSLY on THIS recv goroutine — after
			// routing the reply, before reading the next frame — so a data frame the peer pipelined
			// right after Select.rsp finds IsSelected()==true and is not spuriously Rejected
			// (efb220b). Committing here (a single sequential reader) is what makes the initiator
			// side race-free; a commit on the separate Select goroutine could not.
			if msg.Type() == hsms.SelectRspType && selectStatus(msg) == hsms.SelectStatusSuccess {
				// A genuine NotSelected->Selected commit (CAS success) cancels the T7 dwell
				// (§9.2.2 — reaching Selected ends the NOT-SELECTED window) and starts the
				// auto-linktest (D5a-5); a duplicate (already Selected) returns false and does neither.
				if t.rt.CommitSelected() {
					t.cancelT7()
					t.startLinktest(g)
				}
			}

			break
		}

		// MISS. E37 §8.3.20: an orphan control RESPONSE (even SType — Select/Deselect/Linktest.rsp
		// — with no open transaction) MUST be answered with Reject(TransactionNotOpen, reason 3),
		// keeping the link. An inbound Reject.req (SType 7, odd — not a response) is not re-rejected;
		// an orphan Reject is simply dropped.
		if msg.Type() != hsms.RejectReqType {
			t.sendRejectTransactionNotOpen(frame)
		}

	case hsms.SelectReqType, hsms.DeselectReqType, hsms.LinktestReqType:
		// Inbound control requests → the responder procedures (Select H2 §7.D, Linktest, Deselect D5a-4).
		if msg, err := decodeControlFrame(frame); err == nil {
			t.handleControlReq(g, msg)
		}

	case hsms.SeparateReqType:
		// Peer leaving (E37 §7.9.2): tear down if Selected, otherwise ignore.
		return t.handleSeparateReq()

	default:
		// Unreachable: IsValidSType already rejected every undefined SType above.
	}

	return true
}

// readFrame reads one HSMS E37 frame (spec §6.1) from conn: the 4-byte big-endian length
// prefix, then the [10-byte header || body] into a fresh GC-owned buffer.
//
// Timing (§9.2.3.1 / J1): the FIRST byte of a frame is an idle wait (no deadline — a link may
// sit idle indefinitely between messages); once any byte has been read, T8 governs EVERY
// inter-byte gap, INCLUDING the remaining bytes of the length prefix, by resetting the read
// deadline before each Read. A single whole-frame deadline is deliberately NOT used.
//
// Length validation (J2) happens BEFORE allocation: the length must be 10 <= msgLen <=
// secs2.MaxByteSize. The lower bound is the mandatory 10-byte header — a length < 10 is a
// protocol error, NOT a zero-body data message. The upper bound is a WHOLE-FRAME DoS cap (it is
// compared against the full msgLen, header included), reusing secs2.MaxByteSize as a generous
// ceiling; it is NOT a per-item bound. An oversized length errors out here without allocating the
// body buffer (the attacker never gets to size make()). Consequence (M6): a legitimate but
// enormous deeply-nested message (> ~16 MiB on the wire) is rejected at the framing layer — the
// read errors and the link is dropped, NOT answered with a Reject (HSMS has no "too long" Reject
// reason). Real single-item bodies stay far below the cap.
func (t *transport) readFrame(conn net.Conn) ([]byte, error) {
	t8 := t.rt.Timers().T8 // LIVE T8 (via rt), so UpdateConfigOptions(WithT8) reaches the recv loop

	// started gates the idle-vs-T8 policy for THIS frame. It is threaded through both reads
	// (length prefix then header+body) so T8 applies across the length header (J1), while the
	// very first byte of the frame remains an idle wait.
	var started bool

	var lenBuf [4]byte
	if err := readN(conn, lenBuf[:], t8, &started, t.clock()); err != nil {
		return nil, err
	}

	msgLen := binary.BigEndian.Uint32(lenBuf[:])

	if msgLen < 10 {
		return nil, fmt.Errorf("hsmsss: frame length %d below minimum 10 (protocol error)", msgLen)
	}

	if uint64(msgLen) > uint64(secs2.MaxByteSize) {
		return nil, fmt.Errorf("hsmsss: frame length %d exceeds maximum %d", msgLen, secs2.MaxByteSize)
	}

	frame := t.allocFrame(int(msgLen))
	if err := readN(conn, frame, t8, &started, t.clock()); err != nil {
		return nil, err
	}

	return frame, nil
}

// readN reads exactly len(buf) bytes into buf, applying the idle-vs-T8 deadline policy
// (§9.2.3.1 / J1). While *started is false (no byte of the frame read yet) the read has NO
// deadline — an idle link must not time out, and any stale deadline from the previous frame
// is cleared. Once any byte has been read, *started is set and the deadline is reset to
// now+t8 before EACH subsequent Read, so a slow-but-never-stalled stream passes while a gap
// exceeding T8 trips. It follows io.ReadFull semantics: a Read that completes buf is a
// success even if it also reports io.EOF.
//
// now is the injectable clock for the T8 read deadline (default time.Now via transport.clock);
// it is a package-level function (no t receiver), so the caller passes the clock explicitly.
func readN(conn net.Conn, buf []byte, t8 time.Duration, started *bool, now func() time.Time) error {
	read := 0
	for read < len(buf) {
		if *started {
			if err := conn.SetReadDeadline(now().Add(t8)); err != nil {
				return err
			}
		} else if err := conn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}

		n, err := conn.Read(buf[read:])
		if n > 0 {
			read += n
			*started = true
		}

		if read == len(buf) {
			return nil
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// decodeControlFrame decodes an owned, header-only control frame into an hsms.Message via the
// exported hsms.DecodeHSMSMessage, which expects a 4-byte length prefix. frame is copied into
// a small (14-byte) prefixed buffer; control frames are tiny and rare, so this copy is
// negligible — data frames NEVER take this path (they go zero-copy through DeliverOwnedFrame).
func decodeControlFrame(frame []byte) (hsms.Message, error) {
	full := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(full[0:4], uint32(len(frame)))
	copy(full[4:], frame)

	return hsms.DecodeHSMSMessage(full)
}

// hexDumpFrame renders frame as a lowercase hex string for WithTraceTraffic's control-frame dumps.
func hexDumpFrame(frame []byte) string {
	return hex.EncodeToString(frame)
}
