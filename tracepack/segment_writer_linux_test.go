//go:build linux

package tracepack

import (
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This test lives in a Linux-only file because it maps a 2 GiB payload with mmap,
// which keeps the payload out of the Go heap and out of physical memory.

func TestSegmentWriterRefusesPayloadAboveLimit(t *testing.T) {
	t.Parallel()

	if strconv.IntSize < 64 {
		t.Skip("a 2 GiB payload needs 64-bit ints")
	}
	size := int64(1) << 31
	huge, err := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		t.Skipf("mmap 2 GiB of address space: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(huge) })

	w, sink := newSeg(t, nil)
	calls := sink.calls
	r := segMonoRecord(time.Hour, time.Hour) // a record of the next hour, which would roll
	r.Payload = huge[:maxPayloadLen+1]
	r.FieldValidity = 0
	require.ErrorIs(t, w.Append(t.Context(), &r), ErrPayloadTooLarge)
	assert.Equal(t, calls, sink.calls, "no sink call")
	require.NoError(t, w.Close(t.Context()), "the writer stays usable")
	assert.Equal(t, [][]uint64{{0, 1}}, segSeqs(checkCapture(t, w, sink)))
}
