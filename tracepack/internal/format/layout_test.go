package format

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// layoutField names one field of a fixed layout by its byte range, as listed in the tracepack format specification.
type layoutField struct {
	name  string
	off   int
	width int
}

// requireAscendingFields asserts that every listed field of b holds the bytes off, off+1, ... off+width-1.
//
// The golden vectors of this package give each field the little-endian value whose bytes equal their own offsets,
// so this check catches a field written at the wrong offset, with the wrong width or in the wrong byte order.
func requireAscendingFields(t *testing.T, b []byte, fields []layoutField) {
	t.Helper()

	for _, f := range fields {
		for i := range f.width {
			require.Equalf(t, byte(f.off+i), b[f.off+i], "%s: byte %d", f.name, f.off+i)
		}
	}
}

// requireOffsetError asserts that err is an *OffsetError for the given field and offset wrapping want.
func requireOffsetError(t *testing.T, err error, field string, off int, want error) {
	t.Helper()

	require.ErrorIs(t, err, want)

	var oe *OffsetError
	require.True(t, errors.As(err, &oe), "error %v is not an *OffsetError", err)
	require.Equal(t, field, oe.Field)
	require.Equal(t, off, oe.Offset)
}

// cloneBytes returns a copy of b that a test case may modify freely.
func cloneBytes(b []byte) []byte {
	return append([]byte(nil), b...)
}

// reseal recomputes the CRC over b[:crcOff] and stores it at b[crcOff:crcOff+4],
// so a test can change a covered field and still reach the checks that follow the CRC.
func reseal(b []byte, crcOff int) []byte {
	binary.LittleEndian.PutUint32(b[crcOff:], CRC(b[:crcOff]))

	return b
}
