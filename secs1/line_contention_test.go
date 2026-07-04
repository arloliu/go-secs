package secs1

// line_contention_test.go — TDD coverage for the SP5b T2 outer send discipline (secs1/line.go
// sendBlock): the RTY retry loop, the retryLimit+1 off-by-one boundary, and the slave-yield ACTION
// (EOT → receiveBlock → deliver → reset, per SEMI E4 §7.8.2.1). Each test drives a scripted raw-TCP
// peer over loopback with deadline-bounded reads so a broken send loop makes the peer goroutine EXIT
// on an idle timeout instead of blocking a read forever — no time.Sleep for synchronisation.

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// peerIdle bounds how long the scripted peer waits for our side's NEXT ENQ. The correct send loop
// emits it in microseconds on loopback; this margin only elapses when a teeth-check breaks the loop
// and our side stops sending (so the peer goroutine can end gracefully rather than hang).
const peerIdle = 500 * time.Millisecond

// --- Deadline-bounded peer helpers ---

// peerReadDeadline reads exactly n bytes from conn within peerIdle. It returns (bytes, true) on
// success or (nil, false) if the read did not complete in time — used to detect our side stopping
// early during a teeth-check without hanging the peer goroutine. It flags (not aborts) so it is
// goroutine-safe.
func peerReadDeadline(t *testing.T, conn net.Conn, n int) ([]byte, bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(peerIdle))
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, false
	}

	return buf, true
}

// peerGrant performs one master-grant handshake from the peer side: expect ENQ, reply EOT, read (and
// discard) the full outbound block (blockLen bytes), then reply with reply (ack/nak). It returns true
// on success, or false if no ENQ arrived within peerIdle (our side stopped sending — a graceful end
// of script, NOT flagged as an error).
func peerGrant(t *testing.T, conn net.Conn, blockLen int, reply byte) bool {
	t.Helper()
	first, ok := peerReadDeadline(t, conn, 1)
	if !ok {
		return false // our side stopped sending — end of script
	}
	if first[0] != enq {
		t.Errorf("peerGrant: expected ENQ (0x%02X), got 0x%02X", enq, first[0])
		return false
	}

	peerWrite(t, conn, []byte{eot})

	if _, ok := peerReadDeadline(t, conn, blockLen); !ok {
		t.Errorf("peerGrant: block not received within %s", peerIdle)
		return false
	}

	peerWrite(t, conn, []byte{reply})

	return true
}

// peerContend drives the peer's side of a slave contention: expect our ENQ, reply with a contending
// ENQ (instead of granting EOT), then expect the slave's EOT yield, transmit masterWire (the block
// the slave must deliver), and expect the slave's ACK. It returns true when the whole exchange
// completes, flagging any deviation.
func peerContend(t *testing.T, conn net.Conn, masterWire []byte) bool {
	t.Helper()
	first, ok := peerReadDeadline(t, conn, 1)
	if !ok || first[0] != enq {
		t.Errorf("peerContend: expected ENQ, got %v ok=%v", first, ok)
		return false
	}

	peerWrite(t, conn, []byte{enq}) // contend instead of granting EOT

	yield, ok := peerReadDeadline(t, conn, 1)
	if !ok || yield[0] != eot {
		t.Errorf("peerContend: expected EOT (slave yield), got %v ok=%v", yield, ok)
		return false
	}

	peerWrite(t, conn, masterWire) // the block the slave receives + delivers

	gotAck, ok := peerReadDeadline(t, conn, 1)
	if !ok || gotAck[0] != ack {
		t.Errorf("peerContend: expected ACK for master block, got %v ok=%v", gotAck, ok)
		return false
	}

	return true
}

// failDeliver is a deliver sink for the no-contention tests: the master never yields, so it must
// never be invoked.
func failDeliver(t *testing.T) func(block) error {
	t.Helper()

	return func(block) error {
		t.Errorf("deliver must not be called without a contention yield")
		return nil
	}
}

// --- sendBlock ---

// (a) The peer NAKs the first RetryLimit attempts then ACKs the (RetryLimit+1)th; sendBlock returns
// nil and the peer observes exactly RetryLimit+1 block transmissions.
func TestLineSendBlock_RetryThenACK(t *testing.T) {
	const retryLimit = 3
	cfg := newLineTestConfig(t, WithRetryLimit(retryLimit))
	line, peer := newLinePair(t, cfg)

	out := makeTestBlock(t, []byte("outbound"))
	outWire := out.appendTo(nil)

	blocksCh := make(chan int, 1)
	go func() {
		blocks := 0
		for i := 0; i <= retryLimit; i++ {
			reply := nak
			if i == retryLimit {
				reply = ack
			}
			if !peerGrant(t, peer, len(outWire), reply) {
				break
			}
			blocks++
		}
		blocksCh <- blocks
	}()

	err := line.sendBlock(context.Background(), out, retryLimit, failDeliver(t))
	require.NoError(t, err)
	require.Equal(t, retryLimit+1, <-blocksCh, "peer must observe exactly RetryLimit+1 block transmissions")
}

// (b) The peer NAKs every attempt; sendBlock surfaces ErrSendFailed after EXACTLY RetryLimit+1 block
// transmissions. This is the off-by-one guard on the loop bound (retry <= retryLimit).
func TestLineSendBlock_RetryExhaustion(t *testing.T) {
	const retryLimit = 3
	cfg := newLineTestConfig(t, WithRetryLimit(retryLimit))
	line, peer := newLinePair(t, cfg)

	out := makeTestBlock(t, []byte("outbound"))
	outWire := out.appendTo(nil)

	blocksCh := make(chan int, 1)
	go func() {
		blocks := 0
		for peerGrant(t, peer, len(outWire), nak) {
			blocks++
		}
		blocksCh <- blocks
	}()

	err := line.sendBlock(context.Background(), out, retryLimit, failDeliver(t))
	require.ErrorIs(t, err, ErrSendFailed)
	require.Equal(t, retryLimit+1, <-blocksCh, "exhaustion must occur after exactly RetryLimit+1 transmissions")
}

// (c) Slave contention (the P1 case): the peer NAKs twice (retry->2), then contends. The slave yields
// (EOT), takes the master's block, delivers it, and RESETS retry to 0; the peer then NAKs twice more
// (retry->2) and ACKs. The send ultimately succeeds AND the master's block reaches deliver.
//
// Teeth: without the reset the first post-contention NAK would push retry to 3 and exhaust, so a nil
// return proves the reset; the delivered-block equality proves the P1 delivery.
func TestLineSendBlock_SlaveContentionResetAndDeliver(t *testing.T) {
	const retryLimit = 2
	cfg := newLineTestConfig(t, WithRetryLimit(retryLimit)) // default role: host (slave)
	require.False(t, cfg.IsEquip())
	line, peer := newLinePair(t, cfg)

	out := makeTestBlock(t, []byte("slave-out"))
	outWire := out.appendTo(nil)

	master := makeTestBlock(t, []byte("MASTER-BLOCK"))
	masterWire := master.appendTo(nil)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		// NAK, NAK (retry->2).
		if !peerGrant(t, peer, len(outWire), nak) {
			return
		}
		if !peerGrant(t, peer, len(outWire), nak) {
			return
		}
		// Contention yield: deliver the master block, reset->0.
		if !peerContend(t, peer, masterWire) {
			return
		}
		// NAK, NAK (retry->2) — would exhaust WITHOUT the reset.
		if !peerGrant(t, peer, len(outWire), nak) {
			return
		}
		if !peerGrant(t, peer, len(outWire), nak) {
			return
		}
		// ACK -> success.
		_ = peerGrant(t, peer, len(outWire), ack)
	}()

	var delivered []block
	err := line.sendBlock(context.Background(), out, retryLimit, func(b block) error {
		delivered = append(delivered, b)
		return nil
	})
	<-doneCh

	require.NoError(t, err, "the retry reset after a contention yield must let the send ultimately ACK")
	require.Len(t, delivered, 1, "the master's contention block must be delivered exactly once (P1)")
	require.Equal(t, masterWire, delivered[0].appendTo(nil), "the delivered block must be the master's block, not our own")
	require.Equal(t, uint64(1), line.metrics.ContentionYieldCount(),
		"the single slave-contention yield must be counted exactly once")
}

// TestLineSendBlock_ContentionYieldDeliverErrorNonFatal proves a deliver error during a slave
// contention yield is NON-FATAL: delivering the master's block may fail (an inbound decode/route
// error), but that must not fail the OUTBOUND send — the postponed send restarts and completes
// normally, mirroring the idle inbound path's `_ = sink(blk)`. Otherwise a routine inbound decode
// failure would drive a spurious line teardown.
//
// Teeth: restore `if derr := deliver(recv); derr != nil { return derr }` in sendBlock and this fails —
// sendBlock returns the deliver error instead of the eventual ACK's nil.
func TestLineSendBlock_ContentionYieldDeliverErrorNonFatal(t *testing.T) {
	const retryLimit = 2
	cfg := newLineTestConfig(t, WithRetryLimit(retryLimit)) // default role: host (slave)
	require.False(t, cfg.IsEquip())
	line, peer := newLinePair(t, cfg)

	out := makeTestBlock(t, []byte("slave-out"))
	outWire := out.appendTo(nil)
	master := makeTestBlock(t, []byte("MASTER-BLOCK"))
	masterWire := master.appendTo(nil)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		// Contention yield: the peer contends and hands us its block (our deliver callback errors).
		if !peerContend(t, peer, masterWire) {
			return
		}
		// The postponed send restarts; ACK it → success.
		_ = peerGrant(t, peer, len(outWire), ack)
	}()

	err := line.sendBlock(context.Background(), out, retryLimit, func(block) error {
		return errors.New("simulated inbound deliver failure")
	})
	<-doneCh

	require.NoError(t, err,
		"a deliver error during a contention yield must be non-fatal — the send must still complete")
}

// (d) Master contention: with IsEquip(true) a peer ENQ before EOT is ignored and the send completes
// normally in a single attempt — the master never yields, so deliver is never called.
func TestLineSendBlock_MasterIgnoresContention(t *testing.T) {
	cfg := newLineTestConfig(t, WithEquipment()) // equipment == master
	require.True(t, cfg.IsEquip())
	line, peer := newLinePair(t, cfg)

	out := makeTestBlock(t, []byte("master-out"))
	outWire := out.appendTo(nil)

	blkCh := make(chan []byte, 1)
	go func() {
		first, ok := peerReadDeadline(t, peer, 1)
		if !ok || first[0] != enq {
			t.Errorf("master: expected ENQ, got %v ok=%v", first, ok)
			blkCh <- nil
			return
		}
		peerWrite(t, peer, []byte{enq}) // contend — master must IGNORE
		peerWrite(t, peer, []byte{eot}) // then grant with EOT
		blk, ok := peerReadDeadline(t, peer, len(outWire))
		if !ok {
			t.Errorf("master: block not received")
			blkCh <- nil
			return
		}
		peerWrite(t, peer, []byte{ack})
		blkCh <- blk
	}()

	var delivered []block
	err := line.sendBlock(context.Background(), out, cfg.RetryLimit(), func(b block) error {
		delivered = append(delivered, b)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, delivered, "a master never yields, so deliver is never called")
	require.Equal(t, outWire, <-blkCh, "master ignores the contending ENQ and transmits its block")
}
