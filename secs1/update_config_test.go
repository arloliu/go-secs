package secs1

// update_config_test.go — rc4 Gap 1 behavioral proof. TestUpdateConfigOptions_SessionIDStaysDeviceID
// (new_test.go) and TestUpdateConfigOptions_WriteTimeoutStaysZero_DoesNotBreakOtherInvariants
// (new_test.go) only prove the live update composes without error; neither can observe the core's
// live writeTimeout value directly (no public accessor). This file proves the actual bug via its
// real consequence: a write timeout wrongly re-armed on the wire socket would truncate an
// in-progress SECS-I line transaction with a legitimately slow (but well within T2) peer.

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// TestUpdateConfigOptions_WriteTimeoutDoesNotArmDeadline proves Gap 1 (rc4): after
// UpdateConfigOptions(hsms.WithWriteTimeout(shortDeadline)), a send to a peer that replies
// slower than shortDeadline (but well within T2) must still succeed — because writeTimeout
// must stay pinned at 0 for SECS-I, never armed on the wire socket. Before the fix, this send
// would fail (write deadline exceeded mid-transaction) instead.
func TestUpdateConfigOptions_WriteTimeoutDoesNotArmDeadline(t *testing.T) {
	const (
		shortWriteTimeout = 50 * time.Millisecond  // what the (would-be) bug re-arms
		peerDelay         = 300 * time.Millisecond // peer replies slower than shortWriteTimeout...
		t2                = 5 * time.Second        // ...but comfortably within T2, so only a core
	) // write-deadline bug (not a protocol timeout) could fail this

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)

	peerCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			peerCh <- c
		}
	}()

	cfg, err := NewConfig("127.0.0.1", tcpAddr.Port, WithT2(t2), WithDeviceID(0x0001))
	require.NoError(t, err)
	conn, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.Open(t.Context(), hsms.OpenWaitSelected)) // secs1 auto-selects on TCP connect

	var peer net.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not connect")
	}
	t.Cleanup(func() { _ = peer.Close() })

	require.NoError(t, conn.UpdateConfigOptions(hsms.WithWriteTimeout(shortWriteTimeout)))

	// Drive the peer side of a single-block SECS-I send with an artificial delay before its
	// EOT/ACK responses: read the ENQ, sleep peerDelay, grant EOT, read the block, ACK it. Modeled
	// on write_test.go's TestWrite_SingleBlockDataSend / readWireBlock helper (peerReadN, block
	// length+checksum framing).
	//
	// replyExpected is deliberately false (not the brief's literal true): per
	// hsms.(*connection).sendWaitReply, a W-bit send only short-circuits (returns as soon as the
	// frame is on the wire) when the data message is fire-and-forget (W-bit clear); a W-bit-set
	// send instead arms a T3-bounded reply wait AFTER the wire write completes, which our fake peer
	// never satisfies (it only ACKs the block, it never sends a reply message) — that would hang
	// the send until T3 (default 45s) regardless of the writeTimeout bug under test, defeating the
	// test's own bounded wait below. false isolates exactly what this test needs to observe: does
	// the wire write (ENQ/EOT/block/ACK) itself survive a slow-but-within-T2 peer.
	sendErrCh := make(chan error, 1)
	go func() {
		_, sendErr := conn.SendDataMessage(t.Context(), 1, 1, false, secs2.NewASCIIItem("PING"))
		sendErrCh <- sendErr
	}()

	first := peerReadN(t, peer, 1) // ENQ
	if len(first) == 1 && first[0] != enq {
		t.Errorf("engine send must open with ENQ, got 0x%02X", first[0])
	}
	time.Sleep(peerDelay) // deliberately scripted delay: the peer replies slower than shortWriteTimeout
	peerWrite(t, peer, []byte{eot})

	// Read the block WITHOUT write_test.go's readWireBlock: under the bug (write timeout re-armed),
	// the local side's block write fails and tears the connection down, so this read legitimately
	// sees EOF/short-read instead of a well-formed block — readWireBlock's require.NoError would
	// panic on that (t.Errorf + a still-executed index into an empty slice). Fail soft here so the
	// real, meaningful assertion is the sendErr check below, not a crash in the peer harness.
	lenByte := make([]byte, 1)
	if _, rerr := io.ReadFull(peer, lenByte); rerr != nil {
		t.Logf("peer: block length byte not received (expected if the write-timeout bug truncated the send): %v", rerr)
	} else {
		rest := peerReadN(t, peer, int(lenByte[0])+checksumSize)
		if len(rest) == int(lenByte[0])+checksumSize {
			if _, perr := parseBlock(lenByte[0], rest); perr == nil {
				peerWrite(t, peer, []byte{ack})
			}
		}
	}

	select {
	case sendErr := <-sendErrCh:
		require.NoError(t, sendErr, "a correctly-inert writeTimeout must not fail a send slower than the (bugged) short deadline")
	case <-time.After(t2):
		t.Fatal("send did not complete within T2")
	}
}
