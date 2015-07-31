#include "textflag.h"

// A byte-swapping mask used for converting endianness in xmm registers.
DATA bswap<>+0x00(SB)/1, $0xf
DATA bswap<>+0x01(SB)/1, $0xe
DATA bswap<>+0x02(SB)/1, $0xd
DATA bswap<>+0x03(SB)/1, $0xc
DATA bswap<>+0x04(SB)/1, $0xb
DATA bswap<>+0x05(SB)/1, $0xa
DATA bswap<>+0x06(SB)/1, $0x9
DATA bswap<>+0x07(SB)/1, $0x8
DATA bswap<>+0x08(SB)/1, $0x7
DATA bswap<>+0x09(SB)/1, $0x6
DATA bswap<>+0x0a(SB)/1, $0x5
DATA bswap<>+0x0b(SB)/1, $0x4
DATA bswap<>+0x0c(SB)/1, $0x3
DATA bswap<>+0x0d(SB)/1, $0x2
DATA bswap<>+0x0e(SB)/1, $0x1
DATA bswap<>+0x0f(SB)/1, $0x0
GLOBL bswap<>(SB), RODATA, $16

TEXT ·canUpdateBlocksFast(SB),NOSPLIT,$0-1
	// We need the following instructions, whose feature flags are indicated:
	//
	//     PCLMULQDQ:  PCLMULQDQ
	//     PSHUFB:     SSSE3
	//     PXOR:       SSE2
	//     PSRLDQ:     SSE2
	//     PSLLL, etc: SSE2
	//     POR:        SSE2
	//
	// Test for each.
	MOVQ $1, AX
	CPUID

	MOVQ $1, AX

	// PCLMULQDQ
	MOVQ CX, DI
	SHRQ $1, DI
	ANDQ DI, AX

	// SSSE3
	MOVQ CX, DI
	SHRQ $9, DI
	ANDQ DI, AX

	// SSE2
	MOVQ DX, DI
	SHRQ $26, DI
	ANDQ DI, AX

	MOVB AX, ret+0(FP)
	RET

TEXT ·updateBlocksFast(SB),NOSPLIT,$0-64
	// Load [blocks, blocks+len) into [SI, DI).
	MOVQ blocks+0(FP), SI
	MOVQ blocksLen+8(FP), DI
	ADDQ SI, DI

	// Load the byte-swapping mask, which we use repeatedly below, into X10.
	MOVOU bswap<>(SB), X10

	// Load the initial state of X into X0. Note that the order in memory for the
	// arguments will yield the desired order: coefficient for x^0 in the most
	// significant bit of X0.
	MOVOU xHigh+16(FP), X0

	// Load H into X1. The same argument applies here.
	MOVOU hHigh+32(FP), X1

	// Keep going until we've run out of blocks.
loop:
	CMPQ SI, DI
	JGE done

	CALL updateBlock<>(SB)

	ADDQ $16, SI
	JMP loop

done:
	// Store the result.
	MOVOU X0, outHigh+48(FP)

	RET

// Perform one iteration for updateBlocksFast.
//
// The GF(2^128) multiplication is based on the algorithm in this whitepaper:
//
//     https://software.intel.com/sites/default/files/managed/72/cc/clmul-wp-rev-2.02-2014-04-20.pdf
//
// Inputs:
//     SI  -- Pointer to first byte of block
//     X0  -- Current state of X
//     X1  -- H
//     X10 -- bswap mask
//
// Outputs:
//     X0 -- New state of X
//
// Guaranteed unmodified:
//     SI
//     DI
//     X1
//     X10
//
TEXT updateBlock<>(SB),NOSPLIT,$0-0
	// Load the block into X3. We must byte swap this because the first byte will
	// be loaded as the least significant byte of X3, but we require it to be the
	// most significant byte.
	MOVOU (SI), X3
	PSHUFB X10, X3

	// Compute X xor *block into X0.
	PXOR X3, X0

	// Perform the carryless multiplication.
	MOVOA X0, X3
	PCLMULQDQ $0, X1, X3

	MOVO X0, X4
	PCLMULQDQ $16, X1, X4

	MOVO X0, X5
	PCLMULQDQ $1, X1, X5

	MOVO X0, X6
	PCLMULQDQ $17, X1, X6

	PXOR X5, X4
	MOVO X4, X5

	PSRLDQ $8, X4
	PSLLO $8, X5

	PXOR X5, X3
	PXOR X4, X6

	// Shift the result left by one position to compensate for reversed bits.
	MOVO X3, X7
	MOVO X6, X8

	PSLLL $1, X3
	PSLLL $1, X6
	PSRLL $31, X7
	PSRLL $31, X8
	MOVO X7, X9
	PSLLO $4, X8
	PSLLO $4, X7
	PSRLDQ $12, X9

	POR X7, X3
	POR X8, X6
	POR X9, X6

	// Reduce (first phase)
	MOVO X3, X7
	MOVO X3, X8
	MOVO X3, X9

	PSLLL $31, X7
	PSLLL $30, X8
	PSLLL $25, X9
	PXOR X8, X7
	PXOR X9, X7

	MOVO X7, X8
	PSLLO $12, X7
	PSRLDQ $4, X8
	PXOR X7, X3

	// Reduce (second phase)
	MOVO X3, X2
	MOVO X3, X4
	MOVO X3, X5

	PSRLL $1, X2
	PSRLL $2, X4
	PSRLL $7, X5

	PXOR X4, X2
	PXOR X5, X2
	PXOR X8, X2
	PXOR X2, X3
	PXOR X3, X6

	// Put the result in the appropriate place.
	MOVO X6, X0

	RET
