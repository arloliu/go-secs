package secs1

// assembler_test.go — TDD coverage for the SP5b T5 inbound multi-block assembler (secs1/assembler.go):
// the SEMI E4 §9.4 message-receive algorithm that reassembles ordered blocks into a synthesized HSMS
// frame and delivers it via rt.DeliverOwnedFrame. Blocks are fed directly to newAssembler(...).accept
// (single-goroutine, no wire I/O), the injectable clock drives T4, and a capture sink records the
// delivered frames. No time.Sleep.

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/internal/wire"
	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/stretchr/testify/require"
)

// asmHeader builds a non-trivial block-invariant header for the assembler tests. rBit selects the
// message direction (E4 §8.2: false = to equipment, true = to host).
func asmHeader(rBit bool) messageHeader {
	return messageHeader{
		deviceID:    0x1234,
		rBit:        rBit,
		stream:      6,
		function:    0x11,
		waitBit:     true,
		systemBytes: [4]byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
}

// asmBlock builds a received block with the given header, block number, last-block flag, and body.
// The body is copied so the block owns an independent buffer (mirroring receiveBlock's fresh buffer).
func asmBlock(mh messageHeader, num uint16, last bool, body []byte) block {
	var chunk wire.Chunk
	if len(body) > 0 {
		chunk = wire.ChunkOf(append([]byte(nil), body...))
	}

	return block{header: buildHeader(mh, num, last), body: chunk}
}

// newCaptureAssembler builds an assembler whose deliver sink records every delivered frame. The
// returned *[][]byte grows one entry per completed message.
func newCaptureAssembler(t *testing.T, cfg Config) (*assembler, *[][]byte) {
	t.Helper()
	frames := &[][]byte{}
	a := newAssembler(cfg, func(f []byte) error {
		*frames = append(*frames, append([]byte(nil), f...))

		return nil
	}, cfg.Timers)

	return a, frames
}

// --- (a) two-block message → one frame ---

// TestAssembler_TwoBlockMessageDeliversOneFrame feeds a well-formed 2-block message (block 1 no-E,
// block 2 E) directed to us and asserts exactly ONE synthesized HSMS frame is delivered, decodes it,
// and confirms stream / function / W-bit / system bytes and the concatenated body.
func TestAssembler_TwoBlockMessageDeliversOneFrame(t *testing.T) {
	cfg := newLineTestConfig(t) // host role (isEquip=false) → accepts to-host (rBit=true) blocks
	a, frames := newCaptureAssembler(t, cfg)

	mh := asmHeader(true) // to host
	require.NoError(t, a.accept(asmBlock(mh, 1, false, []byte("hello "))))
	require.NoError(t, a.accept(asmBlock(mh, 2, true, []byte("world"))))

	require.Len(t, *frames, 1, "a complete 2-block message must deliver exactly one frame")

	frame := (*frames)[0]
	// Raw HSMS header byte-map (assembleFrame): bytes0-1 device ID (big-endian), byte2 W|stream,
	// byte3 function, bytes4-5 PType/SType 0, bytes6-9 system bytes.
	require.Equal(t, byte(0x12), frame[0], "bytes 0-1 must be the received device ID (big-endian)")
	require.Equal(t, byte(0x34), frame[1], "bytes 0-1 must be the received device ID (big-endian)")
	require.Equal(t, byte(0x80|6), frame[2], "byte2 must be (W-bit 0x80)|stream")
	require.Equal(t, byte(0x11), frame[3], "byte3 must be the function")
	require.Equal(t, byte(0), frame[4], "byte4 must be PType 0")
	require.Equal(t, byte(0), frame[5], "byte5 must be SType 0 (data)")
	require.Equal(t, []byte{0xDE, 0xAD, 0xBE, 0xEF}, frame[6:10], "bytes6-9 must be the system bytes")
	require.Equal(t, []byte("hello world"), frame[10:], "body must be the ordered block concatenation")

	// Decode the frame through the core HSMS decoder (proves it is a valid, routable data frame).
	msg, err := hsms.DecodeHSMSMessage(withHSMSLengthPrefix(frame))
	require.NoError(t, err)
	data, ok := msg.(*hsms.DataMessage)
	require.True(t, ok, "the synthesized frame must decode to a DataMessage")
	require.Equal(t, uint16(0x1234), data.SessionID(), "inbound SessionID() must report the received device ID")
	require.Equal(t, uint8(6), data.Stream())
	require.Equal(t, uint8(0x11), data.Function())
	require.True(t, data.WaitBit())
	require.Equal(t, [4]byte{0xDE, 0xAD, 0xBE, 0xEF}, data.SystemBytes())
	require.Equal(t, []byte("hello world"), data.AppendBodyTo(nil))
}

// TestSECS1_SessionIDIsDeviceID is the SECS-I identity round-trip: with a device ID that is neither 0
// nor the 0xFFFF core sentinel, BOTH an outbound message's SessionID() and an INBOUND (peer-sent,
// reassembled) message's SessionID() must report that device ID. Before the fix, secs1 seeded the
// core session ID with the 0xFFFF sentinel and stamped inbound frames with 0xFFFF, so both assertions
// have teeth.
func TestSECS1_SessionIDIsDeviceID(t *testing.T) {
	const devID uint16 = 0x1234

	cfg, err := NewConfig("127.0.0.1", 5000, WithDeviceID(devID))
	require.NoError(t, err)

	// (a) The device ID seeds the core session identity, so an OUTBOUND message built with the
	// connection's session ID (as the session's send path does) reports the device ID.
	require.Equal(t, devID, cfg.SessionID(), "core session ID must equal the configured device ID")
	out, err := hsms.NewDataMessage(1, 1, true, cfg.SessionID(), [4]byte{0, 0, 0, 1}, secs2.A("ping"))
	require.NoError(t, err)
	require.Equal(t, devID, out.SessionID(), "outbound msg.SessionID() must report the device ID")

	// (b) An INBOUND message reassembled from received SECS-I blocks carrying devID must also report
	// it. Feed a block through the real assembler, then decode the synthesized HSMS frame.
	a, frames := newCaptureAssembler(t, newLineTestConfig(t)) // host role → accepts to-host (rBit=true)
	mh := messageHeader{deviceID: devID, rBit: true, stream: 1, function: 1, waitBit: true, systemBytes: [4]byte{0, 0, 0, 2}}
	require.NoError(t, a.accept(asmBlock(mh, 1, true, []byte("body"))))
	require.Len(t, *frames, 1)

	msg, err := hsms.DecodeHSMSMessage(withHSMSLengthPrefix((*frames)[0]))
	require.NoError(t, err)
	in, ok := msg.(*hsms.DataMessage)
	require.True(t, ok, "the synthesized frame must decode to a DataMessage")
	require.Equal(t, devID, in.SessionID(), "inbound msg.SessionID() must report the wire device ID (was 0xFFFF before the fix)")
}

// --- (b) single-block-0 ---

// TestAssembler_SingleBlockZeroDelivered proves the D5b-12 leniency: a lone block numbered 0 with the
// E-bit is accepted, assembled, and delivered.
//
// Teeth-check: require the first block to be number 1 (reject 0) in beginMessage and this fails — the
// block is dropped and no frame is delivered.
func TestAssembler_SingleBlockZeroDelivered(t *testing.T) {
	cfg := newLineTestConfig(t)
	a, frames := newCaptureAssembler(t, cfg)

	mh := asmHeader(true)
	require.NoError(t, a.accept(asmBlock(mh, 0, true, []byte("solo"))))

	require.Len(t, *frames, 1, "a lone single-block-0 (E-bit) must be delivered (D5b-12 leniency)")
	require.Equal(t, []byte("solo"), (*frames)[0][10:])
}

// --- (c) T4 inter-block gap ---

// TestAssembler_T4InterBlockGapDiscardsPartial feeds block 1 (no E), advances the injected clock past
// T4, then feeds block 2. The stale partial is discarded on block 2's arrival; block 2 alone
// (numbered 2) is not a valid first block, so NOTHING is delivered.
func TestAssembler_T4InterBlockGapDiscardsPartial(t *testing.T) {
	cfg := newLineTestConfig(t)
	a, frames := newCaptureAssembler(t, cfg)

	fakeNow := time.Unix(0, 0)
	a.now = func() time.Time { return fakeNow }

	mh := asmHeader(true)
	require.NoError(t, a.accept(asmBlock(mh, 1, false, []byte("part1"))))
	require.True(t, a.open, "block 1 must open a partial message")

	// Advance past T4 so the next block's arrival trips the lazy inter-block deadline.
	fakeNow = fakeNow.Add(cfg.T4() + time.Second)

	require.NoError(t, a.accept(asmBlock(mh, 2, true, []byte("part2"))))
	require.Empty(t, *frames, "a T4 inter-block gap must discard the partial — no frame delivered")
	require.False(t, a.open, "the stale partial (and the orphaned block 2) must leave no open message")
}

// TestAssembler_T4LiveUpdate proves accept's T4 discard check reads T4 live: a gap of 200ms doesn't
// trip the ORIGINAL 5s T4 but DOES trip a live-updated 100ms T4 — this only passes if the mutation to
// `current` (not a value snapshotted at construction) is actually what accept reads.
func TestAssembler_T4LiveUpdate(t *testing.T) {
	cfg := newLineTestConfig(t)

	current := hsms.TimerConfig{T4: 5 * time.Second}
	timers := func() hsms.TimerConfig { return current }

	frames := &[][]byte{}
	a := newAssembler(cfg, func(f []byte) error {
		*frames = append(*frames, append([]byte(nil), f...))

		return nil
	}, timers) // 3-arg Task-3 signature — Task 9 has not landed yet at this point in the task order

	fakeNow := time.Unix(0, 0)
	a.now = func() time.Time { return fakeNow }

	mh := asmHeader(true)
	require.NoError(t, a.accept(asmBlock(mh, 1, false, []byte("part1"))))
	require.True(t, a.open, "block 1 must open a partial message")

	// Shrink T4 live, then advance the clock by a gap that is ABOVE the new 100ms T4 but WELL BELOW
	// the original 5s T4.
	current.T4 = 100 * time.Millisecond
	fakeNow = fakeNow.Add(200 * time.Millisecond)

	require.NoError(t, a.accept(asmBlock(mh, 2, true, []byte("part2"))))
	require.Empty(t, *frames, "a 200ms gap must trip the LIVE 100ms T4, not the stale 5s value")
	require.False(t, a.open, "the stale partial must be discarded under the live T4")
}

// --- (d) duplicate block ---

// TestAssembler_DuplicateMiddleBlockAssemblesOnce feeds a 3-block message with the MIDDLE block
// retransmitted (peer missed our ACK — identical full header). The duplicate is discarded, not
// re-appended, so the message assembles exactly ONCE with the correct (single-copy) body.
//
// Teeth-check: remove the duplicate-block guard and this fails — the duplicate block 2 (number 2)
// no longer matches the expected number 3, so the partial is aborted and NO frame is ever delivered.
func TestAssembler_DuplicateMiddleBlockAssemblesOnce(t *testing.T) {
	cfg := newLineTestConfig(t)
	a, frames := newCaptureAssembler(t, cfg)

	mh := asmHeader(true)
	require.NoError(t, a.accept(asmBlock(mh, 1, false, []byte("A"))))
	require.NoError(t, a.accept(asmBlock(mh, 2, false, []byte("B"))))
	require.NoError(t, a.accept(asmBlock(mh, 2, false, []byte("B")))) // duplicate of block 2
	require.NoError(t, a.accept(asmBlock(mh, 3, true, []byte("C"))))

	require.Len(t, *frames, 1, "a duplicate middle block must not break assembly — exactly one frame")
	require.Equal(t, []byte("ABC"), (*frames)[0][10:], "the duplicate block must not be double-appended")
}

// --- (e) wrong-direction R-bit → dropped ---

// TestAssembler_WrongDirectionBlockDropped proves the SP5b §6f / E4 §8.2 direction guard (the P1
// fix): a host assembler fed a to-equipment block (rBit=false) drops it — not accumulated, not
// delivered, no error (the link stays up).
//
// Teeth-check: remove the R-bit direction guard and this fails — the block is a valid single-block
// message and IS delivered.
func TestAssembler_WrongDirectionBlockDropped(t *testing.T) {
	cfg := newLineTestConfig(t) // host role (isEquip=false) → must accept only to-host (rBit=true)
	require.False(t, cfg.IsEquip())
	a, frames := newCaptureAssembler(t, cfg)

	wrong := asmHeader(false) // to equipment — the WRONG direction for a host
	err := a.accept(asmBlock(wrong, 1, true, []byte("nope")))

	require.NoError(t, err, "a wrong-direction block is a protocol violation, not a link teardown")
	require.Empty(t, *frames, "a wrong-direction block must NOT be delivered")
	require.False(t, a.open, "a wrong-direction block must NOT open a partial message")
}

// withHSMSLengthPrefix prepends the 4-byte big-endian HSMS length prefix to a synthesized frame so it
// can be fed to hsms.DecodeHSMSMessage (which expects [length][header||body]).
func withHSMSLengthPrefix(frame []byte) []byte {
	out := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(frame)))
	copy(out[4:], frame)

	return out
}
