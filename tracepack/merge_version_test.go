package tracepack

import (
	"testing"

	"github.com/arloliu/go-secs/tracepack/internal/format"
)

// TestMergeRefusesUnknownVersions merges, beside a valid segment, a pack whose trailer_version is unknown,
// which leaves it not finalized, and a pack whose footer_layout_version is unknown, which leaves it without a valid footer
// (the tracepack format specification §10, §11 and §14):
// Merge refuses each with ErrMergeInput naming the input, before it touches dst
// (the tracepack storage specification §4 Merge, Input verification).
func TestMergeRefusesUnknownVersions(t *testing.T) {
	t.Parallel()

	good := mergePack(t, seg0, nil, hourSteps(10, 13, 2))
	file := mergePack(t, seg1, nil, hourSteps(20, 23, 2))
	_, decoded, _ := splitPack(t, file)
	tests := []struct {
		name string
		file []byte
		msg  string
	}{
		{
			name: "trailer_version 2",
			file: patchTrailer(t, file, func(tr *format.Trailer) { tr.TrailerVersion = 2 }),
			msg:  "the pack is not finalized",
		},
		{
			name: "footer_layout_version 2",
			file: refooter(t, file, patchPrologue(t, decoded, func(pro *format.FooterPrologue) { pro.FooterLayoutVersion = 2 })),
			msg:  "the pack has no valid footer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requireMergeRefused(t, inputsOf(good, tt.file), mergeOpts(), "input 1, pack "+seg1.String()+": "+tt.msg, ErrMergeInput)
		})
	}
}
