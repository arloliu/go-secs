package integration

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
)

// errHSMSPeerClosed is returned by a peer-originated send when the peer is torn down before the
// command is accepted or its reply arrives. errHSMSPeerTimeout is returned when the bounded wait
// for a peer-originated reply expires.
var (
	errHSMSPeerClosed  = errors.New("hsms peer: closed")
	errHSMSPeerTimeout = errors.New("hsms peer: reply timeout")
)

// hsmsPeerReplyTimeout bounds a peer-originated sendPrimary so a withheld or lost reply surfaces as
// an error instead of wedging the calling test. It sits far above the deterministic round-trip cost
// over the in-memory pipe, so it only ever fires on a genuine fault.
const hsmsPeerReplyTimeout = 3 * time.Second

// hsmsPeer is a full-duplex scripted HSMS peer living on the far (endB) end of a net.Pipe.
//
// net.Pipe is unbuffered and synchronous: a Write blocks until the other side Reads it. The
// peer therefore MUST read and write concurrently with the connection under test. It runs
// two goroutines:
//
//   - readLoop continuously drains 4-byte-length-prefixed E37 frames off the pipe and pushes
//     the decoded messages onto the decoded channel; and
//   - run consumes that channel and writes the scripted replies (Select.rsp, Linktest.rsp,
//     and a secondary echo for each W-bit data primary), and — routed through the same single
//     coordinator — originates the peer's own W-bit primaries on demand.
//
// Splitting read from write is what avoids a deadlock. Because readLoop always drains endB,
// the connection's writes never block on a peer with no reader; and the peer's own writes are
// drained by the connection's independent recv loop. A single read-then-write goroutine would
// deadlock the moment both sides tried to write at once (concurrent bidirectional traffic).
//
// The run coordinator is the SOLE writer, so peer-originated sends never introduce a second
// concurrent writer: sendPrimary posts a command onto cmds, the coordinator writes the primary
// and records its System Bytes in pending, and the correlated secondary (which reuses those
// System Bytes) is matched back to the waiting caller when it later arrives via decoded. Because
// both the write and the correlation run on the one coordinator goroutine, distinct System Bytes
// per in-flight send are all that is needed to keep concurrent bidirectional replies from crossing.
type hsmsPeer struct {
	endB      net.Conn
	decoded   chan hsms.Message
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	// dataIn buffers the raw bodies of every inbound data message for inbound(); pushes are
	// non-blocking (drop-if-full) so recording never wedges the coordinator.
	dataIn chan []byte
	// cmds carries peer-originated send requests to the coordinator (the sole writer).
	cmds chan hsmsSendCmd
	// deselectCmds carries peer-originated Deselect.req commands to the coordinator (the sole writer),
	// so a peer-originated control frame never introduces a second concurrent writer.
	deselectCmds chan hsmsDeselectCmd
	// withhold, when true, suppresses the default auto-echo of W-bit primaries (reply-timeout
	// scenario). Read on the coordinator, set from the test goroutine — an atomic keeps it race-free.
	withhold atomic.Bool
	// withholdSelect, when true, suppresses the auto Select.rsp so the connection dwells in NotSelected
	// (the NOT-SELECTED-timeout / T7 dwell scenario). Read on the coordinator, set from the test
	// goroutine or the spawn hook — an atomic keeps it race-free.
	withholdSelect atomic.Bool

	// pending and sysCounter are touched ONLY by the coordinator goroutine, so they need no lock.
	pending    map[[4]byte]chan hsmsSendResult // peer-originated primaries awaiting their reply, keyed by System Bytes
	sysCounter uint32                          // monotonic source of the peer's own outbound System Bytes
}

// hsmsSendCmd is one peer-originated primary handed to the coordinator. replyCh is buffered so the
// coordinator delivers the correlated reply (or an error) without ever blocking.
type hsmsSendCmd struct {
	stream, function byte
	body             []byte
	replyCh          chan hsmsSendResult
}

// hsmsSendResult carries the outcome of a peer-originated send back to sendPrimary.
type hsmsSendResult struct {
	body []byte
	err  error
}

// hsmsDeselectCmd is one peer-originated Deselect.req (SEMI E37 §7.7) handed to the coordinator. done
// is closed once the coordinator has written the frame, so the caller can sequence a follow-on state
// assertion without ever becoming a second writer.
type hsmsDeselectCmd struct {
	sessionID uint16
	done      chan struct{}
}

// Ensure *hsmsPeer satisfies the extended scripting surface.
var _ scriptablePeer = (*hsmsPeer)(nil)

// newHSMSPeer spawns the reader and coordinator goroutines on conn and returns the peer.
func newHSMSPeer(conn net.Conn) *hsmsPeer {
	p := &hsmsPeer{
		endB:         conn,
		decoded:      make(chan hsms.Message, 8),
		stop:         make(chan struct{}),
		dataIn:       make(chan []byte, 16),
		cmds:         make(chan hsmsSendCmd),
		deselectCmds: make(chan hsmsDeselectCmd),
		pending:      make(map[[4]byte]chan hsmsSendResult),
	}

	p.wg.Add(2)

	go p.readLoop()
	go p.run()

	return p
}

// readLoop drains inbound frames off endB until a read error (pipe closed / deadline) or a
// stop signal. It is the always-draining side that keeps the unbuffered pipe from deadlocking.
func (p *hsmsPeer) readLoop() {
	defer p.wg.Done()
	defer close(p.decoded)

	for {
		frame, err := readHSMSFrame(p.endB)
		if err != nil {
			return
		}

		msg, err := hsms.DecodeHSMSMessage(frame)
		if err != nil {
			continue // skip an undecodable frame; keep draining so the pipe never blocks
		}

		select {
		case p.decoded <- msg:
		case <-p.stop:
			return
		}
	}
}

// run is the serialized coordinator: it consumes decoded inbound messages, writes the scripted
// replies, and originates the peer's own primaries. All peer writes originate here, so nothing
// the peer sends ever interleaves with anything else it sends.
func (p *hsmsPeer) run() {
	defer p.wg.Done()

	for {
		select {
		case <-p.stop:
			return
		case cmd := <-p.cmds:
			p.originate(cmd)
		case cmd := <-p.deselectCmds:
			p.originateDeselect(cmd)
		case msg, ok := <-p.decoded:
			if !ok {
				return
			}

			p.handle(msg)
		}
	}
}

// handle dispatches one decoded inbound message to the control or data responder.
func (p *hsmsPeer) handle(msg hsms.Message) {
	switch m := msg.(type) {
	case *hsms.ControlMessage:
		p.handleControl(m)
	case *hsms.DataMessage:
		p.handleData(m)
	default:
		// DecodeHSMSMessage yields only control or data messages; nothing else to script.
	}
}

// handleControl answers a Select.req with Select.rsp(success) and a Linktest.req with
// Linktest.rsp. Other control frames (Separate, etc.) need no scripted reply here.
func (p *hsmsPeer) handleControl(m *hsms.ControlMessage) {
	switch m.Type() { //nolint:exhaustive // the peer scripts only Select.req and Linktest.req replies.
	case hsms.SelectReqType:
		if p.withholdSelect.Load() {
			return // suppress the Select.rsp so the connection dwells in NotSelected (T7 dwell scenario)
		}
		if rsp, err := hsms.NewSelectRsp(m, hsms.SelectStatusSuccess); err == nil {
			p.write(rsp.ToBytes())
		}
	case hsms.LinktestReqType:
		if rsp, err := hsms.NewLinktestRsp(m); err == nil {
			p.write(rsp.ToBytes())
		}
	default:
		// other control frames (Separate, etc.) need no scripted reply in this scenario
	}
}

// handleData records every inbound data body for inbound(), correlates a secondary that answers
// one of the peer's own originated primaries, and otherwise echoes each W-bit data primary. The
// echo replies with a secondary that reuses the primary's System Bytes (so the core correlates it)
// and the same SECS-II item. A non-W-bit primary (fire-and-forget) needs no reply.
func (p *hsmsPeer) handleData(m *hsms.DataMessage) {
	p.recordInbound(m.AppendBodyTo(nil))

	if !m.WaitBit() {
		// A secondary (even function, W-bit clear) that reuses one of the peer's own outbound
		// System Bytes is the reply to a peer-originated primary — hand its body to the waiter.
		// Any other non-W-bit message (a fire-and-forget primary) is only recorded above.
		if m.Function()%2 == 0 {
			sb := m.SystemBytes()
			if ch, ok := p.pending[sb]; ok {
				delete(p.pending, sb)
				ch <- hsmsSendResult{body: m.AppendBodyTo(nil)}
			}
		}

		return
	}

	// W-bit primary: auto-echo unless the test has toggled replies off.
	if p.withhold.Load() {
		return
	}

	item, err := m.Item()
	if err != nil {
		return
	}

	// function+1 makes the reply an even (secondary) function with the W-bit clear, reusing
	// the primary's System Bytes — exactly what the core's reply registry correlates on.
	reply, err := hsms.NewDataMessage(m.Stream(), m.Function()+1, false, m.SessionID(), m.SystemBytes(), item)
	if err != nil {
		return
	}

	p.write(reply.ToBytes())
}

// recordInbound pushes a received data body onto dataIn for inbound(). The push is non-blocking
// (drop-if-full) so recording never wedges the coordinator when a test is not draining.
func (p *hsmsPeer) recordInbound(body []byte) {
	select {
	case p.dataIn <- body:
	default:
	}
}

// originate builds and writes a peer-originated W-bit primary, registering its System Bytes so the
// correlated secondary is later routed back to the waiting sendPrimary. It runs on the coordinator
// (the sole writer). A decode/build/write failure is reported immediately; otherwise the reply is
// delivered by handleData when the secondary arrives.
func (p *hsmsPeer) originate(cmd hsmsSendCmd) {
	item, err := secs2.Decode(cmd.body)
	if err != nil {
		cmd.replyCh <- hsmsSendResult{err: err}
		return
	}

	sb := p.nextSystemBytes()

	// hsmsPeerSessionID mirrors the connection's default session ID (0xFFFF); the connection routes
	// an inbound primary to its handlers regardless of session ID, and the auto-echo path mirrors
	// this same value on the connection's own primaries.
	const hsmsPeerSessionID uint16 = 0xFFFF

	msg, err := hsms.NewDataMessage(cmd.stream, cmd.function, true, hsmsPeerSessionID, sb, item)
	if err != nil {
		cmd.replyCh <- hsmsSendResult{err: err}
		return
	}

	p.pending[sb] = cmd.replyCh

	if _, werr := p.endB.Write(msg.ToBytes()); werr != nil {
		delete(p.pending, sb)
		cmd.replyCh <- hsmsSendResult{err: werr}
	}
}

// originateDeselect builds and writes a peer-originated Deselect.req (SEMI E37 §7.7) on the
// coordinator (the sole writer), minting the System Bytes there so sysCounter stays single-goroutine.
// Answered by the connection's Deselect responder while Selected, it drives Selected -> NotSelected
// (the Select-lost transition). It signals completion by closing done; the correlated Deselect.rsp the
// connection sends back is drained by readLoop and ignored (no scripted reply needed).
func (p *hsmsPeer) originateDeselect(cmd hsmsDeselectCmd) {
	req := hsms.NewDeselectReq(cmd.sessionID, p.nextSystemBytes())
	p.write(req.ToBytes())
	close(cmd.done)
}

// nextSystemBytes mints the peer's next outbound System Bytes. The high bit is set to keep the
// peer's own transaction identifiers in a distinct range from the connection's generator, so
// correlation of a peer-originated reply is unambiguous. Called only on the coordinator goroutine.
func (p *hsmsPeer) nextSystemBytes() [4]byte {
	p.sysCounter++

	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], 0x80000000|p.sysCounter)

	return sb
}

// sendPrimary makes the peer originate a W-bit primary and returns the correlated reply body. It
// posts the request to the coordinator and waits on a buffered response channel, so it never adds a
// second writer. A bounded timeout keeps a withheld or lost reply from wedging the caller.
func (p *hsmsPeer) sendPrimary(stream, function byte, body []byte) ([]byte, error) {
	cmd := hsmsSendCmd{
		stream:   stream,
		function: function,
		body:     append([]byte(nil), body...),
		replyCh:  make(chan hsmsSendResult, 1),
	}

	select {
	case p.cmds <- cmd:
	case <-p.stop:
		return nil, errHSMSPeerClosed
	}

	timer := time.NewTimer(hsmsPeerReplyTimeout)
	defer timer.Stop()

	select {
	case res := <-cmd.replyCh:
		return res.body, res.err
	case <-timer.C:
		return nil, errHSMSPeerTimeout
	case <-p.stop:
		return nil, errHSMSPeerClosed
	}
}

// inbound exposes the buffered stream of inbound data bodies for fire-and-forget assertions.
func (p *hsmsPeer) inbound() <-chan []byte { return p.dataIn }

// setWithholdReplies toggles the auto-echo of W-bit primaries off/on for the reply-timeout scenario.
func (p *hsmsPeer) setWithholdReplies(withhold bool) { p.withhold.Store(withhold) }

// setWithholdSelect toggles the auto Select.rsp off/on. With it on, the peer never answers the
// connection's Select.req, so the connection dwells in NotSelected until its T7 (NOT-SELECTED) timer
// expires — the driver for the T7 dwell-then-disconnect scenario. Set it via the spawn hook so every
// generation the reconnect loop dials starts with Select withheld.
func (p *hsmsPeer) setWithholdSelect(withhold bool) { p.withholdSelect.Store(withhold) }

// sendDeselect makes the peer originate a Deselect.req (SEMI E37 §7.7) through the coordinator (the
// sole writer) and returns once that frame has been written. While the connection is Selected its
// Deselect responder answers Deselect.rsp(success) and transitions Selected -> NotSelected, so the
// caller can then await the Select-lost edge. It posts the request to the coordinator and waits on a
// completion signal, so it never adds a second writer.
func (p *hsmsPeer) sendDeselect(sessionID uint16) error {
	cmd := hsmsDeselectCmd{sessionID: sessionID, done: make(chan struct{})}

	select {
	case p.deselectCmds <- cmd:
	case <-p.stop:
		return errHSMSPeerClosed
	}

	select {
	case <-cmd.done:
		return nil
	case <-p.stop:
		return errHSMSPeerClosed
	}
}

// dropLine simulates an involuntary disconnect by tearing the peer down: it stops the goroutines and
// closes endB, so the connection's recv loop sees EOF and reconnects to a fresh generation. It routes
// through the same goroutine-safe teardown as close, so a later closeAll double-close is a safe no-op.
func (p *hsmsPeer) dropLine() { p.close() }

// write pushes a whole framed message onto endB. Write errors are ignored: the peer is
// best-effort and a torn-down connection surfaces as the test's own assertion failure.
func (p *hsmsPeer) write(frame []byte) {
	_, _ = p.endB.Write(frame)
}

// close stops the reader and coordinator, then closes the pipe. It sets a past deadline to
// unblock any goroutine parked in a pipe Read or Write, waits for BOTH to exit (so no
// goroutine touches the conn after this point), and only then closes endB — there is no
// read/write-on-closed race.
func (p *hsmsPeer) close() {
	p.closeOnce.Do(func() {
		close(p.stop)
		_ = p.endB.SetDeadline(time.Unix(0, 0)) // in the past: unblocks any parked Read/Write
		p.wg.Wait()
		_ = p.endB.Close()
	})
}

// readHSMSFrame reads one whole HSMS frame (a 4-byte big-endian length prefix followed by
// the payload) from conn and returns the complete frame, prefix included, so it can be
// handed straight to hsms.DecodeHSMSMessage.
func readHSMSFrame(conn net.Conn) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}

	msgLen := binary.BigEndian.Uint32(lenBuf[:])
	frame := make([]byte, 4+int(msgLen))
	copy(frame, lenBuf[:])

	if _, err := io.ReadFull(conn, frame[4:]); err != nil {
		return nil, err
	}

	return frame, nil
}
