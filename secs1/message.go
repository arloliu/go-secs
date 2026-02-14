package secs1

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/internal/pool"
	"github.com/arloliu/go-secs/logger"
	"github.com/arloliu/go-secs/secs2"
)

// ===========================================================================
// Message splitting — split a DataMessage into SECS-I blocks for sending.
// ===========================================================================

// SplitMessage splits a DataMessage into one or more SECS-I blocks.
//
// Per SEMI E4 §9.2.2, message data is divided into blocks of at most
// MaxBlockBodySize (244) bytes. The first block has BlockNumber = 1,
// incrementing by 1 for each subsequent block. The last block has E-bit = 1.
// For a header-only message (no SECS-II body), a single block with
// BlockNumber = 0 and E-bit = 1 is produced.
//
// isEquip determines the R-bit direction per §8.2:
//   - true  (Equipment sending to Host): R-bit = 1, DeviceID = source
//   - false (Host sending to Equipment): R-bit = 0, DeviceID = destination
func SplitMessage(msg *hsms.DataMessage, deviceID uint16, isEquip bool) []*Block {
	body := marshalItem(msg.Item())
	systemBytes := msg.SystemBytes()

	if len(body) == 0 {
		blk := newBlockFromMessage(msg, deviceID, isEquip, systemBytes, nil, 0, true)

		return []*Block{blk}
	}

	var blocks []*Block

	blockNum := uint16(1)

	for offset := 0; offset < len(body); offset += MaxBlockBodySize {
		end := offset + MaxBlockBodySize
		if end > len(body) {
			end = len(body)
		}

		isLast := end == len(body)
		chunk := make([]byte, end-offset)
		copy(chunk, body[offset:end])

		blocks = append(blocks, newBlockFromMessage(msg, deviceID, isEquip, systemBytes, chunk, blockNum, isLast))
		blockNum++
	}

	return blocks
}

// ===========================================================================
// Message assembly — reassemble blocks received from the wire.
// ===========================================================================

// AssembleMessage reassembles a complete DataMessage from its constituent blocks.
//
// The blocks must be in order (by BlockNumber). Header information (Stream,
// Function, W-bit, DeviceID, SystemBytes) is taken from the first block.
// Body data is concatenated from all blocks.
//
// If the body is non-empty, it is decoded as a SECS-II item. If decoding
// fails, the raw data is wrapped in a BinaryItem to preserve it.
func AssembleMessage(blocks []*Block) (*hsms.DataMessage, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("secs1: cannot assemble message from zero blocks")
	}

	first := blocks[0]

	// Concatenate bodies with preallocation.
	totalLen := 0
	for _, b := range blocks {
		totalLen += len(b.Body)
	}

	body := make([]byte, 0, totalLen)
	for _, b := range blocks {
		body = append(body, b.Body...)
	}

	// Parse SECS-II item from body bytes.
	var item secs2.Item
	if len(body) > 0 {
		var err error
		item, err = hsms.DecodeSECS2Item(body)
		if err != nil {
			return nil, fmt.Errorf("secs1: decode SECS-II body: %w", err)
		}
	}

	msg, err := hsms.NewDataMessage(
		first.StreamCode(),
		first.FunctionCode(),
		first.WBit(),
		first.DeviceID(),
		first.SystemBytes(),
		item,
	)
	if err != nil {
		return nil, fmt.Errorf("secs1: assemble message: %w", err)
	}

	return msg, nil
}

// ===========================================================================
// Message assembler — stateful multi-block reassembly with T4 timers.
// ===========================================================================

// messageHandler is a callback for completed messages assembled from blocks.
type messageHandler func(blocks []*Block)

// messageAssembler implements the SEMI E4 §9.4 message receive algorithm.
//
// It collects blocks, detects duplicates, manages T4 inter-block timers,
// and delivers completed messages through a callback.
//
// All methods are safe for concurrent use.
type messageAssembler struct {
	cfg    *ConnectionConfig
	logger logger.Logger
	onMsg  messageHandler

	mu              sync.Mutex
	openMessages    map[uint64]*openMessage // key: compositeKey
	lastAcceptedHdr [blockHeaderSize]byte   // for duplicate detection (Section 9.4.2)
	hasPrevHeader   bool
}

// openMessage tracks the state of a multi-block message being assembled.
type openMessage struct {
	blocks      []*Block
	nextBlockNo uint16
	key         uint64
	t4Timer     *time.Timer
	t4Cancel    chan struct{} // closed to signal the T4 goroutine to exit

	// Header invariants from the first block, validated on continuation
	// blocks per SEMI E4 §9.4.4.6.
	wBit         bool
	streamCode   byte
	functionCode byte
}

// newMessageAssembler creates a new assembler.
func newMessageAssembler(cfg *ConnectionConfig, onMsg messageHandler) *messageAssembler {
	return &messageAssembler{
		cfg:          cfg,
		logger:       cfg.logger,
		onMsg:        onMsg,
		openMessages: make(map[uint64]*openMessage),
	}
}

// processBlock implements the message receive algorithm of SEMI E4 Section 9.4.4.
//
// It is called once for each block successfully received by the block-transfer
// layer. Blocks are buffered until a complete message is assembled, at which
// point the messageHandler callback is invoked.
//
// Returns an error for routing or protocol violations; the caller may
// choose to send S9Fx error messages in response.
func (a *messageAssembler) processBlock(block *Block) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Step 1: Routing check (Section 9.4.1).
	if block.DeviceID() != a.cfg.deviceID {
		a.logger.Debug("assembler: routing error, deviceID mismatch",
			"got", block.DeviceID(), "want", a.cfg.deviceID)

		return fmt.Errorf("%w: got %d, want %d",
			ErrDeviceIDMismatch, block.DeviceID(), a.cfg.deviceID)
	}

	// Step 2: Duplicate detection (Section 9.4.2).
	if a.cfg.duplicateDetection && a.isDuplicate(block) {
		a.logger.Debug("assembler: duplicate block discarded")

		return ErrDuplicateBlock
	}

	a.updateLastHeader(block)

	// Step 3: Expected block matching (Section 9.4.4).
	key := compositeKeyFromBlock(block)
	om, exists := a.openMessages[key]

	if !exists {
		return a.handleNewBlock(block, key)
	}

	return a.handleExpectedBlock(block, om)
}

// Close cancels all open T4 timers and clears state.
func (a *messageAssembler) close() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for key, om := range a.openMessages {
		a.cancelT4(om)
		delete(a.openMessages, key)
	}
}

// --- duplicate detection (Section 9.4.2) ---

func (a *messageAssembler) isDuplicate(block *Block) bool {
	if !a.hasPrevHeader {
		return false
	}

	return block.Header == a.lastAcceptedHdr
}

func (a *messageAssembler) updateLastHeader(block *Block) {
	a.lastAcceptedHdr = block.Header
	a.hasPrevHeader = true
}

// --- new block handling (Section 9.4.4.2) ---

// handleNewBlock processes a block that is NOT one of the expected blocks.
// Per Section 9.4.4.2, it must be the first block of a message (either
// primary or secondary). Secondary (reply) blocks are accepted because
// reply messages received over TCP also go through the assembler.
// Caller must hold a.mu.
func (a *messageAssembler) handleNewBlock(block *Block, key uint64) error {
	// Block number must be 0 (single block) or 1 (start of multi-block).
	bn := block.BlockNumber()
	if bn > 1 {
		a.logger.Debug("assembler: unexpected block number for new message",
			"blockNumber", bn)

		return fmt.Errorf("%w: expected 0 or 1 for new message, got %d",
			ErrBlockNumberMismatch, bn)
	}

	if block.EBit() {
		// Single-block message — deliver immediately.
		a.deliverComplete([]*Block{block})

		return nil
	}

	// Start of multi-block message.
	om := &openMessage{
		blocks:       []*Block{block},
		nextBlockNo:  bn + 1,
		key:          key,
		wBit:         block.WBit(),
		streamCode:   block.StreamCode(),
		functionCode: block.FunctionCode(),
	}
	a.openMessages[key] = om
	a.startT4(om)

	return nil
}

// --- expected block handling (Section 9.4.4.3 — 9.4.4.6) ---

// handleExpectedBlock processes a block that matches an open message.
// Caller must hold a.mu.
func (a *messageAssembler) handleExpectedBlock(block *Block, om *openMessage) error {
	// Validate block number.
	if block.BlockNumber() != om.nextBlockNo {
		a.logger.Debug("assembler: block number mismatch, aborting message",
			"expected", om.nextBlockNo, "got", block.BlockNumber())

		a.abortOpenMessage(om)

		return fmt.Errorf("%w: expected %d, got %d",
			ErrBlockNumberMismatch, om.nextBlockNo, block.BlockNumber())
	}

	// Validate header invariants per §9.4.4.6: continuation blocks must
	// have the same W-bit, stream (upper message ID), and function
	// (lower message ID) as the first block.
	if block.WBit() != om.wBit ||
		block.StreamCode() != om.streamCode ||
		block.FunctionCode() != om.functionCode {
		a.logger.Debug("assembler: header mismatch in continuation block",
			"expectedW", om.wBit, "gotW", block.WBit(),
			"expectedStream", om.streamCode, "gotStream", block.StreamCode(),
			"expectedFunction", om.functionCode, "gotFunction", block.FunctionCode())

		a.abortOpenMessage(om)

		return fmt.Errorf("%w: W-bit/stream/function do not match first block",
			ErrHeaderMismatch)
	}

	// Cancel the current T4 timer (block arrived in time).
	a.cancelT4(om)

	om.blocks = append(om.blocks, block)

	if block.EBit() {
		// Last block — message complete (Section 9.4.4.5).
		delete(a.openMessages, om.key)
		a.deliverComplete(om.blocks)

		return nil
	}

	// More blocks expected (Section 9.4.4.6).
	om.nextBlockNo = block.BlockNumber() + 1
	a.startT4(om)

	return nil
}

// --- T4 timer management ---

func (a *messageAssembler) startT4(om *openMessage) {
	om.t4Timer = pool.GetTimer(a.cfg.t4Timeout)
	om.t4Cancel = make(chan struct{})

	go func(key uint64, timer *time.Timer, cancel <-chan struct{}) {
		select {
		case <-timer.C:
			a.handleT4Expiry(key)
		case <-cancel:
			// T4 was cancelled (block arrived in time or message aborted).
		}
	}(om.key, om.t4Timer, om.t4Cancel)
}

func (a *messageAssembler) cancelT4(om *openMessage) {
	if om.t4Cancel != nil {
		close(om.t4Cancel)
		om.t4Cancel = nil
	}

	if om.t4Timer != nil {
		pool.PutTimer(om.t4Timer)
		om.t4Timer = nil
	}
}

// handleT4Expiry is called when T4 fires for an open message.
func (a *messageAssembler) handleT4Expiry(key uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	om, exists := a.openMessages[key]
	if !exists {
		return // already completed or aborted
	}

	a.logger.Warn("assembler: T4 inter-block timeout, aborting message",
		"key", key, "blocksReceived", len(om.blocks))

	delete(a.openMessages, key)
}

// abortOpenMessage removes an open message and cancels its T4 timer.
// Caller must hold a.mu.
func (a *messageAssembler) abortOpenMessage(om *openMessage) {
	a.cancelT4(om)
	delete(a.openMessages, om.key)
}

// --- delivery ---

// deliverComplete invokes the message handler outside the lock.
func (a *messageAssembler) deliverComplete(blocks []*Block) {
	if a.onMsg == nil {
		return
	}

	// Release the lock before calling the handler to avoid deadlock.
	a.mu.Unlock()
	a.onMsg(blocks)
	a.mu.Lock()
}

// ===========================================================================
// Helpers
// ===========================================================================

// newBlockFromMessage creates a single Block from DataMessage header info.
func newBlockFromMessage(
	msg *hsms.DataMessage,
	deviceID uint16,
	isEquip bool,
	systemBytes []byte,
	body []byte,
	blockNum uint16,
	isLast bool,
) *Block {
	blk := &Block{}

	// R-bit + DeviceID (bytes 0-1).
	// Per SEMI E4 §8.2: R=0 Host→Equipment, R=1 Equipment→Host.
	blk.SetDeviceID(deviceID)
	blk.SetRBit(isEquip)

	// W-bit + Stream (byte 2).
	blk.SetWBit(msg.WaitBit())
	blk.SetStreamCode(msg.StreamCode())

	// Function (byte 3).
	blk.SetFunctionCode(msg.FunctionCode())

	// E-bit + BlockNumber (bytes 4-5).
	blk.SetBlockNumber(blockNum)
	blk.SetEBit(isLast)

	// System Bytes (bytes 6-9).
	if len(systemBytes) == 4 {
		copy(blk.Header[6:10], systemBytes)
	}

	// Body.
	blk.Body = body

	return blk
}

// marshalItem serializes a SECS-II item to bytes. Returns nil for nil/empty items.
func marshalItem(item secs2.Item) []byte {
	if item == nil {
		return nil
	}

	data := item.ToBytes()
	if len(data) == 0 {
		return nil
	}

	return data
}

// compositeKey creates a unique map key from system bytes + device ID + R-bit.
//
// Layout (64-bit):
//
//	[63]    = R-bit
//	[47:32] = DeviceID (15-bit)
//	[31:0]  = SystemBytes (32-bit)
func compositeKey(systemBytes uint32, deviceID uint16, rBit bool) uint64 {
	key := uint64(systemBytes) | (uint64(deviceID) << 32)
	if rBit {
		key |= 1 << 63
	}

	return key
}

// compositeKeyFromBlock extracts the composite key from a block's header.
func compositeKeyFromBlock(b *Block) uint64 {
	return compositeKey(
		binary.BigEndian.Uint32(b.Header[6:10]),
		b.DeviceID(),
		b.RBit(),
	)
}
