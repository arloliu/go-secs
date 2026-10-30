//go:build !linux

package tracepack

import "testing"

// hugePayload skips the test: the payload above the block limit is mapped with mmap, which this test does only on Linux.
func hugePayload(t *testing.T) []byte {
	t.Helper()
	t.Skip("a 2 GiB payload is mapped only on Linux")

	return nil
}
