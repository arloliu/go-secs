//go:build linux

package tracepack

import (
	"strconv"
	"syscall"
	"testing"
)

// hugePayload returns a payload one byte longer than any block holds,
// mapped with mmap outside the Go heap and never touched; it skips the test where that is not possible.
func hugePayload(t *testing.T) []byte {
	t.Helper()

	if strconv.IntSize < 64 {
		t.Skip("a 2 GiB payload needs 64-bit ints")
	}
	size := int64(1) << 31
	huge, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		t.Skipf("mmap 2 GiB of address space: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(huge) })

	return huge[:maxPayloadLen+1]
}
