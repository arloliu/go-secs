//go:build !amd64 || purego

package format

// simdHeaders is false where the AVX-512 kernels are not built: every transpose is the pure-Go code's.
var simdHeaders = false

// transposeTiles64 and untransposeTiles64 do nothing where the kernels are not built, leaving every byte to the pure-Go code.
func transposeTiles64(_, _ []byte, _, _ int) int { return 0 }

func untransposeTiles64(_, _ []byte, _, _ int) int { return 0 }
