package secs1

import (
	"fmt"
	"net"

	"github.com/arloliu/go-secs/v2/internal/wire"
)

// splitFrame converts one core-framed HSMS DATA frame into the ordered SECS-I blocks to transmit
// (§4a). It is the send adapter Write uses after the shape + control guard has already accepted the
// frame as a data frame (SType 0).
//
// header is the 10-byte HSMS header (bufs[0][4:14]); bufs[1:] are the body's zero-copy sub-slices
// after the 14-byte prefix. The block-invariant SECS-I header takes the deviceID and R-bit direction
// from config (send-side only — T5 owns inbound R-bit validation) and the stream / function / W-bit /
// system-bytes from the HSMS header. Byte map (SEMI E37 §6.2 / SEMI E4 §8):
//
//	header[2] = (W-bit 0x80) | (stream 0x7F)
//	header[3] = function
//	header[6:10] = system bytes
//
// splitBody then partitions the gathered body into <=244-byte blocks (numbers 1..N, E-bit on the
// last, >=1 block even for an empty body — a header-only block).
func (t *transport) splitFrame(header []byte, bufs net.Buffers) ([]block, error) {
	mh := messageHeader{
		deviceID:    t.cfg.DeviceID(),
		rBit:        t.cfg.IsEquip(), // direction (block.go): false = to equipment, true = to host
		stream:      header[2] & 0x7F,
		function:    header[3],
		waitBit:     header[2]&0x80 != 0,
		systemBytes: [4]byte(header[6:10]),
	}

	// Gather the body from the post-prefix sub-slices. Common case (bufs len 2): a single body slice —
	// take it zero-copy (§4a copy-free send note). Write blocks until the last block ACKs and
	// block.appendTo re-reads the body per retransmission, so bufs outlives every read of this view; the
	// blocks hold read-only sub-views and never mutate it. A list body may arrive as several slices
	// (bufs len > 2), so concatenate those into one contiguous buffer. A control / empty-body data frame
	// (bufs len 1) yields a nil body — splitBody still emits one header-only block.
	var body []byte
	if len(bufs) == 2 {
		body = bufs[1]
	} else {
		for _, b := range bufs[1:] {
			body = append(body, b...)
		}
	}

	seq, err := splitBody(wire.AdoptBody(body), mh)
	if err != nil {
		return nil, fmt.Errorf("secs1: split frame body: %w", err)
	}

	blocks := make([]block, 0, 1)
	for blk := range seq {
		blocks = append(blocks, blk)
	}

	return blocks, nil
}
