package secs2

import (
	"testing"
)

func BenchmarkIntItem_Create_FastPath(b *testing.B) {
	values := make([]int64, 1000)
	for i := 0; i < 1000; i++ {
		values[i] = int64(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item, _ := NewIntItem(4, values).(*IntItem)
		item.Free()
	}
}

func BenchmarkIntItem_Create_Mixed(b *testing.B) {
	values := []any{
		int(1), int64(2), []int{3, 4}, []int64{5, 6},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item, _ := NewIntItem(4, values...).(*IntItem)
		item.Free()
	}
}

func BenchmarkIntItem_Create_String(b *testing.B) {
	values := make([]string, 100)
	for i := 0; i < 100; i++ {
		values[i] = "12345"
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item, _ := NewIntItem(4, values).(*IntItem)
		item.Free()
	}
}
