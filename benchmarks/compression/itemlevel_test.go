package compression

import (
	"fmt"
	"testing"

	"github.com/arloliu/go-secs/v2/secs2"
	"github.com/klauspost/compress/zstd"
)

// TestItemLevelVsWholeBody answers the question the E37.1 PType=0 constraint
// forces: if wire-level (whole-body) compression is not conformant for HSMS-SS,
// how much benefit is lost by instead compressing the large payload as a SECS-II
// binary item INSIDE the body (application layer, fully spec-clean)?
//
// Item-level result is modelled as: compress only the big item's payload bytes,
// then re-wrap as L{A(ppid), B(compressed)} — i.e. pay the SECS-II framing of
// the surviving structure, uncompressed.
func TestItemLevelVsWholeBody(t *testing.T) {
	enc := mustEnc(zstd.SpeedFastest, nil)

	cases := []struct {
		name    string
		body    secs2.Item
		payload []byte // the large inner value that an application would compress
	}{}

	// S7F3: L{A(ppid), A(recipeText)} — inner A holds nearly the whole body.
	cases = append(cases, struct {
		name    string
		body    secs2.Item
		payload []byte
	}{"S7F3_recipe_500step", s7f3(500, 8), []byte(recipeText(500, 8))})

	// WaferMap: L{U4(size), B(bins)} — inner B holds nearly the whole body.
	cases = append(cases, struct {
		name    string
		body    secs2.Item
		payload []byte
	}{"WaferMap_100k", waferMap(100_000, 9), waferBins(100_000, 9)})

	fmt.Printf("\n%-22s %10s %14s %14s %10s\n", "SAMPLE", "RAW", "WHOLE-BODY", "ITEM-LEVEL", "DELTA")
	for _, c := range cases {
		raw := c.body.ToBytes()
		whole := len(enc.EncodeAll(raw, nil))

		// item-level: compressed payload re-wrapped as a SECS-II binary item
		comp := enc.EncodeAll(c.payload, nil)
		rewrapped := secs2.L(secs2.A("RECIPE_OXIDE_ETCH_02"), secs2.B(anySlice(comp)...))
		item := len(rewrapped.ToBytes())

		fmt.Printf("%-22s %10d %9d (%3.1f%%) %9d (%3.1f%%) %+9d\n",
			c.name, len(raw),
			whole, float64(whole)/float64(len(raw))*100,
			item, float64(item)/float64(len(raw))*100,
			item-whole)
	}
	fmt.Println()
}
