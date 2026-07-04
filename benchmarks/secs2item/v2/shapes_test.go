package v2

import (
	"math"

	"github.com/arloliu/go-secs/v2/secs2"
)

// Shapes reused across the v1/v2 secs2item benchmark pair. Keep this file
// byte-identical to secs2item/v2/shapes_test.go except for the import path —
// that is what makes the v1/v2 benchmark numbers a fair comparison.

const (
	waferDieCount = 100_000
	waferSize     = 300
	recipeBytes   = 1 << 20 // 1 MiB, typical recipe transfer size
	intListLen    = 1000
)

// nestedListItem builds a 100-sublist x 13-mixed-scalar-type nested list, the
// same shape as the repo's existing "AllTypes" benchmarks.
func nestedListItem() secs2.Item {
	leaves := make([]secs2.Item, 0, 13)
	for i := range 13 {
		switch i {
		case 0:
			leaves = append(leaves, secs2.B(127))
		case 1:
			leaves = append(leaves, secs2.BOOLEAN(true))
		case 2:
			leaves = append(leaves, secs2.A("test message"))
		case 3:
			leaves = append(leaves, secs2.I1(math.MaxInt8))
		case 4:
			leaves = append(leaves, secs2.I2(math.MaxInt16))
		case 5:
			leaves = append(leaves, secs2.I4(math.MaxInt32))
		case 6:
			leaves = append(leaves, secs2.I8(math.MaxInt64))
		case 7:
			leaves = append(leaves, secs2.U1(math.MaxUint8))
		case 8:
			leaves = append(leaves, secs2.U2(math.MaxUint16))
		case 9:
			leaves = append(leaves, secs2.U4(math.MaxUint32))
		case 10:
			leaves = append(leaves, secs2.U8(uint64(math.MaxUint64)))
		case 11:
			leaves = append(leaves, secs2.F4(1.2345678))
		default:
			leaves = append(leaves, secs2.F8(1.2345678))
		}
	}

	sublists := make([]secs2.Item, 0, 100)
	for range 100 {
		sublists = append(sublists, secs2.L(leaves...))
	}

	return secs2.L(sublists...)
}

// intListValues returns intListLen int64 values boxed as []any, the shape
// secs2.I8's variadic-any shortcut expects for a bulk list.
func intListValues() []any {
	values := make([]any, intListLen)
	for i := range values {
		values[i] = int64(i)
	}

	return values
}

// waferMapItem simulates a 300mm wafer map: die size plus a 100k-die binary
// bin-code blob.
func waferMapItem() secs2.Item {
	data := make([]byte, waferDieCount)
	for i := range data {
		data[i] = byte(i % 4)
	}

	return secs2.L(secs2.U4(uint32(waferSize)), secs2.B(data))
}

// recipeItem simulates a 1 MiB recipe transfer: name plus a large ASCII blob.
func recipeItem() secs2.Item {
	data := make([]byte, recipeBytes)
	for i := range data {
		data[i] = byte('A' + i%26)
	}

	return secs2.L(secs2.A("RECIPE_NAME"), secs2.A(string(data)))
}
