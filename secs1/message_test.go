package secs1

import (
	"testing"
	"time"

	"github.com/arloliu/go-secs/hsms"
	"github.com/arloliu/go-secs/secs2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// SplitMessage tests
// ===========================================================================

func TestSplitMessage_HeaderOnly(t *testing.T) {
	t.Parallel()

	msg := mustDataMessage(t, 1, 13, false, 5, []byte{0, 0, 0, 1}, nil)
	blocks := SplitMessage(msg, 5, true)

	require.Len(t, blocks, 1)

	blk := blocks[0]
	assert.Equal(t, byte(1), blk.StreamCode())
	assert.Equal(t, byte(13), blk.FunctionCode())
	assert.False(t, blk.WBit())
	assert.True(t, blk.RBit(), "equipment sends R=1")
	assert.Equal(t, uint16(5), blk.DeviceID())
	assert.True(t, blk.EBit(), "single block must have E=1")
	assert.Equal(t, uint16(0), blk.BlockNumber(), "header-only block has BlockNumber=0")
	assert.Empty(t, blk.Body)
	assert.Equal(t, []byte{0, 0, 0, 1}, blk.SystemBytes())
}

func TestSplitMessage_SingleBlock(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("hello")
	msg := mustDataMessage(t, 1, 1, true, 10, []byte{0, 0, 0, 2}, item)
	blocks := SplitMessage(msg, 10, false)

	require.Len(t, blocks, 1)

	blk := blocks[0]
	assert.Equal(t, byte(1), blk.StreamCode())
	assert.Equal(t, byte(1), blk.FunctionCode())
	assert.True(t, blk.WBit())
	assert.False(t, blk.RBit(), "host sends R=0")
	assert.Equal(t, uint16(10), blk.DeviceID())
	assert.True(t, blk.EBit())
	assert.Equal(t, uint16(1), blk.BlockNumber())
	assert.Equal(t, item.ToBytes(), blk.Body)
}

func TestSplitMessage_MultiBlock(t *testing.T) {
	t.Parallel()

	// Create a large binary payload that requires multiple blocks.
	// Each block body can hold MaxBlockBodySize (244) bytes.
	// 500 raw bytes → 3 blocks (244 + 244 + header overhead).
	rawValues := make([]byte, 500)
	for i := range rawValues {
		rawValues[i] = byte(i % 256)
	}

	item := secs2.NewBinaryItem(rawValues)
	body := item.ToBytes() // includes SECS-II header bytes

	msg := mustDataMessage(t, 6, 11, true, 100, []byte{0xAB, 0xCD, 0x00, 0x01}, item)
	blocks := SplitMessage(msg, 100, true)

	// Calculate expected block count.
	expectedBlocks := (len(body) + MaxBlockBodySize - 1) / MaxBlockBodySize
	require.Len(t, blocks, expectedBlocks)

	// Verify all blocks share system bytes, DeviceID, Stream, Function.
	for i, blk := range blocks {
		assert.Equal(t, byte(6), blk.StreamCode(), "block %d stream", i)
		assert.Equal(t, byte(11), blk.FunctionCode(), "block %d function", i)
		assert.True(t, blk.WBit(), "block %d W-bit", i)
		assert.True(t, blk.RBit(), "block %d R-bit", i)
		assert.Equal(t, uint16(100), blk.DeviceID(), "block %d deviceID", i)
		assert.Equal(t, []byte{0xAB, 0xCD, 0x00, 0x01}, blk.SystemBytes(), "block %d systemBytes", i)
		assert.Equal(t, uint16(i+1), blk.BlockNumber(), "block %d blockNumber", i)
	}

	// Only the last block has E-bit set.
	for i := 0; i < len(blocks)-1; i++ {
		assert.False(t, blocks[i].EBit(), "block %d should not have E-bit", i)
	}

	assert.True(t, blocks[len(blocks)-1].EBit(), "last block must have E-bit")

	// Body sizes: all but last are MaxBlockBodySize, last is remainder.
	for i := 0; i < len(blocks)-1; i++ {
		assert.Len(t, blocks[i].Body, MaxBlockBodySize, "block %d body size", i)
	}

	lastBodyLen := len(body) - MaxBlockBodySize*(expectedBlocks-1)
	assert.Len(t, blocks[len(blocks)-1].Body, lastBodyLen, "last block body size")

	// Concatenated bodies equal the original serialized SECS-II data.
	var concat []byte
	for _, blk := range blocks {
		concat = append(concat, blk.Body...)
	}

	assert.Equal(t, body, concat)
}

func TestSplitMessage_ExactBoundary(t *testing.T) {
	t.Parallel()

	// Create body that is exactly MaxBlockBodySize bytes (after SECS-II header).
	// BinaryItem with 242 values: 2-byte header (0x21, 0xF2) + 242 data = 244.
	rawValues := make([]byte, 242)
	item := secs2.NewBinaryItem(rawValues)
	body := item.ToBytes()
	require.Equal(t, MaxBlockBodySize, len(body), "setup: body must be exactly 244")

	msg := mustDataMessage(t, 1, 1, true, 1, []byte{0, 0, 0, 1}, item)
	blocks := SplitMessage(msg, 1, false)

	require.Len(t, blocks, 1)
	assert.True(t, blocks[0].EBit())
	assert.Equal(t, uint16(1), blocks[0].BlockNumber())
	assert.Len(t, blocks[0].Body, MaxBlockBodySize)
}

func TestSplitMessage_ExactTwoBlocks(t *testing.T) {
	t.Parallel()

	// Create body that is exactly 2*MaxBlockBodySize.
	// BinaryItem with 485 values: 3-byte header (0x22, 0x01, 0xE5) + 485 = 488 → 2 blocks.
	rawValues := make([]byte, 485)
	item := secs2.NewBinaryItem(rawValues)
	body := item.ToBytes()
	require.Equal(t, 2*MaxBlockBodySize, len(body), "setup: body must be exactly 488")

	msg := mustDataMessage(t, 2, 3, false, 1, []byte{0, 0, 0, 1}, item)
	blocks := SplitMessage(msg, 1, false)

	require.Len(t, blocks, 2)
	assert.False(t, blocks[0].EBit())
	assert.True(t, blocks[1].EBit())
	assert.Equal(t, uint16(1), blocks[0].BlockNumber())
	assert.Equal(t, uint16(2), blocks[1].BlockNumber())
	assert.Len(t, blocks[0].Body, MaxBlockBodySize)
	assert.Len(t, blocks[1].Body, MaxBlockBodySize)
}

func TestSplitMessage_RBitDirection(t *testing.T) {
	t.Parallel()

	sys := []byte{0, 0, 0, 1}

	t.Run("Equipment_R1", func(t *testing.T) {
		t.Parallel()

		item := secs2.NewASCIIItem("x")
		msg := mustDataMessage(t, 1, 2, false, 5, sys, item)
		blocks := SplitMessage(msg, 5, true)
		assert.True(t, blocks[0].RBit())
	})

	t.Run("Host_R0", func(t *testing.T) {
		t.Parallel()

		item := secs2.NewASCIIItem("x")
		msg := mustDataMessage(t, 1, 2, false, 5, sys, item)
		blocks := SplitMessage(msg, 5, false)
		assert.False(t, blocks[0].RBit())
	})
}

// ===========================================================================
// AssembleMessage tests
// ===========================================================================

func TestAssembleMessage_SingleBlock(t *testing.T) {
	t.Parallel()

	item := secs2.NewASCIIItem("hello")
	orig := mustDataMessage(t, 1, 1, true, 10, []byte{0, 0, 0, 1}, item)

	blocks := SplitMessage(orig, 10, false)
	require.Len(t, blocks, 1)

	got, err := AssembleMessage(blocks)
	require.NoError(t, err)
	assert.Equal(t, orig.StreamCode(), got.StreamCode())
	assert.Equal(t, orig.FunctionCode(), got.FunctionCode())
	assert.Equal(t, orig.WaitBit(), got.WaitBit())
	assert.Equal(t, uint16(10), got.SessionID())
	assert.Equal(t, orig.SystemBytes(), got.SystemBytes())
	assert.Equal(t, item.ToSML(), got.Item().ToSML())
}

func TestAssembleMessage_MultiBlock(t *testing.T) {
	t.Parallel()

	rawValues := make([]byte, 500)
	for i := range rawValues {
		rawValues[i] = byte(i % 256)
	}

	item := secs2.NewBinaryItem(rawValues)
	orig := mustDataMessage(t, 6, 11, true, 50, []byte{0, 0, 0, 99}, item)

	blocks := SplitMessage(orig, 50, true)
	require.Greater(t, len(blocks), 1)

	got, err := AssembleMessage(blocks)
	require.NoError(t, err)
	assert.Equal(t, orig.StreamCode(), got.StreamCode())
	assert.Equal(t, orig.FunctionCode(), got.FunctionCode())
	assert.Equal(t, orig.WaitBit(), got.WaitBit())
	assert.Equal(t, uint16(50), got.SessionID())
	assert.Equal(t, item.ToSML(), got.Item().ToSML())
}

func TestAssembleMessage_HeaderOnly(t *testing.T) {
	t.Parallel()

	orig := mustDataMessage(t, 9, 7, false, 1, []byte{0, 0, 0, 5}, nil)
	blocks := SplitMessage(orig, 1, false)
	require.Len(t, blocks, 1)

	got, err := AssembleMessage(blocks)
	require.NoError(t, err)
	assert.Equal(t, byte(9), got.StreamCode())
	assert.Equal(t, byte(7), got.FunctionCode())
	assert.False(t, got.WaitBit())
}

func TestAssembleMessage_ZeroBlocks(t *testing.T) {
	t.Parallel()

	_, err := AssembleMessage(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero blocks")
}

func TestAssembleMessage_InvalidSECS2Body(t *testing.T) {
	t.Parallel()

	// Create a block with garbage body data that cannot be decoded.
	blk := &Block{}
	blk.SetStreamCode(1)
	blk.SetFunctionCode(1)
	blk.SetEBit(true)
	blk.SetBlockNumber(1)
	blk.SetSystemBytesUint32(0x12345678)
	blk.Body = []byte{0xFF, 0xFF, 0xFF} // invalid SECS-II

	_, err := AssembleMessage([]*Block{blk})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode SECS-II")
}

func TestSplitAssemble_Roundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stream  byte
		fn      byte
		wbit    bool
		devID   uint16
		isEquip bool
		item    secs2.Item
	}{
		{"ASCII", 1, 1, true, 5, false, secs2.NewASCIIItem("S1F1")},
		{"EmptyBody", 1, 13, false, 0, true, nil},
		{"Binary500", 6, 11, true, 100, true, secs2.NewBinaryItem(make([]byte, 500))},
		{"SmallBinary", 2, 25, true, 1, false, secs2.NewBinaryItem([]byte{0x01, 0x02, 0x03})},
		{
			"List", 1, 3, true, 7, false,
			secs2.NewListItem(
				secs2.NewASCIIItem("name"),
				secs2.NewUintItem(2, 42),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sys := []byte{0x00, 0x00, 0x01, 0x23}
			orig := mustDataMessage(t, tc.stream, tc.fn, tc.wbit, tc.devID, sys, tc.item)

			blocks := SplitMessage(orig, tc.devID, tc.isEquip)
			require.NotEmpty(t, blocks)

			got, err := AssembleMessage(blocks)
			require.NoError(t, err)
			assert.Equal(t, orig.StreamCode(), got.StreamCode())
			assert.Equal(t, orig.FunctionCode(), got.FunctionCode())
			assert.Equal(t, orig.WaitBit(), got.WaitBit())
			assert.Equal(t, orig.SystemBytes(), got.SystemBytes())

			if tc.item != nil {
				assert.Equal(t, tc.item.ToSML(), got.Item().ToSML())
			}
		})
	}
}

// ===========================================================================
// compositeKey tests
// ===========================================================================

func TestCompositeKey_UniqueForDifferentInputs(t *testing.T) {
	t.Parallel()

	// Same system bytes, different deviceID → different key.
	k1 := compositeKey(0x01, 10, false)
	k2 := compositeKey(0x01, 20, false)
	assert.NotEqual(t, k1, k2)

	// Same system bytes and deviceID, different R-bit → different key.
	k3 := compositeKey(0x01, 10, false)
	k4 := compositeKey(0x01, 10, true)
	assert.NotEqual(t, k3, k4)

	// Different system bytes → different key.
	k5 := compositeKey(0x01, 10, false)
	k6 := compositeKey(0x02, 10, false)
	assert.NotEqual(t, k5, k6)

	// Identical inputs → same key.
	k7 := compositeKey(0xABCD, 100, true)
	k8 := compositeKey(0xABCD, 100, true)
	assert.Equal(t, k7, k8)
}

func TestCompositeKeyFromBlock(t *testing.T) {
	t.Parallel()

	blk := &Block{}
	blk.SetDeviceID(42)
	blk.SetRBit(true)
	blk.SetSystemBytesUint32(0x12345678)

	expected := compositeKey(0x12345678, 42, true)
	assert.Equal(t, expected, compositeKeyFromBlock(blk))
}

// ===========================================================================
// messageAssembler tests
// ===========================================================================

// --- helpers for assembler tests ---

// newAssemblerConfig creates a config suitable for assembler tests.
func newAssemblerConfig(t *testing.T, opts ...ConnOption) *ConnectionConfig {
	t.Helper()

	defaults := []ConnOption{
		WithDuplicateDetection(true),
	}
	defaults = append(defaults, opts...)

	cfg, err := NewConnectionConfig("127.0.0.1", 5000, defaults...)
	require.NoError(t, err)

	return cfg
}

// withShortT4 directly sets the T4 timeout on a config, bypassing
// validation. Use only in tests that need sub-second T4.
func withShortT4(cfg *ConnectionConfig, d time.Duration) {
	cfg.t4Timeout = d
}

// blockOpts configures a test block.
type blockOpts struct {
	stream, function byte
	wBit, rBit, eBit bool
	blockNum         uint16
	deviceID         uint16
	systemBytes      uint32
	body             []byte
}

// newTestAssemblerBlock creates a block from options.
func newTestAssemblerBlock(o blockOpts) *Block {
	blk := &Block{}
	blk.SetStreamCode(o.stream)
	blk.SetFunctionCode(o.function)
	blk.SetWBit(o.wBit)
	blk.SetRBit(o.rBit)
	blk.SetEBit(o.eBit)
	blk.SetBlockNumber(o.blockNum)
	blk.SetDeviceID(o.deviceID)
	blk.SetSystemBytesUint32(o.systemBytes)

	if len(o.body) > 0 {
		blk.Body = make([]byte, len(o.body))
		copy(blk.Body, o.body)
	}

	return blk
}

// --- processBlock: single-block messages ---

func TestProcessBlock_SingleBlockPrimary(t *testing.T) {
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	item := secs2.NewASCIIItem("hello")
	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID, systemBytes: 0x00000001,
		body: item.ToBytes(),
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)
	require.Len(t, delivered, 1)
	assert.Equal(t, byte(1), delivered[0].StreamCode())
	assert.Equal(t, byte(1), delivered[0].FunctionCode())
}

func TestProcessBlock_SingleBlockPrimary_BlockNum1(t *testing.T) {
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: 0x00000002,
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)
	require.Len(t, delivered, 1)
}

// --- processBlock: multi-block messages ---

func TestProcessBlock_MultiBlock(t *testing.T) {
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	sys := uint32(0x00000010)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01, 0x02},
	})
	require.NoError(t, asm.processBlock(blk1))
	require.Nil(t, delivered, "should not deliver yet")

	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: false,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x03, 0x04},
	})
	require.NoError(t, asm.processBlock(blk2))
	require.Nil(t, delivered)

	blk3 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: true,
		blockNum: 3, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x05},
	})
	require.NoError(t, asm.processBlock(blk3))
	require.Len(t, delivered, 3)

	assert.Equal(t, uint16(1), delivered[0].BlockNumber())
	assert.Equal(t, uint16(2), delivered[1].BlockNumber())
	assert.Equal(t, uint16(3), delivered[2].BlockNumber())
}

// --- processBlock: routing error ---

func TestProcessBlock_RoutingError(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver on routing error")
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID + 1, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDeviceIDMismatch)
}

// --- processBlock: duplicate detection ---

func TestProcessBlock_DuplicateDetection(t *testing.T) {
	t.Parallel()

	deliverCount := 0

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		deliverCount++
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)
	assert.Equal(t, 1, deliverCount)

	err = asm.processBlock(blk)
	require.ErrorIs(t, err, ErrDuplicateBlock)
	assert.Equal(t, 1, deliverCount, "duplicate should not trigger delivery")
}

func TestProcessBlock_DuplicateDetectionDisabled(t *testing.T) {
	t.Parallel()

	deliverCount := 0

	cfg := newAssemblerConfig(t, WithDuplicateDetection(false))
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		deliverCount++
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)

	err = asm.processBlock(blk)
	require.NoError(t, err)
	assert.Equal(t, 2, deliverCount)
}

// --- processBlock: secondary (reply) message ---

func TestProcessBlock_SecondaryBlock(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	deliverCount := 0
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		deliverCount++
		require.Len(t, blocks, 1)
		assert.Equal(t, byte(2), blocks[0].FunctionCode())
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 2, wBit: false, rBit: false, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)
	assert.Equal(t, 1, deliverCount)
}

// --- processBlock: block number mismatch ---

func TestProcessBlock_BlockNumberMismatch(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver incomplete message")
	})
	defer asm.close()

	sys := uint32(0x00000020)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	blk3 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 3, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x03},
	})
	err := asm.processBlock(blk3)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBlockNumberMismatch)

	asm.mu.Lock()
	_, exists := asm.openMessages[compositeKeyFromBlock(blk1)]
	asm.mu.Unlock()
	assert.False(t, exists, "open message should be cleaned up after abort")
}

// --- processBlock: new block with invalid block number ---

func TestProcessBlock_NewBlockInvalidBlockNumber(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver")
	})
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 5, deviceID: cfg.deviceID, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBlockNumberMismatch)
}

// --- processBlock: T4 timeout ---

func TestProcessBlock_T4Timeout(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	withShortT4(cfg, 100*time.Millisecond)

	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver incomplete message")
	})
	defer asm.close()

	sys := uint32(0x00000030)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	time.Sleep(250 * time.Millisecond)

	asm.mu.Lock()
	_, exists := asm.openMessages[compositeKeyFromBlock(blk1)]
	asm.mu.Unlock()
	assert.False(t, exists, "T4 expiry should remove the open message")
}

func TestProcessBlock_T4ResetOnBlock(t *testing.T) {
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	withShortT4(cfg, 200*time.Millisecond)

	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	sys := uint32(0x00000040)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	time.Sleep(150 * time.Millisecond)

	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	require.NoError(t, asm.processBlock(blk2))

	time.Sleep(150 * time.Millisecond)

	blk3 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 3, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x03},
	})
	require.NoError(t, asm.processBlock(blk3))

	require.Len(t, delivered, 3, "message should complete despite total time > T4")
}

// --- processBlock: interleaved messages ---

func TestProcessBlock_InterleavedMessages(t *testing.T) {
	t.Parallel()

	var deliveries [][]*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		cp := make([]*Block, len(blocks))
		copy(cp, blocks)
		deliveries = append(deliveries, cp)
	})
	defer asm.close()

	sysA := uint32(0x00000001)
	sysB := uint32(0x00000002)

	blkA1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sysA,
		body: []byte{0xA1},
	})
	require.NoError(t, asm.processBlock(blkA1))

	blkB1 := newTestAssemblerBlock(blockOpts{
		stream: 2, function: 3, wBit: true, rBit: false, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sysB,
		body: []byte{0xB1},
	})
	require.NoError(t, asm.processBlock(blkB1))

	blkA2 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sysA,
		body: []byte{0xA2},
	})
	require.NoError(t, asm.processBlock(blkA2))

	require.Len(t, deliveries, 1, "only message A should be complete")
	assert.Equal(t, uint32(0x00000001), deliveries[0][0].SystemBytesUint32())

	blkB2 := newTestAssemblerBlock(blockOpts{
		stream: 2, function: 3, wBit: true, rBit: false, eBit: true,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sysB,
		body: []byte{0xB2},
	})
	require.NoError(t, asm.processBlock(blkB2))

	require.Len(t, deliveries, 2, "both messages should be complete")
	assert.Equal(t, uint32(0x00000002), deliveries[1][0].SystemBytesUint32())
}

// --- processBlock: Close cleans up ---

func TestAssembler_Close(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {})

	sys := uint32(0x00000050)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	asm.mu.Lock()
	assert.Len(t, asm.openMessages, 1)
	asm.mu.Unlock()

	asm.close()

	asm.mu.Lock()
	assert.Empty(t, asm.openMessages)
	asm.mu.Unlock()
}

// --- processBlock: nil handler ---

func TestProcessBlock_NilHandler(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, nil)
	defer asm.close()

	blk := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 0, deviceID: cfg.deviceID, systemBytes: 0x00000001,
	})

	err := asm.processBlock(blk)
	require.NoError(t, err)
}

// --- processBlock: duplicate in multi-block ---

func TestProcessBlock_DuplicateInMultiBlock(t *testing.T) {
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	sys := uint32(0x00000060)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	require.NoError(t, asm.processBlock(blk2))

	// Block 2 again (sender retried after ACK lost) -> duplicate.
	err := asm.processBlock(blk2)
	require.ErrorIs(t, err, ErrDuplicateBlock)

	// Block 3 (final) should still work.
	blk3 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 3, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x03},
	})
	require.NoError(t, asm.processBlock(blk3))
	require.Len(t, delivered, 3)
}

// ===========================================================================
// test helpers
// ===========================================================================

func mustDataMessage(t *testing.T, stream, function byte, wbit bool, sessionID uint16, sys []byte, item secs2.Item) *hsms.DataMessage {
	t.Helper()

	if item == nil {
		item = secs2.NewEmptyItem()
	}

	msg, err := hsms.NewDataMessage(stream, function, wbit, sessionID, sys, item)
	require.NoError(t, err)

	return msg
}

// ===========================================================================
// Expected-block header validation (§9.4.4.6)
// ===========================================================================

func TestProcessBlock_HeaderMismatch_WBit(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver on header mismatch")
	})
	defer asm.close()

	sys := uint32(0x00000070)

	// Block 1: W-bit = true.
	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	// Block 2: W-bit = false (mismatch).
	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: false, rBit: true, eBit: true,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	err := asm.processBlock(blk2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHeaderMismatch)

	// Open message should be cleaned up.
	asm.mu.Lock()
	assert.Empty(t, asm.openMessages)
	asm.mu.Unlock()
}

func TestProcessBlock_HeaderMismatch_Stream(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver on header mismatch")
	})
	defer asm.close()

	sys := uint32(0x00000071)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	// Block 2: stream changed from 1 to 2 (mismatch).
	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 2, function: 1, wBit: true, rBit: true, eBit: true,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	err := asm.processBlock(blk2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHeaderMismatch)
}

func TestProcessBlock_HeaderMismatch_Function(t *testing.T) {
	t.Parallel()

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(_ []*Block) {
		t.Fatal("should not deliver on header mismatch")
	})
	defer asm.close()

	sys := uint32(0x00000072)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 1, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	// Block 2: function changed from 1 to 3 (mismatch).
	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 1, function: 3, wBit: true, rBit: true, eBit: true,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	err := asm.processBlock(blk2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHeaderMismatch)
}

func TestProcessBlock_HeaderConsistent_Succeeds(t *testing.T) {
	// Positive test: multi-block with consistent headers succeeds.
	t.Parallel()

	var delivered []*Block

	cfg := newAssemblerConfig(t)
	asm := newMessageAssembler(cfg, func(blocks []*Block) {
		delivered = blocks
	})
	defer asm.close()

	sys := uint32(0x00000073)

	blk1 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: false,
		blockNum: 1, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x01},
	})
	require.NoError(t, asm.processBlock(blk1))

	blk2 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: false,
		blockNum: 2, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x02},
	})
	require.NoError(t, asm.processBlock(blk2))

	blk3 := newTestAssemblerBlock(blockOpts{
		stream: 6, function: 11, wBit: true, rBit: true, eBit: true,
		blockNum: 3, deviceID: cfg.deviceID, systemBytes: sys,
		body: []byte{0x03},
	})
	require.NoError(t, asm.processBlock(blk3))

	require.Len(t, delivered, 3)
}
