//go:build amd64 && !purego

#include "textflag.h"

// The AVX-512 F and BW kernels of TransposeHeaders and UntransposeHeaders (header_section_amd64.go).

// func cpuid(leaf, sub uint32) (a, b, c, d uint32)
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL leaf+0(FP), AX
	MOVL sub+4(FP), CX
	CPUID
	MOVL AX, a+8(FP)
	MOVL BX, b+12(FP)
	MOVL CX, c+16(FP)
	MOVL DX, d+20(FP)
	RET

// func xgetbv() (eax, edx uint32)
TEXT ·xgetbv(SB), NOSPLIT, $0-8
	MOVL $0, CX
	XGETBV
	MOVL AX, eax+0(FP)
	MOVL DX, edx+4(FP)
	RET

// func untransposeKernel(rows, section *byte, count, headerLen, tiles, colBlocks int, idx *[64]int64)
//
// For each tile of 64 records and each block of 8 header bytes,
// it loads the block's 8 columns of the tile's records (64 bytes each), interleaves them in three stages of unpacks,
// bytes, then words, then dwords, within each 128-bit lane, which leaves each record's 8 bytes in one qword,
// and scatters the qwords to their rows at the offsets of idx.
TEXT ·untransposeKernel(SB), NOSPLIT, $0-56
	MOVQ rows+0(FP), DI
	MOVQ section+8(FP), SI
	MOVQ count+16(FP), R8
	MOVQ headerLen+24(FP), R9
	MOVQ tiles+32(FP), R10
	MOVQ colBlocks+40(FP), R11
	MOVQ idx+48(FP), AX
	VMOVDQU64 0(AX), Z16
	VMOVDQU64 64(AX), Z17
	VMOVDQU64 128(AX), Z18
	VMOVDQU64 192(AX), Z19
	VMOVDQU64 256(AX), Z20
	VMOVDQU64 320(AX), Z21
	VMOVDQU64 384(AX), Z22
	VMOVDQU64 448(AX), Z23
	MOVQ R9, R12
	SHLQ $6, R12 // 64*hl: out advance per tile
	MOVQ R8, R13
	SHLQ $3, R13 // 8*count: section advance per column block

utileloop:
	TESTQ R10, R10
	JZ udone
	MOVQ SI, AX // column block start in section
	MOVQ DI, CX // out position
	MOVQ R11, DX

ucolloop:
	TESTQ DX, DX
	JZ unexttile
	MOVQ AX, BX
	VMOVDQU64 (BX), Z0
	ADDQ R8, BX
	VMOVDQU64 (BX), Z1
	ADDQ R8, BX
	VMOVDQU64 (BX), Z2
	ADDQ R8, BX
	VMOVDQU64 (BX), Z3
	ADDQ R8, BX
	VMOVDQU64 (BX), Z4
	ADDQ R8, BX
	VMOVDQU64 (BX), Z5
	ADDQ R8, BX
	VMOVDQU64 (BX), Z6
	ADDQ R8, BX
	VMOVDQU64 (BX), Z7

	VPUNPCKLBW Z1, Z0, Z8
	VPUNPCKHBW Z1, Z0, Z9
	VPUNPCKLBW Z3, Z2, Z10
	VPUNPCKHBW Z3, Z2, Z11
	VPUNPCKLBW Z5, Z4, Z12
	VPUNPCKHBW Z5, Z4, Z13
	VPUNPCKLBW Z7, Z6, Z14
	VPUNPCKHBW Z7, Z6, Z15

	VPUNPCKLWD Z10, Z8, Z0
	VPUNPCKHWD Z10, Z8, Z1
	VPUNPCKLWD Z11, Z9, Z2
	VPUNPCKHWD Z11, Z9, Z3
	VPUNPCKLWD Z14, Z12, Z4
	VPUNPCKHWD Z14, Z12, Z5
	VPUNPCKLWD Z15, Z13, Z6
	VPUNPCKHWD Z15, Z13, Z7

	VPUNPCKLDQ Z4, Z0, Z8
	VPUNPCKHDQ Z4, Z0, Z9
	VPUNPCKLDQ Z5, Z1, Z10
	VPUNPCKHDQ Z5, Z1, Z11
	VPUNPCKLDQ Z6, Z2, Z12
	VPUNPCKHDQ Z6, Z2, Z13
	VPUNPCKLDQ Z7, Z3, Z14
	VPUNPCKHDQ Z7, Z3, Z15

	KXNORW K1, K1, K1
	VPSCATTERQQ Z8, K1, (CX)(Z16*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z9, K1, (CX)(Z17*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z10, K1, (CX)(Z18*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z11, K1, (CX)(Z19*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z12, K1, (CX)(Z20*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z13, K1, (CX)(Z21*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z14, K1, (CX)(Z22*1)
	KXNORW K1, K1, K1
	VPSCATTERQQ Z15, K1, (CX)(Z23*1)

	ADDQ R13, AX
	ADDQ $8, CX
	DECQ DX
	JMP ucolloop

unexttile:
	ADDQ $64, SI
	ADDQ R12, DI
	DECQ R10
	JMP utileloop

udone:
	VZEROUPPER
	RET

// func transposeKernel(section, rows *byte, count, headerLen, tiles, colBlocks int, idx *[64]int64, masks *[2][64]byte)
//
// The inverse of untransposeKernel: for each tile and block, it gathers each record's 8 bytes from its row,
// undoes the dword, word and byte interleave stages with VPSHUFD or VPSHUFB and VPUNPCKLQDQ / VPUNPCKHQDQ,
// and stores the block's 8 columns of the tile's records, 64 bytes each.
TEXT ·transposeKernel(SB), NOSPLIT, $0-64
	MOVQ section+0(FP), DI
	MOVQ rows+8(FP), SI
	MOVQ count+16(FP), R8
	MOVQ headerLen+24(FP), R9
	MOVQ tiles+32(FP), R10
	MOVQ colBlocks+40(FP), R11
	MOVQ idx+48(FP), AX
	VMOVDQU64 0(AX), Z16
	VMOVDQU64 64(AX), Z17
	VMOVDQU64 128(AX), Z18
	VMOVDQU64 192(AX), Z19
	VMOVDQU64 256(AX), Z20
	VMOVDQU64 320(AX), Z21
	VMOVDQU64 384(AX), Z22
	VMOVDQU64 448(AX), Z23
	MOVQ masks+56(FP), AX
	VMOVDQU64 0(AX), Z24  // byte deinterleave within each lane
	VMOVDQU64 64(AX), Z25 // word deinterleave within each lane
	MOVQ R9, R12
	SHLQ $6, R12 // 64*hl: rows advance per tile
	MOVQ R8, R13
	SHLQ $3, R13 // 8*count: section advance per column block

ttileloop:
	TESTQ R10, R10
	JZ tdone
	MOVQ DI, AX // section position of this column block
	MOVQ SI, CX // rows position
	MOVQ R11, DX

tcolloop:
	TESTQ DX, DX
	JZ tnexttile
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z16*1), K1, Z8
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z17*1), K1, Z9
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z18*1), K1, Z10
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z19*1), K1, Z11
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z20*1), K1, Z12
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z21*1), K1, Z13
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z22*1), K1, Z14
	KXNORW K1, K1, K1
	VPGATHERQQ (CX)(Z23*1), K1, Z15

	// Undo the dword stage: (U0,V0)=(W0,W1), (U1,V1)=(W2,W3), (U2,V2)=(W4,W5), (U3,V3)=(W6,W7).
	VPSHUFD $0xD8, Z8, Z8
	VPSHUFD $0xD8, Z9, Z9
	VPSHUFD $0xD8, Z10, Z10
	VPSHUFD $0xD8, Z11, Z11
	VPSHUFD $0xD8, Z12, Z12
	VPSHUFD $0xD8, Z13, Z13
	VPSHUFD $0xD8, Z14, Z14
	VPSHUFD $0xD8, Z15, Z15
	VPUNPCKLQDQ Z9, Z8, Z0  // U0
	VPUNPCKHQDQ Z9, Z8, Z4  // V0
	VPUNPCKLQDQ Z11, Z10, Z1 // U1
	VPUNPCKHQDQ Z11, Z10, Z5 // V1
	VPUNPCKLQDQ Z13, Z12, Z2 // U2
	VPUNPCKHQDQ Z13, Z12, Z6 // V2
	VPUNPCKLQDQ Z15, Z14, Z3 // U3
	VPUNPCKHQDQ Z15, Z14, Z7 // V3

	// Undo the word stage: (T0,T2)=(U0,U1), (T1,T3)=(U2,U3), (T4,T6)=(V0,V1), (T5,T7)=(V2,V3).
	VPSHUFB Z25, Z0, Z0
	VPSHUFB Z25, Z1, Z1
	VPSHUFB Z25, Z2, Z2
	VPSHUFB Z25, Z3, Z3
	VPSHUFB Z25, Z4, Z4
	VPSHUFB Z25, Z5, Z5
	VPSHUFB Z25, Z6, Z6
	VPSHUFB Z25, Z7, Z7
	VPUNPCKLQDQ Z1, Z0, Z8   // T0
	VPUNPCKHQDQ Z1, Z0, Z10  // T2
	VPUNPCKLQDQ Z3, Z2, Z9   // T1
	VPUNPCKHQDQ Z3, Z2, Z11  // T3
	VPUNPCKLQDQ Z5, Z4, Z12  // T4
	VPUNPCKHQDQ Z5, Z4, Z14  // T6
	VPUNPCKLQDQ Z7, Z6, Z13  // T5
	VPUNPCKHQDQ Z7, Z6, Z15  // T7

	// Undo the byte stage: (Z0,Z1)=(T0,T1), (Z2,Z3)=(T2,T3), (Z4,Z5)=(T4,T5), (Z6,Z7)=(T6,T7).
	VPSHUFB Z24, Z8, Z8
	VPSHUFB Z24, Z9, Z9
	VPSHUFB Z24, Z10, Z10
	VPSHUFB Z24, Z11, Z11
	VPSHUFB Z24, Z12, Z12
	VPSHUFB Z24, Z13, Z13
	VPSHUFB Z24, Z14, Z14
	VPSHUFB Z24, Z15, Z15
	VPUNPCKLQDQ Z9, Z8, Z0
	VPUNPCKHQDQ Z9, Z8, Z1
	VPUNPCKLQDQ Z11, Z10, Z2
	VPUNPCKHQDQ Z11, Z10, Z3
	VPUNPCKLQDQ Z13, Z12, Z4
	VPUNPCKHQDQ Z13, Z12, Z5
	VPUNPCKLQDQ Z15, Z14, Z6
	VPUNPCKHQDQ Z15, Z14, Z7

	MOVQ AX, BX
	VMOVDQU64 Z0, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z1, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z2, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z3, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z4, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z5, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z6, (BX)
	ADDQ R8, BX
	VMOVDQU64 Z7, (BX)

	ADDQ R13, AX
	ADDQ $8, CX
	DECQ DX
	JMP tcolloop

tnexttile:
	ADDQ $64, DI
	ADDQ R12, SI
	DECQ R10
	JMP ttileloop

tdone:
	VZEROUPPER
	RET
