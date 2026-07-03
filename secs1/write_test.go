package secs1

// write_test.go — TDD coverage for the SP5b T4 send half (secs1/transport.go Write + the
// secs1/adapter.go frame->blocks adapter): the §4a flatten+control-guard, the HSMS-header ->
// SECS-I-header mapping and body split, and the epoch-bound (G-C/I1) hand-off to the line engine.
// Each test builds a REAL hsms message, frames it exactly as the core's writeFrame would, and drives
// a scripted raw-TCP peer over loopback + a mock hsms.TransportRuntime — no time.Sleep for sync.

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// --- Test harness ---

// frameBuffers builds the net.Buffers the core's writeFrame hands to Write for msg. It is
// byte-faithful to hsms.buildFrameBuffers (unexported): msg.ToBytes() is
// [4-byte big-endian length][10-byte HSMS header][body], so raw[:14] is the 14-byte prefix and
// raw[14:] the body. A control / empty-body frame (raw len 14) yields a single-slice Buffers (len 1),
// exactly as the core produces for those.
func frameBuffers(t *testing.T, msg hsms.Message) net.Buffers {
	t.Helper()
	raw := msg.ToBytes()
	require.GreaterOrEqual(t, len(raw), 14, "a framed message must have at least a 14-byte prefix")
	if len(raw) == 14 {
		return net.Buffers{raw}
	}

	return net.Buffers{raw[:14], raw[14:]}
}

// liveConn returns the current generation's live socket (the epoch conn the core would pass to Write)
// and doubles as an assertion that Write's gen bundle was published by bring-up.
func liveConn(t *testing.T, tr *transport) net.Conn {
	t.Helper()
	gs := tr.gen.Load()
	require.NotNil(t, gs, "the generation state must be published after bring-up")

	return gs.conn
}

// bringUpActiveWithConfig mirrors bringUpActive (transport_test.go) but threads config Options
// (DeviceID, role, timers) so a Write test can assert the configured deviceID / R-bit on the wire.
func bringUpActiveWithConfig(t *testing.T, rt *mockRuntime, opts ...Option) (*transport, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	cfg := transportTestConfig(t, tcpAddr.Port, opts...)

	tr := newTransport(cfg)

	peerCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			peerCh <- c
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tr.ArmStart()
	require.NoError(t, tr.Start(ctx, rt))

	var peer net.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("active transport: peer did not connect")
	}
	t.Cleanup(func() { _ = peer.Close() })

	return tr, peer
}

// readWireBlock reads one full SECS-I block wire form ([length][header+body+checksum]) from the peer
// and parses it. The caller must have already consumed the ENQ and sent EOT.
func readWireBlock(t *testing.T, peer net.Conn) block {
	t.Helper()
	lenByte := peerReadN(t, peer, 1)
	rest := peerReadN(t, peer, int(lenByte[0])+checksumSize)
	blk, err := parseBlock(lenByte[0], rest)
	require.NoError(t, err, "peer must receive a well-formed SECS-I block")

	return blk
}

// --- (a) single-block data send ---

// TestWrite_SingleBlockDataSend frames a real S1F1 (W-bit) and asserts Write drives one SECS-I block
// carrying the SAME stream/function/system-bytes plus the configured deviceID and R-bit, E-bit set on
// the single (last) block, body <=244; the peer EOT/ACKs; Write returns nil only after the ACK.
func TestWrite_SingleBlockDataSend(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActiveWithConfig(t, rt, WithDeviceID(0x0ABC), WithEquipment())
	conn := liveConn(t, tr)

	sysBytes := [4]byte{0x11, 0x22, 0x33, 0x44}
	item := secs2.A("ping")
	msg, err := hsms.NewDataMessage(1, 1, true, 0xFFFF, sysBytes, item)
	require.NoError(t, err)
	bufs := frameBuffers(t, msg)
	wantBody := msg.ToBytes()[14:] // the on-wire SECS-II body

	blkCh := make(chan block, 1)
	go func() {
		first := peerReadN(t, peer, 1) // ENQ
		if len(first) == 1 && first[0] != enq {
			t.Errorf("engine send must open with ENQ, got 0x%02X", first[0])
		}
		peerWrite(t, peer, []byte{eot}) // grant the line
		blk := readWireBlock(t, peer)
		peerWrite(t, peer, []byte{ack}) // ACK
		blkCh <- blk
	}()

	require.NoError(t, tr.Write(context.Background(), conn, bufs),
		"a single-block data send must return nil once the block ACKs")

	blk := <-blkCh
	require.Equal(t, uint8(1), blk.stream(), "stream must survive the HSMS->SECS-I header map")
	require.Equal(t, uint8(1), blk.function(), "function must survive the map")
	require.Equal(t, sysBytes, blk.systemBytes(), "system bytes must survive the map")
	require.True(t, blk.waitBit(), "the W-bit must survive the map")
	require.Equal(t, uint16(0x0ABC), blk.deviceID(), "the block must carry the configured deviceID")
	require.True(t, blk.rBit(), "R-bit must reflect the configured equipment role")
	require.Equal(t, uint16(1), blk.blockNumber(), "the sole block is block 1")
	require.True(t, blk.eBit(), "the sole block is the last block (E-bit set)")
	require.LessOrEqual(t, blk.body.Len(), maxBlockBodySize, "a block body may not exceed 244 bytes")
	require.Equal(t, wantBody, blk.body.AppendTo(nil), "the block body must equal the framed SECS-II body")

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// --- (b) multi-block data send ---

// TestWrite_MultiBlockDataSend frames a body > 244 bytes and asserts Write emits N ordered blocks
// (numbers 1..N, E-bit only on N, invariant header across blocks, each body <=244); the peer ACKs
// each; Write returns nil only after the LAST block ACKs. The frame body is handed to Write split
// across MULTIPLE sub-slices to exercise the adapter's body-gather concatenation.
func TestWrite_MultiBlockDataSend(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActiveWithConfig(t, rt, WithDeviceID(0x0123))
	conn := liveConn(t, tr)

	sysBytes := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	item := secs2.A(strings.Repeat("x", 600)) // > 244 encoded => several blocks
	msg, err := hsms.NewDataMessage(6, 11, false, 0xFFFF, sysBytes, item)
	require.NoError(t, err)

	raw := msg.ToBytes()
	body := raw[14:]
	wantBody := append([]byte(nil), body...)
	require.Greater(t, len(body), maxBlockBodySize, "the test body must exceed one block")
	wantN := (len(body) + maxBlockBodySize - 1) / maxBlockBodySize

	// Hand the body to Write as MULTIPLE sub-slices (prefix + two body halves) to exercise the
	// adapter's multi-slice concatenation path.
	half := len(body) / 2
	bufs := net.Buffers{raw[:14], body[:half], body[half:]}

	blocksCh := make(chan []block, 1)
	go func() {
		var got []block
		for {
			first := peerReadN(t, peer, 1) // ENQ
			if len(first) == 1 && first[0] != enq {
				t.Errorf("each block send must open with ENQ, got 0x%02X", first[0])
			}
			peerWrite(t, peer, []byte{eot}) // grant
			blk := readWireBlock(t, peer)
			got = append(got, blk)
			peerWrite(t, peer, []byte{ack}) // ACK
			if blk.eBit() {
				break // last block
			}
		}
		blocksCh <- got
	}()

	require.NoError(t, tr.Write(context.Background(), conn, bufs),
		"a multi-block send must return nil only after the last block ACKs")

	got := <-blocksCh
	require.Len(t, got, wantN, "the body must split into ceil(len/244) blocks")
	for i, blk := range got {
		require.Equal(t, uint16(i+1), blk.blockNumber(), "block numbers must be the contiguous sequence 1..N")
		require.Equal(t, i == len(got)-1, blk.eBit(), "the E-bit must be set exactly on the last block")
		require.LessOrEqual(t, blk.body.Len(), maxBlockBodySize, "each block body must be <=244 bytes")
	}

	// assembleBlocks re-validates block numbers, the E-bit, and header invariance, then returns the
	// concatenated body — a full round-trip check of ordering + the invariant header.
	mh, gotBody, aerr := assembleBlocks(got)
	require.NoError(t, aerr, "the emitted blocks must reassemble (ordering + invariant header)")
	require.Equal(t, wantBody, gotBody.AppendTo(nil), "the reassembled body must equal the framed body")
	require.Equal(t, uint16(0x0123), mh.deviceID, "the invariant header must carry the configured deviceID")
	require.Equal(t, uint8(6), mh.stream)
	require.Equal(t, uint8(11), mh.function)
	require.Equal(t, sysBytes, mh.systemBytes)

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a clean Stop must not fire TCPDown (C1 guard)")
}

// --- (c) control frame dropped (teeth) ---

// TestWrite_ControlFrameDropped proves the P0-2 control guard: an HSMS control frame (SType != 0 —
// here a Separate.req) is DROPPED with no wire I/O and no hand-off, and Write returns nil. This is
// what neutralizes the core's graceful Separate farewell so a v1 SECS-I peer never sees HSMS control
// bytes.
//
// Teeth-check: remove the `header[5] != 0` guard in Write and this fails — the frame is built as a
// data frame and handed off, so the engine puts an ENQ on the wire (the peer read sees n==1).
func TestWrite_ControlFrameDropped(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)
	conn := liveConn(t, tr)

	sep := hsms.NewSeparateReq(0xFFFF, [4]byte{0x01, 0x02, 0x03, 0x04})
	bufs := frameBuffers(t, sep)
	require.NotEqual(t, byte(0), bufs[0][9], "sanity: a Separate.req is a control frame (SType != 0)")

	writeErr := make(chan error, 1)
	go func() { writeErr <- tr.Write(context.Background(), conn, bufs) }()

	// The engine must send NOTHING for a control frame: the peer read must time out with zero bytes.
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	buf := make([]byte, 4)
	n, rerr := peer.Read(buf)
	require.Equal(t, 0, n, "a dropped control frame must put NO bytes on the wire (teeth: an ENQ appears without the SType guard)")
	require.Error(t, rerr, "the peer read must time out — the engine sent nothing")
	var ne net.Error
	require.True(t, errors.As(rerr, &ne) && ne.Timeout(), "expected a read timeout, got %v", rerr)

	select {
	case err := <-writeErr:
		require.NoError(t, err, "Write must return nil for a dropped control frame")
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return for a dropped control frame")
	}

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "dropping a control frame is not a comms failure — no TCPDown")
}

// --- (d) stale-conn I1 ---

// TestWrite_StaleConnReturnsConnClosed proves the I1 binding: a Write whose conn is NOT the live
// generation's socket (a stale sender pinned to a superseded generation) is refused with
// ErrConnClosed and touches no wire — never handing off onto the live generation's engine.
//
// Teeth-check: weaken the `gs.conn != conn` check (accept any non-nil gen) and this fails — Write
// hands off onto the live engine, which ENQs the real peer, and the send never returns ErrConnClosed.
func TestWrite_StaleConnReturnsConnClosed(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)

	// A throwaway conn that is NOT the live generation's socket.
	stale, other := net.Pipe()
	t.Cleanup(func() { _ = stale.Close() })
	t.Cleanup(func() { _ = other.Close() })

	msg, err := hsms.NewDataMessage(1, 1, true, 0xFFFF, [4]byte{1, 2, 3, 4}, secs2.A("stale"))
	require.NoError(t, err)
	bufs := frameBuffers(t, msg)

	require.ErrorIs(t, tr.Write(context.Background(), stale, bufs), hsms.ErrConnClosed,
		"a Write bound to a non-live conn must return ErrConnClosed")

	// No wire I/O on the real peer: its read must time out with zero bytes.
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	buf := make([]byte, 1)
	n, _ := peer.Read(buf)
	require.Equal(t, 0, n, "a stale-conn Write must not touch the live peer's wire")

	require.NoError(t, tr.Stop(context.Background()))
	require.False(t, rt.tcpDownFired(), "a refused stale-conn Write is not a comms failure — no TCPDown")
}

// --- (e) genDone unblocks a parked Write ---

// TestWrite_GenDoneUnblocksParkedWrite proves the G-C teardown release: a Write whose block send
// cannot complete (the peer grants no EOT, so the engine's sendBlock is parked retrying and Write is
// blocked on req.done) is RELEASED by Stop rather than hanging, and never spuriously reports success.
//
// The RELEASE is guaranteed; the specific error is not. Stop closes genDone AND the socket: genDone
// releases the wait select with ErrConnClosed, while the socket close makes the engine's parked send
// fail and deliver a send error on req.done. Both cases of the wait select can therefore be ready at
// once, and Go picks uniformly — so the return is ErrConnClosed (genDone won) OR the send-failure
// error (req.done won). Both are valid outcomes of the G-C contract (a teardown-during-send). The
// load-bearing property this asserts is: the parked Write is released with a NON-NIL error — it does
// not hang, and it never falsely returns nil (a "successful" send) while the link is torn down.
func TestWrite_GenDoneUnblocksParkedWrite(t *testing.T) {
	rt := newMockRuntime()
	tr, peer := bringUpActive(t, rt, nil)
	conn := liveConn(t, tr)

	msg, err := hsms.NewDataMessage(1, 1, true, 0xFFFF, [4]byte{9, 9, 9, 9}, secs2.A("blocked"))
	require.NoError(t, err)
	bufs := frameBuffers(t, msg)

	// The peer reads the engine's ENQ but NEVER grants the line — the engine parks retrying and Write
	// blocks on req.done. Reading the ENQ proves the hand-off happened.
	enqRead := make(chan struct{})
	go func() {
		first := peerReadN(t, peer, 1) // ENQ
		if len(first) == 1 && first[0] != enq {
			t.Errorf("engine send must open with ENQ, got 0x%02X", first[0])
		}
		close(enqRead)
		// never respond
	}()

	writeErr := make(chan error, 1)
	go func() { writeErr <- tr.Write(context.Background(), conn, bufs) }()

	select {
	case <-enqRead:
	case <-time.After(5 * time.Second):
		t.Fatal("engine never sent the ENQ — Write did not hand off")
	}

	// Tear down: close(genDone) must release the parked Write with ErrConnClosed.
	require.NoError(t, tr.Stop(context.Background()))

	select {
	case err := <-writeErr:
		// Released, not hung, and NOT a spurious nil: the return is ErrConnClosed (genDone won the
		// wait select) or the socket-close-driven send failure (req.done won) — both valid; the
		// no-hang + no-spurious-success property is what matters (see the test doc).
		require.Error(t, err,
			"a Write parked on the engine must be released with a non-nil error at teardown, never a spurious success")
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not release the parked Write — it hung")
	}
}
