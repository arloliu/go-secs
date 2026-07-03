package secs1

import (
	"fmt"
	"iter"

	"github.com/arloliu/go-secs/v2/internal/wire"
)

// splitBody partitions body into SECS-I blocks of at most 244 body bytes each, assigning block
// numbers 1..N and setting the E-bit (last-block flag) on the final block (SEMI E4 §8). An empty
// body yields exactly one header-only block (block number 1, E-bit set). The yielded blocks hold
// zero-copy wire.Chunk sub-views of body — no per-block copy.
//
// It returns an error (and a nil iterator) up front if h.deviceID > 0x7FFF, h.stream > 0x7F, or
// body.Len() exceeds the maximum SECS-I message size (244 * 32767).
func splitBody(body wire.Body, h messageHeader) (iter.Seq[block], error) {
	if h.deviceID > 0x7FFF {
		return nil, fmt.Errorf("%w: deviceID %d > 0x7FFF", ErrInvalidHeader, h.deviceID)
	}
	if h.stream > 0x7F {
		return nil, fmt.Errorf("%w: stream %d > 127", ErrInvalidHeader, h.stream)
	}
	total := body.Len()
	if total > maxBlockBodySize*maxBlockNumber {
		return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, total)
	}

	return func(yield func(block) bool) {
		if total == 0 {
			yield(block{header: buildHeader(h, 1, true)}) // header-only block 1, E-bit set
			return
		}
		blockNumber := uint16(1)
		for off := 0; off < total; off += maxBlockBodySize {
			n := min(maxBlockBodySize, total-off)
			last := off+n == total
			if !yield(block{header: buildHeader(h, blockNumber, last), body: body.Chunk(off, n)}) {
				return
			}
			blockNumber++
		}
	}, nil
}

// assembleBlocks reassembles an ordered slice of received blocks into the message body. It is
// stateless: it validates that block numbers are the contiguous sequence 1..len(blocks), that the
// E-bit is set exactly on the last block, and that the block-invariant header fields are identical
// across all blocks; then it concatenates the block bodies into one freshly-owned buffer and
// returns it as a wire.Body (via wire.AdoptBody) for lazy secs2.Decode by the caller.
//
// The returned body is independent of the input block buffers (the concatenation copies), so the
// per-block buffers may be reused or freed once assembleBlocks returns.
func assembleBlocks(blocks []block) (messageHeader, wire.Body, error) {
	if len(blocks) == 0 {
		return messageHeader{}, nil, ErrEmptyBlocks
	}
	first := blocks[0].messageHeader()
	total := 0
	for i, b := range blocks {
		if int(b.blockNumber()) != i+1 {
			return messageHeader{}, nil, fmt.Errorf("%w: position %d has block number %d", ErrBlockNumberMismatch, i+1, b.blockNumber())
		}
		if b.eBit() != (i == len(blocks)-1) {
			return messageHeader{}, nil, fmt.Errorf("%w: position %d", ErrEBitPlacement, i+1)
		}
		if b.messageHeader() != first {
			return messageHeader{}, nil, fmt.Errorf("%w: position %d", ErrHeaderMismatch, i+1)
		}
		total += b.body.Len()
	}

	buf := make([]byte, 0, total)
	for _, b := range blocks {
		buf = b.body.AppendTo(buf)
	}

	return first, wire.AdoptBody(buf), nil
}

// hsmsHeaderLen is the fixed 10-byte HSMS message header (SEMI E37 §6.2) that assembleFrame writes
// ahead of the reassembled body. It equals the SECS-I block header size by coincidence.
const hsmsHeaderLen = 10

// assembleFrame reassembles an ordered slice of received blocks into a synthesized HSMS frame: a
// contiguous OWNED [10-byte HSMS header || body] buffer, safe to hand to the core's
// DeliverOwnedFrame (which takes ownership). The 10 header bytes are reserved up front and written
// in place so the body appends after them with no extra copy (the P2 optimization).
//
// Validation mirrors [assembleBlocks] — the E-bit set exactly on the last block and the
// block-invariant SECS-I header (deviceID/R-bit/stream/function/W-bit/system-bytes) identical across
// every block — but is LENIENT on the first block number per D5b-12: a lone single block may be
// numbered 0 or 1, while a multi-block message must be numbered 1..N.
//
// The block-invariant SECS-I header maps onto the HSMS header (SEMI E37 §6.2 / SEMI E4 §8) as:
//   - bytes 0-1 = deviceID (big-endian) — the device ID carried in the received blocks, surfaced as
//     the HSMS session ID so an inbound message reports its wire device ID through SessionID()
//   - byte 2    = (W-bit 0x80) | (stream 0x7F)
//   - byte 3    = function
//   - bytes 4-5 = 0 — PType 0, SType 0 (a data message)
//   - bytes 6-9 = system bytes
//
// The body is the ordered concatenation of the block bodies. The returned buffer is freshly owned
// and independent of the input block buffers (the body concatenation copies).
func assembleFrame(blocks []block) ([]byte, error) {
	if len(blocks) == 0 {
		return nil, ErrEmptyBlocks
	}
	first := blocks[0].messageHeader()
	// D5b-12 interop leniency: a lone block numbered 0 is a valid single-block message.
	singleBlockZero := len(blocks) == 1 && blocks[0].blockNumber() == 0

	total := 0
	for i, b := range blocks {
		wantNum := i + 1
		if singleBlockZero {
			wantNum = 0
		}
		if int(b.blockNumber()) != wantNum {
			return nil, fmt.Errorf("%w: position %d has block number %d", ErrBlockNumberMismatch, i+1, b.blockNumber())
		}
		if b.eBit() != (i == len(blocks)-1) {
			return nil, fmt.Errorf("%w: position %d", ErrEBitPlacement, i+1)
		}
		if b.messageHeader() != first {
			return nil, fmt.Errorf("%w: position %d", ErrHeaderMismatch, i+1)
		}
		total += b.body.Len()
	}

	// Reserve the 10 HSMS header bytes so the body appends in place (the P2 no-extra-copy path).
	buf := make([]byte, hsmsHeaderLen, hsmsHeaderLen+total)
	// Stamp the session-id field with the device ID carried in the received blocks (big-endian,
	// matching the outbound splitFrame encoding) so an inbound message's SessionID() reports the
	// wire device ID.
	buf[0] = byte(first.deviceID >> 8)
	buf[1] = byte(first.deviceID)
	buf[2] = first.stream & 0x7F
	if first.waitBit {
		buf[2] |= 0x80
	}
	buf[3] = first.function
	// buf[4], buf[5] remain 0 — PType 0, SType 0 (a data message).
	copy(buf[6:hsmsHeaderLen], first.systemBytes[:])

	for _, b := range blocks {
		buf = b.body.AppendTo(buf)
	}

	return buf, nil
}
