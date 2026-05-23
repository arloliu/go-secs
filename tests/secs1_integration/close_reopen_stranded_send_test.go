package secs1integration

// TestSECS1_CloseReopen_NoStaleBlockAcrossGeneration is a positive regression
// test for the send-gate fix in secs1/conn.go (commit 3bdddd3).
//
// Invariant under test (wire-observable):
//
//	After Connection.Close(), calling Connection.Open() on the same
//	*Connection must never deliver a block from a pre-Close Send call to the
//	new TCP generation's peer.  The first block the gen-2 peer receives must
//	belong to a send issued AFTER Open returned.
//
// Generation distinguisher: stream code.
//
//	Gen-1 senders use stream=1 (S1F1, W-bit=0).
//	Gen-2 send     uses stream=3 (S3F1, W-bit=0).
//
// A gen-1 block reaching the gen-2 passive peer means send-gate failed.
// The cycle repeats 30 times to expose the race window reliably.

import (
	"bufio"
	"context"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/secs1"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// passivePeer — raw TCP peer (passive equipment role).
//
// Accepts exactly one TCP connection per run. For each block the host sends
// (ENQ → peer sends EOT → peer reads block + sends ACK), it records
// (stream, function) from the block header.
// ---------------------------------------------------------------------------

type passivePeer struct {
	ln       net.Listener
	port     int
	mu       sync.Mutex
	received []blockSF // stream+function of every received block
	wg       sync.WaitGroup
	stopOnce sync.Once
}

type blockSF struct {
	stream   byte
	function byte
}

// newPassivePeerOnPort opens a TCP listener on port and starts the protocol
// goroutine.  t.Cleanup registers the stop call for safety; the test also
// calls stop() explicitly before recycling the port.
func newPassivePeerOnPort(t *testing.T, port int) *passivePeer {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	require.NoError(t, err)

	p := &passivePeer{ln: ln, port: port}
	p.wg.Add(1)

	go p.run()

	t.Cleanup(p.stop)

	return p
}

// stop closes the listener and waits for the goroutine to exit.
func (p *passivePeer) stop() {
	p.stopOnce.Do(func() {
		_ = p.ln.Close()
	})
	p.wg.Wait()
}

// snapshot returns a copy of received (stream, function) pairs.
func (p *passivePeer) snapshot() []blockSF {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]blockSF, len(p.received))
	copy(out, p.received)

	return out
}

// hasAny returns true if at least one block has been recorded.
func (p *passivePeer) hasAny() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.received) > 0
}

// run accepts one connection and handles the passive SECS-I line-control loop.
func (p *passivePeer) run() {
	defer p.wg.Done()

	conn, err := p.ln.Accept()
	if err != nil {
		// Listener closed normally.
		return
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	for {
		// Wait for the host's ENQ.
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return
		}

		b, err := reader.ReadByte()
		if err != nil {
			return
		}

		if b != secs1.ENQ {
			continue
		}

		// Grant line control.
		if _, err := conn.Write([]byte{secs1.EOT}); err != nil {
			return
		}

		// Read length byte.
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return
		}

		lengthByte, err := reader.ReadByte()
		if err != nil {
			return
		}

		// Read header + body + checksum.
		remaining := int(lengthByte) + 2
		buf := make([]byte, remaining)

		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return
		}

		if _, err := io.ReadFull(reader, buf); err != nil {
			return
		}

		blk, parseErr := secs1.ParseBlock(lengthByte, buf)
		if parseErr != nil {
			_, _ = conn.Write([]byte{secs1.NAK})
			continue
		}

		// ACK.
		if _, err := conn.Write([]byte{secs1.ACK}); err != nil {
			return
		}

		// Record (stream, function).
		p.mu.Lock()
		p.received = append(p.received, blockSF{
			stream:   blk.StreamCode(),
			function: blk.FunctionCode(),
		})
		p.mu.Unlock()
	}
}

// itoa converts a non-negative int to decimal ASCII without importing strconv
// at file scope (strconv is already in helpers_test.go in this package).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte
	pos := len(buf)

	for n > 0 {
		pos--
		buf[pos] = byte(n%10) + '0'
		n /= 10
	}

	return string(buf[pos:])
}

// ---------------------------------------------------------------------------
// TestSECS1_CloseReopen_NoStaleBlockAcrossGeneration
// ---------------------------------------------------------------------------

func TestSECS1_CloseReopen_NoStaleBlockAcrossGeneration(t *testing.T) {
	const (
		cycles      = 30 // close-reopen cycles
		senderCount = 50 // concurrent senders per gen-1 phase
		senderIters = 5  // loop iterations per sender goroutine

		// Wire-distinguishable stream codes.
		gen1Stream = byte(1) // gen-1 senders: S1F1
		gen2Stream = byte(3) // gen-2 send:    S3F1
	)

	ctx, cancel := context.WithTimeout(t.Context(), 170*time.Second)
	defer cancel()

	// The host Connection's host:port is fixed; both peer generations reuse
	// the same port (old peer is fully stopped before new one starts).
	port := getFreePort(t)

	// Build the host endpoint once; Open/Close it per cycle.
	hostEP := newEndpointWithOpts(t, ctx, port, false, true,
		secs1.WithT1Timeout(secs1.MinT1Timeout),
		secs1.WithT2Timeout(secs1.MinT2Timeout),
		secs1.WithT3Timeout(secs1.MinT3Timeout),
		secs1.WithRetryLimit(1),
		secs1.WithConnectRemoteTimeout(500*time.Millisecond),
		secs1.WithSendTimeout(200*time.Millisecond),
	)

	// Counters for post-test sanity logging.
	var totalGen1Sends atomic.Int64
	var totalGen1Errors atomic.Int64

	for cycle := range cycles {
		// ----------------------------------------------------------------
		// Gen-1 phase: start peer, open host, flood sends, close.
		// ----------------------------------------------------------------
		gen1Peer := newPassivePeerOnPort(t, port)

		require.NoError(t, hostEP.conn.Open(true),
			"cycle %d: Open (gen-1) failed", cycle)

		waitSelected(t, hostEP, 5*time.Second)

		// Saturate the send channel so some calls race with Close.
		var senderWg sync.WaitGroup
		senderWg.Add(senderCount)

		for range senderCount {
			go func() {
				defer senderWg.Done()

				for range senderIters {
					err := hostEP.session.SendDataMessageAsync(
						gen1Stream, 1, false, secs2.A("gen1"),
					)
					totalGen1Sends.Add(1)

					if err != nil {
						totalGen1Errors.Add(1)
					}

					runtime.Gosched()
				}
			}()
		}

		// Small sleep to widen the race window: Close fires while some
		// sender goroutines are still calling SendDataMessageAsync.
		// Per 300-testing.md: sleep to *inject* a delay into the scenario.
		time.Sleep(1 * time.Millisecond)

		// Close — sets sendClosed=true, drains any in-flight sends.
		require.NoError(t, hostEP.conn.Close(),
			"cycle %d: Close (gen-1) failed", cycle)

		// Wait for all sender goroutines to return.
		senderWg.Wait()

		// ----------------------------------------------------------------
		// Gate check: SendDataMessageAsync after Close must error, not hang.
		// ----------------------------------------------------------------
		gateDone := make(chan error, 1)
		go func() {
			gateDone <- hostEP.session.SendDataMessageAsync(gen1Stream, 1, false, nil)
		}()

		select {
		case gateErr := <-gateDone:
			require.Error(t, gateErr,
				"cycle %d: SendDataMessageAsync after Close must return an error (send gate should be closed)",
				cycle)
		case <-time.After(2 * time.Second):
			t.Fatalf("cycle %d: SendDataMessageAsync blocked after Close — possible deadlock", cycle)
		}

		// Fully stop gen-1 peer before rebinding the port.
		gen1Peer.stop()

		// ----------------------------------------------------------------
		// Gen-2 phase: new peer on same port, reopen, send, assert.
		// ----------------------------------------------------------------
		gen2Peer := newPassivePeerOnPort(t, port)

		require.NoError(t, hostEP.conn.Open(true),
			"cycle %d: Open (gen-2) failed", cycle)

		waitSelected(t, hostEP, 5*time.Second)

		// Send ONE gen-2-distinguishable message.
		sendErr := hostEP.session.SendDataMessageAsync(
			gen2Stream, 1, false, secs2.A("gen2"),
		)
		require.NoError(t, sendErr,
			"cycle %d: gen-2 SendDataMessageAsync must not fail", cycle)

		// Wait for the gen-2 peer to receive at least one block.
		require.Eventually(t, gen2Peer.hasAny,
			5*time.Second, 10*time.Millisecond,
			"cycle %d: gen-2 peer received no block within 5s", cycle)

		// ----------------------------------------------------------------
		// CORE ASSERTION: every block on the gen-2 connection must be gen-2.
		// ----------------------------------------------------------------
		recv := gen2Peer.snapshot()
		require.NotEmpty(t, recv,
			"cycle %d: snapshot empty after hasAny() returned true", cycle)

		for i, sf := range recv {
			if sf.stream == gen1Stream {
				t.Fatalf(
					"cycle %d: SEND-GATE REGRESSION — recv[%d] has gen-1 stream S%dF%d "+
						"on the gen-2 TCP session. "+
						"gen-1 sends attempted: %d, gen-1 gated: %d. "+
						"A pre-Close block leaked across connection generations.",
					cycle, i, sf.stream, sf.function,
					totalGen1Sends.Load(), totalGen1Errors.Load(),
				)
			}
		}

		// First block must be the gen-2 stream.
		firstStream := recv[0].stream
		require.Equal(t, gen2Stream, firstStream,
			"cycle %d: first gen-2 block has stream S%d, expected S%d",
			cycle, firstStream, gen2Stream)

		// Close gen-2 and stop its peer before the next cycle.
		require.NoError(t, hostEP.conn.Close(),
			"cycle %d: Close (gen-2) failed", cycle)

		gen2Peer.stop()
	}

	// Sanity log: show how often the race window was hit.
	t.Logf("send-gate coverage: %d gen-1 sends attempted, %d gated by Close, over %d cycles",
		totalGen1Sends.Load(), totalGen1Errors.Load(), cycles)
}
