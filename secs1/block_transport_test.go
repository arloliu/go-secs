package secs1

import (
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arloliu/go-secs/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// receiveBlock tests
// ===========================================================================

// --- Receive: success cases ---

func TestReceiveBlock_Success_HeaderOnly(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock() // header-only, no body
	wireData := block.Pack()

	go func() {
		// Remote sends the block wire data (length + header + checksum).
		mustWrite(t, remote, wireData)

		// Remote reads ACK.
		b := readOneByte(t, remote)
		assert.Equal(t, ACK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)
	require.NotNil(t, received)

	assert.Equal(t, block.Header, received.Header)
	assert.Empty(t, received.Body)
	assert.Equal(t, block.Checksum(), received.Checksum())
}

func TestReceiveBlock_Success_WithBody(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	body := []byte("hello, SECS-I!")
	block := makeTestBlockWithBody(body)
	wireData := block.Pack()

	go func() {
		mustWrite(t, remote, wireData)
		b := readOneByte(t, remote)
		assert.Equal(t, ACK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)
	require.NotNil(t, received)

	assert.Equal(t, block.Header, received.Header)
	assert.Equal(t, body, received.Body)
}

func TestReceiveBlock_Success_MaxSize(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	body := make([]byte, MaxBlockBodySize) // 244 bytes
	for i := range body {
		body[i] = byte(i)
	}

	block := makeTestBlockWithBody(body)
	wireData := block.Pack()

	go func() {
		mustWrite(t, remote, wireData)
		b := readOneByte(t, remote)
		assert.Equal(t, ACK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)
	require.NotNil(t, received)

	assert.Len(t, received.Body, MaxBlockBodySize)
	assert.Equal(t, body, received.Body)
}

func TestReceiveBlock_Success_ChunkedDelivery(t *testing.T) {
	// Simulate TCP delivering data in small chunks.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlockWithBody([]byte{0xAA, 0xBB, 0xCC, 0xDD})
	wireData := block.Pack()

	go func() {
		// Send length byte alone.
		mustWrite(t, remote, wireData[:1])
		time.Sleep(5 * time.Millisecond)

		// Send header in two chunks.
		mustWrite(t, remote, wireData[1:6])
		time.Sleep(5 * time.Millisecond)
		mustWrite(t, remote, wireData[6:])

		b := readOneByte(t, remote)
		assert.Equal(t, ACK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)
	require.NotNil(t, received)

	assert.Equal(t, block.Header, received.Header)
}

// --- Receive: error cases ---

func TestReceiveBlock_T2Timeout_NoLengthByte(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	go func() {
		// Remote sends nothing. Receiver T2-times out.
		// Read the NAK that receiver will send.
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrT2Timeout))
}

func TestReceiveBlock_InvalidLength_TooSmall(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	go func() {
		// Send invalid length byte (5 < MinBlockLength=10).
		mustWrite(t, remote, []byte{5})
		// Receiver drains, then sends NAK.
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrInvalidLength))
}

func TestReceiveBlock_InvalidLength_TooLarge(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	go func() {
		// Send invalid length byte (255 > MaxBlockLength=254).
		mustWrite(t, remote, []byte{255})
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrInvalidLength))
}

func TestReceiveBlock_T1Timeout_PartialData(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	go func() {
		// Send valid length byte (10 = header-only), then only 5 bytes (incomplete).
		mustWrite(t, remote, []byte{10})
		mustWrite(t, remote, []byte{0, 0, 0, 0, 0})
		// Don't send the rest. Receiver T1-times out.
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrT1Timeout))
}

func TestReceiveBlock_ChecksumMismatch(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	wireData := block.Pack()

	// Corrupt the checksum (last 2 bytes of wire data).
	wireData[len(wireData)-1] ^= 0xFF
	wireData[len(wireData)-2] ^= 0xFF

	go func() {
		mustWrite(t, remote, wireData)
		// Receiver drains after checksum mismatch, then sends NAK.
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrChecksumMismatch))
}

func TestReceiveBlock_CorruptedHeaderData(t *testing.T) {
	// Send valid length + data but with wrong checksum (corrupted header byte).
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	wireData := block.Pack()

	// Corrupt a header byte (byte index 1 is first header byte after length).
	wireData[1] ^= 0xFF

	go func() {
		mustWrite(t, remote, wireData)
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	received, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, ErrChecksumMismatch))
}

func TestReceiveBlock_ContextCancelled(t *testing.T) {
	cfg := newTestConfig(t)
	bt, _ := newTestTransport(t, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	received, err := bt.receiveBlock(ctx)
	require.Error(t, err)
	assert.Nil(t, received)
	assert.True(t, errors.Is(err, context.Canceled))
}

// --- Receive: verify ACK/NAK wire bytes ---

func TestReceiveBlock_ACK_WireValue(t *testing.T) {
	// Verify that the ACK byte sent after a successful receive is correct.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	wireData := block.Pack()

	ackCh := make(chan byte, 1)

	go func() {
		mustWrite(t, remote, wireData)
		b := readOneByte(t, remote)
		ackCh <- b
	}()

	_, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)

	ackByte := <-ackCh
	assert.Equal(t, byte(0x06), ackByte, "ACK should be 0x06")
}

func TestReceiveBlock_DrainBeforeNAK(t *testing.T) {
	// Verify that on invalid length, the receiver drains extra bytes
	// before sending NAK.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	go func() {
		// Send invalid length byte (5 < 10), followed by trailing garbage.
		mustWrite(t, remote, []byte{5, 0xDE, 0xAD, 0xBE, 0xEF})
		// Receiver should drain the garbage, wait for T1 silence, then send NAK.
		b := readOneByte(t, remote)
		assert.Equal(t, NAK, b)
	}()

	_, err := bt.receiveBlock(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidLength))
}

// --- Receive: roundtrip verification ---

func TestReceiveBlock_Roundtrip_SystemBytes(t *testing.T) {
	// Verify that system bytes survive the pack → receive roundtrip.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	block.SetSystemBytesUint32(0xDEADBEEF)
	wireData := block.Pack()

	go func() {
		mustWrite(t, remote, wireData)
		readOneByte(t, remote) // ACK
	}()

	received, err := bt.receiveBlock(context.Background())
	require.NoError(t, err)

	assert.Equal(t, uint32(0xDEADBEEF), received.SystemBytesUint32())
	assert.Equal(t, binary.BigEndian.Uint32(block.Header[6:10]), received.SystemBytesUint32())
}

// ===========================================================================
// sendBlock tests
// ===========================================================================

// --- Send: success cases ---

func TestSendBlock_Success(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		// Read ENQ from sender.
		b := readOneByte(t, remote)
		assert.Equal(t, ENQ, b)

		// Send EOT.
		mustWrite(t, remote, []byte{EOT})

		// Read block wire data.
		wireData := readExactly(t, remote, len(expectedWire))
		assert.Equal(t, expectedWire, wireData)

		// Send ACK.
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

func TestSendBlock_Success_WithBody(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlockWithBody([]byte("payload data"))
	expectedWire := block.Pack()

	go func() {
		readOneByte(t, remote)                                // ENQ
		mustWrite(t, remote, []byte{EOT})                     // EOT
		wireData := readExactly(t, remote, len(expectedWire)) // block
		assert.Equal(t, expectedWire, wireData)
		mustWrite(t, remote, []byte{ACK}) // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

// --- Send: retry cases ---

func TestSendBlock_T2Timeout_ThenSuccess(t *testing.T) {
	// First attempt: T2 timeout (no response to ENQ).
	// Second attempt: success.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()
	attempt := 0

	go func() {
		// Attempt 1: read ENQ, don't respond (let T2 expire).
		b := readOneByte(t, remote)
		assert.Equal(t, ENQ, b)
		attempt++

		// Attempt 2: read ENQ, respond with EOT + ACK.
		b = readOneByte(t, remote)
		assert.Equal(t, ENQ, b)
		attempt++
		mustWrite(t, remote, []byte{EOT})
		readExactly(t, remote, len(expectedWire))
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
	assert.Equal(t, 2, attempt)
}

func TestSendBlock_RetryExhaustion(t *testing.T) {
	// All retries time out → ErrSendFailure.
	cfg := newTestConfig(t, WithRetryLimit(2))
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()

	go func() {
		// Read 3 ENQs (initial + 2 retries), never respond.
		for i := 0; i < 3; i++ {
			b := readOneByte(t, remote)
			assert.Equal(t, ENQ, b)
		}
	}()

	err := bt.sendBlock(context.Background(), block)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSendFailure))
}

func TestSendBlock_NAK_ThenSuccess(t *testing.T) {
	// First attempt: ENQ→EOT→block→NAK (retry).
	// Second attempt: ENQ→EOT→block→ACK (success).
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		// Attempt 1: ENQ→EOT→block→NAK.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{NAK})         // NAK

		// Attempt 2: ENQ→EOT→block→ACK.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

func TestSendBlock_NonACK_ThenSuccess(t *testing.T) {
	// First attempt: ENQ→EOT→block→garbage (retry).
	// Second attempt: success.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{0xFF})        // garbage byte

		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

func TestSendBlock_T2TimeoutAfterBlock_ThenSuccess(t *testing.T) {
	// First attempt: ENQ→EOT→block→(no ACK, T2 expires) → retry.
	// Second attempt: success.
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		// Attempt 1: don't send ACK after receiving block.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		// No ACK → T2 timeout triggers retry.

		// Attempt 2: full success.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

// --- Send: contention cases ---

func TestSendBlock_Contention_Slave(t *testing.T) {
	// This end is Slave (Host). After sending ENQ, receives ENQ (contention).
	// Slave yields: sends EOT, receives Master's block, then retries own send.
	cfg := newTestConfig(t) // default: Host = Slave
	require.True(t, cfg.IsSlave())

	var receivedBlock *Block
	onRecv := func(blk *Block) {
		receivedBlock = blk
	}

	bt, remote := newTestTransport(t, cfg, onRecv)

	ourBlock := makeTestBlock()
	ourBlock.SetSystemBytesUint32(0x11111111)
	ourWire := ourBlock.Pack()

	// Master's block that we'll receive during contention.
	masterBlock := makeTestBlock()
	masterBlock.SetSystemBytesUint32(0x22222222)
	masterBlock.SetRBit(true) // Equipment→Host
	masterWire := masterBlock.Pack()

	go func() {
		// Phase 1: Slave sends ENQ, we respond with ENQ (contention).
		b := readOneByte(t, remote)
		assert.Equal(t, ENQ, b)
		mustWrite(t, remote, []byte{ENQ}) // contention!

		// Phase 2: Slave yields — sends EOT, we send Master's block.
		b = readOneByte(t, remote)
		assert.Equal(t, EOT, b)
		mustWrite(t, remote, masterWire)

		// Phase 3: Slave sends ACK for Master's block.
		b = readOneByte(t, remote)
		assert.Equal(t, ACK, b)

		// Phase 4: Slave retries its own send (ENQ again).
		b = readOneByte(t, remote)
		assert.Equal(t, ENQ, b)
		mustWrite(t, remote, []byte{EOT})
		wireData := readExactly(t, remote, len(ourWire))
		assert.Equal(t, ourWire, wireData)
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), ourBlock)
	require.NoError(t, err)

	// Verify we received Master's block via onRecvBlock callback.
	require.NotNil(t, receivedBlock)
	assert.Equal(t, uint32(0x22222222), receivedBlock.SystemBytesUint32())
	assert.True(t, receivedBlock.RBit())
}

func TestSendBlock_Contention_Master(t *testing.T) {
	// This end is Master (Equipment). After sending ENQ, receives ENQ.
	// Master ignores ENQ and keeps waiting for EOT.
	cfg := newTestConfig(t, WithEquipRole())
	require.True(t, cfg.IsMaster())

	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		// Read ENQ from Master.
		b := readOneByte(t, remote)
		assert.Equal(t, ENQ, b)

		// Send ENQ (contention). Master should ignore it.
		mustWrite(t, remote, []byte{ENQ})

		// Small delay, then send EOT. Master should accept this.
		time.Sleep(5 * time.Millisecond)
		mustWrite(t, remote, []byte{EOT})

		// Read block data.
		wireData := readExactly(t, remote, len(expectedWire))
		assert.Equal(t, expectedWire, wireData)

		// Send ACK.
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

func TestSendBlock_Contention_MasterIgnoresGarbage(t *testing.T) {
	// Master ignores all characters except EOT (per §7.8.2.1).
	cfg := newTestConfig(t, WithEquipRole())
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		readOneByte(t, remote)                       // ENQ
		mustWrite(t, remote, []byte{ENQ, NAK, 0xFF}) // garbage before EOT
		time.Sleep(5 * time.Millisecond)
		mustWrite(t, remote, []byte{EOT})         // finally EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

func TestSendBlock_ContentionResetsRetry(t *testing.T) {
	// Verify that contention resets the retry counter.
	// With retryLimit=1, two T2 timeouts would fail without contention.
	// But if contention resets the counter between them, we succeed.
	cfg := newTestConfig(t, WithRetryLimit(1))
	require.True(t, cfg.IsSlave())

	var recvCount int
	onRecv := func(blk *Block) {
		recvCount++
	}

	bt, remote := newTestTransport(t, cfg, onRecv)

	block := makeTestBlock()
	masterBlock := makeTestBlock()
	masterBlock.SetRBit(true)
	masterWire := masterBlock.Pack()
	expectedWire := block.Pack()

	go func() {
		// Attempt 1: T2 timeout (no response).
		readOneByte(t, remote) // ENQ
		// No response → T2 timeout → retry counter = 1.

		// Attempt 2 (retry 1): Contention → resets retry counter to 0.
		b := readOneByte(t, remote) // ENQ
		assert.Equal(t, ENQ, b)
		mustWrite(t, remote, []byte{ENQ}) // contention
		b = readOneByte(t, remote)        // EOT (slave yields)
		assert.Equal(t, EOT, b)
		mustWrite(t, remote, masterWire) // master's block
		b = readOneByte(t, remote)       // ACK for master's block
		assert.Equal(t, ACK, b)

		// Attempt 3 (retry counter reset to 0): T2 timeout → retry counter = 1.
		readOneByte(t, remote) // ENQ
		// No response → T2 timeout.

		// Attempt 4 (retry 1 again): success.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
	assert.Equal(t, 1, recvCount) // received one block during contention
}

// --- Send: context cancellation ---

func TestSendBlock_ContextCancelled(t *testing.T) {
	cfg := newTestConfig(t)
	bt, _ := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bt.sendBlock(ctx, block)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestSendBlock_ContextCancelledDuringT2Wait(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		readOneByte(t, remote) // ENQ
		// Don't respond. Cancel context after short delay.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := bt.sendBlock(ctx, block)
	require.Error(t, err)
	// The error should be either context.Canceled or ErrSendFailure
	// depending on timing. Either is acceptable.
}

// --- Send: connection error ---

func TestSendBlock_WriteError(t *testing.T) {
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()

	// Close remote immediately to cause a write error.
	_ = remote.Close()

	err := bt.sendBlock(context.Background(), block)
	require.Error(t, err)
	// Should not be ErrSendFailure (that's for retry exhaustion).
	assert.False(t, errors.Is(err, ErrSendFailure))
}

// --- Send: verify wire protocol ---

func TestSendBlock_WireProtocol_ENQSent(t *testing.T) {
	// Verify that the first byte sent is ENQ (0x05).
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()

	enqCh := make(chan byte, 1)

	go func() {
		b := readOneByte(t, remote)
		enqCh <- b

		// Complete the handshake so sendBlock doesn't hang.
		mustWrite(t, remote, []byte{EOT})
		readExactly(t, remote, len(block.Pack()))
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)

	assert.Equal(t, byte(0x05), <-enqCh, "first byte should be ENQ (0x05)")
}

func TestSendBlock_WireProtocol_BlockDataIntegrity(t *testing.T) {
	// Verify the block wire data matches block.Pack().
	cfg := newTestConfig(t)
	bt, remote := newTestTransport(t, cfg, nil)

	body := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	block := makeTestBlockWithBody(body)
	expected := block.Pack()

	received := make(chan []byte, 1)

	go func() {
		readOneByte(t, remote)                        // ENQ
		mustWrite(t, remote, []byte{EOT})             // EOT
		wire := readExactly(t, remote, len(expected)) // block
		received <- wire
		mustWrite(t, remote, []byte{ACK}) // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)

	assert.Equal(t, expected, <-received)
}

// --- Send: contention edge cases ---

func TestSendBlock_Contention_SlaveReceivesFails(t *testing.T) {
	// During contention, if the Slave fails to receive Master's block,
	// it should still retry its own send.
	cfg := newTestConfig(t)
	require.True(t, cfg.IsSlave())

	var receivedBlock *Block
	onRecv := func(blk *Block) {
		receivedBlock = blk
	}

	bt, remote := newTestTransport(t, cfg, onRecv)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		// Phase 1: Contention.
		readOneByte(t, remote)            // ENQ
		mustWrite(t, remote, []byte{ENQ}) // contention

		// Phase 2: Slave sends EOT, but master sends bad data.
		readOneByte(t, remote)          // EOT
		mustWrite(t, remote, []byte{5}) // invalid length (< 10)
		// Slave drains, sends NAK, then retries its own send.
		readOneByte(t, remote) // NAK

		// Phase 3: Slave retries send.
		b := readOneByte(t, remote) // ENQ
		assert.Equal(t, ENQ, b)
		mustWrite(t, remote, []byte{EOT})
		readExactly(t, remote, len(expectedWire))
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)

	// receivedBlock should be nil since the receive failed.
	assert.Nil(t, receivedBlock)
}

func TestSendBlock_ExactRetryCount(t *testing.T) {
	// With retryLimit=2, we should see exactly 3 ENQs (1 initial + 2 retries).
	cfg := newTestConfig(t, WithRetryLimit(2))
	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()
	var enqCount atomic.Int32

	go func() {
		for i := 0; i < 3; i++ {
			readOneByte(t, remote) // ENQ
			enqCount.Add(1)
			// No response → T2 timeout.
		}
	}()

	err := bt.sendBlock(context.Background(), block)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSendFailure))
	assert.Equal(t, int32(3), enqCount.Load())
}

// ===========================================================================
// onRetry callback tests
// ===========================================================================

func TestSendBlock_OnRetryCallback(t *testing.T) {
	// Verify that the onRetry callback is invoked on each retry.
	cfg := newTestConfig(t, WithRetryLimit(2))

	var retryCount atomic.Int32
	onRetry := func() {
		retryCount.Add(1)
	}

	local, remote := newPipeConn(t)
	bt := newBlockTransport(local, cfg, logger.GetLogger(), nil, onRetry)

	block := makeTestBlock()

	go func() {
		for range 3 {
			readOneByte(t, remote) // ENQ
			// No response → T2 timeout → retry.
		}
	}()

	err := bt.sendBlock(context.Background(), block)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSendFailure)

	// With retryLimit=2: initial + 2 retries = 3 attempts, each fails → 3 onRetry calls.
	assert.Equal(t, int32(3), retryCount.Load())
}

func TestSendBlock_OnRetryCallbackNil(t *testing.T) {
	// Verify that nil onRetry doesn't panic.
	cfg := newTestConfig(t, WithRetryLimit(1))

	local, remote := newPipeConn(t)
	bt := newBlockTransport(local, cfg, logger.GetLogger(), nil, nil)

	block := makeTestBlock()
	expectedWire := block.Pack()

	go func() {
		readOneByte(t, remote) // ENQ — no response (T2 timeout, retry)
		readOneByte(t, remote) // ENQ
		mustWrite(t, remote, []byte{EOT})
		readExactly(t, remote, len(expectedWire))
		mustWrite(t, remote, []byte{ACK})
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
}

// ===========================================================================
// Contention failed-receive: increments retry (not reset)
// ===========================================================================

func TestSendBlock_Contention_FailedReceiveIncrementsRetry(t *testing.T) {
	// With retryLimit=1, a successful contention resets retry to 0 and succeeds.
	// But a FAILED contention receive should NOT reset retry — it should increment.
	// So: attempt 0 → failed contention (retry becomes 1) → T2 timeout (retry becomes 2) → fail.
	cfg := newTestConfig(t, WithRetryLimit(1))
	require.True(t, cfg.IsSlave())

	bt, remote := newTestTransport(t, cfg, nil)

	block := makeTestBlock()

	go func() {
		// Attempt 0: Contention with failed receive.
		readOneByte(t, remote)            // ENQ
		mustWrite(t, remote, []byte{ENQ}) // contention

		readOneByte(t, remote)          // EOT (slave yields)
		mustWrite(t, remote, []byte{5}) // invalid length → receive fails
		readOneByte(t, remote)          // NAK

		// Attempt 1 (retry=1): T2 timeout → retry becomes 2 > retryLimit=1.
		readOneByte(t, remote) // ENQ
		// No response → T2 timeout.
	}()

	err := bt.sendBlock(context.Background(), block)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSendFailure)
}

func TestSendBlock_Contention_SuccessfulReceiveResetsRetry(t *testing.T) {
	// Contrast: with retryLimit=1, a SUCCESSFUL contention resets retry to 0.
	// So: T2 timeout (retry=1) → successful contention (retry=0) →
	// T2 timeout (retry=1) → success on next attempt.
	cfg := newTestConfig(t, WithRetryLimit(1))
	require.True(t, cfg.IsSlave())

	var recvCount int
	onRecv := func(_ *Block) { recvCount++ }
	bt, remote := newTestTransport(t, cfg, onRecv)

	block := makeTestBlock()
	masterBlock := makeTestBlock()
	masterBlock.SetRBit(true)
	masterWire := masterBlock.Pack()
	expectedWire := block.Pack()

	go func() {
		// Attempt 0: T2 timeout (retry becomes 1).
		readOneByte(t, remote) // ENQ
		// No response.

		// Attempt 1 (retry=1): Successful contention → retry resets to 0.
		readOneByte(t, remote)            // ENQ
		mustWrite(t, remote, []byte{ENQ}) // contention
		readOneByte(t, remote)            // EOT
		mustWrite(t, remote, masterWire)  // master's block
		readOneByte(t, remote)            // ACK

		// Attempt 2 (retry=0 after reset): T2 timeout (retry becomes 1).
		readOneByte(t, remote) // ENQ
		// No response.

		// Attempt 3 (retry=1): success.
		readOneByte(t, remote)                    // ENQ
		mustWrite(t, remote, []byte{EOT})         // EOT
		readExactly(t, remote, len(expectedWire)) // block
		mustWrite(t, remote, []byte{ACK})         // ACK
	}()

	err := bt.sendBlock(context.Background(), block)
	require.NoError(t, err)
	assert.Equal(t, 1, recvCount)
}
