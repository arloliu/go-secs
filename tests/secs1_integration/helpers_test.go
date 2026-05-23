package secs1integration

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/arloliu/go-secs/secs1"
)

// blockSpec describes a single SECS-I block to be sent by the custom peer.
//
// The peer builds a base block from the base* fields, then applies overrides.
// If sendDelay > 0, the peer sleeps that duration before sending the ENQ for
// this block (used to inject inter-block gaps for T4 testing).
type blockSpec struct {
	// base message fields
	stream   byte
	function byte
	wBit     bool
	deviceID uint16
	sysbytes uint32

	// blockNum is the block number to put in the wire header.
	// 0 means "use sequential counter", >0 means force that value.
	blockNum uint16

	// eBit marks the block as the last (end) block.
	eBit bool

	// forceStream overrides the stream code in the header.
	// A non-zero value replaces the stream code on the wire.
	forceStream byte
	// forceFunction overrides the function code in the header.
	forceFunction byte
	// forceWBit overrides the W-bit when forceWBitSet is true.
	forceWBit    bool
	forceWBitSet bool // true = the forceWBit value is applied

	// sendDelay introduces a sleep before the ENQ for this block.
	sendDelay time.Duration
}

// customPeerResult holds the blocks received by the peer after all sends.
type customPeerResult struct {
	received []*secs1.Block
	err      error
}

// runCustomBlockPeer connects to host:port as an active host, sends the
// sequence of blocks described by specs, and returns all blocks received from
// the equipment side (e.g. S9F7 error replies).
//
// Each block is sent using the full SECS-I line-control protocol (ENQ/EOT/ACK).
// If the equipment sends one or more blocks back between sends (e.g. S9F7),
// those are collected and returned in customPeerResult.received.
//
// The function blocks until all specs are processed or a fatal I/O error
// occurs.
func runCustomBlockPeer(host string, port int, specs []blockSpec) customPeerResult {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		return customPeerResult{err: err}
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	var received []*secs1.Block

	for _, spec := range specs {
		if spec.sendDelay > 0 {
			time.Sleep(spec.sendDelay)
		}

		blk := buildBlock(spec)

		// Drain any inbound ENQ from equipment before sending our ENQ,
		// then attempt to claim line control.
		if err := sendBlockAsSlave(conn, reader, blk, &received); err != nil {
			return customPeerResult{received: received, err: err}
		}
	}

	// After the scenario sequence, drain any final inbound blocks from the
	// equipment (e.g. a delayed S9F7) with a short read window.
	drainInbound(conn, reader, &received, 500*time.Millisecond)

	return customPeerResult{received: received}
}

// sendBlockAsSlave sends one block using the host (Slave) line-control protocol:
//
//  1. Send ENQ.
//  2. Wait for EOT (equipment → host grants line).  If we get ENQ first,
//     yield: send EOT, receive equipment's block, ACK it, then retry.
//  3. Send block wire data.
//  4. Wait for ACK.
//
// inbound blocks captured during contention yield are appended to recv.
func sendBlockAsSlave(conn net.Conn, reader *bufio.Reader, blk *secs1.Block, recv *[]*secs1.Block) error {
	const t2 = 2 * time.Second

	for {
		// Send ENQ.
		if _, err := conn.Write([]byte{secs1.ENQ}); err != nil {
			return err
		}

		// Wait for response.
		if err := conn.SetReadDeadline(time.Now().Add(t2)); err != nil {
			return err
		}

		b, err := reader.ReadByte()
		if err != nil {
			return err
		}

		switch b {
		case secs1.EOT:
			// Got line control — send the block.
			return sendBlockDataAndACK(conn, reader, blk)

		case secs1.ENQ:
			// Equipment sent ENQ simultaneously — we are Slave, so yield.
			// Send EOT to let equipment send its block.
			if _, err := conn.Write([]byte{secs1.EOT}); err != nil {
				return err
			}
			// Receive equipment's block.
			blkFromEquip, recvErr := receiveOneBlock(conn, reader)
			if recvErr == nil && blkFromEquip != nil {
				*recv = append(*recv, blkFromEquip)
			}
			// Retry our send.
			continue

		default:
			// Unexpected byte — try again.
			continue
		}
	}
}

// sendBlockDataAndACK transmits the packed block and waits for ACK.
func sendBlockDataAndACK(conn net.Conn, reader *bufio.Reader, blk *secs1.Block) error {
	if _, err := conn.Write(blk.Pack()); err != nil {
		return err
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}

	b, err := reader.ReadByte()
	if err != nil {
		return err
	}

	if b != secs1.ACK {
		// NAK or unexpected — non-fatal for test purposes; continue.
		_ = b
	}

	return nil
}

// receiveOneBlock reads a single SECS-I block from the equipment,
// sends ACK, and returns the parsed block.
func receiveOneBlock(conn net.Conn, reader *bufio.Reader) (*secs1.Block, error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}

	lengthByte, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}

	remaining := int(lengthByte) + 2 // header/body + 2 checksum bytes
	buf := make([]byte, remaining)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil, err
	}

	blk, err := secs1.ParseBlock(lengthByte, buf)
	if err != nil {
		_, _ = conn.Write([]byte{secs1.NAK})
		return nil, err
	}

	_, _ = conn.Write([]byte{secs1.ACK})

	return blk, nil
}

// drainInbound reads any equipment-initiated blocks for up to timeout.
// Equipment sends ENQ → peer sends EOT → peer receives block.
func drainInbound(conn net.Conn, reader *bufio.Reader, recv *[]*secs1.Block, timeout time.Duration) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		_ = conn.SetReadDeadline(time.Now().Add(remaining))

		b, err := reader.ReadByte()
		if err != nil {
			break
		}

		if b != secs1.ENQ {
			continue
		}

		// Equipment wants to send — grant line control.
		if _, err := conn.Write([]byte{secs1.EOT}); err != nil {
			break
		}

		blk, err := receiveOneBlock(conn, reader)
		if err != nil {
			break
		}

		if blk != nil {
			*recv = append(*recv, blk)
		}
	}
}

// buildBlock constructs a secs1.Block from a blockSpec.
func buildBlock(spec blockSpec) *secs1.Block {
	blk := &secs1.Block{}

	blk.SetDeviceID(spec.deviceID)
	blk.SetRBit(false) // host → equipment: R-bit = 0

	stream := spec.stream
	if spec.forceStream != 0 {
		stream = spec.forceStream
	}

	wBit := spec.wBit
	if spec.forceWBitSet {
		wBit = spec.forceWBit
	}

	blk.SetWBit(wBit)
	blk.SetStreamCode(stream)

	fn := spec.function
	if spec.forceFunction != 0 {
		fn = spec.forceFunction
	}

	blk.SetFunctionCode(fn)
	blk.SetBlockNumber(spec.blockNum)
	blk.SetEBit(spec.eBit)
	blk.SetSystemBytesUint32(spec.sysbytes)

	// Body: a small ASCII payload for non-zero-body tests.
	// For the purposes of these error tests, no body is needed.

	return blk
}
