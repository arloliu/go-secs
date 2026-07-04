package secs2

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFormatCodeDefinedType verifies FormatCode is a distinct defined uint8 type, not an int
// alias, so its constants no longer auto-convert to/from int.
func TestFormatCodeDefinedType(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	rt := reflect.TypeFor[FormatCode]()
	require.Equal(reflect.Uint8, rt.Kind())
	require.Equal("FormatCode", rt.Name())
	require.Contains(rt.PkgPath(), "secs2")
}

// TestFormatCodeString verifies String returns the SECS-II item-type name for every defined
// format code, plus the fallback for an unknown code.
func TestFormatCodeString(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	cases := map[FormatCode]string{
		ListFormatCode:         ListType,
		BinaryFormatCode:       BinaryType,
		BooleanFormatCode:      BooleanType,
		ASCIIFormatCode:        ASCIIType,
		JIS8FormatCode:         JIS8Type,
		LocalizedStrFormatCode: LocalizedStrType,
		Int8FormatCode:         Int8Type,
		Int16FormatCode:        Int16Type,
		Int32FormatCode:        Int32Type,
		Int64FormatCode:        Int64Type,
		Uint8FormatCode:        Uint8Type,
		Uint16FormatCode:       Uint16Type,
		Uint32FormatCode:       Uint32Type,
		Uint64FormatCode:       Uint64Type,
		Float32FormatCode:      Float32Type,
		Float64FormatCode:      Float64Type,
	}
	for fc, want := range cases {
		require.Equal(want, fc.String())
	}

	// 63 (0o77) is a valid 6-bit value but not an assigned SECS-II format code.
	require.Contains(FormatCode(63).String(), "unknown")
}
