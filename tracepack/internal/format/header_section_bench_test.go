package format

import "testing"

func BenchmarkUntransposeHeaders(b *testing.B) {
	const count = 12404
	rows := make([]byte, count*RecordHeaderLen)
	for k := range rows {
		rows[k] = byte(k * 7)
	}
	section := TransposeHeaders(nil, rows, count, RecordHeaderLen)
	dst := make([]byte, 0, len(section))
	b.SetBytes(int64(len(section)))
	for b.Loop() {
		dst = UntransposeHeaders(dst[:0], section, count, RecordHeaderLen)
	}
}

func BenchmarkTransposeHeaders(b *testing.B) {
	const count = 12404
	rows := make([]byte, count*RecordHeaderLen)
	for k := range rows {
		rows[k] = byte(k * 7)
	}
	dst := make([]byte, 0, len(rows))
	b.SetBytes(int64(len(rows)))
	for b.Loop() {
		dst = TransposeHeaders(dst[:0], rows, count, RecordHeaderLen)
	}
}
