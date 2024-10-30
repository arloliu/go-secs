package secs2

import (
	"math"
	"testing"
)

func BenchmarkToSML_AllTypes(b *testing.B) {
	items := []Item{}

	for i := 0; i < 39; i++ {
		switch i % 13 {
		case 0:
			items = append(items, B(127))
		case 1:
			items = append(items, BOOLEAN(true))
		case 2:
			items = append(items, A("test message"))
		case 3:
			items = append(items, I1(math.MaxInt8))
		case 4:
			items = append(items, I2(math.MaxInt16))
		case 5:
			items = append(items, I4(math.MaxInt32))
		case 6:
			items = append(items, I8(math.MaxInt64))
		case 7:
			items = append(items, U1(math.MaxUint8))
		case 8:
			items = append(items, U2(math.MaxUint16))
		case 9:
			items = append(items, U4(math.MaxUint32))
		case 10:
			items = append(items, U8(uint64(math.MaxUint64)))
		case 11:
			items = append(items, F4(1.2345678))
		case 12:
			items = append(items, F8(1.2345678))
		}
	}
	listItems := []Item{}
	for i := 0; i < 100; i++ {
		listItems = append(listItems, L(items...))
	}

	benchItem := L(listItems...)
	b.ResetTimer()
	for i := 0; i <= b.N; i++ {
		sml := benchItem.ToSML()
		_ = sml
	}
	b.StopTimer()
}
