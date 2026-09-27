package tlv

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// SeqRangeLen is the length of an encoded seq range: first u64, then last u64.
const SeqRangeLen = 16

// SeqRange is the value of a seq_range entry of a block summary or of the pack statistics,
// an inclusive range of seqs (the tracepack format specification §10).
type SeqRange struct {
	First uint64
	Last  uint64
}

// AppendU32Array appends counts as a u32 array (little-endian u32 elements) and returns the extended slice.
func AppendU32Array(dst []byte, counts []uint32) []byte {
	dst = slices.Grow(dst, 4*len(counts))
	for _, c := range counts {
		dst = binary.LittleEndian.AppendUint32(dst, c)
	}

	return dst
}

// DecodeU32Array decodes a u32 array.
// The length of b must be a multiple of 4.
// Every element is returned, including trailing elements beyond those the reader knows;
// the caller treats missing trailing elements as 0.
func DecodeU32Array(b []byte) ([]uint32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("u32 array of %d bytes: %w", len(b), ErrLength)
	}
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[4*i:])
	}

	return out, nil
}

// AppendU64Array appends counts as a u64 array (little-endian u64 elements) and returns the extended slice.
// A writer must keep every element at or below MaxU64.
func AppendU64Array(dst []byte, counts []uint64) []byte {
	dst = slices.Grow(dst, 8*len(counts))
	for _, c := range counts {
		dst = binary.LittleEndian.AppendUint64(dst, c)
	}

	return dst
}

// DecodeU64Array decodes a u64 array.
// The length of b must be a multiple of 8, and every element must be at or below MaxU64.
// Every element is returned, including trailing elements beyond those the reader knows;
// the caller treats missing trailing elements as 0.
func DecodeU64Array(b []byte) ([]uint64, error) {
	if len(b)%8 != 0 {
		return nil, fmt.Errorf("u64 array of %d bytes: %w", len(b), ErrLength)
	}
	out := make([]uint64, len(b)/8)
	for i := range out {
		v := binary.LittleEndian.Uint64(b[8*i:])
		if v > MaxU64 {
			return nil, fmt.Errorf("u64 array element %d: %w", i, ErrLimit)
		}
		out[i] = v
	}

	return out, nil
}

// AppendSeqRange appends the 16-byte encoding of r and returns the extended slice.
func AppendSeqRange(dst []byte, r SeqRange) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, r.First)

	return binary.LittleEndian.AppendUint64(dst, r.Last)
}

// DecodeSeqRange decodes a 16-byte seq range whose seqs are at or below MaxU64.
// It does not require First ≤ Last:
// the order and coalescing of ranges belong to footer validation.
func DecodeSeqRange(b []byte) (SeqRange, error) {
	if len(b) != SeqRangeLen {
		return SeqRange{}, fmt.Errorf("seq range of %d bytes: %w", len(b), ErrLength)
	}
	r := SeqRange{
		First: binary.LittleEndian.Uint64(b),
		Last:  binary.LittleEndian.Uint64(b[8:]),
	}
	if r.First > MaxU64 || r.Last > MaxU64 {
		return SeqRange{}, fmt.Errorf("seq range: %w", ErrLimit)
	}

	return r, nil
}
