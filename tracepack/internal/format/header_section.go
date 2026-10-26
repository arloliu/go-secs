package format

import (
	"encoding/binary"
	"slices"
)

// TransposeHeaders appends to dst the header section of count record headers of headerLen bytes each,
// given as consecutive rows in rows, and returns the extended slice.
// The header section stores the headers column by column (the tracepack format specification §6):
// byte j of record i's header is at offset j × count + i of the section.
//
// rows holds exactly count × headerLen bytes; the caller checked that product with HeaderSectionLen.
// A block of one record is laid out as its header's bytes in order.
func TransposeHeaders(dst, rows []byte, count, headerLen int) []byte {
	if count <= 1 {
		return append(dst, rows...)
	}

	start := len(dst)
	dst = slices.Grow(dst, len(rows))[:start+len(rows)]
	section := dst[start:]
	full, cols := count&^7, headerLen&^7
	le := binary.LittleEndian
	for i := 0; i < full; i += 8 {
		in := rows[i*headerLen : (i+8)*headerLen]
		for j := 0; j < cols; j += 8 {
			r := in[j:]
			a0, a1, a2, a3, a4, a5, a6, a7 := transpose8x8(le.Uint64(r), le.Uint64(r[headerLen:]),
				le.Uint64(r[2*headerLen:]), le.Uint64(r[3*headerLen:]), le.Uint64(r[4*headerLen:]),
				le.Uint64(r[5*headerLen:]), le.Uint64(r[6*headerLen:]), le.Uint64(r[7*headerLen:]))
			c := section[j*count+i:]
			le.PutUint64(c, a0)
			le.PutUint64(c[count:], a1)
			le.PutUint64(c[2*count:], a2)
			le.PutUint64(c[3*count:], a3)
			le.PutUint64(c[4*count:], a4)
			le.PutUint64(c[5*count:], a5)
			le.PutUint64(c[6*count:], a6)
			le.PutUint64(c[7*count:], a7)
		}
		for j := cols; j < headerLen; j++ {
			col := section[j*count+i : j*count+i+8]
			for r := range col {
				col[r] = in[r*headerLen+j]
			}
		}
	}
	for i := full; i < count; i++ {
		row := rows[i*headerLen : (i+1)*headerLen]
		for j, b := range row {
			section[j*count+i] = b
		}
	}

	return dst
}

// UntransposeHeaders appends to dst the count record headers of headerLen bytes each gathered from section,
// a header section stored column by column (the tracepack format specification §6),
// as consecutive rows, and returns the extended slice: TransposeHeaders reversed.
//
// section holds exactly count × headerLen bytes; the caller checked that product with HeaderSectionLen.
func UntransposeHeaders(dst, section []byte, count, headerLen int) []byte {
	if count <= 1 {
		return append(dst, section...)
	}

	start := len(dst)
	dst = slices.Grow(dst, len(section))[:start+len(section)]
	rows := dst[start:]
	full, cols := count&^7, headerLen&^7
	le := binary.LittleEndian
	for i := 0; i < full; i += 8 {
		out := rows[i*headerLen : (i+8)*headerLen]
		for j := 0; j < cols; j += 8 {
			c := section[j*count+i:]
			a0, a1, a2, a3, a4, a5, a6, a7 := transpose8x8(le.Uint64(c), le.Uint64(c[count:]),
				le.Uint64(c[2*count:]), le.Uint64(c[3*count:]), le.Uint64(c[4*count:]),
				le.Uint64(c[5*count:]), le.Uint64(c[6*count:]), le.Uint64(c[7*count:]))
			r := out[j:]
			le.PutUint64(r, a0)
			le.PutUint64(r[headerLen:], a1)
			le.PutUint64(r[2*headerLen:], a2)
			le.PutUint64(r[3*headerLen:], a3)
			le.PutUint64(r[4*headerLen:], a4)
			le.PutUint64(r[5*headerLen:], a5)
			le.PutUint64(r[6*headerLen:], a6)
			le.PutUint64(r[7*headerLen:], a7)
		}
		for j := cols; j < headerLen; j++ {
			col := section[j*count+i : j*count+i+8]
			for r, b := range col {
				out[r*headerLen+j] = b
			}
		}
	}
	for i := full; i < count; i++ {
		row := rows[i*headerLen : (i+1)*headerLen]
		for j := range row {
			row[j] = section[j*count+i]
		}
	}

	return dst
}

// transpose8x8 transposes the 8×8 byte matrix whose row k is the little-endian word ak:
// byte c of result word k is byte k of input word c.
// It swaps 4×4, then 2×2, then 1×1 blocks of bytes, each with one mask and shift per pair of words.
//
//nolint:revive // eight words in and out keep the matrix in registers; an [8]uint64 is not, and runs at about 60 % of the speed
func transpose8x8(a0, a1, a2, a3, a4, a5, a6, a7 uint64) (b0, b1, b2, b3, b4, b5, b6, b7 uint64) {
	const m32, m16, m8 = 0x00000000FFFFFFFF, 0x0000FFFF0000FFFF, 0x00FF00FF00FF00FF

	t := (a0>>32 ^ a4) & m32
	a0, a4 = a0^t<<32, a4^t
	t = (a1>>32 ^ a5) & m32
	a1, a5 = a1^t<<32, a5^t
	t = (a2>>32 ^ a6) & m32
	a2, a6 = a2^t<<32, a6^t
	t = (a3>>32 ^ a7) & m32
	a3, a7 = a3^t<<32, a7^t

	t = (a0>>16 ^ a2) & m16
	a0, a2 = a0^t<<16, a2^t
	t = (a1>>16 ^ a3) & m16
	a1, a3 = a1^t<<16, a3^t
	t = (a4>>16 ^ a6) & m16
	a4, a6 = a4^t<<16, a6^t
	t = (a5>>16 ^ a7) & m16
	a5, a7 = a5^t<<16, a7^t

	t = (a0>>8 ^ a1) & m8
	a0, a1 = a0^t<<8, a1^t
	t = (a2>>8 ^ a3) & m8
	a2, a3 = a2^t<<8, a3^t
	t = (a4>>8 ^ a5) & m8
	a4, a5 = a4^t<<8, a5^t
	t = (a6>>8 ^ a7) & m8
	a6, a7 = a6^t<<8, a7^t

	return a0, a1, a2, a3, a4, a5, a6, a7
}
