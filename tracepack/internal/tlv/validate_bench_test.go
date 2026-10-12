package tlv

import "testing"

// BenchmarkValidate measures Validate on a list holding its registry's required tags,
// as a reader validates each F-3 summary of a footer and each pack metadata:
// the cost a large pack's Open and a merge's inputs pay per entry list.
func BenchmarkValidate(b *testing.B) {
	for _, bc := range []struct {
		name string
		reg  Registry
	}{
		{"F3", F3},
		{"PackMetadata", PackMetadata},
	} {
		b.Run(bc.name, func(b *testing.B) {
			entries := entriesFor(bc.reg, requiredTags(bc.reg))
			b.ReportAllocs()
			for b.Loop() {
				if err := Validate(entries, bc.reg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
