package v2

import "github.com/arloliu/go-secs/v2/secs2"

// Shapes reused across the v1/v2 hsmsssdata benchmark pair. Keep this file
// byte-identical to hsmsssdata/v2/shapes_test.go except for the import path.

const (
	waferDieCount = 100_000
	waferSize     = 300
	recipeBytes   = 1 << 20 // 1 MiB, typical recipe transfer size
	structListLen = 100_000
)

func smallItem() secs2.Item {
	return secs2.L(
		secs2.A("test"),
		secs2.F8(1, 2, 3, 4),
		secs2.BOOLEAN(true, false, false),
	)
}

func structuredListItem() secs2.Item {
	data := make([]secs2.Item, structListLen)
	for i := range data {
		data[i] = secs2.I8(int64(i))
	}

	return secs2.L(data...)
}

func waferMapItem() secs2.Item {
	data := make([]byte, waferDieCount)
	for i := range data {
		data[i] = byte(i % 4)
	}

	return secs2.L(secs2.U4(uint32(waferSize)), secs2.B(data))
}

func recipeItem() secs2.Item {
	data := make([]byte, recipeBytes)
	for i := range data {
		data[i] = byte('A' + i%26)
	}

	return secs2.L(secs2.A("RECIPE_NAME"), secs2.A(string(data)))
}
