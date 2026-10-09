package corpus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"

	"github.com/arloliu/go-secs/tracepack/internal/codec"
	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// Damage kinds of a hand-built zstd frame (the tracepack corpus specification §6.2).
const (
	// ZstdMalformed ends the frame with a Compressed block whose content is invalid:
	// a literals section header declaring 31 Raw literal bytes, and none of them.
	ZstdMalformed ZstdDamage = iota + 1
	// ZstdShort ends the frame with a Raw block that leaves the body's last byte unproduced.
	ZstdShort
)

// Parts of an RFC 8878 frame the hand-built frames use.
const (
	zstdMagic uint32 = 0xFD2FB528
	// zstdDescriptor is a Frame_Header_Descriptor of a single-segment frame with a 4-byte Frame_Content_Size and no dictionary.
	zstdDescriptor byte = 0xA0
	// zstdChecksumFlag is the Content_Checksum_flag of a Frame_Header_Descriptor.
	zstdChecksumFlag byte = 0x04
	// zstdFrameHeaderLen is the length of the frame header zstdDescriptor describes: magic, descriptor, content size.
	zstdFrameHeaderLen  = 9
	zstdBlockHeaderLen  = 3
	zstdBlockRaw        = 0
	zstdBlockCompressed = 2
	// zstdMaxBlock is the largest Block_Size of any frame.
	zstdMaxBlock = 128 << 10
	// zstdBadLiterals is the malformed block's content: a Raw_Literals_Block header of Regenerated_Size 31 in its 1-byte form.
	zstdBadLiterals byte = 31 << 3
)

// XXH64 primes (the xxHash specification), for the content checksum of RFC 8878 §3.1.1.
const (
	xxhPrime1 uint64 = 11400714785074694791
	xxhPrime2 uint64 = 14029467366897019727
	xxhPrime3 uint64 = 1609587929392839161
	xxhPrime4 uint64 = 9650029242287828579
	xxhPrime5 uint64 = 2870177450012600261
)

// ZstdDamage names how a hand-built zstd frame fails after its intact header section.
type ZstdDamage uint8

// DamageZstd returns a copy of pack whose block i (in walk order, see Locate) holds a hand-built zstd frame instead of its body
// (the tracepack corpus specification §6.2):
// a single-segment frame whose first block is a Raw block holding the whole header section of the decoded body,
// then the damage, then a content checksum when checksum is set.
//
// For ZstdMalformed the frame's content size is the body's length and the checksum covers the whole body;
// for ZstdShort the frame is well formed but for its length:
// its content size is the bytes it produces, one fewer than the body, and the checksum covers them.
// The construction checks the frame: decoding its first block alone yields the header section, and decoding the whole frame fails.
// A body whose header section is empty or larger than a zstd block, or a short frame of a body without payload bytes, cannot prove that and is refused.
//
// The envelope is edited in place: codec becomes zstd, and body_len, body_crc and envelope_crc are recomputed,
// uncompressed_len and its other bytes kept.
// In a finalized pack the block's F-2 entry takes on_disk_len and body_crc, the rest following the relocation rule of PatchBody.
// The block's stored body must decode with its stored codec.
func DamageZstd(pack []byte, i int, damage ZstdDamage, checksum bool) ([]byte, error) {
	l, err := Locate(pack)
	if err != nil {
		return nil, err
	}
	s, err := l.block(i)
	if err != nil {
		return nil, err
	}

	stored := pack[s.Offset : s.Offset+s.Len]
	decoded, hs, err := decodeBlock(stored)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", i, err)
	}
	frame, err := damagedFrame(decoded, hs, damage, checksum, nil)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", i, err)
	}

	blk := append(bytes.Clone(stored[:format.EnvelopeLen]), frame...)
	blk[envelopeCodecOff] = codec.Zstd
	binary.LittleEndian.PutUint32(blk[envelopeBodyLenOff:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(blk[envelopeBodyCRCOff:], format.CRC(frame))
	putCRC(blk[:format.EnvelopeLen], blockEnvelopeCRCOff)

	return replaceBlock(pack, &l, i, blk, mirrorOnDisk)
}

// damagedFrame builds the frame DamageZstd describes for the decoded body whose header section is its first hs bytes.
// mutate, when set, changes the frame before the construction checks it.
func damagedFrame(body []byte, hs int, damage ZstdDamage, checksum bool, mutate func(frame []byte)) ([]byte, error) {
	if hs <= 0 || hs > zstdMaxBlock || hs > len(body) {
		return nil, fmt.Errorf("%w: a header section of %d bytes cannot be one Raw block of the %d-byte body", ErrPackEdit, hs, len(body))
	}

	var frame, content []byte
	switch damage {
	case ZstdMalformed:
		content = body
		frame = zstdFrameHeader(uint32(len(content)), checksum)
		frame = appendZstdBlock(frame, zstdBlockRaw, false, body[:hs])
		frame = appendZstdBlock(frame, zstdBlockCompressed, true, []byte{zstdBadLiterals})
	case ZstdShort:
		content = body[:len(body)-1]
		if len(content) < hs || len(content)-hs > zstdMaxBlock {
			return nil, fmt.Errorf("%w: a %d-byte body cannot end short after its %d-byte header section", ErrPackEdit, len(body), hs)
		}
		frame = zstdFrameHeader(uint32(len(content)), checksum)
		frame = appendZstdBlock(frame, zstdBlockRaw, false, body[:hs])
		frame = appendZstdBlock(frame, zstdBlockRaw, true, content[hs:])
	default:
		return nil, fmt.Errorf("%w: damage %d", ErrPackEdit, damage)
	}
	if checksum {
		frame = binary.LittleEndian.AppendUint32(frame, uint32(xxh64(content)))
	}
	if mutate != nil {
		mutate(frame)
	}

	if err := checkDamagedFrame(frame, body, hs, damage); err != nil {
		return nil, err
	}

	return frame, nil
}

// checkDamagedFrame checks that decoding the first block of frame alone yields body's header section, its first hs bytes,
// and that decoding the whole frame to len(body) bytes fails;
// for ZstdShort also that the frame is well formed but for its length: it decodes to body without its last byte,
// its content checksum, when present, included.
func checkDamagedFrame(frame, body []byte, hs int, damage ZstdDamage) error {
	first := zstdFrameHeaderLen + zstdBlockHeaderLen
	if len(frame) < first || binary.LittleEndian.Uint32(frame) != zstdMagic || frame[4]&^zstdChecksumFlag != zstdDescriptor {
		return fmt.Errorf("%w: the frame header is not a single-segment header with a 4-byte content size", ErrPackEdit)
	}
	h := int(frame[9]) | int(frame[10])<<8 | int(frame[11])<<16
	if typ, size := (h>>1)&3, h>>3; typ != zstdBlockRaw || size != hs || len(frame)-first < size {
		return fmt.Errorf("%w: the first block is not a Raw block of the %d-byte header section", ErrPackEdit, hs)
	}

	alone := zstdFrameHeader(uint32(hs), false)
	alone = appendZstdBlock(alone, zstdBlockRaw, true, frame[first:first+hs])
	prefix, err := codec.Decode(codec.Zstd, nil, alone, hs)
	if err != nil {
		return fmt.Errorf("%w: the first block alone does not decode: %w", ErrPackEdit, err)
	}
	if !bytes.Equal(prefix, body[:hs]) {
		return fmt.Errorf("%w: the first block alone does not decode to the header section", ErrPackEdit)
	}
	if _, err := codec.Decode(codec.Zstd, nil, frame, len(body)); err == nil {
		return fmt.Errorf("%w: the whole frame decodes", ErrPackEdit)
	}
	if damage == ZstdShort {
		short, err := codec.Decode(codec.Zstd, nil, frame, len(body)-1)
		if err != nil || !bytes.Equal(short, body[:len(body)-1]) {
			return fmt.Errorf("%w: the short frame does not decode to the body without its last byte", ErrPackEdit)
		}
	}

	return nil
}

// zstdFrameHeader returns a single-segment frame header of content size n, with the content checksum flag when checksum is set.
func zstdFrameHeader(n uint32, checksum bool) []byte {
	d := zstdDescriptor
	if checksum {
		d |= zstdChecksumFlag
	}
	h := binary.LittleEndian.AppendUint32(make([]byte, 0, zstdFrameHeaderLen), zstdMagic)
	h = append(h, d)

	return binary.LittleEndian.AppendUint32(h, n)
}

// appendZstdBlock appends a block of type typ holding content, marked last when last is set;
// content must be shorter than 2^21 bytes.
func appendZstdBlock(dst []byte, typ int, last bool, content []byte) []byte {
	h := len(content)<<3 | typ<<1
	if last {
		h |= 1
	}
	dst = append(dst, byte(h), byte(h>>8), byte(h>>16))

	return append(dst, content...)
}

// xxh64 returns the XXH64 hash of b with seed 0 (the xxHash specification).
func xxh64(b []byte) uint64 {
	n := uint64(len(b))
	var h uint64
	if len(b) >= 32 {
		p1 := xxhPrime1 // a variable, so the seed lanes wrap as the specification's arithmetic does
		v1, v2, v3, v4 := p1+xxhPrime2, xxhPrime2, uint64(0), -p1
		for ; len(b) >= 32; b = b[32:] {
			v1 = xxhRound(v1, binary.LittleEndian.Uint64(b))
			v2 = xxhRound(v2, binary.LittleEndian.Uint64(b[8:]))
			v3 = xxhRound(v3, binary.LittleEndian.Uint64(b[16:]))
			v4 = xxhRound(v4, binary.LittleEndian.Uint64(b[24:]))
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		for _, v := range [...]uint64{v1, v2, v3, v4} {
			h = (h^xxhRound(0, v))*xxhPrime1 + xxhPrime4
		}
	} else {
		h = xxhPrime5
	}
	h += n

	for ; len(b) >= 8; b = b[8:] {
		h ^= xxhRound(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*xxhPrime1 + xxhPrime4
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * xxhPrime1
		h = bits.RotateLeft64(h, 23)*xxhPrime2 + xxhPrime3
		b = b[4:]
	}
	for _, c := range b {
		h ^= uint64(c) * xxhPrime5
		h = bits.RotateLeft64(h, 11) * xxhPrime1
	}

	h ^= h >> 33
	h *= xxhPrime2
	h ^= h >> 29
	h *= xxhPrime3
	h ^= h >> 32

	return h
}

// xxhRound is the XXH64 round: acc + lane × prime 2, rotated left by 31, times prime 1.
func xxhRound(acc, lane uint64) uint64 {
	return bits.RotateLeft64(acc+lane*xxhPrime2, 31) * xxhPrime1
}
