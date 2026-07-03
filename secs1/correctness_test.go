package secs1

// correctness_test.go — SP5b T7 correctness suite. Fills the three v1-parity GAPS the prior
// coverage audit found were NOT yet exercised with teeth, plus documents one teeth-check outcome:
//
//   - Task 1 (TestSECS1_D5b10_RetryExhaustionReconnects): the headline emergent D5b-10 contract —
//     an RTY-exhausted line send surfaces an error from the transport Write, the CORE turns that
//     Write error into a line failure (TCPDown → teardown), and the reconnect loop re-dials. Driven
//     end-to-end against a raw misbehaving TCP peer through the PUBLIC secs1 API.
//   - Task 2 (TestAssembler_UnexpectedBlockNumber*): direct unit coverage of the assembler's
//     accept() "unexpected block → abort the open partial, re-evaluate as a fresh first" branch
//     (assembler.go step 4 else) — the one accept() branch no existing test hits directly.
//   - Task 3 (TestSECS1_O3_ReceiveGoldenReplay): a raw host peer replays a hand-authored 2-block
//     SECS-I wire trace into a PASSIVE secs1 endpoint; the decoded message must match a golden S/F/body.
//
// No time.Sleep for synchronization — require.Eventually / bounded channel receives only. Scope: the
// public secs1 API (Task 1/3) and the white-box assembler (Task 2).

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Task 1 — D5b-10: RTY-exhaustion → core TCPDown → reconnect
// ---------------------------------------------------------------------------

// TestSECS1_D5b10_RetryExhaustionReconnects validates the emergent D5b-10 contract end-to-end over
// the PUBLIC secs1 API against a raw misbehaving TCP peer:
//
//	secs1.Write runs the line engine; when the peer never grants the line (no EOT) the send
//	RTY-exhausts and Write returns an error. The core's writeFrame treats ANY tr.Write error as a
//	line failure → c.TCPDown(err) → teardown → the reconnect loop re-dials. The single misbehaving
//	handler serves BOTH the first connection (forces the send to fail) AND the reconnect (secs1
//	auto-commits Selected on TCP connect — no handshake), so the active re-reaches Selected purely
//	on TCP re-connect.
//
// The observable state timeline is Selected → not-Selected (the core drove TCPDown) → Selected
// (the re-dial re-established the line), with the accept counter reaching >= 2.
//
// The SendDataMessage error is deliberately NOT asserted against a specific sentinel: the line
// RTY-exhaustion (ErrSendFailed from Write) races the core's concurrent TCPDown that cancels the
// epoch (which can surface ErrConnClosed first). Both are valid outcomes of the same failure — only
// "an error, and a nil reply" is asserted (the same don't-over-assert-a-raced-outcome discipline the
// assembler T4-gap test uses).
func TestSECS1_D5b10_RetryExhaustionReconnects(t *testing.T) {
	t.Parallel()

	const ms = time.Millisecond

	// (1) Raw listener on an ephemeral loopback port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := tcpAddr.Port

	// (2) Accept-loop with an accept counter. Each accepted conn gets a MISBEHAVING handler: it never
	// sends EOT/ACK; it only drains until the active closes it (teardown), then closes its end. The
	// SAME handler serves the first connection (forcing the send to RTY-exhaust) and the reconnect.
	var acceptCount atomic.Int32
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return // listener closed by cleanup — exit the accept loop cleanly
			}
			acceptCount.Add(1)
			go func(c net.Conn) {
				// Drain (and discard) everything the active sends — including its ENQs — but NEVER
				// grant the line with EOT. io.Copy returns once the active closes the socket on teardown.
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()

	// (3) Active via the PUBLIC API. Short T2 + RetryLimit(1) makes the send RTY-exhaust in ~2×T2;
	// short T5 makes the reconnect prompt.
	cfg, err := NewConfig("127.0.0.1", port,
		WithActive(),
		WithHost(),
		WithT1(100*ms),
		WithT2(200*ms),
		WithT4(500*ms),
		WithRetryLimit(1),
		WithConnectionOption(hsms.WithT5(200*ms)),
	)
	require.NoError(t, err)

	active, err := New(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, active.Close()) }()

	// (4) Open in the background; the active auto-commits Selected the instant the TCP dial connects.
	require.NoError(t, active.Open(t.Context(), hsms.OpenBackground))
	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState
	}, 10*time.Second, 5*ms, "active must reach Selected on TCP connect (auto-committed, no handshake)")

	// (5) Send a W-bit data message. The peer never grants the line, so the line RTY-exhausts and the
	// send fails. Do NOT assert a specific sentinel (see the function doc — the failure races TCPDown).
	reply, err := active.SendDataMessage(t.Context(), 1, 1, true, secs2.A("x"))
	require.Error(t, err, "an RTY-exhausted send with no line grant must fail")
	require.Nil(t, reply, "a failed send returns no reply")

	// (6) TEARDOWN: the core turned the Write error into a line failure and drove TCPDown — the link
	// must leave Selected.
	require.Eventually(t, func() bool {
		return active.State() != hsms.SelectedState
	}, 5*time.Second, 2*ms, "the core must drive TCPDown on the Write failure (link leaves Selected)")

	// (7) RECONNECT: the reconnect loop re-dials, producing a SECOND accept, and the active re-reaches
	// Selected purely on TCP re-connect. Observing not-Selected first (step 6) disambiguates this from
	// the initial Selected.
	require.Eventually(t, func() bool {
		return active.State() == hsms.SelectedState
	}, 10*time.Second, 5*ms, "the active must reconnect and re-reach Selected after the teardown")
	require.GreaterOrEqual(t, acceptCount.Load(), int32(2),
		"the re-dial must produce a second accept on the raw peer")
}

// ---------------------------------------------------------------------------
// Task 2 — assembler unexpected-block-number abort + restart (step 4 else branch)
// ---------------------------------------------------------------------------

// TestAssembler_UnexpectedBlockNumberRestartsMessage drives the accept() "not the expected block →
// abort the open partial, re-evaluate this block as a fresh first" branch (assembler.go step 4 else),
// which no existing test hits directly (the T4-gap test resets via T4, the duplicate test resets via
// the expected-number mismatch of an already-appended block).
//
// IMPORTANT construction note (WHY the restart uses DIFFERENT system bytes): the duplicate-block guard
// (accept step 3) keys on the FULL 10-byte header, and buildHeader is a pure function of
// (messageHeader, blockNumber, last). So a second block-1 built from the SAME messageHeader would be
// byte-identical to the first and swallowed as a retransmit (step 3) BEFORE ever reaching step 4 —
// delivering "firstsecond", not exercising the abort-restart at all. To reach step 4 the restart must
// be a genuinely NEW message: identical direction/stream/function but distinct system bytes (a new
// transaction). That is exactly the real-world trigger for this branch — a fresh message's block 1
// arriving while a stale partial is still open (before T4).
//
// Sequence (all to-host so the host assembler accepts them):
//  1. block 1 "first"   (message A) → opens a partial.
//  2. block 1 "RESTART" (message B, new system bytes) → num 1 != expected 2, and NOT a duplicate
//     (different header) → the open partial A ABORTS, then this block re-evaluates as a valid fresh
//     first → a new partial B opens. Still open, nothing delivered.
//  3. block 2 "second"  (message B) → completes B → one frame delivered.
//
// The delivered body must be "RESTARTsecond" (NOT "firstsecond"): proof the original block-1 "first"
// was discarded by the restart and the message was rebuilt from message B's blocks.
func TestAssembler_UnexpectedBlockNumberRestartsMessage(t *testing.T) {
	cfg := newLineTestConfig(t) // host role (isEquip=false) → accepts to-host (rBit=true) blocks
	a, frames := newCaptureAssembler(t, cfg)

	msgA := asmHeader(true) // to host, DEADBEEF system bytes
	msgB := asmHeader(true)
	msgB.systemBytes = [4]byte{0x11, 0x22, 0x33, 0x44} // a DIFFERENT transaction (see the WHY note above)

	// (1) block 1 of message A opens a partial.
	require.NoError(t, a.accept(asmBlock(msgA, 1, false, []byte("first"))))
	require.True(t, a.open, "block 1 must open a partial message")

	// (2) block 1 of message B: num 1 != expected 2, not a duplicate → abort A, restart as B.
	require.NoError(t, a.accept(asmBlock(msgB, 1, false, []byte("RESTART"))))
	require.True(t, a.open, "the restarting first block must leave a fresh partial open")
	require.Empty(t, *frames, "no frame may be delivered until the restarted message terminates")

	// (3) block 2 of message B terminates the message.
	require.NoError(t, a.accept(asmBlock(msgB, 2, true, []byte("second"))))

	require.Len(t, *frames, 1, "the completed restarted message must deliver exactly one frame")
	require.Equal(t, []byte("RESTARTsecond"), (*frames)[0][10:],
		`the aborted block-1 "first" must be discarded — the frame is rebuilt from message B's blocks`)
	// The delivered frame's system bytes must be message B's (bytes 6-9 of the synthesized HSMS header),
	// corroborating that the restart adopted B's header, not A's.
	require.Equal(t, []byte{0x11, 0x22, 0x33, 0x44}, (*frames)[0][6:10],
		"the restarted message must carry message B's system bytes")
}

// TestAssembler_UnexpectedBlockNumberSkipDropped covers the sibling case where the aborting block is
// ALSO not a valid fresh first: block 1 (no E) then block 3 (skipping block 2, E-bit). Block 3 is
// neither the expected block (2) nor a valid first (num 3 — not 1, not a lone 0+E), so after aborting
// the open partial it is dropped. Nothing is delivered and no partial remains open.
func TestAssembler_UnexpectedBlockNumberSkipDropped(t *testing.T) {
	cfg := newLineTestConfig(t)
	a, frames := newCaptureAssembler(t, cfg)

	mh := asmHeader(true)
	require.NoError(t, a.accept(asmBlock(mh, 1, false, []byte("first"))))
	require.True(t, a.open, "block 1 must open a partial message")

	// block 3 (skips 2, E-bit): differs from block 1's header (block number) so it is NOT a duplicate;
	// num 3 != expected 2 → abort; num 3 is not a valid first → dropped.
	require.NoError(t, a.accept(asmBlock(mh, 3, true, []byte("orphan"))))

	require.Empty(t, *frames, "a skipped-then-orphan block must deliver nothing")
	require.False(t, a.open, "the aborted partial and the orphan block must leave no open message")
}

// ---------------------------------------------------------------------------
// Task 3 — O3: receive-direction golden replay
// ---------------------------------------------------------------------------

// TestSECS1_O3_ReceiveGoldenReplay replays a hand-authored 2-block SECS-I wire trace from a raw host
// peer into a PASSIVE secs1 (equipment) endpoint and asserts the delivered, decoded message matches a
// golden S/F/body. It exercises the full receive direction over the live engine: idle-ENQ detect →
// EOT grant → receiveBlock (length/T1/T2/checksum + ACK) → assembler → DeliverOwnedFrame → handler.
//
// Role wiring (SEMI E4 §8.2): passive == equipment (IsEquip=true), accepts to-equipment (rBit=false)
// blocks from a host peer. The peer drives the half-duplex script:
//
//	ENQ → (await EOT) → block1 → (await ACK) → ENQ → (await EOT) → block2(E) → (await ACK).
//
// The peer's socket carries an overall deadline so a misbehaving passive can never wedge the peer
// goroutine past the test (which would risk a log-after-test panic); on the happy path every ACK
// arrives in milliseconds, well inside it.
func TestSECS1_O3_ReceiveGoldenReplay(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	ctx := t.Context()

	// Capture sink: the passive's data handler forwards the received primary to a cap-1 channel.
	recvCh := make(chan *hsms.DataMessage, 1)
	handler := func(msg *hsms.DataMessage, _ hsms.SECS2Endpoint) {
		select {
		case recvCh <- msg:
		default:
		}
	}

	passive := newSECS1Passive(t, port, handler)
	defer closeSECS1Endpoint(t, passive)
	require.NoError(t, passive.conn.Open(ctx, hsms.OpenBackground))

	// Golden 2-block message, to-equipment (rBit=false) so the equipment assembler accepts it. The
	// block bodies concatenate to the golden message body; asmHeader pins S6F17 (W-bit) as the golden
	// stream/function.
	mh := asmHeader(false)
	block1Wire := asmBlock(mh, 1, false, []byte("golden-")).appendTo(nil)
	block2Wire := asmBlock(mh, 2, true, []byte("payload")).appendTo(nil)

	// Raw host peer dials the passive listener (retry until the listener is bound — Open completes
	// ListenTCP synchronously, so a single attempt normally suffices).
	var peer net.Conn
	require.Eventually(t, func() bool {
		c, derr := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if derr != nil {
			return false
		}
		peer = c

		return true
	}, 5*time.Second, 10*time.Millisecond, "raw peer must connect to the passive listener")
	t.Cleanup(func() { _ = peer.Close() })

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		// Bound every peer read/write so a stalled passive cannot hang this goroutine past the test.
		_ = peer.SetDeadline(time.Now().Add(4 * time.Second))

		expect := func(label string, want byte) bool {
			got := peerReadN(t, peer, 1)
			if len(got) != 1 || got[0] != want {
				t.Errorf("%s: expected 0x%02X, got %v", label, want, got)

				return false
			}

			return true
		}

		// Block 1: ENQ → EOT → block1 → ACK.
		peerWrite(t, peer, []byte{enq})
		if !expect("block1 EOT", eot) {
			return
		}
		peerWrite(t, peer, block1Wire)
		if !expect("block1 ACK", ack) {
			return
		}
		// Block 2 (E-bit): ENQ → EOT → block2 → ACK.
		peerWrite(t, peer, []byte{enq})
		if !expect("block2 EOT", eot) {
			return
		}
		peerWrite(t, peer, block2Wire)
		_ = expect("block2 ACK", ack)
	}()

	select {
	case msg := <-recvCh:
		require.Equal(t, uint8(6), msg.Stream(), "golden stream must survive the receive path")
		require.Equal(t, uint8(0x11), msg.Function(), "golden function must survive the receive path")
		require.True(t, msg.WaitBit(), "golden W-bit must survive the receive path")
		require.Equal(t, []byte("golden-payload"), msg.AppendBodyTo(nil),
			"the ordered 2-block body must reassemble to the golden payload")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the replayed 2-block message to be delivered")
	}
	<-scriptDone // the block-2 ACK precedes delivery, so the script has finished by now
}
