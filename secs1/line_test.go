package secs1

// line_test.go — TDD coverage for the SP5b T1 pure line-transfer mechanics (secs1/line.go): the
// byte primitives, single-block receiveBlock, and the single-block send happy-path with contention
// DETECTION (the slave-yield ACTION and the RTY loop are T2). Each test drives a scripted raw-TCP
// peer over loopback — no engine goroutine, no time.Sleep for synchronisation.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
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
	base := []Option{WithT1(50 * time.Millisecond), WithT2(150 * time.Millisecond), WithDeviceID(0x1234)}
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

	return newLineIO(local, cfg, cfg.Timers, &ConnectionMetrics{}), a.conn
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

type scriptedRead struct {
	data []byte
	err  error
}

type scriptedConn struct {
	reads         []scriptedRead
	writeErr      error
	writeHook     func([]byte)
	writeAttempts [][]byte
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}

	step := &c.reads[0]
	n := copy(p, step.data)
	if n < len(step.data) {
		step.data = step.data[n:]

		return n, nil
	}

	err := step.err
	c.reads = c.reads[1:]

	return n, err
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	attempt := append([]byte(nil), p...)
	c.writeAttempts = append(c.writeAttempts, attempt)
	if c.writeHook != nil {
		c.writeHook(attempt)
	}
	if c.writeErr != nil {
		return 0, c.writeErr
	}

	return len(p), nil
}

func (*scriptedConn) Close() error                     { return nil }
func (*scriptedConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (*scriptedConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (*scriptedConn) SetDeadline(time.Time) error      { return nil }
func (*scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptedConn) SetWriteDeadline(time.Time) error { return nil }

var _ net.Conn = (*scriptedConn)(nil)

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
	require.Equal(t, uint64(1), line.metrics.BlockNAKSentCount(), "a NAK'd inbound block must be counted")
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

func TestLineReceiveBlock_LengthIOError(t *testing.T) {
	readErr := errors.New("length read failed")
	cfg := newLineTestConfig(t)
	conn := &scriptedConn{reads: []scriptedRead{{err: readErr}}}
	line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

	_, err := line.receiveBlock(t.Context())
	require.ErrorIs(t, err, readErr)
	require.NotErrorIs(t, err, ErrT1Timeout)
	require.NotErrorIs(t, err, ErrT2Timeout)
	require.Empty(t, conn.writeAttempts)
	require.Zero(t, line.metrics.BlockNAKSentCount())
}

func TestLineReceiveBlock_MidBodyIOError(t *testing.T) {
	readErr := errors.New("body read failed")
	cfg := newLineTestConfig(t)
	conn := &scriptedConn{reads: []scriptedRead{
		{data: []byte{minBlockLength}},
		{data: []byte{0x01}, err: readErr},
	}}
	line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

	_, err := line.receiveBlock(t.Context())
	require.ErrorIs(t, err, readErr)
	require.NotErrorIs(t, err, ErrT1Timeout)
	require.NotErrorIs(t, err, ErrT2Timeout)
	require.Empty(t, conn.writeAttempts)
	require.Zero(t, line.metrics.BlockNAKSentCount())
}

func TestLineReceiveBlock_NAKWriteFailure(t *testing.T) {
	nakWriteErr := errors.New("NAK write failed")
	badChecksum := makeTestBlock(t, []byte("bad checksum")).appendTo(nil)
	badChecksum[len(badChecksum)-1] ^= 0xFF

	tests := []struct {
		name    string
		reads   []scriptedRead
		wantErr error
	}{
		{
			name:    "T2 length timeout",
			reads:   []scriptedRead{{err: os.ErrDeadlineExceeded}},
			wantErr: ErrT2Timeout,
		},
		{
			name: "T1 body timeout",
			reads: []scriptedRead{
				{data: []byte{minBlockLength}},
				{data: []byte{0x01}, err: os.ErrDeadlineExceeded},
			},
			wantErr: ErrT1Timeout,
		},
		{
			name: "invalid length",
			reads: []scriptedRead{
				{data: []byte{minBlockLength - 1}},
				{err: os.ErrDeadlineExceeded},
			},
			wantErr: ErrInvalidLength,
		},
		{
			name: "checksum mismatch",
			reads: []scriptedRead{
				{data: badChecksum},
				{err: os.ErrDeadlineExceeded},
			},
			wantErr: ErrChecksumMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &scriptedConn{reads: tt.reads, writeErr: nakWriteErr}
			cfg := newLineTestConfig(t)
			line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

			_, err := line.receiveBlock(t.Context())
			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, [][]byte{{nak}}, conn.writeAttempts)
			require.Zero(t, line.metrics.BlockNAKSentCount())
		})
	}
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

func TestLineSendBlockOnce_IOErrorsAbort(t *testing.T) {
	tests := []struct {
		name     string
		closeACK bool
	}{
		{name: "waiting for EOT"},
		{name: "waiting for ACK", closeACK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newLineTestConfig(t)
			line, peer := newLinePair(t, cfg)
			out := makeTestBlock(t, nil)
			outWire := out.appendTo(nil)

			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				peerReadN(t, peer, 1)
				if tt.closeACK {
					peerWrite(t, peer, []byte{eot})
					peerReadN(t, peer, len(outWire))
				}
				_ = peer.Close()
			}()

			result, err := line.sendBlockOnce(t.Context(), out)
			<-peerDone
			require.Equal(t, sendAbort, result)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrT2Timeout)
		})
	}
}

func TestLineSendBlock_IOErrorsDoNotRetry(t *testing.T) {
	tests := []struct {
		name     string
		closeACK bool
	}{
		{name: "waiting for EOT"},
		{name: "waiting for ACK", closeACK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newLineTestConfig(t)
			line, peer := newLinePair(t, cfg)
			out := makeTestBlock(t, nil)
			outWire := out.appendTo(nil)

			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				peerReadN(t, peer, 1)
				if tt.closeACK {
					peerWrite(t, peer, []byte{eot})
					peerReadN(t, peer, len(outWire))
				}
				_ = peer.Close()
			}()

			err := line.sendBlock(t.Context(), out, cfg.RetryLimit(), failDeliver(t))
			<-peerDone
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrT2Timeout)
			require.Zero(t, line.metrics.BlockRetryCount())
		})
	}
}

func TestLineSendBlock_ContentionYieldIOErrorAborts(t *testing.T) {
	readErr := errors.New("contention receive failed")
	cfg := newLineTestConfig(t)
	conn := &scriptedConn{reads: []scriptedRead{
		{data: []byte{enq}},
		{err: readErr},
	}}
	line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

	err := line.sendBlock(t.Context(), makeTestBlock(t, nil), cfg.RetryLimit(), failDeliver(t))
	require.ErrorIs(t, err, readErr)
	require.Equal(t, [][]byte{{enq}, {eot}}, conn.writeAttempts)
	require.Zero(t, line.metrics.BlockRetryCount())
}

func TestLineSendBlock_ContentionYieldCancellationAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	conn := &scriptedConn{reads: []scriptedRead{{data: []byte{enq}}}}
	conn.writeHook = func(p []byte) {
		if len(p) == 1 && p[0] == eot {
			cancel()
		}
	}
	cfg := newLineTestConfig(t)
	line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

	err := line.sendBlock(ctx, makeTestBlock(t, nil), cfg.RetryLimit(), failDeliver(t))
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, [][]byte{{enq}, {eot}}, conn.writeAttempts)
	require.Zero(t, line.metrics.BlockRetryCount())
}

func TestLineSendBlock_ContentionYieldRetryableFailures(t *testing.T) {
	tests := []struct {
		name  string
		reads []scriptedRead
	}{
		{
			name:  "T2 timeout",
			reads: []scriptedRead{{data: []byte{enq}}, {err: os.ErrDeadlineExceeded}},
		},
		{
			name: "invalid length",
			reads: []scriptedRead{
				{data: []byte{enq}},
				{data: []byte{minBlockLength - 1}},
				{err: os.ErrDeadlineExceeded},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newLineTestConfig(t)
			conn := &scriptedConn{reads: tt.reads}
			line := newLineIO(conn, cfg, cfg.Timers, &ConnectionMetrics{})

			err := line.sendBlock(t.Context(), makeTestBlock(t, nil), 0, failDeliver(t))
			require.ErrorIs(t, err, ErrSendFailed)
			require.Equal(t, [][]byte{{enq}, {eot}, {nak}}, conn.writeAttempts)
			require.Equal(t, uint64(1), line.metrics.BlockRetryCount())
			require.Equal(t, uint64(1), line.metrics.BlockNAKSentCount())
		})
	}
}
