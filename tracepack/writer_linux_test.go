//go:build linux

package tracepack_test

import (
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arloliu/go-secs/tracepack"
)

// This test lives in a Linux-only file because it maps a 2 GiB payload with mmap,
// which keeps the payload out of the Go heap and out of physical memory.

func TestWriterRejectsPayloadAboveLimitWithoutAllocating(t *testing.T) {
	// Not parallel: the allocation check reads process-wide memory statistics.
	// 2^31 bytes of address space mapped outside the Go heap and never touched:
	// the writer must reject the payload by its length alone, never reading or copying it.
	huge, err := syscall.Mmap(-1, 0, 1<<31, syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		t.Skipf("mmap 2 GiB of address space: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(huge) })
	// The largest payload a block can hold alone: 2^31-1 minus the 44 header bytes, plus one.
	justOver := huge[:(1<<31-1)-44+1]

	w, buf := newTestWriter(t, tracepack.WriterOptions{})
	headLen := buf.Len()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	for _, p := range [][]byte{huge, justOver} {
		r := dataRecord(0, hourStart)
		r.Payload = p
		require.ErrorIs(t, w.Append(&r), tracepack.ErrPayloadTooLarge)
	}

	runtime.ReadMemStats(&after)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "rejecting the payload allocated for it")

	r := dataRecord(0, hourStart)
	require.NoError(t, w.Append(&r), "a rejected payload consumes no seq")
	mustClose(t, w)
	assert.Greater(t, buf.Len(), headLen)
}
