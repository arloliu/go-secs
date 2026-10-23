package gem_test

import (
	"testing"

	"github.com/arloliu/go-secs/v2/gem"
	"github.com/arloliu/go-secs/v2/secs2"
)

// BenchmarkPresentEach measures the nil check every builder runs on a repeat of items
// that holds no nil, the case every correct send takes; it reports 0 allocs/op.
func BenchmarkPresentEach(b *testing.B) {
	items := []secs2.Item{secs2.U4(1), secs2.A("IDLE"), secs2.L(secs2.U1(2)), secs2.B(3), secs2.F8(4.5)}

	b.ReportAllocs()

	for b.Loop() {
		_ = gem.PresentEach(items, "Report.VALUES")
	}
}
