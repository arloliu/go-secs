package secs1

// line_test.go — TDD coverage for the SP5b T1 pure line-transfer mechanics (secs1/line.go): the
// byte primitives, single-block receiveBlock, and the single-block send happy-path with contention
// DETECTION (the slave-yield ACTION and the RTY loop are T2). Each test drives a scripted raw-TCP
// peer over loopback — no engine goroutine, no time.Sleep for synchronisation.

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/stretchr/testify/require"
)

// --- Test harness ---

// newLineTestConfig builds a Config with short T1/T2 so the timeout paths resolve fast under
// -race -count. Later options win, so a caller may override the defaults.
func newLineTestConfig(t *testing.T, opts ...Option) Config {
	t.Helper()
	base := []Option{WithT1(50 * time.Millisecond), WithT2(150 * time.Millisecond)}
	cfg, err := NewConfig("127.0.0.1", 5000, append(base, opts...)...)
	require.NoError(t, err)

	return cfg
}

// newLinePair wires a lineIO (the "our side") to a raw scripted peer over a loopback TCP pair. The
// returned net.Conn is the peer end the test script reads/writes control bytes and block wire bytes
// on. All three sockets are closed on cleanup.
func newLinePair(t *testing.T, cfg Config) (*lineIO, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- accepted{conn: c, err: aerr}
	}()

	local, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })

	a := <-ch
	require.NoError(t, a.err)
	t.Cleanup(func() { _ = a.conn.Close() })

	return newLineIO(local, cfg), a.conn
}

// peerWrite writes b to the scripted peer, flagging (not aborting) on error so it is safe to call
// from the peer goroutine.
func peerWrite(t *testing.T, conn net.Conn, b []byte) {
	t.Helper()
	if _, err := conn.Write(b); err != nil {
		t.Errorf("peer write: %v", err)
	}
}

// peerReadN reads exactly n bytes from the scripted peer. t.Errorf (goroutine-safe) flags failure
// without a fatal Goexit.
func peerReadN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Errorf("peer readN(%d): %v", n, err)
		return nil
	}

	return buf
}

// makeTestBlock builds a golden block with the given body (nil => header-only). The header pins a
// non-trivial field layout so the wire-byte and roundtrip assertions have teeth.
func makeTestBlock(t *testing.T, body []byte) block {
	t.Helper()
	h := messageHeader{
		deviceID:    0x1234,
		rBit:        true,
		stream:      6,
		function:    0x11,
		waitBit:     true,
		systemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	var chunk wire.Chunk
	if len(body) > 0 {
		chunk = wire.AdoptBody(body).Chunk(0, len(body))
	}

	return block{header: buildHeader(h, 1, true), body: chunk}
}

// --- receiveBlock ---

func TestLineReceiveBlock_HappyPath(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, []byte("hi"))
	goldenWire := golden.appendTo(nil)

	ackCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, goldenWire)
		ackCh <- peerReadN(t, peer, 1)
	}()

	got, err := line.receiveBlock(context.Background())
	require.NoError(t, err)
	require.Equal(t, goldenWire, got.appendTo(nil), "received block must roundtrip to the golden wire form")
	require.Equal(t, []byte{ack}, <-ackCh, "receiver must ACK a valid block")
}

func TestLineReceiveBlock_HeaderOnly(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, nil) // header-only
	goldenWire := golden.appendTo(nil)

	ackCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, goldenWire)
		ackCh <- peerReadN(t, peer, 1)
	}()

	got, err := line.receiveBlock(context.Background())
	require.NoError(t, err)
	require.Equal(t, goldenWire, got.appendTo(nil))
	require.Equal(t, []byte{ack}, <-ackCh)
}

func TestLineReceiveBlock_BadChecksum(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	wireBytes := makeTestBlock(t, []byte("hi")).appendTo(nil)
	wireBytes[len(wireBytes)-1] ^= 0xFF // corrupt the checksum low byte

	nakCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, wireBytes)
		nakCh <- peerReadN(t, peer, 1)
	}()

	_, err := line.receiveBlock(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrChecksumMismatch)
	require.Equal(t, []byte{nak}, <-nakCh, "receiver must NAK a checksum mismatch")
}

func TestLineReceiveBlock_InvalidLength(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	nakCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, []byte{5}) // 5 < minBlockLength (10)
		nakCh <- peerReadN(t, peer, 1)
	}()

	_, err := line.receiveBlock(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidLength)
	require.Equal(t, []byte{nak}, <-nakCh)
}

func TestLineReceiveBlock_T2LengthTimeout(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	nakCh := make(chan []byte, 1)
	go func() {
		// Peer sends nothing; the receiver T2-times out waiting for the length byte, then NAKs.
		nakCh <- peerReadN(t, peer, 1)
	}()

	_, err := line.receiveBlock(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrT2Timeout)
	require.Equal(t, []byte{nak}, <-nakCh)
}

func TestLineReceiveBlock_T1MidBlockTimeout(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	nakCh := make(chan []byte, 1)
	go func() {
		peerWrite(t, peer, []byte{minBlockLength}) // valid length byte (10), then stall
		nakCh <- peerReadN(t, peer, 1)
	}()

	_, err := line.receiveBlock(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrT1Timeout)
	require.Equal(t, []byte{nak}, <-nakCh)
}

func TestLineReceiveBlock_CtxCancelled(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, _ := newLinePair(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := line.receiveBlock(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// --- sendBlockOnce ---

func TestLineSendBlockOnce_HappyPath(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, []byte("payload"))
	goldenWire := golden.appendTo(nil)

	type peerResult struct {
		first byte
		block []byte
	}
	resCh := make(chan peerResult, 1)
	go func() {
		first := peerReadN(t, peer, 1)             // ENQ
		peerWrite(t, peer, []byte{eot})            // EOT
		blk := peerReadN(t, peer, len(goldenWire)) // block wire bytes
		peerWrite(t, peer, []byte{ack})            // ACK
		var fb byte
		if len(first) == 1 {
			fb = first[0]
		}
		resCh <- peerResult{first: fb, block: blk}
	}()

	res, err := line.sendBlockOnce(context.Background(), golden)
	require.NoError(t, err)
	require.Equal(t, sendOK, res)

	pr := <-resCh
	require.Equal(t, enq, pr.first, "line control must open with ENQ")
	require.Equal(t, goldenWire, pr.block, "transmitted block must equal the golden wire form")
}

func TestLineSendBlockOnce_NonACK(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, nil)
	goldenWire := golden.appendTo(nil)

	go func() {
		peerReadN(t, peer, 1)               // ENQ
		peerWrite(t, peer, []byte{eot})     // EOT
		peerReadN(t, peer, len(goldenWire)) // block
		peerWrite(t, peer, []byte{0xFF})    // non-ACK reply
	}()

	res, err := line.sendBlockOnce(context.Background(), golden)
	require.Equal(t, sendRetry, res, "a non-ACK reply is a retryable failure")
	require.Error(t, err)
}

func TestLineSendBlockOnce_T2AckTimeout(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, nil)
	goldenWire := golden.appendTo(nil)

	go func() {
		peerReadN(t, peer, 1)               // ENQ
		peerWrite(t, peer, []byte{eot})     // EOT
		peerReadN(t, peer, len(goldenWire)) // block, then send no ACK (T2 elapses)
	}()

	res, err := line.sendBlockOnce(context.Background(), golden)
	require.Equal(t, sendRetry, res)
	require.ErrorIs(t, err, ErrT2Timeout)
}

func TestLineSendBlockOnce_T2EnqTimeout(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, peer := newLinePair(t, cfg)

	go func() {
		peerReadN(t, peer, 1) // ENQ, then never respond (T2 elapses waiting for EOT)
	}()

	res, err := line.sendBlockOnce(context.Background(), makeTestBlock(t, nil))
	require.Equal(t, sendRetry, res)
	require.ErrorIs(t, err, ErrT2Timeout)
}

func TestLineSendBlockOnce_SlaveContentionDetected(t *testing.T) {
	cfg := newLineTestConfig(t) // default role is host (slave)
	require.False(t, cfg.IsEquip())
	line, peer := newLinePair(t, cfg)

	go func() {
		peerReadN(t, peer, 1)           // ENQ
		peerWrite(t, peer, []byte{enq}) // peer ENQ => contention
	}()

	// The slave DETECTS contention and reports it WITHOUT yielding; the yield action is T2.
	res, err := line.sendBlockOnce(context.Background(), makeTestBlock(t, nil))
	require.NoError(t, err)
	require.Equal(t, sendContention, res)
}

func TestLineSendBlockOnce_MasterIgnoresContention(t *testing.T) {
	cfg := newLineTestConfig(t, WithEquipment()) // equipment == master
	require.True(t, cfg.IsEquip())
	line, peer := newLinePair(t, cfg)

	golden := makeTestBlock(t, nil)
	goldenWire := golden.appendTo(nil)

	go func() {
		peerReadN(t, peer, 1)               // ENQ
		peerWrite(t, peer, []byte{enq})     // contention — master must ignore it
		peerWrite(t, peer, []byte{eot})     // then grant with EOT
		peerReadN(t, peer, len(goldenWire)) // block
		peerWrite(t, peer, []byte{ack})     // ACK
	}()

	res, err := line.sendBlockOnce(context.Background(), golden)
	require.NoError(t, err)
	require.Equal(t, sendOK, res, "master ignores a contending ENQ and completes the send")
}

func TestLineSendBlockOnce_CtxCancelled(t *testing.T) {
	cfg := newLineTestConfig(t)
	line, _ := newLinePair(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := line.sendBlockOnce(ctx, makeTestBlock(t, nil))
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, sendAbort, res)
}
