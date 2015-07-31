// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cipher

import (
	"reflect"
	"unsafe"
)

// Performs the necessary CPU feature tests.
func canUpdateBlocksFast() bool

var shouldUpdateFast = canUpdateBlocksFast()

//go:noescape

// updateBlocksFast performs multiple block-aligned iterations of the
// computation of GHASH as defined in the NIST paper. For each iteration it
// computes:
//
//     X_i+1 = (X_i xor *block_i+1) * H
//
// Blocks are 128-bit big endian numbers where the most significant bit
// represents the coefficient of x^0 in GF(2^128).
func updateBlocksFast(
	blocks unsafe.Pointer,
	blocksLen uintptr,
	xHigh uint64,
	xLow uint64,
	hHigh uint64,
	hLow uint64) (outHigh uint64, outLow uint64)

func (g *gcm) updateBlocks(y *gcmFieldElement, blocks []byte) {
	// Use the fast implementation if possible.
	if shouldUpdateFast {
		sh := (*reflect.SliceHeader)(unsafe.Pointer(&blocks))
		y.high, y.low = updateBlocksFast(
			unsafe.Pointer(sh.Data),
			uintptr(sh.Len),
			y.high,
			y.low,
			g.hashKey.high,
			g.hashKey.low)

		return
	}

	g.updateBlocksSlow(y, blocks)
}
