package corpus

import (
	"bytes"
	"fmt"
	"math"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Block is a block decoded into typed structures, as ReencodeBlocks hands it to an edit.
// Unlike the raw surgery of PatchBody, re-encoding writes every structure from its typed fields:
// the envelope's reserved bytes are zero,
// and each record header keeps its extension area but only the bits format 1.0 defines.
type Block struct {
	// Envelope is the block's envelope.
	// The re-encoded envelope takes Codec, RecordCount and FirstSeq from it,
	// and record_header_len, body_len, uncompressed_len and body_crc from the encoded body.
	Envelope format.BlockEnvelope
	// Headers holds the record headers in record order; all must have extension areas of one length.
	Headers []format.RecordHeader
	// Payloads holds each record's payload, parallel to Headers.
	Payloads [][]byte
	// Tail holds the decoded bytes after the last payload.
	Tail []byte
}

// ReencodeBlocks returns pack with every block, in walk order (see Locate), decoded, changed by edit and encoded again.
//
// Each re-encoded block replaces the stored one under the relocation rule of PatchBody,
// its F-2 entry also taking first_seq from the envelope.
// Every value a record determines (last_seq, times, epochs, F-3 and F-5 statistics) keeps its stored bytes,
// so an edit of those makes the records disagree with the footer.
// Every block must decode: a valid envelope, a body that decodes with its codec, and record headers that fill it.
func ReencodeBlocks(pack []byte, edit func(i int, b *Block) error) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}

	out := pack
	for i := range l.Blocks {
		cur, err := Locate(out)
		if err != nil {
			return nil, err
		}
		s := cur.Blocks[i]
		b, err := decodeTypedBlock(out[s.Offset : s.Offset+s.Len])
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if err := edit(i, b); err != nil {
			return nil, err
		}
		blk, err := b.encode()
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if out, err = replaceBlock(out, &cur, i, blk, mirrorOnDisk|mirrorDecoded|mirrorFirstSeq); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// decodeTypedBlock decodes the stored block blk, an envelope and its body.
func decodeTypedBlock(blk []byte) (*Block, error) {
	env, err := format.UnmarshalBlockEnvelope(blk)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	decoded, hs, err := decodeBlock(blk)
	if err != nil {
		return nil, err
	}

	n, rhl := int(env.RecordCount), int(env.RecordHeaderLen)
	b := &Block{Envelope: env, Headers: make([]format.RecordHeader, 0, n), Payloads: make([][]byte, 0, n)}
	rows := format.UntransposeHeaders(nil, decoded[:hs], n, rhl)
	off := hs
	for j := range n {
		h, err := format.UnmarshalRecordHeader(rows[j*rhl:], rhl)
		if err != nil {
			return nil, fmt.Errorf("%w: record %d: %w", ErrPackEdit, j, err)
		}
		if int(h.PayloadLen) > len(decoded)-off {
			return nil, fmt.Errorf("%w: record %d's payload runs past the body", ErrPackEdit, j)
		}
		b.Headers = append(b.Headers, h)
		b.Payloads = append(b.Payloads, bytes.Clone(decoded[off:off+int(h.PayloadLen)]))
		off += int(h.PayloadLen)
	}
	b.Tail = bytes.Clone(decoded[off:])

	return b, nil
}

// encode returns the stored form of b: its envelope, encoded from b.Envelope with the body's fields, and its encoded body.
func (b *Block) encode() ([]byte, error) {
	if len(b.Headers) == 0 || len(b.Payloads) != len(b.Headers) {
		return nil, fmt.Errorf("%w: %d record headers and %d payloads", ErrPackEdit, len(b.Headers), len(b.Payloads))
	}

	var rows []byte
	for j := range b.Headers {
		rows = format.AppendRecordHeader(rows, &b.Headers[j])
	}
	rhl := len(rows) / len(b.Headers)
	if rhl*len(b.Headers) != len(rows) || rhl > math.MaxUint16 {
		return nil, fmt.Errorf("%w: record headers of different or oversized lengths", ErrPackEdit)
	}
	body := format.TransposeHeaders(nil, rows, len(b.Headers), rhl)
	for _, p := range b.Payloads {
		body = append(body, p...)
	}
	body = append(body, b.Tail...)

	enc, err := codec.Encode(b.Envelope.Codec, nil, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackEdit, err)
	}
	if uint64(len(body)) > math.MaxUint32 || uint64(len(enc)) > math.MaxUint32-format.EnvelopeLen {
		return nil, fmt.Errorf("%w: a body of %d bytes, %d encoded, does not fit the envelope", ErrPackEdit, len(body), len(enc))
	}

	env := b.Envelope
	env.RecordHeaderLen = uint16(rhl)
	env.BodyLen, env.UncompressedLen, env.BodyCRC = uint32(len(enc)), uint32(len(body)), format.CRC(enc)

	return append(format.AppendBlockEnvelope(nil, &env), enc...), nil
}
