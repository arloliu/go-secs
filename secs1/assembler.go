package secs1

import (
	"fmt"
	"time"

	"github.com/arloliu/go-secs/v2/hsms"
	"github.com/arloliu/go-secs/v2/logger"
)

// assembler is the per-generation inbound multi-block accumulator (SEMI E4 §9.4 / SP5b §6e). It is
// fed every checksum-valid, already-ACK'd block the line engine receives — both an inbound-ENQ
// receive and a block taken during a slave-contention yield — and reassembles the ordered blocks of
// one message into a synthesized HSMS frame, which it delivers to the core via deliverFrame
// (rt.DeliverOwnedFrame).
//
// A FRESH assembler is built per connection generation (see transport.newSink) so a straggler
// abandoned by a bounded Stop never shares accumulation state with a successor generation. It is
// driven exclusively from the SINGLE line-engine goroutine (the G-A invariant), so it holds no locks.
type assembler struct {
	isEquip      bool                             // our role, for inbound R-bit direction validation (SP5b §6f / E4 §8.2)
	deviceID     uint16                           // our configured device ID, for inbound routing validation (E4 §9.4.1)
	deliverFrame func([]byte) error               // = rt.DeliverOwnedFrame; takes ownership of the delivered frame buffer
	now          func() time.Time                 // injectable clock for the T4 inter-block deadline (default time.Now)
	timers       func() hsms.TimerConfig          // LIVE T4 source; re-read on every accept() call
	metrics      *ConnectionMetrics               // block-level counters (secs1's own type — see secs1/metrics.go)
	notify       func(err error, header [10]byte) // reports a malformed-block violation; may be nil in tests that don't care

	// Partial-message accumulation state (single-goroutine; no locks).
	open          bool          // a partial (not yet terminated) message is in progress
	header        messageHeader // the block-invariant header of the open message (E4 §8)
	blocks        []block       // the accumulated blocks of the open message, in order
	expected      uint16        // the next expected block number (previous + 1; E4 §9.4.4)
	lastBlockTime time.Time     // arrival time of the last accepted block — the T4 base

	// Duplicate-block detection state (E4 §9.4.2). lastHeader is the full 10-byte header of the last
	// block APPENDED to a message; it PERSISTS across message completion (a running record of "the
	// last block accepted as non-duplicate") so a retransmitted single-/last-block is still caught
	// after the message it terminated has already been delivered.
	lastHeader [10]byte
	haveLast   bool
}

// newAssembler builds a fresh inbound assembler for cfg that delivers reassembled frames to
// deliverFrame (the core's rt.DeliverOwnedFrame). timers is called fresh on every accept() (never
// cached), so a live hsms.Connection.UpdateConfigOptions(WithT4(...)) takes effect on the current
// generation's next inbound block, not just the next reconnect. The clock defaults to time.Now;
// tests inject a.now to drive the T4 inter-block deadline deterministically.
func newAssembler(cfg Config, deliverFrame func([]byte) error, timers func() hsms.TimerConfig, metrics *ConnectionMetrics, notify func(err error, header [10]byte)) *assembler {
	return &assembler{
		isEquip:      cfg.IsEquip(),
		deviceID:     cfg.DeviceID(),
		deliverFrame: deliverFrame,
		now:          time.Now,
		timers:       timers,
		metrics:      metrics,
		notify:       notify,
	}
}

// report invokes a.notify if set (nil-safe — several unit tests construct an assembler with no
// notify callback because they only care about accumulation, not notification).
func (a *assembler) report(violation error, header [10]byte) {
	if a.notify != nil {
		a.notify(violation, header)
	}
}

// accept feeds one received block (already checksum-valid and ACK'd by receiveBlock) into the SEMI E4
// §9.4.4 message-receive algorithm. It runs on the single line-engine goroutine. In order:
//
//  1. R-bit direction validation (SP5b §6f / E4 §8.2, the P1 fix): a block NOT directed toward us is
//     a protocol violation — DROPPED (no append, no teardown; the link stays up).
//  2. Lazy T4 inter-block deadline (E4 §9.4.3): if a partial is open and the gap since its last
//     block exceeded T4, the stale partial is DISCARDED before this block is processed.
//  3. Duplicate-block detection (E4 §9.4.2): a retransmit (same full 10-byte header as the last
//     appended block — the peer missed our ACK) is DROPPED; it was already re-ACK'd by the line
//     layer, so it is neither re-appended nor re-delivered.
//  4. Expected-block accumulation (E4 §9.4.4): a block continuing the open message (matching expected
//     number and invariant header) is appended; any other block aborts the open partial and is
//     re-evaluated as a potential fresh FIRST block (number 1, or number 0 for a lone E-bit single
//     block, D5b-12).
//  5. E-bit termination (E4 §9.4.4.5): the last block completes the message — assembleFrame +
//     deliverFrame — then the partial resets.
//
// It returns nil for every dropped/discarded block (a protocol violation is not a link teardown);
// only a deliverFrame error on a completed message propagates (the engine treats inbound
// decode/route errors as non-fatal — the link stays up).
func (a *assembler) accept(blk block) error {
	// Step 0: device-ID routing (E4 §9.4.1): a block for a different device is not ours to process.
	if blk.deviceID() != a.deviceID {
		logger.Debug("secs1: inbound block dropped — device ID mismatch",
			"got", blk.deviceID(), "want", a.deviceID)
		a.metrics.incDeviceIDMismatchCount()
		a.report(ErrDeviceIDMismatch, blk.header)

		return nil
	}

	// Step 1: R-bit direction (E4 §8.2: R=0 → to equipment, R=1 → to host). Accept only blocks
	// directed TO US: equipment accepts R=0, host accepts R=1 — i.e. blk.rBit() != a.isEquip. A
	// wrong-direction block is dropped without disturbing the link.
	if blk.rBit() == a.isEquip {
		logger.Debug("secs1: inbound block dropped — wrong R-bit direction",
			"isEquip", a.isEquip, "rBit", blk.rBit(), "stream", blk.stream(), "function", blk.function())
		a.metrics.incBlockDirDropCount()

		return nil
	}

	// Step 2: lazy T4 inter-block deadline (E4 §9.4.3). Checked on the next block's arrival — no real
	// timer is armed (the generation tears down on disconnect regardless).
	if a.open && a.now().Sub(a.lastBlockTime) > a.timers().T4 {
		logger.Debug("secs1: inbound partial message discarded — T4 inter-block timeout",
			"stream", a.header.stream, "function", a.header.function, "expected", a.expected)
		a.metrics.incPartialTimeoutCount()
		a.reset()
	}

	// Step 3: duplicate-block detection (E4 §9.4.2). The line layer already re-ACK'd the retransmit.
	if a.haveLast && blk.header == a.lastHeader {
		a.metrics.incBlockDupDropCount()

		return nil
	}

	// Step 4: continue the open message, or (re)start on a fresh first block.
	if a.open {
		if blk.blockNumber() == a.expected && blk.messageHeader() == a.header {
			return a.appendBlock(blk)
		}
		// Not the expected block (§9.4.4.2): abort the open partial and fall through to re-evaluate
		// this block as a potential fresh first block.
		logger.Debug("secs1: inbound partial message discarded — unexpected block",
			"expected", a.expected, "got", blk.blockNumber())
		a.metrics.incBlockNumberMismatchCount()

		violation := ErrHeaderMismatch
		if blk.blockNumber() != a.expected {
			violation = ErrBlockNumberMismatch
		}
		a.report(violation, blk.header)
		a.reset()
	}

	return a.beginMessage(blk)
}

// beginMessage starts (or, for a single-block message, completes) a new inbound message from blk,
// which must be a valid FIRST block: number 1, or number 0 with the E-bit set (a lone single-block-0,
// D5b-12 interop leniency). A block that is neither is discarded (E4 §9.4.4.2 — sent in error).
func (a *assembler) beginMessage(blk block) error {
	num := blk.blockNumber()
	// A valid first block is number 1, or number 0 with the E-bit set (a lone single-block-0, D5b-12).
	validFirst := num == 1 || (num == 0 && blk.eBit())
	if !validFirst {
		logger.Debug("secs1: inbound block dropped — not a valid first block",
			"blockNumber", num, "eBit", blk.eBit(), "stream", blk.stream(), "function", blk.function())
		a.metrics.incInvalidFirstBlockCount()
		a.report(ErrInvalidFirstBlock, blk.header)

		return nil
	}

	a.open = true
	a.header = blk.messageHeader()
	a.blocks = append(a.blocks[:0], blk)
	a.expected = num + 1
	a.lastBlockTime = a.now()
	a.lastHeader = blk.header
	a.haveLast = true

	if blk.eBit() {
		return a.complete()
	}

	return nil
}

// appendBlock appends an in-sequence continuation block to the open message, advancing the T4 base
// and the expected block number (E4 §9.4.4.6). On the E-bit block it completes the message.
func (a *assembler) appendBlock(blk block) error {
	a.blocks = append(a.blocks, blk)
	a.expected = blk.blockNumber() + 1
	a.lastBlockTime = a.now()
	a.lastHeader = blk.header
	a.haveLast = true

	if blk.eBit() {
		return a.complete()
	}

	return nil
}

// complete assembles the accumulated blocks into a synthesized HSMS frame and delivers it to the
// core, then resets the partial state (the duplicate-detection record persists). It runs on the
// E-bit (last) block (E4 §9.4.4.5).
func (a *assembler) complete() error {
	frame, err := assembleFrame(a.blocks)
	a.reset()
	if err != nil {
		return fmt.Errorf("secs1: assemble inbound message: %w", err)
	}

	return a.deliverFrame(frame)
}

// reset clears the partial-message accumulation state. The duplicate-detection record
// (lastHeader/haveLast) deliberately PERSISTS — it is a running record of the last block accepted as
// non-duplicate (E4 §9.4.2), independent of message boundaries. The blocks slice is truncated (not
// niled) so its backing array is reused by the next message; the prior message's bodies were already
// copied into the delivered frame by complete, so the reuse cannot alias a live frame.
func (a *assembler) reset() {
	a.open = false
	a.header = messageHeader{}
	a.blocks = a.blocks[:0]
	a.expected = 0
	a.lastBlockTime = time.Time{}
}
