package secs1

import (
	"io"
	"net"
	"testing"

	"github.com/arloliu/go-secs/logger"
)

// newTestConfig creates a ConnectionConfig with short timeouts suitable for tests.
func newTestConfig(t *testing.T, opts ...ConnOption) *ConnectionConfig {
	t.Helper()

	defaults := []ConnOption{
		WithT1Timeout(MinT1Timeout), // 100ms
		WithT2Timeout(MinT2Timeout), // 200ms
	}

	cfg, err := NewConnectionConfig("127.0.0.1", 5000, append(defaults, opts...)...)
	if err != nil {
		t.Fatalf("newTestConfig: %v", err)
	}

	return cfg
}

// newTestTransport creates a blockTransport backed by local end of net.Pipe().
// Returns the transport and the remote end for test simulation.
func newTestTransport(t *testing.T, cfg *ConnectionConfig, onRecv func(*Block)) (*blockTransport, net.Conn) {
	t.Helper()

	local, remote := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = remote.Close()
	})

	bt := newBlockTransport(local, cfg, logger.GetLogger(), onRecv, nil)

	return bt, remote
}

// makeTestBlock creates a minimal S1F1 W block (header-only, no body).
func makeTestBlock() *Block {
	blk := &Block{}
	blk.SetDeviceID(1)
	blk.SetStreamCode(1)
	blk.SetFunctionCode(1)
	blk.SetWBit(true)
	blk.SetEBit(true)
	blk.SetBlockNumber(0)

	return blk
}

// makeTestBlockWithBody creates a block with the given body data.
func makeTestBlockWithBody(body []byte) *Block {
	blk := makeTestBlock()
	blk.Body = make([]byte, len(body))
	copy(blk.Body, body)

	return blk
}

// readExactly reads exactly n bytes from r, failing the test on error.
func readExactly(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()

	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("readExactly: %v", err)
	}

	return buf
}

// readOneByte reads exactly 1 byte from r, failing the test on error.
func readOneByte(t *testing.T, r io.Reader) byte {
	t.Helper()

	return readExactly(t, r, 1)[0]
}

// mustWrite writes data to w, failing the test on error.
func mustWrite(t *testing.T, w io.Writer, data []byte) {
	t.Helper()

	_, err := w.Write(data)
	if err != nil {
		t.Fatalf("mustWrite: %v", err)
	}
}

// newPipeConn creates a net.Pipe pair and registers cleanup.
func newPipeConn(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	local, remote := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = remote.Close()
	})

	return local, remote
}
