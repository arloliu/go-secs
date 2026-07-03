package integration

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// errSECS1PeerClosed is returned by a peer-originated send when the peer is torn down; errSECS1Send
// on a failed master-side block hand-off; errSECS1ReplyTimeout when the bounded wait for the
// connection's correlated reply expires.
var (
	errSECS1PeerClosed   = errors.New("secs1 peer: closed")
	errSECS1Send         = errors.New("secs1 peer: send failed")
	errSECS1ReplyTimeout = errors.New("secs1 peer: reply timeout")
)

// SECS-I line-control characters (SEMI E4 §7.8) — the single-byte handshake characters that
// coordinate the half-duplex block transfer direction on the wire.
const (
	secs1ENQ byte = 0x05 // request to send (initiator asks for line control)
	secs1EOT byte = 0x04 // ready to receive (granted in response to ENQ)
	secs1ACK byte = 0x06 // block correctly received
	secs1NAK byte = 0x15 // block incorrectly received (bad length/checksum)
)

// SECS-I block-size bounds (SEMI E4 §7.6). A wire block is [length(1)][header(10)][body(0..244)]
// [checksum(2)]; the length byte counts the header+body only, so it ranges 10..254.
const (
	secs1HeaderSize   = 10
	secs1MaxBodySize  = 244
	secs1MinBlockLen  = secs1HeaderSize
	secs1MaxBlockLen  = secs1HeaderSize + secs1MaxBodySize
	secs1ChecksumSize = 2
)

// Peer read timeouts. The past-deadline set in close() is the primary unblock for a parked read or
// write; these bounded per-read deadlines are a belt-and-suspenders guard so a torn-down pipe can
// never wedge the single peer goroutine. Both sit far below the core reply timeout (T3), so a peer
// that bails surfaces as the test's own assertion failure, never a hang.
const (
	secs1IdlePoll     = 25 * time.Millisecond // idle poll tick while waiting for the connection's ENQ
	secs1StepTimeout  = 3 * time.Second       // bound on a within-transaction read (length/EOT/ACK/data)
	secs1ReplyTimeout = 3 * time.Second       // bound on a peer-originated send awaiting the connection's reply
)

// secs1Header holds the block-invariant SECS-I header fields (SEMI E4 §8) the peer needs to build a
// reply block. Block number and the E-bit are per-block and assigned by the splitter.
type secs1Header struct {
	deviceID    uint16
	rBit        bool
	stream      uint8
	function    uint8
	waitBit     bool
	systemBytes [4]byte
}

// secs1Block is one received SECS-I block: its 10-byte header and an owned copy of the body bytes.
type secs1Block struct {
	header [secs1HeaderSize]byte
	body   []byte
}

func (b secs1Block) rBit() bool           { return b.header[0]&0x80 != 0 }
func (b secs1Block) deviceID() uint16     { return binary.BigEndian.Uint16(b.header[0:2]) & 0x7FFF }
func (b secs1Block) stream() uint8        { return b.header[2] & 0x7F }
func (b secs1Block) waitBit() bool        { return b.header[2]&0x80 != 0 }
func (b secs1Block) function() uint8      { return b.header[3] }
func (b secs1Block) eBit() bool           { return b.header[4]&0x80 != 0 }
func (b secs1Block) systemBytes() [4]byte { return [4]byte(b.header[6:10]) }

// secs1Peer is a scripted SECS-I (SEMI E4) half-duplex peer living on the far (endB) end of a
// net.Pipe. It plays the equipment side of the line: it receives the connection-under-test's W-bit
// primary and echoes the reassembled body back as the secondary reply.
//
// SECS-I is half-duplex — exactly one end owns the line at a time — so the peer runs a SINGLE
// goroutine that never reads and writes concurrently. The auto-echo round-trip strictly alternates
// (the connection sends its primary first, then the peer sends the reply). A peer-originated primary
// (sendPrimary) is likewise routed through that one goroutine via cmds: the goroutine takes the line
// as master (ENQ/EOT/block/ACK), then turns around to receive the connection's correlated reply
// (grant EOT to its ENQ, reassemble, match System Bytes). Because every byte still flows through the
// single goroutine, there is no second writer; the scenarios drive the peer and connection so their
// line turns never overlap, so no ENQ contention arises.
type secs1Peer struct {
	endB      net.Conn
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	// dataIn buffers the raw bodies of every reassembled inbound data message for inbound(); pushes
	// are non-blocking (drop-if-full) so recording never wedges the line goroutine.
	dataIn chan []byte
	// cmds carries peer-originated send requests to the line goroutine (the sole owner of the wire).
	cmds chan secs1SendCmd
	// withhold, when true, suppresses the default auto-echo of W-bit primaries (reply-timeout
	// scenario). Read on the line goroutine, set from the test goroutine — an atomic keeps it race-free.
	withhold atomic.Bool

	// sysCounter is touched ONLY by the line goroutine, so it needs no lock.
	sysCounter uint32 // monotonic source of the peer's own outbound System Bytes
}

// secs1SendCmd is one peer-originated primary handed to the line goroutine. replyCh is buffered so
// the goroutine delivers the correlated reply (or an error) without ever blocking.
type secs1SendCmd struct {
	stream, function byte
	body             []byte
	replyCh          chan secs1SendResult
}

// secs1SendResult carries the outcome of a peer-originated send back to sendPrimary.
type secs1SendResult struct {
	body []byte
	err  error
}

// Ensure *secs1Peer satisfies the extended scripting surface.
var _ scriptablePeer = (*secs1Peer)(nil)

// newSECS1Peer spawns the single line goroutine on conn and returns the peer.
func newSECS1Peer(conn net.Conn) *secs1Peer {
	p := &secs1Peer{
		endB:   conn,
		stop:   make(chan struct{}),
		dataIn: make(chan []byte, 16),
		cmds:   make(chan secs1SendCmd),
	}

	p.wg.Add(1)
	go p.run()

	return p
}

// run is the single half-duplex line goroutine: it owns every byte read from and written to endB.
// It polls the idle line for the connection's ENQ, receives one block per granted turn, reassembles
// the ordered blocks of a message, and — once a W-bit message completes — echoes the secondary reply.
func (p *secs1Peer) run() {
	defer p.wg.Done()

	var body []byte // reassembly buffer for the in-progress inbound message

	for {
		select {
		case <-p.stop:
			return
		case cmd := <-p.cmds:
			p.originate(cmd) // peer takes the line as master, then receives the correlated reply
			continue
		default:
		}

		// Idle poll for the connection's ENQ. A timeout is an idle tick (loop back and re-check stop);
		// a real read error (pipe closed / the past deadline set by close) ends the goroutine.
		b, err := p.readByte(secs1IdlePoll)
		if err != nil {
			if isNetTimeout(err) {
				continue
			}

			return
		}

		if b != secs1ENQ {
			continue // a non-ENQ byte on an idle line is a protocol violation — ignore it
		}

		blk, ok := p.receiveBlock()
		if !ok {
			continue // the block was NAK'd or the turn errored; the connection will retry
		}

		body = append(body, blk.body...)
		if !blk.eBit() {
			continue // more blocks of this message are still to come
		}

		// A full inbound message is reassembled. Record its body and echo it back if the primary set
		// the W-bit (unless the test has toggled replies off).
		p.recordInbound(body)
		if blk.waitBit() && !p.withhold.Load() {
			p.sendReply(blk, body)
		}

		body = body[:0] // reset for the next message (the reply, if any, already serialized these bytes)
	}
}

// receiveBlock runs the peer's receive side of one SEMI E4 handshake (§7.8.4–5). The caller has
// already read the ENQ; this grants the line with EOT, reads and validates the block, and ACKs it.
// It returns the parsed block and true on success; on a bad length/checksum it best-effort NAKs and
// returns false, and on any wire error it returns false.
func (p *secs1Peer) receiveBlock() (secs1Block, bool) {
	if err := p.writeByte(secs1EOT); err != nil { // grant the line
		return secs1Block{}, false
	}

	lengthByte, err := p.readByte(secs1StepTimeout)
	if err != nil {
		return secs1Block{}, false
	}

	n := int(lengthByte)
	if n < secs1MinBlockLen || n > secs1MaxBlockLen {
		_ = p.writeByte(secs1NAK)

		return secs1Block{}, false
	}

	// Read header+body+checksum into an owned buffer.
	buf := make([]byte, n+secs1ChecksumSize)
	if err := p.readFull(buf, secs1StepTimeout); err != nil {
		return secs1Block{}, false
	}

	// Verify the 16-bit arithmetic checksum over the header+body bytes (the length byte excluded).
	var sum uint32
	for _, v := range buf[:n] {
		sum += uint32(v)
	}
	if uint16(sum&0xFFFF) != binary.BigEndian.Uint16(buf[n:n+secs1ChecksumSize]) {
		_ = p.writeByte(secs1NAK)

		return secs1Block{}, false
	}

	if err := p.writeByte(secs1ACK); err != nil {
		return secs1Block{}, false
	}

	var hdr [secs1HeaderSize]byte
	copy(hdr[:], buf[:secs1HeaderSize])
	bodyCopy := append([]byte(nil), buf[secs1HeaderSize:n]...)

	return secs1Block{header: hdr, body: bodyCopy}, true
}

// sendReply delivers the echoed secondary for a W-bit primary. It reuses the primary's deviceID and
// system bytes so the core correlates the reply, reverses the R-bit direction back to the originator,
// sets function+1 with the W-bit clear, and echoes the reassembled primary body verbatim (the core
// decodes the echoed body back into the SECS-II item). The body is split into <=244-byte blocks
// (numbers 1..N, E-bit on the last).
func (p *secs1Peer) sendReply(primary secs1Block, body []byte) {
	h := secs1Header{
		deviceID:    primary.deviceID(),
		rBit:        !primary.rBit(), // reverse direction: to equipment (R=0) becomes to host (R=1)
		stream:      primary.stream(),
		function:    primary.function() + 1,
		waitBit:     false,
		systemBytes: primary.systemBytes(),
	}

	for _, frame := range secs1SplitReply(h, body) {
		if !p.sendBlock(frame) {
			return // torn down, NAK'd, or a non-ACK response — best-effort, so stop
		}
	}
}

// sendBlock performs the peer's send side of one SEMI E4 handshake (§7.8.2–3) for a single packed
// block: request the line (ENQ), await the grant (EOT), write the block, and await the ACK. It
// returns true once the block is ACK'd; false on any wire error, a non-EOT grant, or a non-ACK reply.
func (p *secs1Peer) sendBlock(frame []byte) bool {
	if err := p.writeByte(secs1ENQ); err != nil {
		return false
	}

	if b, err := p.readByte(secs1StepTimeout); err != nil || b != secs1EOT {
		return false
	}

	if err := p.writeAll(frame); err != nil {
		return false
	}

	b, err := p.readByte(secs1StepTimeout)

	return err == nil && b == secs1ACK
}

// originate runs a full peer-originated W-bit transaction on the line goroutine: it takes the line
// as master and sends the primary block(s), then turns around to receive the connection's correlated
// reply. The result (reply body or error) is delivered on the buffered replyCh. Because it runs on
// the single line goroutine, it never races the auto-echo path for the wire.
func (p *secs1Peer) originate(cmd secs1SendCmd) {
	sb := p.nextSystemBytes()

	h := secs1Header{
		deviceID:    0,
		rBit:        true, // equipment→host direction: the connection is the host and accepts R=1
		stream:      cmd.stream,
		function:    cmd.function,
		waitBit:     true, // a W-bit primary — the connection's handler is expected to reply
		systemBytes: sb,
	}

	for _, frame := range secs1SplitReply(h, cmd.body) {
		if !p.sendBlock(frame) {
			cmd.replyCh <- secs1SendResult{err: errSECS1Send}
			return
		}
	}

	body, err := p.awaitReply(sb)
	cmd.replyCh <- secs1SendResult{body: body, err: err}
}

// awaitReply receives the connection's reply to a peer-originated primary. The connection replies as
// master (ENQ), so the peer polls the line, grants EOT, reassembles the reply blocks, and returns the
// body once the E-bit block whose System Bytes match sb arrives. A bounded deadline keeps a withheld
// or lost reply from wedging the line goroutine.
func (p *secs1Peer) awaitReply(sb [4]byte) ([]byte, error) {
	var body []byte

	deadline := time.Now().Add(secs1ReplyTimeout)

	for {
		select {
		case <-p.stop:
			return nil, errSECS1PeerClosed
		default:
		}

		if time.Now().After(deadline) {
			return nil, errSECS1ReplyTimeout
		}

		b, err := p.readByte(secs1IdlePoll)
		if err != nil {
			if isNetTimeout(err) {
				continue
			}

			return nil, err
		}

		if b != secs1ENQ {
			continue
		}

		blk, ok := p.receiveBlock()
		if !ok {
			continue
		}

		body = append(body, blk.body...)
		if !blk.eBit() {
			continue
		}

		if blk.systemBytes() == sb {
			return append([]byte(nil), body...), nil
		}

		body = body[:0] // an unexpected message (not our reply) — discard and keep waiting
	}
}

// recordInbound pushes a reassembled inbound body onto dataIn for inbound(). The push is non-blocking
// (drop-if-full) and copies the body (the caller reuses its reassembly buffer).
func (p *secs1Peer) recordInbound(body []byte) {
	cp := append([]byte(nil), body...)

	select {
	case p.dataIn <- cp:
	default:
	}
}

// nextSystemBytes mints the peer's next outbound System Bytes. The high bit is set to keep the peer's
// own transaction identifiers in a distinct range from the connection's generator. Called only on the
// line goroutine.
func (p *secs1Peer) nextSystemBytes() [4]byte {
	p.sysCounter++

	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], 0x80000000|p.sysCounter)

	return sb
}

// sendPrimary makes the peer originate a W-bit primary and returns the correlated reply body. It
// posts the request to the line goroutine and waits on a buffered response channel, so it never adds
// a second writer to the half-duplex line.
func (p *secs1Peer) sendPrimary(stream, function byte, body []byte) ([]byte, error) {
	cmd := secs1SendCmd{
		stream:   stream,
		function: function,
		body:     append([]byte(nil), body...),
		replyCh:  make(chan secs1SendResult, 1),
	}

	select {
	case p.cmds <- cmd:
	case <-p.stop:
		return nil, errSECS1PeerClosed
	}

	select {
	case res := <-cmd.replyCh:
		return res.body, res.err
	case <-p.stop:
		return nil, errSECS1PeerClosed
	}
}

// inbound exposes the buffered stream of inbound data bodies for fire-and-forget assertions.
func (p *secs1Peer) inbound() <-chan []byte { return p.dataIn }

// setWithholdReplies toggles the auto-echo of W-bit primaries off/on for the reply-timeout scenario.
func (p *secs1Peer) setWithholdReplies(withhold bool) { p.withhold.Store(withhold) }

// dropLine simulates an involuntary disconnect by tearing the peer down: it stops the line goroutine
// and closes endB, so the connection's recv loop sees EOF and reconnects to a fresh generation. It
// routes through the same goroutine-safe teardown as close, so a later closeAll double-close is a safe
// no-op.
func (p *secs1Peer) dropLine() { p.close() }

// close stops the line goroutine, then closes the pipe. It sets a past deadline to unblock the
// goroutine if it is parked in a pipe Read or Write, waits for it to exit (so nothing touches the
// conn after this point), and only then closes endB — there is no read/write-on-closed race.
func (p *secs1Peer) close() {
	p.closeOnce.Do(func() {
		close(p.stop)
		_ = p.endB.SetDeadline(time.Unix(0, 0)) // in the past: unblocks any parked Read/Write
		p.wg.Wait()
		_ = p.endB.Close()
	})
}

// readByte reads exactly one byte, arming timeout as an absolute read deadline first.
func (p *secs1Peer) readByte(timeout time.Duration) (byte, error) {
	if err := p.endB.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}

	var b [1]byte
	if _, err := io.ReadFull(p.endB, b[:]); err != nil {
		return 0, err
	}

	return b[0], nil
}

// readFull reads exactly len(buf) bytes, arming timeout as an absolute read deadline first.
func (p *secs1Peer) readFull(buf []byte, timeout time.Duration) error {
	if err := p.endB.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}

	_, err := io.ReadFull(p.endB, buf)

	return err
}

// writeByte writes a single handshake character (ENQ/EOT/ACK/NAK).
func (p *secs1Peer) writeByte(b byte) error {
	_, err := p.endB.Write([]byte{b})

	return err
}

// writeAll writes all of data to endB, looping over any short write.
func (p *secs1Peer) writeAll(data []byte) error {
	for len(data) > 0 {
		n, err := p.endB.Write(data)
		if err != nil {
			return err
		}

		data = data[n:]
	}

	return nil
}

// secs1BuildHeader packs a secs1Header plus the per-block number and last-block flag into the 10-byte
// SECS-I header (SEMI E4 §8): the R-bit and E-bit occupy the top bit of bytes 0 and 4, the W-bit the
// top bit of byte 2, and deviceID/blockNumber are 15-bit big-endian.
func secs1BuildHeader(h secs1Header, blockNumber uint16, last bool) [secs1HeaderSize]byte {
	var b [secs1HeaderSize]byte

	b[0] = byte(h.deviceID >> 8)
	if h.rBit {
		b[0] |= 0x80
	}
	b[1] = byte(h.deviceID)
	b[2] = h.stream & 0x7F
	if h.waitBit {
		b[2] |= 0x80
	}
	b[3] = h.function
	b[4] = byte(blockNumber >> 8)
	if last {
		b[4] |= 0x80
	}
	b[5] = byte(blockNumber)
	copy(b[6:secs1HeaderSize], h.systemBytes[:])

	return b
}

// secs1PackBlock serializes a header and body into one SEMI E4 wire block:
//
//	[length][header(10)][body][checksum hi][checksum lo]
//
// length = 10 + len(body). The checksum is the 16-bit arithmetic sum of the header+body bytes (the
// length byte excluded), written big-endian.
func secs1PackBlock(header [secs1HeaderSize]byte, body []byte) []byte {
	n := secs1HeaderSize + len(body)

	frame := make([]byte, 0, 1+n+secs1ChecksumSize)
	frame = append(frame, byte(n))
	frame = append(frame, header[:]...)
	frame = append(frame, body...)

	var sum uint32
	for _, v := range frame[1:] { // header+body, skipping the length byte
		sum += uint32(v)
	}
	cs := uint16(sum & 0xFFFF)

	return append(frame, byte(cs>>8), byte(cs))
}

// secs1SplitReply partitions body into packed SECS-I reply blocks of at most 244 body bytes each,
// numbered 1..N with the E-bit set on the final block. An empty body yields exactly one header-only
// block (number 1, E-bit set).
func secs1SplitReply(h secs1Header, body []byte) [][]byte {
	if len(body) == 0 {
		return [][]byte{secs1PackBlock(secs1BuildHeader(h, 1, true), nil)}
	}

	var frames [][]byte
	blockNumber := uint16(1)
	for off := 0; off < len(body); off += secs1MaxBodySize {
		end := off + secs1MaxBodySize
		if end > len(body) {
			end = len(body)
		}
		last := end == len(body)
		frames = append(frames, secs1PackBlock(secs1BuildHeader(h, blockNumber, last), body[off:end]))
		blockNumber++
	}

	return frames
}

// isNetTimeout reports whether err is a deadline expiry (an idle poll tick), distinguishing it from a
// real read error (pipe closed / the past deadline set by close).
func isNetTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}
