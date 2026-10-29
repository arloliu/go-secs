//go:build linux && amd64 && !purego

package format

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// guardedTail returns a slice of n bytes that ends exactly at a page the process may not read or write,
// so that a kernel reading or writing past its end faults.
func guardedTail(t *testing.T, n int) []byte {
	t.Helper()

	page := syscall.Getpagesize()
	size := (n + page - 1) / page * page
	mem, err := syscall.Mmap(-1, 0, size+page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Munmap(mem) })
	require.NoError(t, syscall.Mprotect(mem[size:], syscall.PROT_NONE))

	return mem[size-n : size : size]
}

// The kernels read no byte past the end of their input and write none past the end of their output,
// each of which here ends at a page that faults.
func TestTransposeHeadersGuardPage(t *testing.T) {
	if !simdCapable {
		t.Skip("this host does not run the kernels")
	}
	for _, shape := range []struct{ count, headerLen int }{{64, 44}, {128, 44}, {1489, 44}, {320, 64}} {
		n := shape.count * shape.headerLen
		rows := guardedTail(t, n)
		for k := range rows {
			rows[k] = byte(k * 31)
		}
		section := guardedTail(t, n)
		transposeWith(section[:0], rows, shape.count, shape.headerLen, true)
		require.Equal(t, refTransposeHeaders(rows, shape.count, shape.headerLen), section[:n])

		back := guardedTail(t, n)
		untransposeWith(back[:0], section[:n], shape.count, shape.headerLen, true)
		require.Equal(t, rows, back[:n])
	}
}
