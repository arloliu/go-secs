package secs2

import (
	"testing"
)

func BenchmarkUintItem_CombineValues(b *testing.B) {
	// Fast path: []uint64
	b.Run("FastPath_Uint64Slice", func(b *testing.B) {
		input := make([]uint64, 100)
		for i := range input {
			input[i] = uint64(i)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := &UintItem{byteSize: 8}
			_ = item.combineUintValues(input)
		}
	})

	// Fast path: []uint
	b.Run("FastPath_UintSlice", func(b *testing.B) {
		input := make([]uint, 100)
		for i := range input {
			input[i] = uint(i)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := &UintItem{byteSize: 8}
			_ = item.combineUintValues(input)
		}
	})

	// Slow path: []string
	b.Run("SlowPath_StringSlice", func(b *testing.B) {
		input := make([]string, 100)
		for i := range input {
			input[i] = "12345"
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := &UintItem{byteSize: 8}
			_ = item.combineUintValues(input)
		}
	})

	// Slow path: Mixed types
	b.Run("SlowPath_Mixed", func(b *testing.B) {
		input := []any{
			uint64(10), "20", int(30), uint(40),
			uint64(10), "20", int(30), uint(40),
			uint64(10), "20", int(30), uint(40),
			uint64(10), "20", int(30), uint(40),
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			item := &UintItem{byteSize: 8}
			_ = item.combineUintValues(input...)
		}
	})
}
